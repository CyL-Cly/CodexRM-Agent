package installation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
	"github.com/CyL-Cly/CodexRM-Agent/internal/release"
)

func UsesSetup(root string) bool {
	b, err := os.ReadFile(filepath.Join(root, ".state", "installation.json"))
	var marker struct{ Installer string }
	return err == nil && decodeJSON(b, &marker) == nil && marker.Installer == "inno"
}

// SetupAction coordinates install records and Agent shutdown for Inno Setup.
// Inno owns the installed files, shortcuts and Windows app entry.
func SetupAction(action, root string, autoStart bool) error {
	root = filepath.Clean(root)
	if runtime.GOOS != "windows" || !filepath.IsAbs(root) || filepath.Dir(root) == root {
		return errors.New("无效的 Windows 安装目录")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	switch action {
	case "prepare":
		previous := ResolveDir()
		if !strings.EqualFold(previous, root) {
			if _, err := os.Stat(filepath.Join(previous, "bin", "codexrm-agent.exe")); err == nil {
				return fmt.Errorf("请使用已有的安装目录：%s", previous)
			}
		}
		binary := filepath.Join(root, "bin", "codexrm-agent.exe")
		if _, err := os.Stat(binary); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		versionCtx, cancelVersion := context.WithTimeout(ctx, 5*time.Second)
		defer cancelVersion()
		cmd := exec.CommandContext(versionCtx, binary, "--version")
		hide(cmd)
		output, err := cmd.Output()
		var installed struct{ VersionCode int64 }
		if err != nil || decodeJSON(output, &installed) != nil || installed.VersionCode <= 0 {
			return errors.New("无法确认已安装的 Agent 版本")
		}
		if installed.VersionCode > release.VersionCode {
			return errors.New("已安装更新版本的 Agent")
		}
		return stopInstalled(ctx, root)
	case "register":
		if err := RegisterPhoneMenu(filepath.Join(root, "bin", "codexrm-agent.exe")); err != nil {
			return err
		}
		if err := clientconfig.WriteJSON(filepath.Join(root, ".state", "installation.json"), map[string]any{
			"schemaVersion": 2, "installDir": root, "installer": "inno",
		}); err != nil {
			return err
		}
		return clientconfig.WriteJSON(RecordPath(), map[string]any{
			"schemaVersion": 2, "installDir": root, "autoStart": autoStart, "updatedAt": time.Now().UTC(),
		})
	case "unregister":
		if err := stopInstalled(ctx, root); err != nil {
			return err
		}
		if err := unregisterPhoneMenu(filepath.Join(root, "bin", "codexrm-agent.exe")); err != nil {
			return err
		}
		if strings.EqualFold(ResolveDir(), root) {
			if err := os.Remove(RecordPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown setup action %q", action)
	}
}
