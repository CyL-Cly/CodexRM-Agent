package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type wxProfile struct {
	Workspace string `json:"workspace"`
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
}

type wxGroupPolicy struct {
	wxProfile
	Session  string   `json:"session"`
	Mode     string   `json:"mode"`
	Members  []string `json:"members"`
	Admins   []string `json:"admins"`
	Revision string   `json:"revision"`
}

func validWxGroup(id string, group wxGroupPolicy) bool {
	name, ok := strings.CutSuffix(id, "@chatroom")
	if !ok || !validWxProfile(group.wxProfile) || !validWxActor(name) || len(id) > 64 || (group.Session != "shared" && group.Session != "member") || (group.Mode != "text" && group.Mode != "tools") || len(group.Revision) != 32 || len(group.Members) > 500 || len(group.Admins) > 100 {
		return false
	}
	if _, err := hex.DecodeString(group.Revision); err != nil {
		return false
	}
	for _, members := range [][]string{group.Members, group.Admins} {
		for _, actor := range members {
			if !validWxActor(actor) {
				return false
			}
		}
	}
	return true
}

func validWxActor(id string) bool {
	return id != "" && len(id) <= 64 && !strings.ContainsAny(id, "@| \r\n\t")
}

func wxThreadIdentity(policy wxPolicy, method string, params map[string]any) (identity, mode string, allowed bool) {
	actor, _ := params["wxId"].(string)
	if !validWxActor(actor) {
		return "", "", false
	}
	raw, groupCall := params["groupId"]
	if !groupCall {
		mode, allowed = accessMode(policy, actor)
		return actor, mode, allowed
	}
	groupID, _ := raw.(string)
	revision, _ := params["groupRevision"].(string)
	group, ok := policy.Groups[groupID]
	if !ok || group.Revision != revision || (len(group.Members) > 0 && !slices.Contains(group.Members, actor)) {
		return "", "", false
	}
	if group.Session == "shared" {
		if method == "turn/interrupt" && !slices.Contains(group.Admins, actor) {
			return "", "", false
		}
		actor = ""
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{groupID, actor, revision, group.Session}, "|")))
	return "g:" + hex.EncodeToString(digest[:30]), group.Mode, true
}

func validWxProfile(profile wxProfile) bool {
	return len(profile.Workspace) <= 1024 && len(profile.Model) <= 128 && len(profile.Prompt) <= 4096 && !strings.ContainsAny(profile.Workspace, "\x00\r\n") && !strings.ContainsAny(profile.Model, "\x00\r\n") && !strings.ContainsRune(profile.Prompt, 0)
}

func (a *Agent) wxWorkspace(input string) (string, error) {
	if input == "" || samePath(input, a.cfg.Cwd) {
		return a.cfg.Cwd, nil
	}
	projects, err := a.workspaceRPC("codexrm/workspace/list", nil)
	if err != nil {
		return "", err
	}
	for _, entry := range projects.(map[string]any)["workspaces"].([]workspace) {
		if samePath(entry.Path, input) {
			return directoryPath(entry.Path)
		}
	}
	return "", robotErr("此工作区未在 Agent 上登记")
}

func (a *Agent) WXWorkspaces() (any, error) { return a.workspaceRPC("codexrm/workspace/list", nil) }

const wxGroupToolsProfile = "codexrm-wx-group"

func (a *Agent) constrainGroupTools(method, clientID, identity, cwd string, params map[string]any) error {
	switch method {
	case "turn/start":
		delete(params, "sandboxPolicy")
		params["permissions"] = wxGroupToolsProfile
	case "thread/start", "thread/resume":
		workspace, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			return err
		}
		dataDir, err := filepath.EvalSymlinks(a.cfg.DataDir)
		if err != nil {
			return err
		}
		if withinDir(workspace, dataDir) || withinDir(dataDir, workspace) {
			return robotErr("群工具需要单独的工作区，不能包含 Agent 数据目录")
		}
		records, err := a.wxRecordScope(clientID, identity)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(records, 0700); err != nil {
			return err
		}
		delete(params, "sandbox")
		params["permissions"] = wxGroupToolsProfile
		params["config"] = map[string]any{
			"default_permissions": wxGroupToolsProfile,
			"permissions": map[string]any{
				wxGroupToolsProfile: map[string]any{
					"filesystem": map[string]string{":minimal": "read", workspace: "write", records: "read"},
					"network":    map[string]bool{"enabled": false},
				},
			},
			"mcp_servers": map[string]any{},
			"web_search":  "disabled",
			"features": map[string]bool{
				"browser_use": false, "computer_use": false, "shell_snapshot": false,
				"multi_agent": false, "default_mode_request_user_input": false,
			},
		}
	}
	return nil
}

func verifyWxGroupTools(raw json.RawMessage) error {
	var result struct {
		ApprovalPolicy string `json:"approvalPolicy"`
		Profile        struct {
			ID      string
			Extends string
		} `json:"activePermissionProfile"`
		Sandbox struct {
			Type          string
			NetworkAccess bool
		} `json:"sandbox"`
	}
	if json.Unmarshal(raw, &result) != nil || result.ApprovalPolicy != "never" ||
		result.Profile.ID != wxGroupToolsProfile || result.Profile.Extends != "" ||
		result.Sandbox.Type != "workspaceWrite" || result.Sandbox.NetworkAccess {
		return robotErr("当前 Codex 无法验证群工具隔离，请使用仅聊天模式")
	}
	return nil
}
