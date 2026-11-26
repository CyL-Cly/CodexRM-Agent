package agent

import (
	"context"
	"errors"
	"testing"
)

func TestUpdateWaitsForAllConversationsAndQueuedMessages(t *testing.T) {
	a := &Agent{queues: map[string]*messageQueue{
		"first":  {ActiveTurn: "turn-1"},
		"second": {ActiveTurn: "turn-2", Entries: []*queuedMessage{{ID: "queued"}}},
	}}
	started := 0
	start := func() error { started++; return nil }
	assertBlocked := func() {
		t.Helper()
		if ok, err := a.TryStartUpdate(start); ok || err != nil || started != 0 {
			t.Fatalf("busy agent started an update: %v, %v, %d", ok, err, started)
		}
	}
	assertBlocked()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	complete := func(threadID, turnID string) {
		a.queueNotification(ctx, nil, mustJSON(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "status": "interrupted"}}}))
	}
	complete("first", "turn-1")
	assertBlocked()
	complete("second", "turn-2")
	assertBlocked() // Interrupted tasks leave their messages paused but retained.
	a.queueMu.Lock()
	a.queues["second"].Entries = nil
	a.queues["second"].Starting = true
	a.queueMu.Unlock()
	assertBlocked()
	a.queueMu.Lock()
	a.queues["second"].Starting = false
	a.queueMu.Unlock()
	if !a.beginRequest() {
		t.Fatal("idle agent rejected a request")
	}
	assertBlocked()
	a.endRequest()
	if ok, err := a.TryStartUpdate(start); !ok || err != nil || started != 1 {
		t.Fatalf("idle agent did not start its update: %v, %v, %d", ok, err, started)
	}
	if a.beginRequest() {
		t.Fatal("new request slipped in after update admission")
	}
	if ok, _ := a.TryStartUpdate(start); ok || started != 1 {
		t.Fatal("update helper started twice")
	}
}

func TestFailedUpdateReleasesRequestAdmission(t *testing.T) {
	a := &Agent{}
	want := errors.New("helper failed")
	if ok, err := a.TryStartUpdate(func() error { return want }); ok || !errors.Is(err, want) {
		t.Fatalf("failed helper: %v, %v", ok, err)
	}
	if !a.beginRequest() {
		t.Fatal("failed update blocked conversations")
	}
	a.endRequest()
	if ok, err := a.TryStartUpdate(func() error { return nil }); !ok || err != nil {
		t.Fatalf("helper retry failed: %v, %v", ok, err)
	}
}

func TestTurnStartResponseKeepsUpdatePendingUntilCompletion(t *testing.T) {
	a := &Agent{}
	params := map[string]any{"threadId": "thread"}
	raw := mustJSON(map[string]any{"turn": map[string]any{"id": "turn", "status": "inProgress"}})
	a.recordStartedTurn("turn/start", params, raw)
	if ok, _ := a.TryStartUpdate(func() error { t.Fatal("active turn was interrupted"); return nil }); ok {
		t.Fatal("active turn admitted an update")
	}
	a.queueMu.Lock()
	a.queues["thread"].LastCompleted = "turn"
	a.queues["thread"].ActiveTurn = ""
	a.queueMu.Unlock()
	a.recordStartedTurn("turn/start", params, raw)
	if ok, err := a.TryStartUpdate(func() error { return nil }); !ok || err != nil {
		t.Fatalf("late response revived a completed turn: %v, %v", ok, err)
	}
}
