package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"math/rand"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

func TestRelayChatImagesAndDirectOnlyFiles(t *testing.T) {
	_, h := newServer(t)
	page, err := http.Get(h.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = page.Body.Close()
	if !strings.Contains(page.Header.Get("Content-Security-Policy"), "img-src 'self' data: blob:") {
		t.Fatal("page policy blocks attachment previews")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := agent.New(agent.Config{ServerURL: h.URL, DataDir: t.TempDir(), Cwd: t.TempDir(), Mock: true, EnrollmentKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := a.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Agent did not stop")
		}
	})
	var claimed protocol.PairClaimResponse
	post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.ManualPairingCode}, &claimed)
	c := ws(t, h, "client", claimed.Token)
	status := until(t, c, func(e protocol.Envelope) bool { return e.Status == "connected" })
	if !status.FileTransferDirectOnly {
		t.Fatal("relay did not advertise its file transfer policy")
	}
	id := 0
	call := func(method string, params any) map[string]any {
		t.Helper()
		id++
		rpc(t, c, id, method, params)
		return response(t, c, id)
	}
	info := call("codexrm/info", nil)
	attachments := info["attachments"].(map[string]any)
	if attachments["maxSize"] != float64(20<<20) || attachments["relayImages"] != true {
		t.Fatal("Agent did not advertise attachments")
	}
	for _, test := range []struct {
		method string
		params map[string]any
	}{
		{"codexrm/attachment/begin", map[string]any{"name": "share.bin", "size": 3, "destination": "downloads"}},
		{"codexrm/attachment/begin", map[string]any{"name": "chat.bin", "size": 3}},
		{"codexrm/attachment/chunk", map[string]any{"id": "upload", "offset": 0, "data": "YWJj"}},
		{"codexrm/attachment/finish", map[string]any{"id": "upload"}},
		{"codexrm/attachment/abort", map[string]any{"id": "upload"}},
		{"codexrm/attachment/read", map[string]any{"id": "upload", "offset": 0}},
		{"codexrm/image/read", map[string]any{"path": "example.png"}},
		{"turn/start", map[string]any{"input": []any{map[string]any{"type": "image", "url": "data:image/png;base64,YWJj"}}}},
		{protocol.DirectFilePrefix + "read", map[string]any{"id": "download", "offset": 0}},
	} {
		assertRelayFileDenied(t, c, test.method, test.params)
	}
	// Send a real, multi-chunk picture without opening a P2P session.
	picture := image.NewNRGBA(image.Rect(0, 0, 400, 400))
	_, _ = rand.New(rand.NewSource(1)).Read(picture.Pix)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	prefix := protocol.ChatImagePrefix
	begun := call(prefix+"begin", map[string]any{"name": "photo.png", "size": len(data)})
	uploadID := begun["id"].(string)
	chunkSize := int(begun["chunkSize"].(float64))
	if chunkSize != 256<<10 || len(data) <= chunkSize {
		t.Fatalf("expected multiple bounded chunks, size=%d chunk=%d", len(data), chunkSize)
	}
	for offset := 0; offset < len(data); offset += chunkSize {
		end := min(offset+chunkSize, len(data))
		result := call(prefix+"chunk", map[string]any{"id": uploadID, "offset": offset, "data": base64.StdEncoding.EncodeToString(data[offset:end])})
		if result["offset"] != float64(end) {
			t.Fatalf("unexpected upload offset: %v", result)
		}
	}
	file := call(prefix+"finish", map[string]any{"id": uploadID})
	if file["mimeType"] != "image/png" || file["id"] != uploadID || file["path"] == "" {
		t.Fatalf("unexpected attachment metadata: %v", file)
	}
	thread := call("thread/start", map[string]any{})["thread"].(map[string]any)
	input := []any{
		map[string]any{"type": "text", "text": "请读取附件\n[CodexRM attachment] " + string(raw(file)), "text_elements": []any{}},
		map[string]any{"type": "localImage", "path": file["path"]},
	}
	turn := call("turn/start", map[string]any{"threadId": thread["id"], "input": input})["turn"].(map[string]any)
	content := turn["items"].([]any)[0].(map[string]any)["content"]
	if !bytes.Equal(raw(content), raw(input)) {
		t.Fatalf("turn lost attachment content: %s", raw(content))
	}
	readThread := call("thread/read", map[string]any{"threadId": thread["id"], "includeTurns": true})["thread"].(map[string]any)
	saved := readThread["turns"].([]any)[0].(map[string]any)["items"].([]any)[0].(map[string]any)["content"]
	if !bytes.Equal(raw(saved), raw(input)) {
		t.Fatal("reopened history lost attachment content")
	}
	var downloaded []byte
	for offset := 0; offset < len(data); offset += chunkSize {
		result := call(prefix+"read", map[string]any{"id": uploadID, "offset": offset})
		chunk, err := base64.StdEncoding.DecodeString(result["data"].(string))
		if err != nil || result["offset"] != float64(offset+len(chunk)) {
			t.Fatalf("invalid download chunk: %v", err)
		}
		downloaded = append(downloaded, chunk...)
	}
	if !bytes.Equal(data, downloaded) {
		t.Fatal("read does not match the uploaded picture")
	}
}
