package agenthost

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWXTaskCancellationAndFailedFinalWrite(t *testing.T) {
	h := &Host{path: filepath.Join(t.TempDir(), "connection.json")}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	t.Cleanup(h.cancel)
	task, err := h.beginWXTask("maintenance", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	h.startWXTask(task, func(ctx context.Context, _ *wxTask) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("maintenance did not start")
	}
	path := h.wxTaskPath()
	old := path + ".saved"
	if err := os.Rename(path, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	h.cancel()
	h.wxTaskWG.Wait()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(old, path); err != nil {
		t.Fatal(err)
	}
	h.resumeWXTask()
	if result := h.loadWXTask(); result == nil || result.Status != "interrupted" {
		t.Fatalf("polling did not recover the interrupted result: %#v", result)
	}
}
