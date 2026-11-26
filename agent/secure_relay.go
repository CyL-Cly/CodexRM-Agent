package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/CyL-Cly/CodexRM-Server/securechannel"
	"github.com/gorilla/websocket"
)

const handshakeTimeout = 15 * time.Second

var ready = []byte("ready")

type invitation struct {
	Key       securechannel.Key `json:"key"`
	ExpiresAt int64             `json:"expiresAt"`
}

func (a *Agent) secureStatePath(id string) string {
	return filepath.Join(a.StateDir(), "e2ee", id)
}

func (a *Agent) createSecureInvitation(out *protocol.PairStartResponse) error {
	route := out.PairingCode
	now := time.Now()
	// Old relays ignore Secure. Never use their password as an invitation.
	if !securechannel.ValidRoute(route) || out.ManualPairingCode != "" || out.ExpiresAt <= now.Unix() || out.ExpiresAt > now.Add(11*time.Minute).Unix() {
		return errors.New("relay does not support secure pairing; upgrade the server")
	}
	key, err := securechannel.NewKey()
	if err != nil {
		return err
	}
	path := a.secureStatePath(route)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err == nil {
		for _, e := range entries {
			if !securechannel.ValidRoute(e.Name()) {
				continue
			}
			if m, err := e.Info(); err == nil && now.Sub(m.ModTime()) > 11*time.Minute {
				_ = os.Remove(a.secureStatePath(e.Name()))
			}
		}
	}
	data, err := json.Marshal(invitation{Key: key, ExpiresAt: out.ExpiresAt})
	if err != nil {
		return err
	}
	if err := writeNewSecureState(path, data); err != nil {
		return err
	}
	out.PairingCode = securechannel.PairingCode(route, key)
	return nil
}

func writeNewSecureState(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

func (a *Agent) removeSecureDevice(client string) error {
	if !securechannel.ValidClient(client) {
		return errors.New("invalid client ID")
	}
	err := os.Remove(a.secureStatePath(client))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		a.eventMu.Lock()
		if direct := a.direct[client]; direct != nil {
			direct.cancel()
			delete(a.direct, client)
		}
		a.eventMu.Unlock()
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

type securePeer struct {
	channel *securechannel.Channel
	until   time.Time
	route   string
	key     securechannel.Key
}

type encryptedRelay struct {
	agent   *Agent
	conn    *websocket.Conn
	mu      sync.Mutex
	pending map[string]*securePeer
	active  map[string]*securePeer
}

func newEncryptedRelay(agent *Agent, conn *websocket.Conn) *encryptedRelay {
	return &encryptedRelay{agent: agent, conn: conn, pending: map[string]*securePeer{}, active: map[string]*securePeer{}}
}

func (r *encryptedRelay) send(client, step string, data []byte) error {
	return r.agent.write(r.conn, protocol.Envelope{
		Type: protocol.TypeSecure, ClientID: client,
		Secure: &protocol.SecureFrame{Step: step, Data: data},
	})
}

func (r *encryptedRelay) hello(client string, f *protocol.SecureFrame) {
	if len(f.Data) != 48 {
		return
	}
	now := time.Now()
	for id, s := range r.pending {
		if now.After(s.until) {
			delete(r.pending, id)
		}
	}
	if len(r.pending) >= 64 {
		return
	}
	var key securechannel.Key
	var payload []byte
	s := &securePeer{until: now.Add(handshakeTimeout), route: f.Route}
	if f.Route != "" {
		if !securechannel.ValidRoute(f.Route) {
			return
		}
		var invite invitation
		b, err := os.ReadFile(r.agent.secureStatePath(f.Route))
		if err != nil || json.Unmarshal(b, &invite) != nil || now.Unix() >= invite.ExpiresAt {
			return
		}
		key = invite.Key
		if s.key, err = securechannel.NewKey(); err != nil {
			return
		}
		payload = s.key[:]
	} else {
		b, err := os.ReadFile(r.agent.secureStatePath(client))
		if err != nil || len(b) != len(key) {
			return
		}
		copy(key[:], b)
		s.key = key
	}
	h, err := securechannel.Handshake(false, client, f.Route, key)
	if err != nil {
		return
	}
	if p, _, _, err := h.ReadMessage(nil, f.Data); err != nil || len(p) != 0 {
		return
	}
	wire, up, down, err := h.WriteMessage(nil, payload)
	if err != nil || r.send(client, protocol.SecureReply, wire) != nil {
		return
	}
	s.channel = securechannel.ResponderChannel(up, down)
	r.pending[client] = s
}

func (r *encryptedRelay) confirm(client string, f *protocol.SecureFrame) {
	s := r.pending[client]
	if s == nil || !time.Now().Before(s.until) || len(f.Data) != len(ready)+16 {
		return
	}
	delete(r.pending, client)
	plain, err := s.channel.Open(f.Data)
	if err != nil || string(plain) != string(ready) {
		return
	}
	if s.route != "" {
		path := r.agent.secureStatePath(s.route)
		b, err := os.ReadFile(path)
		var invite invitation
		if err != nil || json.Unmarshal(b, &invite) != nil || time.Now().Unix() >= invite.ExpiresAt {
			return
		}
		// Consume before persisting/acknowledging. On local write failure the
		// user must generate a fresh code; never permit reuse.
		if os.Remove(path) != nil || writeNewSecureState(r.agent.secureStatePath(client), s.key[:]) != nil {
			return
		}
	}
	if _, err := os.Stat(r.agent.secureStatePath(client)); err != nil {
		return
	}
	b, err := s.channel.Seal(ready)
	if err == nil && r.send(client, protocol.SecureConfirm, b) == nil {
		r.active[client] = s
	}
}

func (r *encryptedRelay) receive(env protocol.Envelope, dispatch func(protocol.Envelope, sendEnvelope)) {
	client, f := env.ClientID, env.Secure
	if f == nil || !securechannel.ValidClient(client) || (f.Step != protocol.SecureHello && f.Route != "") {
		return
	}
	r.mu.Lock()
	switch f.Step {
	case protocol.SecureHello:
		r.hello(client, f)
	case protocol.SecureConfirm:
		r.confirm(client, f)
	case protocol.SecureData:
		s := r.active[client]
		if s == nil {
			break
		}
		if _, err := os.Stat(r.agent.secureStatePath(client)); err != nil {
			delete(r.active, client)
			break
		}
		plain, err := s.channel.Open(f.Data)
		if err != nil {
			delete(r.active, client)
			break
		}
		var inner protocol.Envelope
		if json.Unmarshal(plain, &inner) != nil || inner.Type != protocol.TypeClientMessage || !json.Valid(inner.Message) {
			break
		}
		r.mu.Unlock()
		// Trust neither the relay's nor the endpoint's supplied client kind,
		// sequence, or identity for authorization.
		inner = protocol.Envelope{Type: protocol.TypeClientMessage, ClientID: client, StreamID: inner.StreamID, Message: inner.Message}
		dispatch(inner, func(reply protocol.Envelope) error {
			reply.ClientID = ""
			reply.Message, _ = protocol.WithoutInlineFiles(reply.Message)
			b, err := json.Marshal(reply)
			if err != nil {
				return err
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.writeData(client, s, b)
		})
		return
	}
	r.mu.Unlock()
}

func (r *encryptedRelay) writeData(client string, s *securePeer, plain []byte) error {
	if r.active[client] != s {
		return errors.New("encrypted relay session changed")
	}
	if _, err := os.Stat(r.agent.secureStatePath(client)); err != nil {
		delete(r.active, client)
		return err
	}
	b, err := s.channel.Seal(plain)
	if err != nil {
		return err
	}
	return r.send(client, protocol.SecureData, b)
}

func (r *encryptedRelay) directKey(client string) *securechannel.Key {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.active[client]
	if s == nil {
		return nil
	}
	if _, err := os.Stat(r.agent.secureStatePath(client)); err != nil {
		return nil
	}
	key := s.key
	return &key
}

func (r *encryptedRelay) publish(plain []byte) {
	var e struct {
		ClientID   string
		SkipDirect []string
	}
	if json.Unmarshal(plain, &e) != nil {
		return
	}
	skip := make(map[string]bool)
	if len(e.SkipDirect) > 0 {
		r.agent.eventMu.Lock()
		for client, direct := range r.agent.direct {
			for _, id := range e.SkipDirect {
				if id == direct.id && direct.active && time.Now().Before(direct.lease) {
					skip[client] = true
				}
			}
		}
		r.agent.eventMu.Unlock()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for client, s := range r.active {
		if !skip[client] && (e.ClientID == "" || e.ClientID == client) {
			_ = r.writeData(client, s, plain)
		}
	}
}
