package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type wxOperation struct {
	ID, Method, Path string
	Download         bool
	NoRobot          bool
	Stream           bool
	Raw              bool
	RobotIDInBody    bool
	ConfirmText      string
	Fields           []struct{ Key, Input, Type string }
}

var wxOperationCatalog = sync.OnceValues(func() (map[string]wxOperation, error) {
	raw, err := assets.ReadFile("web/wx-operations.json")
	if err != nil {
		return nil, err
	}
	var list []wxOperation
	if err = json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	catalog := make(map[string]wxOperation, len(list))
	for _, item := range list {
		catalog[item.ID] = item
	}
	return catalog, nil
})

var wxAdminCredentialMu sync.Mutex

func (h *Host) wxOperation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	catalog, err := wxOperationCatalog()
	if err != nil {
		failure(w, err)
		return
	}
	operation, ok := catalog[r.PathValue("operation")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("robot"), 10, 64)
	if !operation.NoRobot && (err != nil || id < 1) {
		failure(w, errors.New("请选择机器人"))
		return
	}
	if operation.ConfirmText != "" && r.Header.Get("X-CodexRM-Confirm") != "delete-robot" {
		failure(w, errors.New("请确认删除机器人数据"))
		return
	}
	duration := 2 * time.Minute
	if operation.Stream {
		duration = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), duration)
	defer cancel()
	if operation.Path == "/user/api-token/refresh" || operation.Path == "/user/logout" {
		wxAdminCredentialMu.Lock()
		defer wxAdminCredentialMu.Unlock()
	}
	var client *wxAdminClient
	if operation.ID == "get-wx-admin-probe" || operation.ID == "get-wx-admin-swagger" || operation.ID == "get-wx-admin-oauth-qr" {
		var base string
		base, err = wxAdminEndpoint(ctx)
		client = &wxAdminClient{base: base}
	} else {
		client, err = h.wxAdmin(ctx)
	}
	if err != nil {
		failure(w, err)
		return
	}
	if r.URL.Query().Has("account") {
		account, err := client.wxOperationAccount(ctx, r, id)
		if err != nil {
			failure(w, err)
			return
		}
		switch operation.Path {
		case "/chat-room-settings", "/friend-settings":
			h.wxContactPluginSettings(ctx, w, r, operation, account)
			return
		}
	}
	if operation.ID == "test-oss-settings" {
		var robot wxAdminRobot
		if err := client.call(ctx, http.MethodGet, "/robot/view?id="+strconv.FormatInt(id, 10), nil, &robot); err != nil {
			failure(w, err)
			return
		}
		account := inspectWXAccount(ctx, "client_"+robot.Code)
		if !account.address.IsValid() {
			failure(w, errors.New("请先启动所选机器人的客户端"))
			return
		}
		var result struct {
			OK       bool   `json:"ok"`
			Provider string `json:"provider"`
		}
		if err := account.call(ctx, operation.Method, "oss-settings/test", nil, &result); err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, result)
		return
	}
	query := url.Values{}
	if !operation.NoRobot {
		query.Set("id", strconv.FormatInt(id, 10))
	}
	headers := http.Header{}
	var body io.Reader
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	defer r.Body.Close()
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data;") {
		// Multipart fields go only to documented upload operations.
		upload := false
		for _, field := range operation.Fields {
			if field.Input == "formData" && field.Type == "file" {
				upload = true
				break
			}
		}
		if !upload {
			failure(w, errors.New("此操作不接受文件"))
			return
		}
		body = r.Body
		headers.Set("Content-Type", r.Header.Get("Content-Type"))
	}
	for _, field := range operation.Fields {
		if field.Input == "query" && field.Key != "id" && !(field.Key == "profile" && strings.HasPrefix(operation.Path, "/pprof/")) {
			if values := r.URL.Query()[field.Key]; len(values) > 0 {
				query[field.Key] = values
			}
		}
	}
	var raw json.RawMessage
	if body == nil && operation.Method != http.MethodGet {
		if err = json.NewDecoder(r.Body).Decode(&raw); err != nil {
			failure(w, errors.New("请求字段无效"))
			return
		}
	}
	if operation.RobotIDInBody {
		// Bind the selected account to the upstream request body as well as the owner check query.
		raw = json.RawMessage(`{"id":` + strconv.FormatInt(id, 10) + `}`)
	}
	path := operation.Path
	if operation.ID == "get-robot--pprof" {
		profile := r.URL.Query().Get("profile")
		switch profile {
		case "", "allocs", "block", "goroutine", "heap", "mutex", "profile", "threadcreate", "trace", "cmdline", "symbol":
		default:
			failure(w, errors.New("不支持的诊断类型"))
			return
		}
		if seconds := query.Get("seconds"); seconds != "" {
			n, err := strconv.Atoi(seconds)
			if err != nil || n < 1 || n > 90 {
				failure(w, errors.New("采样时间必须在 1 至 90 秒之间"))
				return
			}
		}
		path += profile
	}
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	if operation.Stream || operation.Raw {
		client.wxRawOperation(ctx, w, operation, path, raw)
		return
	}
	if operation.Download {
		client.download(ctx, w, path)
		return
	}
	if body == nil {
		if err = client.call(ctx, operation.Method, path, raw, &raw); err != nil {
			failure(w, err)
			return
		}
		if operation.Path == "/user/api-token/refresh" && client.apiToken != "" {
			var token string
			if err = json.Unmarshal(raw, &token); err != nil || token == "" {
				failure(w, errors.New("获取新令牌失败"))
				return
			}
			if err := writeJSONFile(h.wxAdminSecretPath(), "wxapi:"+token); err != nil {
				respond(w, 200, map[string]string{"token": token, "warning": "新令牌已生效，Agent 保存失败。请复制令牌以重新接管。"})
				return
			}
		}
		if operation.Path == "/user/logout" {
			if err := os.Remove(h.wxAdminSecretPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
				failure(w, errors.New("本地后台凭据清除失败"))
				return
			}
		}
		respond(w, 200, raw)
		return
	}
	resp, err := client.request(ctx, operation.Method, path, body, headers.Get("Content-Type"))
	if err != nil {
		failure(w, errors.New("上传失败"))
		return
	}
	defer resp.Body.Close()
	var result struct {
		Code    int
		Message string
		Data    json.RawMessage
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&result) != nil || resp.StatusCode != 200 {
		failure(w, errors.New("上传接口响应无效"))
		return
	}
	if result.Code != 200 {
		failure(w, errors.New(result.Message))
		return
	}
	respond(w, 200, result.Data)
}

func (client *wxAdminClient) wxOperationAccount(ctx context.Context, r *http.Request, id int64) (wxAccount, error) {
	query := r.URL.Query()
	var robot wxAdminRobot
	if err := client.call(ctx, http.MethodGet, "/robot/view?id="+strconv.FormatInt(id, 10), nil, &robot); err != nil {
		return wxAccount{}, err
	}
	if robot.Code == "" || query.Get("account") != "client_"+robot.Code {
		return wxAccount{}, errors.New("机器人与当前选中的微信账号不匹配")
	}
	account := inspectWXAccount(ctx, query.Get("account"))
	if !account.LoggedIn || account.WxID != query.Get("wxid") {
		return wxAccount{}, errors.New("微信账号未登录或已更换")
	}
	return account, nil
}

func (h *Host) wxContactPluginSettings(ctx context.Context, w http.ResponseWriter, r *http.Request, op wxOperation, account wxAccount) {
	query := r.URL.Query()
	contact := query.Get("contact")
	isGroup := op.Path == "/chat-room-settings"
	if contact == "" || len(contact) > 64 || strings.ContainsAny(contact, "\r\n\t \x00") ||
		isGroup != strings.HasSuffix(contact, "@chatroom") {
		failure(w, errors.New("联系人 ID 无效"))
		return
	}
	queryKey, bodyKey := "contact_id", "wechat_id"
	if isGroup {
		queryKey, bodyKey = "chat_room_id", "chat_room_id"
	}
	path := strings.TrimPrefix(op.Path, "/")
	if op.Method == http.MethodPost {
		// Share the read/create lock with the welcome and departure editor.
		wxGroupNoticeSaveMu.Lock()
		defer wxGroupNoticeSaveMu.Unlock()
	}
	var current map[string]json.RawMessage
	if err := account.call(ctx, http.MethodGet, path+"?"+url.Values{queryKey: {contact}}.Encode(), nil, &current); err != nil {
		failure(w, err)
		return
	}
	var configID int64
	if raw := current["id"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &configID); err != nil || configID < 0 {
			failure(w, errors.New("设置记录 ID 无效"))
			return
		}
	}
	if configID > 0 {
		var owner string
		if json.Unmarshal(current[bodyKey], &owner) != nil || owner != contact {
			failure(w, errors.New("设置与当前联系人不匹配"))
			return
		}
	}
	var global map[string]json.RawMessage
	if configID == 0 {
		if err := account.call(ctx, http.MethodGet, "global-settings", nil, &global); err != nil {
			failure(w, err)
			return
		}
	}
	catalog, _ := wxOperationCatalog()
	allowed := catalog["post-"+path].Fields
	editable := func(key string) bool {
		return key != "id" && key != "config_id" && key != bodyKey
	}
	if op.Method == http.MethodGet {
		values := make(map[string]json.RawMessage)
		for _, field := range allowed {
			if !editable(field.Key) {
				continue
			}
			value := current[field.Key]
			if configID == 0 {
				value = global[field.Key]
			}
			if len(value) != 0 {
				values[field.Key] = value
			}
		}
		respond(w, 200, map[string]any{"values": values, "inherited": configID == 0})
		return
	}
	var input map[string]json.RawMessage
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input) == 0 {
		failure(w, errors.New("请修改要保存的设置"))
		return
	}
	patch := map[string]any{bodyKey: contact, "id": configID}
	for _, field := range allowed {
		if !editable(field.Key) {
			continue
		}
		if value, ok := input[field.Key]; ok {
			patch[field.Key] = value
		} else if configID == 0 {
			if value := global[field.Key]; len(value) != 0 && string(value) != "null" {
				patch[field.Key] = value
			}
		}
	}
	if isGroup && configID == 0 {
		for key, value := range wxNoticeFields(global, account.Capabilities.GroupNoticeFormats) {
			if _, sent := patch[key]; !sent && string(value) != "null" {
				patch[key] = value
			}
		}
	}
	// These switches require their associated fields in the same request.
	for _, key := range []string{"welcome_type", "welcome_text", "welcome_url", "welcome_image_url", "welcome_emoji_md5", "welcome_emoji_len", "leave_chat_room_alert_text", "podcast_config", "wxhb_notify_member_list"} {
		if _, sent := patch[key]; !sent && len(current[key]) != 0 {
			patch[key] = current[key]
		}
	}
	if err := account.call(ctx, http.MethodPost, path, patch, nil); err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, nil)
}

func (c *wxAdminClient) download(ctx context.Context, w http.ResponseWriter, path string) {
	resp, err := c.request(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		failure(w, errors.New("无法获取微信文件"))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		failure(w, errors.New("微信文件不可用"))
		return
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		var result struct{ Message string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&result)
		if result.Message == "" {
			result.Message = "微信文件不可用"
		}
		failure(w, errors.New(result.Message))
		return
	}
	const max = 64 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil || len(raw) > max {
		failure(w, errors.New("下载失败或超过 64 MiB 限制"))
		return
	}
	name := "wechat-media.bin"
	if strings.HasPrefix(path, "/chat/image/") {
		name = "wechat-image"
	}
	if strings.HasPrefix(path, "/chat/voice/") {
		name = "wechat-voice.wav"
	}
	if strings.HasPrefix(path, "/chat/video/") {
		name = "wechat-video.mp4"
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+name)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(raw)
}
