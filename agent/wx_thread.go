package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

func (a *Agent) textOnlyStatus(ctx context.Context) (bool, string) {
	if _, err := a.ensureTextApp(ctx); err != nil {
		return false, textOnlyUnavailable
	}
	return true, ""
}

func (a *Agent) ensureTextApp(ctx context.Context) (*appserver.Client, error) {
	a.textMu.Lock()
	defer a.textMu.Unlock()
	if a.textApp != nil {
		select {
		case <-a.textApp.Done():
			a.textApp = nil
			a.textVerified = false
		default:
			if a.textVerified {
				return a.textApp, nil
			}
		}
	}
	app, err := a.openTextApp(ctx)
	if err != nil {
		return nil, robotErr(textOnlyUnavailable)
	}
	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, err = app.Initialize(initCtx, "codexrm-agent-text", protocol.Version)
	cancel()
	if err != nil {
		_ = app.Close()
		return nil, robotErr(textOnlyUnavailable)
	}
	if err = a.verifyTextOnly(ctx, app); err != nil {
		_ = app.Close()
		return nil, robotErr(textOnlyUnavailable)
	}
	app.OnNotification(func(raw json.RawMessage) {
		if !a.handleWxHistoryTool(app, raw) {
			a.onTextNotification(raw)
		}
	})
	a.textApp = app
	a.textVerified = true
	go func() {
		<-app.Done()
		a.clearWxRecordApp(app)
	}()
	return app, nil
}

func (a *Agent) openTextApp(ctx context.Context) (*appserver.Client, error) {
	if a.cfg.Mock {
		return appserver.DialTextOnly(ctx, a.cfg.AppServerURL, a.cfg.AppServerToken)
	}
	if a.cfg.AppServerURL != "" {
		return appserver.Dial(ctx, a.cfg.AppServerURL, a.cfg.AppServerToken)
	}
	return appserver.LaunchTextOnly(ctx, a.cfg.CodexPath, a.cfg.Cwd)
}

func (a *Agent) verifyTextOnly(ctx context.Context, app *appserver.Client) error {
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	raw, err := app.Call(callCtx, "experimentalFeature/list", map[string]any{})
	if err != nil {
		return err
	}
	var listed struct {
		Data []struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &listed) != nil {
		return robotErr(textOnlyUnavailable)
	}
	found := map[string]bool{}
	for _, feature := range listed.Data {
		found[feature.Name] = feature.Enabled
	}
	for _, name := range []string{"shell_tool", "browser_use", "computer_use", "view_image", "shell_snapshot"} {
		enabled, ok := found[name]
		if !ok || enabled {
			return robotErr(textOnlyUnavailable)
		}
	}
	started, err := app.Call(callCtx, "thread/start", map[string]any{
		"cwd": a.cfg.Cwd, "approvalPolicy": "never", "sandbox": "read-only", "ephemeral": true,
		"config": map[string]any{"sandbox_mode": "read-only", "approval_policy": "never", "web_search": "disabled"},
	})
	if err != nil {
		return err
	}
	var probe struct {
		ApprovalPolicy any `json:"approvalPolicy"`
		Sandbox        any `json:"sandbox"`
		Thread         struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(started, &probe) != nil || probe.ApprovalPolicy != "never" || !sandboxIsReadOnly(probe.Sandbox) {
		return robotErr(textOnlyUnavailable)
	}
	if probe.Thread.ID != "" {
		_, _ = app.Call(callCtx, "thread/unsubscribe", map[string]any{"threadId": probe.Thread.ID})
	}
	return nil
}

func sandboxIsReadOnly(value any) bool {
	switch sandbox := value.(type) {
	case string:
		return sandbox == "read-only" || sandbox == "readOnly"
	case map[string]any:
		kind, _ := sandbox["type"].(string)
		return kind == "readOnly" || kind == "read-only"
	default:
		return false
	}
}

func (a *Agent) robotThread(ctx context.Context, main *appserver.Client, clientID, method string, params map[string]any) (json.RawMessage, error) {
	policy := a.currentPolicy(clientID)
	actor, _ := params["wxId"].(string)
	group, _ := params["groupId"].(string)
	profile := policy.Access[actor].wxProfile
	if group != "" {
		profile = policy.Groups[group].wxProfile
	}
	wxid, mode, ok := wxThreadIdentity(policy, method, params)
	localOneBot := clientID == oneBotClientID
	delete(params, "oneBot")
	delete(params, "wxId")
	delete(params, "groupId")
	delete(params, "groupRevision")
	if !ok {
		return nil, robotErr("无权使用 Codex")
	}
	if mode == "admin" && !a.robotAdmin(clientID, wxid) {
		return nil, robotErr("管理员权限未在 Agent 上绑定")
	}
	threadID, _ := params["threadId"].(string)
	backend := mode
	if method != "thread/start" {
		owned, err := a.ownThread(clientID, wxid, threadID)
		if err != nil {
			return nil, err
		}
		if owned.Tainted {
			return nil, robotErr(textOnlyUnavailable)
		}
		if (mode != owned.Backend || profile != owned.Profile) && (method == "turn/start" || method == "turn/steer" || method == "thread/resume") {
			return nil, robotErr("无权操作该会话")
		}
		backend = owned.Backend
	}
	target := main
	if backend == "text" {
		textApp, err := a.ensureTextApp(ctx)
		if err != nil {
			return nil, err
		}
		target = textApp
	}
	cwd, err := a.wxWorkspace(profile.Workspace)
	if err != nil {
		return nil, err
	}
	historyKey, _ := params["wxHistoryKey"].(string)
	delete(params, "wxHistoryKey")
	delete(params, "dynamicTools")
	if group != "" && !localOneBot && method == "thread/start" {
		params["dynamicTools"] = []any{wxHistoryTool()}
	}
	if localOneBot {
		err = constrainRobotParamsLimit(method, params, cwd, backend, 1+maxOneBotImages)
	} else {
		err = constrainRobotParams(method, params, cwd, backend, group != "" && historyKey != "")
	}
	if err != nil {
		return nil, err
	}
	groupTools := group != "" && backend == "tools" && !localOneBot
	if groupTools {
		if err := a.constrainGroupTools(method, clientID, wxid, cwd, params); err != nil {
			return nil, err
		}
	}
	var recordDir string
	if method == "turn/start" || method == "turn/steer" {
		withGroupHistory := method == "turn/start" && group != "" && historyKey != ""
		recordDir, err = a.applyWxRecord(clientID, wxid, backend, params, withGroupHistory)
		if err != nil {
			return nil, err
		}
	} else {
		delete(params, "wxRecord")
	}
	delete(params, "model")
	delete(params, "baseInstructions")
	delete(params, "developerInstructions")
	if method == "thread/start" || method == "thread/resume" || method == "turn/start" {
		if profile.Model != "" {
			params["model"] = profile.Model
		}
		if method == "turn/start" {
			params["cwd"] = cwd
		}
	}
	if method == "thread/start" && profile.Prompt != "" {
		params["developerInstructions"] = profile.Prompt
	}
	if method == "turn/steer" {
		params = map[string]any{
			"threadId": threadID, "expectedTurnId": params["expectedTurnId"], "input": params["input"],
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var raw json.RawMessage
	if method == "codexrm/thread/snapshot" {
		raw, err = a.threadSnapshot(callCtx, target, params)
	} else {
		if group != "" && method == "turn/start" {
			a.setWxHistorySession(target, threadID, historyKey)
		}
		pin := a.pinWxRecord(target, threadID, recordDir)
		raw, err = a.callThread(callCtx, target, clientID, method, params)
		if err != nil && group != "" && method == "turn/start" {
			a.setWxHistorySession(target, threadID, "")
		}
		a.bindWxRecordTurn(threadID, pin, raw)
	}
	if err != nil {
		return nil, err
	}
	if groupTools && (method == "thread/start" || method == "thread/resume") {
		if err := verifyWxGroupTools(raw); err != nil {
			return nil, err
		}
	}
	if method == "thread/start" {
		var started struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if json.Unmarshal(raw, &started) != nil || started.Thread.ID == "" {
			return nil, robotErr("thread start failed")
		}
		a.rememberWxThread(started.Thread.ID, clientID, wxid, backend, profile)
	}
	return raw, nil
}

func constrainRobotParams(method string, params map[string]any, cwd, mode string, history ...bool) error {
	maxItems := 0
	if method == "turn/start" && len(history) > 0 && history[0] {
		maxItems = 13
	}
	return constrainRobotParamsLimit(method, params, cwd, mode, maxItems)
}

func constrainRobotParamsLimit(method string, params map[string]any, cwd, mode string, inputLimit int) error {
	delete(params, "permissions")
	delete(params, "runtimeWorkspaceRoots")
	delete(params, "collaborationMode")
	switch method {
	case "thread/start", "thread/resume":
		params["cwd"] = cwd
		params["approvalPolicy"] = "never"
		sandbox := "read-only"
		switch mode {
		case "admin":
			sandbox = "danger-full-access"
		case "tools":
			sandbox = "workspace-write"
		}
		params["sandbox"] = sandbox
		config := map[string]any{
			"sandbox_mode": sandbox, "approval_policy": "never",
			"features": map[string]any{"default_mode_request_user_input": false},
		}
		if mode != "admin" {
			config["web_search"] = "disabled"
		}
		params["config"] = config
	case "turn/start", "turn/steer":
		maxItems := inputLimit
		if maxItems == 0 {
			maxItems = 2
		}
		input, err := robotInputWithLimit(params["input"], maxItems)
		if err != nil {
			return err
		}
		params["input"] = input
		if method == "turn/steer" {
			if turnID, _ := params["expectedTurnId"].(string); strings.TrimSpace(turnID) == "" {
				return robotErr("expectedTurnId is required")
			}
			return nil
		}
		params["approvalPolicy"] = "never"
		switch mode {
		case "admin":
			params["sandboxPolicy"] = map[string]any{"type": "dangerFullAccess"}
		case "text":
			params["sandboxPolicy"] = map[string]any{"type": "readOnly", "networkAccess": false}
		default:
			params["sandboxPolicy"] = map[string]any{"type": "workspaceWrite", "networkAccess": false, "writableRoots": []any{}, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}
		}
	}
	return nil
}

func robotInputWithLimit(value any, maxItems int) ([]any, error) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 || len(items) > maxItems {
		return nil, robotErr("参数错误")
	}
	input := make([]any, 0, len(items))
	for _, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			return nil, robotErr("参数错误")
		}
		switch row["type"] {
		case "text":
			text, ok := row["text"].(string)
			if !ok {
				return nil, robotErr("参数错误")
			}
			input = append(input, map[string]any{"type": "text", "text": text, "text_elements": []any{}})
		case "image":
			url, _ := row["url"].(string)
			if !validRobotImage(url) {
				return nil, robotErr("图片格式无效或超过 5 MiB")
			}
			input = append(input, map[string]any{"type": "image", "url": url})
		default:
			return nil, robotErr("不支持这类微信消息")
		}
	}
	return input, nil
}

func validRobotImage(url string) bool {
	const maxSize = 5 << 20
	header, encoded, ok := strings.Cut(url, ",")
	if !ok || len(encoded) > base64.StdEncoding.EncodedLen(maxSize) {
		return false
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > maxSize {
		return false
	}
	mime := http.DetectContentType(data)
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return header == "data:"+mime+";base64"
	default:
		return false
	}
}

func (a *Agent) rememberWxThread(id, clientID, wxid, backend string, profiles ...wxProfile) {
	a.wxMu.Lock()
	defer a.wxMu.Unlock()
	if a.wxThreads == nil {
		a.wxThreads = map[string]wxThread{}
	}
	var profile wxProfile
	if len(profiles) > 0 {
		profile = profiles[0]
	}
	a.wxThreads[id] = wxThread{ClientID: clientID, WxID: wxid, Backend: backend, Profile: profile}
}

func (a *Agent) ownThread(clientID, wxid, threadID string) (wxThread, error) {
	a.wxMu.Lock()
	defer a.wxMu.Unlock()
	thread, ok := a.wxThreads[threadID]
	if !ok || thread.ClientID != clientID || thread.WxID != wxid {
		return wxThread{}, robotErr("无权操作该会话")
	}
	return thread, nil
}

func (a *Agent) onTextNotification(raw json.RawMessage) {
	var note struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			Turn     struct {
				ID string `json:"id"`
			} `json:"turn"`
			Item struct {
				Type string `json:"type"`
			} `json:"item"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &note) != nil {
		return
	}
	if !textToolEvent(note.Method, note.Params.Item.Type) {
		a.wxMu.Lock()
		thread := a.wxThreads[note.Params.ThreadID]
		a.wxMu.Unlock()
		if thread.Backend == "task" {
			a.publishEvent(protocol.Envelope{Type: protocol.TypeServerMessage, ClientID: thread.ClientID, Message: raw})
			return
		}
		a.forward(raw)
		return
	}
	threadID := note.Params.ThreadID
	turnID := note.Params.TurnID
	if turnID == "" {
		turnID = note.Params.Turn.ID
	}
	a.wxMu.Lock()
	thread := a.wxThreads[threadID]
	wasTainted := thread.Tainted
	thread.Tainted = true
	if a.wxThreads == nil {
		a.wxThreads = map[string]wxThread{}
	}
	if threadID != "" {
		a.wxThreads[threadID] = thread
	}
	owner := thread.ClientID
	a.wxMu.Unlock()
	if wasTainted {
		return
	}
	a.invalidateOneBotThread(threadID)
	if owner != "" {
		a.publishEvent(protocol.Envelope{Type: protocol.TypeServerMessage, ClientID: owner, Message: mustJSON(map[string]any{
			"method": "codexrm/wx/textonly/blocked",
			"params": map[string]any{"threadId": threadID},
		})})
	}
	if threadID != "" && turnID != "" {
		// The app-server reader is running this callback and must remain
		// available to read the interrupt response.
		go a.interruptTextTurn(threadID, turnID)
	}
}

func (a *Agent) interruptTextTurn(threadID, turnID string) {
	a.textMu.Lock()
	app := a.textApp
	a.textMu.Unlock()
	if app == nil {
		return
	}
	callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = app.Call(callCtx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
}

func textToolEvent(method, itemType string) bool {
	switch itemType {
	case "exec", "functionCall", "toolCall", "customTool":
		return true
	}
	blob := method + " " + itemType
	for _, part := range []string{"commandExecution", "fileChange", "requestApproval", "browser", "computerUse", "mcpServer/tool", "tool/call"} {
		if strings.Contains(blob, part) {
			return true
		}
	}
	return false
}

func (a *Agent) robotNotificationClient(owner, threadID string) string {
	if owner != "" || threadID == "" {
		return ""
	}
	a.wxMu.Lock()
	defer a.wxMu.Unlock()
	thread, ok := a.wxThreads[threadID]
	if !ok {
		return ""
	}
	return thread.ClientID
}
