package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Agent/internal/release"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/CyL-Cly/CodexRM-Server/server"
	"github.com/gorilla/websocket"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Use the real browser assets from the pinned Server module, not a second checkout.
var resolveServerModule = sync.OnceValues(func() (string, error) {
	dir, err := exec.Command("go", "list", "-mod=readonly", "-m", "-f", "{{.Dir}}", "github.com/CyL-Cly/CodexRM-Server").Output()
	return strings.TrimSpace(string(dir)), err
})

func serverWebDir(t *testing.T) string {
	t.Helper()
	dir, err := resolveServerModule()
	if err != nil || dir == "" {
		t.Fatalf("resolve Server module assets: %v", err)
	}
	return filepath.Join(dir, "web")
}

func newServer(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	s, e := server.New(server.Config{DataDir: t.TempDir(), WebDir: serverWebDir(t), AdminToken: "test-key"})
	if e != nil {
		t.Fatal(e)
	}
	h := httptest.NewServer(s.Handler())
	t.Cleanup(func() { s.Close(); h.Close() })
	return s, h
}
func post(t *testing.T, h *httptest.Server, path, token, key string, v any, out any) int {
	t.Helper()
	b, _ := json.Marshal(v)
	r, _ := http.NewRequest("POST", h.URL+path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Enrollment-Key", key)
	resp, e := http.DefaultClient.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	if out != nil {
		if e = json.NewDecoder(resp.Body).Decode(out); e != nil {
			t.Fatal(e)
		}
	}
	return resp.StatusCode
}
func ws(t *testing.T, h *httptest.Server, role, token string) *websocket.Conn {
	t.Helper()
	c, _, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http")+"/api/v1/"+role+"/ws", http.Header{"Authorization": []string{"Bearer " + token}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func read(t *testing.T, c *websocket.Conn) protocol.Envelope {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(45 * time.Second))
	var env protocol.Envelope
	if e := c.ReadJSON(&env); e != nil {
		t.Fatal(e)
	}
	return env
}
func write(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	if e := c.WriteJSON(v); e != nil {
		t.Fatal(e)
	}
}
func rpc(t *testing.T, c *websocket.Conn, id int, method string, p any) {
	t.Helper()
	write(t, c, protocol.Envelope{Type: protocol.TypeClientMessage, Message: raw(map[string]any{"id": id, "method": method, "params": p})})
}
func raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func until(t *testing.T, c *websocket.Conn, predicate func(protocol.Envelope) bool) protocol.Envelope {
	t.Helper()
	for i := 0; i < 200; i++ {
		e := read(t, c)
		if predicate(e) {
			return e
		}
	}
	t.Fatal("expected event not received")
	return protocol.Envelope{}
}
func response(t *testing.T, c *websocket.Conn, id int) map[string]any {
	t.Helper()
	e := until(t, c, func(e protocol.Envelope) bool {
		var m map[string]any
		_ = json.Unmarshal(e.Message, &m)
		return m["id"] == float64(id)
	})
	var m map[string]any
	_ = json.Unmarshal(e.Message, &m)
	if m["error"] != nil {
		t.Fatalf("RPC error: %s", e.Message)
	}
	return m["result"].(map[string]any)
}

func TestAuthOriginsAndDiskUI(t *testing.T) {
	_, h := newServer(t)
	resp, e := http.Get(h.URL + "/")
	if e != nil {
		t.Fatal(e)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatal(resp.Status)
	}
	resp, e = http.Get(h.URL + "/api/v1/agents")
	if e != nil {
		t.Fatal(e)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("agent listing must require auth")
	}
	req := protocol.EnrollRequest{Name: "test", InstallationID: "known-install"}
	var enrolled protocol.EnrollResponse
	if status := post(t, h, "/api/v1/agent/enroll", "", "", req, nil); status != 401 {
		t.Fatalf("enroll without key: %d", status)
	}
	if status := post(t, h, "/api/v1/agent/enroll", "", "test-key", req, &enrolled); status != 200 {
		t.Fatalf("enroll: %d", status)
	}
	if status := post(t, h, "/api/v1/agent/enroll", "", "test-key", req, nil); status != 401 {
		t.Fatalf("installation takeover accepted: %d", status)
	}
	r, _ := http.NewRequest("POST", h.URL+"/api/v1/client/claim", strings.NewReader(`{"code":"x"}`))
	r.Header.Set("Origin", "https://evil.invalid")
	resp, e = http.DefaultClient.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("cross-origin claim allowed")
	}
	var pair protocol.PairStartResponse
	post(t, h, "/api/v1/agent/pair", enrolled.Token, "", map[string]any{}, &pair)
	var claimed protocol.PairClaimResponse
	if status := post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.ManualPairingCode}, &claimed); status != 200 {
		t.Fatal(status)
	}
	if status := post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.PairingCode}, nil); status != 409 {
		t.Fatalf("pair reused via other alias: %d", status)
	}
	c := ws(t, h, "client", claimed.Token)
	_ = read(t, c)
	// Browser authentication stays out of URL/access logs.
	dialer := websocket.Dialer{Subprotocols: []string{"codexrm", "codexrm.token." + claimed.Token}}
	browser, _, e := dialer.Dial("ws"+strings.TrimPrefix(h.URL, "http")+"/api/v1/client/ws", nil)
	if e != nil {
		t.Fatal(e)
	}
	defer browser.Close()
	if browser.Subprotocol() != "codexrm" {
		t.Fatal("browser subprotocol not negotiated")
	}
	_ = read(t, browser)
	if status := post(t, h, "/api/v1/client/revoke", claimed.Token, "", map[string]any{}, nil); status != 200 {
		t.Fatal(status)
	}
	r, _ = http.NewRequest("GET", h.URL+"/api/v1/agents", nil)
	r.Header.Set("Authorization", "Bearer "+claimed.Token)
	resp, e = http.DefaultClient.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("revoked client still authenticated")
	}
}

func TestAgentDeviceManagement(t *testing.T) {
	_, h := newServer(t)
	var enrolled protocol.EnrollResponse
	post(t, h, "/api/v1/agent/enroll", "", "test-key", protocol.EnrollRequest{Name: "host", InstallationID: "device-management"}, &enrolled)
	var pair protocol.PairStartResponse
	post(t, h, "/api/v1/agent/pair", enrolled.Token, "", map[string]any{}, &pair)
	var claimed protocol.PairClaimResponse
	post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.ManualPairingCode, ClientName: "phone"}, &claimed)
	if claimed.ExpiresAt != 0 {
		t.Fatalf("authorization has an expiry: %d", claimed.ExpiresAt)
	}
	r, _ := http.NewRequest("GET", h.URL+"/api/v1/agent/clients", nil)
	r.Header.Set("Authorization", "Bearer "+enrolled.Token)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Data []protocol.ClientInfo `json:"data"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || len(listed.Data) != 1 || listed.Data[0].ClientName != "phone" {
		t.Fatalf("unexpected device list: %d %+v", resp.StatusCode, listed.Data)
	}
	r, _ = http.NewRequest("DELETE", h.URL+"/api/v1/agent/clients/"+claimed.ClientID, nil)
	r.Header.Set("Authorization", "Bearer "+enrolled.Token)
	resp, err = http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("delete authorization: %d", resp.StatusCode)
	}
	r, _ = http.NewRequest("GET", h.URL+"/api/v1/agents", nil)
	r.Header.Set("Authorization", "Bearer "+claimed.Token)
	resp, err = http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("deleted client still authenticated: %d", resp.StatusCode)
	}
}

func TestDiskWebChangesDoNotRequireServerRestart(t *testing.T) {
	webDir := t.TempDir()
	index := filepath.Join(webDir, "index.html")
	if err := os.WriteFile(index, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := server.New(server.Config{DataDir: t.TempDir(), WebDir: webDir})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewTLSServer(s.Handler())
	t.Cleanup(func() { s.Close(); h.Close() })
	readIndex := func() string {
		resp, err := h.Client().Get(h.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("index status: %s", resp.Status)
		}
		return string(body)
	}
	if got := readIndex(); got != "before" {
		t.Fatalf("initial index: %q", got)
	}
	if err := os.WriteFile(index, []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readIndex(); got != "after" {
		t.Fatalf("index after disk update: %q", got)
	}
}

func TestPWAAssetsAndCacheHeaders(t *testing.T) {
	s, _ := newServer(t)
	h := httptest.NewTLSServer(s.Handler())
	t.Cleanup(h.Close)
	for _, path := range []string{"/manifest.webmanifest", "/sw.js", "/pwa.js?v=0.6.0", "/icons/icon-192.png", "/icons/icon-512.png", "/icons/maskable-512.png", "/icons/icon.svg"} {
		resp, err := h.Client().Get(h.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %s", path, resp.Status)
		}
	}

	resp, err := h.Client().Get(h.URL + "/manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "application/manifest+json; charset=utf-8" {
		t.Fatalf("manifest Content-Type: %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("manifest Cache-Control: %q", got)
	}

	resp, err = h.Client().Get(h.URL + "/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := resp.Header.Get("Service-Worker-Allowed"); got != "/" {
		t.Fatalf("Service-Worker-Allowed: %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("service worker Cache-Control: %q", got)
	}

	for _, path := range []string{"/app.js", "/styles.css", "/dsh-theme.css", "/pwa.js", "/theme.js"} {
		resp, err = h.Client().Get(h.URL + path + "?v=20261118-1")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %s", path, resp.Status)
		}
		if got := resp.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Fatalf("%s versioned asset Cache-Control: %q", path, got)
		}
	}
	resp, err = h.Client().Get(h.URL + "/app.js?v=old")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); strings.Contains(got, "immutable") {
		t.Fatalf("old asset URL must not be immutable: %q", got)
	}
	resp, err = h.Client().Get(h.URL + "/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("API Cache-Control: %q", got)
	}
}

func TestAgentMockConcurrentRPCApprovalAndInterrupt(t *testing.T) {
	_, h := newServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, e := agent.New(agent.Config{ServerURL: h.URL, DataDir: t.TempDir(), Cwd: t.TempDir(), Mock: true, EnrollmentKey: "test-key"})
	if e != nil {
		t.Fatal(e)
	}
	pair, e := a.Pair(ctx)
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Agent did not stop")
		}
	})
	var claimed protocol.PairClaimResponse
	post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.ManualPairingCode}, &claimed)
	c := ws(t, h, "client", claimed.Token)
	until(t, c, func(e protocol.Envelope) bool { return e.Status == "connected" })
	rpc(t, c, 1, "thread/start", map[string]any{})
	thread := response(t, c, 1)["thread"].(map[string]any)
	tid := thread["id"]
	rpc(t, c, 2, "turn/start", map[string]any{"threadId": tid, "input": []any{map[string]any{"type": "text", "text": "审批"}}})
	var approvalID any
	gotResponse := false
	for approvalID == nil || !gotResponse {
		env := read(t, c)
		var m map[string]any
		_ = json.Unmarshal(env.Message, &m)
		if m["id"] == float64(2) {
			if m["error"] != nil {
				t.Fatal(m)
			}
			gotResponse = true
		}
		if m["method"] == "item/commandExecution/requestApproval" {
			approvalID = m["id"]
		}
	}
	// A second call must complete while the turn is blocked awaiting user approval.
	rpc(t, c, 3, "thread/list", map[string]any{})
	if len(response(t, c, 3)["data"].([]any)) != 1 {
		t.Fatal("thread list during approval failed")
	}
	write(t, c, protocol.Envelope{Type: protocol.TypeClientMessage, Message: raw(map[string]any{"id": approvalID, "result": map[string]any{"decision": "accept"}})})
	completed := until(t, c, func(e protocol.Envelope) bool {
		var m map[string]any
		_ = json.Unmarshal(e.Message, &m)
		return m["method"] == "turn/completed"
	})
	if !strings.Contains(string(completed.Message), "accept") {
		t.Fatalf("approval response not forwarded: %s", completed.Message)
	}
	rpc(t, c, 4, "thread/resume", map[string]any{"threadId": tid})
	history := response(t, c, 4)["thread"].(map[string]any)["turns"].([]any)
	if len(history) != 1 {
		t.Fatalf("history missing: %v", history)
	}
	rpc(t, c, 5, "turn/start", map[string]any{"threadId": tid, "input": []any{map[string]any{"type": "text", "text": "long reply"}}})
	turnID := response(t, c, 5)["turn"].(map[string]any)["id"]
	rpc(t, c, 6, "turn/interrupt", map[string]any{"threadId": tid, "turnId": turnID})
	env := until(t, c, func(e protocol.Envelope) bool {
		var m map[string]any
		_ = json.Unmarshal(e.Message, &m)
		return m["method"] == "turn/completed"
	})
	if !strings.Contains(string(env.Message), "interrupted") {
		t.Fatalf("turn not interrupted: %s", env.Message)
	}
}

func TestRelayTenantIsolationAndReplacement(t *testing.T) {
	_, h := newServer(t)
	enroll := func(installation string) (protocol.EnrollResponse, protocol.PairClaimResponse) {
		var a protocol.EnrollResponse
		post(t, h, "/api/v1/agent/enroll", "", "test-key", protocol.EnrollRequest{InstallationID: installation}, &a)
		var p protocol.PairStartResponse
		post(t, h, "/api/v1/agent/pair", a.Token, "", map[string]any{}, &p)
		var c protocol.PairClaimResponse
		post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: p.ManualPairingCode}, &c)
		return a, c
	}
	a1, c1 := enroll("a1")
	a2, c2 := enroll("a2")
	agent1 := ws(t, h, "agent", a1.Token)
	agent2 := ws(t, h, "agent", a2.Token)
	_ = agent2
	client1 := ws(t, h, "client", c1.Token)
	client2 := ws(t, h, "client", c2.Token)
	_ = read(t, client1)
	_ = read(t, client2)
	replacement := ws(t, h, "agent", a1.Token)
	// Wait for the replacement's own status; the old read loop must not mark it offline.
	until(t, client1, func(e protocol.Envelope) bool { return e.Status == "connected" })
	rpc(t, client1, 1, "thread/list", map[string]any{})
	env := read(t, replacement)
	if env.ClientID != c1.ClientID {
		t.Fatal("wrong RPC destination")
	}
	_ = agent1.Close()
	write(t, replacement, protocol.Envelope{Type: protocol.TypeServerMessage, ClientID: c2.ClientID, Message: raw(map[string]any{"method": "tenant-leak"})})
	write(t, client2, protocol.Envelope{Type: protocol.TypePing, PingID: "isolated"})
	for i := 0; i < 5; i++ {
		env = read(t, client2)
		if strings.Contains(string(env.Message), "tenant-leak") {
			t.Fatal("cross-tenant event leak")
		}
		if env.Type == protocol.TypePong {
			break
		}
	}
}

func TestAgentConnectionRefreshesAppVersion(t *testing.T) {
	_, h := newServer(t)
	var enrolled protocol.EnrollResponse
	post(t, h, "/api/v1/agent/enroll", "", "test-key", protocol.EnrollRequest{Name: "host", AppVersion: "0.2.0", InstallationID: "stale-version"}, &enrolled)
	connect := func(version string) {
		t.Helper()
		header := http.Header{"Authorization": []string{"Bearer " + enrolled.Token}}
		if version != "" {
			header[protocol.AppVersionHeader] = []string{version}
		}
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.URL, "http")+"/api/v1/agent/ws", header)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
	}
	versionOf := func() string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, h.URL+"/api/v1/agents", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var listed struct {
			Data []protocol.AgentInfo `json:"data"`
		}
		if err = json.NewDecoder(resp.Body).Decode(&listed); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || len(listed.Data) != 1 {
			t.Fatalf("agent list: %d %+v", resp.StatusCode, listed.Data)
		}
		return listed.Data[0].AppVersion
	}
	connect("")
	if got := versionOf(); got != "0.2.0" {
		t.Fatalf("version without a report: %q", got)
	}
	connect("   ")
	connect(strings.Repeat("v", 65))
	if got := versionOf(); got != "0.2.0" {
		t.Fatalf("invalid report changed version to %q", got)
	}
	connect(" " + release.VersionName + " ")
	if got := versionOf(); got != release.VersionName {
		t.Fatalf("version = %q, want %q", got, release.VersionName)
	}
}
