package agenthost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
)

const wxAdminContainer = "wechat-robot-admin-backend"

type wxAdminClient struct {
	base     string
	cookies  []*http.Cookie
	apiToken string
}

type wxAdminRobot struct {
	ID       int64  `json:"id"`
	Code     string `json:"robot_code"`
	Name     string `json:"robot_name"`
	WxID     string `json:"wechat_id"`
	Nickname string `json:"nickname"`
	Status   string `json:"status"`
}

func wxAdminEndpoint(ctx context.Context) (string, error) {
	docker, err := wxLookPath("docker")
	if err != nil {
		return "", errors.New("无法访问本机 Docker")
	}
	output, err := wxCommandOutput(ctx, docker, wxDockerArgs("inspect", "--type=container", "--format", `{{if .State.Running}}{{with index .NetworkSettings.Networks "wechat-robot"}}{{.IPAddress}}{{end}}{{end}}`, wxAdminContainer)...)
	if err != nil {
		return "", errors.New("wxbot 管理服务未启动")
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(string(output)))
	if err != nil || !addr.Is4() || !addr.IsPrivate() {
		return "", errors.New("无法定位 wxbot 管理服务")
	}
	return "http://" + net.JoinHostPort(addr.String(), "9000") + "/api/v1", nil
}

func (c *wxAdminClient) call(ctx context.Context, method, path string, input, out any) error {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	contentType := ""
	if body != nil {
		contentType = "application/json"
	}
	resp, err := c.request(ctx, method, path, body, contentType)
	if err != nil {
		return errors.New("无法连接 wxbot 管理服务")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("wxbot 管理接口返回 HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		Code    int
		Message string
		Data    json.RawMessage
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&envelope); err != nil {
		return errors.New("wxbot 管理接口响应无效")
	}
	if envelope.Code != 200 {
		// Initialization can fail after a row has been created. Keep that ID for recovery.
		if strings.HasPrefix(path, "/robot/create") && out != nil {
			_ = json.Unmarshal(envelope.Data, out)
		}
		if envelope.Message == "" {
			return fmt.Errorf("wxbot 管理接口错误 %d", envelope.Code)
		}
		return errors.New(envelope.Message)
	}
	if path == "/login" || path == "/oauth/wechat" {
		c.cookies = resp.Cookies()
	}
	if out != nil {
		return json.Unmarshal(envelope.Data, out)
	}
	return nil
}

func wxAdminLogin(ctx context.Context, token string) (*wxAdminClient, error) {
	if token == "" {
		return nil, errors.New("请接管 wxbot 后再管理机器人")
	}
	base, err := wxAdminEndpoint(ctx)
	if err != nil {
		return nil, err
	}
	client := &wxAdminClient{base: base}
	if apiToken, ok := strings.CutPrefix(token, "wxapi:"); ok {
		client.apiToken = apiToken
		err := client.call(ctx, http.MethodGet, "/robot/list?page_size=1&page_index=1", nil, nil)
		return client, err
	}
	if err = client.call(ctx, http.MethodPost, "/login", map[string]string{"token": token}, nil); err != nil {
		return nil, err
	}
	if len(client.cookies) == 0 {
		return nil, errors.New("wxbot 未建立管理会话")
	}
	return client, nil
}

func wxAdminOAuthCredential(ctx context.Context, code string) (string, error) {
	base, err := wxAdminEndpoint(ctx)
	if err != nil {
		return "", err
	}
	client := &wxAdminClient{base: base}
	if err := client.call(ctx, http.MethodPost, "/oauth/wechat", map[string]string{"code": code}, nil); err != nil {
		return "", err
	}
	var user struct {
		Token string `json:"api_token"`
	}
	if err := client.call(ctx, http.MethodGet, "/user/self", nil, &user); err != nil {
		return "", err
	}
	if user.Token == "" {
		if err := client.call(ctx, http.MethodPost, "/user/api-token/refresh", nil, &user.Token); err != nil {
			return "", err
		}
	}
	return "wxapi:" + user.Token, nil
}

func (h *Host) wxAdminSecretPath() string { return filepath.Join(h.StateDir(), "wx-admin-secret.json") }

func (h *Host) wxAdmin(ctx context.Context) (*wxAdminClient, error) {
	secret, err := readJSONFile[string](h.wxAdminSecretPath())
	if err != nil {
		return nil, errors.New("无法读取 wxbot 管理凭据")
	}
	if secret == nil {
		return nil, errors.New("请先安装或接管 wxbot")
	}
	return wxAdminLogin(ctx, *secret)
}

func (c *wxAdminClient) robots(ctx context.Context) ([]wxAdminRobot, error) {
	items := []wxAdminRobot{}
	for page := 1; page <= 20; page++ {
		var rows struct {
			Items []wxAdminRobot
			Total int
		}
		if err := c.call(ctx, http.MethodGet, "/robot/list?page_size=50&page_index="+strconv.Itoa(page), nil, &rows); err != nil {
			return nil, err
		}
		items = append(items, rows.Items...)
		if len(rows.Items) == 0 || len(items) >= rows.Total {
			return items, nil
		}
	}
	return items, nil
}

func (c *wxAdminClient) ensureFirstRobot(ctx context.Context) error {
	rows, err := c.robots(ctx)
	if err != nil {
		return err
	}
	id := int64(0)
	if len(rows) == 0 {
		var result struct {
			ID int64 `json:"id"`
		}
		if err := c.call(ctx, http.MethodPost, "/robot/create", map[string]string{"robot_name": "CodexRM 微信机器人", "version": "8.0.74"}, &result); err != nil {
			return err
		}
		id = result.ID
		rows, err = c.robots(ctx)
		if err != nil {
			return err
		}
	} else {
		id = rows[0].ID
	}
	for _, robot := range rows {
		if robot.ID == id {
			return c.ensureRobotContainers(ctx, robot)
		}
	}
	return errors.New("无法找到新建的机器人")
}

func (c *wxAdminClient) ensureRobotContainers(ctx context.Context, robot wxAdminRobot) error {
	docker, err := wxLookPath("docker")
	if err != nil {
		return err
	}
	for _, kind := range []string{"server", "client"} {
		name := kind + "_" + robot.Code
		if !wxRobotContainer.MatchString(name) {
			return errors.New("现有机器人实例 ID 无效")
		}
		state, inspectErr := wxCommandOutput(ctx, docker, wxDockerArgs("inspect", "--type=container", "--format", "{{.State.Status}}", name)...)
		if inspectErr == nil {
			if strings.TrimSpace(string(state)) != "running" {
				if _, err := wxCommandOutput(ctx, docker, wxDockerArgs("start", name)...); err != nil {
					return fmt.Errorf("无法启动已有容器 %s: %w", name, err)
				}
			}
			continue
		}
		id := strconv.FormatInt(robot.ID, 10)
		if err := c.call(ctx, http.MethodPost, "/robot/docker/container/"+kind+"/start?id="+id, map[string]int64{"id": robot.ID}, nil); err != nil {
			return err
		}
	}
	return waitWXClientCapabilities(ctx, "client_"+robot.Code, 90*time.Second)
}

func waitWXClientCapabilities(ctx context.Context, name string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		account := inspectWXAccount(waitCtx, name)
		if account.Capabilities.CodexRM && account.Capabilities.GroupChat && account.Capabilities.FileContext && account.Capabilities.CodexOnly {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return errors.New("客户端尚未就绪，或镜像缺少仅 Codex 回复、群聊或文件上下文支持")
		case <-time.After(2 * time.Second):
		}
	}
}

type wxLoginScan struct {
	ID, QRUUID           string
	UUID, Ticket, Data62 string
	ExpiresAt            time.Time
	Slider               *wxSliderChallenge
}

var wxSecurityCode = regexp.MustCompile(`^[0-9]{6}$`)

func (h *Host) saveWXScan(robotID int64, scan wxLoginScan) {
	h.wxScanMu.Lock()
	defer h.wxScanMu.Unlock()
	if h.wxScans == nil {
		h.wxScans = make(map[int64]wxLoginScan)
	}
	for id, old := range h.wxScans {
		if time.Now().After(old.ExpiresAt) {
			delete(h.wxScans, id)
		}
	}
	h.wxScans[robotID] = scan
}

func (h *Host) getWXScan(robotID int64, id string) (wxLoginScan, error) {
	h.wxScanMu.Lock()
	defer h.wxScanMu.Unlock()
	scan := h.wxScans[robotID]
	if id == "" || scan.ID != id || time.Now().After(scan.ExpiresAt) {
		return wxLoginScan{}, errors.New("登录请求已过期，请重新获取二维码")
	}
	return scan, nil
}

func (h *Host) updateWXScan(robotID int64, scan wxLoginScan, finished bool) {
	h.wxScanMu.Lock()
	defer h.wxScanMu.Unlock()
	if h.wxScans[robotID].ID != scan.ID {
		return
	}
	if finished {
		delete(h.wxScans, robotID)
	} else {
		if current := h.wxScans[robotID]; current.Ticket == scan.Ticket {
			scan.Slider = current.Slider
		} else {
			scan.Slider = nil
		}
		h.wxScans[robotID] = scan
	}
}

func (h *Host) wxRobotManagement(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("operation")
	if (action == "list" && r.Method != http.MethodGet) || (action != "list" && r.Method != http.MethodPost) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	switch action {
	case "list", "create", "login", "login-check", "login-code", "login-slider", "login-slider-auto", "logout", "restart-client", "restart-server", "activate", "export-login":
	default:
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	client, err := h.wxAdmin(ctx)
	if err != nil {
		failure(w, err)
		return
	}
	if action == "list" {
		rows, err := client.robots(ctx)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, rows)
		return
	}
	if action == "export-login" {
		if r.Method != http.MethodPost || r.Header.Get("X-CodexRM-Confirm") != "export-login" {
			failure(w, errors.New("请确认导出设备登录数据"))
			return
		}
		id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil || id < 1 {
			failure(w, errors.New("请选择机器人"))
			return
		}
		var data string
		if err := client.call(ctx, http.MethodGet, "/robot/export-login-data?id="+strconv.FormatInt(id, 10), nil, &data); err != nil {
			failure(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename=wxbot-login-"+strconv.FormatInt(id, 10)+".txt")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, data)
		return
	}
	var input struct {
		ID                 int64
		Name, ScanID, Code string
		Version            string
		Proxy              map[string]string
		LoginType          string
		IsPretender        bool
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err = json.NewDecoder(r.Body).Decode(&input); err != nil {
		failure(w, errors.New("机器人操作参数无效"))
		return
	}
	if action == "create" {
		name := strings.TrimSpace(input.Name)
		if name == "" || len(name) > 255 {
			failure(w, errors.New("请输入机器人名称"))
			return
		}
		var result struct {
			ID int64 `json:"id"`
		}
		if input.Version == "" {
			input.Version = "8.0.74"
		}
		err = client.call(ctx, http.MethodPost, "/robot/create", map[string]any{"robot_name": name, "version": input.Version, "proxy": input.Proxy}, &result)
		if err != nil && result.ID == 0 {
			failure(w, err)
			return
		}
		respond(w, 200, map[string]any{"id": result.ID, "created": err == nil})
		return
	}
	if input.ID <= 0 {
		failure(w, errors.New("请选择机器人"))
		return
	}
	query := "?" + url.Values{"id": {strconv.FormatInt(input.ID, 10)}}.Encode()
	var response any
	switch action {
	case "login":
		loginType := strings.TrimSpace(input.LoginType)
		switch loginType {
		case "", "ipad":
			loginType, input.IsPretender = "ipad", false
		case "win", "mac", "car":
		default:
			failure(w, errors.New("请选择支持扫码的微信设备"))
			return
		}
		var result struct {
			UUID      string
			Data62    string
			AutoLogin bool `json:"auto_login"`
		}
		err = client.call(ctx, http.MethodPost, "/robot/login"+query, map[string]any{"login_type": loginType, "is_pretender": input.IsPretender}, &result)
		if err == nil && result.UUID != "" && len(result.UUID) <= 256 {
			png, pngErr := qrcode.Encode("http://weixin.qq.com/x/"+url.PathEscape(result.UUID), qrcode.Medium, 224)
			if pngErr != nil {
				err = pngErr
			} else {
				scan := wxLoginScan{ID: randomSecret(16), QRUUID: result.UUID, Data62: result.Data62, ExpiresAt: time.Now().Add(5 * time.Minute)}
				h.saveWXScan(input.ID, scan)
				response = map[string]any{"scanId": scan.ID, "qr": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)}
			}
		} else if err == nil && result.AutoLogin {
			response = map[string]bool{"loggedIn": true}
		} else if err == nil {
			err = errors.New("wxbot 未返回登录二维码")
		}
	case "login-check":
		scan, lookupErr := h.getWXScan(input.ID, input.ScanID)
		if lookupErr != nil {
			err = lookupErr
			break
		}
		var result struct {
			UUID, Ticket        string
			Status, ExpiredTime int
			Account             struct{ UserName string } `json:"acctSectResp"`
		}
		err = client.call(ctx, http.MethodPost, "/robot/login-check"+query, map[string]string{"uuid": scan.QRUUID}, &result)
		if err == nil {
			loggedIn := result.Account.UserName != ""
			expired := result.Status == 4 || (result.ExpiredTime <= 0 && result.Ticket == "")
			scan.Ticket, scan.UUID = result.Ticket, result.UUID
			h.updateWXScan(input.ID, scan, loggedIn || expired)
			response = map[string]any{"loggedIn": loggedIn, "needsCode": result.Ticket != "", "expired": expired}
		}
	case "login-slider":
		var path string
		path, err = h.wxPrepareSlider(ctx, client, input.ID, input.ScanID)
		response = map[string]string{"path": path}
	case "login-slider-auto":
		scan, lookupErr := h.getWXScan(input.ID, input.ScanID)
		if lookupErr != nil {
			err = lookupErr
			break
		}
		if scan.Ticket == "" || scan.Data62 == "" {
			err = errors.New("微信未要求滑块验证或验证已过期")
			break
		}
		if err = client.call(ctx, http.MethodGet, "/robot/view"+query, nil, nil); err == nil {
			err = client.call(ctx, http.MethodPost, "/robot/login/slider-auto"+query, map[string]string{"data62": scan.Data62, "ticket": scan.Ticket}, nil)
		}
	case "login-code":
		scan, lookupErr := h.getWXScan(input.ID, input.ScanID)
		if lookupErr != nil {
			err = lookupErr
			break
		}
		if scan.Ticket == "" || !wxSecurityCode.MatchString(input.Code) {
			err = errors.New("请输入微信显示的六位安全验证码")
			break
		}
		uuid := scan.UUID
		if uuid == "" {
			uuid = scan.QRUUID
		}
		err = client.call(ctx, http.MethodPost, "/robot/login/2fa"+query, map[string]any{"id": input.ID, "uuid": uuid, "code": input.Code, "ticket": scan.Ticket, "data62": scan.Data62}, nil)
		if err == nil {
			scan.Ticket = ""
			h.updateWXScan(input.ID, scan, false)
		}
	case "logout":
		err = client.call(ctx, http.MethodDelete, "/robot/logout"+query, nil, nil)
	case "restart-client", "restart-server":
		err = client.call(ctx, http.MethodPost, "/robot/"+action+query, map[string]any{}, nil)
	case "activate":
		var row wxAdminRobot
		if err = client.call(ctx, http.MethodGet, "/robot/view"+query, nil, &row); err == nil {
			account := inspectWXAccount(ctx, "client_"+row.Code)
			if !account.LoggedIn {
				err = errors.New("等待微信客户端就绪")
			} else if !account.Capabilities.CodexOnly {
				err = errors.New("请先升级机器人客户端至支持仅 Codex 回复的版本")
			} else {
				synced, syncErr := h.syncWXContacts(ctx, account)
				var settings wxSettings
				pairErr := account.call(ctx, http.MethodGet, "codexrm/settings", nil, &settings)
				if pairErr == nil && !settings.Paired {
					pairErr = h.wxConfigure(ctx, account, wxInput{Enabled: true, WorkspaceAlias: settings.WorkspaceAlias})
				}
				result := map[string]any{"id": account.ID, "wxid": account.WxID, "synced": syncErr == nil, "paired": pairErr == nil, "syncedAt": synced}
				if pairErr != nil {
					result["pairError"] = pairErr.Error()
				}
				if syncErr != nil {
					result["syncError"] = syncErr.Error()
				}
				response = result
			}
		}
	}
	if err != nil {
		failure(w, err)
		return
	}
	if response == nil {
		response = map[string]bool{"ok": true}
	}
	respond(w, 200, response)
}
