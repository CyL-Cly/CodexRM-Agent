package agenthost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var errWXBootstrapAuthorization = errors.New("需要执行一次性 Docker 安装授权")

const wxDockerMirrorListURL = "https://mirrors.cernet.edu.cn/api/apt/mirrorlist/docker-ce"

var wxDockerMirrorURL = regexp.MustCompile(`^https://(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}(?:/[a-zA-Z0-9_-]+)+/?$`)

func wxDockerMirrorSources(list, distro string) []string {
	var sources []string
	seen := map[string]bool{}
	add := func(base string) {
		base = strings.TrimSuffix(base, "/")
		if !wxDockerMirrorURL.MatchString(base) || seen[base] {
			return
		}
		seen[base] = true
		sources = append(sources, base+"/linux/"+distro)
	}
	// CERNET lists the endpoints in order of preference for the requesting host.
	for _, line := range strings.Split(list, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			add(fields[0])
		}
		if len(sources) == 12 {
			break
		}
	}
	add("https://mirrors.tuna.tsinghua.edu.cn/docker-ce")
	add("https://mirror.sysu.edu.cn/docker-ce")
	return sources
}

func wxDockerAPTFiles(sources []string, suite, keyPath, mirrorPath string) (repo, mirrors []byte) {
	var list strings.Builder
	for i, source := range sources {
		fmt.Fprintf(&list, "%s/\tpriority:%d\n", source, i+1)
	}
	// CERNET lists the Docker CE roots. The generated list appends Docker's
	// distro-specific repository path so APT can fail over for indexes and debs.
	return []byte(fmt.Sprintf("deb [arch=amd64 signed-by=%s] mirror+file:%s %s stable\n", keyPath, mirrorPath, suite)), []byte(list.String())
}

// InstallWXDocker runs only the fixed OS bootstrap. It does not grant Docker
// access to the Agent account or accept commands from the settings server.
func InstallWXDocker(ctx context.Context) error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("请在支持的 Linux 主机上以 root 运行 Docker 安装")
	}
	id, _ := wxOSRelease()
	if !wxOSSupported(id, runtime.GOARCH) {
		return errors.New("仅支持 Debian / Ubuntu amd64")
	}
	raw, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return err
	}
	var suite string
	for _, line := range strings.Split(string(raw), "\n") {
		if value, ok := strings.CutPrefix(line, "VERSION_CODENAME="); ok {
			suite = strings.Trim(value, "\"'")
		}
	}
	if suite == "" || strings.IndexFunc(suite, func(r rune) bool { return r < 'a' || r > 'z' }) >= 0 {
		return errors.New("无法识别 Docker 软件包套件")
	}
	run := func(program string, args ...string) error {
		cmd := exec.CommandContext(ctx, program, args...)
		cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "DEBIAN_FRONTEND=noninteractive", "LANG=C"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s 执行失败: %s: %w", filepath.Base(program), strings.TrimSpace(string(out)), err)
		}
		return nil
	}
	apt := func(args ...string) error {
		return run("/usr/bin/apt-get", append([]string{"-o", "Acquire::https::Timeout=12", "-o", "Acquire::Retries=0"}, args...)...)
	}
	for _, path := range []string{"/usr/bin/curl", "/usr/bin/gpg", "/etc/ssl/certs/ca-certificates.crt"} {
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := apt("update"); err != nil {
			return err
		}
		if err := apt("install", "-y", "ca-certificates", "curl", "gnupg"); err != nil {
			return err
		}
		break
	}
	dir, err := os.MkdirTemp("", "codexrm-docker-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	fetch := func(path, url string) error {
		return run("/usr/bin/curl", "--fail", "--silent", "--show-error", "--location", "--max-redirs", "3",
			"--proto", "=https", "--proto-redir", "=https", "--connect-timeout", "4", "--max-time", "8",
			"--max-filesize", "65536", "--output", path, url)
	}
	listFile := filepath.Join(dir, "cernet-mirrors")
	var list []byte
	if err := fetch(listFile, wxDockerMirrorListURL); err != nil {
		fmt.Fprintf(os.Stderr, "获取 CERNET Docker CE 镜像列表失败，尝试内置国内源。%v\n", err)
	} else {
		list, err = os.ReadFile(listFile)
		if err != nil {
			return err
		}
	}
	sources := wxDockerMirrorSources(string(list), id)
	key := filepath.Join(dir, "docker.asc")
	var keyData []byte
	var keyErrors []error
	for _, source := range sources {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := fetch(key, source+"/gpg"); err != nil {
			keyErrors = append(keyErrors, fmt.Errorf("%s: %w", source, err))
			continue
		}
		cmd := exec.CommandContext(ctx, "/usr/bin/gpg", "--homedir", dir, "--batch", "--with-colons", "--show-keys", key)
		output, err := cmd.Output()
		if err != nil || !wxDockerSigningKey(string(output)) {
			keyErrors = append(keyErrors, fmt.Errorf("%s: Docker 签名密钥指纹校验失败", source))
			continue
		}
		keyData, err = os.ReadFile(key)
		if err != nil {
			return err
		}
		break
	}
	if len(keyData) == 0 {
		return fmt.Errorf("无法从国内镜像下载通过指纹校验的 Docker 签名密钥: %w", errors.Join(keyErrors...))
	}
	const keyDir = "/etc/apt/keyrings"
	const keyPath = keyDir + "/codexrm-docker.asc"
	if err = os.MkdirAll(keyDir, 0755); err != nil {
		return err
	}
	if err = os.WriteFile(keyPath, keyData, 0644); err != nil {
		return err
	}
	const mirrorPath = "/etc/apt/codexrm-docker-mirrors"
	repo, mirrors := wxDockerAPTFiles(sources, suite, keyPath, mirrorPath)
	if err = os.WriteFile(mirrorPath, mirrors, 0644); err != nil {
		return err
	}
	const repoPath = "/etc/apt/sources.list.d/codexrm-docker.list"
	if err = os.WriteFile(repoPath, repo, 0644); err != nil {
		return err
	}
	if err = apt("update"); err != nil {
		return err
	}
	// Refresh the Docker index in isolation and fail if no mirror supplies it.
	// Keep indexes for the host's normal repositories for package dependencies.
	if err = apt("-o", "Dir::Etc::sourcelist="+repoPath, "-o", "Dir::Etc::sourceparts=-",
		"-o", "APT::Get::List-Cleanup=false", "-o", "APT::Update::Error-Mode=any", "update"); err != nil {
		return err
	}
	if wxDockerStatus(ctx).Usable {
		return apt("install", "-y", "docker-compose-plugin")
	}
	if err = apt("install", "-y", "docker-ce", "docker-ce-cli", "containerd.io", "docker-buildx-plugin", "docker-compose-plugin"); err != nil {
		return err
	}
	return startWXInstalledDocker(ctx, run)
}

func startWXInstalledDocker(ctx context.Context, run func(string, ...string) error) error {
	if err := run("/usr/bin/systemctl", "enable", "--now", "docker.service"); err != nil {
		return fmt.Errorf("Docker 已安装，启动 docker.service 失败: %w", err)
	}
	if status := wxDockerStatus(ctx); !status.Usable {
		init, _ := os.ReadFile("/proc/1/comm")
		if len(init) != 0 && strings.TrimSpace(string(init)) != "systemd" {
			return errors.New("Docker 软件包已安装，但本机 Docker 守护进程未启动。当前环境没有以 systemd 启动，请在具备独立 Docker 守护进程的 Linux 环境中重试。")
		}
		return errors.New("Docker 软件包已安装，但 docker.service 启动后仍无法连接。请检查 journalctl -u docker.service，修复后重试。")
	}
	return nil
}

func wxDockerSigningKey(output string) bool {
	const fingerprint = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88"
	primary, valid, nextFingerprint := 0, false, false
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(line, ":")
		switch fields[0] {
		case "pub":
			primary++
			nextFingerprint = true
		case "sub":
			nextFingerprint = false
		case "fpr":
			if nextFingerprint {
				valid = len(fields) > 9 && fields[9] == fingerprint
				nextFingerprint = false
			}
		}
	}
	return primary == 1 && valid
}

func runWXBootstrap(ctx context.Context) error {
	if os.Geteuid() == 0 {
		return InstallWXDocker(ctx)
	}
	if _, err := os.Stat(wxHelperUnitPath); err != nil {
		return errWXBootstrapAuthorization
	}
	if err := exec.CommandContext(ctx, "/usr/bin/sudo", "-n", "-l", "/usr/bin/systemctl", "start", wxHelperUnit).Run(); err != nil {
		return errWXBootstrapAuthorization
	}
	output, err := exec.CommandContext(ctx, "/usr/bin/sudo", "-n", "/usr/bin/systemctl", "start", wxHelperUnit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Docker 安装服务失败，请检查 journalctl -u %s: %s: %w", wxHelperUnit, strings.TrimSpace(string(output)), err)
	}
	return nil
}
