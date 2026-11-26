package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/gorilla/websocket"
)

func TestLargeThreadHistoryKeepsRelayConnected(t *testing.T) {
	const largeThread = "large"
	const oversizedTurn = "oversized-turn"
	const legacyThread = "legacy"
	const generatedFiles = "generated-files"
	const imageSize = 81 * (1 << 20) / 10
	root := t.TempDir()
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aA3sAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	largeImage := strings.Repeat("A", base64.StdEncoding.EncodedLen(imageSize))
	smallImage := strings.Repeat("B", 1<<20)
	turns := make([]json.RawMessage, 7)
	inlineItems := make([]map[string]string, 0, 4)
	fileItems := make([]map[string]string, 0, 4)
	for i := range turns {
		id := strconv.Itoa(i)
		item := map[string]string{"type": "imageGeneration", "id": "image-" + id, "result": smallImage}
		if i < 4 {
			item["result"] = largeImage
			inlineItems = append(inlineItems, item)
			path := filepath.Join(root, "generated-"+id+".png")
			data := append(bytes.Clone(png), bytes.Repeat([]byte{byte(i)}, imageSize-len(png))...)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			fileItems = append(fileItems, map[string]string{"type": "imageGeneration", "id": item["id"], "fileId": id, "savedPath": path})
		}
		turns[i] = raw(map[string]any{"id": id, "status": "completed", "items": []any{item}})
	}
	oversized := raw(map[string]any{"id": "too-large", "status": "completed", "items": inlineItems})
	filesTurn := raw(map[string]any{"id": "files", "status": "completed", "items": fileItems})
	up := websocket.Upgrader{}
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			var req struct {
				ID     json.RawMessage
				Method string
				Params struct {
					ThreadID                   string
					ExcludeTurns, IncludeTurns bool
					Limit                      int
					Cursor, ItemsView          string
				}
			}
			if err := c.ReadJSON(&req); err != nil {
				return
			}
			if req.ID == nil {
				continue
			}
			answer := map[string]any{"id": req.ID, "result": map[string]any{}}
			history := turns
			switch req.Params.ThreadID {
			case generatedFiles:
				history = []json.RawMessage{filesTurn}
			case oversizedTurn:
				history = []json.RawMessage{turns[0], oversized}
			case legacyThread:
				history = []json.RawMessage{raw(map[string]string{"id": "legacy-turn"})}
			}
			switch req.Method {
			case "test/emitGeneratedImages":
				for _, item := range inlineItems {
					if err := c.WriteJSON(map[string]any{"method": "item/completed", "params": map[string]any{
						"threadId": oversizedTurn, "turnId": "too-large", "item": item,
					}}); err != nil {
						return
					}
				}
				if err := c.WriteJSON(map[string]any{"method": "turn/completed", "params": map[string]any{
					"threadId": oversizedTurn, "turn": oversized,
				}}); err != nil {
					return
				}
			case "thread/resume", "thread/read":
				thread := map[string]any{"id": req.Params.ThreadID, "historyMode": "inline", "status": map[string]string{"type": "idle"}, "turns": []any{}}
				if req.Method == "thread/resume" && !req.Params.ExcludeTurns || req.Method == "thread/read" && req.Params.IncludeTurns {
					thread["turns"] = history
				}
				answer["result"] = map[string]any{"thread": thread, "model": "fixture"}
			case "thread/turns/list":
				if req.Params.ThreadID == legacyThread {
					delete(answer, "result")
					answer["error"] = map[string]any{"code": -32601, "message": "Method not found"}
					break
				}
				if req.Params.Limit != 1 {
					delete(answer, "result")
					answer["error"] = map[string]any{"code": -32602, "message": "expected one turn per upstream request"}
					break
				}
				offset, _ := strconv.Atoi(req.Params.Cursor)
				page := []json.RawMessage{}
				for i := len(history) - 1 - offset; i >= 0 && len(page) < req.Params.Limit; i-- {
					turn := history[i]
					if req.Params.ThreadID == oversizedTurn && i == 1 && req.Params.ItemsView == "notLoaded" {
						turn = raw(map[string]any{"id": "too-large", "status": "completed", "items": []any{}})
					}
					page = append(page, turn)
				}
				var cursor any
				if offset+len(page) < len(history) {
					cursor = strconv.Itoa(offset + len(page))
				}
				answer["result"] = map[string]any{"data": page, "nextCursor": cursor}
			}
			if err := c.WriteJSON(answer); err != nil {
				return
			}
		}
	}))
	defer app.Close()

	_, relay := newServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := agent.New(agent.Config{ServerURL: relay.URL, AppServerURL: "ws" + strings.TrimPrefix(app.URL, "http"), DataDir: t.TempDir(), Name: "history fixture", EnrollmentKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := a.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Agent did not stop")
		}
	}()
	var claimed protocol.PairClaimResponse
	post(t, relay, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.ManualPairingCode}, &claimed)
	c := ws(t, relay, "client", claimed.Token)
	until(t, c, func(e protocol.Envelope) bool { return e.Status == "connected" })
	callID := 0
	call := func(method string, params map[string]any) json.RawMessage {
		t.Helper()
		callID++
		rpc(t, c, callID, method, params)
		return until(t, c, func(e protocol.Envelope) bool {
			if e.Type == protocol.TypeAgentStatus && e.Status != string(protocol.StatusConnected) {
				t.Fatalf("opening history disconnected the Agent: %s", e.Status)
			}
			var res struct{ ID int }
			_ = json.Unmarshal(e.Message, &res)
			return res.ID == callID
		}).Message
	}
	type pageResponse struct {
		Result struct {
			Model      string
			NextCursor string
			Thread     struct{ Turns []json.RawMessage }
			Data       []json.RawMessage
		}
		Error *struct{ Code int }
	}
	decode := func(message json.RawMessage) pageResponse {
		t.Helper()
		var out pageResponse
		if err := json.Unmarshal(message, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	opened := decode(call("thread/resume", map[string]any{"threadId": largeThread}))
	if opened.Error != nil || opened.Result.Model != "fixture" || opened.Result.NextCursor != "3" || len(opened.Result.Thread.Turns) != 3 {
		t.Fatal("resume did not return a partial page with a cursor and model")
	}
	loaded := opened.Result.Thread.Turns
	for cursor := opened.Result.NextCursor; cursor != ""; {
		page := decode(call("thread/turns/list", map[string]any{"threadId": largeThread, "cursor": cursor, "limit": 30, "sortDirection": "desc", "itemsView": "full"}))
		if page.Error != nil || len(page.Result.Data) == 0 || page.Result.NextCursor == cursor {
			t.Fatal("older history did not advance")
		}
		for _, turn := range page.Result.Data {
			loaded = append([]json.RawMessage{turn}, loaded...)
		}
		cursor = page.Result.NextCursor
	}
	if len(loaded) != len(turns) {
		t.Fatalf("pagination returned %d turns, want %d", len(loaded), len(turns))
	}
	for i, turn := range loaded {
		filtered, _ := protocol.WithoutInlineFiles(turns[i])
		if string(turn) != string(filtered) {
			t.Fatalf("turn %d was changed, skipped or reordered", i)
		}
	}
	snapshot := decode(call("codexrm/thread/snapshot", map[string]any{"threadId": largeThread}))
	if snapshot.Error != nil || snapshot.Result.NextCursor == "" || len(snapshot.Result.Thread.Turns) == 0 {
		t.Fatal("snapshot did not return a partial page")
	}
	if result := decode(call("thread/resume", map[string]any{"threadId": legacyThread})); result.Error != nil || len(result.Result.Thread.Turns) != 1 {
		t.Fatal("resume fallback lost legacy history")
	}
	degraded := decode(call("thread/resume", map[string]any{"threadId": oversizedTurn}))
	if degraded.Error != nil || len(degraded.Result.Thread.Turns) != 1 || degraded.Result.NextCursor != "1" {
		t.Fatal("oversized turn should open as metadata while leaving older history available")
	}
	var omitted struct {
		ID                    string
		CodexRMHistoryOmitted bool
	}
	if err := json.Unmarshal(degraded.Result.Thread.Turns[0], &omitted); err != nil || omitted.ID != "too-large" || !omitted.CodexRMHistoryOmitted {
		t.Fatalf("oversized turn marker or identity lost: %+v (%v)", omitted, err)
	}
	older := decode(call("thread/turns/list", map[string]any{"threadId": oversizedTurn, "cursor": degraded.Result.NextCursor}))
	if older.Error != nil || len(older.Result.Data) != 1 || !bytes.Equal(older.Result.Data[0], loaded[0]) {
		t.Fatal("could not paginate past an oversized turn")
	}
	if result := decode(call("thread/read", map[string]any{"threadId": oversizedTurn, "includeTurns": true})); result.Error == nil {
		t.Fatal("expected an RPC error for a large raw history response")
	}
	files := decode(call("thread/resume", map[string]any{"threadId": generatedFiles}))
	if files.Error != nil || len(files.Result.Thread.Turns) != 1 || !bytes.Equal(files.Result.Thread.Turns[0], filesTurn) {
		t.Fatal("generated file references were lost in history")
	}
	for _, item := range fileItems {
		assertRelayFileDenied(t, c, "codexrm/image/read", map[string]any{"path": item["savedPath"]})
	}
	callID++
	rpc(t, c, callID, "test/emitGeneratedImages", nil)
	receivedImages := 0
	until(t, c, func(e protocol.Envelope) bool {
		if e.Type == protocol.TypeAgentStatus && e.Status != string(protocol.StatusConnected) {
			t.Fatalf("generated images disconnected the Agent: %s", e.Status)
		}
		var event struct {
			ID     int
			Method string
			Params struct{ Item map[string]string }
			Error  *struct{ Code int }
		}
		_ = json.Unmarshal(e.Message, &event)
		if event.Method == "item/completed" {
			if receivedImages >= len(inlineItems) || event.Params.Item["id"] != inlineItems[receivedImages]["id"] || event.Params.Item["result"] != "" {
				t.Fatal("generated image metadata was reordered or image bytes reached the relay")
			}
			receivedImages++
		}
		if event.Method == "turn/completed" {
			t.Fatal("oversized aggregate notification was unexpectedly forwarded")
		}
		if event.ID != callID {
			return false
		}
		if event.Error != nil || receivedImages != len(inlineItems) {
			t.Fatalf("did not receive all generated images before the next reply: %d", receivedImages)
		}
		return true
	})
	if result := decode(call("codexrm/info", nil)); result.Error != nil {
		t.Fatal("Agent could not answer a subsequent request")
	}
}
