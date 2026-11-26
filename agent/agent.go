package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Agent/internal/desktop"
	"github.com/CyL-Cly/CodexRM-Agent/internal/release"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/CyL-Cly/CodexRM-Server/securechannel"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type UpdateResult struct {
	UpdateAvailable bool `json:"updateAvailable"`
	UpdatePending   bool `json:"updatePending"`
}

type ConnectionState string

const (
	Connecting ConnectionState = "connecting"
	Connected  ConnectionState = "connected"
	Retrying   ConnectionState = "retrying"
	Degraded   ConnectionState = "degraded"
)

type ConnectionSnapshot struct {
	State           ConnectionState `json:"state"`
	Error           string          `json:"error,omitempty"`
	LastConnectedAt *time.Time      `json:"lastConnectedAt,omitempty"`
	NextRetryAt     *time.Time      `json:"nextRetryAt,omitempty"`
}

type httpStatusError struct{ status int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.status) }

func connectionAdvice(err error) string {
	var status *httpStatusError
	if errors.As(err, &status) {
		switch {
		case status.status == http.StatusUnauthorized || status.status == http.StatusForbidden:
			return "服务器拒绝了授权；请重新获取连接授权密钥，或重新配对设备"
		case status.status >= 500:
			return "远程服务器暂时不可用；请稍后重试，或联系服务器管理员检查服务状态"
		}
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "无法解析服务器域名；请检查服务器地址、DNS 设置和网络连接"
	}
	var unknownAuthority x509.UnknownAuthorityError
	var certificateInvalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	var recordHeader tls.RecordHeaderError
	if errors.As(err, &unknownAuthority) || errors.As(err, &certificateInvalid) || errors.As(err, &hostname) || errors.As(err, &recordHeader) {
		return "服务器 TLS 证书校验失败；请检查证书链、域名和系统时间，不要在公共终端跳过证书校验"
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) || (errors.As(err, &network) && network.Timeout()) {
		return "连接服务器超时；请检查网络、防火墙和代理设置，稍后重试"
	}
	if err != nil && (strings.Contains(strings.ToLower(err.Error()), "tls") || strings.Contains(strings.ToLower(err.Error()), "x509")) {
		return "服务器 TLS 握手失败；请检查 HTTPS 配置、证书和系统时间"
	}
	return "无法连接远程服务器；请检查服务器地址、网络和服务状态，稍后重试"
}

type Config struct {
	AllowFileManagement                                                                                             bool
	OnReady                                                                                                         func()
	CheckForUpdate                                                                                                  func() (UpdateResult, error)
	ServerURL, AppServerURL, AppServerToken, Name, DataDir, InstallationID, EnrollmentKey, CodexPath, Cwd, MockAddr string
	CCSwitchDB                                                                                                      string
	ProviderDir                                                                                                     string // Directory for Agent-managed model providers.
	OneBotSettingsPath                                                                                              string // Optional OneBot 11 configuration; empty disables the local adapter.
	Mock                                                                                                            bool
	SecureRelay                                                                                                     bool
	NoAutoPair                                                                                                      bool
}
type creds struct {
	AgentID        string `json:"agentId"`
	EnvironmentID  string `json:"environmentId"`
	Token          string `json:"token"`
	InstallationID string `json:"installationId"`
	ServerURL      string `json:"serverUrl"`
	ExpiresAt      int64  `json:"expiresAt"`
}
type approval struct {
	ID          json.RawMessage
	Raw         json.RawMessage
	Owner       string
	ThreadID    string
	TurnID      string
	Sending     bool
	NonBlocking bool
}
type Agent struct {
	cfg            Config
	localEvent     sendEnvelope // In-process pipe transport, immutable after startup.
	mentionedFiles mentionedFileIndex
	creds          creds
	credsDirty     bool
	credsMu        sync.RWMutex
	enrollMu       sync.Mutex
	mu             sync.Mutex
	writeMu        sync.Mutex
	eventMu        sync.Mutex
	direct         map[string]*directSession
	lanAuth        atomic.Pointer[lanAuthorization]
	eventClients   map[string]bool  // nil until this RM supplies a routing snapshot
	relayEvents    *relayEventQueue // protected by eventMu
	relay          *websocket.Conn
	owners         map[string]string
	approvals      map[string]approval
	queueMu        sync.Mutex
	queues         map[string]*messageQueue
	queueCtx       context.Context
	requests       int
	updating       bool
	attachmentMu   sync.Mutex
	phoneReads     phoneFileReaders
	receiveBatches receiveBatchQueue
	conversationMu sync.Mutex
	uploads        map[string]*attachmentUpload
	providerApp    atomic.Pointer[appserver.Client]
	oneBotMu       sync.RWMutex
	oneBotOpMu     sync.Mutex
	oneBot         *oneBotRuntime
	oneBotErr      string

	textMu           sync.Mutex
	textApp          *appserver.Client
	textVerified     bool
	wxMu             sync.Mutex
	wxPolicy         map[string]wxPolicy
	wxThreads        map[string]wxThread
	wxHistoryPending map[string]wxHistoryPending
	wxRecordTurns    map[string]map[*wxRecordTurn]bool
	connectionMu     sync.RWMutex
	connection       ConnectionSnapshot
}

func New(cfg Config) (*Agent, error) {
	cfg.ServerURL = strings.TrimRight(cfg.ServerURL, "/")
	u, e := url.Parse(cfg.ServerURL)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("--server must be an http(s) URL")
	}
	if cfg.DataDir == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return nil, e
		}
		hash := sha256.Sum256([]byte(cfg.ServerURL))
		cfg.DataDir = filepath.Join(home, ".codexrm", hex.EncodeToString(hash[:6]))
	}
	if cfg.Cwd == "" {
		cfg.Cwd, _ = os.Getwd()
	}
	cfg.Cwd, e = filepath.Abs(cfg.Cwd)
	if e != nil {
		return nil, e
	}
	if st, e := os.Stat(cfg.Cwd); e != nil || !st.IsDir() {
		return nil, fmt.Errorf("working directory does not exist: %s", cfg.Cwd)
	}
	if cfg.Name == "" {
		cfg.Name, _ = os.Hostname()
	}
	if cfg.MockAddr == "" {
		cfg.MockAddr = "127.0.0.1:0"
	}
	if e = os.MkdirAll(cfg.DataDir, 0700); e != nil {
		return nil, e
	}
	a := &Agent{cfg: cfg, owners: map[string]string{}, approvals: map[string]approval{}, connection: ConnectionSnapshot{State: Connecting}}
	data, e := os.ReadFile(filepath.Join(cfg.DataDir, "agent.json"))
	if e == nil {
		if e = json.Unmarshal(data, &a.creds); e != nil {
			return nil, e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	if a.creds.ServerURL != "" && a.creds.ServerURL != cfg.ServerURL {
		return nil, errors.New("agent data belongs to another server; use a separate --data directory")
	}
	if cfg.InstallationID != "" {
		a.creds.InstallationID = cfg.InstallationID
	}
	if a.creds.InstallationID == "" {
		a.creds.InstallationID = uuid.NewString()
	}
	a.creds.ServerURL = cfg.ServerURL
	return a, nil
}

func (a *Agent) StateDir() string { return a.cfg.DataDir }

func (a *Agent) Connected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.relay != nil
}

func (a *Agent) ConnectionSnapshot() ConnectionSnapshot {
	a.connectionMu.RLock()
	defer a.connectionMu.RUnlock()
	snapshot := a.connection
	if snapshot.State == "" {
		snapshot.State = Connecting
	}
	return snapshot
}

func (a *Agent) setConnection(state ConnectionState, err error, retryAt time.Time) {
	a.connectionMu.Lock()
	defer a.connectionMu.Unlock()
	a.connection.State = state
	a.connection.Error = ""
	if err != nil {
		a.connection.Error = connectionAdvice(err)
	}
	a.connection.NextRetryAt = nil
	if !retryAt.IsZero() {
		retryAt = retryAt.UTC()
		a.connection.NextRetryAt = &retryAt
	}
	if state == Connected {
		now := time.Now().UTC()
		a.connection.LastConnectedAt = &now
	}
}

func (a *Agent) credentials() (creds, bool) {
	a.credsMu.RLock()
	defer a.credsMu.RUnlock()
	return a.creds, a.credsDirty
}

func (a *Agent) save() error {
	a.credsMu.Lock()
	defer a.credsMu.Unlock()
	b, e := json.MarshalIndent(a.creds, "", "  ")
	if e != nil {
		return e
	}
	p := filepath.Join(a.cfg.DataDir, "agent.json")
	if e = os.WriteFile(p+".tmp", b, 0600); e != nil {
		return e
	}
	if e = os.Rename(p+".tmp", p); e != nil {
		return e
	}
	a.credsDirty = false
	return nil
}
func (a *Agent) request(ctx context.Context, path string, input, output any) error {
	return a.requestMethod(ctx, "POST", path, input, output)
}

func (a *Agent) requestMethod(ctx context.Context, method, path string, input, output any) error {
	raw, e := json.Marshal(input)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, method, a.cfg.ServerURL+path, bytes.NewReader(raw))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	credentials, _ := a.credentials()
	req.Header.Set("Authorization", "Bearer "+credentials.Token)
	if path == "/api/v1/agent/enroll" {
		req.Header.Set("X-Enrollment-Key", a.cfg.EnrollmentKey)
	}
	resp, e := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	data, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if e != nil {
		return e
	}
	if resp.StatusCode >= 300 {
		return &httpStatusError{status: resp.StatusCode}
	}
	if output == nil {
		return nil
	}
	return json.Unmarshal(data, output)
}
func (a *Agent) ensureEnrolled(ctx context.Context) error {
	a.enrollMu.Lock()
	defer a.enrollMu.Unlock()
	credentials, dirty := a.credentials()
	if dirty {
		if err := a.save(); err != nil {
			return err
		}
	}
	return a.enrollLocked(ctx, credentials, false)
}

func (a *Agent) enrollLocked(ctx context.Context, credentials creds, force bool) error {
	if !force && credentials.Token != "" && (credentials.ExpiresAt == 0 || credentials.ExpiresAt > time.Now().Add(24*time.Hour).Unix()) {
		return nil
	}
	var out protocol.EnrollResponse
	if e := a.request(ctx, "/api/v1/agent/enroll", protocol.EnrollRequest{Name: a.cfg.Name, OS: runtime.GOOS, Arch: runtime.GOARCH, AppVersion: release.VersionName, InstallationID: credentials.InstallationID}, &out); e != nil {
		return e
	}
	a.credsMu.Lock()
	a.creds.AgentID = out.AgentID
	a.creds.EnvironmentID = out.EnvironmentID
	a.creds.Token = out.Token
	a.creds.ExpiresAt = out.ExpiresAt
	a.credsDirty = true
	a.credsMu.Unlock()
	return a.save()
}
func (a *Agent) Pair(ctx context.Context) (*protocol.PairStartResponse, error) {
	if e := a.ensureEnrolled(ctx); e != nil {
		return nil, e
	}
	var out protocol.PairStartResponse
	if e := a.request(ctx, "/api/v1/agent/pair", protocol.PairStartRequest{ManualCode: !a.cfg.SecureRelay, Secure: a.cfg.SecureRelay}, &out); e != nil {
		return nil, e
	}
	if a.cfg.SecureRelay {
		if err := a.createSecureInvitation(&out); err != nil {
			return nil, err
		}
	}
	return &out, nil
}
func (a *Agent) ListDevices(ctx context.Context) ([]protocol.ClientInfo, error) {
	if err := a.ensureEnrolled(ctx); err != nil {
		return nil, err
	}
	var out struct {
		Data []protocol.ClientInfo `json:"data"`
	}
	if err := a.requestMethod(ctx, "GET", "/api/v1/agent/clients", nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func (a *Agent) RemoveDevice(ctx context.Context, clientID string) error {
	if err := a.ensureEnrolled(ctx); err != nil {
		return err
	}
	if a.cfg.SecureRelay {
		if err := a.removeSecureDevice(clientID); err != nil {
			return err
		}
	}
	return a.requestMethod(ctx, "DELETE", "/api/v1/agent/clients/"+url.PathEscape(clientID), nil, nil)
}

func (a *Agent) Run(ctx context.Context) error {
	a.setConnection(Connecting, nil, time.Time{})
	for {
		e := a.ensureEnrolled(ctx)
		if e == nil {
			break
		}
		if _, dirty := a.credentials(); !dirty {
			return e
		}
		log.Printf("save renewed agent credentials: %v; retrying", e)
		a.setConnection(Retrying, e, time.Now().Add(3*time.Second))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	if !a.cfg.NoAutoPair {
		if p, e := a.Pair(ctx); e == nil {
			instructions, e := a.PairingInstructions(p)
			if e != nil {
				return e
			}
			log.Print(instructions)
		} else {
			return e
		}
	}
	if a.cfg.Mock {
		mock, e := appserver.StartMock(a.cfg.MockAddr)
		if e != nil {
			return e
		}
		defer mock.Close()
		a.cfg.AppServerURL = "ws://" + mock.Addr
	}
	for ctx.Err() == nil {
		var app *appserver.Client
		var e error
		if a.cfg.AppServerURL != "" {
			app, e = appserver.Dial(ctx, a.cfg.AppServerURL, a.cfg.AppServerToken)
		} else {
			app, e = appserver.Launch(ctx, a.cfg.CodexPath, a.cfg.Cwd)
		}
		if e != nil {
			a.setConnection(Degraded, errors.New("Codex app-server unavailable"), time.Time{})
			return e
		}
		initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, e = app.Initialize(initCtx, "codexrm-agent", protocol.Version)
		cancel()
		if e != nil {
			a.closeTextApp()
			a.closeTextApp()
			_ = app.Close()
			a.setConnection(Degraded, errors.New("Codex initialization failed"), time.Time{})
			return fmt.Errorf("initialize Codex: %w", e)
		}
		log.Printf("Codex ready; workspace: %s; mock=%v", a.cfg.Cwd, a.cfg.Mock)
		if a.cfg.OnReady != nil {
			a.cfg.OnReady()
		}
		a.mu.Lock()
		a.approvals = map[string]approval{}
		a.mu.Unlock()
		appCtx, stop := context.WithCancel(ctx)
		a.providerApp.Store(app)
		a.queueMu.Lock()
		a.queues = map[string]*messageQueue{}
		a.queueCtx = appCtx
		a.queueMu.Unlock()
		app.OnNotification(func(raw json.RawMessage) {
			if a.handleWxHistoryTool(app, raw) {
				return
			}
			a.forward(raw)
			a.queueNotification(appCtx, app, raw)
		})
		if a.cfg.SecureRelay {
			go a.runLAN(appCtx, app)
		}
		if e = a.startConfiguredOneBot(appCtx, app); e != nil {
			// A local bot configuration must not take down browser access to Codex.
			log.Printf("OneBot startup failed; continuing without OneBot: %v", e)
		}
		go func() {
			select {
			case <-app.Done():
				stop()
			case <-appCtx.Done():
			}
		}()
		for appCtx.Err() == nil {
			if e = a.ensureEnrolled(appCtx); e != nil {
				log.Printf("renew agent registration: %v", e)
				a.setConnection(Retrying, e, time.Now().Add(3*time.Second))
				select {
				case <-appCtx.Done():
				case <-time.After(3 * time.Second):
				}
				continue
			}
			credential, _ := a.credentials()
			e = a.session(appCtx, app)
			if appCtx.Err() != nil {
				break
			}
			var status *httpStatusError
			if errors.As(e, &status) && status.status == http.StatusUnauthorized {
				a.enrollMu.Lock()
				current, _ := a.credentials()
				if current.Token == credential.Token {
					if enrollErr := a.enrollLocked(appCtx, current, true); enrollErr != nil {
						log.Printf("recover agent registration: %v", enrollErr)
					}
				}
				a.enrollMu.Unlock()
			}
			log.Printf("relay disconnected: %v; retry in 3s", e)
			a.setConnection(Retrying, e, time.Now().Add(3*time.Second))
			select {
			case <-appCtx.Done():
			case <-time.After(3 * time.Second):
			}
		}
		a.providerApp.Store(nil)
		a.stopOneBot()
		stop()
		_ = app.Close()
		a.clearWxRecordApp(app)
		if ctx.Err() == nil {
			log.Printf("Codex exited; restarting in 3s")
			a.setConnection(Degraded, errors.New("Codex app-server exited"), time.Now().Add(3*time.Second))
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
		}
	}
	return ctx.Err()
}
func (a *Agent) write(conn *websocket.Conn, env protocol.Envelope) error {
	if conn == nil {
		return errors.New("relay offline")
	}
	if env.Type == protocol.TypeServerMessage {
		env.Message, _ = protocol.WithoutInlineFiles(env.Message)
	}
	frame, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if len(frame) > protocol.MaxEnvelopeSize {
		return errors.New("relay frame exceeds size limit")
	}
	return a.writeRelayFrame(conn, frame)
}

func (a *Agent) writeRelayFrame(conn *websocket.Conn, frame []byte) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		log.Printf("relay write: %v", err)
		_ = conn.Close()
		return err
	}
	return nil
}

func (a *Agent) forward(raw json.RawMessage) {
	a.notifyOneBot(raw)
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	owner := ""
	var method string
	_ = json.Unmarshal(m["method"], &method)
	var meta struct {
		ThreadID   string          `json:"threadId"`
		TurnID     string          `json:"turnId"`
		IsBlocking *bool           `json:"isBlocking"`
		RequestID  json.RawMessage `json:"requestId"`
		Turn       struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(m["params"], &meta)
	if method == "item/completed" {
		var item struct{ Item assistantFileItem }
		if json.Unmarshal(m["params"], &item) == nil {
			a.mentionedFiles.addItem(meta.ThreadID, item.Item)
		}
	}
	a.mu.Lock()
	if method == "serverRequest/resolved" {
		for key, pending := range a.approvals {
			if string(pending.ID) == string(meta.RequestID) && pending.ThreadID == meta.ThreadID {
				delete(a.approvals, key)
				var params map[string]json.RawMessage
				_ = json.Unmarshal(m["params"], &params)
				params["requestId"] = mustJSON(key)
				m["params"] = mustJSON(params)
				raw = mustJSON(m)
				owner = pending.Owner
				break
			}
		}
	}
	if method == "turn/completed" {
		a.releaseWxRecordTurn(meta.ThreadID, meta.Turn.ID)
		for key, pending := range a.approvals {
			if pending.ThreadID == meta.ThreadID && pending.TurnID == meta.Turn.ID &&
				(!pending.NonBlocking || meta.Turn.Status != "completed") {
				delete(a.approvals, key)
			}
		}
	}
	if id := m["id"]; len(id) > 0 && string(id) != "null" {
		owner = a.owners[meta.ThreadID]
		key := "approval_" + uuid.NewString()
		m["id"] = mustJSON(key)
		raw = mustJSON(m)
		a.approvals[key] = approval{ID: id, Raw: raw, Owner: owner, ThreadID: meta.ThreadID, TurnID: meta.TurnID,
			NonBlocking: method == "item/tool/requestUserInput" && meta.IsBlocking != nil && !*meta.IsBlocking}
	}
	a.mu.Unlock()
	a.publishEvent(protocol.Envelope{Type: protocol.TypeServerMessage, ClientID: owner, Message: raw})
	if extra := a.robotNotificationClient(owner, meta.ThreadID); extra != "" {
		a.publishEvent(protocol.Envelope{Type: protocol.TypeServerMessage, ClientID: extra, Message: raw})
	}
}
func (a *Agent) session(ctx context.Context, app *appserver.Client) error {
	u, _ := url.Parse(a.cfg.ServerURL + "/api/v1/agent/ws")
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	credentials, _ := a.credentials()
	conn, response, e := (&websocket.Dialer{HandshakeTimeout: 10 * time.Second}).DialContext(ctx, u.String(), http.Header{
		"Authorization":           []string{"Bearer " + credentials.Token},
		protocol.AppVersionHeader: []string{release.VersionName},
	})
	if e != nil {
		if response != nil {
			return &httpStatusError{status: response.StatusCode}
		}
		return e
	}
	defer conn.Close()
	conn.SetReadLimit(protocol.MaxEnvelopeSize)
	refreshReadDeadline := func() error { return conn.SetReadDeadline(time.Now().Add(65 * time.Second)) }
	if err := refreshReadDeadline(); err != nil {
		return err
	}
	conn.SetPongHandler(func(string) error { return refreshReadDeadline() })
	ping := conn.PingHandler()
	conn.SetPingHandler(func(payload string) error {
		if err := refreshReadDeadline(); err != nil {
			return err
		}
		return ping(payload)
	})
	events := newRelayEventQueue(conn)
	a.eventMu.Lock()
	a.eventClients = nil
	a.relayEvents = events
	a.mu.Lock()
	a.relay = conn
	a.mu.Unlock()
	a.eventMu.Unlock()
	a.setConnection(Connected, nil, time.Time{})
	defer func() {
		a.eventMu.Lock()
		defer a.eventMu.Unlock()
		if a.relayEvents == events {
			a.relayEvents = nil
		}
		a.mu.Lock()
		if a.relay == conn {
			a.relay = nil
		}
		a.mu.Unlock()
	}()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-ctx.Done(); _ = conn.Close() }()
	var secured *encryptedRelay
	if a.cfg.SecureRelay {
		secured = newEncryptedRelay(a, conn)
	}
	go a.writeEncryptedRelayEvents(ctx, events, secured)
	log.Printf("relay connected as %s (%s)", credentials.AgentID, a.cfg.Name)
	hello := protocol.P2P(protocol.P2PKindHello, "")
	hello.LAN = true
	hello.DualNATPunch = true
	hello.ClientInitiated = true
	hello.IPv6 = true
	hello.NATDiagnostic = true
	if e = a.write(conn, protocol.Envelope{Type: protocol.TypeP2P, P2P: hello}); e != nil {
		return e
	}
	slots := make(chan struct{}, 32)
	send := func(env protocol.Envelope) error { return a.write(conn, env) }
	for {
		var env protocol.Envelope
		if e = conn.ReadJSON(&env); e != nil {
			return e
		}
		if err := refreshReadDeadline(); err != nil {
			return err
		}
		if env.Type == protocol.TypePing {
			if err := a.write(conn, protocol.Envelope{Type: protocol.TypePong, PingID: env.PingID}); err != nil {
				return err
			}
			continue
		}
		if secured != nil {
			if env.Type == protocol.TypeSecure {
				secured.receive(env, func(inner protocol.Envelope, reply sendEnvelope) {
					a.dispatch(ctx, app, reply, slots, inner, nil)
				})
			} else if env.Type == protocol.TypeP2P && env.P2P != nil {
				key := secured.directKey(env.ClientID)
				if key != nil || env.P2P.Kind == protocol.P2PKindRouting || env.P2P.Kind == protocol.P2PKindLeave {
					a.directSignalWithKey(ctx, app, conn, slots, env, key)
				}
			}
			continue
		}
		if env.Type == protocol.TypeP2P {
			a.directSignal(ctx, app, conn, slots, env)
			continue
		}
		if env.Type != protocol.TypeClientMessage {
			continue
		}
		a.dispatch(ctx, app, send, slots, env, nil)
	}
}

func (a *Agent) dispatch(ctx context.Context, app *appserver.Client, send sendEnvelope, slots chan struct{}, env protocol.Envelope, direct *directSession) {
	select {
	case slots <- struct{}{}:
		go func() {
			defer func() { <-slots }()
			if ctx.Err() == nil {
				a.handle(ctx, app, send, env, direct)
			}
		}()
	default:
		a.respond(send, env, nil, &appserver.RPCError{Code: -32001, Message: "too many concurrent requests"})
	}
}

func (a *Agent) handle(ctx context.Context, app *appserver.Client, send sendEnvelope, env protocol.Envelope, direct *directSession) {
	if ctx.Err() != nil {
		return
	}
	var probe struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(env.Message, &probe)
	if probe.Method == "codexrm/update" {
		a.queueMu.Lock()
		updating := a.updating
		a.queueMu.Unlock()
		if updating {
			a.respond(send, env, nil, updateInProgressError)
			return
		}
		if a.cfg.CheckForUpdate == nil {
			a.respond(send, env, nil, &appserver.RPCError{Code: -32601, Message: "此 Agent 不支持远程更新"})
			return
		}
		result, err := a.cfg.CheckForUpdate()
		a.respond(send, env, mustJSON(result), err)
		return
	}
	if !a.beginRequest() {
		a.respond(send, env, nil, updateInProgressError)
		return
	}
	defer a.endRequest()
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(env.Message, &req) != nil {
		return
	}
	if req.Method == "" {
		if env.ClientKind == protocol.ClientKindRobot {
			return
		}
		if len(req.Result) == 0 && len(req.Error) == 0 {
			return
		}
		var key string
		_ = json.Unmarshal(req.ID, &key)
		a.mu.Lock()
		p, ok := a.approvals[key]
		if ok && !p.Sending && (p.Owner == "" || p.Owner == env.ClientID) {
			p.Sending = true
			a.approvals[key] = p
		} else {
			ok = false
		}
		a.mu.Unlock()
		if ok {
			raw := map[string]json.RawMessage{"id": p.ID}
			if len(req.Error) > 0 {
				raw["error"] = req.Error
			} else if len(req.Result) > 0 {
				raw["result"] = req.Result
			} else {
				return
			}
			var q *messageQueue
			if p.ThreadID != "" && retainsSessionApproval(p.Raw, raw["result"]) {
				q = a.threadQueue(p.ThreadID)
				// Serialize the grant with idle release, even if Codex resolves
				// the request and completes the turn before WriteRaw returns.
				q.RPCMu.Lock()
			}
			err := app.WriteRaw(mustJSON(raw))
			if q != nil {
				if err == nil {
					a.queueMu.Lock()
					q.SessionApproval = true
					a.queueMu.Unlock()
				}
				q.RPCMu.Unlock()
			}
			a.mu.Lock()
			if current, exists := a.approvals[key]; exists {
				if err == nil {
					delete(a.approvals, key)
				} else {
					current.Sending = false
					a.approvals[key] = current
				}
			}
			a.mu.Unlock()
			if err == nil {
				a.publishEvent(protocol.Envelope{Type: protocol.TypeServerMessage, Message: mustJSON(map[string]any{"method": "codexrm/request/resolved", "params": map[string]any{"id": key}})})
				a.queueMu.Lock()
				lifetime := a.queueCtx
				a.queueMu.Unlock()
				if lifetime != nil {
					go a.releaseIdleThread(lifetime, app, p.ThreadID)
				}
			} else {
				a.publishEvent(protocol.Envelope{Type: protocol.TypeServerMessage, ClientID: env.ClientID, Message: mustJSON(map[string]any{"method": "codexrm/request/failed", "params": map[string]any{"id": key, "message": err.Error()}})})
			}
		}
		return
	}
	if req.Method == "initialize" || req.Method == "initialized" {
		a.respond(send, env, nil, &appserver.RPCError{Code: -32600, Message: "Agent owns initialization"})
		return
	}
	if req.Method == "codexrm/info" {
		// Older Agents ignore these params and still return a valid response.
		// A probe needs no capabilities or approval replay. Use the same RPC
		// path as interactive traffic, not a separate faster measurement path.
		if direct != nil {
			var options struct {
				LatencyProbe bool `json:"latencyProbe"`
			}
			if json.Unmarshal(req.Params, &options) == nil && options.LatencyProbe {
				a.respond(send, env, json.RawMessage(`{}`), nil)
				return
			}
		}
		a.respond(send, env, mustJSON(map[string]any{"name": a.cfg.Name, "cwd": a.cfg.Cwd, "mock": a.cfg.Mock, "version": release.VersionName, "os": runtime.GOOS, "workspaces": true, "threadSnapshots": true, "inlineVisualizations": a.supportsDirectFiles(), "conversationTools": true, "providerModels": true, "providerManagement": a.supportsProviderManagement(), "sameModelProviderSwitch": !a.cfg.Mock && a.cfg.AppServerURL == "", "threadModelProviderSwitch": !a.cfg.Mock && a.cfg.AppServerURL == "" && a.cfg.DataDir != "", "messageQueue": true, "images": a.localEvent == nil, "directFiles": a.localEvent == nil && a.allowsFileManagement(), "computerFiles": a.localEvent == nil && a.allowsFileManagement(), "mentionedFileDownloads": a.localEvent == nil && a.supportsDirectFiles(), "attachments": map[string]any{"maxSize": attachmentMaxSize, "chunkSize": attachmentChunkSize, "relayImages": true}, "phoneTransfer": a.localEvent == nil, "phoneTransferBatches": a.localEvent == nil && desktop.ReceivesFiles, "desktopShare": a.localEvent == nil && a.cfg.AllowFileManagement && desktop.ReceivesFiles, "remoteUpdate": a.cfg.CheckForUpdate != nil, "transport": map[bool]string{true: "websocket", false: "stdio"}[a.cfg.AppServerURL != ""]}), nil)
		a.mu.Lock()
		pending := []approval{}
		for _, p := range a.approvals {
			if p.Owner == "" || p.Owner == env.ClientID {
				pending = append(pending, p)
			}
		}
		a.mu.Unlock()
		for _, p := range pending {
			_ = send.write(protocol.Envelope{Type: protocol.TypeServerMessage, ClientID: env.ClientID, Message: p.Raw})
		}
		return
	}
	var params map[string]any
	if len(req.Params) > 0 && string(req.Params) != "null" {
		if e := json.Unmarshal(req.Params, &params); e != nil {
			a.respond(send, env, nil, &appserver.RPCError{Code: -32602, Message: e.Error()})
			return
		}
	}
	if params == nil {
		params = map[string]any{}
	}
	if env.ClientKind == protocol.ClientKindRobot {
		a.handleRobot(ctx, app, send, env, req.Method, params)
		return
	}
	if err := remoteRPCPermission(req.Method, params); err != nil {
		a.respond(send, env, nil, err)
		return
	}
	if direct == nil && protocol.RequiresDirectFileTransfer(req.Method, env.Message) {
		a.respond(send, env, nil, errors.New(protocol.FileTransferDirectRequired))
		return
	}
	if strings.HasPrefix(req.Method, protocol.PhoneTransferPrefix) {
		if len(req.ID) != 0 {
			result, err := a.phoneTransferRPC(ctx, env.ClientID, req.Method, params, direct)
			a.respond(send, env, mustJSON(result), err)
		}
		return
	}
	if strings.HasPrefix(req.Method, protocol.DirectFilePrefix) {
		if len(req.ID) != 0 {
			result, err := a.directFileRPC(ctx, app, direct, req.Method, params)
			a.respond(send, env, mustJSON(result), err)
		}
		return
	}
	if strings.HasPrefix(req.Method, "codexrm/provider/") {
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		result, err := a.providerSettingsRPC(callCtx, app, req.Method, req.Params, direct != nil || a.localEvent != nil)
		a.respond(send, env, mustJSON(result), err)
		return
	}
	if req.Method == "codexrm/model/list" {
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		result, err := a.modelCatalog(callCtx, app)
		a.respond(send, env, mustJSON(result), err)
		return
	}
	if req.Method == "codexrm/visualization/read" {
		result, err := a.readVisualization(ctx, app, params)
		a.respond(send, env, mustJSON(result), err)
		return
	}
	if req.Method == "codexrm/thread/snapshot" {
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		result, err := a.threadSnapshot(callCtx, app, params)
		if err == nil {
			a.mentionedFiles.addThread(result)
		}
		a.respond(send, env, result, err)
		return
	}
	switch req.Method {
	case "codexrm/thread/search", "codexrm/thread/pins", "codexrm/thread/pin", "codexrm/thread/related":
		callCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
		defer cancel()
		result, err := a.conversationRPC(callCtx, app, req.Method, params)
		a.respond(send, env, mustJSON(result), err)
		return
	}
	if req.Method == "codexrm/workspace/diff" {
		callCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		result, err := a.workspaceDiff(callCtx, app, params)
		a.respond(send, env, mustJSON(result), err)
		return
	}
	if strings.HasPrefix(req.Method, "codexrm/queue/") {
		result, err := a.queueRPC(ctx, app, env.ClientID, req.Method, params)
		a.respond(send, env, mustJSON(result), err)
		return
	}
	if strings.HasPrefix(req.Method, "codexrm/attachment/") || strings.HasPrefix(req.Method, protocol.ChatImagePrefix) {
		result, err := a.attachmentRPC(env.ClientID, req.Method, params, direct)
		a.respond(send, env, mustJSON(result), err)
		return
	}
	if req.Method == "codexrm/image/read" {
		result, err := a.remoteImage(ctx, app, params)
		a.respond(send, env, mustJSON(result), err)
		return
	}
	if req.Method == switchProviderMethod {
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		result, err := a.callThread(callCtx, app, env.ClientID, req.Method, params)
		a.respond(send, env, result, err)
		return
	}
	if strings.HasPrefix(req.Method, "codexrm/") {
		result, err := a.workspaceRPC(req.Method, params)
		a.respond(send, env, mustJSON(result), err)
		return
	}
	if req.Method == "thread/start" && params["cwd"] == nil {
		params["cwd"] = a.cfg.Cwd
	}
	if len(req.ID) == 0 {
		_ = app.Notify(req.Method, params)
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result, e := a.callThread(callCtx, app, env.ClientID, req.Method, params)
	if e == nil {
		a.mentionedFiles.addHistory(req.Method, params, result)
	}
	a.respond(send, env, result, e)
}
func (a *Agent) respond(send sendEnvelope, env protocol.Envelope, result json.RawMessage, err error) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(env.Message, &req)
	if len(req.ID) == 0 {
		return
	}
	tooLarge := &appserver.RPCError{Code: -32000, Message: "响应内容过大，无法通过当前连接传输。"}
	switch req.Method {
	case "thread/start", "turn/start", "turn/steer", "codexrm/queue/add":
		// The request already ran if only the successful reply exceeded the relay limit.
		tooLarge.Code = protocol.RPCOutcomeUnknownCode
	}
	maxPayload := maxRPCPayloadBytes
	if a.cfg.SecureRelay {
		maxPayload = securechannel.MaxPlaintext - (64 << 10)
	}
	if err == nil && len(result) > maxPayload {
		err = tooLarge
	}
	resp := map[string]any{"id": req.ID}
	var rpcError *appserver.RPCError
	if err != nil {
		rpcError = &appserver.RPCError{Code: -32000, Message: err.Error()}
		var unknown *appserver.OutcomeUnknown
		var original *appserver.RPCError
		if errors.As(err, &unknown) {
			rpcError.Code = protocol.RPCOutcomeUnknownCode
		} else if errors.As(err, &original) {
			rpcError = original
		}
		resp["error"] = rpcError
	} else {
		if len(result) == 0 {
			result = json.RawMessage("null")
		}
		resp["result"] = result
	}
	message := mustJSON(resp)
	if len(message) > maxPayload {
		if rpcError != nil {
			tooLarge.Code = rpcError.Code
		}
		message = mustJSON(map[string]any{"id": req.ID, "error": tooLarge})
	}
	_ = send.write(protocol.Envelope{Type: protocol.TypeServerMessage, ClientID: env.ClientID, StreamID: env.StreamID, Message: message})
}
func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
