package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

// Exercise Codex's real session permission grants without model credentials or a live provider.
func TestSessionApprovalAcrossTurnsWithLocalCodex(t *testing.T) {
	executable := os.Getenv("CODEXRM_TEST_CODEX")
	if executable == "" {
		t.Skip("set CODEXRM_TEST_CODEX to test real session approvals")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		call := calls.Add(1)
		responseID := fmt.Sprintf("fixture-%d", call)
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(event map[string]any) {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], mustJSON(event))
		}
		emit(map[string]any{"type": "response.created", "response": map[string]any{"id": responseID}})
		if call == 1 {
			emit(map[string]any{"type": "response.output_item.done", "item": map[string]any{
				"type": "custom_tool_call", "call_id": responseID, "name": "exec",
				"input": `await tools.request_permissions({permissions: {file_system: {write: ["."]}}, reason: "Write the fixture file"});`,
			}})
		} else if call == 3 {
			emit(map[string]any{"type": "response.output_item.done", "item": map[string]any{
				"type": "custom_tool_call", "call_id": responseID, "name": "exec",
				"input": `text(await tools.apply_patch("*** Begin Patch\n*** Add File: session-approval.txt\n+session grant retained\n*** End Patch"));`,
			}})
		}
		emit(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": responseID, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		}})
	}))
	defer upstream.Close()
	codexDir := t.TempDir()
	t.Setenv("CODEX_HOME", codexDir)
	t.Setenv("RUST_LOG", "error")
	config := fmt.Sprintf("model = 'gpt-5.5'\nmodel_provider = 'fixture'\n[features]\nrequest_permissions_tool = true\ncode_mode_only = true\n[model_providers.fixture]\nname = 'Approval fixture'\nbase_url = %q\nwire_api = 'responses'\nrequires_openai_auth = false\n", upstream.URL)
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	app, err := appserver.Launch(ctx, executable, cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if _, err := app.Initialize(ctx, "codexrm-approval-test", "0.0.0"); err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: Config{DataDir: t.TempDir()}, owners: map[string]string{}, approvals: map[string]approval{}, queueCtx: ctx}
	events := make(chan json.RawMessage, 256)
	app.OnNotification(func(raw json.RawMessage) {
		a.forward(raw)
		a.queueNotification(ctx, app, raw)
		events <- raw
	})
	threadID := lifecycleCall(t, a, app, "thread/start", map[string]any{
		"cwd": cwd, "sandbox": "read-only", "approvalPolicy": "on-request",
	})["thread"].(map[string]any)["id"].(string)
	start := func(text string) {
		params := queueInput(threadID, text)
		delete(params, "model")
		lifecycleCall(t, a, app, "turn/start", params)
	}
	runTurn := func(wantApproval bool) {
		t.Helper()
		approved := false
		for {
			select {
			case raw := <-events:
				var event struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Params struct {
						Permissions json.RawMessage `json:"permissions"`
						Turn        struct{ Status string }
					}
				}
				if err := json.Unmarshal(raw, &event); err != nil {
					t.Fatal(err)
				}
				switch event.Method {
				case "item/fileChange/requestApproval":
					t.Fatalf("session grant was lost: %s", raw)
				case "item/permissions/requestApproval":
					if !wantApproval || approved {
						t.Fatalf("unexpected repeat approval: %s", raw)
					}
					a.mu.Lock()
					var key string
					for id, pending := range a.approvals {
						if pending.ThreadID == threadID && string(pending.ID) == string(event.ID) {
							key = id
							break
						}
					}
					a.mu.Unlock()
					if key == "" {
						t.Fatal("Agent did not register the approval request")
					}
					a.handle(ctx, app, nil, protocol.Envelope{ClientID: "owner", Message: mustJSON(map[string]any{
						"id": key, "result": map[string]any{"permissions": event.Params.Permissions, "scope": "session"},
					})}, nil)
					approved = true
				case "turn/completed":
					if approved != wantApproval || event.Params.Turn.Status != "completed" {
						t.Fatalf("prompted=%t (wanted=%t), turn=%s", approved, wantApproval, raw)
					}
					return
				}
			case <-ctx.Done():
				t.Fatal("timed out waiting for the turn")
			}
		}
	}
	start("Request write access to the fixture directory.")
	runTurn(true)
	a.releaseIdleThread(ctx, app, threadID)
	start("Write the fixture file using the previously granted access.")
	runTurn(false)
	if data, err := os.ReadFile(filepath.Join(cwd, "session-approval.txt")); err != nil || string(data) != "session grant retained\n" {
		t.Fatalf("expected the next turn to write using the session grant: %q, %v", data, err)
	}
	lifecycleCall(t, a, app, "thread/archive", map[string]any{"threadId": threadID})
	waitQueue(t, a, threadID, func(q *messageQueue) bool { return q.Paused && !q.SessionApproval })
	if loaded := queueCall(t, app, "thread/loaded/list", map[string]any{})["data"].([]any); len(loaded) != 0 {
		t.Fatalf("archive retained the live permissions session: %v", loaded)
	}
}
