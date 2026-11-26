package agenthost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const wxUpstreamSliderSubmit = "/api/v1/robot/login/slider-verify-submit"
const wxSliderMaxHTML = 512 << 10

type wxSliderChallenge struct {
	id   string
	html []byte
}

func fetchWXSlider(ctx context.Context, client *wxAdminClient, robotID int64, scan wxLoginScan) ([]byte, error) {
	if scan.Data62 == "" || scan.Ticket == "" {
		return nil, errors.New("微信未要求滑块验证或验证已过期")
	}
	query := url.Values{"id": {strconv.FormatInt(robotID, 10)}, "data62": {scan.Data62}, "ticket": {scan.Ticket}}
	resp, err := client.request(ctx, http.MethodGet, "/robot/login/slider?"+query.Encode(), nil, "")
	if err != nil {
		return nil, errors.New("无法连接 wxbot 滑块服务")
	}
	defer resp.Body.Close()
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	html, err := io.ReadAll(io.LimitReader(resp.Body, wxSliderMaxHTML+1))
	if resp.StatusCode != http.StatusOK || media != "text/html" || err != nil || len(html) == 0 || len(html) > wxSliderMaxHTML || bytes.Contains(html, []byte("<title>获取滑块失败</title>")) {
		return nil, errors.New("wxbot 未返回有效的滑块页面，请检查滑块服务和凭据，或使用微信显示的验证码")
	}
	return html, nil
}

func (h *Host) wxPrepareSlider(ctx context.Context, client *wxAdminClient, robotID int64, scanID string) (string, error) {
	scan, err := h.getWXScan(robotID, scanID)
	if err != nil {
		return "", err
	}
	var robot wxAdminRobot
	if err = client.call(ctx, http.MethodGet, "/robot/view?id="+strconv.FormatInt(robotID, 10), nil, &robot); err != nil {
		return "", err
	}
	html, err := fetchWXSlider(ctx, client, robotID, scan)
	if err != nil {
		return "", err
	}
	id := settingsSecret()
	path := "/wx-slider/" + id
	html = bytes.ReplaceAll(html, []byte(wxUpstreamSliderSubmit), []byte(path+"/submit"))
	h.wxScanMu.Lock()
	defer h.wxScanMu.Unlock()
	latest := h.wxScans[robotID]
	if latest.ID != scan.ID || latest.Ticket != scan.Ticket || time.Now().After(latest.ExpiresAt) {
		return "", errors.New("登录请求已过期，请重新获取二维码")
	}
	latest.Slider = &wxSliderChallenge{id: id, html: html}
	h.wxScans[robotID] = latest
	return path, nil
}

func (h *Host) wxFindSlider(id string) (int64, wxLoginScan) {
	h.wxScanMu.Lock()
	defer h.wxScanMu.Unlock()
	for robotID, scan := range h.wxScans {
		if scan.Slider != nil && scan.Slider.id == id && scan.Ticket != "" && time.Now().Before(scan.ExpiresAt) {
			return robotID, scan
		}
	}
	return 0, wxLoginScan{}
}

func (h *Host) wxSliderFrame(w http.ResponseWriter, r *http.Request) {
	_, scan := h.wxFindSlider(r.PathValue("challenge"))
	if scan.Slider == nil {
		http.NotFound(w, r)
		return
	}
	// The widget is supplied by wxbot and may contain executable HTML. Give it
	// an opaque origin and allow only the provider's verification resources.
	const provider = "https://*.qq.com https://*.qcloud.com"
	policy := "default-src 'none'; sandbox allow-scripts allow-forms; frame-ancestors 'self'; base-uri 'none'; " +
		"script-src 'unsafe-inline' " + provider + "; style-src 'unsafe-inline' " + provider + "; " +
		"connect-src 'self' " + provider + "; frame-src " + provider + "; " +
		"img-src data: blob: " + provider + "; form-action 'self' " + provider
	w.Header().Set("Content-Security-Policy", policy)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(scan.Slider.html)
}

func (h *Host) wxSliderSubmit(w http.ResponseWriter, r *http.Request) {
	const invalid = "无效的滑块验证请求"
	// This is the only callback available to the sandboxed document. The route
	// carries a per-scan capability, and upstream verification is bound to the
	// active device and challenge data kept in Agent memory.
	if r.Header.Get("Origin") != "null" || len(r.URL.RawQuery) > 16<<10 {
		http.Error(w, invalid, http.StatusForbidden)
		return
	}
	id, scan := h.wxFindSlider(r.PathValue("challenge"))
	if id == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "null")
	q := r.URL.Query()
	if q.Get("data") != scan.Data62 || q.Get("secticket") != scan.Ticket || q.Get("verifyid") == "" || q.Get("randstr") == "" || q.Get("ticket") == "" {
		respond(w, http.StatusBadRequest, map[string]any{"code": 400, "message": invalid})
		return
	}
	params := url.Values{"id": {strconv.FormatInt(id, 10)}}
	for _, field := range []string{"data", "verifyid", "secticket", "randstr", "ticket", "device_type", "client_version", "ret", "errorCode", "errorMessage"} {
		if value := strings.TrimSpace(q.Get(field)); value != "" {
			params.Set(field, value)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	client, err := h.wxAdmin(ctx)
	if err == nil {
		err = client.call(ctx, http.MethodGet, "/robot/login/slider-verify-submit?"+params.Encode(), nil, nil)
	}
	if err != nil {
		respond(w, 200, map[string]any{"code": 400, "message": "wxbot 滑块验证未通过"})
		return
	}
	respond(w, 200, map[string]any{"code": 200, "message": "滑块验证通过"})
}
