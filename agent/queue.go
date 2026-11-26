package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/google/uuid"
)

// Queues live beside Codex, so closing a browser or losing the relay does not
// prevent the next message from starting. They end with this app-server session.
type queuedMessage struct {
	ID, Owner      string
	Params         map[string]any
	outcomeUnknown bool
}

type messageQueue struct {
	// Serialize resume/start/unsubscribe without blocking notification delivery.
	RPCMu               sync.Mutex
	Loaded              bool
	Detached            bool
	ProviderUnavailable bool
	Ephemeral           bool
	SessionApproval     bool
	ResumeParams        map[string]any
	Unloaded            chan struct{}
	Polling             bool
	Known               bool
	Entries             []*queuedMessage
	ActiveTurn          string
	LastCompleted       string
	Starting            bool
	ObservedStart       string
	Paused              bool
	Error               string
	Version             uint64
}

func (a *Agent) queueLocked(threadID string) *messageQueue {
	if a.queues == nil {
		a.queues = map[string]*messageQueue{}
	}
	q := a.queues[threadID]
	if q == nil {
		q = &messageQueue{}
		a.queues[threadID] = q
	}
	return q
}

func (a *Agent) queueViewLocked(threadID string) map[string]any {
	q := a.queueLocked(threadID)
	entries := []any{}
	for _, entry := range q.Entries {
		entries = append(entries, map[string]any{"id": entry.ID, "input": entry.Params["input"], "model": entry.Params["model"], "effort": entry.Params["effort"]})
	}
	return map[string]any{"threadId": threadID, "entries": entries, "paused": q.Paused, "error": q.Error, "dispatching": q.Starting, "version": q.Version}
}

func (a *Agent) publishQueue(view map[string]any) {
	a.publishEvent(protocol.Envelope{Type: protocol.TypeServerMessage, Message: mustJSON(map[string]any{"method": "codexrm/queue/updated", "params": view})})
}

func (a *Agent) publishThreadState(state, threadID string) {
	a.publishEvent(protocol.Envelope{Type: protocol.TypeServerMessage, Message: mustJSON(map[string]any{
		"method": "codexrm/thread/" + state,
		"params": map[string]any{"threadId": threadID},
	})})
}

func (a *Agent) ensureQueue(ctx context.Context, app *appserver.Client, threadID string) error {
	a.queueMu.Lock()
	known := a.queues[threadID] != nil && a.queues[threadID].Known
	a.queueMu.Unlock()
	if known {
		return nil
	}
	raw, err := app.Call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false})
	if err != nil {
		return err
	}
	var result struct {
		Thread struct {
			Status struct{ Type string }         `json:"status"`
			Turns  []struct{ ID, Status string } `json:"turns"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	// Long histories may require the paginated API even for their active turn.
	if result.Thread.Status.Type == "active" && len(result.Thread.Turns) == 0 {
		raw, err = app.Call(ctx, "thread/turns/list", map[string]any{"threadId": threadID, "limit": 1, "sortDirection": "desc", "itemsView": "full"})
		if err != nil {
			return err
		}
		var page struct {
			Data []struct{ ID, Status string } `json:"data"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		result.Thread.Turns = page.Data
	}
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	// Notifications received during thread/read take precedence over its snapshot.
	q := a.queueLocked(threadID)
	if !q.Known {
		for _, turn := range result.Thread.Turns {
			if turn.Status == "inProgress" {
				q.ActiveTurn = turn.ID
			}
		}
		if q.ActiveTurn == "" && result.Thread.Status.Type == "active" {
			return fmt.Errorf("无法确认当前任务状态，请重新打开会话后再排队")
		}
		q.Known = true
	}
	return nil
}

func (a *Agent) queueRPC(ctx context.Context, app *appserver.Client, owner, method string, params map[string]any) (any, error) {
	threadID, _ := params["threadId"].(string)
	if threadID == "" {
		return nil, fmt.Errorf("threadId is required")
	}
	if method != "codexrm/queue/list" && method != "codexrm/queue/add" && method != "codexrm/queue/remove" && method != "codexrm/queue/resume" {
		return nil, fmt.Errorf("unknown queue method: %s", method)
	}
	if method == "codexrm/queue/add" {
		// Do not enqueue a message while its thread is switching providers.
		q := a.threadQueue(threadID)
		q.RPCMu.Lock()
		defer q.RPCMu.Unlock()
	}
	if method == "codexrm/queue/add" || method == "codexrm/queue/resume" {
		if err := a.ensureQueue(ctx, app, threadID); err != nil {
			return nil, requestNotSent(err)
		}
	}
	a.queueMu.Lock()
	q := a.queueLocked(threadID)
	if q.ProviderUnavailable && (method == "codexrm/queue/add" || method == "codexrm/queue/resume") {
		a.queueMu.Unlock()
		return nil, errProviderUnavailable
	}
	switch method {
	case "codexrm/queue/add":
		input, _ := params["input"].([]any)
		text := ""
		hasMedia := false
		for _, item := range input {
			if item, ok := item.(map[string]any); ok {
				if item["type"] == "text" {
					value, _ := item["text"].(string)
					text += value
				}
				if item["type"] == "localImage" || item["type"] == "image" {
					hasMedia = true
				}
			}
		}
		if (strings.TrimSpace(text) == "" && !hasMedia) || len(q.Entries) >= 50 {
			a.queueMu.Unlock()
			return nil, fmt.Errorf("排队消息不能为空，每个会话最多排队 50 条")
		}
		q.Entries = append(q.Entries, &queuedMessage{ID: uuid.NewString(), Owner: owner, Params: params})
	case "codexrm/queue/remove":
		id, _ := params["id"].(string)
		index := -1
		for i, entry := range q.Entries {
			if entry.ID == id {
				index = i
			}
		}
		if index < 0 || (index == 0 && q.Starting) {
			a.queueMu.Unlock()
			return nil, fmt.Errorf("消息已发送或正在发送，请同步排队列表")
		}
		q.Entries = append(q.Entries[:index], q.Entries[index+1:]...)
	case "codexrm/queue/resume":
		if len(q.Entries) > 0 && q.Entries[0].outcomeUnknown {
			a.queueMu.Unlock()
			return nil, fmt.Errorf("上一条消息是否已执行无法确认。请检查会话历史，移除该条消息后再继续排队")
		}
		q.Paused = false
		q.Error = ""
	}
	if method != "codexrm/queue/list" {
		q.Version++
	}
	view := a.queueViewLocked(threadID)
	queueCtx := a.queueCtx
	a.queueMu.Unlock()
	if method != "codexrm/queue/list" {
		a.publishQueue(view)
		if queueCtx != nil {
			go a.drainQueue(queueCtx, app, threadID)
		}
	}
	return view, nil
}

func (a *Agent) queueNotification(ctx context.Context, app *appserver.Client, raw json.RawMessage) {
	var event struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string                      `json:"threadId"`
			Turn     struct{ ID, Status string } `json:"turn"`
		} `json:"params"`
	}
	if ctx.Err() != nil || json.Unmarshal(raw, &event) != nil || event.Params.ThreadID == "" {
		return
	}
	if event.Method == "thread/closed" {
		a.queueMu.Lock()
		if q := a.queues[event.Params.ThreadID]; q != nil {
			q.SessionApproval = false
			if q.Unloaded != nil {
				close(q.Unloaded)
				q.Unloaded = nil
			}
		}
		a.queueMu.Unlock()
		return
	}
	if event.Method != "turn/started" && event.Method != "turn/completed" && event.Method != "thread/archived" && event.Method != "serverRequest/resolved" {
		return
	}
	threadID := event.Params.ThreadID
	a.queueMu.Lock()
	q := a.queueLocked(threadID)
	switch event.Method {
	case "turn/started":
		q.Known = true
		q.ActiveTurn = event.Params.Turn.ID
		if q.Starting {
			q.ObservedStart = event.Params.Turn.ID
		}
	case "turn/completed":
		q.Known = true
		q.LastCompleted = event.Params.Turn.ID
		if q.ActiveTurn == event.Params.Turn.ID {
			q.ActiveTurn = ""
		}
		if event.Params.Turn.Status != "completed" && len(q.Entries) > 0 {
			q.Paused = true
			q.Error = "当前任务已停止或失败，排队已暂停"
		}
	case "thread/archived":
		q.SessionApproval = false
		q.Paused = true
		q.Error = "会话已归档，排队已暂停"
	}
	q.Version++
	view := a.queueViewLocked(threadID)
	a.queueMu.Unlock()
	// Never make an RPC from the app-server reader callback itself.
	go func() {
		if ctx.Err() != nil {
			return
		}
		a.publishQueue(view)
		a.drainQueue(ctx, app, threadID)
		a.releaseIdleThread(ctx, app, threadID)
	}()
}

const detachedQueuePollInterval = time.Second

func (a *Agent) startDetachedQueuePoll(ctx context.Context, app *appserver.Client, threadID string) {
	a.queueMu.Lock()
	q := a.queueLocked(threadID)
	if q.Polling || !q.Detached || q.Paused || q.Starting || q.ActiveTurn == "" || len(q.Entries) == 0 {
		a.queueMu.Unlock()
		return
	}
	q.Polling = true
	a.queueMu.Unlock()
	go a.pollDetachedQueue(ctx, app, threadID, q)
}

// A detached thread receives no turn/completed notification from a desktop
// writer. Poll its durable status so queued work resumes when that writer idles.
func (a *Agent) pollDetachedQueue(ctx context.Context, app *appserver.Client, threadID string, q *messageQueue) {
	defer func() {
		a.queueMu.Lock()
		q.Polling = false
		a.queueMu.Unlock()
	}()
	for {
		timer := time.NewTimer(detachedQueuePollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		a.queueMu.Lock()
		blocked := !q.Detached || q.Paused || q.Starting || q.ActiveTurn == "" || len(q.Entries) == 0
		a.queueMu.Unlock()
		if blocked || ctx.Err() != nil {
			return
		}
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		raw, err := app.Call(callCtx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false})
		cancel()
		if err != nil {
			continue
		}
		var snapshot struct {
			Thread struct{ Status struct{ Type string } } `json:"thread"`
		}
		if json.Unmarshal(raw, &snapshot) != nil || snapshot.Thread.Status.Type != "idle" {
			continue
		}
		a.queueMu.Lock()
		if q.Detached && !q.Paused && !q.Starting && len(q.Entries) > 0 {
			q.ActiveTurn = ""
			q.Known = true
			a.queueMu.Unlock()
			a.drainQueue(ctx, app, threadID)
			return
		}
		a.queueMu.Unlock()
		return
	}
}

func (a *Agent) drainQueue(ctx context.Context, app *appserver.Client, threadID string) {
	a.queueMu.Lock()
	q := a.queueLocked(threadID)
	if ctx.Err() != nil || a.updating || q.Paused || q.Starting || q.ActiveTurn != "" || len(q.Entries) == 0 {
		poll := ctx.Err() == nil && !a.updating && q.Detached && !q.Paused && !q.Starting && q.ActiveTurn != "" && len(q.Entries) > 0
		a.queueMu.Unlock()
		if poll {
			a.startDetachedQueuePoll(ctx, app, threadID)
		}
		return
	}
	entry := q.Entries[0]
	q.Starting = true
	q.ObservedStart = ""
	q.Version++
	view := a.queueViewLocked(threadID)
	a.queueMu.Unlock()
	a.publishQueue(view)

	callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	raw, err := a.callThread(callCtx, app, entry.Owner, "turn/start", entry.Params)
	var result struct {
		Turn struct{ ID string } `json:"turn"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &result)
		if err == nil && result.Turn.ID == "" {
			err = fmt.Errorf("Codex 未返回任务编号，请同步历史后再继续排队")
		}
	}
	a.queueMu.Lock()
	if a.queues[threadID] != q || ctx.Err() != nil {
		a.queueMu.Unlock()
		return
	}
	q.Starting = false
	observed := q.ObservedStart
	q.ObservedStart = ""
	var rejected *appserver.RPCError
	if err != nil && !errors.As(err, &rejected) && observed != "" {
		result.Turn.ID = observed
		err = nil
	}
	if err != nil {
		q.Paused = true
		entry.outcomeUnknown = !errors.As(err, &rejected)
		q.Error = err.Error()
		if entry.outcomeUnknown {
			q.Error = "发送结果未知。请检查会话历史，移除该条消息后再继续排队"
		}
	} else {
		q.Entries = q.Entries[1:]
		if q.LastCompleted != result.Turn.ID {
			q.ActiveTurn = result.Turn.ID
		}
	}
	q.Version++
	view = a.queueViewLocked(threadID)
	a.queueMu.Unlock()
	a.publishQueue(view)
	if err == nil {
		a.drainQueue(ctx, app, threadID)
	}
	a.releaseIdleThread(ctx, app, threadID)
}
