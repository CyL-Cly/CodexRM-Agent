package agent

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
)

func (a *Agent) wxTaskStart(ctx context.Context, clientID string, params map[string]any) (any, error) {
	policy := a.currentPolicy(clientID)
	kind, _ := params["kind"].(string)
	groupID, _ := params["groupId"].(string)
	wxID, _ := params["wxId"].(string)
	var profile wxProfile
	switch kind {
	case "summary":
		configured, ok := policy.Summaries[groupID]
		if !ok {
			return nil, robotErr("此群未启用群聊总结")
		}
		profile = configured
	case "moments":
		if !policy.Moments || !validWxActor(wxID) {
			return nil, robotErr("未启用朋友圈自动互动")
		}
		profile = policy.Access[wxID].wxProfile
	case "memory":
		groupName, isGroup := strings.CutSuffix(groupID, "@chatroom")
		if !policy.Memory || (groupID == "" && !validWxActor(wxID)) ||
			(groupID != "" && (!isGroup || !validWxActor(groupName))) {
			return nil, robotErr("未启用记忆提取")
		}
		if isGroup {
			profile = policy.Groups[groupID].wxProfile
		} else {
			profile = policy.Access[wxID].wxProfile
		}
	case "":
		access := policy.Access[wxID]
		if !access.Enabled {
			return nil, robotErr("此联系人未授权 Codex")
		}
		profile = access.wxProfile
	default:
		return nil, robotErr("Codex 任务类型无效")
	}
	instructions, _ := params["instructions"].(string)
	input, err := robotInputWithLimit(params["input"], 5)
	raw, _ := json.Marshal(input)
	if err != nil || len(raw) > 5<<20 || strings.TrimSpace(instructions) == "" || len(instructions) > 8192 {
		return nil, robotErr("Codex 任务输入无效")
	}
	cwd, err := a.wxWorkspace(profile.Workspace)
	if err != nil {
		return nil, err
	}
	app, err := a.ensureTextApp(ctx)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := map[string]any{"ephemeral": true, "developerInstructions": instructions}
	if err := constrainRobotParams("thread/start", start, cwd, "text"); err != nil {
		return nil, err
	}
	if profile.Model != "" {
		start["model"] = profile.Model
	}
	value, err := a.callThread(callCtx, app, clientID, "thread/start", start)
	if err != nil {
		return nil, err
	}
	var created struct{ Thread struct{ ID string } }
	if json.Unmarshal(value, &created) != nil || created.Thread.ID == "" {
		return nil, robotErr("Codex 未返回任务会话")
	}
	threadID := created.Thread.ID
	a.rememberWxThread(threadID, clientID, "", "task")
	turnParams := map[string]any{"threadId": threadID, "input": input, "cwd": cwd,
		"approvalPolicy": "never", "sandboxPolicy": map[string]any{"type": "readOnly", "networkAccess": false}}
	value, err = a.callThread(callCtx, app, clientID, "turn/start", turnParams)
	if err != nil {
		a.closeWxTask(app, threadID, "")
		return nil, err
	}
	var started struct{ Turn struct{ ID string } }
	if json.Unmarshal(value, &started) != nil || started.Turn.ID == "" {
		a.closeWxTask(app, threadID, "")
		return nil, robotErr("Codex 未返回任务 ID")
	}
	return map[string]string{"threadId": threadID, "turnId": started.Turn.ID}, nil
}

func (a *Agent) wxTaskClose(clientID string, params map[string]any) error {
	threadID, _ := params["threadId"].(string)
	turnID, _ := params["turnId"].(string)
	thread, err := a.ownThread(clientID, "", threadID)
	if err != nil || thread.Backend != "task" {
		return robotErr("无权操作该任务")
	}
	a.textMu.Lock()
	app := a.textApp
	a.textMu.Unlock()
	a.closeWxTask(app, threadID, turnID)
	return nil
}

func (a *Agent) closeWxTask(app *appserver.Client, threadID, turnID string) {
	defer func() {
		a.wxMu.Lock()
		delete(a.wxThreads, threadID)
		a.wxMu.Unlock()
		a.mu.Lock()
		delete(a.owners, threadID)
		a.mu.Unlock()
		a.queueMu.Lock()
		delete(a.queues, threadID)
		a.queueMu.Unlock()
	}()
	if app == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if turnID != "" {
		_, _ = app.Call(ctx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
	}
	_, _ = app.Call(ctx, "thread/unsubscribe", map[string]any{"threadId": threadID})
}
