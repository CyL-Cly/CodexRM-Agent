package agent

import (
	"encoding/json"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
)

// TryStartUpdate reserves the idle agent until shutdown. Admission and the idle
// check share queueMu so a new request cannot slip between checking and restart.
func (a *Agent) TryStartUpdate(start func() error) (bool, error) {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	if a.updating || a.requests != 0 {
		return false, nil
	}
	for _, q := range a.queues {
		if q.ActiveTurn != "" || q.Starting || len(q.Entries) != 0 {
			return false, nil
		}
	}
	a.updating = true
	if err := start(); err != nil {
		a.updating = false
		return false, err
	}
	return true, nil
}

func (a *Agent) beginRequest() bool {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	if a.updating {
		return false
	}
	a.requests++
	return true
}

func (a *Agent) endRequest() {
	a.queueMu.Lock()
	a.requests--
	a.queueMu.Unlock()
}

// Responses and notifications can arrive in either order. Track the returned
// turn before releasing the request, without reviving an already completed turn.
func (a *Agent) recordStartedTurn(method string, params map[string]any, raw json.RawMessage) {
	if method != "turn/start" {
		return
	}
	threadID, _ := params["threadId"].(string)
	var result struct {
		Turn struct{ ID, Status string } `json:"turn"`
	}
	if threadID == "" || json.Unmarshal(raw, &result) != nil || result.Turn.ID == "" {
		return
	}
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	q := a.queueLocked(threadID)
	if q.LastCompleted != result.Turn.ID && (result.Turn.Status == "" || result.Turn.Status == "inProgress") {
		q.ActiveTurn = result.Turn.ID
		q.Known = true
	}
}

var updateInProgressError = &appserver.RPCError{Code: -32001, Message: "Agent 正在安装更新，请重连后重试"}
