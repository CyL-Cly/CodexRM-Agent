package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
)

func queueTestAgent(t *testing.T) (*Agent, *appserver.Client, <-chan json.RawMessage) {
	t.Helper()
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
	t.Cleanup(func() { cancel(); _ = app.Close(); _ = mock.Close() })
	a := &Agent{owners: map[string]string{}, approvals: map[string]approval{}, queueCtx: ctx}
	events := make(chan json.RawMessage, 256)
	app.OnNotification(func(raw json.RawMessage) {
		a.queueNotification(ctx, app, raw)
		events <- raw
	})
	return a, app, events
}

func queueCall(t *testing.T, app *appserver.Client, method string, params map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := app.Call(ctx, method, params)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func queueInput(threadID, text string) map[string]any {
	return map[string]any{"threadId": threadID, "model": "mock-codex", "effort": "medium", "input": []any{map[string]any{"type": "text", "text": text, "text_elements": []any{}}}}
}

func queueEvent(t *testing.T, events <-chan json.RawMessage, method string) map[string]any {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case raw := <-events:
			var event map[string]any
			_ = json.Unmarshal(raw, &event)
			if event["method"] == method {
				return event
			}
		case <-deadline:
			t.Fatalf("waiting for %s", method)
			return nil
		}
	}
}

func waitQueue(t *testing.T, a *Agent, threadID string, want func(*messageQueue) bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		a.queueMu.Lock()
		q := a.queueLocked(threadID)
		ok := want(q)
		a.queueMu.Unlock()
		if ok {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("queue did not reach expected state")
		}
	}
}

func TestQueueFIFOAndSteerWithoutBrowserConnection(t *testing.T) {
	a, app, events := queueTestAgent(t)
	threadID := queueCall(t, app, "thread/start", map[string]any{})["thread"].(map[string]any)["id"].(string)
	turnID := queueCall(t, app, "turn/start", queueInput(threadID, "approval first"))["turn"].(map[string]any)["id"].(string)
	approval := queueEvent(t, events, "item/commandExecution/requestApproval")
	for _, text := range []string{"approval second", "approval third"} {
		if _, err := a.queueRPC(context.Background(), app, "owner", "codexrm/queue/add", queueInput(threadID, text)); err != nil {
			t.Fatal(err)
		}
	}
	queueCall(t, app, "turn/steer", map[string]any{"threadId": threadID, "expectedTurnId": turnID, "input": queueInput(threadID, "supplement")["input"]})
	thread := queueCall(t, app, "thread/read", map[string]any{"threadId": threadID})["thread"].(map[string]any)
	turns := thread["turns"].([]any)
	if len(turns) != 1 || len(turns[0].(map[string]any)["items"].([]any)) != 2 {
		t.Fatalf("steering did not stay in the active turn: %v", turns)
	}
	waitQueue(t, a, threadID, func(q *messageQueue) bool { return q.ActiveTurn == turnID && len(q.Entries) == 2 && !q.Starting })
	for _, want := range []string{"approval second", "approval third"} {
		if err := app.WriteRaw(mustJSON(map[string]any{"id": approval["id"], "result": map[string]any{"decision": "accept"}})); err != nil {
			t.Fatal(err)
		}
		// The old turn must finish before the next queued turn starts.
		queueEvent(t, events, "turn/completed")
		started := queueEvent(t, events, "turn/started")["params"].(map[string]any)["turn"].(map[string]any)
		message := started["items"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
		if message != want {
			t.Fatalf("FIFO: got %v, want %s", message, want)
		}
		approval = queueEvent(t, events, "item/commandExecution/requestApproval")
	}
	waitQueue(t, a, threadID, func(q *messageQueue) bool { return len(q.Entries) == 0 && !q.Starting })
	a.mu.Lock()
	owner := a.owners[threadID]
	a.mu.Unlock()
	if owner != "owner" {
		t.Fatal("queued approvals lost their sending browser owner")
	}
}

func TestQueuePauseRemoveResumeAndThreadIsolation(t *testing.T) {
	a, app, events := queueTestAgent(t)
	threadID := queueCall(t, app, "thread/start", map[string]any{})["thread"].(map[string]any)["id"].(string)
	turnID := queueCall(t, app, "turn/start", queueInput(threadID, "approval first"))["turn"].(map[string]any)["id"].(string)
	queueEvent(t, events, "item/commandExecution/requestApproval")
	var removeID string
	for _, text := range []string{"cancel me", "approval remaining"} {
		view, err := a.queueRPC(context.Background(), app, "owner", "codexrm/queue/add", queueInput(threadID, text))
		if err != nil {
			t.Fatal(err)
		}
		if removeID == "" {
			removeID = view.(map[string]any)["entries"].([]any)[0].(map[string]any)["id"].(string)
		}
	}
	queueCall(t, app, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
	queueEvent(t, events, "turn/completed")
	waitQueue(t, a, threadID, func(q *messageQueue) bool { return q.Paused && q.ActiveTurn == "" && len(q.Entries) == 2 })
	if _, err := a.queueRPC(context.Background(), app, "owner", "codexrm/queue/remove", map[string]any{"threadId": threadID, "id": removeID}); err != nil {
		t.Fatal(err)
	}
	otherID := queueCall(t, app, "thread/start", map[string]any{})["thread"].(map[string]any)["id"].(string)
	if _, err := a.queueRPC(context.Background(), app, "other-owner", "codexrm/queue/add", queueInput(otherID, "approval other")); err != nil {
		t.Fatal(err)
	}
	started := queueEvent(t, events, "turn/started")["params"].(map[string]any)
	if started["threadId"] != otherID {
		t.Fatal("a paused queue started when another thread changed")
	}
	queueEvent(t, events, "item/commandExecution/requestApproval")
	if _, err := a.queueRPC(context.Background(), app, "owner", "codexrm/queue/resume", map[string]any{"threadId": threadID}); err != nil {
		t.Fatal(err)
	}
	started = queueEvent(t, events, "turn/started")["params"].(map[string]any)
	if started["threadId"] != threadID {
		t.Fatal("resuming dispatched to the wrong thread")
	}
	waitQueue(t, a, threadID, func(q *messageQueue) bool {
		return !q.Paused && !q.Starting && len(q.Entries) == 0 && q.ActiveTurn != ""
	})
}

func TestQueueStartFailureRetainsMessageAndDoesNotReplay(t *testing.T) {
	a, app, _ := queueTestAgent(t)
	a.queueMu.Lock()
	a.queueLocked("missing").Known = true
	a.queueMu.Unlock()
	if _, err := a.queueRPC(context.Background(), app, "owner", "codexrm/queue/add", queueInput("missing", "keep this")); err != nil {
		t.Fatal(err)
	}
	waitQueue(t, a, "missing", func(q *messageQueue) bool { return q.Paused && !q.Starting && len(q.Entries) == 1 && q.Error != "" })
	// Even a later success notification must not replay a failed start.
	a.queueNotification(a.queueCtx, app, mustJSON(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "missing", "turn": map[string]any{"id": "old", "status": "completed"}}}))
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	q := a.queueLocked("missing")
	if !q.Paused || len(q.Entries) != 1 {
		t.Fatal("failure was lost or automatically replayed")
	}
}

func TestQueueDiscoversActiveTurnAfterQueueListWithoutTurnsInSnapshot(t *testing.T) {
	a, app, events := queueTestAgent(t)
	threadID := queueCall(t, app, "thread/start", map[string]any{})["thread"].(map[string]any)["id"].(string)
	turnID := queueCall(t, app, "turn/start", queueInput(threadID, "approval active"))["turn"].(map[string]any)["id"].(string)
	queueEvent(t, events, "item/commandExecution/requestApproval")
	// Simulate loading a thread whose active turn started before this connection.
	a.queueMu.Lock()
	delete(a.queues, threadID)
	a.queueMu.Unlock()
	for _, method := range []string{"codexrm/queue/list", "codexrm/queue/add"} {
		if _, err := a.queueRPC(context.Background(), app, "owner", method, queueInput(threadID, "waiting")); err != nil {
			t.Fatal(err)
		}
	}
	waitQueue(t, a, threadID, func(q *messageQueue) bool {
		return q.ActiveTurn == turnID && !q.Starting && !q.Paused && len(q.Entries) == 1
	})
}
