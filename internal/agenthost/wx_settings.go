package agenthost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

const wxSupported = runtime.GOOS == "linux"

var wxContainerName = regexp.MustCompile(`^client_[A-Za-z0-9]{16}$`)
var wxTransport = &http.Client{
	Transport:     &http.Transport{DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, DisableKeepAlives: true},
	Timeout:       6 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

type wxAccount struct {
	ID           string         `json:"id"`
	WxID         string         `json:"wxid"`
	Name         string         `json:"name"`
	LoggedIn     bool           `json:"loggedIn"`
	Capabilities wxCapabilities `json:"capabilities"`
	address      netip.Addr
}

type wxSettings struct {
	Enabled        bool   `json:"enabled"`
	Paired         bool   `json:"paired"`
	AgentOnline    bool   `json:"agent_online"`
	ServerURL      string `json:"server_url"`
	WorkspaceAlias string `json:"workspace_alias"`
}

func wxAccounts(ctx context.Context) ([]wxAccount, error) {
	if !wxSupported {
		return []wxAccount{}, nil
	}
	docker, err := wxLookPath("docker")
	if err != nil {
		return []wxAccount{}, nil
	}
	out, err := wxCommandOutput(ctx, docker, wxDockerArgs("ps", "-a", "--format", "{{.Names}}")...)
	if err != nil {
		return nil, err
	}
	names := strings.Fields(string(out))
	accounts := make([]wxAccount, 0, len(names))
	for _, name := range names {
		if wxContainerName.MatchString(name) {
			accounts = append(accounts, wxAccount{ID: name})
		}
	}
	var all sync.WaitGroup
	for i := range accounts {
		all.Add(1)
		go func() {
			defer all.Done()
			accounts[i] = inspectWXAccount(ctx, accounts[i].ID)
		}()
	}
	all.Wait()
	slices.SortFunc(accounts, func(a, b wxAccount) int { return strings.Compare(a.ID, b.ID) })
	return accounts, nil
}

func inspectWXAccount(ctx context.Context, id string) wxAccount {
	account := wxAccount{ID: id}
	if !wxSupported || !wxContainerName.MatchString(id) {
		return account
	}
	docker, err := wxLookPath("docker")
	if err != nil {
		return account
	}
	out, err := wxCommandOutput(ctx, docker, wxDockerArgs("inspect", "--type=container", "--format", `{{if and .State.Running (eq .Path "/app/wechat-robot-client")}}{{with index .NetworkSettings.Networks "wechat-robot"}}{{.IPAddress}}{{end}}{{end}}`, id)...)
	if err != nil {
		return account
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(string(out)))
	if err != nil || !addr.Is4() || !addr.IsPrivate() {
		return account
	}
	account.address = addr
	if account.call(ctx, http.MethodGet, "codexrm/capabilities", nil, &account.Capabilities) == nil {
		account.Capabilities.CodexRM = true
	}
	var identity struct {
		WxID string `json:"Wxid"`
		Name string `json:"NickName"`
	}
	if account.call(ctx, http.MethodGet, "get-cached-info", nil, &identity) == nil {
		account.WxID, account.Name = identity.WxID, identity.Name
	}
	if account.WxID != "" {
		_ = account.call(ctx, http.MethodGet, "is-loggedin", nil, &account.LoggedIn)
	}
	if account.LoggedIn && !account.Capabilities.CodexRM {
		account.Capabilities.CodexRM = account.call(ctx, http.MethodGet, "codexrm/settings", nil, nil) == nil
	}
	return account
}

func (a wxAccount) call(ctx context.Context, method, path string, value, result any) error {
	var body io.Reader
	if value != nil {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+net.JoinHostPort(a.address.String(), "9000")+"/api/v1/robot/"+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	transport := *wxTransport
	if path == "contacts/sync" || path == "chat-room/members/sync" || path == "oss-settings/test" {
		transport.Timeout = 0
	}
	resp, err := transport.Do(req)
	if err != nil {
		return errors.New("无法访问微信机器人容器")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errors.New("微信机器人客户端版本缺少 CodexRM 接口")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("微信机器人接口返回 HTTP %d", resp.StatusCode)
	}
	var data struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&data); err != nil {
		return errors.New("微信机器人接口响应无法读取")
	}
	if data.Code != 200 {
		if data.Message != "" {
			return errors.New(data.Message)
		}
		return fmt.Errorf("微信机器人接口返回错误 %d", data.Code)
	}
	if result != nil && len(data.Data) > 0 {
		return json.Unmarshal(data.Data, result)
	}
	return nil
}

func (h *Host) wxList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	accounts, err := wxAccounts(ctx)
	if err != nil {
		failure(w, errors.New("无法读取本机 Docker 中的微信机器人"))
		return
	}
	respond(w, 200, accounts)
}

type wxInput struct {
	Enabled        bool   `json:"enabled"`
	WorkspaceAlias string `json:"workspace_alias"`
}

func (h *Host) wxConfigure(ctx context.Context, account wxAccount, input wxInput) error {
	if input.Enabled && !account.Capabilities.CodexOnly {
		return errors.New("请先升级机器人客户端至支持仅 Codex 回复的版本")
	}
	h.opMu.Lock()
	defer h.opMu.Unlock()
	var before wxSettings
	if err := account.call(ctx, http.MethodGet, "codexrm/settings", nil, &before); err != nil {
		return err
	}
	h.mu.Lock()
	relay := h.wxLocalServer
	if relay == "" && !h.base.SecureRelay {
		relay = h.config.Server
	}
	needsLocalRelay := !wxLocalRelay(relay) || (h.base.SecureRelay && relay == h.config.Server)
	h.mu.Unlock()
	if input.Enabled && !before.Paired && needsLocalRelay {
		if err := h.ensureWXRelay(ctx); err != nil {
			return err
		}
		h.mu.Lock()
		relay = h.wxLocalServer
		h.mu.Unlock()
	}
	if before.Paired && input.Enabled {
		relay = before.ServerURL
	}
	server := before.ServerURL
	if input.Enabled {
		server = relay
	}
	if err := account.call(ctx, http.MethodPost, "codexrm/settings", map[string]any{"enabled": input.Enabled, "server_url": server, "workspace_alias": input.WorkspaceAlias}, nil); err != nil {
		return err
	}
	if !input.Enabled || before.Paired {
		return nil
	}
	a, pairCtx, done, err := h.waitWXAgent(ctx)
	if err == nil {
		defer done()
		var code string
		pair, pairErr := a.Pair(pairCtx)
		err = pairErr
		if pairErr == nil {
			code = pair.PairingCode
			err = account.call(pairCtx, http.MethodPost, "codexrm/pair", map[string]string{"code": code}, nil)
		}
	}
	if err != nil {
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 6*time.Second)
		defer cancel()
		restore := map[string]any{"enabled": before.Enabled, "server_url": before.ServerURL, "workspace_alias": before.WorkspaceAlias}
		_ = account.call(restoreCtx, http.MethodPost, "codexrm/settings", restore, nil)
	}
	return err
}

func wxLocalRelay(raw string) bool {
	relay, err := url.Parse(raw)
	if err != nil || (relay.Scheme != "http" && relay.Scheme != "https") {
		return false
	}
	ip, err := netip.ParseAddr(relay.Hostname())
	if err != nil || ip.IsLoopback() || !ip.IsPrivate() {
		return false
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, address := range addresses {
		if prefix, err := netip.ParsePrefix(address.String()); err == nil && prefix.Addr() == ip {
			return true
		}
	}
	return false
}

func (h *Host) wxSettings(w http.ResponseWriter, r *http.Request) {
	method, route := r.Method, r.PathValue("action")
	allowed := map[string]string{"settings": "GET POST", "access": "GET POST", "commands": "GET POST", "command-enabled": "POST", "command-users": "POST", "disconnect": "POST", "group": "GET POST", "groups": "GET"}
	if !slices.Contains(strings.Fields(allowed[route]), method) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	account := inspectWXAccount(ctx, r.URL.Query().Get("id"))
	if !account.LoggedIn || account.WxID != r.URL.Query().Get("wxid") {
		http.Error(w, "微信账号未登录或已更换", http.StatusConflict)
		return
	}
	var input json.RawMessage
	if method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			failure(w, err)
			return
		}
	}
	if route == "settings" && method == http.MethodPost {
		var cfg wxInput
		if err := json.Unmarshal(input, &cfg); err != nil {
			failure(w, err)
			return
		}
		if err := h.wxConfigure(ctx, account, cfg); err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	paths := map[string]string{"settings": "settings", "access": "access", "commands": "commands", "command-enabled": "commands/enabled", "command-users": "commands/users", "disconnect": "revoke", "group": "group", "groups": "groups"}
	path := "codexrm/" + paths[route]
	if route == "access" && method == http.MethodGet {
		path += "?wechat_id=" + url.QueryEscape(r.URL.Query().Get("friend"))
	}
	if route == "group" && method == http.MethodGet {
		path += "?group_id=" + url.QueryEscape(r.URL.Query().Get("friend"))
	}
	var result json.RawMessage
	var payload any
	if input != nil {
		payload = input
	}
	if err := account.call(ctx, method, path, payload, &result); err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, result)
}
