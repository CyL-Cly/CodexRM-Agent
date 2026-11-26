package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
)

// Use two real app-server processes and one isolated CODEX_HOME to exercise
// the same durable writer conflict as Agent versus desktop, without live APIs.
func TestThreadHandoffWithLocalCodex(t *testing.T) {
	executable := os.Getenv("CODEXRM_TEST_CODEX")
	if executable == "" {
		t.Skip("set CODEXRM_TEST_CODEX to test real Codex thread handoff")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	requests := make(chan chan struct{}, 10)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gate := make(chan struct{})
		select {
		case requests <- gate:
		case <-ctx.Done():
			return
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-test\"}}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-test\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
	}))
	defer upstream.Close()
	defer cancel()
	codexDir := t.TempDir()
	t.Setenv("CODEX_HOME", codexDir)
	t.Setenv("RUST_LOG", "error")
	config := fmt.Sprintf("model='test-model'\nmodel_provider='fixture'\n[model_providers.fixture]\nname='fixture'\nbase_url=%q\nwire_api='responses'\nrequires_openai_auth=false\n", upstream.URL)
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	launch := func() *appserver.Client {
		t.Helper()
		app, err := appserver.Launch(ctx, executable, cwd)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = app.Close() })
		if _, err := app.Initialize(ctx, "codexrm-handoff-test", "0.0.0"); err != nil {
			t.Fatal(err)
		}
		return app
	}
	app, desktop := launch(), launch()
	a := &Agent{cfg: Config{DataDir: t.TempDir()}, owners: map[string]string{}, approvals: map[string]approval{}, queueCtx: ctx}
	app.OnNotification(func(raw json.RawMessage) {
		a.forward(raw)
		a.queueNotification(ctx, app, raw)
	})
	desktopEvents := make(chan json.RawMessage, 256)
	desktop.OnNotification(func(raw json.RawMessage) { desktopEvents <- raw })
	nextRequest := func() chan struct{} {
		t.Helper()
		select {
		case gate := <-requests:
			return gate
		case <-ctx.Done():
			t.Fatal("Codex did not reach the local provider")
			return nil
		}
	}
	threadID := lifecycleCall(t, a, app, "thread/start", map[string]any{"cwd": cwd, "sandbox": "read-only", "approvalPolicy": "never"})["thread"].(map[string]any)["id"].(string)
	lifecycleCall(t, a, app, "turn/start", queueInput(threadID, "first"))
	first := nextRequest()
	if _, err := a.queueRPC(ctx, app, "owner", "codexrm/queue/add", queueInput(threadID, "queued")); err != nil {
		t.Fatal(err)
	}
	assertWriterHeld := func() {
		t.Helper()
		_, err := desktop.Call(ctx, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true})
		if err == nil || !strings.Contains(err.Error(), "active writer") {
			t.Fatalf("running Agent did not retain its writer: %v", err)
		}
	}
	assertWriterHeld()
	close(first)
	second := nextRequest()
	assertWriterHeld() // The queued turn starts without relinquishing ownership.
	close(second)
	waitQueue(t, a, threadID, func(q *messageQueue) bool { return q.Detached && len(q.Entries) == 0 })
	if len(queueCall(t, app, "thread/loaded/list", map[string]any{})["data"].([]any)) != 0 {
		t.Fatal("completed Agent retained a loaded thread")
	}
	queueCall(t, desktop, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true})
	queueCall(t, desktop, "turn/start", queueInput(threadID, "desktop continuation"))
	close(nextRequest())
	queueEvent(t, desktopEvents, "turn/completed")
	// A different writer must remain protected even when the Agent auto-resumes.
	if _, err := a.callThread(ctx, app, "owner", "turn/start", queueInput(threadID, "must not start")); err == nil || !strings.Contains(err.Error(), "active writer") {
		t.Fatalf("Agent did not respect desktop ownership: %v", err)
	}
	queueCall(t, desktop, "thread/unsubscribe", map[string]any{"threadId": threadID})
	queueEvent(t, desktopEvents, "thread/closed")
	lifecycleCall(t, a, app, "turn/start", queueInput(threadID, "remote continuation"))
	close(nextRequest())
	waitQueue(t, a, threadID, func(q *messageQueue) bool { return q.Detached })
	page := queueCall(t, app, "thread/turns/list", map[string]any{"threadId": threadID, "limit": 10})
	if len(page["data"].([]any)) != 4 {
		t.Fatalf("handoff lost or replayed history: %v", page)
	}
	metadata := queueCall(t, app, "thread/turns/list", map[string]any{"threadId": threadID, "limit": 1, "itemsView": "notLoaded"})
	metadataTurns, _ := metadata["data"].([]any)
	if len(metadataTurns) != 1 || metadata["nextCursor"] == nil {
		t.Fatalf("metadata-only page cannot advance: %v", metadata)
	}
	items, _ := metadataTurns[0].(map[string]any)["items"].([]any)
	if len(items) != 0 {
		t.Fatal("metadata-only page contains full items")
	}
	reopened := lifecycleCall(t, a, app, "thread/resume", map[string]any{"threadId": threadID})
	thread := reopened["thread"].(map[string]any)
	if thread["historyMode"] != "paginated" && len(thread["turns"].([]any)) != 4 {
		t.Fatalf("resume lost history from a legacy thread: %v", thread["turns"])
	}
	rawSnapshot, err := a.threadSnapshot(ctx, app, map[string]any{"threadId": threadID})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Thread struct{ Turns []json.RawMessage }
	}
	if err := json.Unmarshal(rawSnapshot, &snapshot); err != nil || len(snapshot.Thread.Turns) != 4 {
		t.Fatalf("snapshot lost history: turns=%d, error=%v", len(snapshot.Thread.Turns), err)
	}
}
