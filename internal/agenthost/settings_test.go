package agenthost

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
	"github.com/CyL-Cly/CodexRM-Agent/internal/release"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/CyL-Cly/CodexRM-Server/server"
	"github.com/CyL-Cly/CodexRM-Server/updater"
)

func startHost(t *testing.T, cfg clientconfig.Connection, installDirs ...string) (*Host, string, string, func()) {
	t.Helper()
	installDir := ""
	if len(installDirs) > 0 {
		installDir = installDirs[0]
	}
	h, err := New(filepath.Join(t.TempDir(), ".state", "connection.json"), installDir, cfg, agent.Config{Mock: true, Cwd: t.TempDir(), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Run(ctx) }()
	var address string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		address, err = h.SettingsURL()
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	u, _ := url.Parse(address)
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("host did not stop")
		}
	}
	t.Cleanup(stop)
	base := "http://" + u.Host
	return h, base, testSettingsToken(t, h, base, u.Fragment), stop
}

// Linux uses the password/session flow even on loopback; desktop uses the URL token.
func testSettingsToken(t *testing.T, h *Host, base, fragment string) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		return fragment
	}
	access, err := readSettingsAccess(h.settingsAccessPath())
	if err != nil {
		t.Fatal(err)
	}
	code, body := request(t, base, access.Password, http.MethodPost, "/api/settings/login", nil)
	var session struct {
		Token string `json:"sessionToken"`
	}
	if err = json.Unmarshal(body, &session); code != http.StatusOK || err != nil || session.Token == "" {
		t.Fatalf("settings login: status %d, decode %v", code, err)
	}
	return session.Token
}

func request(t *testing.T, base, token, method, path string, input any) (int, []byte) {
	t.Helper()
	var body io.Reader
	if input != nil {
		data, _ := json.Marshal(input)
		body = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, base+path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

func TestLocalSettingsListenerLifetimeAndAccess(t *testing.T) {
	h, base, token, stop := startHost(t, clientconfig.Connection{})
	address, err := h.SettingsURL()
	wantAddress := base + "/"
	if runtime.GOOS != "linux" {
		wantAddress += "#" + token
	}
	if err != nil || address != wantAddress {
		t.Fatalf("listener changed: %s, %v", address, err)
	}
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Fatal("not bound to IPv4 loopback")
	}
	if code, _ := request(t, base, "", "GET", "/api/settings", nil); code != 401 {
		t.Fatalf("unauthorized API: %d", code)
	}
	for _, mode := range []string{"origin", "host", "fetch-site"} {
		req, _ := http.NewRequest("PUT", base+"/api/settings", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+token)
		switch mode {
		case "origin":
			req.Header.Set("Origin", "https://evil.example")
		case "host":
			req.Host = "evil.example"
		case "fetch-site":
			req.Header.Set("Sec-Fetch-Site", "cross-site")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("%s accepted: %d", mode, resp.StatusCode)
		}
	}
	code, b := request(t, base, token, "GET", "/api/settings", nil)
	if code != 200 || bytes.Contains(b, []byte(`"updateServer"`)) || bytes.Contains(b, []byte(`"updateManifestUrl"`)) {
		t.Fatalf("default settings: %d %s", code, b)
	}
	code, b = request(t, base, "", "GET", "/", nil)
	if code != 200 || bytes.Contains(b, []byte("update-server")) || bytes.Contains(b, []byte("更新服务器")) {
		t.Fatalf("update server is still exposed in settings: %d %s", code, b)
	}
	stop()
	u, _ := url.Parse(base)
	conn, err := net.DialTimeout("tcp", u.Host, 100*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("settings port remained open after shutdown")
	}
}

func TestSettingsWeChatCapability(t *testing.T) {
	_, base, token, _ := startHost(t, clientconfig.Connection{})
	code, body := request(t, base, token, http.MethodGet, "/api/settings", nil)
	var settings map[string]any
	if code != http.StatusOK || json.Unmarshal(body, &settings) != nil {
		t.Fatalf("settings: %d %s", code, body)
	}
	want := runtime.GOOS == "linux"
	if settings["wxSupported"] != want {
		t.Fatalf("WeChat capability on %s: got %v, want %v", runtime.GOOS, settings["wxSupported"], want)
	}
	if want {
		return
	}
	for _, route := range []string{"/api/wx/accounts", "/api/wx/settings"} {
		if code, _ := request(t, base, token, http.MethodGet, route, nil); code != http.StatusNotFound {
			t.Errorf("%s on %s: got %d, want 404", route, runtime.GOOS, code)
		}
	}
}

func TestUnconfiguredStartupOpensWorkingLocalSettings(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux starts headless and does not open a settings browser")
	}
	h, err := New(filepath.Join(t.TempDir(), ".state", "connection.json"), "", clientconfig.Connection{}, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if h.page.server != nil {
		t.Fatal("settings listener started before opening settings")
	}
	opened := make(chan string, 1)
	h.openBrowser = func(address string) error { opened <- address; return nil }
	h.OpenOnStart = !h.Configured()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case address := <-opened:
		u, _ := url.Parse(address)
		if code, _ := request(t, "http://"+u.Host, u.Fragment, "GET", "/api/settings", nil); code != 200 {
			t.Fatal("browser opened before settings were available")
		}
	case <-time.After(time.Second):
		t.Fatal("unconfigured startup did not open settings")
	}
}

func TestSettingsSaveFailureDoesNotApplyAndServerChangeClearsOldKey(t *testing.T) {
	h, base, token, _ := startHost(t, clientconfig.Connection{Server: "http://127.0.0.1:1", EnrollmentKey: "old-secret"})
	code, b := request(t, base, token, "GET", "/api/settings", nil)
	if code != 200 || bytes.Contains(b, []byte("old-secret")) {
		t.Fatal("settings exposed the saved key")
	}
	input := map[string]any{"server": "http://127.0.0.1:2"}
	if err := os.MkdirAll(h.path, 0700); err != nil {
		t.Fatal(err)
	}
	code, _ = request(t, base, token, "PUT", "/api/settings", input)
	if code == 200 || h.snapshot()["server"] != "http://127.0.0.1:1" {
		t.Fatal("failed persistence changed live configuration")
	}
	if err := os.Remove(h.path); err != nil {
		t.Fatal(err)
	}
	code, b = request(t, base, token, "PUT", "/api/settings", input)
	if code != 200 {
		t.Fatalf("save: %d %s", code, b)
	}
	cfg, err := clientconfig.Load(h.path)
	if err != nil || cfg.Server != "http://127.0.0.1:2" || cfg.EnrollmentKey != "" {
		t.Fatalf("cross-server credential leak: %+v, %v", cfg, err)
	}
	input["enrollmentKey"] = "new-secret"
	if code, _ := request(t, base, token, "PUT", "/api/settings", input); code != 200 {
		t.Fatal("new key not saved")
	}
	delete(input, "enrollmentKey")
	if code, _ := request(t, base, token, "PUT", "/api/settings", input); code != 200 {
		t.Fatal("unchanged key not retained")
	}
	cfg, err = clientconfig.Load(h.path)
	if err != nil || cfg.EnrollmentKey != "new-secret" {
		t.Fatal("blank form erased the saved key")
	}
}

func TestRemotePermissionsPersistIndependentlyAndCancelPreviousGeneration(t *testing.T) {
	const serverURL = "http://127.0.0.1:1"
	h, base, token, _ := startHost(t, clientconfig.Connection{Server: serverURL})
	assertSettings := func(method string, input any, files bool) {
		t.Helper()
		code, b := request(t, base, token, method, "/api/settings", input)
		var data map[string]any
		if code != 200 || json.Unmarshal(b, &data) != nil || data["allowFileManagement"] != files {
			t.Fatalf("%s: expected files %v, got %d %s", method, files, code, b)
		}
	}
	assertSettings("GET", nil, false)
	oldGeneration, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	deadline := time.Now().Add(time.Second)
	for {
		h.mu.Lock()
		originalCancel := h.generationCancel
		if originalCancel != nil {
			h.generationCancel = func() { cancel(); originalCancel() }
			h.mu.Unlock()
			break
		}
		h.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("host did not start a generation")
		}
		time.Sleep(time.Millisecond)
	}
	assertSettings("PUT", map[string]any{"server": serverURL, "allowFileManagement": true}, true)
	if oldGeneration.Err() == nil {
		t.Fatal("previous permission generation still active after saving")
	}
	assertSettings("PUT", map[string]any{"server": serverURL}, true)
	cfg, err := clientconfig.Load(h.path)
	if err != nil || !cfg.AllowFileManagement {
		t.Fatalf("permission configuration did not round trip: %+v, %v", cfg, err)
	}
	applied := withLocalSettings(agent.Config{}, cfg)
	if !applied.AllowFileManagement {
		t.Fatalf("host did not apply persisted permissions: %+v", applied)
	}
	assertSettings("PUT", map[string]any{"server": serverURL, "allowFileManagement": false}, false)
}

func manualUpdateFeed(t *testing.T, versionCode int64) *httptest.Server {
	t.Helper()
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	file, err := zw.Create("CodexRM/bin/codexrm-agent.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("new agent")); err != nil {
		t.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(archive.Bytes())
	manifest := updater.Manifest{VersionCode: versionCode, VersionName: "forced-test", DownloadURL: "client.zip", FileSize: int64(archive.Len()), SHA256: hex.EncodeToString(hash[:])}
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/update.json":
			_ = json.NewEncoder(w).Encode(manifest)
		case "/client.zip":
			_, _ = w.Write(archive.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(feed.Close)
	return feed
}

func TestManualUpdateChecksImmediatelyWithoutRestartingCurrentVersion(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("uses a Windows installation and update-feed fixture")
	}
	feed := manualUpdateFeed(t, release.VersionCode)
	h, base, token, _ := startHost(t, clientconfig.Connection{}, t.TempDir())
	useUpdateFeed(t, h, feed)
	code, body := request(t, base, token, http.MethodPost, "/api/update", nil)
	if code != http.StatusOK || !bytes.Contains(body, []byte(`"updateAvailable":false`)) {
		t.Fatalf("manual current-version check: %d %s", code, body)
	}
	h.mu.Lock()
	running := h.ctx.Err() == nil
	h.mu.Unlock()
	if !running {
		t.Fatal("current-version check stopped the agent")
	}
	if code, _ = request(t, base, token, http.MethodGet, "/api/update", nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected update method accepted: %d", code)
	}
}

func TestManualUpdateStartsHelperAndStopsRunningHost(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("uses the Windows detached update helper")
	}
	feed := manualUpdateFeed(t, release.VersionCode+1)
	h, _, _, _ := startHost(t, clientconfig.Connection{}, t.TempDir())
	useUpdateFeed(t, h, feed)
	started := make(chan string, 1)
	h.startUpdate = func(plan *updater.Plan) error {
		started <- plan.Manifest.VersionName
		plan.Discard()
		return nil
	}
	available, err := h.checkForUpdate()
	if err != nil || !available {
		t.Fatalf("forced update was not prepared: available=%v, err=%v", available, err)
	}
	select {
	case version := <-started:
		if version != "forced-test" {
			t.Fatalf("unexpected update started: %s", version)
		}
	default:
		t.Fatal("verified update did not start its helper")
	}
	h.mu.Lock()
	ctxErr := h.ctx.Err()
	h.mu.Unlock()
	if ctxErr == nil {
		t.Fatal("idle host did not stop for its update")
	}
}

func useUpdateFeed(t *testing.T, h *Host, feed *httptest.Server) {
	t.Helper()
	t.Setenv("CODEXRM_AGENT_UPDATE_SERVER", "https://updates.example/base")
	h.newUpdater = func(cfg updater.Config) (*updater.Updater, error) {
		if cfg.ServerURL != "https://updates.example/base" || cfg.ManifestURL != "https://updates.example/base/update.json" {
			t.Errorf("host did not use the configured update feed: %+v", cfg)
		}
		cfg.ServerURL, cfg.ManifestURL = feed.URL, feed.URL+"/update.json"
		return updater.New(cfg)
	}
}

func TestSettingsPairListAndRevokeAgainstRelay(t *testing.T) {
	relay, err := server.New(server.Config{DataDir: t.TempDir(), AdminToken: "relay-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	remote := httptest.NewServer(relay.Handler())
	defer remote.Close()
	h, base, token, _ := startHost(t, clientconfig.Connection{Server: remote.URL, EnrollmentKey: "relay-key"})
	for deadline := time.Now().Add(5 * time.Second); ; {
		if h.snapshot()["status"] == "已连接" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no relay connection: %+v", h.snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
	code, b := request(t, base, token, "POST", "/api/pair", nil)
	if code != 200 {
		t.Fatalf("pair: %d %s", code, b)
	}
	var pair struct{ Code, URL, QR string }
	if err := json.Unmarshal(b, &pair); err != nil {
		t.Fatal(err)
	}
	if pair.Code == "" || !strings.HasPrefix(pair.QR, "data:image/png;base64,") || !strings.HasPrefix(pair.URL, remote.URL+"/#pair=") {
		t.Fatalf("incomplete pair response: %s", b)
	}
	code, b = request(t, remote.URL, "", "POST", "/api/v1/client/claim", map[string]string{"code": pair.Code, "clientName": "test browser", "deviceType": "desktop"})
	if code != 200 {
		t.Fatalf("claim: %d %s", code, b)
	}
	var claimed protocol.PairClaimResponse
	_ = json.Unmarshal(b, &claimed)
	code, b = request(t, base, token, "GET", "/api/devices", nil)
	if code != 200 || !bytes.Contains(b, []byte(claimed.ClientID)) {
		t.Fatalf("device missing: %d %s", code, b)
	}
	var devices []protocol.ClientInfo
	if err := json.Unmarshal(b, &devices); err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].LastConnectedAt == "" || devices[0].LastIP != "127.0.0.1" {
		t.Fatalf("connection activity missing from local Agent settings: %s", b)
	}
	code, b = request(t, base, token, "DELETE", "/api/devices/"+claimed.ClientID, nil)
	if code != 200 {
		t.Fatalf("revoke: %d %s", code, b)
	}
	code, b = request(t, base, token, "GET", "/api/devices", nil)
	if code != 200 || strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("revoked device remained: %s", b)
	}
}
