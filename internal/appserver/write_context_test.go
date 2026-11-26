package appserver

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCanceledActiveWriteClosesClient(t *testing.T) {
	c := newClient()
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c.write = func([]byte) error { close(entered); <-release; close(exited); return nil }
	c.shutdown = func() error { close(release); return nil }
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := c.Call(ctx, "ping", nil); result <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}
	// A canceled call waiting behind the writer must leave the active call alone.
	waiting, stop := context.WithCancel(context.Background())
	stop()
	var unknown *OutcomeUnknown
	if _, err := c.Call(waiting, "ping", nil); !errors.Is(err, context.Canceled) || errors.As(err, &unknown) {
		t.Fatalf("queued write should be a definite failure: %v", err)
	}
	select {
	case <-c.Done():
		t.Fatal("waiting call closed the connection")
	default:
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || !errors.As(err, &unknown) {
			t.Fatalf("active write should have an unknown outcome: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked write ignored call cancellation")
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("writer remained blocked")
	}
}
