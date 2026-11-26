package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
)

func lifecycleCall(t *testing.T, a *Agent, app *appserver.Client, method string, params map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := a.callThread(ctx, app, "owner", method, params)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestIdleThreadReleasesAndNextMessageResumes(t *testing.T) {
	a, app, events := queueTestAgent(t)
	a.cfg.Mock = true
	threadID := lifecycleCall(t, a, app, "thread/start", map[string]any{
		"sandbox": "read-only", "approvalPolicy": "never", "config": map[string]any{"testSetting": true},
	})["thread"].(map[string]any)["id"].(string)
	otherID := lifecycleCall(t, a, app, "thread/start", map[string]any{})["thread"].(map[string]any)["id"].(string)
	for i := 0; i < 2; i++ {
		turnID := lifecycleCall(t, a, app, "turn/start", queueInput(threadID, "approval wait"))["turn"].(map[string]any)["id"].(string)
		queueEvent(t, events, "item/commandExecution/requestApproval")
		a.mu.Lock()
		owner := a.owners[threadID]
		a.mu.Unlock()
		if owner != "owner" {
			t.Fatal("resumed turn lost approval ownership")
		}
		loaded := queueCall(t, app, "thread/loaded/list", nil)["data"].([]any)
		if len(loaded) != 2 {
			t.Fatalf("active thread was released: %v", loaded)
		}
		lifecycleCall(t, a, app, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
		waitQueue(t, a, threadID, func(q *messageQueue) bool { return q.Detached && !q.Loaded })
		loaded = queueCall(t, app, "thread/loaded/list", nil)["data"].([]any)
		if len(loaded) != 1 || loaded[0] != otherID {
			t.Fatalf("idle release affected another thread: %v", loaded)
		}
	}
	a.queueMu.Lock()
	settings := a.queues[threadID].ResumeParams
	a.queueMu.Unlock()
	if settings["sandbox"] != "read-only" || settings["approvalPolicy"] != "never" || settings["config"].(map[string]any)["testSetting"] != true {
		t.Fatalf("runtime settings were lost: %v", settings)
	}
}

func TestPausedQueueReleasesThenResumesWithoutLosingMessages(t *testing.T) {
	a, app, events := queueTestAgent(t)
	a.cfg.Mock = true
	ctx := a.queueCtx
	threadID := lifecycleCall(t, a, app, "thread/start", nil)["thread"].(map[string]any)["id"].(string)
	turnID := lifecycleCall(t, a, app, "turn/start", queueInput(threadID, "approval first"))["turn"].(map[string]any)["id"].(string)
	queueEvent(t, events, "item/commandExecution/requestApproval")
	if _, err := a.queueRPC(ctx, app, "owner", "codexrm/queue/add", queueInput(threadID, "approval queued")); err != nil {
		t.Fatal(err)
	}
	lifecycleCall(t, a, app, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
	waitQueue(t, a, threadID, func(q *messageQueue) bool { return q.Detached && q.Paused && len(q.Entries) == 1 })
	if _, err := a.queueRPC(ctx, app, "owner", "codexrm/queue/resume", map[string]any{"threadId": threadID}); err != nil {
		t.Fatal(err)
	}
	queueEvent(t, events, "item/commandExecution/requestApproval")
	waitQueue(t, a, threadID, func(q *messageQueue) bool {
		return !q.Detached && q.ActiveTurn != "" && len(q.Entries) == 0 && !q.Starting
	})
	thread := queueCall(t, app, "thread/read", map[string]any{"threadId": threadID})["thread"].(map[string]any)
	if len(thread["turns"].([]any)) != 2 {
		t.Fatal("queued message was lost or replayed after resume")
	}
}

func TestDetachedQueuePollsDesktopCompletionAndResumes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	mock, err := appserver.StartMock("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	app, err := appserver.Dial(ctx, "ws://"+mock.Addr)
	if err != nil {
		cancel()
		_ = mock.Close()
		t.Fatal(err)
	}
	desktop, err := appserver.Dial(ctx, "ws://"+mock.Addr)
	if err != nil {
		cancel()
		_ = app.Close()
		_ = mock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = desktop.Close(); _ = app.Close(); _ = mock.Close() })
	a := &Agent{cfg: Config{Mock: true}, owners: map[string]string{}, approvals: map[string]approval{}, queueCtx: ctx}
	app.OnNotification(func(raw json.RawMessage) { a.queueNotification(ctx, app, raw) })
	desktopEvents := make(chan json.RawMessage, 32)
	desktop.OnNotification(func(raw json.RawMessage) { desktopEvents <- raw })

	threadID := lifecycleCall(t, a, app, "thread/start", nil)["thread"].(map[string]any)["id"].(string)
	lifecycleCall(t, a, app, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true})
	waitQueue(t, a, threadID, func(q *messageQueue) bool { return q.Detached })
	turn := queueCall(t, desktop, "thread/resume", map[string]any{"threadId": threadID})
	if turn["thread"] == nil {
		t.Fatal("desktop could not acquire released thread")
	}
	queueCall(t, desktop, "turn/start", queueInput(threadID, "approval desktop"))
	rawSnapshot, err := a.threadSnapshot(ctx, app, map[string]any{"threadId": threadID})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct{ Active bool }
	if err = json.Unmarshal(rawSnapshot, &snapshot); err != nil || !snapshot.Active {
		t.Fatalf("detached snapshot did not expose desktop activity: %s, %v", rawSnapshot, err)
	}
	if _, err = a.queueRPC(ctx, app, "owner", "codexrm/queue/add", queueInput(threadID, "queued after desktop")); err != nil {
		t.Fatal(err)
	}

	var approvalID any
	deadline := time.After(3 * time.Second)
	for approvalID == nil {
		select {
		case raw := <-desktopEvents:
			var event map[string]any
			_ = json.Unmarshal(raw, &event)
			if event["method"] == "item/commandExecution/requestApproval" {
				approvalID = event["id"]
			}
		case <-deadline:
			t.Fatal("desktop approval was not emitted")
		}
	}
	if err = desktop.WriteRaw(mustJSON(map[string]any{"id": approvalID, "result": map[string]any{"decision": "accept"}})); err != nil {
		t.Fatal(err)
	}
	// The mock emits desktop completion only on the desktop connection. The Agent
	// must discover the idle state through its detached poll rather than an event.
	waitQueue(t, a, threadID, func(q *messageQueue) bool {
		return !q.Detached && !q.Starting && len(q.Entries) == 0 && q.ActiveTurn != ""
	})
	thread := queueCall(t, app, "thread/read", map[string]any{"threadId": threadID})["thread"].(map[string]any)
	if len(thread["turns"].([]any)) != 2 {
		t.Fatal("detached queue did not dispatch exactly one message after desktop completion")
	}
}

func TestIdleResumeReleasesButActiveResumeStaysLoaded(t *testing.T) {
	a, app, events := queueTestAgent(t)
	a.cfg.Mock = true
	threadID := lifecycleCall(t, a, app, "thread/start", nil)["thread"].(map[string]any)["id"].(string)
	turnID := lifecycleCall(t, a, app, "turn/start", queueInput(threadID, "approval wait"))["turn"].(map[string]any)["id"].(string)
	queueEvent(t, events, "item/commandExecution/requestApproval")
	// Emulate a metadata-only resume which did not receive turn/started.
	a.queueMu.Lock()
	a.queues[threadID].ActiveTurn = ""
	a.queueMu.Unlock()
	a.releaseIdleThread(a.queueCtx, app, threadID)
	if len(queueCall(t, app, "thread/loaded/list", nil)["data"].([]any)) != 1 {
		t.Fatal("metadata-only resume released an active turn")
	}
	lifecycleCall(t, a, app, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
	waitQueue(t, a, threadID, func(q *messageQueue) bool { return q.Detached })
	lifecycleCall(t, a, app, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true})
	waitQueue(t, a, threadID, func(q *messageQueue) bool { return q.Detached })
	if len(queueCall(t, app, "thread/loaded/list", nil)["data"].([]any)) != 0 {
		t.Fatal("viewing an idle thread retained the writer")
	}
}
