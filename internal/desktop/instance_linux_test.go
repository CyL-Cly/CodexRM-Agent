//go:build linux

package desktop

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLinuxSingleInstanceOpenAndStop(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir, false)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if first == nil {
		t.Fatal("first acquire returned nil instance")
	}
	defer first.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opens := make(chan struct{}, 4)
	stops := make(chan struct{}, 4)
	go first.Watch(ctx, func() { opens <- struct{}{} }, func() { stops <- struct{}{} })

	// A second process with the same state directory only forwards "open".
	second, err := Acquire(dir, false)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if second != nil {
		t.Fatal("second acquire must not create a second instance")
	}
	select {
	case <-opens:
	case <-time.After(3 * time.Second):
		t.Fatal("open event was not delivered to the primary")
	}

	// A different state directory is independent.
	other, err := Acquire(filepath.Join(dir, "other"), false)
	if err != nil {
		t.Fatalf("other acquire: %v", err)
	}
	if other == nil {
		t.Fatal("independent state directory must get its own instance")
	}
	other.Close()

	if err := Stop(dir); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-stops:
	case <-time.After(3 * time.Second):
		t.Fatal("stop event was not delivered to the primary")
	}
}

func TestLinuxRestartTakesOverAfterOwnerCloses(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir, false)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go first.Watch(ctx, func() {}, func() { close(stopped); cancel() })

	acquired := make(chan *Instance, 1)
	go func() {
		instance, err := Acquire(dir, true)
		if err != nil {
			acquired <- nil
			return
		}
		acquired <- instance
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("restart did not signal the old owner")
	}
	first.Close()

	select {
	case instance := <-acquired:
		if instance == nil {
			t.Fatal("restart acquire failed")
		}
		instance.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("restart did not acquire the released lock")
	}
}

func TestLinuxStaleSocketIsReplaced(t *testing.T) {
	dir := t.TempDir()
	path, err := instanceSocket(dir)
	if err != nil {
		t.Fatalf("socket path: %v", err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0600); err != nil {
		t.Fatalf("create stale socket: %v", err)
	}
	instance, err := Acquire(dir, false)
	if err != nil {
		t.Fatalf("acquire with stale socket: %v", err)
	}
	if instance == nil {
		t.Fatal("nil instance")
	}
	defer instance.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket was not recreated: %v", err)
	}
}

func TestLinuxStopWithoutOwnerIsNoop(t *testing.T) {
	if err := Stop(t.TempDir()); err != nil {
		t.Fatalf("stop on unused directory: %v", err)
	}
}
