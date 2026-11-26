package agent

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/desktop"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/google/uuid"
)

func TestReceiveBatchNotifiesOnceForAllSavedFiles(t *testing.T) {
	var q receiveBatchQueue
	type popup struct {
		sender string
		files  []desktop.ReceivedFile
	}
	var popups []popup
	notify := func(sender string, files []desktop.ReceivedFile) { popups = append(popups, popup{sender, files}) }
	a := receiveBatchKey{"paired-a", "same-batch-id"}
	b := receiveBatchKey{"paired-b", "same-batch-id"}
	first := desktop.ReceivedFile{Name: "one.txt", Path: "first", Size: 3}
	second := desktop.ReceivedFile{Name: "two.txt", Path: "second", Size: 5}
	q.record(a, "A", first, time.Hour, notify)
	q.record(b, "B", second, time.Hour, notify)
	q.record(a, "A", second, time.Hour, notify)
	if len(popups) != 0 {
		t.Fatal("individual files showed popups before batch completion")
	}
	q.flush(a, notify)
	q.flush(a, notify) // lost completion response / retry must not create another popup
	if want := []popup{{"A", []desktop.ReceivedFile{first, second}}}; !reflect.DeepEqual(popups, want) {
		t.Fatalf("first phone did not get a single popup for both files: %#v", popups)
	}
	q.flush(b, notify)
	if len(popups) != 2 || !reflect.DeepEqual(popups[1], popup{"B", []desktop.ReceivedFile{second}}) {
		t.Fatalf("batches from different phones mixed: %#v", popups)
	}
}

func TestReceiveBatchShowsSavedFilesIfPhoneLeaves(t *testing.T) {
	var q receiveBatchQueue
	key := receiveBatchKey{"phone", "incomplete"}
	file := desktop.ReceivedFile{Name: "saved.txt"}
	popups := make(chan []desktop.ReceivedFile, 2)
	notify := func(_ string, files []desktop.ReceivedFile) { popups <- files }
	q.record(key, "phone", file, 10*time.Millisecond, notify)
	select {
	case files := <-popups:
		if !reflect.DeepEqual(files, []desktop.ReceivedFile{file}) {
			t.Fatalf("wrong fallback popup: %#v", files)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("saved files were not shown when phone left")
	}
	q.flush(key, notify)
	select {
	case <-popups:
		t.Fatal("batch completion repeated the fallback popup")
	default:
	}
}

func TestFinishPhoneBatchRequiresOriginalPairedPhone(t *testing.T) {
	a := &Agent{}
	key := receiveBatchKey{"alice", uuid.NewString()}
	a.receiveBatches.record(key, "Alice", desktop.ReceivedFile{Name: "test.txt"}, time.Hour, func(string, []desktop.ReceivedFile) {})
	defer a.receiveBatches.flush(key, func(string, []desktop.ReceivedFile) {})
	ctx := context.Background()
	method := protocol.PhoneTransferPrefix + "finishBatch"
	params := map[string]any{"id": key.id}
	for _, session := range []*directSession{nil, {client: "bob", ctx: ctx}} {
		if _, err := a.phoneTransferRPC(ctx, key.owner, method, params, session); err == nil {
			t.Fatal("session not owned by Alice could finish her batch")
		}
	}
	if _, err := a.phoneTransferRPC(ctx, "bob", method, params, &directSession{client: "bob", ctx: ctx}); err != nil {
		t.Fatal(err)
	}
	a.receiveBatches.mu.Lock()
	stillPending := a.receiveBatches.pending[key] != nil
	a.receiveBatches.mu.Unlock()
	if !stillPending {
		t.Fatal("Bob's valid session cleared Alice's batch")
	}
}
