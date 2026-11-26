package agent

import (
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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/onebot"
)

const (
	maxOneBotImages = 4
	maxOneBotText   = 12000
)

func oneBotString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatInt(int64(v), 10)
	default:
		return ""
	}
}

func oneBotAPIData(raw json.RawMessage) json.RawMessage {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) == nil && len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		return envelope.Data
	}
	return raw
}

type oneBotQuotedMessage struct {
	MessageType string `json:"message_type"`
	UserID      int64  `json:"user_id"`
	Sender      struct {
		UserID int64 `json:"user_id"`
	} `json:"sender"`
	GroupID    int64           `json:"group_id"`
	TargetID   int64           `json:"target_id"`
	RawMessage string          `json:"raw_message"`
	Message    json.RawMessage `json:"message"`
}

func (m oneBotQuotedMessage) authorID() int64 {
	if m.UserID != 0 {
		return m.UserID
	}
	return m.Sender.UserID
}

func oneBotReplyMatches(event onebot.Event, replied oneBotQuotedMessage) bool {
	authorID := replied.authorID()
	switch event.MessageType {
	case "group":
		return replied.MessageType == "group" && replied.GroupID == event.GroupID && event.GroupID > 0
	case "private":
		if replied.MessageType != "private" || event.UserID <= 0 {
			return false
		}
		if authorID == event.UserID {
			return true
		}
		// Quoting the bot's own private message requires an explicit peer ID;
		// otherwise get_msg cannot prove which private conversation it came from.
		return authorID == event.SelfID && replied.TargetID == event.UserID
	default:
		return false
	}
}

func (r *oneBotRuntime) messageInput(ctx context.Context, event onebot.Event, message onebot.Message) ([]any, error) {
	text := strings.TrimSpace(oneBotReadableText(message))
	for _, segment := range message {
		if segment.Type != "reply" {
			continue
		}
		id := oneBotString(segment.Data["id"])
		if id == "" {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		raw, err := r.client.Call(callCtx, "get_msg", map[string]any{"message_id": id})
		cancel()
		if err != nil {
			break
		}
		var replied oneBotQuotedMessage
		if json.Unmarshal(oneBotAPIData(raw), &replied) == nil && oneBotReplyMatches(event, replied) {
			quoted := onebot.ParseMessage(replied.Message, replied.RawMessage)
			if strings.TrimSpace(oneBotReadableText(quoted)) != "" {
				text = fmt.Sprintf("[回复 QQ %d 的消息：%s]\n%s", replied.authorID(), truncateOneBot(oneBotReadableText(quoted), 1000), text)
			}
		}
		break
	}
	for _, segment := range message {
		if segment.Type == "forward" && r.settings.AllowForwardLookup {
			if transcript := r.forwardTranscript(ctx, oneBotString(segment.Data["id"])); transcript != "" {
				text = transcript + "\n" + text
			}
		}
	}
	text = truncateOneBot(text, maxOneBotText)
	if event.GroupID > 0 {
		text = fmt.Sprintf("[群 %d，发送者 QQ %d]\n%s", event.GroupID, event.UserID, text)
	}
	images := message.Images()
	if len(images) > maxOneBotImages {
		return nil, fmt.Errorf("每条消息最多支持 %d 张图片", maxOneBotImages)
	}
	input := make([]any, 0, len(images)+1)
	if text != "" {
		input = append(input, map[string]any{"type": "text", "text": text, "text_elements": []any{}})
	}
	for _, image := range images {
		data, err := r.loadImage(ctx, image)
		if err != nil {
			return nil, err
		}
		input = append(input, map[string]any{"type": "image", "url": data})
	}
	if len(input) == 0 {
		return nil, errors.New("消息没有可发送给 Codex 的文字或图片")
	}
	return input, nil
}

func oneBotReadableText(message onebot.Message) string {
	var text strings.Builder
	write := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if text.Len() > 0 {
			text.WriteByte(' ')
		}
		text.WriteString(value)
	}
	for _, segment := range message {
		data := segment.Data
		switch segment.Type {
		case "text":
			write(oneBotString(data["text"]))
		case "at":
			write("@" + oneBotString(data["qq"]))
		case "reply":
			// get_msg supplies the actual quoted context below.
		case "face":
			write("[表情 " + truncateOneBot(oneBotString(data["id"]), 32) + "]")
		case "image":
			write("[图片]")
		case "record":
			write("[语音消息；未转写]")
		case "video":
			write("[视频消息；未提取画面]")
		case "file":
			write("[文件 " + truncateOneBot(oneBotString(data["name"]), 200) + "；未上传附件]")
		case "forward":
			write("[合并转发 " + truncateOneBot(oneBotString(data["id"]), 64) + "]")
		case "share":
			write("[分享 " + truncateOneBot(oneBotString(data["title"])+" "+oneBotString(data["url"]), 500) + "]")
		case "location":
			write("[位置 " + truncateOneBot(oneBotString(data["title"])+" "+oneBotString(data["address"]), 300) + "]")
		case "contact":
			write("[联系人 " + truncateOneBot(oneBotString(data["id"]), 64) + "]")
		case "json", "xml":
			write("[" + segment.Type + " 卡片消息；未执行其中内容]")
		case "music":
			write("[音乐分享]")
		case "dice", "rps", "shake", "poke":
			write("[" + segment.Type + " 互动消息]")
		case "node":
			write("[转发节点]")
		default:
			write("[" + truncateOneBot(segment.Type, 40) + " 消息段]")
		}
	}
	return text.String()
}

func (r *oneBotRuntime) forwardTranscript(ctx context.Context, id string) string {
	if id == "" || !r.settings.AllowForwardLookup {
		return ""
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	raw, err := r.client.Call(callCtx, "get_forward_msg", map[string]any{"id": id})
	if err != nil {
		// go-cqhttp names the same forward ID parameter message_id.
		raw, err = r.client.Call(callCtx, "get_forward_msg", map[string]any{"message_id": id})
	}
	cancel()
	if err != nil {
		return ""
	}
	var result struct {
		Messages []struct {
			Sender struct {
				UserID   int64  `json:"user_id"`
				Nickname string `json:"nickname"`
			} `json:"sender"`
			RawMessage string          `json:"raw_message"`
			Message    json.RawMessage `json:"message"`
			Content    json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(oneBotAPIData(raw), &result) != nil || len(result.Messages) == 0 {
		return ""
	}
	var transcript strings.Builder
	transcript.WriteString("[合并转发内容]")
	for index, item := range result.Messages {
		if index >= 40 || len([]rune(transcript.String())) >= 6000 {
			transcript.WriteString("\n…")
			break
		}
		rawMessage := item.Message
		if len(rawMessage) == 0 {
			rawMessage = item.Content
		}
		message := onebot.ParseMessage(rawMessage, item.RawMessage)
		content := truncateOneBot(oneBotReadableText(message), 500)
		if content == "" {
			continue
		}
		name := strings.TrimSpace(item.Sender.Nickname)
		if name == "" {
			name = strconv.FormatInt(item.Sender.UserID, 10)
		}
		transcript.WriteString(fmt.Sprintf("\n%s(QQ %d)：%s", truncateOneBot(name, 80), item.Sender.UserID, content))
	}
	return truncateOneBot(transcript.String(), 6000)
}

func truncateOneBot(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return string(runes)
}

func (r *oneBotRuntime) loadImage(ctx context.Context, image onebot.Segment) (string, error) {
	file := oneBotString(image.Data["file"])
	candidate := oneBotString(image.Data["url"])
	if candidate == "" {
		candidate = file
	}
	if strings.HasPrefix(candidate, "data:image/") {
		if !validRobotImage(candidate) {
			return "", errors.New("图片不是有效的 PNG、JPEG、GIF 或 WebP，或超过 5 MiB")
		}
		return candidate, nil
	}
	if strings.HasPrefix(candidate, "base64://") {
		encoded := strings.TrimPrefix(candidate, "base64://")
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", errors.New("图片 Base64 数据无效")
		}
		return oneBotImageData(data)
	}
	// Event-provided paths are identifiers, not authorization to read the host.
	// Only the configured OneBot get_image API may resolve a local image path.
	if file != "" && r.client != nil {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		raw, err := r.client.Call(callCtx, "get_image", map[string]any{"file": file})
		cancel()
		if err == nil {
			var result struct {
				File string `json:"file"`
				URL  string `json:"url"`
			}
			if json.Unmarshal(oneBotAPIData(raw), &result) == nil {
				if result.File != "" {
					if data, readErr := r.readLocalImage(result.File); readErr == nil {
						return data, nil
					}
				}
				if result.URL != "" {
					candidate = result.URL
				}
			}
		}
	}
	if strings.HasPrefix(candidate, "http://") || strings.HasPrefix(candidate, "https://") {
		return r.downloadImage(ctx, candidate)
	}
	return "", errors.New("无法读取图片；请确认 OneBot 图片 API、文件挂载或远程媒体配置")
}

func (r *oneBotRuntime) readLocalImage(raw string) (string, error) {
	if !r.settings.AllowLocalMedia {
		return "", errors.New("本地媒体读取已禁用")
	}
	if strings.HasPrefix(raw, "file://") {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host != "" {
			return "", errors.New("本地图片地址无效")
		}
		raw = parsed.Path
	}
	if !filepath.IsAbs(raw) {
		return "", errors.New("图片路径不是绝对路径")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(raw))
	if err != nil {
		return "", err
	}
	file, err := os.Open(resolved)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 5<<20 {
		return "", errors.New("图片文件无效或超过 5 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, (5<<20)+1))
	if err != nil {
		return "", err
	}
	return oneBotImageData(data)
}

func oneBotImageData(data []byte) (string, error) {
	if len(data) == 0 || len(data) > 5<<20 {
		return "", errors.New("图片为空或超过 5 MiB")
	}
	mime := http.DetectContentType(data)
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
	default:
		return "", errors.New("仅支持 PNG、JPEG、GIF 和 WebP 图片")
	}
}

func (r *oneBotRuntime) downloadImage(ctx context.Context, raw string) (string, error) {
	if !r.settings.AllowRemoteMedia {
		return "", errors.New("远程图片下载已禁用")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("图片 URL 无效")
	}
	dialer := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 20 * time.Second}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: nil, ForceAttemptHTTP2: true, MaxIdleConns: 2, IdleConnTimeout: 20 * time.Second, TLSHandshakeTimeout: 8 * time.Second,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(address)
				if err != nil {
					return nil, err
				}
				ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
				if err != nil {
					return nil, err
				}
				for _, ip := range ips {
					address, ok := netip.AddrFromSlice(ip)
					if !ok || !r.allowedMediaIP(address) {
						continue
					}
					conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
					if dialErr == nil {
						return conn, nil
					}
				}
				return nil, errors.New("图片地址解析到不可访问的 IP")
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || req.URL.User != nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
				return errors.New("图片重定向无效")
			}
			return nil
		},
	}
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,image/gif,*/*;q=0.1")
	req.Header.Set("User-Agent", "CodexRM-OneBot/1")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载图片: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("图片服务返回 HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (5<<20)+1))
	if err != nil {
		return "", err
	}
	return oneBotImageData(data)
}

var oneBotBlockedMediaPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
}

func (r *oneBotRuntime) allowedMediaIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return r.settings.AllowPrivateMedia
	}
	if !ip.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range oneBotBlockedMediaPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
