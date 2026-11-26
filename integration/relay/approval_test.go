package server_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

func TestParallelApprovalsWithSameItemHaveIndependentOwnersAndResponses(t *testing.T) {
	_, h := newServer(t)
	ctx, cancel := context.WithCancel(context.Background())
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
	otherPair, err := a.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var other protocol.PairClaimResponse
	post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: otherPair.ManualPairingCode}, &other)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop")
		}
	})
	c := ws(t, h, "client", claimed.Token)
	until(t, c, func(e protocol.Envelope) bool { return e.Status == "connected" })
	c2 := ws(t, h, "client", other.Token)
	rpc(t, c, 1, "thread/start", map[string]any{})
	tid := response(t, c, 1)["thread"].(map[string]any)["id"]
	rpc(t, c, 2, "turn/start", map[string]any{"threadId": tid, "input": []any{map[string]any{"type": "text", "text": "multi approval"}}})
	ids := []any{}
	for len(ids) < 3 {
		var message map[string]any
		_ = json.Unmarshal(read(t, c).Message, &message)
		if message["method"] == "item/commandExecution/requestApproval" {
			ids = append(ids, message["id"])
		}
	}
	// Another paired browser cannot answer this owner's requests. An empty
	// response must not discard a request before the real answer arrives.
	write(t, c2, protocol.Envelope{Type: protocol.TypeClientMessage, Message: raw(map[string]any{"id": ids[0], "result": map[string]any{"decision": "decline"}})})
	write(t, c, protocol.Envelope{Type: protocol.TypeClientMessage, Message: raw(map[string]any{"id": ids[0]})})
	for _, i := range []int{2, 1, 0} {
		write(t, c, protocol.Envelope{Type: protocol.TypeClientMessage, Message: raw(map[string]any{"id": ids[i], "result": map[string]any{"decision": "acceptForSession"}})})
	}
	acks := map[any]bool{}
	completed := false
	for len(acks) < 3 || !completed {
		var message map[string]any
		_ = json.Unmarshal(read(t, c).Message, &message)
		if message["method"] == "codexrm/request/resolved" {
			acks[message["params"].(map[string]any)["id"]] = true
		}
		if message["method"] == "turn/completed" {
			completed = true
			encoded := string(raw(message))
			if strings.Count(encoded, "acceptForSession") != 3 || strings.Contains(encoded, "decline") {
				t.Fatalf("responses mixed or foreign response accepted: %s", encoded)
			}
		}
	}
}
