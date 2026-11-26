package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

const textOnlyUnavailable = "仅文本模式不可用"

type wxAccess struct {
	wxProfile
	Enabled  bool     `json:"enabled"`
	Mode     string   `json:"mode"`
	Commands []string `json:"commands"`
}

type wxPolicy struct {
	Workspace string                   `json:"workspace,omitempty"`
	Enabled   []string                 `json:"enabled"`
	Access    map[string]wxAccess      `json:"access"`
	Groups    map[string]wxGroupPolicy `json:"groups,omitempty"`
	Summaries map[string]wxProfile     `json:"summaries,omitempty"`
	Moments   bool                     `json:"moments,omitempty"`
	Memory    bool                     `json:"memory,omitempty"`
}

type wxThread struct {
	ClientID string
	WxID     string
	Backend  string
	Profile  wxProfile
	Tainted  bool
	History  wxHistorySession
}

type wxCommand struct {
	Name       string
	Help       string
	Argv0      string
	WorkDir    string
	MaxArgs    int
	TimeoutSec int
	MaxBytes   int
	Builtin    bool
}

func (a *Agent) closeTextApp() {
	a.textMu.Lock()
	defer a.textMu.Unlock()
	if a.textApp != nil {
		_ = a.textApp.Close()
		a.textApp = nil
	}
	a.textVerified = false
}

func (a *Agent) handleRobot(ctx context.Context, app *appserver.Client, send sendEnvelope, env protocol.Envelope, method string, params map[string]any) {
	if !protocol.RobotMethodAllowed(method) {
		a.respond(send, env, nil, robotErr("method not allowed"))
		return
	}
	switch method {
	case "codexrm/wx/task/start":
		result, err := a.wxTaskStart(ctx, env.ClientID, params)
		a.respond(send, env, mustJSON(result), err)
	case "codexrm/wx/task/close":
		a.respond(send, env, mustJSON(map[string]bool{"ok": true}), a.wxTaskClose(env.ClientID, params))
	case "codexrm/wx/history/result":
		a.respond(send, env, mustJSON(map[string]any{"ok": true}), a.acceptWxHistoryResult(env.ClientID, params))
	case "codexrm/wx/policy/set":
		a.respond(send, env, mustJSON(map[string]any{"ok": true}), a.setWxPolicy(env.ClientID, params))
	case "codexrm/wx/commands/list":
		result, err := a.listWxCommands()
		a.respond(send, env, mustJSON(result), err)
	case "codexrm/wx/exec":
		result, err := a.execWxCommand(ctx, env.ClientID, params)
		a.respond(send, env, mustJSON(result), err)
	case "codexrm/wx/textonly/status":
		available, message := a.textOnlyStatus(ctx)
		a.respond(send, env, mustJSON(map[string]any{"available": available, "cwd": a.cfg.Cwd, "message": message}), nil)
	default:
		result, err := a.robotThread(ctx, app, env.ClientID, method, params)
		a.respond(send, env, result, err)
	}
}

func robotErr(message string) error {
	return &appserver.RPCError{Code: -32602, Message: message}
}

func (a *Agent) setWxPolicy(clientID string, params map[string]any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return robotErr("invalid policy")
	}
	var policy wxPolicy
	if json.Unmarshal(raw, &policy) != nil {
		return robotErr("invalid policy")
	}
	if strings.TrimSpace(policy.Workspace) != "" && filepath.Clean(policy.Workspace) != filepath.Clean(a.cfg.Cwd) {
		return robotErr("workspace is not allowed")
	}
	policy.Workspace = a.cfg.Cwd
	if policy.Access == nil {
		policy.Access = map[string]wxAccess{}
	}
	if len(policy.Access) > 200 || len(policy.Enabled) > 100 || len(policy.Groups) > 200 || len(policy.Summaries) > 2000 {
		return robotErr("policy is too large")
	}
	for id, group := range policy.Groups {
		if !validWxGroup(id, group) {
			return robotErr("invalid group settings")
		}
	}
	for id, profile := range policy.Summaries {
		name, ok := strings.CutSuffix(id, "@chatroom")
		if !ok || len(id) > 64 || !validWxActor(name) || !validWxProfile(profile) {
			return robotErr("invalid summary settings")
		}
	}
	known := map[string]bool{}
	for _, command := range a.wxCommands() {
		known[command.Name] = true
	}
	for _, name := range policy.Enabled {
		if !known[name] || !validCommandName(name) {
			return robotErr("unknown command")
		}
	}
	for wxid, access := range policy.Access {
		if !validWxActor(wxid) || strings.HasPrefix(wxid, "g:") {
			return robotErr("invalid wxid")
		}
		if !validWxProfile(access.wxProfile) {
			return robotErr("invalid profile")
		}
		if access.Mode != "text" && access.Mode != "tools" && access.Mode != "admin" {
			return robotErr("invalid codex mode")
		}
		for _, name := range access.Commands {
			if !known[name] {
				return robotErr("unknown command")
			}
		}
		policy.Access[wxid] = access
	}
	a.wxMu.Lock()
	if a.wxPolicy == nil {
		a.wxPolicy = map[string]wxPolicy{}
	}
	a.wxPolicy[clientID] = policy
	a.wxMu.Unlock()
	return nil
}

func (a *Agent) currentPolicy(clientID string) wxPolicy {
	a.wxMu.Lock()
	defer a.wxMu.Unlock()
	if a.wxPolicy == nil {
		return wxPolicy{Workspace: a.cfg.Cwd}
	}
	policy, ok := a.wxPolicy[clientID]
	if !ok {
		return wxPolicy{Workspace: a.cfg.Cwd}
	}
	return policy
}

// Full host access requires a local binding between the robot pairing and the
// sender. A robot policy update alone must not grant it.
func (a *Agent) wxAdmin(clientID, wxid string) bool {
	raw, err := os.ReadFile(filepath.Join(a.cfg.DataDir, "wx-admins.json"))
	if err != nil {
		return false
	}
	var admins map[string][]string
	if json.Unmarshal(raw, &admins) != nil {
		return false
	}
	for _, permitted := range admins[clientID] {
		if wxid == permitted {
			return true
		}
	}
	return false
}

func accessMode(policy wxPolicy, wxid string) (string, bool) {
	access, ok := policy.Access[wxid]
	if !ok || !access.Enabled {
		return "", false
	}
	if access.Mode == "tools" || access.Mode == "admin" {
		return access.Mode, true
	}
	return "text", true
}

func commandGranted(policy wxPolicy, wxid, name string) bool {
	access, ok := policy.Access[wxid]
	if !ok || !access.Enabled {
		return false
	}
	enabled := false
	for _, item := range policy.Enabled {
		if item == name {
			enabled = true
		}
	}
	if !enabled {
		return false
	}
	for _, item := range access.Commands {
		if item == name {
			return true
		}
	}
	return false
}

func (a *Agent) listWxCommands() (map[string]any, error) {
	items := []map[string]any{}
	for _, command := range a.wxCommands() {
		kind := "local"
		if command.Builtin {
			kind = "builtin"
		}
		items = append(items, map[string]any{
			"name": command.Name, "help": command.Help, "source": kind,
			"maxArgs": command.MaxArgs, "timeoutSec": command.TimeoutSec, "maxBytes": command.MaxBytes,
		})
	}
	return map[string]any{"cwd": a.cfg.Cwd, "commands": items}, nil
}

func (a *Agent) wxCommands() []wxCommand {
	commands := []wxCommand{{
		Name: "codexrm-echo", Help: "返回已校验的参数，不执行外部程序", MaxArgs: 8, TimeoutSec: 5, MaxBytes: 1000, Builtin: true,
	}}
	path := filepath.Join(a.cfg.DataDir, "wx-commands.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return commands
	}
	var file struct {
		Commands []wxCommand `json:"commands"`
	}
	if json.Unmarshal(raw, &file) != nil {
		return commands
	}
	for _, command := range file.Commands {
		if command.Name == "codexrm-echo" || !validCommandName(command.Name) || !filepath.IsAbs(command.Argv0) || !filepath.IsAbs(command.WorkDir) {
			continue
		}
		if blockedExecutable(command.Argv0) || !withinDir(a.cfg.Cwd, command.WorkDir) {
			continue
		}
		if command.MaxArgs <= 0 || command.MaxArgs > 16 {
			command.MaxArgs = 4
		}
		if command.TimeoutSec <= 0 || command.TimeoutSec > 60 {
			command.TimeoutSec = 15
		}
		if command.MaxBytes <= 0 || command.MaxBytes > 16000 {
			command.MaxBytes = 4000
		}
		commands = append(commands, command)
	}
	return commands
}

func validCommandName(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	for i, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-'
		if i == 0 && (r < 'a' || r > 'z') {
			return false
		}
		if !ok {
			return false
		}
	}
	return true
}

func blockedExecutable(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	switch base {
	case "cmd", "cmd.exe", "powershell", "powershell.exe", "pwsh", "pwsh.exe", "bash", "bash.exe", "sh", "sh.exe", "wsl", "wsl.exe":
		return true
	default:
		return false
	}
}

func withinDir(root, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (a *Agent) findWxCommand(name string) (wxCommand, bool) {
	for _, command := range a.wxCommands() {
		if command.Name == name {
			return command, true
		}
	}
	return wxCommand{}, false
}

func (a *Agent) execWxCommand(ctx context.Context, clientID string, params map[string]any) (map[string]any, error) {
	wxid, _ := params["wxId"].(string)
	name, _ := params["command"].(string)
	args, err := stringArgs(params["args"])
	if err != nil {
		return nil, robotErr("参数错误")
	}
	if !commandGranted(a.currentPolicy(clientID), wxid, name) {
		return nil, robotErr("无权调用该命令")
	}
	command, ok := a.findWxCommand(name)
	if !ok {
		return nil, robotErr("命令不可用")
	}
	if len(args) > command.MaxArgs {
		return nil, robotErr("参数错误")
	}
	for _, arg := range args {
		if len(arg) > 200 || strings.ContainsAny(arg, "\r\n\t`") {
			return nil, robotErr("参数错误")
		}
		for _, r := range arg {
			if r < 32 {
				return nil, robotErr("参数错误")
			}
		}
	}
	if command.Builtin {
		text := strings.Join(args, " ")
		if text == "" {
			text = "ok"
		}
		return map[string]any{"text": text}, nil
	}
	timeout := time.Duration(command.TimeoutSec) * time.Second
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := appserver.RunBoundedCommand(callCtx, command.Argv0, args, command.WorkDir, minimalExecEnv(), command.MaxBytes)
	if errors.Is(err, appserver.ErrCommandOutputLimit) {
		return nil, robotErr("命令输出超限")
	}
	if callCtx.Err() == context.DeadlineExceeded {
		return nil, robotErr("命令执行超时")
	}
	if err != nil {
		return nil, robotErr("命令执行失败")
	}
	return map[string]any{"text": strings.TrimSpace(string(out))}, nil
}

func minimalExecEnv() []string {
	env := []string{}
	for _, key := range []string{"PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func stringArgs(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("args")
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, errors.New("args")
		}
		out = append(out, text)
	}
	return out, nil
}
