package server_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

func TestThreadDetachedAndLoadedNotificationsReachClients(t *testing.T) {
	_, h := newServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := agent.New(agent.Config{ServerURL: h.URL, DataDir: t.TempDir(), Cwd: t.TempDir(), Mock: true, EnrollmentKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := a.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var claimed protocol.PairClaimResponse
	post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.ManualPairingCode}, &claimed)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Agent did not stop")
		}
	})
	client := ws(t, h, "client", claimed.Token)
	until(t, client, func(e protocol.Envelope) bool { return e.Status == "connected" })
	rpc(t, client, 1, "thread/start", map[string]any{})
	threadID := response(t, client, 1)["thread"].(map[string]any)["id"].(string)
	callLifecycle := func(id int, method string, params map[string]any, wants ...string) {
		t.Helper()
		rpc(t, client, id, method, params)
		seen := map[string]bool{}
		for len(seen) < len(wants)+1 {
			env := read(t, client)
			var message map[string]any
			_ = json.Unmarshal(env.Message, &message)
			if message["id"] == float64(id) {
				if message["error"] != nil {
					t.Fatalf("%s: %v", method, message["error"])
				}
				seen["response"] = true
			}
			eventParams, _ := message["params"].(map[string]any)
			for _, want := range wants {
				if message["method"] == want && eventParams["threadId"] == threadID {
					seen[want] = true
				}
			}
		}
	}
	callLifecycle(2, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true}, "codexrm/thread/loaded", "codexrm/thread/detached")
	callLifecycle(3, "turn/start", map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "continue"}}}, "codexrm/thread/loaded")
}
