package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The robot creates a row when the database ID is absent. Share the lock
// with the contact editor so both panels can safely initialize a group.
var wxGroupNoticeSaveMu sync.Mutex

const wxLeaveNoticePrefix = "leave_chat_room_alert_"

var wxNoticeSuffixes = []string{"enabled", "type", "text", "emoji_md5", "emoji_len", "image_url", "url", "card_title", "card_description", "card_thumb_url"}

func wxExtendedNoticeField(key string) bool {
	return strings.Contains(key, "_card_") || (strings.HasPrefix(key, wxLeaveNoticePrefix) &&
		key != wxLeaveNoticePrefix+"enabled" && key != wxLeaveNoticePrefix+"text")
}

func wxNoticeFields(raw map[string]json.RawMessage, extended bool) map[string]json.RawMessage {
	values := make(map[string]json.RawMessage)
	for _, prefix := range []string{"welcome_", wxLeaveNoticePrefix} {
		for _, suffix := range wxNoticeSuffixes {
			if prefix == "welcome_" && suffix == "card_description" {
				continue // The original welcome text field is the card description.
			}
			key := prefix + suffix
			if value, ok := raw[key]; ok && (extended || !wxExtendedNoticeField(key)) {
				values[key] = value
			}
		}
	}
	return values
}

func wxReadNoticeSettings(ctx context.Context, account wxAccount, group string) (map[string]json.RawMessage, int64, error) {
	var raw map[string]json.RawMessage
	path := "global-settings"
	if group != "" {
		path = "chat-room-settings?" + url.Values{"chat_room_id": {group}}.Encode()
	}
	if err := account.call(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, 0, err
	}
	var id int64
	if value := raw["id"]; len(value) != 0 {
		if err := json.Unmarshal(value, &id); err != nil || id < 0 {
			return nil, 0, errors.New("机器人返回了无效的设置记录 ID")
		}
	}
	if group == "" {
		return raw, id, nil
	}
	if id == 0 {
		global, _, err := wxReadNoticeSettings(ctx, account, "")
		return global, 0, err
	}
	var owner string
	if json.Unmarshal(raw["chat_room_id"], &owner) != nil || owner != group {
		return nil, 0, errors.New("群聊设置与当前选中的群不匹配")
	}
	return raw, id, nil
}

func wxDecodeNoticeInput(reader io.Reader, extended bool) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(reader)
	var raw map[string]json.RawMessage
	if decoder.Decode(&raw) != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		return nil, errors.New("通告设置格式无效")
	}
	values := wxNoticeFields(raw, true)
	if len(values) != len(raw) {
		return nil, errors.New("通告设置包含未知字段")
	}
	for _, prefix := range []string{"welcome_", wxLeaveNoticePrefix} {
		var enabled *bool
		if json.Unmarshal(values[prefix+"enabled"], &enabled) != nil || enabled == nil {
			return nil, errors.New("请设置进群通告和退群通告的开关")
		}
		for _, suffix := range wxNoticeSuffixes[1:] {
			key := prefix + suffix
			value, ok := values[key]
			if !ok {
				continue
			}
			if !extended && wxExtendedNoticeField(key) {
				return nil, errors.New("请升级微信机器人客户端以支持更多通告格式")
			}
			if suffix == "emoji_len" {
				var size *int64
				if json.Unmarshal(value, &size) != nil || size == nil || *size <= 0 || *size > math.MaxInt32 {
					return nil, errors.New("请填写有效的表情包长度")
				}
				continue
			}
			var content *string
			if json.Unmarshal(value, &content) != nil || content == nil {
				return nil, errors.New("通告设置包含无效文本")
			}
			normalized := strings.TrimSpace(*content)
			if suffix == "type" && normalized != "text" && normalized != "emoji" && normalized != "image" && normalized != "url" {
				return nil, errors.New("通告格式无效")
			}
			limit := 255
			if suffix == "emoji_md5" {
				limit = 64
			}
			if utf8.RuneCountInString(normalized) > limit {
				return nil, fmt.Errorf("通告字段最多 %d 个字符", limit)
			}
			if strings.HasSuffix(suffix, "url") && normalized != "" {
				address, err := url.Parse(normalized)
				if err != nil || address.Hostname() == "" || (address.Scheme != "http" && address.Scheme != "https") {
					return nil, errors.New("请填写有效的 HTTP 或 HTTPS 地址")
				}
			}
			values[key], _ = json.Marshal(normalized)
		}
	}
	// Older settings pages sent welcome_text on its own to change to text.
	if values["welcome_text"] != nil && values["welcome_type"] == nil {
		values["welcome_type"] = json.RawMessage(`"text"`)
	}
	return values, nil
}

func wxGroupNoticePatch(group string, id int64, current, input map[string]json.RawMessage, extended bool) (map[string]any, error) {
	patch := make(map[string]any)
	effective := wxNoticeFields(current, extended)
	for key, value := range input {
		effective[key] = value
		patch[key] = value
	}
	for _, prefix := range []string{"welcome_", wxLeaveNoticePrefix} {
		if string(effective[prefix+"enabled"]) != "true" {
			continue
		}
		key := prefix + "type"
		var format string
		_ = json.Unmarshal(effective[key], &format)
		if format == "" {
			format = "text"
			effective[key] = json.RawMessage(`"text"`)
		}
		required := []string{}
		switch format {
		case "text":
			required = []string{"text"}
		case "emoji":
			required = []string{"emoji_md5", "emoji_len"}
		case "image":
			required = []string{"image_url"}
		case "url":
			required = []string{"url"}
			if prefix == "welcome_" {
				required = append(required, "text")
			}
		default:
			return nil, errors.New("通告格式无效")
		}
		if prefix == "welcome_" || extended {
			patch[key] = effective[key]
		}
		for _, suffix := range required {
			key := prefix + suffix
			switch string(effective[key]) {
			case "", "null", `""`, "0":
				return nil, errors.New("请填写已启用通告的必填内容")
			}
			patch[key] = effective[key]
		}
	}
	if id != 0 {
		patch["id"] = id
	}
	if group != "" {
		patch["chat_room_id"] = group
	}
	if group != "" && id == 0 {
		for key, value := range effective {
			if _, sent := patch[key]; !sent && string(value) != "null" {
				patch[key] = value
			}
		}
		patch["short_video_parsing_enabled"] = false
		patch["wxhb_notify_member_list"] = ""
		for _, key := range []string{"chat_ai_enabled", "image_ai_enabled", "tts_enabled", "max_completion_tokens", "pat_enabled", "pat_type", "pat_text", "pat_voice_timbre"} {
			if value := current[key]; len(value) != 0 && string(value) != "null" {
				patch[key] = value
			}
		}
	}
	return patch, nil
}

func (h *Host) wxGroupNotices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	group := r.URL.Query().Get("group")
	global := r.URL.Query().Get("scope") == "global"
	if (global && group != "") || (!global && (len(group) > 64 ||
		!strings.HasSuffix(group, "@chatroom") || len(group) == len("@chatroom") ||
		strings.ContainsAny(group, "\r\n\t \x00"))) {
		failure(w, errors.New("群聊 ID 无效"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 24*time.Second)
	defer cancel()
	account, err := h.wxRequestedAccount(ctx, r)
	if err != nil {
		failure(w, err)
		return
	}
	extended := account.Capabilities.GroupNoticeFormats
	var input map[string]json.RawMessage
	if r.Method == http.MethodPost {
		input, err = wxDecodeNoticeInput(http.MaxBytesReader(w, r.Body, 8192), extended)
		if err != nil {
			failure(w, err)
			return
		}
		wxGroupNoticeSaveMu.Lock()
		defer wxGroupNoticeSaveMu.Unlock()
	}
	current, id, err := wxReadNoticeSettings(ctx, account, group)
	if err != nil {
		failure(w, err)
		return
	}
	if r.Method == http.MethodPost {
		path := "chat-room-settings"
		if global {
			if id == 0 {
				failure(w, errors.New("机器人全局设置尚未初始化"))
				return
			}
			path = "global-settings"
		}
		patch, err := wxGroupNoticePatch(group, id, current, input, extended)
		if err != nil {
			failure(w, err)
			return
		}
		if err := account.call(ctx, http.MethodPost, path, patch, nil); err != nil {
			failure(w, err)
			return
		}
		current, id, err = wxReadNoticeSettings(ctx, account, group)
		if err != nil {
			failure(w, errors.New("通告已保存，读取最新设置失败，请刷新后检查"))
			return
		}
	}
	result := wxNoticeFields(current, extended)
	result["extended_formats"], _ = json.Marshal(extended)
	result["inherited"], _ = json.Marshal(!global && id == 0)
	for _, key := range []string{"welcome_enabled", wxLeaveNoticePrefix + "enabled"} {
		if value := string(result[key]); value == "" || value == "null" {
			result[key] = json.RawMessage("false")
		}
	}
	respond(w, http.StatusOK, result)
}
