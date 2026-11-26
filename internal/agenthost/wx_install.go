package agenthost

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

func (h *Host) wxStartInstall(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var input struct {
		InstallDir       string   `json:"installDir"`
		OptionalServices []string `json:"optionalServices"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			failure(w, err)
			return
		}
	}
	dir := strings.TrimSpace(input.InstallDir)
	if dir == "" {
		dir = h.wxInstallDirectory()
	}
	if filepath.Clean(dir) != h.wxInstallDirectory() {
		failure(w, errors.New("安装目录必须为 Agent 的 wxbot 数据目录"))
		return
	}
	if err := validateWXOptionalServices(input.OptionalServices); err != nil {
		failure(w, err)
		return
	}
	task, err := h.beginWXTask("install", wxInstallSteps(), dir, func(task *wxTask) {
		task.OptionalServices = input.OptionalServices
	})
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, task)
	h.startWXTask(task, h.runWXInstall)
}

func (h *Host) wxStartTakeover(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var input struct {
		Token      string `json:"token"`
		OAuthCode  string `json:"oauthCode"`
		ComposeDir string `json:"composeDir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		failure(w, err)
		return
	}
	token := strings.TrimSpace(input.Token)
	oauthCode := strings.TrimSpace(input.OAuthCode)
	if token == "" && oauthCode == "" {
		failure(w, errors.New("请输入现有后台的管理凭据"))
		return
	}
	dir := strings.TrimSpace(input.ComposeDir)
	task, err := h.beginWXTask("takeover", wxTakeoverSteps(), dir, func(task *wxTask) {
		wxTakeoverTokens.Store(task.ID, wxTakeoverCredential{Token: token, OAuthCode: oauthCode})
	})
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, task)
	h.startWXTask(task, h.runWXTakeover)
}

func (h *Host) updateWXTask(task *wxTask, mutate func(*wxTask)) error {
	wxRuntime.mu.Lock()
	defer wxRuntime.mu.Unlock()
	latest, err := readJSONFile[wxTask](h.wxTaskPath())
	if err != nil {
		return err
	}
	if latest == nil || latest.ID != task.ID {
		return errors.New("WeChat task record is missing or was replaced")
	}
	mutate(latest)
	if err := h.saveWXTask(latest); err != nil {
		return fmt.Errorf("save WeChat task progress: %w", err)
	}
	*task = *latest
	return nil
}

func (h *Host) runWXInstall(ctx context.Context, task *wxTask) error {
	fail := func(id string, err error) error {
		persistErr := h.updateWXTask(task, func(current *wxTask) {
			setWXStep(current, id, "failed", err.Error())
		})
		if persistErr != nil {
			return errors.Join(err, persistErr)
		}
		return err
	}
	step := func(id string) error {
		return h.updateWXTask(task, func(current *wxTask) {
			setWXStep(current, id, "running", "")
		})
	}
	done := func(id string) error {
		return h.updateWXTask(task, func(current *wxTask) {
			setWXStep(current, id, "completed", "")
		})
	}

	if err := step("check-env"); err != nil {
		return err
	}
	probe, err := h.collectWXProbe(ctx)
	if err != nil {
		return fail("check-env", err)
	}
	if !probe.OSSupported {
		return fail("check-env", errors.New("首批仅支持 Debian、Ubuntu 的 amd64 主机"))
	}
	if probe.Existing != nil && probe.Existing.Found && !wxOwnsCompose(task.InstallDir) {
		return fail("check-env", errors.New("本机已有微信机器人，请改用「接管已有安装」"))
	}
	if probe.DiskFreeBytes > 0 && probe.DiskFreeBytes < 4<<30 {
		return fail("check-env", errors.New("磁盘剩余空间不足 4GB，无法安装"))
	}
	images, err := wxImageLock()
	if err != nil {
		return fail("check-env", err)
	}
	if !wxOwnsCompose(task.InstallDir) {
		for _, port := range probe.Ports {
			if port.InUse {
				return fail("check-env", fmt.Errorf("本机端口 %d 已占用", port.Port))
			}
		}
	}
	if err := done("check-env"); err != nil {
		return err
	}

	if err := step("install-docker"); err != nil {
		return err
	}
	if !probe.Docker.Usable || !probe.Compose.Usable {
		if err := runWXBootstrap(ctx); err != nil {
			if !errors.Is(err, errWXBootstrapAuthorization) {
				return fail("install-docker", err)
			}
			if saveErr := h.updateWXTask(task, func(current *wxTask) {
				current.PrivilegeCommand = wxHelperCommand()
				setWXStep(current, "install-docker", "needs_privilege", "首次安装需要一次性系统授权，以安装 Docker 并管理本机微信机器人服务。")
			}); saveErr != nil {
				return saveErr
			}
			return err
		}
		if status := wxDockerStatus(ctx); !status.Usable {
			if !status.socketPermissionDenied {
				return fail("install-docker", errors.New(status.Error))
			}
			message := "Docker 已安装，请为当前 Agent 账号授权访问 Docker，然后重启 Agent 并重试。"
			if account, err := user.Current(); err == nil && validWXLocalUser(account.Username) {
				if saveErr := h.updateWXTask(task, func(current *wxTask) {
					current.PrivilegeCommand = "sudo usermod -aG docker -- " + account.Username
					setWXStep(current, "install-docker", "needs_privilege", message)
				}); saveErr != nil {
					return saveErr
				}
				return errors.New(message)
			}
			return fail("install-docker", errors.New(message))
		}
		if status := wxComposeStatus(ctx); !status.Usable {
			return fail("install-docker", fmt.Errorf("Docker 已启动，但 Compose 插件不可用: %s", status.Error))
		}
	}
	if err := done("install-docker"); err != nil {
		return err
	}

	if err := step("prepare-files"); err != nil {
		return err
	}
	if err := prepareWXInstallDir(ctx, task.InstallDir, images, task.OptionalServices); err != nil {
		return fail("prepare-files", err)
	}
	if err := done("prepare-files"); err != nil {
		return err
	}

	if err := step("pull-images"); err != nil {
		return err
	}
	if err := h.pullWXImages(ctx, task, images, task.OptionalServices); err != nil {
		return fail("pull-images", err)
	}
	if err := done("pull-images"); err != nil {
		return err
	}

	if err := step("start-services"); err != nil {
		return err
	}
	if err := wxCompose(ctx, task.InstallDir, "up", "-d", "--pull", "never"); err != nil {
		return fail("start-services", err)
	}
	if err := done("start-services"); err != nil {
		return err
	}

	if err := step("wait-healthy"); err != nil {
		return err
	}
	token, err := wxInstallLoginToken(task.InstallDir)
	if err != nil {
		return fail("wait-healthy", err)
	}
	client, err := waitWXHealthy(ctx, token, 3*time.Minute)
	if err != nil {
		return fail("wait-healthy", err)
	}
	if err := done("wait-healthy"); err != nil {
		return err
	}
	if err := h.ensureWXRelay(ctx); err != nil {
		return fail("wait-healthy", err)
	}
	if err := step("create-robot"); err != nil {
		return err
	}
	if err = client.ensureFirstRobot(ctx); err != nil {
		return fail("create-robot", err)
	}
	if err := done("create-robot"); err != nil {
		return err
	}
	if err = writeJSONFile(h.wxAdminSecretPath(), token); err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := h.saveWXInstance(&wxInstance{Mode: "installed", ComposeDir: task.InstallDir, InstalledAt: now}); err != nil {
		return err
	}
	return nil
}

func (h *Host) runWXTakeover(ctx context.Context, task *wxTask) error {
	fail := func(id string, err error) error {
		wxTakeoverTokens.Delete(task.ID)
		persistErr := h.updateWXTask(task, func(current *wxTask) {
			setWXStep(current, id, "failed", err.Error())
		})
		if persistErr != nil {
			return errors.Join(err, persistErr)
		}
		return err
	}
	step := func(id string) error {
		return h.updateWXTask(task, func(current *wxTask) {
			setWXStep(current, id, "running", "")
		})
	}
	done := func(id string) error {
		return h.updateWXTask(task, func(current *wxTask) {
			setWXStep(current, id, "completed", "")
		})
	}

	if err := step("detect"); err != nil {
		return err
	}
	probe, err := h.collectWXProbe(ctx)
	if err != nil {
		return fail("detect", err)
	}
	if probe.Existing == nil || !probe.Existing.Found {
		return fail("detect", errors.New("没有识别到可接管的微信机器人"))
	}
	existing := probe.Existing
	if task.InstallDir != "" {
		existing.ComposeDir = task.InstallDir
	}
	if err := done("detect"); err != nil {
		return err
	}

	if err := step("verify-credentials"); err != nil {
		return err
	}
	pending, _ := wxTakeoverTokens.Load(task.ID)
	credential, _ := pending.(wxTakeoverCredential)
	token := credential.Token
	if credential.OAuthCode != "" {
		authCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		token, err = wxAdminOAuthCredential(authCtx, credential.OAuthCode)
		cancel()
		if err != nil {
			return fail("verify-credentials", err)
		}
		wxTakeoverTokens.Store(task.ID, wxTakeoverCredential{Token: token})
	}
	verifiedPath := filepath.Join(h.StateDir(), "wx-takeover-verified.json")
	if token == "" {
		verified, err := readJSONFile[struct{ ID, Token string }](verifiedPath)
		if err != nil || verified == nil || verified.ID != task.ID {
			return fail("verify-credentials", errors.New("请重新输入现有后台的管理凭据"))
		}
		token = verified.Token
	}
	client, err := wxAdminLogin(ctx, token)
	if err != nil {
		return fail("verify-credentials", err)
	}
	if err := writeJSONFile(verifiedPath, struct{ ID, Token string }{task.ID, token}); err != nil {
		return fail("verify-credentials", err)
	}
	wxTakeoverTokens.Delete(task.ID)
	if err := done("verify-credentials"); err != nil {
		return err
	}

	if err := step("snapshot"); err != nil {
		return err
	}
	rows, err := client.robots(ctx)
	if err != nil {
		return fail("snapshot", err)
	}
	existing.Robots = rows
	if err := writeJSONFile(h.wxAdminSecretPath(), token); err != nil {
		return fail("snapshot", err)
	}
	if err := done("snapshot"); err != nil {
		return err
	}

	if err := step("probe-capabilities"); err != nil {
		return err
	}
	for i := range existing.Accounts {
		account := inspectWXAccount(ctx, existing.Accounts[i].ID)
		existing.Accounts[i].Capabilities = account.Capabilities
	}
	if err := done("probe-capabilities"); err != nil {
		return err
	}

	instance := h.loadWXInstance()
	if instance != nil && instance.Mode == "installed" && instance.ComposeDir == existing.ComposeDir {
		return os.Remove(verifiedPath)
	}
	if err := h.saveWXInstance(&wxInstance{
		Mode:          "taken-over",
		ComposeDir:    existing.ComposeDir,
		TakenOverAt:   time.Now().UTC(),
		AdminVerified: true,
		Snapshot:      existing,
	}); err != nil {
		return err
	}
	return os.Remove(verifiedPath)
}

func (h *Host) wxInstallDirectory() string {
	dir, err := filepath.Abs(filepath.Join(h.StateDir(), "wxbot"))
	if err != nil {
		return wxDefaultInstallDir
	}
	return dir
}

func wxOwnsCompose(dir string) bool {
	marker, err := os.ReadFile(filepath.Join(dir, ".codexrm-managed"))
	return err == nil && string(marker) == "codexrm-wxbot-v1\n"
}

func wxInstallLoginToken(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if token, ok := strings.CutPrefix(line, "LOGIN_TOKEN="); ok && token != "" {
			return token, nil
		}
	}
	return "", errors.New("wxbot 安装缺少管理凭据")
}

func prepareWXInstallDir(ctx context.Context, dir string, images map[string]string, optionalServices []string) error {
	if strings.ContainsAny(dir, "\r\n'$") {
		return errors.New("wxbot 数据目录含不支持的字符")
	}
	if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err == nil && !wxOwnsCompose(dir) {
		return errors.New("目录中存在未接管的 Compose 配置")
	}
	if err := os.MkdirAll(filepath.Join(dir, "secrets", "nginx"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ".codexrm-managed"), []byte("codexrm-wxbot-v1\n"), 0600); err != nil {
		return err
	}
	envPath := filepath.Join(dir, ".env")
	if _, err := os.Stat(envPath); errors.Is(err, os.ErrNotExist) {
		secrets := map[string]string{
			"MYSQL_ROOT_PASSWORD": randomSecret(12),
			"MYSQL_PASSWORD":      randomSecret(12),
			"REDIS_PASSWORD":      randomSecret(12),
			"QDRANT_API_KEY":      randomSecret(16),
			"LOGIN_TOKEN":         randomSecret(12),
			"ROBOT_SECRET_KEY":    randomSecret(24),
			"SESSION_SECRET":      randomSecret(16),
			"WECHAT_SERVER_TOKEN": randomSecret(12),
		}
		var b strings.Builder
		fmt.Fprintf(&b, "WX_DATA_DIR='%s'\n", dir)
		for k, v := range secrets {
			b.WriteString(k)
			b.WriteByte('=')
			b.WriteString(v)
			b.WriteByte('\n')
		}
		if err := os.WriteFile(envPath, []byte(b.String()), 0o600); err != nil {
			return err
		}
	}
	compose := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(compose, []byte(wxRenderCompose(images, optionalServices)), 0600); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(dir, "images.json"), images); err != nil {
		return err
	}
	nginx := filepath.Join(dir, "nginx.conf")
	if _, err := os.Stat(nginx); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(nginx, []byte(wxNginxTemplate), 0o600); err != nil {
			return err
		}
	}
	crt := filepath.Join(dir, "secrets", "nginx", "tls.crt")
	key := filepath.Join(dir, "secrets", "nginx", "tls.key")
	if err := wxEnsureCertificate(crt, key); err != nil {
		return fmt.Errorf("生成 HTTPS 证书失败: %w", err)
	}
	if err := exec.CommandContext(ctx, "docker", wxDockerArgs("network", "inspect", "wechat-robot")...).Run(); err != nil {
		return exec.CommandContext(ctx, "docker", wxDockerArgs("network", "create", "wechat-robot")...).Run()
	}
	return nil
}

func wxEnsureCertificate(certPath, keyPath string) error {
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	if certErr == nil && keyErr == nil {
		return nil
	}
	for _, err := range []error{certErr, keyErr} {
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	now := time.Now()
	profile := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "wxbot"},
		DNSNames:     []string{"wxbot", "localhost"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	cert, err := x509.CreateCertificate(rand.Reader, profile, profile, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}), 0600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0600)
}

func validWXLocalUser(name string) bool {
	return name != "" && len(name) <= 32 && name[0] != '-' && strings.IndexFunc(name, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-'
	}) < 0
}

func randomSecret(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (h *Host) pullWXImages(ctx context.Context, task *wxTask, images map[string]string, optionalServices []string) error {
	services := wxEnabledComposeServices(optionalServices)
	progress := make([]wxImageProgress, 0, len(services)+3)
	for _, service := range services {
		progress = append(progress, wxImageProgress{Name: service.name, Image: images[service.key], Status: "pending"})
	}
	for _, key := range []string{"client", "protocol", "backup"} {
		progress = append(progress, wxImageProgress{Name: key, Image: images[key], Status: "pending"})
	}
	if saveErr := h.updateWXTask(task, func(current *wxTask) { current.Images = progress }); saveErr != nil {
		return saveErr
	}
	for i, item := range progress {
		if saveErr := h.updateWXTask(task, func(current *wxTask) { current.Images[i].Status = "pulling" }); saveErr != nil {
			return saveErr
		}
		var err error
		if i < len(services) {
			err = wxCompose(ctx, task.InstallDir, "pull", item.Name)
		} else {
			docker, lookErr := wxLookPath("docker")
			if lookErr != nil {
				return lookErr
			}
			if out, pullErr := wxCommandOutput(ctx, docker, wxDockerArgs("pull", item.Image)...); pullErr != nil {
				err = fmt.Errorf("%w: %s", pullErr, strings.TrimSpace(string(out)))
			}
		}
		if saveErr := h.updateWXTask(task, func(current *wxTask) {
			if err == nil {
				current.Images[i].Status = "completed"
			} else {
				current.Images[i].Status = "failed"
				current.Images[i].Error = err.Error()
			}
		}); saveErr != nil {
			return saveErr
		}
		if err != nil {
			return fmt.Errorf("拉取 %s 失败: %w", item.Name, err)
		}
	}
	return nil
}

func wxCompose(ctx context.Context, dir string, args ...string) error {
	path, err := wxLookPath("docker")
	if err != nil {
		return errors.New("未找到 docker")
	}
	cmdArgs := append(wxDockerArgs("compose", "--project-name", "codexrm-wxbot", "--project-directory", dir, "-f", filepath.Join(dir, "docker-compose.yml")), args...)
	out, err := wxCommandOutput(ctx, path, cmdArgs...)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(msg)
	}
	return nil
}

func waitWXHealthy(ctx context.Context, token string, timeout time.Duration) (*wxAdminClient, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		attempt, stop := context.WithTimeout(ctx, 5*time.Second)
		client, err := wxAdminLogin(attempt, token)
		stop()
		if err == nil {
			return client, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wxbot 管理接口就绪超时: %w", err)
		case <-time.After(2 * time.Second):
		}
	}
}

const wxComposeTemplate = `services:
  wechat-admin-redis:
    image: @@redis@@
    container_name: wechat-admin-redis
    volumes: ["./wechat_admin_redis_data:/data"]
    restart: always
    networks: [wechat-robot]
    environment:
      TZ: Asia/Shanghai
      REDIS_PASSWORD: ${REDIS_PASSWORD}
    command: ["sh", "-c", "redis-server --requirepass \"$${REDIS_PASSWORD}\""]
  wechat-admin-mysql:
    image: @@mysql@@
    container_name: wechat-admin-mysql
    restart: always
    networks: [wechat-robot]
    volumes:
      - ./wechat_admin_mysql_data:/var/lib/mysql
    environment:
      TZ: Asia/Shanghai
      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD}
      MYSQL_USER: wechat
      MYSQL_PASSWORD: ${MYSQL_PASSWORD}
      MYSQL_DATABASE: robot_admin
  wechat-admin-qdrant:
    image: @@qdrant@@
    container_name: wechat-admin-qdrant
    volumes: ["./wechat_admin_qdrant_data:/qdrant/storage"]
    restart: always
    networks: [wechat-robot]
    environment:
      QDRANT__SERVICE__API_KEY: ${QDRANT_API_KEY}
  wechat-server:
    image: @@wechat-auth@@
    container_name: wechat-server
    restart: always
    ports: ["127.0.0.1:8090:3000"]
    networks: [wechat-robot]
  wechat-slider:
    image: @@slider@@
    container_name: wechat-slider
    restart: always
    networks: [wechat-robot]
    environment:
      GIN_MODE: release
      TZ: Asia/Shanghai
      UUID_SERVER_URL: http://wechat-uuid:9000
  wechat-uuid:
    image: @@uuid@@
    container_name: wechat-uuid
    restart: always
    networks: [wechat-robot]
    environment:
      TZ: Asia/Shanghai
      GIN_MODE: release
@@optional-services@@  wechat-robot-admin-frontend:
    image: @@admin-web@@
    container_name: wechat-robot-admin-frontend
    restart: always
    networks: [wechat-robot]
  wechat-robot-admin-backend:
    image: @@admin-api@@
    container_name: wechat-robot-admin-backend
    restart: always
    networks: [wechat-robot]
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    environment:
      GIN_MODE: release
      TZ: Asia/Shanghai
      DOCKER_NETWORK: wechat-robot
      ROBOT_SECRET_KEY: ${ROBOT_SECRET_KEY}
      SESSION_SECRET: ${SESSION_SECRET}
      LOGIN_METHOD: token
      LOGIN_TOKEN: ${LOGIN_TOKEN}
      WECHAT_SERVER_ADDRESS: http://wechat-server:3000
      WECHAT_SERVER_TOKEN: ${WECHAT_SERVER_TOKEN}
      HOST_DATA_DIR: ${WX_DATA_DIR}
      SLIDER_SERVER_BASE_URL: http://wechat-slider:9000
      SLIDER_TOKEN: ${SLIDER_TOKEN:-not-configured}
      UUID_URL: http://wechat-slider:9000
      CODEXRM_WXBOT_CLIENT_IMAGE: @@client@@
      CODEXRM_WXBOT_PROTOCOL_IMAGE: @@protocol@@
      CODEXRM_WXBOT_MANAGED: "true"
      MYSQL_DRIVER: mysql
      MYSQL_HOST: wechat-admin-mysql
      MYSQL_PORT: 3306
      MYSQL_USER: root
      MYSQL_PASSWORD: ${MYSQL_ROOT_PASSWORD}
      MYSQL_DB: robot_admin
      REDIS_HOST: wechat-admin-redis
      REDIS_PORT: 6379
      REDIS_PASSWORD: ${REDIS_PASSWORD}
      QDRANT_HOST: wechat-admin-qdrant
      QDRANT_PORT: 6334
      QDRANT_API_KEY: ${QDRANT_API_KEY}
    depends_on:
      - wechat-admin-redis
      - wechat-admin-mysql
  wechat-nginx:
    image: @@nginx@@
    container_name: wechat-nginx
    restart: always
    ports: ["127.0.0.1:8080:80", "127.0.0.1:8443:443"]
    volumes:
      - ./nginx.conf:/etc/nginx/conf.d/default.conf:ro
      - ./secrets/nginx:/etc/nginx/certs:ro
    networks: [wechat-robot]
    depends_on:
      - wechat-robot-admin-frontend
      - wechat-robot-admin-backend
networks:
  wechat-robot:
    external: true
`

type wxTakeoverCredential struct {
	Token, OAuthCode string
}

const wxNginxTemplate = `resolver 127.0.0.11 ipv6=off valid=30s;

upstream admin_backend {
  zone admin_backend 64k; # 共享内存区，用于缓存 upstream 状态
  server wechat-robot-admin-backend:9000 resolve;
}

upstream admin_frontend {
  zone admin_frontend 64k;
  server wechat-robot-admin-frontend:9000 resolve;
}

server {
  server_name _;
  listen 80 default_server;
  listen [::]:80 default_server;
  return 301 https://$host$request_uri;
}

server {
  root /var/www/html;
  index index.html index.htm index.nginx-debian.html;
  server_name _;

  listen 443 ssl default_server;
  listen [::]:443 ssl default_server;

  http2 on;

  ssl_certificate     /etc/nginx/certs/tls.crt;
  ssl_certificate_key /etc/nginx/certs/tls.key;
  ssl_protocols       TLSv1.2 TLSv1.3;
  ssl_session_cache   shared:SSL:10m;
  ssl_session_timeout 10m;

  # 后端 API
  location /api/v1 {
    client_max_body_size 64m;
    proxy_http_version 1.1;
    proxy_pass http://admin_backend;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_cache_bypass $http_upgrade;
    proxy_set_header Accept-Encoding gzip;
    proxy_connect_timeout 5s;
    proxy_send_timeout  900s;
    proxy_read_timeout  900s;
  }

  # 前端静态资源
  location / {
    client_max_body_size 64m;
    proxy_http_version 1.1;
    proxy_pass http://admin_frontend;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_cache_bypass $http_upgrade;
    proxy_set_header Accept-Encoding gzip;
    proxy_read_timeout 60s;
  }
}`
