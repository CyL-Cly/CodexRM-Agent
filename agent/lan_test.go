package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

func TestLANEventDeliveryWithoutRelayAndAfterExpiry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	auth := &lanAuthorization{}
	direct := &directSession{client: "paired", ctx: ctx, active: true, lanAuth: auth, out: make(chan []byte, 1)}
	agent := &Agent{direct: map[string]*directSession{direct.client: direct}}
	frame := protocol.Envelope{Type: protocol.TypeServerMessage, Message: json.RawMessage(`{"method":"turn/completed"}`)}
	checkNoEvent := func() {
		t.Helper()
		agent.publishEvent(frame)
		select {
		case <-direct.out:
			t.Fatal("event delivered to a LAN link without current authorization")
		default:
		}
	}
	checkNoEvent()
	auth.roster.Store(&lanRoster{until: time.Now().Add(time.Minute), clients: map[string]bool{"paired": true}})
	agent.publishEvent(frame)
	select {
	case <-direct.out:
	default:
		t.Fatal("authorized LAN link did not receive events while RM was disconnected")
	}
	auth.roster.Store(&lanRoster{until: time.Now().Add(-time.Second), clients: map[string]bool{"paired": true}})
	checkNoEvent()
	auth.roster.Store(&lanRoster{until: time.Now().Add(time.Minute), clients: map[string]bool{"other": true}})
	checkNoEvent()
}
