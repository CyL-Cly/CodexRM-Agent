package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

func TestDefaultRemotePermissionsAndMentionedFiles(t *testing.T) {
	workspace, other := t.TempDir(), t.TempDir()
	private := filepath.Join(workspace, "state")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jW0cAAAAASUVORK5CYII=")
	for path, data := range map[string][]byte{
		filepath.Join(workspace, "报告 1.txt"):     []byte("linked file"),
		filepath.Join(workspace, "unlisted.png"): png,
		filepath.Join(workspace, "preview.png"):  png,
		filepath.Join(other, "报告 1.txt"):         []byte("not the linked file"),
		filepath.Join(private, "settings.json"):  []byte("private"),
	} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	mock, err := appserver.StartMock("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mock.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	app, err := appserver.Dial(ctx, "ws://"+mock.Addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	startThread := func(cwd string) string {
		t.Helper()
		raw, err := app.Call(ctx, "thread/start", map[string]any{"cwd": cwd})
		var out struct{ Thread struct{ ID string } }
		if err != nil || json.Unmarshal(raw, &out) != nil || out.Thread.ID == "" {
			t.Fatalf("thread/start: %s, %v", raw, err)
		}
		return out.Thread.ID
	}
	thread, differentThread := startThread(workspace), startThread(other)
	a := &Agent{cfg: Config{Cwd: workspace, DataDir: private}}
	d := &directSession{ctx: ctx}
	t.Cleanup(d.files.close)
	if a.allowsFileManagement() || !a.supportsDirectFiles() {
		t.Fatal("expected default denial but supported mentioned file downloads")
	}
	if _, err := a.workspaceRPC("codexrm/fs/list", nil); !errors.Is(err, errFileManagementDisabled) {
		t.Fatalf("directory picker default=%v", err)
	}
	if _, err := a.attachmentRPC("mobile", "codexrm/attachment/begin", map[string]any{"destination": protocol.DesktopShareDestination}, d); !errors.Is(err, errFileManagementDisabled) {
		t.Fatalf("desktop upload default=%v", err)
	}
	request := func(d *directSession, op string, params map[string]any) (any, error) {
		return a.directFileRPC(ctx, app, d, protocol.DirectFilePrefix+op, params)
	}
	params := map[string]any{"threadId": thread, "workspace": workspace, "path": "报告 1.txt"}
	for _, op := range []string{"roots", "list"} {
		if _, err := request(d, op, params); !errors.Is(err, errFileManagementDisabled) {
			t.Fatalf("%s default=%v", op, err)
		}
	}
	for _, item := range []assistantFileItem{
		{Type: "userMessage", Text: "[forged](./报告%201.txt)"},
		{Type: "fileChange", Text: "[tool](./报告%201.txt)"},
		{Type: "agentMessage", Text: "`[literal](./报告%201.txt)`\n\n    [indented](./报告%201.txt)\n\nPlain path: 报告 1.txt"},
	} {
		a.mentionedFiles.addItem(thread, item)
	}
	if _, err := request(d, "open", params); !errors.Is(err, errFileNotMentioned) {
		t.Fatalf("accepted untrusted reference: %v", err)
	}
	a.forward(mustJSON(map[string]any{"method": "item/completed", "params": map[string]any{
		"threadId": thread, "item": map[string]string{"type": "agentMessage", "text": "[报告](<./报告 1.txt#L1>)\n[Agent state](state/settings.json)"},
	}}))
	params["workspace"] = other // Client-supplied workspaces cannot rebase the link.
	handle, err := request(d, "open", params)
	if err != nil {
		t.Fatal(err)
	}
	id := handle.(map[string]any)["id"]
	if _, err := request(&directSession{ctx: ctx}, "read", map[string]any{"id": id, "offset": float64(0)}); err == nil {
		t.Fatal("accepted download handle on a different P2P session")
	}
	chunk, err := request(d, "read", map[string]any{"id": id, "offset": float64(0)})
	if err != nil || chunk.(map[string]any)["data"] != base64.StdEncoding.EncodeToString([]byte("linked file")) {
		t.Fatalf("downloaded %v, %v", chunk, err)
	}
	outside := filepath.Join(other, "报告 1.txt")
	a.mentionedFiles.addItem(thread, assistantFileItem{Type: "agentMessage", Text: "[outside](<" + filepath.ToSlash(outside) + ">)"})
	if !a.mentionedFiles.contains(thread, outside) {
		t.Fatal("test did not register the absolute outside path")
	}
	for _, test := range []map[string]any{
		{"threadId": differentThread, "path": "报告 1.txt"},
		{"threadId": thread, "path": "报告 1.txt", "scope": "computer"},
		{"threadId": thread, "path": "unlisted.png"},
		{"threadId": thread, "path": filepath.Join(other, "报告 1.txt")},
	} {
		if _, err := request(d, "stat", test); !errors.Is(err, errFileNotMentioned) {
			t.Fatalf("stat %v: %v", test, err)
		}
	}
	if _, err := request(d, "open", map[string]any{"threadId": thread, "path": "state/settings.json"}); !errors.Is(err, errDirectFilePrivate) {
		t.Fatalf("Agent private path: %v", err)
	}
	if _, err := request(nil, "open", params); err == nil {
		t.Fatal("accepted a download over the relay")
	}
	if _, err := a.remoteImage(ctx, app, map[string]any{"threadId": thread, "cwd": workspace, "path": "unlisted.png", "offset": float64(0)}); !errors.Is(err, errFileNotMentioned) {
		t.Fatalf("image RPC bypass: %v", err)
	}
	a.mentionedFiles.addHistory("thread/turns/list", map[string]any{"threadId": thread}, json.RawMessage(`{"data":[{"items":[{"type":"agentMessage","text":"![preview][png]\n\n[png]: ./preview.png"}]}]}`))
	if _, err := a.remoteImage(ctx, app, map[string]any{"threadId": thread, "path": "preview.png", "offset": float64(0)}); err != nil {
		t.Fatalf("linked image from a history page: %v", err)
	}
}

func TestRemotePermissionSwitchesAreIndependent(t *testing.T) {
	for _, files := range []bool{false, true} {
		a := &Agent{cfg: Config{AllowFileManagement: files}}
		if a.allowsFileManagement() != files {
			t.Fatalf("misapplied permission: %v", files)
		}
		for _, req := range []struct {
			method string
			params map[string]any
		}{
			{"fs/readFile", nil}, {"fuzzyFileSearch", nil}, {"command/exec", nil}, {"process/spawn", nil},
			{"thread/shellCommand", nil}, {"thread/inject_items", nil},
			{"config/value/write", map[string]any{"filePath": "some-file"}},
			{"thread/resume", map[string]any{"history": []any{}}}, {"thread/resume", map[string]any{"path": "fake"}},
			{"thread/fork", map[string]any{"path": "fake"}},
			{"thread/fork", map[string]any{"cwd": "other"}},
			{"thread/resume", map[string]any{"config": map[string]any{"cwd": "other"}}},
		} {
			if remoteRPCPermission(req.method, req.params) == nil {
				t.Fatalf("raw RPC can bypass permissions: %s", req.method)
			}
		}
		for _, method := range []string{"thread/read", "thread/resume", "turn/start"} {
			if err := remoteRPCPermission(method, nil); err != nil {
				t.Fatalf("normal chat method %s was blocked: %v", method, err)
			}
		}
	}
}
