package agenthost

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
	"github.com/CyL-Cly/CodexRM-Agent/internal/desktop"
	"github.com/CyL-Cly/CodexRM-Agent/internal/installation"
	"github.com/CyL-Cly/CodexRM-Agent/internal/release"
	"github.com/CyL-Cly/CodexRM-Server/updater"
)

type Host struct {
	OpenOnStart            bool
	LANSettings            bool
	LoopbackSettings       bool
	LinuxUpdateExecutable  string // Set by the standalone agent before Run.
	mu                     sync.Mutex
	opMu                   sync.Mutex
	config                 clientconfig.Connection
	path                   string
	base                   agent.Config
	installDir             string
	active                 *agent.Agent
	activeCtx              context.Context
	generationCancel       context.CancelFunc
	wxLocalServer          string
	wxLocalEnrollment      string
	wxRelayMu              sync.Mutex
	wxTaskWG               sync.WaitGroup
	wxInstanceMu           sync.Mutex
	wxScanMu               sync.Mutex
	wxScans                map[int64]wxLoginScan
	wxActive               *agent.Agent
	wxActiveCtx            context.Context
	wxActiveVersion        uint64
	version, activeVersion uint64
	lastError              string
	connectionState        agent.ConnectionState
	lastConnectedAt        *time.Time
	nextRetryAt            *time.Time
	changed                chan struct{}
	ctx                    context.Context
	cancel                 context.CancelFunc
	page                   settingsPage
	openBrowser            func(string) error
	startUpdate            func(*updater.Plan) error
	newUpdater             func(updater.Config) (*updater.Updater, error)
	activeUpdater          *updater.Updater
	pendingUpdate          *updater.Plan
	linuxUpdate            *updater.LinuxUpdate
	updateRetryAt          time.Time
	updateWake             chan struct{}
}

func New(path, installDir string, config clientconfig.Connection, base agent.Config) (*Host, error) {
	if err := config.Normalize(); err != nil {
		return nil, err
	}
	if base.ProviderDir == "" {
		base.ProviderDir = filepath.Dir(path)
	}
	if runtime.GOOS == "linux" {
		base.OneBotSettingsPath = filepath.Join(filepath.Dir(path), "onebot.json")
	}
	return &Host{path: path, installDir: installDir, config: config, base: base, version: 1, changed: make(chan struct{}, 1), updateWake: make(chan struct{}, 1), openBrowser: desktop.OpenBrowser, newUpdater: updater.New, startUpdate: func(plan *updater.Plan) error { return plan.StartHelper() }}, nil
}

func (h *Host) StateDir() string { return filepath.Dir(h.path) }
func (h *Host) Configured() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.config.Server != "" }

func (h *Host) OpenSettings() { h.openSettingsView("") }

func (h *Host) OpenPhoneTransfers() { h.openSettingsView("phone-transfers") }

func (h *Host) openSettingsView(view string) {
	address, err := h.SettingsURL()
	if err == nil {
		var u *url.URL
		u, err = url.Parse(address)
		if err == nil {
			if view != "" {
				u.RawQuery = url.Values{"view": {view}}.Encode()
			}
			err = h.openBrowser(u.String())
		}
	}
	if err != nil {
		// Headless Linux has no browser; the URL is still needed for an SSH tunnel.
		log.Print("无法自动打开设置页面，请从 CodexRM 设置入口重试")
	}
}

func (h *Host) Run(ctx context.Context) error {
	h.mu.Lock()
	h.ctx, h.cancel = context.WithCancel(ctx)
	runCtx := h.ctx
	h.mu.Unlock()
	defer func() {
		h.cancel()
		wxRuntime.mu.Lock()
		wxRuntime.mu.Unlock() // Prevent Add racing with Wait on shutdown.
		h.wxTaskWG.Wait()
	}()
	if wxSupported {
		go h.resumeWXTask()
		if instance := h.loadWXInstance(); instance != nil && (instance.Mode == "installed" || instance.Mode == "taken-over") {
			probeCtx, stop := context.WithTimeout(runCtx, 5*time.Second)
			if err := h.ensureWXRelay(probeCtx); err != nil {
				log.Printf("WeChat relay: %v", err)
			}
			stop()
		}
	}
	defer h.cancel()
	defer h.closePage()
	if h.LANSettings {
		address, err := h.SettingsURL()
		if err != nil {
			return fmt.Errorf("启动局域网设置页面: %w", err)
		}
		local, _ := url.Parse(address)
		log.Printf("Agent 设置页面监听端口 %s，密码保存在 %s；风险提示：仅可在可信网络启用 LAN 设置", local.Port(), h.settingsAccessPath())
	} else if h.LoopbackSettings || runtime.GOOS == "linux" {
		address, err := h.SettingsURL()
		if err != nil {
			return fmt.Errorf("启动本机设置页面: %w", err)
		}
		local, _ := url.Parse(address)
		log.Printf("设置服务仅监听 %s，密码保存在 %s。远程访问可使用 ssh -L %s:%s <主机>", local.Host, h.settingsAccessPath(), local.Port(), local.Host)
	} else if h.OpenOnStart {
		h.OpenSettings()
	}
	// A configured server may be offline, and a new installation may have no
	// Codex yet. Both must leave the tray and settings usable after an update.
	updater.SignalReady()
	updates := make(chan *updater.Plan)
	updateDone := make(chan struct{})
	go func() { defer close(updateDone); h.runUpdates(runCtx, updates) }()
	defer func() {
		h.cancel()
		<-updateDone
		h.opMu.Lock()
		h.mu.Lock()
		if h.pendingUpdate != nil {
			h.pendingUpdate.Discard()
			h.pendingUpdate = nil
		}
		h.mu.Unlock()
		h.opMu.Unlock()
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for runCtx.Err() == nil {
		generationCtx, cancel := context.WithCancel(runCtx)
		h.mu.Lock()
		h.generationCancel = cancel
		cfg, version := h.config, h.version
		wxRelay, wxKey := h.wxLocalServer, h.wxLocalEnrollment
		h.mu.Unlock()
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			if wxRelay != "" && wxRelay != cfg.Server {
				wxDone := make(chan struct{})
				go func() { defer close(wxDone); h.runWXConnection(generationCtx, version, wxRelay, wxKey) }()
				defer func() { <-wxDone }()
			}
			h.runConnection(generationCtx, cfg, version)
		}()
		reconfigure := false
		for !reconfigure && runCtx.Err() == nil {
			select {
			case <-runCtx.Done():
			case <-h.changed:
				reconfigure = true
			case plan := <-updates:
				h.opMu.Lock()
				h.queueUpdate(plan)
				h.opMu.Unlock()
			case <-h.updateWake:
			case <-ticker.C:
			}
			if !reconfigure && runCtx.Err() == nil {
				h.opMu.Lock()
				err := h.applyPendingUpdate()
				h.opMu.Unlock()
				if err != nil {
					log.Printf("update postponed: %v", err)
				}
			}
		}
		cancel()
		<-finished
	}
	return nil
}

type updateMode string

const (
	updateLinuxAgent   updateMode = "linux-agent"
	updateLinuxManual  updateMode = "linux-manual"
	updateWindowsStore updateMode = "windows-package"
	updateWindowsAgent updateMode = "windows-agent"
	updateManual       updateMode = "manual"
)

func (h *Host) updateMode() updateMode {
	switch runtime.GOOS {
	case "linux":
		if h.LinuxUpdateExecutable != "" && !h.base.Mock {
			return updateLinuxAgent
		}
		return updateLinuxManual
	case "windows":
		if installation.IsPackaged() {
			return updateWindowsStore
		}
		if h.installDir != "" {
			return updateWindowsAgent
		}
	}
	return updateManual
}

// RestartUpdate completes a Linux update after Run has shut down all listeners
// and connections. Windows hands the update off to its detached helper.
func (h *Host) RestartUpdate(ctx context.Context) error {
	if h.linuxUpdate != nil {
		return h.linuxUpdate.Restart(ctx)
	}
	return nil
}

func (h *Host) updaterConfig() (updater.Config, error) {
	linux := h.updateMode() == updateLinuxAgent
	base, manifest, err := clientconfig.UpdateFeed(linux)
	cfg := updater.Config{ServerURL: base, ManifestURL: manifest, VersionCode: release.VersionCode}
	if linux {
		cfg.StateDir = filepath.Join(h.StateDir(), ".codexrm-updates")
		cfg.LinuxExecutable = h.LinuxUpdateExecutable
	} else {
		cfg.InstallDir = h.installDir
		cfg.Args = []string{"--config", h.path}
	}
	return cfg, err
}

func (h *Host) runUpdates(ctx context.Context, ready chan<- *updater.Plan) {
	h.mu.Lock()
	autoUpdate := h.config.AutoUpdate
	h.mu.Unlock()
	mode := h.updateMode()
	if (mode != updateWindowsAgent && mode != updateLinuxAgent) || h.base.Mock || !autoUpdate {
		return
	}
	cfg, err := h.updaterConfig()
	if err != nil {
		log.Printf("automatic update: %v", err)
		return
	}
	if cfg.ManifestURL == "" {
		return
	}
	u, err := h.newUpdater(cfg)
	if err != nil {
		log.Printf("automatic update: %v", err)
		return
	}
	h.mu.Lock()
	h.activeUpdater = u
	h.mu.Unlock()
	u.Run(ctx, ready)
}

// queueUpdate and applyPendingUpdate are serialized with settings and manual
// checks by opMu. The pending package survives relay and settings reconnections.
func (h *Host) queueUpdate(plan *updater.Plan) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx.Err() != nil || h.pendingUpdate != nil {
		plan.Discard()
		return
	}
	h.pendingUpdate = plan
	log.Printf("update %s ready; waiting for conversations to finish", plan.Manifest.VersionName)
}

var errUpdateBusy = errors.New("secondary agent is busy")

func (h *Host) applyPendingUpdate() error {
	wxRuntime.mu.Lock()
	defer wxRuntime.mu.Unlock()
	if wxRuntime.running[h.StateDir()] {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pendingUpdate == nil || h.ctx.Err() != nil || time.Now().Before(h.updateRetryAt) {
		return nil
	}
	plan := h.pendingUpdate
	start := func() error {
		var err error
		if h.updateMode() == updateLinuxAgent {
			h.linuxUpdate, err = plan.StageLinux()
		} else {
			err = h.startUpdate(plan)
		}
		if err != nil {
			h.updateRetryAt = time.Now().Add(time.Minute)
			return err
		}
		h.pendingUpdate = nil // The restart path now owns the staged files.
		log.Printf("applying update %s", plan.Manifest.VersionName)
		h.cancel()
		return nil
	}
	// Both connections share this host and are stopped by the update.
	if h.wxActive != nil && h.wxActiveCtx.Err() == nil {
		startUpdate := start
		start = func() error {
			started, err := h.wxActive.TryStartUpdate(startUpdate)
			if !started && err == nil {
				return errUpdateBusy
			}
			return err
		}
	}
	var err error
	if h.active != nil && h.activeCtx.Err() == nil {
		_, err = h.active.TryStartUpdate(start)
	} else {
		err = start()
	}
	if errors.Is(err, errUpdateBusy) {
		return nil
	}
	return err
}

// Manual checks use the same idle gate as automatic updates and return while a
// conversation is still running, leaving the verified package pending.
func (h *Host) checkForUpdate() (bool, error) {
	return h.checkForUpdateMode(true)
}

func (h *Host) checkRemoteUpdate() (agent.UpdateResult, error) {
	available, err := h.checkForUpdateMode(false)
	return agent.UpdateResult{UpdateAvailable: available, UpdatePending: available}, err
}

func (h *Host) checkForUpdateMode(applyImmediately bool) (bool, error) {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	h.mu.Lock()
	ctx := h.ctx
	pending := h.pendingUpdate != nil
	running := ctx != nil && ctx.Err() == nil
	h.mu.Unlock()
	if !running {
		return false, errors.New("Agent 正在退出")
	}
	switch h.updateMode() {
	case updateLinuxManual:
		return false, errors.New("Linux Agent 请下载更新包，停止 Agent 后替换二进制文件并重新启动")
	case updateWindowsStore:
		return false, errors.New("此版本由 Windows 管理更新")
	case updateManual:
		return false, errors.New("请下载新版本并手动更新 Agent")
	}
	if pending {
		return true, nil
	}
	cfg, err := h.updaterConfig()
	if err != nil {
		return false, err
	}
	if cfg.ManifestURL == "" {
		return false, errors.New("尚未配置更新源，请设置 CODEXRM_AGENT_UPDATE_SERVER 后重启 Agent")
	}
	u, err := h.newUpdater(cfg)
	if err != nil {
		return false, err
	}
	h.mu.Lock()
	h.activeUpdater = u
	h.mu.Unlock()
	plan, err := u.Prepare(ctx)
	if err != nil || plan == nil {
		return false, err
	}
	h.queueUpdate(plan)
	if applyImmediately {
		if err = h.applyPendingUpdate(); err != nil {
			return true, err
		}
		select {
		case h.updateWake <- struct{}{}:
		default:
		}
	}
	return true, nil
}

func (h *Host) runConnection(ctx context.Context, cfg clientconfig.Connection, version uint64) {
	if cfg.Server == "" {
		return
	}
	base := h.base
	base.ServerURL, base.EnrollmentKey = cfg.Server, cfg.EnrollmentKey
	if h.updateMode() != updateLinuxManual {
		base.CheckForUpdate = h.checkRemoteUpdate
	}
	base.NoAutoPair = true
	base = withLocalSettings(base, cfg)
	a, err := agent.NewForServer(base)
	if err != nil {
		h.setError(version, err)
		return
	}
	h.mu.Lock()
	if h.version != version || ctx.Err() != nil {
		h.mu.Unlock()
		return
	}
	h.active, h.activeCtx, h.activeVersion, h.lastError = a, ctx, version, ""
	h.connectionState, h.nextRetryAt = agent.Connecting, nil
	h.mu.Unlock()
	for ctx.Err() == nil {
		err = a.Run(ctx)
		if ctx.Err() != nil {
			break
		}
		h.setError(version, err)
		h.mu.Lock()
		if h.version == version {
			h.connectionState = agent.Retrying
			retryAt := time.Now().Add(3 * time.Second).UTC()
			h.nextRetryAt = &retryAt
		}
		h.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

func (h *Host) setError(version uint64, err error) {
	if err == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.version == version {
		h.lastError = "连接初始化或运行失败；请检查服务器、授权、网络及 Codex 配置后重试"
	}
	log.Printf("agent connection: %v", err)
}

func (h *Host) current(ctx context.Context) (*agent.Agent, context.Context, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.active == nil || h.activeVersion != h.version || h.activeCtx.Err() != nil {
		return nil, nil, nil, errors.New("请先保存远程服务器设置，稍后重试")
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	stop := context.AfterFunc(h.activeCtx, cancel)
	return h.active, requestCtx, func() { stop(); cancel() }, nil
}

func withLocalSettings(base agent.Config, cfg clientconfig.Connection) agent.Config {
	base.AllowFileManagement = cfg.AllowFileManagement
	if cfg.Workspace != "" {
		base.Cwd = cfg.Workspace
	}
	if cfg.CodexPath != "" {
		base.CodexPath = cfg.CodexPath
		if filepath.IsAbs(base.CodexPath) {
			if _, err := os.Stat(base.CodexPath); errors.Is(err, os.ErrNotExist) {
				base.CodexPath = ""
			}
		}
	}
	return base
}
