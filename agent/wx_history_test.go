package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/gorilla/websocket"
)

func TestWxHistoryToolReturnsOnlyThePairedRobotResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peers := make(chan *websocket.Conn, 2)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			peers <- conn
		}
	}))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	app, err := appserver.Dial(ctx, url, "")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	appPeer := <-peers
	defer appPeer.Close()
	relay, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	relayPeer := <-peers
	defer relayPeer.Close()
	_ = appPeer.SetReadDeadline(time.Now().Add(4 * time.Second))
	_ = relayPeer.SetReadDeadline(time.Now().Add(4 * time.Second))

	events := newRelayEventQueue(relay)
	a := &Agent{relay: relay, relayEvents: events}
	go a.writeRelayEvents(ctx, events)
	a.rememberWxThread("wx-thread", "paired-robot", "g:group", "text")
	key := strings.Repeat("ab", 16)
	a.setWxHistorySession(app, "wx-thread", key)
	app.OnNotification(func(raw json.RawMessage) { a.handleWxHistoryTool(app, raw) })
	if err := appPeer.WriteJSON(map[string]any{
		"id": 73, "method": "item/tool/call",
		"params": map[string]any{"threadId": "wx-thread", "tool": wxHistoryToolName, "arguments": map[string]any{"keyword": "previous"}},
	}); err != nil {
		t.Fatal(err)
	}

	var query protocol.Envelope
	if err := relayPeer.ReadJSON(&query); err != nil {
		t.Fatal(err)
	}
	var request struct {
		Method string
		Params struct {
			RequestID string
			Key       string
			Arguments map[string]string
		}
	}
	if err := json.Unmarshal(query.Message, &request); err != nil {
		t.Fatal(err)
	}
	if query.ClientID != "paired-robot" || request.Method != "codexrm/wx/history/query" ||
		request.Params.Key != key || request.Params.Arguments["keyword"] != "previous" {
		t.Fatalf("unexpected history request: %s", query.Message)
	}
	result := map[string]any{"requestId": request.Params.RequestID, "text": "earlier message"}
	if err := a.acceptWxHistoryResult("other-robot", result); err == nil {
		t.Fatal("accepted another robot's result")
	}
	if err := a.acceptWxHistoryResult("paired-robot", result); err != nil {
		t.Fatal(err)
	}
	var reply struct {
		ID     int
		Result struct {
			Success      bool
			ContentItems []struct{ Type, Text string }
		}
	}
	if err := appPeer.ReadJSON(&reply); err != nil {
		t.Fatal(err)
	}
	if reply.ID != 73 || !reply.Result.Success || len(reply.Result.ContentItems) != 1 ||
		reply.Result.ContentItems[0].Type != "inputText" || reply.Result.ContentItems[0].Text != "earlier message" {
		t.Fatalf("unexpected reply: %+v", reply)
	}
}
