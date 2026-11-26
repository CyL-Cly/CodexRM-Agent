package appserver

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type MockServer struct {
	Addr string
	http *http.Server
	hub  *mockHub
}
type mockHub struct {
	mu          sync.Mutex
	threads     []map[string]any
	next        atomic.Int64
	active      map[string]context.CancelFunc
	connections map[*mockConn]bool
}
type mockConn struct {
	hub       *mockHub
	conn      *websocket.Conn
	textOnly  bool
	mu        sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan json.RawMessage
	ctx       context.Context
	loaded    map[string]bool
}

func StartMock(addr string) (*MockServer, error) {
	listener, e := net.Listen("tcp", addr)
	if e != nil {
		return nil, e
	}
	h := &mockHub{active: map[string]context.CancelFunc{}, connections: map[*mockConn]bool{}}
	mux := http.NewServeMux()
	up := websocket.Upgrader{}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, e := up.Upgrade(w, r, nil)
		if e == nil {
			h.serve(conn, r.Header.Get("X-CodexRM-TextOnly") == "1")
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	return &MockServer{Addr: listener.Addr().String(), http: srv, hub: h}, nil
}
func (s *MockServer) Close() error {
	s.hub.mu.Lock()
	for _, cancel := range s.hub.active {
		cancel()
	}
	for c := range s.hub.connections {
		_ = c.conn.Close()
	}
	s.hub.mu.Unlock()
	return s.http.Close()
}
func (c *mockConn) write(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(v)
}
func (c *mockConn) event(method string, p any) {
	_ = c.write(map[string]any{"method": method, "params": p})
}
func (c *mockConn) reply(id json.RawMessage, v any) {
	if len(id) > 0 {
		_ = c.write(map[string]any{"id": id, "result": v})
	}
}
func clone(v any) any { b, _ := json.Marshal(v); var out any; _ = json.Unmarshal(b, &out); return out }
func (h *mockHub) threadLocked(id string) map[string]any {
	for _, t := range h.threads {
		if t["id"] == id {
			return t
		}
	}
	return nil
}
func (h *mockHub) serve(conn *websocket.Conn, textOnly bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer conn.Close()
	c := &mockConn{hub: h, conn: conn, textOnly: textOnly, pending: map[string]chan json.RawMessage{}, ctx: ctx, loaded: map[string]bool{}}
	h.mu.Lock()
	h.connections[c] = true
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.connections, c); h.mu.Unlock() }()
	for {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if conn.ReadJSON(&msg) != nil {
			return
		}
		if msg.Method == "" {
			c.pendingMu.Lock()
			ch := c.pending[string(msg.ID)]
			c.pendingMu.Unlock()
			if ch != nil {
				select {
				case ch <- msg.Result:
				default:
				}
			}
			continue
		}
		p := msg.Params
		id := msg.ID
		tid, _ := p["threadId"].(string)
		switch msg.Method {
		case "initialize":
			c.reply(id, map[string]any{"userAgent": "codexrm-mock/0.2.0", "platformFamily": "mock"})
		case "experimentalFeature/list":
			enabled := !c.textOnly
			c.reply(id, map[string]any{"data": []any{
				map[string]any{"name": "shell_tool", "enabled": enabled, "defaultEnabled": true, "stage": "stable"},
				map[string]any{"name": "browser_use", "enabled": enabled, "defaultEnabled": true, "stage": "stable"},
				map[string]any{"name": "computer_use", "enabled": enabled, "defaultEnabled": true, "stage": "stable"},
				map[string]any{"name": "view_image", "enabled": enabled, "defaultEnabled": true, "stage": "stable"},
				map[string]any{"name": "shell_snapshot", "enabled": enabled, "defaultEnabled": true, "stage": "stable"},
			}})
		case "initialized":
		case "model/list":
			c.reply(id, map[string]any{"data": []any{map[string]any{"id": "mock-codex", "model": "mock-codex", "displayName": "Codex · 演示", "isDefault": true, "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "medium", "description": "默认"}}}}, "nextCursor": nil})
		case "account/read":
			c.reply(id, map[string]any{"account": map[string]any{"type": "mock", "email": "本地模拟模式"}, "requiresOpenaiAuth": false})
		case "thread/list":
			h.mu.Lock()
			data := []any{}
			for _, t := range h.threads {
				archived, _ := t["archived"].(bool)
				want, _ := p["archived"].(bool)
				cwd, _ := p["cwd"].(string)
				if archived == want && (cwd == "" || t["cwd"] == cwd) {
					th := clone(t).(map[string]any)
					delete(th, "turns")
					data = append(data, th)
				}
			}
			h.mu.Unlock()
			c.reply(id, map[string]any{"data": data, "nextCursor": nil})
		case "thread/loaded/list":
			data := []string{}
			for threadID := range c.loaded {
				data = append(data, threadID)
			}
			c.reply(id, map[string]any{"data": data, "nextCursor": nil})
		case "thread/unsubscribe":
			status := "notLoaded"
			if c.loaded[tid] {
				delete(c.loaded, tid)
				status = "unsubscribed"
			}
			c.reply(id, map[string]any{"status": status})
			if status == "unsubscribed" {
				c.event("thread/closed", map[string]any{"threadId": tid})
			}
		case "thread/start":
			cwd, _ := p["cwd"].(string)
			if cwd == "" {
				cwd = "mock-workspace"
			}
			model, _ := p["model"].(string)
			if model == "" {
				model = "mock-codex"
			}
			provider, _ := p["modelProvider"].(string)
			t := map[string]any{"id": fmt.Sprintf("thread_%d", h.next.Add(1)), "name": "新会话", "preview": "", "createdAt": time.Now().Unix(), "updatedAt": time.Now().Unix(), "cwd": cwd, "modelProvider": provider, "model": model, "status": map[string]any{"type": "idle"}, "turns": []any{}}
			h.mu.Lock()
			h.threads = append([]map[string]any{t}, h.threads...)
			copy := clone(t)
			h.mu.Unlock()
			c.loaded[t["id"].(string)] = true
			reply := map[string]any{"thread": copy, "model": model, "modelProvider": provider, "cwd": cwd}
			if sandbox, ok := p["sandbox"]; ok {
				reply["sandbox"] = sandbox
			}
			if approval, ok := p["approvalPolicy"]; ok {
				reply["approvalPolicy"] = approval
			}
			c.reply(id, reply)
			c.event("thread/started", map[string]any{"thread": copy})
		case "thread/fork":
			h.mu.Lock()
			source := h.threadLocked(tid)
			if source == nil {
				h.mu.Unlock()
				c.rpcError(id, "thread not found")
				break
			}
			thread := clone(source).(map[string]any)
			turns := thread["turns"].([]any)
			before, _ := p["beforeTurnId"].(string)
			last, _ := p["lastTurnId"].(string)
			if before != "" || last != "" {
				end := -1
				for i, value := range turns {
					turn := value.(map[string]any)
					if turn["id"] == before {
						end = i
						break
					}
					if turn["id"] == last {
						end = i + 1
						break
					}
				}
				if end < 0 {
					h.mu.Unlock()
					c.rpcError(id, "unknown fork turn")
					break
				}
				turns = turns[:end]
			}
			threadID := fmt.Sprintf("thread_%d", h.next.Add(1))
			thread["id"], thread["forkedFromId"], thread["archived"] = threadID, tid, false
			thread["createdAt"], thread["updatedAt"] = time.Now().Unix(), time.Now().Unix()
			thread["turns"], thread["status"] = turns, map[string]any{"type": "idle"}
			if len(turns) == 0 {
				thread["preview"] = ""
			}
			h.threads = append([]map[string]any{thread}, h.threads...)
			response := clone(thread).(map[string]any)
			h.mu.Unlock()
			c.loaded[threadID] = true
			if p["excludeTurns"] == true {
				delete(response, "turns")
			}
			c.reply(id, map[string]any{"thread": response, "model": response["model"], "modelProvider": response["modelProvider"]})
			c.event("thread/started", map[string]any{"thread": response})
		case "thread/resume", "thread/read":
			h.mu.Lock()
			t := h.threadLocked(tid)
			var copy any
			if t != nil {
				copy = clone(t)
			}
			h.mu.Unlock()
			if copy == nil {
				c.rpcError(id, "thread not found")
			} else {
				thread := copy.(map[string]any)
				if msg.Method == "thread/resume" {
					c.loaded[tid] = true
				}
				if (msg.Method == "thread/read" && p["includeTurns"] == false) ||
					(msg.Method == "thread/resume" && p["excludeTurns"] == true) {
					delete(thread, "turns")
				}
				c.reply(id, map[string]any{"thread": thread, "model": thread["model"], "modelProvider": thread["modelProvider"]})
			}
		case "thread/turns/list":
			cursor, _ := p["cursor"].(string)
			offset, _ := strconv.Atoi(cursor)
			offset = max(0, offset)
			limit, _ := p["limit"].(float64)
			h.mu.Lock()
			t := h.threadLocked(tid)
			data := []any{}
			var nextCursor any
			if t != nil {
				turns := t["turns"].([]any)
				for i := offset; i < len(turns); i++ {
					if limit > 0 && len(data) >= int(limit) {
						nextCursor = strconv.Itoa(i)
						break
					}
					index := i
					if p["sortDirection"] != "asc" {
						index = len(turns) - 1 - i
					}
					data = append(data, clone(turns[index]))
				}
			}
			h.mu.Unlock()
			c.reply(id, map[string]any{"data": data, "nextCursor": nextCursor})
		case "thread/name/set":
			h.mu.Lock()
			t := h.threadLocked(tid)
			if t != nil {
				t["name"] = p["name"]
			}
			h.mu.Unlock()
			c.reply(id, map[string]any{})
			c.event("thread/name/updated", map[string]any{"threadId": tid, "threadName": p["name"]})
		case "thread/archive", "thread/unarchive":
			h.mu.Lock()
			t := h.threadLocked(tid)
			if t != nil {
				t["archived"] = msg.Method == "thread/archive"
			}
			h.mu.Unlock()
			c.reply(id, map[string]any{})
			c.event(msg.Method+"d", map[string]any{"threadId": tid})
		case "turn/interrupt":
			h.mu.Lock()
			cancel := h.active[tid]
			h.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			c.reply(id, map[string]any{})
		case "turn/start", "turn/steer":
			if !c.loaded[tid] {
				c.rpcError(id, "thread is not loaded; resume it first")
				continue
			}
			if msg.Method == "turn/steer" {
				h.mu.Lock()
				t := h.threadLocked(tid)
				var turn map[string]any
				if t != nil && h.active[tid] != nil {
					turns := t["turns"].([]any)
					if len(turns) > 0 {
						turn = turns[len(turns)-1].(map[string]any)
					}
				}
				if turn == nil || turn["id"] != p["expectedTurnId"] {
					h.mu.Unlock()
					c.rpcError(id, "active turn does not match expectedTurnId")
					continue
				}
				input, _ := p["input"].([]any)
				if len(input) == 0 {
					h.mu.Unlock()
					c.rpcError(id, "input is required")
					continue
				}
				item := map[string]any{"id": fmt.Sprintf("user_%d", h.next.Add(1)), "type": "userMessage", "content": input}
				turn["items"] = append(turn["items"].([]any), item)
				turnID := turn["id"]
				copy := clone(item)
				h.mu.Unlock()
				c.reply(id, map[string]any{"turnId": turnID})
				c.event("item/completed", map[string]any{"threadId": tid, "turnId": turnID, "item": copy})
				continue
			}
			h.mu.Lock()
			t := h.threadLocked(tid)
			if t == nil {
				h.mu.Unlock()
				c.rpcError(id, "thread not found")
				continue
			}
			if h.active[tid] != nil {
				h.mu.Unlock()
				c.rpcError(id, "turn already active")
				continue
			}
			turnCtx, stop := context.WithCancel(ctx)
			h.active[tid] = stop
			text := ""
			if input, ok := p["input"].([]any); ok {
				for _, item := range input {
					if v, ok := item.(map[string]any); ok {
						if s, ok := v["text"].(string); ok {
							for _, line := range strings.Split(s, "\n") {
								const marker = "[CodexRM attachment] "
								var file struct{ Name string }
								if strings.HasPrefix(line, marker) && json.Unmarshal([]byte(strings.TrimPrefix(line, marker)), &file) == nil && file.Name != "" {
									line = "[附件：" + file.Name + "]"
								}
								if text != "" {
									text += "\n"
								}
								text += line
							}
						}
					}
				}
			}
			turn := map[string]any{"id": fmt.Sprintf("turn_%d", h.next.Add(1)), "status": "inProgress", "items": []any{map[string]any{"id": fmt.Sprintf("user_%d", h.next.Add(1)), "type": "userMessage", "content": p["input"]}}}
			t["turns"] = append(t["turns"].([]any), turn)
			t["status"] = map[string]any{"type": "active"}
			t["updatedAt"] = time.Now().Unix()
			if t["preview"] == "" {
				title := []rune(text)
				if len(title) > 28 {
					title = title[:28]
				}
				t["name"] = string(title)
				t["preview"] = text
			}
			copy := clone(turn)
			name := t["name"]
			h.mu.Unlock()
			c.reply(id, map[string]any{"turn": copy})
			c.event("turn/started", map[string]any{"threadId": tid, "turn": copy})
			c.event("thread/name/updated", map[string]any{"threadId": tid, "threadName": name})
			go c.runTurn(turnCtx, tid, turn, text)
		default:
			c.rpcError(id, "mock method not implemented: "+msg.Method)
		}
	}
}
func (c *mockConn) rpcError(id json.RawMessage, msg string) {
	if len(id) > 0 {
		_ = c.write(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": msg}})
	}
}
func (c *mockConn) runTurn(ctx context.Context, tid string, turn map[string]any, text string) {
	h := c.hub
	turnID := turn["id"]
	status := "completed"
	defer func() {
		h.mu.Lock()
		if ctx.Err() != nil {
			status = "interrupted"
		}
		turn["status"] = status
		delete(h.active, tid)
		if t := h.threadLocked(tid); t != nil {
			t["status"] = map[string]any{"type": "idle"}
		}
		copy := clone(turn)
		h.mu.Unlock()
		c.event("turn/completed", map[string]any{"threadId": tid, "turn": copy})
	}()
	reply := "已收到你的任务：**" + text + "**\n\n当前是 CodexRM 演示模式。这条回复通过本地 Agent 和服务器实时转发，未调用真实模型。\n\n连接真实 Codex 后，你可以在这里：\n- 让 Codex 阅读、修改本地项目\n- 查看命令输出和文件变更\n- 处理审批，随时停止任务\n\n```sh\ncodexrm-agent --server http://your-server:8080 --cwd /your/project\n```"
	if strings.Contains(text, "审批") || strings.Contains(text, "approval") {
		count := 1
		if strings.Contains(text, "multi approval") {
			count = 3
		}
		channels := make([]chan json.RawMessage, count)
		for i := range channels {
			key := fmt.Sprintf("mock-approval-%d", h.next.Add(1))
			ch := make(chan json.RawMessage, 1)
			channels[i] = ch
			c.pendingMu.Lock()
			c.pending[string(mustMockJSON(key))] = ch
			c.pendingMu.Unlock()
			defer func() { c.pendingMu.Lock(); delete(c.pending, string(mustMockJSON(key))); c.pendingMu.Unlock() }()
			_ = c.write(map[string]any{"id": key, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": tid, "turnId": turnID, "itemId": "mock-command", "command": "go test ./...", "cwd": "mock-workspace", "reason": "演示审批流程，不会执行真实命令。"}})
		}
		decisions := []string{}
		for _, ch := range channels {
			select {
			case <-ctx.Done():
				return
			case result := <-ch:
				var r struct {
					Decision string `json:"decision"`
				}
				_ = json.Unmarshal(result, &r)
				decisions = append(decisions, r.Decision)
			}
		}
		reply = "审批结果：**" + strings.Join(decisions, ", ") + "**。演示流程已完成，没有执行真实命令。"
	}
	if strings.Contains(text, "提问") || strings.Contains(text, "question") {
		key := fmt.Sprintf("mock-question-%d", h.next.Add(1))
		ch := make(chan json.RawMessage, 1)
		c.pendingMu.Lock()
		c.pending[string(mustMockJSON(key))] = ch
		c.pendingMu.Unlock()
		defer func() { c.pendingMu.Lock(); delete(c.pending, string(mustMockJSON(key))); c.pendingMu.Unlock() }()
		_ = c.write(map[string]any{"id": key, "method": "item/tool/requestUserInput", "params": map[string]any{
			"threadId": tid, "turnId": turnID, "itemId": key, "isBlocking": true,
			"questions": []any{
				map[string]any{"id": "approach", "header": "实现方式", "question": "你希望如何完成这项任务？", "isOther": true,
					"options": []any{
						map[string]any{"label": "直接实现（推荐）", "description": "完成代码修改并验证结果。"},
						map[string]any{"label": "先看方案", "description": "先解释实施方案，再开始修改。"},
					}},
				map[string]any{"id": "details", "header": "补充说明", "question": "还有哪些需要注意的要求？", "options": nil},
			},
		}})
		select {
		case <-ctx.Done():
			return
		case result := <-ch:
			reply = "已收到你的回答：\n\n```json\n" + string(result) + "\n```"
			c.event("serverRequest/resolved", map[string]any{"threadId": tid, "requestId": key})
		}
	}
	itemID := fmt.Sprintf("item_%d", h.next.Add(1))
	item := map[string]any{"id": itemID, "type": "agentMessage", "text": ""}
	h.mu.Lock()
	turn["items"] = append(turn["items"].([]any), item)
	h.mu.Unlock()
	c.event("item/started", map[string]any{"threadId": tid, "turnId": turnID, "item": clone(item)})
	runes := []rune(reply)
	for i := 0; i < len(runes); i += 6 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(35 * time.Millisecond):
		}
		end := i + 6
		if end > len(runes) {
			end = len(runes)
		}
		delta := string(runes[i:end])
		h.mu.Lock()
		item["text"] = item["text"].(string) + delta
		h.mu.Unlock()
		c.event("item/agentMessage/delta", map[string]any{"threadId": tid, "turnId": turnID, "itemId": itemID, "delta": delta})
	}
	h.mu.Lock()
	copy := clone(item)
	h.mu.Unlock()
	c.event("item/completed", map[string]any{"threadId": tid, "turnId": turnID, "item": copy})
}
func mustMockJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
