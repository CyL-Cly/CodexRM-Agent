package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
	"github.com/CyL-Cly/CodexRM-Agent/internal/onebot"
)

const (
	oneBotClientID          = "codexrm-onebot-local"
	defaultOneBotMessageLen = 1800
)

// OneBotAccess is a local authorization. Admin mode is therefore bound to both
// this Agent configuration and the configured QQ account, not to a relay claim.
type OneBotAccess struct {
	Enabled   bool   `json:"enabled"`
	Mode      string `json:"mode"`
	Workspace string `json:"workspace,omitempty"`
	Model     string `json:"model,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
}

type OneBotGroup struct {
	OneBotAccess
	Trigger string   `json:"trigger"`
	Session string   `json:"session"`
	Members []string `json:"members,omitempty"`
	Admins  []string `json:"admins,omitempty"`
}

type OneBotSettings struct {
	Enabled            bool                    `json:"enabled"`
	Mode               string                  `json:"mode"`
	URL                string                  `json:"url,omitempty"`
	Listen             string                  `json:"listen,omitempty"`
	AccessToken        string                  `json:"accessToken,omitempty"`
	ClearAccessToken   bool                    `json:"clearAccessToken,omitempty"`
	SelfID             int64                   `json:"selfId,omitempty"`
	Users              map[string]OneBotAccess `json:"users"`
	Groups             map[string]OneBotGroup  `json:"groups,omitempty"`
	MessageLimit       int                     `json:"messageLimit,omitempty"`
	AllowRemoteMedia   bool                    `json:"allowRemoteMedia"`
	AllowPrivateMedia  bool                    `json:"allowPrivateMedia"`
	AllowLocalMedia    bool                    `json:"allowLocalMedia"`
	AllowForwardLookup bool                    `json:"allowForwardLookup"`
	AutoAcceptFriend   bool                    `json:"autoAcceptFriend"`
	AutoAcceptGroup    bool                    `json:"autoAcceptGroupInvite"`
}

func DefaultOneBotSettings() OneBotSettings {
	return OneBotSettings{
		Mode: "ws-reverse", Listen: "127.0.0.1:6701", Users: map[string]OneBotAccess{}, Groups: map[string]OneBotGroup{},
		MessageLimit: defaultOneBotMessageLen, AllowRemoteMedia: true, AllowLocalMedia: false,
	}
}

func normalizeOneBotID(raw string) (string, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != strings.TrimSpace(raw) {
		return "", errors.New("QQ 号必须是正整数且不能带前导零")
	}
	return strconv.FormatInt(id, 10), nil
}

func validOneBotMode(mode string, group bool) bool {
	if mode == "text" || mode == "tools" {
		return true
	}
	return !group && mode == "admin"
}

func validateOneBotProfile(access OneBotAccess) bool {
	return validWxProfile(wxProfile{Workspace: access.Workspace, Model: access.Model, Prompt: access.Prompt})
}

func (s *OneBotSettings) Normalize() error {
	s.Mode = strings.TrimSpace(s.Mode)
	s.URL = strings.TrimSpace(s.URL)
	s.Listen = strings.TrimSpace(s.Listen)
	s.AccessToken = strings.TrimSpace(s.AccessToken)
	if s.Mode == "" {
		s.Mode = "ws-reverse"
	}
	if s.Listen == "" && s.Mode != "ws" {
		s.Listen = "127.0.0.1:6701"
	}
	if s.MessageLimit == 0 {
		s.MessageLimit = defaultOneBotMessageLen
	}
	if s.MessageLimit < 200 || s.MessageLimit > 4000 {
		return errors.New("单条消息长度必须在 200 到 4000 字之间")
	}
	if s.SelfID < 0 {
		return errors.New("机器人 QQ 号无效")
	}
	if len(s.AccessToken) > 4096 || strings.ContainsAny(s.AccessToken, "\r\n\x00") {
		return errors.New("OneBot Access Token 无效")
	}
	if s.Users == nil {
		s.Users = map[string]OneBotAccess{}
	}
	if s.Groups == nil {
		s.Groups = map[string]OneBotGroup{}
	}
	if len(s.Users) > 1000 || len(s.Groups) > 500 {
		return errors.New("OneBot 联系人或群配置过多")
	}
	users := make(map[string]OneBotAccess, len(s.Users))
	for raw, access := range s.Users {
		id, err := normalizeOneBotID(raw)
		if err != nil || !validOneBotMode(access.Mode, false) || !validateOneBotProfile(access) {
			return fmt.Errorf("私聊配置 %q 无效", raw)
		}
		users[id] = access
	}
	s.Users = users
	groups := make(map[string]OneBotGroup, len(s.Groups))
	for raw, group := range s.Groups {
		id, err := normalizeOneBotID(raw)
		if err != nil || !validOneBotMode(group.Mode, true) || !validateOneBotProfile(group.OneBotAccess) {
			return fmt.Errorf("群配置 %q 无效", raw)
		}
		if group.Trigger != "mention" && group.Trigger != "all" {
			return fmt.Errorf("群 %s 的触发方式无效", id)
		}
		if group.Session != "shared" && group.Session != "member" {
			return fmt.Errorf("群 %s 的会话方式无效", id)
		}
		if len(group.Members) > 1000 || len(group.Admins) > 200 {
			return fmt.Errorf("群 %s 的成员名单过长", id)
		}
		for field, members := range map[string][]string{"成员": group.Members, "管理员": group.Admins} {
			normalized := make([]string, 0, len(members))
			seen := map[string]bool{}
			for _, rawMember := range members {
				member, memberErr := normalizeOneBotID(rawMember)
				if memberErr != nil {
					return fmt.Errorf("群 %s 的%s QQ 号无效", id, field)
				}
				if !seen[member] {
					normalized = append(normalized, member)
					seen[member] = true
				}
			}
			if field == "成员" {
				group.Members = normalized
			} else {
				group.Admins = normalized
			}
		}
		groups[id] = group
	}
	s.Groups = groups
	transport := onebot.Config{Enabled: s.Enabled, Mode: s.Mode, URL: s.URL, Listen: s.Listen, AccessToken: s.AccessToken, SelfID: s.SelfID}
	if err := transport.Validate(); err != nil {
		return err
	}
	return nil
}

func (s OneBotSettings) transport() onebot.Config {
	return onebot.Config{Enabled: s.Enabled, Mode: s.Mode, URL: s.URL, Listen: s.Listen, AccessToken: s.AccessToken, SelfID: s.SelfID}
}

func LoadOneBotSettings(path string) (OneBotSettings, error) {
	settings := DefaultOneBotSettings()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	if len(raw) > 1<<20 {
		return settings, errors.New("OneBot 配置超过 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return settings, fmt.Errorf("解析 OneBot 配置: %w", err)
	}
	settings.ClearAccessToken = false
	if err := settings.Normalize(); err != nil {
		return settings, err
	}
	return settings, nil
}

func SaveOneBotSettings(path string, settings OneBotSettings) error {
	settings.ClearAccessToken = false
	if err := settings.Normalize(); err != nil {
		return err
	}
	return clientconfig.WriteJSON(path, settings)
}

func oneBotGroupKey(id string) string { return id + "@chatroom" }

func oneBotRevision(id string, group OneBotGroup) string {
	raw, _ := json.Marshal(struct {
		ID string `json:"id"`
		OneBotGroup
	}{id, group})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:16])
}

func (s OneBotSettings) robotPolicy() (wxPolicy, map[string]string) {
	policy := wxPolicy{Access: map[string]wxAccess{}, Groups: map[string]wxGroupPolicy{}}
	revisions := map[string]string{}
	for id, access := range s.Users {
		if !access.Enabled {
			continue
		}
		policy.Access[id] = wxAccess{wxProfile: wxProfile{Workspace: access.Workspace, Model: access.Model, Prompt: access.Prompt}, Enabled: true, Mode: access.Mode}
	}
	for id, group := range s.Groups {
		if !group.Enabled {
			continue
		}
		key := oneBotGroupKey(id)
		revision := oneBotRevision(id, group)
		revisions[id] = revision
		policy.Groups[key] = wxGroupPolicy{
			wxProfile: wxProfile{Workspace: group.Workspace, Model: group.Model, Prompt: group.Prompt},
			Session:   group.Session, Mode: group.Mode, Members: group.Members, Admins: group.Admins, Revision: revision,
		}
	}
	return policy, revisions
}

func oneBotSessionsPath(settingsPath string) string {
	return filepath.Join(filepath.Dir(settingsPath), "onebot-sessions.json")
}
