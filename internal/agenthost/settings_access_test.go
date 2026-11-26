package agenthost

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
)

func TestLANSettingsPersistAcrossHostRuns(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), ".state", "connection.json")
	accessPath := filepath.Join(filepath.Dir(configPath), settingsAccessFile)
	newHost := func() *Host {
		t.Helper()
		h, err := New(configPath, "", clientconfig.Connection{}, agent.Config{})
		if err != nil {
			t.Fatal(err)
		}
		h.LANSettings = true
		h.OpenOnStart = true
		h.openBrowser = func(string) error {
			t.Error("LAN settings must work without a local browser")
			return nil
		}
		return h
	}
	start := func() (settingsAccess, func()) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		host := newHost()
		go func() {
			defer close(done)
			if err := host.Run(ctx); err != nil {
				t.Error(err)
			}
		}()
		stop := func() { cancel(); <-done }
		t.Cleanup(stop)
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
			access, err := readSettingsAccess(accessPath)
			if err == nil {
				addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(access.Port))
				if conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
					conn.Close()
					return access, stop
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("LAN settings did not start automatically")
		return settingsAccess{}, stop
	}
	first, stop := start()
	base := "http://127.0.0.1:" + strconv.Itoa(first.Port)
	if runtime.GOOS == "linux" {
		if info, err := os.Stat(accessPath); err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("settings access file must be mode 0600: %v", err)
		}
	}
	if code, body := request(t, base, "", http.MethodGet, "/", nil); code != 200 || !bytes.Contains(body, []byte(`id="login-form"`)) || bytes.Contains(body, []byte(first.Password)) {
		t.Fatal("expected a public login page without the stored secret")
	}
	for _, token := range []string{"", rand.Text()} {
		if code, body := request(t, base, token, http.MethodGet, "/api/settings", nil); code != 401 || !bytes.Contains(body, []byte(`"passwordRequired":true`)) {
			t.Fatalf("unexpected authentication response: %d", code)
		}
		if code, _ := request(t, base, token, http.MethodPut, "/api/settings", map[string]string{"server": "http://127.0.0.1:1"}); code != 401 {
			t.Fatalf("unauthorized settings update: %d", code)
		}
	}
	for _, overwrite := range []func(*http.Request){
		func(r *http.Request) { r.Host = "settings.example:" + strconv.Itoa(first.Port) },
		func(r *http.Request) { r.Header.Set("Origin", "http://settings.example") },
	} {
		req, err := http.NewRequest(http.MethodGet, base+"/api/settings", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+first.Password)
		overwrite(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("unexpected cross-origin response: %d", resp.StatusCode)
		}
	}
	if code, _ := request(t, base, first.Password, http.MethodGet, "/api/settings", nil); code != 200 {
		t.Fatalf("saved password rejected: %d", code)
	}
	stop()
	second, stop := start()
	if first != second {
		t.Fatal("port or password changed across restart")
	}
	if code, _ := request(t, base, first.Password, http.MethodGet, "/api/settings", nil); code != 200 {
		t.Fatalf("restarted listener rejected saved password: %d", code)
	}
	stop()

	occupied, err := net.Listen("tcp", ":"+strconv.Itoa(first.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	listener, _, err := listenPasswordSettings(accessPath, "")
	if err == nil {
		listener.Close()
		t.Fatal("occupied port fell back to a different port")
	}
	if saved, err := readSettingsAccess(accessPath); err != nil || saved != first {
		t.Fatalf("bind failure changed the saved credentials: %v", err)
	}
}

func TestLoopbackSettingsKeepPasswordPortAndBind(t *testing.T) {
	path := filepath.Join(t.TempDir(), settingsAccessFile)
	listener, password, err := listenPasswordSettings(path, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().(*net.TCPAddr)
	if !addr.IP.IsLoopback() || password == "" {
		listener.Close()
		t.Fatal("password listener must bind only to loopback")
	}
	listener.Close()
	second, saved, err := listenPasswordSettings(path, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.Addr().String() != addr.String() || saved != password {
		t.Fatal("loopback endpoint changed across restart")
	}
}

func TestSettingsSessionLogin(t *testing.T) {
	password := settingsSecret()
	server := httptest.NewServer(guardSettings(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), "", password, true, true))
	defer server.Close()

	const login = "/api/settings/login"
	const settings = "/api/settings"
	if code, body := request(t, server.URL, "", http.MethodGet, settings, nil); code != http.StatusUnauthorized || !bytes.Contains(body, []byte(`"sessionLogin":true`)) {
		t.Fatalf("expected login to be offered, got %d %s", code, body)
	}
	if code, _ := request(t, server.URL, "wrong", http.MethodPost, login, nil); code != http.StatusUnauthorized {
		t.Fatalf("incorrect password returned %d", code)
	}
	before := time.Now()
	code, body := request(t, server.URL, password, http.MethodPost, login, nil)
	var result struct {
		SessionToken string `json:"sessionToken"`
	}
	if err := json.Unmarshal(body, &result); code != http.StatusOK || err != nil || result.SessionToken == "" {
		t.Fatalf("login failed with status %d, decoding error %v", code, err)
	}
	expiry, _, _ := strings.Cut(result.SessionToken, ".")
	expiresAt, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || expiresAt < before.Add(48*time.Hour).Unix() || expiresAt > time.Now().Add(48*time.Hour).Unix() {
		t.Fatal("expected a session valid for 48 hours from login")
	}
	if code, _ := request(t, server.URL, result.SessionToken, http.MethodGet, settings, nil); code != http.StatusNoContent {
		t.Fatalf("valid session returned %d", code)
	}
	if code, _ := request(t, server.URL, result.SessionToken, http.MethodPost, login, nil); code != http.StatusUnauthorized {
		t.Fatalf("a session attempted to renew itself and returned %d", code)
	}
	for _, invalid := range []string{
		signSettingsSession(password, before.Unix()),
		signSettingsSession(settingsSecret(), expiresAt),
		result.SessionToken + "x",
	} {
		if code, _ := request(t, server.URL, invalid, http.MethodGet, settings, nil); code != http.StatusUnauthorized {
			t.Fatalf("invalid session returned %d", code)
		}
	}
}
