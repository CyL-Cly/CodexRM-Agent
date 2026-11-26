package installation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
	"github.com/CyL-Cly/CodexRM-Agent/internal/desktop"
)

type Options struct {
	Workspace, CodexPath              string
	NoShortcuts, NoAutostart, NoStart bool
}

func DefaultDir() string { return filepath.Join(os.Getenv("LOCALAPPDATA"), "CodexRM") }
func RecordPath() string { return filepath.Join(DefaultDir(), "installation.json") }

func ResolveDir() string {
	b, err := os.ReadFile(RecordPath())
	if err == nil {
		var record struct {
			InstallDir string `json:"installDir"`
		}
		if decodeJSON(b, &record) == nil && filepath.IsAbs(record.InstallDir) {
			return record.InstallDir
		}
	}
	return DefaultDir()
}

func Install(ctx context.Context, binary, root string, opts Options) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if filepath.Dir(root) == root {
		return errors.New("不能安装到磁盘根目录")
	}
	if UsesSetup(root) {
		return errors.New("请使用新版 CodexRM-Setup.exe 更新此安装")
	}
	configPath := filepath.Join(root, ".state", "connection.json")
	cfg, err := clientconfig.Load(configPath)
	if err != nil {
		return err
	}
	if opts.Workspace != "" {
		cfg.Workspace = opts.Workspace
	}
	if cfg.Workspace == "" {
		cfg.Workspace, err = os.UserHomeDir()
		if err != nil {
			return err
		}
	}
	if st, err := os.Stat(cfg.Workspace); err != nil || !st.IsDir() {
		return fmt.Errorf("工作区不存在：%s", cfg.Workspace)
	}
	if opts.CodexPath != "" {
		cfg.CodexPath = opts.CodexPath
	}
	if err := cfg.Normalize(); err != nil {
		return err
	}
	installed := filepath.Join(root, "bin", "codexrm-agent.exe")
	if err := stopInstalled(ctx, root); err != nil {
		return err
	}
	if !strings.EqualFold(filepath.Clean(binary), installed) {
		if err := copyBinary(binary, installed); err != nil {
			return err
		}
	}
	if err := clientconfig.Save(configPath, cfg); err != nil {
		return err
	}
	if err := clientconfig.WriteJSON(filepath.Join(root, ".state", "installation.json"), map[string]any{"installDir": root, "schemaVersion": 2}); err != nil {
		return err
	}
	if err := register(root, opts); err != nil {
		return err
	}
	if err := clientconfig.WriteJSON(RecordPath(), map[string]any{"schemaVersion": 2, "installDir": root, "updatedAt": time.Now().UTC(), "autoStart": !opts.NoAutostart}); err != nil {
		return err
	}
	if !opts.NoStart {
		return Start(root, true)
	}
	return nil
}

func copyBinary(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(destination), ".agent-*.exe")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, copyErr := io.Copy(f, in)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), destination)
}

func Start(root string, settings bool) error {
	args := []string{"--config", filepath.Join(root, ".state", "connection.json")}
	if settings {
		args = append(args, "--settings")
	}
	cmd := exec.Command(filepath.Join(root, "bin", "codexrm-agent.exe"), args...)
	cmd.Dir = root
	hide(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func Uninstall(ctx context.Context, root string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if filepath.Dir(root) == root {
		return errors.New("拒绝卸载磁盘根目录")
	}
	if UsesSetup(root) {
		return errors.New("请在 Windows 设置中卸载 CodexRM")
	}
	// Only a directory carrying an installation marker and the agent can be removed.
	if _, err := os.Stat(filepath.Join(root, ".state", "installation.json")); err != nil {
		return errors.New("找不到 CodexRM 安装标记")
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "codexrm-agent.exe")); err != nil {
		return err
	}
	if err := stopInstalled(ctx, root); err != nil {
		return err
	}
	if err := unregister(root); err != nil {
		return err
	}
	registered := strings.EqualFold(ResolveDir(), root)
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	if registered {
		if err := os.Remove(RecordPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// BeginUninstall runs from a temporary copy so Windows can remove the installed image.
func BeginUninstall(root string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "CodexRM-uninstall-")
	if err != nil {
		return err
	}
	helper := filepath.Join(dir, "uninstall.exe")
	if err := copyBinary(self, helper); err != nil {
		return err
	}
	cmd := exec.Command(helper, "--uninstall-dir", root)
	hide(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func stopInstalled(ctx context.Context, root string) error {
	state := filepath.Join(root, ".state")
	if err := desktop.Stop(state); err != nil {
		return err
	}
	if err := waitStopped(ctx, state); err != nil {
		return err
	}
	return stopLegacy(ctx, filepath.Join(root, "bin", "codexrm-agent.exe"))
}
