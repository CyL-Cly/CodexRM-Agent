package agenthost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
	"github.com/CyL-Cly/CodexRM-Agent/internal/release"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/CyL-Cly/CodexRM-Server/server"
	"github.com/CyL-Cly/CodexRM-Server/updater"
	"github.com/gorilla/websocket"
)

func TestAutomaticStartupUpdateOnIdleUnconfiguredHost(t *testing.T) {
	feed := manualUpdateFeed(t, release.VersionCode+1)
	h, err := New(filepath.Join(t.TempDir(), "connection.json"), t.TempDir(), clientconfig.Connection{AutoUpdate: true}, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	useUpdateFeed(t, h, feed)
	started := make(chan struct{}, 1)
	h.startUpdate = func(plan *updater.Plan) error {
		plan.Discard()
		started <- struct{}{}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err = h.Run(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	default:
		t.Fatal("startup did not apply the update on an idle host")
	}
}

func TestPendingUpdateWaitsForConversationsAndQueueAfterBrowserDisconnect(t *testing.T) {
	relay, err := server.New(server.Config{DataDir: t.TempDir(), AdminToken: "relay-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	remote := httptest.NewServer(relay.Handler())
	defer remote.Close()
	h, base, token, _ := startHost(t, clientconfig.Connection{Server: remote.URL, EnrollmentKey: "relay-key"}, t.TempDir())
	feed := manualUpdateFeed(t, release.VersionCode+1)
	useUpdateFeed(t, h, feed)
	started := make(chan struct{}, 2)
	h.startUpdate = func(plan *updater.Plan) error {
		plan.Discard()
		started <- struct{}{}
		return nil
	}
	for deadline := time.Now().Add(3 * time.Second); h.snapshot()["status"] != "已连接"; {
		if time.Now().After(deadline) {
			t.Fatalf("agent did not connect: %+v", h.snapshot())
		}
		time.Sleep(time.Millisecond)
	}
	code, body := request(t, base, token, "POST", "/api/pair", nil)
	if code != http.StatusOK {
		t.Fatalf("pair: %d %s", code, body)
	}
	var pair struct{ Code string }
	_ = json.Unmarshal(body, &pair)
	code, body = request(t, remote.URL, "", "POST", "/api/v1/client/claim", map[string]string{"code": pair.Code})
	if code != http.StatusOK {
		t.Fatalf("claim: %d %s", code, body)
	}
	var claim protocol.PairClaimResponse
	_ = json.Unmarshal(body, &claim)
	dial := func() *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(remote.URL, "http")+"/api/v1/client/ws", http.Header{"Authorization": []string{"Bearer " + claim.Token}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	conn := dial()
	nextID := 0
	call := func(method string, params any) map[string]any {
		t.Helper()
		nextID++
		raw, _ := json.Marshal(map[string]any{"id": nextID, "method": method, "params": params})
		if err := conn.WriteJSON(protocol.Envelope{Type: protocol.TypeClientMessage, Message: raw}); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			var env protocol.Envelope
			if err := conn.ReadJSON(&env); err != nil {
				t.Fatal(err)
			}
			var reply struct {
				ID     int            `json:"id"`
				Result map[string]any `json:"result"`
				Error  any            `json:"error"`
			}
			if json.Unmarshal(env.Message, &reply) == nil && reply.ID == nextID {
				if reply.Error != nil {
					t.Fatalf("%s: %v", method, reply.Error)
				}
				return reply.Result
			}
		}
	}
	input := func(threadID string) map[string]any {
		return map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "approval wait"}}}
	}
	info := call("codexrm/info", nil)
	if info["remoteUpdate"] != true || info["version"] != release.VersionName {
		t.Fatalf("remote update capability or release version missing: %+v", info)
	}
	first := call("thread/start", map[string]any{})["thread"].(map[string]any)["id"].(string)
	firstTurn := call("turn/start", input(first))["turn"].(map[string]any)["id"].(string)
	second := call("thread/start", map[string]any{})["thread"].(map[string]any)["id"].(string)
	secondTurn := call("turn/start", input(second))["turn"].(map[string]any)["id"].(string)
	queued := call("codexrm/queue/add", input(second))["entries"].([]any)[0].(map[string]any)["id"].(string)
	updateResult := call("codexrm/update", nil)
	if updateResult["updatePending"] != true {
		t.Fatalf("active remote update did not become pending: %+v", updateResult)
	}
	assertPending := func() {
		t.Helper()
		select {
		case <-started:
			t.Fatal("update interrupted active conversations or lost queued messages")
		case <-time.After(1100 * time.Millisecond):
		}
		if h.snapshot()["updatePending"] != true {
			t.Fatal("pending package was lost")
		}
	}
	_ = conn.Close()
	assertPending() // Browser presence does not determine whether a turn is active.
	conn = dial()
	call("turn/interrupt", map[string]any{"threadId": first, "turnId": firstTurn})
	assertPending() // The other conversation is still active.
	call("turn/interrupt", map[string]any{"threadId": second, "turnId": secondTurn})
	assertPending() // Paused queued messages are retained until resumed or removed.
	call("codexrm/queue/remove", map[string]any{"threadId": second, "id": queued})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("update did not automatically start after all work ended")
	}
	select {
	case <-started:
		t.Fatal("update helper started more than once")
	default:
	}
}
