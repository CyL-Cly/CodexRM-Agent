package desktop

import (
	"context"
	"testing"
	"time"
)

func TestDuplicateOpenAndRestartWaitForOriginalExit(t *testing.T) {
	state := t.TempDir()
	first, err := Acquire(state, false)
	if err != nil || first == nil {
		t.Fatalf("first instance: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() { defer close(done); first.Watch(ctx, func() { opened <- struct{}{} }, cancel); first.Close() }()
	duplicate, err := Acquire(state, false)
	if err != nil || duplicate != nil {
		t.Fatal("duplicate created another instance")
	}
	select {
	case <-opened:
	case <-time.After(time.Second):
		t.Fatal("duplicate did not open existing settings")
	}
	replacement, err := Acquire(state, true)
	if err != nil || replacement == nil {
		t.Fatalf("restart failed: %v", err)
	}
	defer replacement.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("restart did not wait for original shutdown")
	}
}
