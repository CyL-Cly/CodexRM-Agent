//go:build windows

package installation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
	"github.com/CyL-Cly/CodexRM-Agent/internal/desktop"
	"github.com/CyL-Cly/CodexRM-Agent/internal/release"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const uninstallKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\CodexRM`
const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func decodeJSON(b []byte, v any) error {
	return json.Unmarshal(bytes.TrimPrefix(b, []byte{239, 187, 191}), v)
}
func hide(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}
func quote(value string) string { return syscall.EscapeArg(value) }

func waitStopped(ctx context.Context, state string) error {
	// Acquire with restart waits for the existing instance to finish closing Codex.
	done := make(chan error, 1)
	go func() {
		instance, err := desktop.Acquire(state, true)
		if instance != nil {
			instance.Close()
		}
		done <- err
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func stopLegacy(ctx context.Context, binary string) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err := windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		if int(entry.ProcessID) == os.Getpid() || !strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), "codexrm-agent.exe") {
			continue
		}
		process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, entry.ProcessID)
		if err != nil {
			continue
		}
		buffer := make([]uint16, windows.MAX_LONG_PATH)
		size := uint32(len(buffer))
		if windows.QueryFullProcessImageName(process, 0, &buffer[0], &size) == nil && strings.EqualFold(windows.UTF16ToString(buffer[:size]), binary) {
			if err := windows.TerminateProcess(process, 0); err != nil {
				windows.CloseHandle(process)
				return err
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				status, err := windows.WaitForSingleObject(process, 100)
				if err != nil || status == windows.WAIT_OBJECT_0 {
					break
				}
				if ctx.Err() != nil || time.Now().After(deadline) {
					windows.CloseHandle(process)
					return errors.New("等待旧版 Agent 退出超时")
				}
			}
		}
		windows.CloseHandle(process)
	}
	return nil
}

func MigrateLegacy(root string) error {
	marker := filepath.Join(root, ".state", "installation.json")
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	var record struct {
		InstallDir string `json:"installDir"`
		AutoStart  bool   `json:"autoStart"`
	}
	b, err := os.ReadFile(RecordPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := decodeJSON(b, &record); err != nil {
		return err
	}
	if !strings.EqualFold(filepath.Clean(record.InstallDir), filepath.Clean(root)) {
		return nil
	}
	if err := register(root, Options{NoAutostart: !record.AutoStart}); err != nil {
		return err
	}
	return clientconfig.WriteJSON(marker, map[string]any{"schemaVersion": 2, "installDir": root})
}

func folder(id *windows.KNOWNFOLDERID, testName string) (string, error) {
	if root := os.Getenv("CODEXRM_INSTALL_TEST_ROOT"); root != "" {
		return filepath.Join(root, testName), nil
	}
	return windows.KnownFolderPath(id, 0)
}

func register(root string, opts Options) error {
	binary := filepath.Join(root, "bin", "codexrm-agent.exe")
	if err := removeLegacyShortcuts(); err != nil {
		return err
	}
	desktopDir, err := folder(windows.FOLDERID_Desktop, "Desktop")
	if err != nil {
		return err
	}
	if !opts.NoShortcuts {
		if err := shortcut(filepath.Join(desktopDir, "CodexRM 重启服务.lnk"), binary, "--restart", root); err != nil {
			return err
		}
	}
	if os.Getenv("CODEXRM_INSTALL_TEST_ROOT") != "" {
		return nil
	}
	if err := RegisterPhoneMenu(binary); err != nil {
		return err
	}
	run, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer run.Close()
	if opts.NoAutostart {
		_ = run.DeleteValue("CodexRM")
	} else if err := run.SetStringValue("CodexRM", quote(binary)); err != nil {
		return err
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, uninstallKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	for name, value := range map[string]string{"DisplayName": "CodexRM", "DisplayVersion": release.VersionName, "Publisher": "CodexRM", "InstallLocation": root, "DisplayIcon": binary, "UninstallString": quote(binary) + " --uninstall", "QuietUninstallString": quote(binary) + " --uninstall"} {
		if err := key.SetStringValue(name, value); err != nil {
			return err
		}
	}
	if err := key.SetDWordValue("NoModify", 1); err != nil {
		return err
	}
	return key.SetDWordValue("NoRepair", 1)
}

func removeLegacyShortcuts() error {
	dir, err := folder(windows.FOLDERID_Desktop, "Desktop")
	if err != nil {
		return err
	}
	for _, name := range []string{"CodexRM 重启服务", "CodexRM 设置和生成配对码", "CodexRM", "CodexRM Pair", "CodexRM Settings"} {
		if err := os.Remove(filepath.Join(dir, name+".lnk")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	startup, err := folder(windows.FOLDERID_Startup, "Startup")
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(startup, "CodexRM.lnk")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func unregister(root string) error {
	if err := unregisterPhoneMenu(filepath.Join(root, "bin", "codexrm-agent.exe")); err != nil {
		return err
	}
	if err := removeLegacyShortcuts(); err != nil {
		return err
	}
	if os.Getenv("CODEXRM_INSTALL_TEST_ROOT") != "" {
		return nil
	}
	run, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err == nil {
		_ = run.DeleteValue("CodexRM")
		run.Close()
	}
	err = registry.DeleteKey(registry.CURRENT_USER, uninstallKey)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}

// IShellLinkW and IPersistFile create a real .lnk without a script host.
type comObject struct{ table *[24]uintptr }

func comCall(obj *comObject, index int, args ...uintptr) error {
	r, _, _ := syscall.SyscallN(obj.table[index], append([]uintptr{uintptr(unsafe.Pointer(obj))}, args...)...)
	if int32(r) < 0 {
		return fmt.Errorf("Windows shortcut: HRESULT 0x%x", uint32(r))
	}
	return nil
}
func shortcut(path, target, arguments, cwd string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ole := windows.NewLazySystemDLL("ole32.dll")
	init, _, _ := ole.NewProc("CoInitializeEx").Call(0, 2)
	if int32(init) < 0 {
		return fmt.Errorf("CoInitializeEx: 0x%x", init)
	}
	defer ole.NewProc("CoUninitialize").Call()
	clsid, _ := windows.GUIDFromString("{00021401-0000-0000-C000-000000000046}")
	iid, _ := windows.GUIDFromString("{000214F9-0000-0000-C000-000000000046}")
	var link *comObject
	r, _, _ := ole.NewProc("CoCreateInstance").Call(uintptr(unsafe.Pointer(&clsid)), 0, 1, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&link)))
	if int32(r) < 0 {
		return fmt.Errorf("CoCreateInstance: 0x%x", r)
	}
	defer comCall(link, 2)
	for _, entry := range []struct {
		index int
		value string
	}{{20, target}, {11, arguments}, {9, cwd}, {7, "重启 CodexRM 服务"}} {
		ptr, err := windows.UTF16PtrFromString(entry.value)
		if err != nil {
			return err
		}
		if err := comCall(link, entry.index, uintptr(unsafe.Pointer(ptr))); err != nil {
			return err
		}
	}
	persistIID, _ := windows.GUIDFromString("{0000010B-0000-0000-C000-000000000046}")
	var persist *comObject
	if err := comCall(link, 0, uintptr(unsafe.Pointer(&persistIID)), uintptr(unsafe.Pointer(&persist))); err != nil {
		return err
	}
	defer comCall(persist, 2)
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return comCall(persist, 6, uintptr(unsafe.Pointer(ptr)), 1)
}
