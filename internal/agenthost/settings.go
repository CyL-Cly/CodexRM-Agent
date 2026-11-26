package agenthost

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
	"github.com/CyL-Cly/CodexRM-Agent/internal/installation"
	"github.com/CyL-Cly/CodexRM-Agent/internal/release"
	"github.com/skip2/go-qrcode"
)

//go:embed web/*
var assets embed.FS

type settingsPage struct {
	mu     sync.Mutex
	server *http.Server
	url    string
}

func (h *Host) settingsAccessPath() string {
	return filepath.Join(h.StateDir(), settingsAccessFile)
}

// SettingsURL starts or reuses the settings listener for this process lifetime.
func (h *Host) SettingsURL() (string, error) {
	h.page.mu.Lock()
	defer h.page.mu.Unlock()
	h.mu.Lock()
	ctx := h.ctx
	h.mu.Unlock()
	if ctx == nil || ctx.Err() != nil {
		return "", errors.New("agent 尚未启动或已退出")
	}
	if h.page.server != nil {
		return h.page.url, nil
	}
	var listener net.Listener
	var token string
	var err error
	passwordSettings := h.LANSettings || h.LoopbackSettings || runtime.GOOS == "linux"
	if passwordSettings {
		bindHost := "127.0.0.1"
		if h.LANSettings {
			bindHost = ""
		}
		listener, token, err = listenPasswordSettings(h.settingsAccessPath(), bindHost)
	} else {
		listener, err = net.Listen("tcp4", "127.0.0.1:0")
		token = settingsSecret()
	}
	if err != nil {
		return "", err
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	localHost := net.JoinHostPort("127.0.0.1", port)
	h.page.url = "http://" + localHost + "/"
	if !passwordSettings {
		h.page.url += "#" + token
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/settings", h.settings)
	mux.HandleFunc("/api/update", h.update)
	mux.HandleFunc("/api/pair", h.pair)
	mux.HandleFunc("/api/devices", h.devices)
	mux.HandleFunc("/api/devices/", h.devices)
	mux.HandleFunc("POST /api/providers/{action}", h.providerSettings)
	h.registerPhoneTransferRoutes(mux)
	if oneBotSupported {
		mux.HandleFunc("/api/onebot", h.oneBotSettings)
		mux.HandleFunc("POST /api/onebot/test", h.oneBotTest)
	}
	if wxSupported {
		mux.HandleFunc("/api/wx/accounts", h.wxList)
		mux.HandleFunc("GET /api/wx/workspaces", h.wxWorkspaces)
		mux.HandleFunc("/api/wx/cache", h.wxRecordCache)
		mux.HandleFunc("/api/wx/maintenance", h.wxMaintenance)
		mux.HandleFunc("/api/wx/robot/{operation}", h.wxRobotManagement)
		mux.HandleFunc("GET /wx-slider/{challenge}", h.wxSliderFrame)
		mux.HandleFunc("GET /wx-slider/{challenge}/submit", h.wxSliderSubmit)
		mux.HandleFunc("/api/wx/manage/{operation}", h.wxOperation)
		mux.HandleFunc("/api/wx/probe", h.wxProbe)
		mux.HandleFunc("/api/wx/task", h.wxTask)
		mux.HandleFunc("POST /api/wx/install", h.wxStartInstall)
		mux.HandleFunc("POST /api/wx/takeover", h.wxStartTakeover)
		mux.HandleFunc("/api/wx/contacts", h.wxContacts)
		mux.HandleFunc("/api/wx/group-members", h.wxGroupMembers)
		mux.HandleFunc("/api/wx/group-notices", h.wxGroupNotices)
		mux.HandleFunc("/api/wx/contacts/sync", h.wxSyncContacts)
		mux.HandleFunc("/api/wx/{action}", h.wxSettings)
	}
	settingsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/wx/") {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(3 * time.Minute))
		}
		mux.ServeHTTP(w, r)
	})
	root, _ := fs.Sub(assets, "web")
	mux.Handle("/", http.FileServer(http.FS(root)))
	sessionLogin := passwordSettings && runtime.GOOS == "linux"
	h.page.server = &http.Server{Handler: guardSettings(settingsHandler, localHost, token, passwordSettings, sessionLogin), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go h.page.server.Serve(listener)
	return h.page.url, nil
}

func (h *Host) closePage() {
	h.page.mu.Lock()
	defer h.page.mu.Unlock()
	if h.page.server != nil {
		// Let a manual Linux update return its result before we re-exec.
		if h.linuxUpdate != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = h.page.server.Shutdown(ctx)
			cancel()
		}
		_ = h.page.server.Close()
		h.page.server = nil
		h.page.url = ""
	}
}

// Require the literal address of the receiving interface to reject DNS rebinding.
func matchesSettingsInterface(r *http.Request) bool {
	local, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	if !ok {
		return false
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || port != strconv.Itoa(local.Port) {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.WithZone("").Unmap() == local.AddrPort().Addr().WithZone("").Unmap()
}

func guardSettings(next http.Handler, localHost, secret string, passwordSettings, sessionLogin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		validHost := r.Host == localHost
		if passwordSettings {
			validHost = matchesSettingsInterface(r)
		}
		origin := r.Header.Get("Origin")
		sliderCallback := wxSupported && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/wx-slider/") && strings.HasSuffix(r.URL.Path, "/submit") && origin == "null"
		if !validHost || (!sliderCallback && ((origin != "" && origin != "http://"+r.Host) || r.Header.Get("Sec-Fetch-Site") == "cross-site")) {
			http.Error(w, "请直接通过 Agent 的 IP 地址打开设置页面", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			passwordOK := bearer && subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
			login := sessionLogin && r.Method == http.MethodPost && r.URL.Path == "/api/settings/login"
			if !passwordOK && (!bearer || !sessionLogin || login || !validSettingsSession(secret, token, time.Now())) {
				if passwordSettings {
					respond(w, http.StatusUnauthorized, map[string]any{"error": "请输入正确的设置密码", "passwordRequired": true, "sessionLogin": sessionLogin})
				} else {
					http.Error(w, "请从托盘重新打开设置页面", http.StatusUnauthorized)
				}
				return
			}
			if login {
				expiresAt := time.Now().Add(settingsSessionLifetime)
				respond(w, http.StatusOK, map[string]string{"sessionToken": signSettingsSession(secret, expiresAt.Unix())})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func failure(w http.ResponseWriter, err error) {
	respond(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

type Status struct {
	State           agent.ConnectionState `json:"state"`
	Error           string                `json:"error,omitempty"`
	LastConnectedAt *time.Time            `json:"lastConnectedAt,omitempty"`
	NextRetryAt     *time.Time            `json:"nextRetryAt,omitempty"`
}

func (h *Host) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.config.Server == "" {
		return Status{}
	}
	if h.active != nil && h.activeVersion == h.version {
		snapshot := h.active.ConnectionSnapshot()
		lastConnectedAt := snapshot.LastConnectedAt
		if lastConnectedAt != nil {
			h.lastConnectedAt = lastConnectedAt
		}
		if lastConnectedAt == nil {
			lastConnectedAt = h.lastConnectedAt
		}
		return Status{State: snapshot.State, Error: snapshot.Error, LastConnectedAt: lastConnectedAt, NextRetryAt: snapshot.NextRetryAt}
	}
	state := h.connectionState
	if state == "" {
		state = agent.Connecting
	}
	return Status{State: state, Error: h.lastError, LastConnectedAt: h.lastConnectedAt, NextRetryAt: h.nextRetryAt}
}

func (h *Host) snapshot() map[string]any {
	h.mu.Lock()
	server, keySet := h.config.Server, h.config.EnrollmentKey != ""
	fileManagement := h.config.AllowFileManagement
	pending := h.pendingUpdate != nil
	updaterInstance := h.activeUpdater
	h.mu.Unlock()
	connection := h.Status()
	status := "未配置"
	switch connection.State {
	case agent.Connecting:
		status = "正在连接"
	case agent.Connected:
		status = "已连接"
	case agent.Retrying:
		status = "等待重试"
	case agent.Degraded:
		status = "功能受限"
	}
	updateStatus := ""
	if pending {
		updateStatus = "更新已校验，等待进行中的对话和排队消息结束后安装并重启。"
	} else if updaterInstance != nil {
		s := updaterInstance.Snapshot()
		switch s.Phase {
		case "checking":
			updateStatus = "正在检查更新。"
		case "error":
			updateStatus = "更新检查失败"
			if s.Error != "" {
				updateStatus += "：" + s.Error
			}
			if retry := s.NextRetryAt; retry != nil {
				updateStatus += "；下次重试 " + retry.Local().Format("15:04:05")
			}
		case "ready":
			updateStatus = "新版本 " + s.TargetVersion + " 已校验。"
		case "up-to-date":
			updateStatus = "当前已是最新版本"
			if !s.LastSuccessfulCheck.IsZero() {
				updateStatus += "；最近检查 " + s.LastSuccessfulCheck.Local().Format("2006-01-02 15:04")
			}
		}
	}
	return map[string]any{"allowFileManagement": fileManagement, "server": server, "enrollmentKeySet": keySet, "updatePending": pending, "updateStatus": updateStatus, "updateMode": h.updateMode(), "status": status, "state": connection.State, "error": connection.Error, "lastConnectedAt": connection.LastConnectedAt, "nextRetryAt": connection.NextRetryAt, "version": release.VersionName, "packaged": installation.IsPackaged(), "wxSupported": wxSupported, "oneBotSupported": oneBotSupported}
}

func (h *Host) settings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		data := h.snapshot()
		data["phoneTransfersSupported"] = localFileTransferSettings(r)
		respond(w, 200, data)
		return
	}
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		Server              string  `json:"server"`
		EnrollmentKey       *string `json:"enrollmentKey"`
		AllowFileManagement *bool   `json:"allowFileManagement"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		failure(w, err)
		return
	}
	h.opMu.Lock()
	defer h.opMu.Unlock()
	h.mu.Lock()
	cfg := h.config
	stopping := h.ctx == nil || h.ctx.Err() != nil
	h.mu.Unlock()
	if stopping {
		http.Error(w, "Agent 正在退出", http.StatusServiceUnavailable)
		return
	}
	server, err := clientconfig.ServerURL(input.Server)
	if err != nil {
		failure(w, err)
		return
	}
	if server != cfg.Server {
		cfg.EnrollmentKey = ""
	}
	cfg.Server = server
	if input.EnrollmentKey != nil {
		cfg.EnrollmentKey = *input.EnrollmentKey
	}
	if input.AllowFileManagement != nil {
		cfg.AllowFileManagement = *input.AllowFileManagement
	}
	if err := cfg.Normalize(); err != nil {
		failure(w, err)
		return
	}
	if err := clientconfig.Save(h.path, cfg); err != nil {
		failure(w, err)
		return
	}
	h.mu.Lock()
	h.config = cfg
	if h.generationCancel != nil {
		h.generationCancel() // Revoke the previous policy and close its direct sessions.
	}
	h.version++
	h.lastError = ""
	h.connectionState = agent.Connecting
	h.nextRetryAt = nil
	h.mu.Unlock()
	select {
	case h.changed <- struct{}{}:
	default:
	}
	respond(w, 200, h.snapshot())
}

func (h *Host) update(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Package downloads can legitimately outlast the small settings requests.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Minute))
	available, err := h.checkForUpdate()
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]bool{"updateAvailable": available, "updatePending": h.snapshot()["updatePending"].(bool)})
}

func (h *Host) pair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	h.opMu.Lock()
	defer h.opMu.Unlock()
	a, ctx, cancel, err := h.current(r.Context())
	if err != nil {
		failure(w, err)
		return
	}
	defer cancel()
	pair, err := a.Pair(ctx)
	if err != nil {
		failure(w, err)
		return
	}
	code := pair.ManualPairingCode
	if code == "" {
		code = pair.PairingCode
	}
	qrLink, webLink := a.PairingLink(code)
	png, err := qrcode.Encode(qrLink, qrcode.Medium, 224)
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, map[string]any{"code": code, "url": webLink, "expiresAt": pair.ExpiresAt, "qr": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)})
}

func (h *Host) devices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	h.opMu.Lock()
	defer h.opMu.Unlock()
	a, ctx, cancel, err := h.current(r.Context())
	if err != nil {
		failure(w, err)
		return
	}
	defer cancel()
	if r.Method == http.MethodDelete {
		id := strings.TrimPrefix(r.URL.Path, "/api/devices/")
		if id == "" || id == r.URL.Path || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if err := a.RemoveDevice(ctx, id); err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
		return
	}
	devices, err := a.ListDevices(ctx)
	if err != nil {
		failure(w, err)
		return
	}
	if devices == nil {
		respond(w, 200, []any{})
		return
	}
	respond(w, 200, devices)
}
