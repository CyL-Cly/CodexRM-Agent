package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
)

// Exercises the installed Codex with an isolated home and a local model fixture.
// It does not contact a provider or use the user's model credentials.
func TestAttachmentsWithLocalCodex(t *testing.T) {
	executable := os.Getenv("CODEXRM_TEST_CODEX")
	if executable == "" {
		t.Skip("set CODEXRM_TEST_CODEX to test the local Codex app-server")
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("RUST_LOG", "error")
	requests := make(chan json.RawMessage, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid test request", 400)
			return
		}
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"attachment-test\"}}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"attachment-test\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
	}))
	defer upstream.Close()
	config := fmt.Sprintf("model = 'gpt-5.1-codex'\nmodel_provider = 'attachment_test'\n[model_providers.attachment_test]\nname = 'Attachment test'\nbase_url = %q\nwire_api = 'responses'\nrequires_openai_auth = false\n", upstream.URL)
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: Config{DataDir: t.TempDir()}}
	picture := image.NewRGBA(image.Rect(0, 0, 16, 16))
	picture.Set(0, 0, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil {
		t.Fatal(err)
	}
	photo := uploadTestAttachment(t, a, "测试图片.png", encoded.Bytes())
	document := uploadTestAttachment(t, a, "附件说明.txt", []byte("attachment fixture document"))
	input := []any{
		map[string]any{"type": "text", "text": "Inspect the image and the attached file paths.\n[CodexRM attachment] " + string(mustJSON(photo)) + "\n[CodexRM attachment] " + string(mustJSON(document)), "text_elements": []any{}},
		map[string]any{"type": "localImage", "path": photo.Path},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	app, err := appserver.Launch(ctx, executable, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if _, err := app.Initialize(ctx, "codexrm-attachment-test", "0.0.0"); err != nil {
		t.Fatal(err)
	}
	raw, err := app.Call(ctx, "thread/start", map[string]any{"approvalPolicy": "never", "sandbox": "read-only"})
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		Thread struct{ ID string } `json:"thread"`
	}
	if err := json.Unmarshal(raw, &started); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Call(ctx, "turn/start", map[string]any{"threadId": started.Thread.ID, "input": input}); err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-requests:
		var request struct {
			Input []struct {
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					ImageURL string `json:"image_url"`
				} `json:"content"`
			} `json:"input"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		foundImage, foundFile := false, false
		for _, item := range request.Input {
			for _, content := range item.Content {
				foundImage = foundImage || (content.Type == "input_image" && strings.HasPrefix(content.ImageURL, "data:image/"))
				foundFile = foundFile || strings.Contains(content.Text, `"path":`+string(mustJSON(document.Path)))
			}
		}
		if !foundImage || !foundFile {
			t.Fatalf("model request lost image or file: image=%v file=%v", foundImage, foundFile)
		}
	case <-ctx.Done():
		t.Fatal("local Codex did not send the attachment request")
	}
}
