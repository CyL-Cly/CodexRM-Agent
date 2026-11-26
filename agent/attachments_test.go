package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

func attachmentCall(t *testing.T, a *Agent, owner, action string, params map[string]any) any {
	t.Helper()
	// Match decoded JSON RPC numbers.
	var decoded map[string]any
	_ = json.Unmarshal(mustJSON(params), &decoded)
	result, err := a.attachmentRPC(owner, "codexrm/attachment/"+action, decoded, nil)
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	return result
}

func uploadTestAttachment(t *testing.T, a *Agent, name string, data []byte) attachment {
	t.Helper()
	id := attachmentCall(t, a, "owner", "begin", map[string]any{"name": name, "size": len(data)}).(map[string]any)["id"].(string)
	for offset := 0; offset < len(data); {
		end := min(offset+attachmentChunkSize, len(data))
		result := attachmentCall(t, a, "owner", "chunk", map[string]any{"id": id, "offset": offset, "data": base64.StdEncoding.EncodeToString(data[offset:end])}).(map[string]any)
		offset = int(result["offset"].(int64))
	}
	return attachmentCall(t, a, "owner", "finish", map[string]any{"id": id}).(attachment)
}

func TestAttachmentChunkRoundTripAndRestart(t *testing.T) {
	a := &Agent{cfg: Config{DataDir: t.TempDir()}}
	data := bytes.Repeat([]byte("文档\x00binary\xff"), 55000)
	file := uploadTestAttachment(t, a, `..\..\CON?.txt`, data)
	if filepath.Dir(filepath.Dir(file.Path)) != filepath.Join(a.cfg.DataDir, "attachments") || filepath.Base(file.Path) != "file-CON_.txt" {
		t.Fatalf("unsafe upload path: %q", file.Path)
	}
	if got, err := os.ReadFile(file.Path); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("written bytes differ: %v", err)
	}
	// Finalized files are readable after Agent restart and by another paired browser.
	a = &Agent{cfg: a.cfg}
	var got []byte
	for offset := int64(0); offset < file.Size; {
		result := attachmentCall(t, a, "other-browser", "read", map[string]any{"id": file.ID, "offset": offset}).(map[string]any)
		chunk, err := base64.StdEncoding.DecodeString(result["data"].(string))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, chunk...)
		offset = result["offset"].(int64)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("download differs from uploaded bytes")
	}
}

func TestAttachmentValidationOwnershipAndAbort(t *testing.T) {
	a := &Agent{cfg: Config{DataDir: t.TempDir()}}
	for _, size := range []any{-1, 1.5, attachmentMaxSize + 1, "20"} {
		var params map[string]any
		_ = json.Unmarshal(mustJSON(map[string]any{"size": size}), &params)
		if _, err := a.attachmentRPC("owner", "codexrm/attachment/begin", params, nil); err == nil {
			t.Fatalf("accepted invalid size %v", size)
		}
	}
	id := attachmentCall(t, a, "owner", "begin", map[string]any{"name": "file.txt", "size": 3}).(map[string]any)["id"].(string)
	for _, test := range []struct {
		owner, method string
		params        map[string]any
	}{
		{"other", "chunk", map[string]any{"id": id, "offset": float64(0), "data": "YWJj"}},
		{"other", "abort", map[string]any{"id": id}},
		{"owner", "chunk", map[string]any{"id": id, "offset": float64(1), "data": "YWJj"}},
		{"owner", "chunk", map[string]any{"id": id, "offset": float64(0), "data": "not base64!"}},
		{"owner", "chunk", map[string]any{"id": id, "offset": float64(0), "data": "YWJjZA=="}},
		{"owner", "chunk", map[string]any{"id": id, "offset": float64(0), "data": base64.StdEncoding.EncodeToString(make([]byte, attachmentChunkSize+1))}},
		{"owner", "finish", map[string]any{"id": id}},
		{"owner", "read", map[string]any{"id": id, "offset": float64(0)}},
		{"owner", "read", map[string]any{"id": "../agent.json", "offset": float64(0)}},
	} {
		if _, err := a.attachmentRPC(test.owner, "codexrm/attachment/"+test.method, test.params, nil); err == nil {
			t.Fatalf("accepted invalid %s from %s", test.method, test.owner)
		}
	}
	path := a.uploads[id].File.Path + ".part"
	attachmentCall(t, a, "owner", "abort", map[string]any{"id": id})
	if _, err := os.Stat(path); !os.IsNotExist(err) || len(a.uploads) != 0 {
		t.Fatal("abort did not remove the partial upload")
	}
}

func TestRelayChatImagesRejectNonImagesAndDirectUploads(t *testing.T) {
	a := &Agent{cfg: Config{DataDir: t.TempDir()}}
	call := func(owner, action string, params map[string]any) (any, error) {
		return a.attachmentRPC(owner, protocol.ChatImagePrefix+action, params, nil)
	}
	for _, params := range []map[string]any{
		{"size": float64(3), "destination": protocol.DesktopShareDestination},
		{"size": float64(3), "uploadStream": true},
		{"size": float64(0)},
		{"size": float64(attachmentMaxSize + 1)},
	} {
		if _, err := call("owner", "begin", params); err == nil {
			t.Fatalf("accepted invalid chat image upload: %v", params)
		}
	}
	begun, err := call("owner", "begin", map[string]any{"size": float64(3), "name": "fake.png", "mimeType": "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	id := begun.(map[string]any)["id"].(string)
	path := a.uploads[id].File.Path
	chunk := map[string]any{"id": id, "offset": float64(0), "data": "YWJj"}
	if _, err := call("other", "chunk", chunk); err == nil {
		t.Fatal("accepted a chunk from a different owner")
	}
	if _, err := call("owner", "chunk", chunk); err != nil {
		t.Fatal(err)
	}
	if _, err := a.attachmentRPC("owner", "codexrm/attachment/finish", map[string]any{"id": id}, nil); err == nil {
		t.Fatal("accepted finalizing an image-only upload through the ordinary attachment RPC")
	}
	if _, err := call("owner", "finish", map[string]any{"id": id}); err == nil {
		t.Fatal("accepted a non-image with a .png suffix and a claimed image MIME type")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) || a.uploads[id] != nil {
		t.Fatalf("rejected upload was not removed: %v", err)
	}
	text := uploadTestAttachment(t, a, "notes.txt", []byte("plain text"))
	if _, err := call("owner", "read", map[string]any{"id": text.ID, "offset": float64(0)}); err == nil {
		t.Fatal("read an ordinary attachment through the chat image RPC")
	}
}

func TestAttachmentDetectsImageAndPreservesEmptyFiles(t *testing.T) {
	a := &Agent{cfg: Config{DataDir: t.TempDir()}}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aA3sAAAAASUVORK5CYII=")
	image := uploadTestAttachment(t, a, "图片.txt", png)
	if image.MimeType != "image/png" {
		t.Fatalf("did not sniff the actual image: %#v", image)
	}
	text := uploadTestAttachment(t, a, "fake.png", []byte("plain text"))
	if text.MimeType != "text/plain" {
		t.Fatal("trusted a fake image filename")
	}
	empty := uploadTestAttachment(t, a, "empty.txt", nil)
	result := attachmentCall(t, a, "owner", "read", map[string]any{"id": empty.ID, "offset": 0}).(map[string]any)
	if result["offset"] != int64(0) || result["data"] != "" {
		t.Fatal("empty file did not round trip")
	}
	if _, err := a.attachmentRPC("owner", "codexrm/attachment/read", map[string]any{"id": text.ID, "offset": float64(text.Size + 1)}, nil); err == nil {
		t.Fatal("accepted offset past the file")
	}
}

func TestAttachmentReclaimsExpiredPartialUploads(t *testing.T) {
	a := &Agent{cfg: Config{DataDir: t.TempDir()}}
	id := attachmentCall(t, a, "owner", "begin", map[string]any{"name": "old.txt", "size": 3}).(map[string]any)["id"].(string)
	path := a.uploads[id].File.Path + ".part"
	a.uploads[id].Updated = time.Now().Add(-time.Hour)
	attachmentCall(t, a, "owner", "begin", map[string]any{"name": "new.txt", "size": 1})
	if _, err := os.Stat(path); !os.IsNotExist(err) || a.uploads[id] != nil {
		t.Fatal("expired partial upload was retained")
	}
}
