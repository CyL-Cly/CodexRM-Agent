package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
	"github.com/CyL-Cly/CodexRM-Agent/internal/onebot"
)

type oneBotSession struct {
	ThreadID  string    `json:"threadId"`
	Identity  string    `json:"identity"`
	Backend   string    `json:"backend"`
	Profile   wxProfile `json:"profile"`
	GroupID   string    `json:"groupId,omitempty"`
	Revision  string    `json:"groupRevision,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
	Loaded    bool      `json:"-"`
}

type oneBotConversation struct {
	key      string
	target   onebot.Target
	actor    string
	group    string
	revision string
	identity string
	mode     string
	profile  wxProfile
	admin    bool
}

func (c oneBotConversation) params() map[string]any {
	params := map[string]any{"wxId": c.actor}
	if c.group != "" {
		params["groupId"] = oneBotGroupKey(c.group)
		params["groupRevision"] = c.revision
	}
	return params
}

type oneBotTurn struct {
	ThreadID      string
	TurnID        string
	StopRequested bool
	Conversation  oneBotConversation
	Target        onebot.Target
	ReplyTo       int64
	Cancel        context.CancelFunc
	Order         []string
	Items         map[string]string
}

type oneBotReply struct {
	target  onebot.Target
	text    string
	replyTo int64
}

type oneBotRuntime struct {
	agent     *Agent
	app       *appserver.Client
	settings  OneBotSettings
	revisions map[string]string
	client    *onebot.Client
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	replies   chan oneBotReply
	statePath string

	mu       sync.Mutex
	saveMu   sync.Mutex
	workers  sync.WaitGroup
	slots    chan struct{}
	sessions map[string]*oneBotSession
	active   map[string]*oneBotTurn
}

func (a *Agent) startOneBot(parent context.Context, app *appserver.Client, settings OneBotSettings) (returnErr error) {
	a.oneBotOpMu.Lock()
	defer a.oneBotOpMu.Unlock()
	a.stopOneBotLocked()
	a.oneBotMu.Lock()
	a.oneBotErr = ""
	a.oneBotMu.Unlock()
	defer func() {
		if returnErr != nil {
			a.oneBotMu.Lock()
			a.oneBotErr = returnErr.Error()
			a.oneBotMu.Unlock()
		}
	}()
	if !settings.Enabled {
		return nil
	}
	if app == nil || parent == nil || parent.Err() != nil {
		return errors.New("OneBot 需要在线的 Codex app-server")
	}
	if err := settings.Normalize(); err != nil {
		return err
	}
	r := &oneBotRuntime{
		agent: a, app: app, settings: settings, revisions: map[string]string{}, done: make(chan struct{}), replies: make(chan oneBotReply, 32),
		statePath: oneBotSessionsPath(a.cfg.OneBotSettingsPath), slots: make(chan struct{}, 8), sessions: map[string]*oneBotSession{}, active: map[string]*oneBotTurn{},
	}
	r.ctx, r.cancel = context.WithCancel(parent)
	client, err := onebot.New(settings.transport(), r.handleEvent)
	if err != nil {
		r.cancel()
		return err
	}
	r.client = client
	policy, revisions := settings.robotPolicy()
	r.revisions = revisions
	a.wxMu.Lock()
	if a.wxPolicy == nil {
		a.wxPolicy = map[string]wxPolicy{}
	}
	a.wxPolicy[oneBotClientID] = policy
	a.wxMu.Unlock()
	if err := r.loadSessions(); err != nil {
		r.cancel()
		return fmt.Errorf("读取 OneBot 会话: %w", err)
	}
	a.oneBotMu.Lock()
	a.oneBot = r
	a.oneBotMu.Unlock()
	go r.deliverReplies()
	go func() {
		defer close(r.done)
		if err := client.Run(r.ctx); err != nil && r.ctx.Err() == nil {
			log.Printf("OneBot connection: %v", err)
		}
	}()
	log.Printf("OneBot 11 enabled: mode=%s listen=%s", settings.Mode, settings.Listen)
	return nil
}

func (a *Agent) stopOneBot() {
	a.oneBotOpMu.Lock()
	defer a.oneBotOpMu.Unlock()
	a.stopOneBotLocked()
}

func (a *Agent) stopOneBotLocked() {
	a.oneBotMu.RLock()
	r := a.oneBot
	a.oneBotMu.RUnlock()
	if r == nil {
		return
	}
	r.cancel()
	r.mu.Lock()
	active := make(map[string]*oneBotTurn, len(r.active))
	for key, turn := range r.active {
		active[key] = turn
	}
	r.mu.Unlock()
	for _, turn := range active {
		turn.Cancel()
	}
	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
		log.Print("OneBot shutdown timed out")
	}
	// Let a canceled turn/start settle before inspecting the thread. appserver
	// cancellation stops waiting for the RPC, not necessarily the remote turn.
	r.workers.Wait()
	interruptCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	var interrupts sync.WaitGroup
	for _, turn := range active {
		if turn.ThreadID == "" {
			continue
		}
		interrupts.Add(1)
		go func(turn *oneBotTurn) {
			defer interrupts.Done()
			if err := a.interruptOneBotTurn(interruptCtx, r, turn); err != nil && interruptCtx.Err() == nil {
				log.Printf("interrupt OneBot turn: %v", err)
			}
		}(turn)
	}
	interrupts.Wait()
	cancel()
	// A new runtime must never reuse a thread with late notifications from the
	// stopped generation. Idle threads remain persistent across reloads.
	r.mu.Lock()
	removed := false
	retired := map[string]string{}
	for key, turn := range active {
		if turn.ThreadID != "" {
			retired[key] = turn.ThreadID
			delete(r.sessions, key)
			removed = true
		}
	}
	r.mu.Unlock()
	if removed {
		a.wxMu.Lock()
		for _, threadID := range retired {
			delete(a.wxThreads, threadID)
		}
		a.wxMu.Unlock()
		r.saveSessions()
	}
	a.oneBotMu.Lock()
	if a.oneBot == r {
		a.oneBot = nil
	}
	a.oneBotMu.Unlock()
}

// interruptOneBotTurn uses the runtime's verified ownership, not the sender's
// interactive permission. Shared-group members may start a turn that only a
// group admin can stop manually; Agent shutdown must still stop that turn.
func (a *Agent) interruptOneBotTurn(ctx context.Context, r *oneBotRuntime, turn *oneBotTurn) error {
	r.mu.Lock()
	threadID, turnID := turn.ThreadID, turn.TurnID
	identity := turn.Conversation.identity
	r.mu.Unlock()
	if threadID == "" {
		return nil
	}
	owned, err := a.ownThread(oneBotClientID, identity, threadID)
	if err != nil {
		return err
	}
	target := r.app
	if owned.Backend == "text" {
		a.textMu.Lock()
		target = a.textApp
		a.textMu.Unlock()
	}
	if target == nil {
		return errors.New("Codex app-server 已关闭")
	}
	if turnID == "" {
		// The RPC response may have been canceled after Codex accepted the turn.
		// Resolve the active turn from the owned thread instead of dropping stop.
		raw, readErr := a.callThread(ctx, target, oneBotClientID, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true})
		if readErr != nil {
			return readErr
		}
		var snapshot struct {
			Thread struct {
				Turns []struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"turns"`
			} `json:"thread"`
		}
		if json.Unmarshal(raw, &snapshot) != nil {
			return errors.New("Codex 返回了无效的任务状态")
		}
		var errs []error
		for _, candidate := range snapshot.Thread.Turns {
			if candidate.ID == "" || (candidate.Status != "inProgress" && candidate.Status != "running") {
				continue
			}
			_, interruptErr := a.callThread(ctx, target, oneBotClientID, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": candidate.ID})
			errs = append(errs, interruptErr)
		}
		return errors.Join(errs...)
	}
	_, err = a.callThread(ctx, target, oneBotClientID, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
	return err
}

func (a *Agent) startConfiguredOneBot(ctx context.Context, app *appserver.Client) error {
	if a.cfg.OneBotSettingsPath == "" {
		return nil
	}
	settings, err := LoadOneBotSettings(a.cfg.OneBotSettingsPath)
	if err != nil {
		a.oneBotMu.Lock()
		a.oneBotErr = err.Error()
		a.oneBotMu.Unlock()
		return err
	}
	return a.startOneBot(ctx, app, settings)
}

func (a *Agent) ReloadOneBot(settings OneBotSettings) error {
	app := a.providerApp.Load()
	a.queueMu.Lock()
	ctx := a.queueCtx
	a.queueMu.Unlock()
	if app == nil || ctx == nil {
		err := errors.New("Codex 尚未就绪，配置将在下次连接时生效")
		a.oneBotMu.Lock()
		a.oneBotErr = err.Error()
		a.oneBotMu.Unlock()
		return err
	}
	return a.startOneBot(ctx, app, settings)
}

func (a *Agent) notifyOneBot(raw json.RawMessage) {
	a.oneBotMu.RLock()
	r := a.oneBot
	a.oneBotMu.RUnlock()
	if r != nil {
		r.handleNotification(raw)
	}
}

func (a *Agent) invalidateOneBotThread(threadID string) {
	if threadID == "" {
		return
	}
	a.oneBotMu.RLock()
	r := a.oneBot
	a.oneBotMu.RUnlock()
	if r == nil {
		return
	}
	removed := false
	r.mu.Lock()
	for key, session := range r.sessions {
		if session.ThreadID == threadID {
			delete(r.sessions, key)
			removed = true
		}
	}
	r.mu.Unlock()
	if removed {
		r.saveSessions()
	}
}

func (a *Agent) robotAdmin(clientID, wxid string) bool {
	if clientID != oneBotClientID {
		return a.wxAdmin(clientID, wxid)
	}
	a.oneBotMu.RLock()
	r := a.oneBot
	a.oneBotMu.RUnlock()
	if r == nil {
		return false
	}
	access, ok := r.settings.Users[wxid]
	return ok && access.Enabled && access.Mode == "admin"
}

func (a *Agent) OneBotStatus() map[string]any {
	a.oneBotMu.RLock()
	r := a.oneBot
	startupError := a.oneBotErr
	a.oneBotMu.RUnlock()
	if r == nil {
		if startupError != "" {
			return map[string]any{"enabled": false, "state": "retrying", "error": startupError}
		}
		return map[string]any{"enabled": false, "state": "disabled"}
	}
	r.mu.Lock()
	sessions, active := len(r.sessions), len(r.active)
	r.mu.Unlock()
	return map[string]any{"transport": r.client.Status(), "enabled": true, "sessions": sessions, "activeTurns": active}
}

func (a *Agent) SendOneBotTest(ctx context.Context, target onebot.Target, text string) error {
	a.oneBotMu.RLock()
	r := a.oneBot
	a.oneBotMu.RUnlock()
	if r == nil || !r.settings.Enabled {
		return errors.New("OneBot 尚未启用或 Codex 尚未就绪")
	}
	if (target.UserID <= 0) == (target.GroupID <= 0) {
		return errors.New("测试消息必须且只能指定一个私聊 QQ 号或群号")
	}
	return r.sendText(ctx, target, text, 0)
}

func (r *oneBotRuntime) loadSessions() error {
	raw, err := os.ReadFile(r.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(raw) > 4<<20 {
		return errors.New("会话文件过大")
	}
	var saved map[string]*oneBotSession
	if err := json.Unmarshal(raw, &saved); err != nil {
		return err
	}
	if len(saved) > 2000 {
		return errors.New("会话数量超过限制")
	}
	for key, session := range saved {
		if session == nil || session.ThreadID == "" || session.Identity == "" || len(key) > 160 || len(session.ThreadID) > 160 {
			continue
		}
		if session.Backend != "text" && session.Backend != "tools" && session.Backend != "admin" {
			continue
		}
		session.Loaded = false
		r.sessions[key] = session
		r.agent.rememberWxThread(session.ThreadID, oneBotClientID, session.Identity, session.Backend, session.Profile)
	}
	return nil
}

func (r *oneBotRuntime) saveSessions() {
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	r.mu.Lock()
	saved := make(map[string]oneBotSession, len(r.sessions))
	for key, session := range r.sessions {
		saved[key] = *session
	}
	r.mu.Unlock()
	if err := clientconfig.WriteJSON(r.statePath, saved); err != nil {
		log.Printf("save OneBot sessions: %v", err)
	}
}

func (r *oneBotRuntime) handleEvent(ctx context.Context, event onebot.Event) {
	if event.PostType == "request" {
		r.handleRequest(ctx, event)
		return
	}
	if event.PostType != "message" || event.UserID <= 0 || event.SelfID == event.UserID {
		return
	}
	message := event.Message
	if len(message) == 0 && event.RawMessage != "" {
		message = onebot.DecodeCQ(event.RawMessage)
	}
	conversation, message, ok := r.conversation(event, message)
	if !ok {
		return
	}
	select {
	case r.slots <- struct{}{}:
		r.workers.Add(1)
		go func() {
			defer r.workers.Done()
			defer func() { <-r.slots }()
			r.handleMessage(conversation, event, message)
		}()
	default:
		log.Printf("OneBot message worker limit reached; dropping message %d", event.MessageID)
	}
}

func (r *oneBotRuntime) handleRequest(ctx context.Context, event onebot.Event) {
	if event.Flag == "" || len(event.Flag) > 4096 || strings.ContainsRune(event.Flag, 0) {
		return
	}
	var action string
	params := map[string]any{"flag": event.Flag, "approve": true}
	switch event.RequestType {
	case "friend":
		access, ok := r.settings.Users[strconv.FormatInt(event.UserID, 10)]
		if !r.settings.AutoAcceptFriend || !ok || !access.Enabled {
			return
		}
		action = "set_friend_add_request"
	case "group":
		group, ok := r.settings.Groups[strconv.FormatInt(event.GroupID, 10)]
		if !r.settings.AutoAcceptGroup || event.SubType != "invite" || !ok || !group.Enabled {
			return
		}
		action = "set_group_add_request"
		params["sub_type"] = event.SubType
	default:
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := r.client.Call(callCtx, action, params); err != nil {
		log.Printf("OneBot %s: %v", action, err)
	}
}

func (r *oneBotRuntime) conversation(event onebot.Event, message onebot.Message) (oneBotConversation, onebot.Message, bool) {
	actor := strconv.FormatInt(event.UserID, 10)
	c := oneBotConversation{target: onebot.Target{UserID: event.UserID}, actor: actor}
	switch event.MessageType {
	case "private":
		if event.GroupID != 0 {
			return c, message, false
		}
		access, ok := r.settings.Users[actor]
		if !ok || !access.Enabled {
			return c, message, false
		}
		c.key = "p:" + actor
	case "group":
		if event.GroupID <= 0 {
			return c, message, false
		}
		groupID := strconv.FormatInt(event.GroupID, 10)
		group, ok := r.settings.Groups[groupID]
		if !ok || !group.Enabled || (len(group.Members) > 0 && !slices.Contains(group.Members, actor)) {
			return c, message, false
		}
		selfID := event.SelfID
		if selfID == 0 {
			selfID = r.settings.SelfID
		}
		if group.Trigger == "mention" {
			if selfID <= 0 || !message.At(selfID) {
				return c, message, false
			}
			message = message.WithoutAt(selfID)
		}
		c.group = groupID
		c.revision = r.revisions[groupID]
		c.target = onebot.Target{GroupID: event.GroupID}
		c.admin = slices.Contains(group.Admins, actor)
		if group.Session == "shared" {
			c.key = "g:" + groupID
		} else {
			c.key = "g:" + groupID + ":u:" + actor
		}
	default:
		return c, message, false
	}
	params := c.params()
	identity, mode, allowed := wxThreadIdentity(r.agent.currentPolicy(oneBotClientID), "turn/start", params)
	if !allowed {
		return c, message, false
	}
	c.identity, c.mode = identity, mode
	if c.group == "" {
		access := r.settings.Users[actor]
		c.profile = wxProfile{Workspace: access.Workspace, Model: access.Model, Prompt: access.Prompt}
		c.admin = access.Mode == "admin"
	} else {
		group := r.settings.Groups[c.group]
		c.profile = wxProfile{Workspace: group.Workspace, Model: group.Model, Prompt: group.Prompt}
	}
	return c, message, true
}

func (r *oneBotRuntime) handleMessage(c oneBotConversation, event onebot.Event, message onebot.Message) {
	commandText := strings.TrimSpace(message.Text())
	if command, args, ok := oneBotCommand(commandText); ok {
		r.handleCommand(c, event, command, args)
		return
	}
	if strings.HasPrefix(commandText, "/") {
		_ = r.sendText(r.ctx, c.target, "未知命令。发送 /help 查看可用命令。", event.MessageID)
		return
	}
	if strings.TrimSpace(oneBotReadableText(message)) == "" && len(message.Images()) == 0 {
		_ = r.sendText(r.ctx, c.target, "请发送文字或图片；发送 /help 查看会话命令。", event.MessageID)
		return
	}
	r.startTurn(c, event, message)
}

func oneBotCommand(text string) (string, string, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", "", false
	}
	command := strings.ToLower(fields[0])
	aliases := map[string]string{
		"/help": "help", "#帮助": "help", "/new": "new", "/reset": "new", "#新会话": "new", "#重置": "new",
		"/stop": "stop", "/interrupt": "stop", "#停止": "stop", "/status": "status", "#状态": "status", "/id": "id", "/whoami": "id",
	}
	name, ok := aliases[command]
	if !ok {
		return "", "", false
	}
	return name, strings.TrimSpace(strings.TrimPrefix(text, fields[0])), true
}

func (r *oneBotRuntime) handleCommand(c oneBotConversation, event onebot.Event, command, args string) {
	if c.group != "" && r.settings.Groups[c.group].Session == "shared" && (command == "new" || command == "stop") && !c.admin {
		_ = r.sendText(r.ctx, c.target, "只有群会话管理员可以停止或重置共享会话。", event.MessageID)
		return
	}
	switch command {
	case "help":
		_ = r.sendText(r.ctx, c.target, "CodexRM OneBot 11\n直接发送文字或图片开始对话。\n/new 或 /reset：新建会话\n/stop：停止当前任务\n/status：查看会话与任务状态\n/id：查看 QQ、群和会话标识\n群聊默认只响应 @机器人；共享群会话的停止和重置仅管理员可用。", event.MessageID)
	case "id":
		_ = r.sendText(r.ctx, c.target, fmt.Sprintf("QQ：%s\n会话：%s\n权限：%s", c.actor, c.key, c.mode), event.MessageID)
	case "status":
		r.mu.Lock()
		thread, turn := "尚未创建", "无运行中任务"
		if session := r.sessions[c.key]; session != nil {
			thread = session.ThreadID
		}
		if active := r.active[c.key]; active != nil {
			turn = active.TurnID
			if turn == "" {
				turn = "正在启动"
			}
		}
		r.mu.Unlock()
		workspace := c.profile.Workspace
		if workspace == "" {
			workspace = r.agent.cfg.Cwd
		}
		_ = r.sendText(r.ctx, c.target, fmt.Sprintf("会话：%s\n任务：%s\n模式：%s\n工作区：%s", thread, turn, c.mode, workspace), event.MessageID)
	case "new":
		r.mu.Lock()
		if r.active[c.key] != nil {
			r.mu.Unlock()
			_ = r.sendText(r.ctx, c.target, "任务仍在运行，请先发送 /stop，再新建会话。", event.MessageID)
			return
		}
		session := r.sessions[c.key]
		delete(r.sessions, c.key)
		r.mu.Unlock()
		if session != nil {
			r.agent.wxMu.Lock()
			delete(r.agent.wxThreads, session.ThreadID)
			r.agent.wxMu.Unlock()
		}
		r.saveSessions()
		_ = r.sendText(r.ctx, c.target, "已清除当前 OneBot 会话绑定；下一条消息将创建新 Codex 会话。", event.MessageID)
	case "stop":
		r.stopTurn(c, event.MessageID)
	}
}

func (r *oneBotRuntime) startTurn(c oneBotConversation, event onebot.Event, message onebot.Message) {
	r.mu.Lock()
	if r.active[c.key] != nil {
		r.mu.Unlock()
		_ = r.sendText(r.ctx, c.target, "当前会话已有任务在运行；请等待完成，或发送 /stop。", event.MessageID)
		return
	}
	ctx, cancel := context.WithCancel(r.ctx)
	turn := &oneBotTurn{Conversation: c, Target: c.target, ReplyTo: event.MessageID, Cancel: cancel, Items: map[string]string{}}
	r.active[c.key] = turn
	r.mu.Unlock()
	input, err := r.messageInput(ctx, event, message)
	if err != nil {
		r.failTurn(c.key, turn, err)
		return
	}
	params := c.params()
	params["input"] = input
	r.mu.Lock()
	session := r.sessions[c.key]
	r.mu.Unlock()
	if session != nil && (session.Identity != c.identity || session.Backend != c.mode || session.Profile != c.profile) {
		r.agent.wxMu.Lock()
		delete(r.agent.wxThreads, session.ThreadID)
		r.agent.wxMu.Unlock()
		r.mu.Lock()
		delete(r.sessions, c.key)
		r.mu.Unlock()
		session = nil
		r.saveSessions()
	}
	if session == nil {
		raw, callErr := r.agent.robotThread(ctx, r.app, oneBotClientID, "thread/start", copyOneBotParams(params))
		if callErr != nil {
			r.failTurn(c.key, turn, callErr)
			return
		}
		var started struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if json.Unmarshal(raw, &started) != nil || started.Thread.ID == "" {
			r.failTurn(c.key, turn, errors.New("Codex 未返回会话 ID"))
			return
		}
		session = &oneBotSession{ThreadID: started.Thread.ID, Identity: c.identity, Backend: c.mode, Profile: c.profile, GroupID: c.group, Revision: c.revision, Loaded: true, UpdatedAt: time.Now().UTC()}
		r.mu.Lock()
		r.sessions[c.key] = session
		if r.active[c.key] == turn {
			turn.ThreadID = session.ThreadID
		}
		r.mu.Unlock()
		r.saveSessions()
	} else if !session.Loaded {
		resume := c.params()
		resume["threadId"] = session.ThreadID
		if _, callErr := r.agent.robotThread(ctx, r.app, oneBotClientID, "thread/resume", resume); callErr != nil {
			r.failTurn(c.key, turn, callErr)
			return
		}
		r.mu.Lock()
		session.Loaded = true
		if r.active[c.key] == turn {
			turn.ThreadID = session.ThreadID
		}
		r.mu.Unlock()
	}
	params["threadId"] = session.ThreadID
	r.mu.Lock()
	current := r.active[c.key] == turn
	if current {
		turn.ThreadID = session.ThreadID
	}
	stopRequested := current && turn.StopRequested
	r.mu.Unlock()
	if !current {
		return
	}
	if stopRequested {
		r.finishPendingStop(c.key, turn)
		return
	}
	raw, err := r.agent.robotThread(ctx, r.app, oneBotClientID, "turn/start", params)
	if err != nil {
		r.failTurn(c.key, turn, err)
		return
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(raw, &started) == nil && started.Turn.ID != "" {
		r.mu.Lock()
		if r.active[c.key] == turn {
			turn.TurnID = started.Turn.ID
		}
		stopRequested = turn.StopRequested
		r.mu.Unlock()
		if stopRequested {
			r.interruptRequestedTurn(turn)
		}
	}
}

func (r *oneBotRuntime) finishPendingStop(key string, turn *oneBotTurn) {
	r.mu.Lock()
	current := r.active[key] == turn
	if current {
		delete(r.active, key)
	}
	r.mu.Unlock()
	turn.Cancel()
	if current && r.ctx.Err() == nil {
		_ = r.sendText(r.ctx, turn.Target, "任务已在提交前停止。", turn.ReplyTo)
	}
}

func copyOneBotParams(params map[string]any) map[string]any {
	copy := make(map[string]any, len(params))
	for key, value := range params {
		copy[key] = value
	}
	delete(copy, "input")
	delete(copy, "threadId")
	return copy
}

func (r *oneBotRuntime) stopTurn(c oneBotConversation, replyTo int64) {
	r.mu.Lock()
	turn := r.active[c.key]
	if turn != nil {
		turn.StopRequested = true
	}
	started := turn != nil && turn.ThreadID != ""
	r.mu.Unlock()
	if turn == nil {
		_ = r.sendText(r.ctx, c.target, "当前会话没有运行中的任务。", replyTo)
		return
	}
	if started {
		ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
		err := r.agent.interruptOneBotTurn(ctx, r, turn)
		cancel()
		if err != nil {
			_ = r.sendText(r.ctx, c.target, "停止失败："+oneBotError(err), replyTo)
			return
		}
	}
	// If thread/start is still pending, startTurn observes StopRequested before
	// submitting a turn. If turn/start is in flight, its response/notification
	// supplies the ID and issues the delayed interrupt.
	_ = r.sendText(r.ctx, c.target, "已请求停止当前任务。", replyTo)
}

func (r *oneBotRuntime) interruptRequestedTurn(turn *oneBotTurn) {
	go func() {
		ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
		defer cancel()
		if err := r.agent.interruptOneBotTurn(ctx, r, turn); err != nil && ctx.Err() == nil {
			log.Printf("delayed OneBot interrupt: %v", err)
		}
	}()
}

func (r *oneBotRuntime) failTurn(key string, turn *oneBotTurn, err error) {
	r.mu.Lock()
	current := r.active[key] == turn
	stopRequested := current && turn.StopRequested && turn.ThreadID != ""
	if current {
		delete(r.active, key)
	}
	r.mu.Unlock()
	turn.Cancel()
	if stopRequested && r.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if interruptErr := r.agent.interruptOneBotTurn(ctx, r, turn); interruptErr != nil {
			log.Printf("interrupt failed OneBot startup: %v", interruptErr)
		}
		cancel()
	}
	if current && r.ctx.Err() == nil {
		_ = r.sendText(r.ctx, turn.Target, "Codex 处理失败："+oneBotError(err), turn.ReplyTo)
	}
}

func oneBotError(err error) string {
	if err == nil {
		return "未知错误"
	}
	text := strings.TrimSpace(err.Error())
	runes := []rune(text)
	if len(runes) > 500 {
		text = string(runes[:500]) + "…"
	}
	return text
}

func (r *oneBotRuntime) handleNotification(raw json.RawMessage) {
	var note struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
			Item     struct {
				ID   string `json:"id"`
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Turn struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &note) != nil {
		return
	}
	r.mu.Lock()
	var key string
	var turn *oneBotTurn
	for candidate, active := range r.active {
		if active.ThreadID == note.Params.ThreadID {
			key, turn = candidate, active
			break
		}
	}
	if turn == nil {
		r.mu.Unlock()
		return
	}
	turnID := note.Params.TurnID
	if turnID == "" {
		turnID = note.Params.Turn.ID
	}
	if turn.TurnID == "" {
		// Do not let a late completion from an earlier generation finish a new
		// turn/start before its own ID is known.
		if note.Method != "turn/started" || turnID == "" {
			r.mu.Unlock()
			return
		}
		turn.TurnID = turnID
	} else if (turnID != "" && turn.TurnID != turnID) ||
		(note.Method == "turn/completed" && turnID == "") {
		// Some item-level events omit turnId; only reject an explicit mismatch.
		// A completion without an ID is ambiguous and must not finish a turn.
		r.mu.Unlock()
		return
	}
	switch note.Method {
	case "turn/started":
		stopRequested := turn.StopRequested
		r.mu.Unlock()
		if stopRequested {
			r.interruptRequestedTurn(turn)
		}
		return
	case "item/started":
		if note.Params.Item.Type == "agentMessage" && note.Params.Item.ID != "" && !slices.Contains(turn.Order, note.Params.Item.ID) {
			turn.Order = append(turn.Order, note.Params.Item.ID)
		}
	case "item/agentMessage/delta":
		id := note.Params.ItemID
		if id == "" {
			id = note.Params.Item.ID
		}
		if id != "" {
			if !slices.Contains(turn.Order, id) {
				turn.Order = append(turn.Order, id)
			}
			turn.Items[id] += note.Params.Delta
		}
	case "item/completed":
		if note.Params.Item.Type == "agentMessage" && note.Params.Item.ID != "" {
			id := note.Params.Item.ID
			if !slices.Contains(turn.Order, id) {
				turn.Order = append(turn.Order, id)
			}
			turn.Items[id] = note.Params.Item.Text
		}
	case "turn/completed":
		delete(r.active, key)
		parts := make([]string, 0, len(turn.Order))
		for _, id := range turn.Order {
			if text := strings.TrimSpace(turn.Items[id]); text != "" {
				parts = append(parts, text)
			}
		}
		text := strings.Join(parts, "\n\n")
		status := note.Params.Turn.Status
		if text == "" {
			switch status {
			case "interrupted":
				text = "任务已停止。"
			case "failed":
				text = "Codex 任务失败。"
			default:
				text = "任务已完成，但没有文本回复。"
			}
		}
		if note.Params.Turn.Error != nil && note.Params.Turn.Error.Message != "" && status != "completed" {
			text += "\n\n错误：" + note.Params.Turn.Error.Message
		}
		target, replyTo := turn.Target, turn.ReplyTo
		turn.Cancel()
		r.mu.Unlock()
		select {
		case r.replies <- oneBotReply{target, text, replyTo}:
		default:
			log.Print("OneBot reply queue full; reply could not be sent")
		}
		return
	default:
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
}

func (r *oneBotRuntime) deliverReplies() {
	for r.ctx.Err() == nil {
		select {
		case <-r.ctx.Done():
			return
		case reply := <-r.replies:
			ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
			err := r.sendText(ctx, reply.target, reply.text, reply.replyTo)
			cancel()
			if err != nil && r.ctx.Err() == nil {
				log.Printf("OneBot reply delivery: %v", err)
			}
		}
	}
}

func (r *oneBotRuntime) sendText(ctx context.Context, target onebot.Target, text string, replyTo int64) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	limit := r.settings.MessageLimit
	if limit == 0 {
		limit = defaultOneBotMessageLen
	}
	chunks := splitOneBotText(text, limit)
	send := func(message onebot.Message) error {
		callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		_, err := r.client.Send(callCtx, target, message)
		return err
	}
	for index, chunk := range chunks {
		message := onebot.Message{{Type: "text", Data: map[string]any{"text": chunk}}}
		if index == 0 && target.GroupID > 0 && replyTo > 0 {
			message = append(onebot.Message{{Type: "reply", Data: map[string]any{"id": strconv.FormatInt(replyTo, 10)}}}, message...)
		}
		if err := send(message); err != nil {
			if index == 0 && target.GroupID > 0 && replyTo > 0 {
				if fallback := send(onebot.Message{{Type: "text", Data: map[string]any{"text": chunk}}}); fallback == nil {
					continue
				}
			}
			return err
		}
	}
	return nil
}

func splitOneBotText(text string, limit int) []string {
	if limit <= 0 {
		limit = defaultOneBotMessageLen
	}
	runes := []rune(text)
	chunks := []string{}
	for len(runes) > 0 {
		if len(runes) <= limit {
			if chunk := strings.TrimSpace(string(runes)); chunk != "" {
				chunks = append(chunks, chunk)
			}
			break
		}
		cut := limit
		for index := limit - 1; index >= limit/2; index-- {
			if runes[index] == '\n' {
				cut = index
				break
			}
		}
		if cut == 0 {
			cut = limit
		}
		if chunk := strings.TrimSpace(string(runes[:cut])); chunk != "" {
			chunks = append(chunks, chunk)
		}
		runes = runes[cut:]
		for len(runes) > 0 && runes[0] == '\n' {
			runes = runes[1:]
		}
	}
	return chunks
}
