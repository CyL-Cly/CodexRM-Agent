package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

func testRobotAgent(t *testing.T) (*Agent, *appserver.MockServer) {
	t.Helper()
	mock, err := appserver.StartMock("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mock.Close() })
	dir := t.TempDir()
	return &Agent{
		cfg:       Config{Mock: true, AppServerURL: "ws://" + mock.Addr, Cwd: dir, DataDir: dir},
		owners:    map[string]string{},
		approvals: map[string]approval{},
	}, mock
}

func robotCall(t *testing.T, a *Agent, clientID, method string, params map[string]any) (map[string]any, protocol.Envelope) {
	t.Helper()
	var got protocol.Envelope
	env := protocol.Envelope{Type: protocol.TypeClientMessage, ClientID: clientID, ClientKind: protocol.ClientKindRobot, Message: mustJSON(map[string]any{"id": 7, "method": method, "params": params})}
	a.handleRobot(context.Background(), nil, func(out protocol.Envelope) error { got = out; return nil }, env, method, params)
	var message map[string]any
	if err := json.Unmarshal(got.Message, &message); err != nil {
		t.Fatal(err)
	}
	return message, got
}

func TestWxExecIsGrantedPerUserAndRejectsShell(t *testing.T) {
	a, _ := testRobotAgent(t)
	if err := a.setWxPolicy("cli", map[string]any{
		"enabled": []any{"codexrm-echo"},
		"access": map[string]any{
			"wx-a": map[string]any{"enabled": true, "mode": "text", "commands": []any{"codexrm-echo"}},
			"wx-b": map[string]any{"enabled": true, "mode": "text", "commands": []any{}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ok, _ := robotCall(t, a, "cli", "codexrm/wx/exec", map[string]any{"wxId": "wx-a", "command": "codexrm-echo", "args": []any{"pong"}})
	if ok["error"] != nil {
		t.Fatal(ok)
	}
	if ok["result"].(map[string]any)["text"] != "pong" {
		t.Fatalf("echo: %#v", ok)
	}
	denied, _ := robotCall(t, a, "cli", "codexrm/wx/exec", map[string]any{"wxId": "wx-b", "command": "codexrm-echo", "args": []any{"pong"}})
	if denied["error"] == nil {
		t.Fatal("ungranted command ran")
	}
	raw, err := json.Marshal(map[string]any{"commands": []map[string]any{{
		"name": "run-shell", "argv0": filepath.Join(os.Getenv("SYSTEMROOT"), "System32", "cmd.exe"), "workDir": a.cfg.Cwd, "maxArgs": 1,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.cfg.DataDir, "wx-commands.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.setWxPolicy("cli", map[string]any{"enabled": []any{"run-shell"}, "access": map[string]any{"wx-a": map[string]any{"enabled": true, "mode": "text", "commands": []any{"run-shell"}}}}); err == nil {
		t.Fatal("shell command was accepted into the registry")
	}
}

func TestRobotThreadsAreIsolatedAndTextOnly(t *testing.T) {
	a, _ := testRobotAgent(t)
	if err := a.setWxPolicy("cli", map[string]any{
		"enabled": []any{},
		"access": map[string]any{
			"wx-a": map[string]any{"enabled": true, "mode": "text", "commands": []any{}},
			"wx-b": map[string]any{"enabled": true, "mode": "text", "commands": []any{}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	started, _ := robotCall(t, a, "cli", "thread/start", map[string]any{"wxId": "wx-a", "sandbox": "danger-full-access", "approvalPolicy": "on-request"})
	if started["error"] != nil {
		t.Fatal(started)
	}
	thread := started["result"].(map[string]any)["thread"].(map[string]any)
	if started["result"].(map[string]any)["approvalPolicy"] != "never" || started["result"].(map[string]any)["sandbox"] != "read-only" {
		t.Fatalf("text-only settings were not forced: %#v", started["result"])
	}
	stopped, _ := robotCall(t, a, "cli", "turn/interrupt", map[string]any{"wxId": "wx-b", "threadId": thread["id"], "turnId": "turn"})
	if stopped["error"] == nil {
		t.Fatal("other wxid interrupted the thread")
	}
}

func TestRobotAutomationRequiresAccessAndOwnsSeparateThread(t *testing.T) {
	a, _ := testRobotAgent(t)
	t.Cleanup(a.closeTextApp)
	if err := a.setWxPolicy("robot", map[string]any{
		"access": map[string]any{"wx-author": map[string]any{"enabled": true, "mode": "tools"}},
	}); err != nil {
		t.Fatal(err)
	}
	input := []any{map[string]any{"type": "text", "text": "Summarize this"}}
	params := map[string]any{"wxId": "stranger", "instructions": "Summarize", "input": input}
	if out, _ := robotCall(t, a, "robot", "codexrm/wx/task/start", params); out["error"] == nil {
		t.Fatal("unauthorized automation started")
	}
	params["wxId"] = "wx-author"
	out, _ := robotCall(t, a, "robot", "codexrm/wx/task/start", params)
	result, ok := out["result"].(map[string]any)
	if !ok || result["threadId"] == "" || result["turnId"] == "" {
		t.Fatalf("start returned %#v", out)
	}
	threadID := result["threadId"].(string)
	closeParams := map[string]any{"threadId": threadID, "turnId": result["turnId"]}
	if out, _ := robotCall(t, a, "other", "codexrm/wx/task/close", closeParams); out["error"] == nil {
		t.Fatal("another robot closed the automation")
	}
	if out, _ := robotCall(t, a, "robot", "turn/start", map[string]any{"threadId": threadID, "wxId": "wx-author", "input": input}); out["error"] == nil {
		t.Fatal("chat could continue the automation thread")
	}
	if out, _ := robotCall(t, a, "robot", "codexrm/wx/task/close", closeParams); out["error"] != nil {
		t.Fatalf("close returned %#v", out)
	}
	if _, err := a.ownThread("robot", "", threadID); err == nil {
		t.Fatal("closed thread was retained")
	}
}

func TestRobotSteersOnlyItsActiveTurn(t *testing.T) {
	a, _ := testRobotAgent(t)
	t.Cleanup(a.closeTextApp)
	if err := a.setWxPolicy("cli", map[string]any{
		"access": map[string]any{
			"wx-a": map[string]any{"enabled": true, "mode": "text"},
			"wx-b": map[string]any{"enabled": true, "mode": "text"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	start, _ := robotCall(t, a, "cli", "thread/start", map[string]any{"wxId": "wx-a"})
	if start["error"] != nil {
		t.Fatal(start)
	}
	threadID := start["result"].(map[string]any)["thread"].(map[string]any)["id"]
	input := func(text string) []any { return []any{map[string]any{"type": "text", "text": text}} }
	turn, _ := robotCall(t, a, "cli", "turn/start", map[string]any{
		"wxId": "wx-a", "threadId": threadID, "input": input("primary"),
	})
	if turn["error"] != nil {
		t.Fatal(turn)
	}
	turnID := turn["result"].(map[string]any)["turn"].(map[string]any)["id"]
	result, _ := robotCall(t, a, "cli", "turn/steer", map[string]any{
		"wxId": "wx-a", "threadId": threadID, "expectedTurnId": turnID, "input": input("follow-up"),
	})
	if result["error"] != nil || result["result"].(map[string]any)["turnId"] != turnID {
		t.Fatalf("steer did not return the original turn: %#v", result)
	}
	for _, tc := range []struct {
		actor string
		id    any
	}{{"wx-b", turnID}, {"wx-a", "stale-turn"}, {"wx-a", ""}} {
		result, _ := robotCall(t, a, "cli", "turn/steer", map[string]any{
			"wxId": tc.actor, "threadId": threadID, "expectedTurnId": tc.id, "input": input("rejected"),
		})
		if result["error"] == nil {
			t.Fatalf("unexpected steer for %s/%s: %#v", tc.actor, tc.id, result)
		}
	}
}

func TestRobotNotificationsStayTargeted(t *testing.T) {
	a, _ := testRobotAgent(t)
	a.rememberWxThread("thread-a", "robot-client", "wx-a", "text")
	if got := a.robotNotificationClient("", "thread-a"); got != "robot-client" {
		t.Fatalf("robot owner = %q", got)
	}
	if got := a.robotNotificationClient("already", "thread-a"); got != "" {
		t.Fatal("targeted event was duplicated")
	}
	if got := a.robotNotificationClient("", "missing"); got != "" {
		t.Fatal("unknown thread was forwarded")
	}
}

func TestTextToolEventBlocksExec(t *testing.T) {
	if !textToolEvent("item/started", "exec") || !textToolEvent("item/commandExecution/requestApproval", "") {
		t.Fatal("tool event was not blocked")
	}
	if textToolEvent("item/agentMessage/delta", "agentMessage") {
		t.Fatal("assistant text was treated as a tool")
	}
}

func TestRobotCannotContinueToolThreadAfterDowngrade(t *testing.T) {
	a, _ := testRobotAgent(t)
	a.rememberWxThread("old", "cli", "wx-a", "tools")
	if err := a.setWxPolicy("cli", map[string]any{
		"access": map[string]any{"wx-a": map[string]any{"enabled": true, "mode": "text"}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"turn/start", "turn/steer", "thread/resume"} {
		result, _ := robotCall(t, a, "cli", method, map[string]any{
			"wxId": "wx-a", "threadId": "old",
			"input": []any{map[string]any{"type": "text", "text": "hello"}},
		})
		rpcErr, ok := result["error"].(map[string]any)
		if !ok || rpcErr["message"] != "无权操作该会话" {
			t.Fatalf("%s did not reject the old tool-enabled thread: %#v", method, result)
		}
	}
}

func TestRobotImagesAndAdminSandbox(t *testing.T) {
	const png = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="
	for _, mode := range []string{"text", "tools", "admin"} {
		params := map[string]any{"approvalPolicy": "on-request", "input": []any{
			map[string]any{"type": "image", "url": png},
		}}
		if err := constrainRobotParams("turn/start", params, t.TempDir(), mode); err != nil {
			t.Fatalf("%s image rejected: %v", mode, err)
		}
		want := map[string]string{"text": "readOnly", "tools": "workspaceWrite", "admin": "dangerFullAccess"}[mode]
		if params["approvalPolicy"] != "never" || params["sandboxPolicy"].(map[string]any)["type"] != want {
			t.Fatalf("%s: unexpected permissions: %#v", mode, params)
		}
	}
	for _, input := range []map[string]any{
		{"type": "localImage", "path": "/tmp/test.png"},
		{"type": "image", "url": "http://127.0.0.1/test.png"},
		{"type": "image", "url": "data:image/jpeg;base64," + png[len("data:image/png;base64,"):]},
	} {
		if _, err := robotInputWithLimit([]any{input}, 2); err == nil {
			t.Fatal("accepted untrusted image reference")
		}
	}
}

func TestRobotAdminRequiresLocalBinding(t *testing.T) {
	a, _ := testRobotAgent(t)
	if err := a.setWxPolicy("cli", map[string]any{
		"access": map[string]any{"wx-a": map[string]any{"enabled": true, "mode": "admin"}},
	}); err != nil {
		t.Fatal(err)
	}
	result, _ := robotCall(t, a, "cli", "thread/start", map[string]any{"wxId": "wx-a"})
	if rpcErr, ok := result["error"].(map[string]any); !ok || rpcErr["code"] != float64(-32602) {
		t.Fatalf("unbound admin result: %#v", result)
	}
	if err := os.WriteFile(filepath.Join(a.cfg.DataDir, "wx-admins.json"), []byte(`{"cli":["wx-a"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !a.wxAdmin("cli", "wx-a") || a.wxAdmin("other", "wx-a") || a.wxAdmin("cli", "other") {
		t.Fatal("local binding was not scoped to the paired robot and sender")
	}
}
