package agenthost

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const wxHelperUnit = "codexrm-wx-docker-bootstrap.service"
const wxHelperUnitPath = "/etc/systemd/system/" + wxHelperUnit
const wxDefaultInstallDir = "/var/lib/codexrm/wxbot"
const wxDockerHost = "unix:///var/run/docker.sock"

var wxLookPath = exec.LookPath
var wxCommandOutput = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
var wxOSReleasePath = "/etc/os-release"
var wxListenPorts = []int{8080, 8090, 8443}

type wxToolStatus struct {
	Installed              bool   `json:"installed"`
	Version                string `json:"version,omitempty"`
	Error                  string `json:"error,omitempty"`
	Usable                 bool   `json:"usable"`
	socketPermissionDenied bool
}

type wxPortStatus struct {
	Port  int  `json:"port"`
	InUse bool `json:"inUse"`
}

type wxHelperStatus struct {
	Available bool   `json:"available"`
	Command   string `json:"command,omitempty"`
}

type wxServiceInfo struct {
	Name    string `json:"name"`
	Image   string `json:"image"`
	Status  string `json:"status"`
	Running bool   `json:"running"`
}

type wxCapabilities struct {
	CodexOnly          bool `json:"codexOnly"`
	CodexRM            bool `json:"codexrm"`
	GroupChat          bool `json:"groupChat"`
	FileContext        bool `json:"fileContext"`
	GroupNoticeFormats bool `json:"groupNoticeFormats"`
}

type wxExisting struct {
	Found      bool            `json:"found"`
	ComposeDir string          `json:"composeDir,omitempty"`
	Project    string          `json:"project,omitempty"`
	Version    string          `json:"version,omitempty"`
	AdminURL   string          `json:"adminUrl,omitempty"`
	DataDirs   []string        `json:"dataDirs,omitempty"`
	Services   []wxServiceInfo `json:"services,omitempty"`
	Accounts   []wxAccount     `json:"accounts,omitempty"`
	Robots     []wxAdminRobot  `json:"robots,omitempty"`
}

type wxProbe struct {
	Supported     bool           `json:"supported"`
	OS            string         `json:"os"`
	OSID          string         `json:"osId,omitempty"`
	OSVersion     string         `json:"osVersion,omitempty"`
	Arch          string         `json:"arch"`
	OSSupported   bool           `json:"osSupported"`
	DiskFreeBytes uint64         `json:"diskFreeBytes"`
	Docker        wxToolStatus   `json:"docker"`
	Compose       wxToolStatus   `json:"compose"`
	Ports         []wxPortStatus `json:"ports"`
	Helper        wxHelperStatus `json:"helper"`
	NeedPrivilege bool           `json:"needPrivilege"`
	Existing      *wxExisting    `json:"existing,omitempty"`
	Instance      *wxInstance    `json:"instance,omitempty"`
	Task          *wxTask        `json:"task,omitempty"`
	Ready         bool           `json:"ready"`
}

func wxDockerArgs(args ...string) []string {
	return append([]string{"--host", wxDockerHost}, args...)
}

func wxOSRelease() (id, version string) {
	f, err := os.Open(wxOSReleasePath)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	values := map[string]string{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[k] = strings.Trim(v, `"'`)
	}
	return values["ID"], values["VERSION_ID"]
}

func wxOSSupported(id, arch string) bool {
	if arch != "amd64" && arch != "x86_64" {
		return false
	}
	switch strings.ToLower(id) {
	case "debian", "ubuntu":
		return true
	default:
		return false
	}
}

func wxTool(ctx context.Context, name string, args ...string) wxToolStatus {
	path, err := wxLookPath(name)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return wxToolStatus{}
		}
		return wxToolStatus{Error: err.Error()}
	}
	out, err := wxCommandOutput(ctx, path, args...)
	status := wxToolStatus{Installed: true, Version: strings.TrimSpace(string(out))}
	if err != nil {
		status.Error = strings.TrimSpace(string(out))
		if status.Error == "" {
			status.Error = err.Error()
		}
		return status
	}
	if line, _, _ := strings.Cut(status.Version, "\n"); line != "" {
		status.Version = line
	}
	status.Usable = true
	return status
}

func wxDockerStatus(ctx context.Context) wxToolStatus {
	path, err := wxLookPath("docker")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return wxToolStatus{}
		}
		return wxToolStatus{Error: err.Error()}
	}
	out, err := wxCommandOutput(ctx, path, wxDockerArgs("version", "--format", "{{.Server.Version}}")...)
	status := wxToolStatus{Installed: true}
	if err != nil {
		status.socketPermissionDenied = strings.Contains(strings.ToLower(string(out)), "permission denied")
		if status.socketPermissionDenied {
			status.Error = "当前账号无法访问 Docker 套接字。请确认当前用户已加入 docker 组。"
		} else {
			status.Error = "Docker 命令已安装，但无法连接本机 Docker 守护进程。请检查 docker.service 是否运行。"
		}
		return status
	}
	status.Version = strings.TrimSpace(string(out))
	status.Usable = status.Version != ""
	if !status.Usable {
		status.Error = "Docker 命令未返回本机守护进程版本。"
	}
	return status
}

func wxComposeStatus(ctx context.Context) wxToolStatus {
	path, err := wxLookPath("docker")
	if err != nil {
		return wxToolStatus{}
	}
	out, err := wxCommandOutput(ctx, path, wxDockerArgs("compose", "version")...)
	status := wxToolStatus{Installed: err == nil, Version: strings.TrimSpace(string(out))}
	if err != nil {
		status.Error = strings.TrimSpace(string(out))
		if status.Error == "" {
			status.Error = err.Error()
		}
		return status
	}
	if line, _, _ := strings.Cut(status.Version, "\n"); line != "" {
		status.Version = line
	}
	status.Usable = true
	return status
}

func wxPortStates() []wxPortStatus {
	out := make([]wxPortStatus, 0, len(wxListenPorts))
	for _, port := range wxListenPorts {
		item := wxPortStatus{Port: port}
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
		if err == nil {
			item.InUse = true
			_ = conn.Close()
		}
		out = append(out, item)
	}
	return out
}

func wxHelperAvailable() bool {
	if _, err := os.Stat(wxHelperUnitPath); err == nil {
		return true
	}
	return os.Geteuid() == 0
}

func wxHelperCommand() string {
	if _, err := os.Stat(wxHelperUnitPath); err == nil {
		return "sudo systemctl start " + wxHelperUnit
	}
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "codexrm-agent"
	}
	return "sudo " + strconv.Quote(exe) + " -wx-install-helper"
}

func wxDetectExisting(ctx context.Context, dockerUsable bool) (*wxExisting, error) {
	if !dockerUsable {
		return &wxExisting{}, nil
	}
	path, err := wxLookPath("docker")
	if err != nil {
		return &wxExisting{}, nil
	}
	out, err := wxCommandOutput(ctx, path, wxDockerArgs("ps", "-a", "--format", "{{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Labels}}")...)
	if err != nil {
		return nil, errors.New("无法读取本机 Docker 中的微信机器人")
	}
	existing := &wxExisting{}
	dataDirs := map[string]struct{}{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, rest, _ := strings.Cut(line, "\t")
		image, rest, _ := strings.Cut(rest, "\t")
		status, labels, _ := strings.Cut(rest, "\t")
		if !wxManagedName(name) {
			continue
		}
		existing.Found = true
		existing.Services = append(existing.Services, wxServiceInfo{
			Name:    name,
			Image:   image,
			Status:  status,
			Running: strings.HasPrefix(status, "Up"),
		})
		if strings.Contains(name, "admin-backend") || strings.Contains(image, "wechat-robot-admin-backend") {
			existing.Version = imageTag(image)
		}
		for _, label := range strings.Split(labels, ",") {
			k, v, ok := strings.Cut(label, "=")
			if !ok {
				continue
			}
			switch k {
			case "com.docker.compose.project.working_dir":
				if existing.ComposeDir == "" && v != "" {
					existing.ComposeDir = v
					dataDirs[v] = struct{}{}
				}
			case "com.docker.compose.project":
				if existing.Project == "" {
					existing.Project = v
				}
			}
		}
	}
	if !existing.Found {
		return existing, nil
	}
	if dir := wxInspectEnv(ctx, path, "wechat-robot-admin-backend", "HOST_DATA_DIR"); dir != "" {
		existing.ComposeDir = dir
		dataDirs[dir] = struct{}{}
	}
	for _, dir := range wxInspectMounts(ctx, path, "wechat-admin-mysql") {
		dataDirs[dir] = struct{}{}
	}
	for dir := range dataDirs {
		existing.DataDirs = append(existing.DataDirs, dir)
	}
	if existing.ComposeDir == "" && existing.DataDirs != nil {
		existing.ComposeDir = existing.DataDirs[0]
	}
	existing.AdminURL = "https://127.0.0.1:8443"
	accounts, err := wxAccounts(ctx)
	if err != nil {
		return existing, nil
	}
	existing.Accounts = accounts
	return existing, nil
}

func wxManagedName(name string) bool {
	if wxContainerName.MatchString(name) || strings.HasPrefix(name, "server_") {
		return true
	}
	prefixes := []string{"wechat-", "wechat_admin", "word-cloud-server", "jimeng-api", "netease-cloud-music", "xiaohongshu-mcp"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func imageTag(image string) string {
	_, tag, ok := strings.Cut(image, ":")
	if !ok {
		return image
	}
	return tag
}

func wxInspectEnv(ctx context.Context, docker, name, key string) string {
	out, err := wxCommandOutput(ctx, docker, wxDockerArgs("inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", name)...)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && k == key {
			return v
		}
	}
	return ""
}

func wxInspectMounts(ctx context.Context, docker, name string) []string {
	out, err := wxCommandOutput(ctx, docker, wxDockerArgs("inspect", "--format", "{{range .Mounts}}{{println .Source}}{{end}}", name)...)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			dirs = append(dirs, line)
		}
	}
	return dirs
}

func (h *Host) collectWXProbe(ctx context.Context) (wxProbe, error) {
	id, version := wxOSRelease()
	probe := wxProbe{
		Supported:     wxSupported,
		OS:            runtime.GOOS,
		OSID:          id,
		OSVersion:     version,
		Arch:          runtime.GOARCH,
		OSSupported:   wxOSSupported(id, runtime.GOARCH),
		DiskFreeBytes: wxDiskFree("/"),
		Ports:         wxPortStates(),
		Helper:        wxHelperStatus{Available: wxHelperAvailable(), Command: wxHelperCommand()},
		Docker:        wxDockerStatus(ctx),
		Instance:      h.loadWXInstance(),
		Task:          h.loadWXTask(),
	}
	probe.Compose = wxComposeStatus(ctx)
	probe.NeedPrivilege = (!probe.Docker.Usable || !probe.Compose.Usable) && !probe.Helper.Available
	if probe.Docker.Usable {
		existing, err := wxDetectExisting(ctx, true)
		if err != nil {
			return probe, err
		}
		if existing.Found {
			probe.Existing = existing
		}
	}
	credential, _ := readJSONFile[string](h.wxAdminSecretPath())
	probe.Ready = credential != nil && *credential != "" && probe.Instance != nil &&
		(probe.Existing != nil && probe.Existing.Found || probe.Instance.ComposeDir != "")
	return probe, nil
}

func (h *Host) wxProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	h.resumeWXTask()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	probe, err := h.collectWXProbe(ctx)
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, probe)
}
