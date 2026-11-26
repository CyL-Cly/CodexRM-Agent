package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Agent/internal/onebot"
	"github.com/gorilla/websocket"
)

const oneBotTestPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="

type oneBotTestPeer struct {
	server   *httptest.Server
	actions  chan map[string]any
	errors   chan error
	nextID   atomic.Int64
	response func(map[string]any) (any, int)

	mu     sync.RWMutex
	conn   *websocket.Conn
	write  sync.Mutex
	closed bool
}

func newOneBotTestPeer(t *testing.T) *oneBotTestPeer {
	t.Helper()
	peer := &oneBotTestPeer{actions: make(chan map[string]any, 32), errors: make(chan error, 4)}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	peer.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			peer.errors <- err
			return
		}
		peer.mu.Lock()
		old := peer.conn
		peer.conn = conn
		peer.mu.Unlock()
		if old != nil {
			_ = old.Close()
		}
		defer conn.Close()
		for {
			var request map[string]any
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
			if _, ok := request["action"]; !ok {
				continue
			}
			data := any(map[string]any{"message_id": peer.nextID.Add(1)})
			retcode := 0
			peer.mu.RLock()
			respond := peer.response
			peer.mu.RUnlock()
			if respond != nil {
				data, retcode = respond(request)
			}
			response := map[string]any{"status": "ok", "retcode": retcode, "echo": request["echo"], "data": data}
			peer.write.Lock()
			err = conn.WriteJSON(response)
			peer.write.Unlock()
			if err != nil {
				return
			}
			select {
			case peer.actions <- request:
			default:
				peer.errors <- fmt.Errorf("OneBot action queue full")
			}
		}
	}))
	t.Cleanup(func() {
		peer.mu.Lock()
		peer.closed = true
		conn := peer.conn
		peer.mu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		peer.server.Close()
	})
	return peer
}

func (p *oneBotTestPeer) url() string { return "ws" + strings.TrimPrefix(p.server.URL, "http") }

func (p *oneBotTestPeer) setResponse(fn func(map[string]any) (any, int)) {
	p.mu.Lock()
	p.response = fn
	p.mu.Unlock()
}

func startOneBotPeerClient(t *testing.T, peer *oneBotTestPeer) *onebot.Client {
	t.Helper()
	client, err := onebot.New(onebot.Config{Enabled: true, Mode: "ws", URL: peer.url()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = client.Run(ctx)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for client.Status().State != onebot.StateConnected {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("OneBot peer client did not connect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return client
}

func (p *oneBotTestPeer) inject(t *testing.T, event map[string]any) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.RLock()
		conn := p.conn
		p.mu.RUnlock()
		if conn != nil {
			p.write.Lock()
			err := conn.WriteJSON(event)
			p.write.Unlock()
			if err == nil {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("OneBot peer did not accept an event")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p *oneBotTestPeer) action(t *testing.T) map[string]any {
	t.Helper()
	select {
	case action := <-p.actions:
		return action
	case err := <-p.errors:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for OneBot action")
	}
	return nil
}

func oneBotActionText(action map[string]any) string {
	params, _ := action["params"].(map[string]any)
	message, _ := params["message"].([]any)
	var text strings.Builder
	for _, raw := range message {
		segment, _ := raw.(map[string]any)
		if segment["type"] != "text" {
			continue
		}
		data, _ := segment["data"].(map[string]any)
		value, _ := data["text"].(string)
		text.WriteString(value)
	}
	return text.String()
}

func startOneBotIntegration(t *testing.T, settings OneBotSettings) (*Agent, *oneBotTestPeer, context.CancelFunc) {
	t.Helper()
	mock, err := appserver.StartMock("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	app, err := appserver.Dial(ctx, "ws://"+mock.Addr)
	if err != nil {
		cancel()
		_ = mock.Close()
		t.Fatal(err)
	}
	initCtx, stopInit := context.WithTimeout(ctx, 5*time.Second)
	_, err = app.Initialize(initCtx, "onebot-integration", "1")
	stopInit()
	if err != nil {
		cancel()
		_ = app.Close()
		_ = mock.Close()
		t.Fatal(err)
	}
	dir := t.TempDir()
	a := &Agent{
		cfg:    Config{Mock: true, AppServerURL: "ws://" + mock.Addr, Cwd: dir, DataDir: dir, OneBotSettingsPath: dir + "/onebot.json"},
		owners: map[string]string{}, approvals: map[string]approval{},
	}
	a.providerApp.Store(app)
	a.queueMu.Lock()
	a.queueCtx = ctx
	a.queueMu.Unlock()
	app.OnNotification(func(raw json.RawMessage) {
		a.forward(raw)
		a.queueNotification(ctx, app, raw)
	})
	peer := newOneBotTestPeer(t)
	settings.Enabled = true
	settings.Mode = "ws"
	settings.URL = peer.url()
	settings.Listen = ""
	if settings.Users == nil {
		settings.Users = map[string]OneBotAccess{}
	}
	if settings.Groups == nil {
		settings.Groups = map[string]OneBotGroup{}
	}
	if settings.MessageLimit == 0 {
		settings.MessageLimit = 1800
	}
	if err := a.startOneBot(ctx, app, settings); err != nil {
		cancel()
		_ = app.Close()
		_ = mock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a.stopOneBot()
		_ = app.Close()
		_ = mock.Close()
		cancel()
	})
	deadline := time.Now().Add(5 * time.Second)
	for a.OneBotStatus()["transport"].(onebot.Status).State != onebot.StateConnected {
		if time.Now().After(deadline) {
			t.Fatalf("OneBot did not connect: %#v", a.OneBotStatus())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return a, peer, cancel
}

func TestOneBotPrivateAndGroupEndToEnd(t *testing.T) {
	settings := DefaultOneBotSettings()
	settings.SelfID = 999
	settings.Users["10001"] = OneBotAccess{Enabled: true, Mode: "tools"}
	settings.Groups["20001"] = OneBotGroup{OneBotAccess: OneBotAccess{Enabled: true, Mode: "tools"}, Trigger: "mention", Session: "shared", Admins: []string{"10001"}}
	a, peer, _ := startOneBotIntegration(t, settings)
	peer.inject(t, map[string]any{
		"time": time.Now().Unix(), "self_id": 999, "post_type": "message", "message_type": "private", "message_id": 1, "user_id": 10001,
		"message": []any{map[string]any{"type": "text", "data": map[string]any{"text": "hello onebot"}}},
	})
	action := peer.action(t)
	if action["action"] != "send_msg" || !strings.Contains(oneBotActionText(action), "hello onebot") || !strings.Contains(oneBotActionText(action), "已收到你的任务") {
		t.Fatalf("unexpected private response: %#v", action)
	}
	params := action["params"].(map[string]any)
	if params["user_id"].(float64) != 10001 || params["message_type"] != "private" {
		t.Fatalf("private target mismatch: %#v", params)
	}
	if got := a.OneBotStatus()["sessions"]; got != 1 {
		t.Fatalf("sessions = %#v", got)
	}

	// A group message without the required mention must not start Codex.
	peer.inject(t, map[string]any{"self_id": 999, "post_type": "message", "message_type": "group", "message_id": 2, "group_id": 20001, "user_id": 10001, "message": "plain group text"})
	peer.inject(t, map[string]any{"self_id": 999, "post_type": "message", "message_type": "group", "message_id": 3, "group_id": 20001, "user_id": 10002, "message": "[CQ:at,qq=999] group hello"})
	action = peer.action(t)
	if !strings.Contains(oneBotActionText(action), "group hello") {
		t.Fatalf("unexpected group response: %#v", action)
	}
	params = action["params"].(map[string]any)
	if params["group_id"].(float64) != 20001 || params["message_type"] != "group" {
		t.Fatalf("group target mismatch: %#v", params)
	}

	// A non-admin cannot reset the shared thread; the group admin can.
	peer.inject(t, map[string]any{"self_id": 999, "post_type": "message", "message_type": "group", "message_id": 4, "group_id": 20001, "user_id": 10002, "message": "[CQ:at,qq=999] /new"})
	if action = peer.action(t); !strings.Contains(oneBotActionText(action), "只有群会话管理员") {
		t.Fatalf("non-admin reset was not rejected: %#v", action)
	}
	peer.inject(t, map[string]any{"self_id": 999, "post_type": "message", "message_type": "group", "message_id": 5, "group_id": 20001, "user_id": 10001, "message": "[CQ:at,qq=999] /new"})
	if action = peer.action(t); !strings.Contains(oneBotActionText(action), "下一条消息") {
		t.Fatalf("admin reset failed: %#v", action)
	}
}

func TestRemoteRobotCannotForgeInternalOneBotFlag(t *testing.T) {
	a, _, _ := startOneBotIntegration(t, DefaultOneBotSettings())
	a.cfg.Cwd = t.TempDir()
	a.cfg.DataDir = t.TempDir()
	revision := strings.Repeat("a", 32)
	a.wxMu.Lock()
	a.wxPolicy["remote-robot"] = wxPolicy{Access: map[string]wxAccess{}, Groups: map[string]wxGroupPolicy{
		"20001@chatroom": {Session: "member", Mode: "tools", Revision: revision},
	}}
	a.wxMu.Unlock()
	_, err := a.robotThread(context.Background(), a.providerApp.Load(), "remote-robot", "thread/start", map[string]any{
		"wxId": "10001", "groupId": "20001@chatroom", "groupRevision": revision, "oneBot": true,
	})
	if err == nil || !strings.Contains(err.Error(), "无法验证群工具隔离") {
		t.Fatalf("remote robot bypassed group tool confinement: %v", err)
	}
}

func TestOneBotTextModeEndToEnd(t *testing.T) {
	settings := DefaultOneBotSettings()
	settings.Users["10001"] = OneBotAccess{Enabled: true, Mode: "text"}
	_, peer, _ := startOneBotIntegration(t, settings)
	peer.inject(t, map[string]any{"self_id": 999, "post_type": "message", "message_type": "private", "message_id": 25, "user_id": 10001, "message": "text-only hello"})
	action := peer.action(t)
	if !strings.Contains(oneBotActionText(action), "text-only hello") {
		t.Fatalf("text-only notification did not return a response: %#v", action)
	}
}

func TestOneBotReverseWebSocketEndToEnd(t *testing.T) {
	settings := DefaultOneBotSettings()
	settings.Users["10001"] = OneBotAccess{Enabled: true, Mode: "tools"}
	a, _, _ := startOneBotIntegration(t, settings)
	a.stopOneBot()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	settings.Enabled = true
	settings.Mode = "ws-reverse"
	settings.URL = ""
	settings.Listen = address
	settings.AccessToken = "reverse-secret"
	if err := a.startOneBot(context.Background(), a.providerApp.Load(), settings); err != nil {
		t.Fatal(err)
	}
	var conn *websocket.Conn
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, _, err = websocket.DefaultDialer.Dial("ws://"+address+"/ws", http.Header{"Authorization": []string{"Bearer reverse-secret"}})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reverse WebSocket did not listen: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"self_id": 999, "post_type": "message", "message_type": "private", "message_id": 30, "user_id": 10001, "message": "reverse hello"}); err != nil {
		t.Fatal(err)
	}
	var action map[string]any
	if err := conn.ReadJSON(&action); err != nil {
		t.Fatal(err)
	}
	if action["action"] != "send_msg" || !strings.Contains(oneBotActionText(action), "reverse hello") {
		t.Fatalf("reverse response mismatch: %#v", action)
	}
	if err := conn.WriteJSON(map[string]any{"status": "ok", "retcode": 0, "echo": action["echo"], "data": map[string]any{"message_id": 30}}); err != nil {
		t.Fatal(err)
	}
}

func TestOneBotRequestsAreLimitedToAuthorizedTargets(t *testing.T) {
	settings := DefaultOneBotSettings()
	settings.AutoAcceptFriend = true
	settings.AutoAcceptGroup = true
	settings.Users["10001"] = OneBotAccess{Enabled: true, Mode: "tools"}
	settings.Groups["20001"] = OneBotGroup{OneBotAccess: OneBotAccess{Enabled: true, Mode: "tools"}, Trigger: "mention", Session: "shared"}
	_, peer, _ := startOneBotIntegration(t, settings)
	peer.inject(t, map[string]any{"self_id": 999, "post_type": "request", "request_type": "friend", "user_id": 10002, "flag": "denied-friend"})
	peer.inject(t, map[string]any{"self_id": 999, "post_type": "request", "request_type": "friend", "user_id": 10001, "flag": "allowed-friend"})
	action := peer.action(t)
	params := action["params"].(map[string]any)
	if action["action"] != "set_friend_add_request" || params["flag"] != "allowed-friend" || params["approve"] != true {
		t.Fatalf("friend request handling mismatch: %#v", action)
	}
	peer.inject(t, map[string]any{"self_id": 999, "post_type": "request", "request_type": "group", "sub_type": "add", "group_id": 20001, "user_id": 10001, "flag": "denied-join"})
	peer.inject(t, map[string]any{"self_id": 999, "post_type": "request", "request_type": "group", "sub_type": "invite", "group_id": 20001, "user_id": 10001, "flag": "allowed-invite"})
	action = peer.action(t)
	params = action["params"].(map[string]any)
	if action["action"] != "set_group_add_request" || params["flag"] != "allowed-invite" || params["sub_type"] != "invite" {
		t.Fatalf("group request handling mismatch: %#v", action)
	}
}

func TestOneBotShutdownInterruptsActiveTurn(t *testing.T) {
	settings := DefaultOneBotSettings()
	settings.Users["10001"] = OneBotAccess{Enabled: true, Mode: "tools"}
	a, peer, _ := startOneBotIntegration(t, settings)
	peer.inject(t, map[string]any{"self_id": 999, "post_type": "message", "message_type": "private", "message_id": 20, "user_id": 10001, "message": "interrupt on shutdown"})
	var threadID, turnID string
	deadline := time.Now().Add(5 * time.Second)
	for turnID == "" {
		a.oneBotMu.RLock()
		runtime := a.oneBot
		a.oneBotMu.RUnlock()
		if runtime != nil {
			runtime.mu.Lock()
			if active := runtime.active["p:10001"]; active != nil {
				threadID, turnID = active.ThreadID, active.TurnID
			}
			runtime.mu.Unlock()
		}
		if turnID == "" && time.Now().After(deadline) {
			t.Fatal("OneBot turn did not start")
		}
		if turnID == "" {
			time.Sleep(5 * time.Millisecond)
		}
	}
	a.stopOneBot()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := a.providerApp.Load().Call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Thread struct {
			Turns []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, turn := range snapshot.Thread.Turns {
		if turn.ID == turnID && turn.Status == "interrupted" {
			return
		}
	}
	t.Fatalf("active turn was not interrupted during shutdown: %#v", snapshot)
}

func TestOneBotShutdownInterruptsSharedGroupTurnStartedByMember(t *testing.T) {
	settings := DefaultOneBotSettings()
	settings.Groups["20001"] = OneBotGroup{OneBotAccess: OneBotAccess{Enabled: true, Mode: "tools"}, Trigger: "mention", Session: "shared", Admins: []string{"10001"}}
	a, peer, _ := startOneBotIntegration(t, settings)
	peer.inject(t, map[string]any{"self_id": 999, "post_type": "message", "message_type": "group", "message_id": 21, "group_id": 20001, "user_id": 10002, "message": "[CQ:at,qq=999] member started turn"})
	var threadID, turnID string
	deadline := time.Now().Add(5 * time.Second)
	for turnID == "" {
		a.oneBotMu.RLock()
		runtime := a.oneBot
		a.oneBotMu.RUnlock()
		if runtime != nil {
			runtime.mu.Lock()
			if active := runtime.active["g:20001"]; active != nil {
				threadID, turnID = active.ThreadID, active.TurnID
			}
			runtime.mu.Unlock()
		}
		if turnID == "" && time.Now().After(deadline) {
			t.Fatal("shared group turn did not start")
		}
		if turnID == "" {
			time.Sleep(5 * time.Millisecond)
		}
	}
	a.stopOneBot()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := a.providerApp.Load().Call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Thread struct {
			Turns []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, turn := range snapshot.Thread.Turns {
		if turn.ID == turnID && turn.Status == "interrupted" {
			return
		}
	}
	t.Fatalf("member-started group turn was not interrupted: %#v", snapshot)
}

func TestOneBotLateCompletionCannotFinishPendingTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	turn := &oneBotTurn{ThreadID: "thread", TurnID: "", Cancel: cancel, Items: map[string]string{}}
	r := &oneBotRuntime{ctx: ctx, active: map[string]*oneBotTurn{"key": turn}, sessions: map[string]*oneBotSession{}}
	oldCompletion := mustJSON(map[string]any{"method": "turn/completed", "params": map[string]any{
		"threadId": "thread", "turn": map[string]any{"id": "old-turn", "status": "interrupted"},
	}})
	r.handleNotification(oldCompletion)
	r.mu.Lock()
	if r.active["key"] != turn || turn.TurnID != "" {
		t.Fatalf("late completion consumed a pending turn: %#v", turn)
	}
	r.mu.Unlock()
	r.handleNotification(mustJSON(map[string]any{"method": "turn/started", "params": map[string]any{
		"threadId": "thread", "turn": map[string]any{"id": "new-turn", "status": "inProgress"},
	}}))
	r.handleNotification(oldCompletion)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active["key"] != turn || turn.TurnID != "new-turn" {
		t.Fatalf("old completion removed the new active turn: %#v", turn)
	}
}

func TestOneBotActiveReloadUsesANewThread(t *testing.T) {
	settings := DefaultOneBotSettings()
	settings.Users["10001"] = OneBotAccess{Enabled: true, Mode: "tools"}
	a, peer, _ := startOneBotIntegration(t, settings)
	peer.inject(t, map[string]any{"self_id": 999, "post_type": "message", "message_type": "private", "message_id": 40, "user_id": 10001, "message": "long running before reload"})
	var oldThread string
	deadline := time.Now().Add(5 * time.Second)
	for oldThread == "" {
		a.oneBotMu.RLock()
		runtime := a.oneBot
		a.oneBotMu.RUnlock()
		if runtime != nil {
			runtime.mu.Lock()
			if active := runtime.active["p:10001"]; active != nil && active.TurnID != "" {
				oldThread = active.ThreadID
			}
			runtime.mu.Unlock()
		}
		if oldThread == "" && time.Now().After(deadline) {
			t.Fatal("turn did not start before reload")
		}
		if oldThread == "" {
			time.Sleep(5 * time.Millisecond)
		}
	}
	a.stopOneBot()
	settings.Enabled, settings.Mode, settings.URL, settings.Listen = true, "ws", peer.url(), ""
	if err := a.startOneBot(context.Background(), a.providerApp.Load(), settings); err != nil {
		t.Fatal(err)
	}
	peer.inject(t, map[string]any{"self_id": 999, "post_type": "message", "message_type": "private", "message_id": 41, "user_id": 10001, "message": "new turn after active reload"})
	if text := oneBotActionText(peer.action(t)); !strings.Contains(text, "new turn after active reload") {
		t.Fatalf("new turn response mismatch: %s", text)
	}
	a.oneBotMu.RLock()
	runtime := a.oneBot
	a.oneBotMu.RUnlock()
	runtime.mu.Lock()
	newThread := runtime.sessions["p:10001"].ThreadID
	runtime.mu.Unlock()
	if newThread == oldThread {
		t.Fatalf("active thread was reused across runtime generations: %s", oldThread)
	}
}

func TestOneBotSessionReloadAndUnauthorizedIgnore(t *testing.T) {
	settings := DefaultOneBotSettings()
	settings.Users["10001"] = OneBotAccess{Enabled: true, Mode: "tools"}
	a, peer, _ := startOneBotIntegration(t, settings)
	event := map[string]any{"self_id": 999, "post_type": "message", "message_type": "private", "message_id": 10, "user_id": 10001, "message": "before reload"}
	peer.inject(t, event)
	if text := oneBotActionText(peer.action(t)); !strings.Contains(text, "before reload") {
		t.Fatalf("first response: %s", text)
	}
	a.stopOneBot()
	settings.Enabled, settings.Mode, settings.URL, settings.Listen = true, "ws", peer.url(), ""
	if err := a.startOneBot(context.Background(), a.providerApp.Load(), settings); err != nil {
		t.Fatal(err)
	}
	peer.inject(t, map[string]any{"self_id": 999, "post_type": "message", "message_type": "private", "message_id": 11, "user_id": 10001, "message": "after reload"})
	if text := oneBotActionText(peer.action(t)); !strings.Contains(text, "after reload") {
		t.Fatalf("reloaded response: %s", text)
	}
	// This sender is ignored. The following authorized command proves ordering
	// without a timing-only assertion for the ignored message.
	peer.inject(t, map[string]any{"self_id": 999, "post_type": "message", "message_type": "private", "message_id": 12, "user_id": 10003, "message": "must be ignored"})
	peer.inject(t, map[string]any{"self_id": 999, "post_type": "message", "message_type": "private", "message_id": 13, "user_id": 10001, "message": "/id"})
	action := peer.action(t)
	if text := oneBotActionText(action); !strings.Contains(text, "QQ：10001") || strings.Contains(text, "must be ignored") {
		t.Fatalf("authorization filtering failed: %#v", action)
	}
}

// go-cqhttp's get_msg puts the author in sender.user_id and omits a top-level
// user_id. Reply context must still work and must not cross conversations.
func TestOneBotGoCQHTTPGetMsgSenderShape(t *testing.T) {
	peer := newOneBotTestPeer(t)
	client := startOneBotPeerClient(t, peer)
	peer.setResponse(func(request map[string]any) (any, int) {
		params, _ := request["params"].(map[string]any)
		if request["action"] == "get_msg" && fmt.Sprint(params["message_id"]) == "55" {
			return map[string]any{
				"message_type": "private",
				"sender":       map[string]any{"user_id": 10001, "nickname": "Alice"},
				"raw_message":  "quoted context",
				"message":      []any{map[string]any{"type": "text", "data": map[string]any{"text": "quoted context"}}},
			}, 0
		}
		return map[string]any{"message_id": 1}, 0
	})
	r := &oneBotRuntime{client: client, settings: DefaultOneBotSettings()}
	message := onebot.Message{
		{Type: "reply", Data: map[string]any{"id": "55"}},
		{Type: "text", Data: map[string]any{"text": "please summarize"}},
	}
	input, err := r.messageInput(context.Background(), onebot.Event{MessageType: "private", UserID: 10001, SelfID: 999}, message)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := input[0].(map[string]any)["text"].(string)
	if !strings.Contains(first, "quoted context") || !strings.Contains(first, "回复 QQ 10001") {
		t.Fatalf("go-cqhttp sender.user_id reply context was dropped: %q", first)
	}
}

func TestOneBotGoCQHTTPReplyFromAnotherGroupIsIgnored(t *testing.T) {
	peer := newOneBotTestPeer(t)
	client := startOneBotPeerClient(t, peer)
	peer.setResponse(func(request map[string]any) (any, int) {
		if request["action"] == "get_msg" {
			return map[string]any{
				"message_type": "group",
				"group_id":     20002,
				"sender":       map[string]any{"user_id": 10001},
				"raw_message":  "secret from another group",
				"message":      []any{map[string]any{"type": "text", "data": map[string]any{"text": "secret from another group"}}},
			}, 0
		}
		return map[string]any{"message_id": 1}, 0
	})
	r := &oneBotRuntime{client: client, settings: DefaultOneBotSettings()}
	message := onebot.Message{
		{Type: "reply", Data: map[string]any{"id": "55"}},
		{Type: "text", Data: map[string]any{"text": "read it back"}},
	}
	input, err := r.messageInput(context.Background(), onebot.Event{MessageType: "group", GroupID: 20001, UserID: 10001, SelfID: 999}, message)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(input)
	if strings.Contains(string(payload), "secret from another group") {
		t.Fatalf("cross-group reply leaked into Codex input: %s", payload)
	}
}

// go-cqhttp's get_image returns size/filename/url instead of file. The URL must
// be downloaded through the same SSRF guards, never treated as a host path.
func TestOneBotGoCQHTTPGetImageURL(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString(oneBotTestPNGBase64)
	if err != nil {
		t.Fatal(err)
	}
	imageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	defer imageServer.Close()
	peer := newOneBotTestPeer(t)
	client := startOneBotPeerClient(t, peer)
	peer.setResponse(func(request map[string]any) (any, int) {
		if request["action"] == "get_image" {
			return map[string]any{"size": len(png), "filename": "abc.image", "url": imageServer.URL + "/image"}, 0
		}
		return map[string]any{"message_id": 1}, 0
	})
	settings := DefaultOneBotSettings()
	settings.AllowRemoteMedia = true
	settings.AllowPrivateMedia = true
	r := &oneBotRuntime{client: client, settings: settings}
	data, err := r.loadImage(context.Background(), onebot.Segment{Type: "image", Data: map[string]any{"file": "abc.image"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(data, "data:image/png;base64,") {
		t.Fatalf("go-cqhttp get_image url was not used: %q", data)
	}
}

// go-cqhttp expects message_id (string) for get_forward_msg; implementations
// using id are also supported. A failed first attempt must retry, not drop the
// transcript, and the returned content+sender shape must be parsed.
func TestOneBotGoCQHTTPForwardMessageIDFallback(t *testing.T) {
	peer := newOneBotTestPeer(t)
	client := startOneBotPeerClient(t, peer)
	var legacyCalls atomic.Int64
	peer.setResponse(func(request map[string]any) (any, int) {
		params, _ := request["params"].(map[string]any)
		if request["action"] == "get_forward_msg" {
			if _, ok := params["id"]; ok {
				legacyCalls.Add(1)
				return map[string]any{}, 1400
			}
			return map[string]any{"messages": []any{
				map[string]any{"sender": map[string]any{"user_id": 10001, "nickname": "Alice"}, "content": []any{
					map[string]any{"type": "text", "data": map[string]any{"text": "forwarded line one"}},
				}},
			}}, 0
		}
		return map[string]any{"message_id": 1}, 0
	})
	settings := DefaultOneBotSettings()
	settings.AllowForwardLookup = true
	r := &oneBotRuntime{client: client, settings: settings}
	transcript := r.forwardTranscript(context.Background(), "fwd-1")
	if !strings.Contains(transcript, "forwarded line one") || !strings.Contains(transcript, "QQ 10001") {
		t.Fatalf("go-cqhttp forward transcript was not read: %q", transcript)
	}
	if legacyCalls.Load() != 1 {
		t.Fatalf("expected one id call before the message_id fallback, got %d", legacyCalls.Load())
	}
}
