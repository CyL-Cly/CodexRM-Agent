package agenthost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
)

func TestWXOSSupported(t *testing.T) {
	if !wxOSSupported("ubuntu", "amd64") || !wxOSSupported("debian", "amd64") {
		t.Fatal("ubuntu and debian amd64 should be supported")
	}
	if wxOSSupported("fedora", "amd64") || wxOSSupported("ubuntu", "arm64") {
		t.Fatal("unsupported systems must be rejected")
	}
}

func TestCollectWXProbeWithoutDocker(t *testing.T) {
	t.Cleanup(func() {
		wxLookPath = lookPathDefault
		wxCommandOutput = commandOutputDefault
		wxOSReleasePath = "/etc/os-release"
	})
	wxLookPath = func(string) (string, error) { return "", os.ErrNotExist }
	wxOSReleasePath = filepath.Join(t.TempDir(), "missing")
	h, err := New(filepath.Join(t.TempDir(), ".state", "connection.json"), "", clientconfig.Connection{}, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	probe, err := h.collectWXProbe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if probe.Supported != (runtime.GOOS == "linux") {
		t.Fatalf("supported=%v", probe.Supported)
	}
	if probe.Docker.Installed || probe.Docker.Usable || probe.Existing != nil {
		t.Fatalf("docker should be absent: %+v", probe.Docker)
	}
}

func TestWXDockerBootstrapRequiresReachableDaemon(t *testing.T) {
	t.Cleanup(func() {
		wxLookPath = lookPathDefault
		wxCommandOutput = commandOutputDefault
	})
	wxLookPath = func(string) (string, error) { return "docker", nil }
	wxCommandOutput = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[2] == "compose" {
			return []byte("Docker Compose version v5.8.0"), nil
		}
		return []byte("Cannot connect to the Docker daemon"), errors.New("exit status 1")
	}
	ctx := context.Background()
	if docker := wxDockerStatus(ctx); !docker.Installed || docker.Usable || docker.socketPermissionDenied {
		t.Fatalf("unreachable daemon: %+v", docker)
	}
	if compose := wxComposeStatus(ctx); !compose.Usable {
		t.Fatalf("compose can be installed while the daemon is down: %+v", compose)
	}
	if err := startWXInstalledDocker(ctx, func(string, ...string) error { return nil }); err == nil {
		t.Fatal("a successful systemctl exit must not imply that the Docker daemon is ready")
	}
	wxCommandOutput = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("permission denied while trying to connect to the Docker API"), errors.New("exit status 1")
	}
	if docker := wxDockerStatus(ctx); !docker.socketPermissionDenied {
		t.Fatalf("expected a Docker socket permission error: %+v", docker)
	}
}

func TestWXDockerCERNETMirrorList(t *testing.T) {
	list := "https://mirror.sysu.edu.cn/docker-ce/\tpriority:1\r\n" +
		"https://mirror.nju.edu.cn/docker-ce/\tpriority:2\n" +
		"https://mirror.nju.edu.cn/docker-ce\n" +
		"http://mirror.example.net/docker-ce/\n" +
		"https://mirror.example.net/docker-ce/../private\n" +
		"https://user@mirror.example.net/docker-ce/\n" +
		"https://127.0.0.1/docker-ce/\n" +
		"https://mirror.example.net/docker-ce/?path=other\n"
	want := []string{
		"https://mirror.sysu.edu.cn/docker-ce/linux/debian",
		"https://mirror.nju.edu.cn/docker-ce/linux/debian",
		"https://mirrors.tuna.tsinghua.edu.cn/docker-ce/linux/debian",
	}
	sources := wxDockerMirrorSources(list, "debian")
	if !slices.Equal(sources, want) {
		t.Fatalf("got %v, want %v", sources, want)
	}
	repo, mirrors := wxDockerAPTFiles(sources, "bookworm", "/etc/apt/keyrings/docker.asc", "/etc/apt/docker-mirrors")
	if string(repo) != "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] mirror+file:/etc/apt/docker-mirrors bookworm stable\n" ||
		!strings.HasPrefix(string(mirrors), want[0]+"/\tpriority:1\n"+want[1]+"/\tpriority:2\n") {
		t.Fatalf("unexpected APT files: %s %s", repo, mirrors)
	}
	if fallback := wxDockerMirrorSources("", "ubuntu"); len(fallback) != 2 || fallback[0] != "https://mirrors.tuna.tsinghua.edu.cn/docker-ce/linux/ubuntu" {
		t.Fatalf("missing fallback: %v", fallback)
	}
}

func TestWXDetectExistingParsesComposeDir(t *testing.T) {
	t.Cleanup(func() {
		wxLookPath = lookPathDefault
		wxCommandOutput = commandOutputDefault
	})
	wxLookPath = func(string) (string, error) { return "docker", nil }
	wxCommandOutput = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "{{.Labels}}"):
			return []byte("wechat-robot-admin-backend\tregistry.cn-shenzhen.aliyuncs.com/houhou/wechat-robot-admin-backend:latest\tUp 2 hours\tcom.docker.compose.project=local,com.docker.compose.project.working_dir=/opt/wxbot\n"), nil
		case strings.Contains(joined, "wechat-robot-admin-backend") && strings.Contains(joined, "inspect"):
			return []byte("HOST_DATA_DIR=/opt/wxbot\n"), nil
		case strings.Contains(joined, "{{.Names}}"):
			return []byte(""), nil
		default:
			return nil, errors.New(joined)
		}
	}
	existing, err := wxDetectExisting(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !existing.Found || existing.ComposeDir != "/opt/wxbot" || existing.Version != "latest" {
		t.Fatalf("existing: %+v", existing)
	}
}

func TestWXTaskRetryRequiresFailedTask(t *testing.T) {
	h, err := New(filepath.Join(t.TempDir(), ".state", "connection.json"), "", clientconfig.Connection{}, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	task, err := h.beginWXTask("takeover", wxTakeoverSteps(), "")
	if err != nil || task.ID == "" {
		t.Fatalf("begin: %v %+v", err, task)
	}
	task.Status = "failed"
	task.Error = "管理凭据不正确"
	if err := h.saveWXTask(task); err != nil {
		t.Fatal(err)
	}
	loaded := h.loadWXTask()
	if loaded == nil || loaded.Status != "failed" {
		t.Fatalf("saved task: %+v", loaded)
	}
}

var lookPathDefault = wxLookPath
var commandOutputDefault = wxCommandOutput
