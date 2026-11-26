package agent

import (
	"context"
	"errors"
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Server/p2p"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/CyL-Cly/CodexRM-Server/securechannel"
	"github.com/google/uuid"
)

const lanAuthorizationTTL = 75 * time.Second
const lanAuthorizationRefresh = 15 * time.Second

type lanRoster struct {
	until   time.Time
	clients map[string]bool
}

type lanAuthorization struct {
	roster  atomic.Pointer[lanRoster]
	refresh chan struct{}
}

func (a *lanAuthorization) permits(client string, now time.Time) bool {
	roster := a.roster.Load()
	return roster != nil && now.Before(roster.until) && roster.clients[client]
}

func (a *Agent) lanPairedKey(auth *lanAuthorization, client string) (securechannel.Key, bool) {
	var key securechannel.Key
	if !securechannel.ValidClient(client) || !auth.permits(client, time.Now()) {
		return key, false
	}
	data, err := os.ReadFile(a.secureStatePath(client))
	if err != nil || len(data) != len(key) {
		return key, false
	}
	copy(key[:], data)
	return key, true
}

func (a *Agent) refreshLANAuthorization() {
	if auth := a.lanAuth.Load(); auth != nil {
		select {
		case auth.refresh <- struct{}{}:
		default:
		}
	}
}

func (a *Agent) runLAN(ctx context.Context, app *appserver.Client) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	listener, err := p2p.ListenLAN(ctx)
	if err != nil {
		log.Printf("LAN discovery unavailable (UDP %d): %v", p2p.LANPort, err)
		return
	}
	defer listener.Close()
	auth := &lanAuthorization{refresh: make(chan struct{}, 1)}
	a.lanAuth.Store(auth)
	defer a.lanAuth.CompareAndSwap(auth, nil)
	defer auth.roster.Store(nil)
	go a.pollLANAuthorization(ctx, auth)
	slots := make(chan struct{}, 32)
	_ = listener.Serve(ctx, func(client string) (securechannel.Key, bool) {
		return a.lanPairedKey(auth, client)
	}, func(client string, link *p2p.Link) {
		a.serveLAN(ctx, app, slots, auth, client, link)
	})
}

func (a *Agent) pollLANAuthorization(ctx context.Context, auth *lanAuthorization) {
	ticker := time.NewTicker(lanAuthorizationRefresh)
	defer ticker.Stop()
	for ctx.Err() == nil {
		// No RM round trip on the incoming LAN connection itself. If the RM
		// cannot be reached, an existing authorization expires, never renews.
		query, cancel := context.WithTimeout(ctx, 5*time.Second)
		requestedAt := time.Now()
		clients, err := a.ListDevices(query)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			roster := &lanRoster{until: requestedAt.Add(lanAuthorizationTTL), clients: make(map[string]bool, len(clients))}
			for _, row := range clients {
				roster.clients[row.ClientID] = true
			}
			auth.roster.Store(roster)
		} else {
			var status *httpStatusError
			if errors.As(err, &status) && (status.status == 401 || status.status == 403) {
				auth.roster.Store(nil)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-auth.refresh:
		case <-ticker.C:
		}
	}
}

func (a *Agent) serveLAN(ctx context.Context, app *appserver.Client, slots chan struct{}, auth *lanAuthorization, client string, link *p2p.Link) {
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	d := &directSession{id: uuid.NewString(), client: client, ctx: life, cancel: cancel, active: true,
		lanAuth: auth, out: make(chan []byte, 8)}
	a.eventMu.Lock()
	// Recheck authorization after the handshake and before routing events.
	if _, ok := a.lanPairedKey(auth, client); !ok || ctx.Err() != nil {
		a.eventMu.Unlock()
		return
	}
	if old := a.direct[client]; old != nil {
		old.cancel()
	} else if len(a.direct) >= protocol.P2PMaxSessions {
		a.eventMu.Unlock()
		return
	}
	if a.direct == nil {
		a.direct = make(map[string]*directSession)
	}
	a.direct[client] = d
	a.eventMu.Unlock()
	defer a.cleanupDirect(d)
	log.Printf("local LAN direct active")
	a.serveDirectLink(app, slots, d, link, nil)
}
