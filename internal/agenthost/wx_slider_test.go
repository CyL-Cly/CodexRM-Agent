package agenthost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWXSliderCapabilityIsBoundToScan(t *testing.T) {
	const scanID = "scan"
	const ticket = "test-ticket"
	const device = "test-device"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("id") != "1" || r.Header.Get("Cookie") != "session=test" {
			t.Error("upstream must be called using the selected robot and server-side credentials")
		}
		switch r.URL.Path {
		case "/robot/view":
			_, _ = w.Write([]byte(`{"code":200,"data":{"id":1}}`))
		case "/robot/login/slider":
			if query.Get("ticket") != ticket || query.Get("data62") != device {
				t.Error("widget must use the stored challenge parameters")
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<script>const submit = "` + wxUpstreamSliderSubmit + `"</script>`))
		default:
			t.Errorf("unexpected upstream route: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	client := &wxAdminClient{base: upstream.URL, cookies: []*http.Cookie{{Name: "session", Value: "test"}}}
	host := &Host{}
	scan := wxLoginScan{ID: scanID, Ticket: ticket, Data62: device, ExpiresAt: time.Now().Add(time.Minute)}
	host.saveWXScan(1, scan)
	path, err := host.wxPrepareSlider(context.Background(), client, 1, scanID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(path, ticket) || strings.Contains(path, device) || len(strings.TrimPrefix(path, "/wx-slider/")) != 43 {
		t.Fatal("the widget URL should carry only a random challenge capability")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wx-slider/{challenge}", host.wxSliderFrame)
	mux.HandleFunc("GET /wx-slider/{challenge}/submit", host.wxSliderSubmit)
	frame := httptest.NewRecorder()
	mux.ServeHTTP(frame, httptest.NewRequest(http.MethodGet, path, nil))
	policy := frame.Header().Get("Content-Security-Policy")
	if frame.Code != 200 || !strings.Contains(frame.Body.String(), path+"/submit") || !strings.Contains(policy, "sandbox allow-scripts allow-forms") || strings.Contains(policy, "allow-same-origin") {
		t.Fatal("widget with an unscoped callback or a privileged origin")
	}
	callback := httptest.NewRequest(http.MethodGet, path+"/submit?data=wrong", nil)
	callback.Header.Set("Origin", "null")
	result := httptest.NewRecorder()
	mux.ServeHTTP(result, callback)
	if result.Code != http.StatusBadRequest || result.Header().Get("Access-Control-Allow-Origin") != "null" {
		t.Fatal("the sandbox callback must reject another device")
	}
	host.updateWXScan(1, scan, true)
	stale := httptest.NewRecorder()
	mux.ServeHTTP(stale, httptest.NewRequest(http.MethodGet, path, nil))
	if stale.Code != http.StatusNotFound {
		t.Fatal("finished scan still exposes a challenge")
	}
}
