package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Agent/internal/release"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/gorilla/websocket"
)

func TestPairingInstructionsIncludeScannableLink(t *testing.T) {
	a := &Agent{cfg: Config{ServerURL: "https://rm.example/base"}}
	output, err := a.PairingInstructions(&protocol.PairStartResponse{ManualPairingCode: "ABCD-EFGH", ExpiresAt: 1700000000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "https://rm.example/base/#pair=ABCD-EFGH") || !strings.Contains(output, "Scan with the CodexRM app") {
		t.Fatalf("incomplete pairing instructions: %s", output)
	}
}

func TestTurnStartSentWithoutReplyReportsUnknown(t *testing.T) {
	for _, reject := range []bool{false, true} {
		name, want := "lost reply", protocol.RPCOutcomeUnknownCode
		if reject {
			name, want = "explicit rejection", -32602
		}
		t.Run(name, func(t *testing.T) {
			upgrader := websocket.Upgrader{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				var req struct {
					ID     json.RawMessage
					Method string
				}
				if conn.ReadJSON(&req) == nil && req.Method == "turn/start" && reject {
					_ = conn.WriteJSON(map[string]any{"id": req.ID, "error": &appserver.RPCError{Code: want, Message: "invalid model"}})
				}
			}))
			defer upstream.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			app, err := appserver.Dial(ctx, "ws"+strings.TrimPrefix(upstream.URL, "http"))
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			a := &Agent{owners: map[string]string{}}
			var result struct {
				ID    int
				Error *appserver.RPCError
			}
			a.handle(ctx, app, func(reply protocol.Envelope) error {
				return json.Unmarshal(reply.Message, &result)
			}, protocol.Envelope{ClientID: "client", Message: json.RawMessage(`{"id":7,"method":"turn/start","params":{"threadId":"thread"}}`)}, nil)
			if result.ID != 7 || result.Error == nil || result.Error.Code != want {
				t.Fatalf("response = %+v, want error code %d", result, want)
			}
		})
	}
}

func TestDirectLatencyProbeDoesNotReplayApprovals(t *testing.T) {
	pending := json.RawMessage(`{"id":"p","method":"item/commandExecution/requestApproval"}`)
	a := &Agent{approvals: map[string]approval{"p": {Owner: "client", Raw: pending}}}
	for _, tc := range []struct {
		name   string
		direct *directSession
		params string
		probe  bool
	}{
		{"latency probe", &directSession{}, `{"latencyProbe":true}`, true},
		{"direct bootstrap", &directSession{}, `{}`, false},
		{"relay bootstrap", nil, `{"latencyProbe":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var frames []protocol.Envelope
			a.handle(t.Context(), nil, func(reply protocol.Envelope) error {
				frames = append(frames, reply)
				return nil
			}, protocol.Envelope{ClientID: "client", StreamID: "7", Message: json.RawMessage(`{"id":7,"method":"codexrm/info","params":` + tc.params + `}`)}, tc.direct)
			if len(frames) == 0 || frames[0].ClientID != "client" || frames[0].StreamID != "7" {
				t.Fatalf("missing or misrouted reply: %+v", frames)
			}
			var reply struct {
				ID     int
				Result map[string]any
			}
			if err := json.Unmarshal(frames[0].Message, &reply); err != nil || reply.ID != 7 || reply.Result == nil {
				t.Fatalf("invalid reply %s (%v)", frames[0].Message, err)
			}
			if tc.probe {
				if len(reply.Result) != 0 || len(frames) != 1 {
					t.Fatalf("probe unexpectedly included capability or approval data: %s", frames[0].Message)
				}
			} else if reply.Result["workspaces"] != true || len(frames) != 2 || string(frames[1].Message) != string(pending) {
				t.Fatal("bootstrap must still return capability and approval data")
			}
		})
	}
}

func TestResolvedRequestsAreNotReplayed(t *testing.T) {
	a := &Agent{owners: map[string]string{"thread": "client"}, approvals: map[string]approval{}}
	a.forward(mustJSON(map[string]any{"id": 11, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": "thread", "turnId": "turn"}}))
	if len(a.approvals) != 1 {
		t.Fatal("approval not cached")
	}
	a.forward(mustJSON(map[string]any{"method": "serverRequest/resolved", "params": map[string]any{"threadId": "thread", "requestId": 11}}))
	if len(a.approvals) != 0 {
		t.Fatal("resolved approval would be replayed")
	}
	a.forward(mustJSON(map[string]any{"id": "second", "method": "item/fileChange/requestApproval", "params": map[string]any{"threadId": "thread", "turnId": "turn"}}))
	a.forward(mustJSON(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread", "turn": map[string]any{"id": "turn", "status": "interrupted"}}}))
	if len(a.approvals) != 0 {
		t.Fatal("cancelled turn left stale approval")
	}
}

func TestFailedApprovalWriteRemainsRetryable(t *testing.T) {
	mock, err := appserver.StartMock("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	app, err := appserver.Dial(context.Background(), "ws://"+mock.Addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	a := &Agent{approvals: map[string]approval{"pending": {ID: mustJSON(12), Owner: "owner"}}}
	a.handle(context.Background(), app, nil, protocol.Envelope{ClientID: "owner", Message: mustJSON(map[string]any{"id": "pending", "result": map[string]any{"decision": "accept"}})}, nil)
	if p, ok := a.approvals["pending"]; !ok || p.Sending {
		t.Fatal("failed response was lost or left locked")
	}
}

func TestUpstream503NotificationPassesThrough(t *testing.T) {
	raw := mustJSON(map[string]any{"method": "error", "params": map[string]any{
		"threadId": "thread", "turnId": "turn",
		"error": map[string]any{"message": upstream503Message},
	}})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		events := newRelayEventQueue(conn)
		a := &Agent{relay: conn, relayEvents: events}
		a.forward(raw)
		select {
		case frame := <-events.frames:
			_ = a.writeRelayFrame(conn, frame)
		default:
			t.Error("notification was not queued")
		}
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var got protocol.Envelope
	if err := conn.ReadJSON(&got); err != nil {
		t.Fatal(err)
	}
	if got.Type != protocol.TypeServerMessage || string(got.Message) != string(raw) {
		t.Fatalf("forwarded notification = %s, want %s", got.Message, raw)
	}
}

func TestUpstream503InRPCResultPassesThrough(t *testing.T) {
	result := mustJSON(map[string]any{"turn": map[string]any{
		"status": "failed", "error": map[string]any{"message": upstream503Message},
	}})
	var got protocol.Envelope
	send := func(env protocol.Envelope) error {
		got = env
		return nil
	}
	a := &Agent{}
	a.respond(send, protocol.Envelope{Message: json.RawMessage(`{"id":1}`)}, result, nil)
	want := mustJSON(map[string]any{"id": 1, "result": result})
	if got.Type != protocol.TypeServerMessage || string(got.Message) != string(want) {
		t.Fatalf("forwarded RPC response = %s, want %s", got.Message, want)
	}
}

const upstream503Message = "unexpected status 503 Service Unavailable: Service temporarily unavailable, url: https://provider.example/responses, request id: upstream-123"

func TestUnsavedRenewalIsPersistedBeforeAnotherEnroll(t *testing.T) {
	var enrollRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		enrollRequests.Add(1)
		http.Error(w, "unexpected enroll", http.StatusInternalServerError)
	}))
	defer server.Close()

	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{
		cfg:        Config{ServerURL: server.URL, DataDir: filepath.Join(blocker, "state")},
		creds:      creds{Token: "renewed-token", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		credsDirty: true,
	}
	if err := a.ensureEnrolled(context.Background()); err == nil {
		t.Fatal("expected the credential write to fail")
	}
	if got := enrollRequests.Load(); got != 0 {
		t.Fatalf("unsaved credentials triggered %d enrollment requests", got)
	}

	a.cfg.DataDir = t.TempDir()
	a.creds.ExpiresAt = time.Now().Add(48 * time.Hour).Unix()
	if err := a.ensureEnrolled(context.Background()); err != nil {
		t.Fatalf("retry credential write: %v", err)
	}
	if got := enrollRequests.Load(); got != 0 {
		t.Fatalf("successful retry triggered %d enrollment requests", got)
	}
	if a.credsDirty {
		t.Fatal("credentials remained dirty after a successful write")
	}
	data, err := os.ReadFile(filepath.Join(a.cfg.DataDir, "agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved creds
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Token != "renewed-token" {
		t.Fatalf("saved token %q, want renewed token", saved.Token)
	}
}

func TestRelayConnectionReportsCurrentVersion(t *testing.T) {
	var reported atomic.Value
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reported.Store(r.Header.Get(protocol.AppVersionHeader))
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()
	a := &Agent{cfg: Config{ServerURL: server.URL}, creds: creds{Token: "token", AgentID: "agent"}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.session(ctx, nil); err == nil {
		t.Fatal("closed relay was treated as a live session")
	}
	if reported.Load() != release.VersionName {
		t.Fatalf("reported version %v, want %s", reported.Load(), release.VersionName)
	}
}
