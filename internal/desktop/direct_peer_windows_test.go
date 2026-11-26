package desktop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDirectPeerPresence(t *testing.T) {
	stateDir := t.TempDir()
	const client = "test-phone"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if IsDirectPeerActive(stateDir, client) {
		t.Fatal("untracked peer reported active")
	}
	stop := TrackDirectPeer(ctx, stateDir, client)
	defer stop()
	if !IsDirectPeerActive(stateDir, client) {
		t.Fatal("tracked peer not reported active")
	}
	if IsDirectPeerActive(stateDir, "other-phone") || IsDirectPeerActive(filepath.Join(stateDir, "other-server"), client) {
		t.Fatal("peer presence leaked across phone or Agent identity")
	}
	// A replacement connection can be tracked before the old one returns.
	stopReplacement := TrackDirectPeer(ctx, stateDir, client)
	defer stopReplacement()
	stop()
	if !IsDirectPeerActive(stateDir, client) {
		t.Fatal("replacement peer lost when original session ended")
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for IsDirectPeerActive(stateDir, client) {
		if time.Now().After(deadline) {
			t.Fatal("canceled peer still reported active")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDirectPeerPresenceAcrossProcesses(t *testing.T) {
	const childDir = "CODEXRM_TEST_DIRECT_PEER_CHILD_DIR"
	const client = "test-phone"
	if stateDir := os.Getenv(childDir); stateDir != "" {
		stop := TrackDirectPeer(context.Background(), stateDir, client)
		defer stop()
		time.Sleep(time.Minute) // The parent terminates this process without cleanup.
		return
	}
	stateDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDirectPeerPresenceAcrossProcesses$")
	cmd.Env = append(os.Environ(), childDir+"="+stateDir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	for !IsDirectPeerActive(stateDir, client) {
		if ctx.Err() != nil {
			t.Fatal("the menu process could not see the Agent's live session")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if IsDirectPeerActive(stateDir, client) {
		t.Fatal("the session is still visible after the Agent exits")
	}
}
