package appserver

import (
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func hideChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

type windowsManagedProcess struct {
	job       windows.Handle
	killOnce  sync.Once
	closeOnce sync.Once
	killErr   error
	closeErr  error
}

func (p *windowsManagedProcess) Kill() error {
	p.killOnce.Do(func() { p.killErr = windows.TerminateJobObject(p.job, 1) })
	return p.killErr
}

func (p *windowsManagedProcess) Close() error {
	p.closeOnce.Do(func() { p.closeErr = windows.CloseHandle(p.job) })
	return p.closeErr
}

func startManagedProcess(cmd *exec.Cmd) (managedProcess, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	process := &windowsManagedProcess{job: job}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = process.Close()
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_SUSPENDED,
	}
	cmd.Cancel = process.Kill
	if err = cmd.Start(); err != nil {
		_ = process.Close()
		return nil, err
	}
	cleanup := func() {
		_ = process.Kill()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = process.Close()
	}
	if err = assignProcessToJob(process.job, cmd.Process.Pid); err != nil {
		cleanup()
		return nil, err
	}
	if err = resumeProcessMainThread(cmd.Process.Pid); err != nil {
		cleanup()
		return nil, err
	}
	return process, nil
}

func assignProcessToJob(job windows.Handle, pid int) error {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	return windows.AssignProcessToJobObject(job, process)
}

func resumeProcessMainThread(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(pid) {
			continue
		}
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			return openErr
		}
		_, resumeErr := windows.ResumeThread(thread)
		_ = windows.CloseHandle(thread)
		return resumeErr
	}
	return windows.ERROR_NOT_FOUND
}

// Desktop releases use either Codex.exe or ChatGPT.exe for the UI. Resolve the
// sibling resources\codex.exe even before the desktop has spawned an app-server.
func runningCodexDesktopCLIs() []string {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snapshot)
	var desktopPaths, cliPaths []string
	seen := map[string]bool{}
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		name := windows.UTF16ToString(entry.ExeFile[:])
		if !strings.EqualFold(name, "codex.exe") && !strings.EqualFold(name, "chatgpt.exe") {
			continue
		}
		process, openErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, entry.ProcessID)
		if openErr != nil {
			continue
		}
		buffer := make([]uint16, windows.MAX_LONG_PATH)
		size := uint32(len(buffer))
		queryErr := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size)
		windows.CloseHandle(process)
		if queryErr != nil {
			continue
		}
		path := windows.UTF16ToString(buffer[:size])
		if seen[strings.ToLower(path)] {
			continue
		}
		seen[strings.ToLower(path)] = true
		if strings.HasSuffix(strings.ToLower(path), `\resources\codex.exe`) {
			cliPaths = append(cliPaths, codexDesktopProcessCLIPaths(path)...)
		} else {
			desktopPaths = append(desktopPaths, codexDesktopProcessCLIPaths(path)...)
		}
	}
	return append(desktopPaths, cliPaths...)
}

func codexDesktopProcessCLIPaths(path string) []string {
	name := filepath.Base(path)
	if !strings.EqualFold(name, "codex.exe") && !strings.EqualFold(name, "chatgpt.exe") {
		return nil
	}
	if strings.HasSuffix(strings.ToLower(path), `\resources\codex.exe`) {
		return []string{path}
	}
	return codexDesktopCLIPaths(filepath.Dir(path))
}

func codexDesktopCLIPaths(installDir string) []string {
	return []string{
		filepath.Join(installDir, "app", "resources", "codex.exe"),
		filepath.Join(installDir, "resources", "codex.exe"),
	}
}

// Store registration includes custom package volumes and works while the UI is closed.
func registeredCodexDesktopCLIs() []string {
	var paths []string
	packages, err := registry.OpenKey(registry.CURRENT_USER, `Software\Classes\Local Settings\Software\Microsoft\Windows\CurrentVersion\AppModel\Repository\Packages`, registry.READ)
	if err != nil {
		return nil
	}
	defer packages.Close()
	names, _ := packages.ReadSubKeyNames(-1)
	for _, name := range names {
		if !strings.HasPrefix(strings.ToLower(name), "openai.codex_") {
			continue
		}
		key, err := registry.OpenKey(packages, name, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		installDir, _, err := key.GetStringValue("PackageRootFolder")
		key.Close()
		if err == nil && installDir != "" {
			paths = append(paths, codexDesktopCLIPaths(installDir)...)
		}
	}
	return paths
}

func codexDesktopDriveRoots() []string {
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var roots []string
	for i := uint32(0); i < 26; i++ {
		if drives&(1<<i) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		rootPtr, _ := windows.UTF16PtrFromString(root)
		if windows.GetDriveType(rootPtr) == windows.DRIVE_FIXED {
			roots = append(roots, root)
		}
	}
	return roots
}
