package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/desktop"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/google/uuid"
)

func TestCancelPhoneTransferStopsExistingReads(t *testing.T) {
	a := &Agent{cfg: Config{DataDir: t.TempDir()}}
	path := filepath.Join(t.TempDir(), "send.txt")
	data := make([]byte, attachmentChunkSize+1)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	first, err := a.QueuePhoneFile("alice", path, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	other, err := a.QueuePhoneFile("bob", path, "Bob")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session := &directSession{client: "alice", ctx: ctx}
	defer a.closePhoneReader(session)
	open := func() (any, error) {
		return a.phoneTransferRPC(ctx, session.client, protocol.PhoneTransferPrefix+"open", map[string]any{"id": first.ID}, session)
	}
	header, err := open()
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"id": header.(map[string]any)["id"], "offset": float64(0)}
	read := func() (any, error) {
		return a.phoneTransferRPC(ctx, session.client, protocol.PhoneTransferPrefix+"read", params, session)
	}
	if _, err := read(); err != nil {
		t.Fatal(err)
	}
	params["offset"] = float64(attachmentChunkSize)
	if err := a.CancelPhoneTransfer(phoneOwnerBucket("bob"), first.ID); !errors.Is(err, ErrPhoneTransferNotPending) {
		t.Fatalf("cancel across outbox buckets: %v", err)
	}
	if err := a.CancelPhoneTransfer(phoneOwnerBucket("alice"), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := read(); err == nil {
		t.Fatal("the phone kept reading a canceled transfer")
	}
	if _, err := open(); err == nil {
		t.Fatal("the phone reopened a canceled transfer")
	}
	if err := a.CancelPhoneTransfer(phoneOwnerBucket("alice"), first.ID); !errors.Is(err, ErrPhoneTransferNotPending) {
		t.Fatalf("second cancellation: %v", err)
	}
	list, err := a.PendingPhoneTransfers()
	if err != nil || len(list) != 1 || list[0].PhoneTransfer != other || list[0].Bucket != phoneOwnerBucket("bob") {
		t.Fatalf("pending after cancel: %#v, %v", list, err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != int64(len(data)) {
		t.Fatalf("the original file was affected: %v", err)
	}
}

func TestReceivedPhoneHistoryUsesSavedFileAfterRestart(t *testing.T) {
	if !desktop.ReceivesFiles {
		t.Skip("desktop file receiving not supported")
	}
	downloads, state := t.TempDir(), t.TempDir()
	batch := receiveBatchKey{"sender-id", uuid.NewString()}
	file := attachment{ID: uuid.NewString(), Name: "同名文件.txt", Size: 3}
	occupied := filepath.Join(downloads, file.Name)
	if err := os.WriteFile(occupied, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	file.Path = filepath.Join(downloads, ".codexrm-incoming", file.ID, "file-"+file.Name)
	if err := os.MkdirAll(filepath.Dir(file.Path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file.Path+".part", []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: Config{DataDir: state, AllowFileManagement: true}, uploads: map[string]*attachmentUpload{
		file.ID: {File: file, Owner: batch.owner, Offset: file.Size, Downloads: downloads, Sender: "测试手机", DirectID: "live", BatchID: batch.id, Updated: time.Now()},
	}}
	t.Cleanup(func() { a.receiveBatches.flush(batch, func(string, []desktop.ReceivedFile) {}) })
	direct := &directSession{id: "live", client: batch.owner, ctx: context.Background()}
	value, err := a.attachmentRPC(batch.owner, "codexrm/attachment/finish", map[string]any{"id": file.ID}, direct)
	if err != nil {
		t.Fatal(err)
	}
	const finalName = "同名文件 (1).txt"
	if got := value.(attachment); got.Path != "" || got.Name != finalName {
		t.Fatalf("wrong finish response: %+v", got)
	}
	restarted := &Agent{cfg: Config{DataDir: state}}
	items, err := restarted.ReceivedPhoneFiles()
	if err != nil || len(items) != 1 || items[0].Name != finalName || items[0].Sender != "测试手机" {
		t.Fatalf("received history: %#v, %v", items, err)
	}
	path, err := restarted.receivedPhonePath(downloads, file.ID)
	if err != nil || path != filepath.Join(downloads, finalName) {
		t.Fatalf("opening the file should resolve the final path: %q, %v", path, err)
	}
	if _, err := restarted.receivedPhonePath(downloads, "../example"); err == nil {
		t.Fatal("accepted a path instead of a record ID")
	}
	if err := os.Rename(path, filepath.Join(downloads, "moved.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.receivedPhonePath(downloads, file.ID); err == nil {
		t.Fatal("moved file should not open")
	}
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.receivedPhonePath(downloads, file.ID); err == nil {
		t.Fatal("replaced file should not open")
	}
}
