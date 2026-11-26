package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

func TestInlineVisualizationReferenceScope(t *testing.T) {
	const thread = "019aabce-9988-7001-8002-123456781234"
	const other = "019aabce-9988-7001-8002-123456781235"
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	path := filepath.Join(home, "visualizations", "2026", "11", "26", thread, "bars.html")
	otherPath := filepath.Join(home, "visualizations", "2026", "11", "26", other, "bars.html")
	write := func(p string, data []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ref := func(p string) string {
		data, _ := json.Marshal(map[string]string{"path": p, "mode": "wide"})
		return visualizationRefPrefix + string(data) + visualizationRefSuffix
	}
	read := func(a *Agent, id, p string) (any, error) {
		return a.readVisualization(context.Background(), nil, map[string]any{"threadId": id, "path": p})
	}
	html := []byte("<svg><text>图表</text></svg>")
	write(path, html)
	write(otherPath, html)
	a := &Agent{}
	a.mentionedFiles.addItem(thread, assistantFileItem{Type: "userMessage", Text: ref(path)})
	a.mentionedFiles.addItem(thread, assistantFileItem{Type: "agentMessage", Text: "```text\n" + ref(path) + "\n```\n\n[html](<" + filepath.ToSlash(path) + ">)"})
	if _, err := read(a, thread, path); err == nil {
		t.Fatal("untrusted reference authorized a visualization")
	}
	a.mentionedFiles.addItem(thread, assistantFileItem{Type: "agentMessage", Text: "Summary\n" + ref(path) + "\n\n" + ref(otherPath)})
	result, err := read(a, thread, path)
	if err != nil || result.(map[string]string)["html"] != string(html) {
		t.Fatalf("read: %v %v", result, err)
	}
	if _, err := read(a, thread, otherPath); err == nil {
		t.Fatal("another thread's file was accessible")
	}
	if _, err := read(a, other, path); err == nil {
		t.Fatal("reference leaked to another thread")
	}
	write(path, []byte{0xff})
	if _, err := read(a, thread, path); err == nil {
		t.Fatal("accepted invalid UTF-8")
	}
	write(path, make([]byte, maxVisualizationBytes+1))
	if _, err := read(a, thread, path); err == nil {
		t.Fatal("accepted oversized file")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(otherPath, path); err == nil {
		if _, err := read(a, thread, path); err == nil {
			t.Fatal("followed a symlink outside the thread")
		}
	}
}

func TestInlineVisualizationHistoryDoesNotAuthorizeFileDownloads(t *testing.T) {
	var index mentionedFileIndex
	index.addHistory("thread/turns/list", map[string]any{"threadId": "thread"},
		json.RawMessage(`{"data":[{"items":[{"type":"agentMessage","text":"::codex-inline-vis{file=\"chart.html\"}"}]}]}`))
	if !index.containsKind("thread", "chart.html", true) {
		t.Fatal("missing visualization history")
	}
	if index.contains("thread", "chart.html") {
		t.Fatal("visualization granted general file download access")
	}
	for _, line := range []string{`::codex-inline-vis{file="../chart.html"}`, `::codex-inline-vis{file="https://example.com/chart.html"}`, `::codex-inline-vis{file="chart.html:stream"}`, `::codex-inline-vis{file="chart.txt"}`} {
		if path := parseVisualizationReference(line); path != "" {
			t.Errorf("accepted %q as %q", line, path)
		}
	}
}

func TestInlineVisualizationRPCUsesThreadWorkspace(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	home := filepath.Join(workspace, ".codex")
	private := filepath.Join(workspace, "agent-state")
	t.Setenv("CODEX_HOME", home)
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(workspace, "view.html"), "<button>Workspace chart</button>")
	outsideChart := filepath.Join(outside, "out.html")
	write(outsideChart, "outside")
	write(filepath.Join(home, "unrelated.html"), "private codex")
	write(filepath.Join(private, "unrelated.html"), "private agent")

	mock, err := appserver.StartMock("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	app, err := appserver.Dial(ctx, "ws://"+mock.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	started, err := app.Call(ctx, "thread/start", map[string]any{"cwd": workspace})
	var result struct{ Thread struct{ ID string } }
	if err != nil || json.Unmarshal(started, &result) != nil || result.Thread.ID == "" {
		t.Fatalf("thread/start: %s, %v", started, err)
	}
	thread := result.Thread.ID
	a := &Agent{cfg: Config{Cwd: workspace, DataDir: private}}
	call := func(method string, params any) (reply struct {
		Result map[string]any
		Error  *appserver.RPCError
	}) { env := protocol.Envelope{Message: mustJSON(map[string]any{"id": 1, "method": method, "params": params})}; a.handle(ctx, app, func(response protocol.Envelope) error { return json.Unmarshal(response.Message, &reply) }, env, nil); return reply }
	if call("codexrm/info", nil).Result["inlineVisualizations"] != true {
		t.Fatal("no visualization capability")
	}
	params := map[string]any{"threadId": thread, "workspace": outside, "path": "view.html"}
	if call("codexrm/visualization/read", params).Error == nil {
		t.Fatal("unmentioned HTML was readable")
	}
	var refs []string
	for _, path := range []string{"view.html", outsideChart, ".codex/unrelated.html", "agent-state/unrelated.html"} {
		refs = append(refs, `::codex-inline-vis{file="`+path+`"}`)
	}
	a.forward(mustJSON(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": thread,
		"item": map[string]string{"type": "agentMessage", "text": strings.Join(refs, "\n\n")}}}))
	if reply := call("codexrm/visualization/read", params); reply.Error != nil || reply.Result["html"] != "<button>Workspace chart</button>" {
		t.Fatalf("wrong scope: %+v", reply)
	}
	for _, path := range []string{outsideChart, ".codex/unrelated.html", "agent-state/unrelated.html"} {
		params["path"] = path
		if call("codexrm/visualization/read", params).Error == nil {
			t.Fatalf("read forbidden path %q", path)
		}
	}
}
