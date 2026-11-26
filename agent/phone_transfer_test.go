package agent

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/CyL-Cly/CodexRM-Agent/internal/desktop"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/google/uuid"
)

func TestPhoneTransferSurvivesRestartAndIsScopedToPairedPhone(t *testing.T) {
	const alice, bob = "android-a", "android-b"
	const prefix = protocol.PhoneTransferPrefix
	ctx := context.Background()
	dir := t.TempDir()
	a := &Agent{cfg: Config{DataDir: dir}}
	path := filepath.Join(t.TempDir(), "test file.txt")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	item, err := a.QueuePhoneFile(alice, path, "Alice 的手机")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("modified"), 0600); err != nil {
		t.Fatal(err)
	}
	// A new Agent instance must be able to read the staging directory on disk.
	a = &Agent{cfg: Config{DataDir: dir}}
	items := func(owner string) []PhoneTransfer {
		t.Helper()
		value, err := a.phoneTransferRPC(ctx, owner, prefix+"list", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return value.(map[string]any)["files"].([]PhoneTransfer)
	}
	if got := items(alice); len(got) != 1 || got[0] != item {
		t.Fatalf("missing cached transfer: %#v", got)
	}
	if got := items(bob); len(got) != 0 {
		t.Fatalf("exposed another phone's queue: %#v", got)
	}
	params := map[string]any{"id": item.ID, "path": path, "owner": alice}
	for _, operation := range []string{"open", "read", "close", "ack", "finishBatch"} {
		if !protocol.RequiresDirectFileTransfer(prefix+operation, nil) {
			t.Fatalf("relay allowed %s", operation)
		}
		if _, err := a.phoneTransferRPC(ctx, alice, prefix+operation, params, nil); err == nil {
			t.Fatalf("relay allowed %s", operation)
		}
	}
	if protocol.RequiresDirectFileTransfer(prefix+"list", nil) {
		t.Fatal("relay rejected metadata")
	}
	session := &directSession{client: alice, ctx: ctx}
	other := &directSession{client: bob, ctx: ctx}
	defer session.phoneFiles.close()
	defer other.phoneFiles.close()
	if _, err := a.phoneTransferRPC(ctx, bob, prefix+"open", params, other); err == nil {
		t.Fatal("cross-phone file open succeeded")
	}
	if _, err := a.phoneTransferRPC(ctx, alice, prefix+"ack", params, other); err == nil {
		t.Fatal("unauthenticated ACK succeeded")
	}
	if _, err := a.phoneTransferRPC(ctx, alice, prefix+"open", map[string]any{"id": "../agent.json"}, session); err == nil {
		t.Fatal("path traversal succeeded")
	}
	value, err := a.phoneTransferRPC(ctx, alice, prefix+"open", params, session)
	if err != nil {
		t.Fatal(err)
	}
	header := value.(map[string]any)
	if header["name"] != "test file.txt" || header["size"] != int64(8) {
		t.Fatalf("wrong metadata: %#v", header)
	}
	value, err = a.phoneTransferRPC(ctx, alice, prefix+"read", map[string]any{"id": header["id"], "offset": float64(0)}, session)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(value.(map[string]any)["data"].(string))
	if err != nil || string(raw) != "original" {
		t.Fatalf("wrong snapshot %q: %v", raw, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := a.phoneTransferRPC(ctx, alice, prefix+"ack", params, session); err != nil {
			t.Fatal(err)
		}
	}
	if got := items(alice); len(got) != 0 {
		t.Fatalf("acked file still queued: %#v", got)
	}
}

func TestPhoneUploadRetryUsesDurableReceipt(t *testing.T) {
	if !desktop.ReceivesFiles {
		t.Skip("desktop file receiving not supported")
	}
	a := &Agent{cfg: Config{DataDir: t.TempDir(), AllowFileManagement: true}}
	id := uuid.NewString()
	file := attachment{ID: uuid.NewString(), Name: "collision (1).txt", Size: 7, Path: `not public`}
	if err := a.savePhoneReceipt("mobile", id, file); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"destination": protocol.DesktopShareDestination, "transferId": id}
	if _, err := a.attachmentRPC("mobile", "codexrm/attachment/begin", params, nil); err == nil {
		t.Fatal("relay accessed receipt")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &directSession{id: "retry", client: "mobile", ctx: ctx}
	value, err := a.attachmentRPC("mobile", "codexrm/attachment/begin", params, d)
	if err != nil {
		t.Fatal(err)
	}
	receipt := value.(map[string]any)
	if receipt["alreadySaved"] != true || receipt["name"] != file.Name || receipt["size"] != file.Size || receipt["path"] != nil {
		t.Fatalf("wrong receipt: %#v", receipt)
	}
}
