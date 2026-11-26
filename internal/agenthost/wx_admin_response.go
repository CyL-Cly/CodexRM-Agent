package agenthost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

func (c *wxAdminClient) request(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.apiToken != "" {
		req.Header.Set("X-API-Token", c.apiToken)
	}
	for _, cookie := range c.cookies {
		req.AddCookie(cookie)
	}
	client := *wxTransport
	client.Timeout = 0
	return client.Do(req)
}

func (c *wxAdminClient) wxRawOperation(ctx context.Context, w http.ResponseWriter, operation wxOperation, path string, input json.RawMessage) {
	var body io.Reader
	contentType := ""
	if operation.Method == http.MethodPost {
		var data struct{ Symbols string }
		if json.Unmarshal(input, &data) != nil {
			failure(w, errors.New("符号参数无效"))
			return
		}
		body = strings.NewReader(data.Symbols)
		contentType = "application/x-www-form-urlencoded"
	}
	resp, err := c.request(ctx, operation.Method, path, body, contentType)
	if err != nil {
		failure(w, errors.New("无法访问 wxbot 管理接口"))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		failure(w, errors.New("wxbot 管理接口请求失败"))
		return
	}
	if operation.Stream {
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			failure(w, errors.New("镜像拉取未返回进度"))
			return
		}
		rc := http.NewResponseController(w)
		_ = rc.SetWriteDeadline(time.Now().Add(15 * time.Minute))
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		buf := make([]byte, 8192)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, writeErr := w.Write(buf[:n]); writeErr != nil || rc.Flush() != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	const max = 32 << 20
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Minute))
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil || len(data) > max {
		failure(w, errors.New("读取结果失败或数据超过限制"))
		return
	}
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		var envelope struct {
			Code    int
			Message string
		}
		if json.Unmarshal(data, &envelope) == nil && envelope.Code != 0 && envelope.Code != 200 {
			failure(w, errors.New(envelope.Message))
			return
		}
	}
	switch operation.ID {
	case "get-wx-admin-probe":
		if !json.Valid(data) {
			failure(w, errors.New("后台探测响应无效"))
			return
		}
		respond(w, 200, json.RawMessage(data))
	case "get-wx-admin-oauth-qr":
		mime := http.DetectContentType(data)
		if (mime != "image/png" && mime != "image/jpeg" && mime != "image/gif") || len(data) > 1<<20 {
			failure(w, errors.New("后台授权二维码无效"))
			return
		}
		respond(w, 200, map[string]string{"image": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)})
	default:
		name := "wxbot-profile.txt"
		if operation.ID == "get-wx-admin-swagger" {
			name = "wxbot-openapi.json"
		} else if !strings.Contains(resp.Header.Get("Content-Type"), "text/") {
			name = "wxbot-profile.pb.gz"
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename="+name)
		_, _ = w.Write(data)
	}
}
