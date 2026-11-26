//go:build !windows

package appserver

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

func hideChildProcess(cmd *exec.Cmd) {}

type unixManagedProcess struct {
	cmd      *exec.Cmd
	killOnce sync.Once
	killErr  error
}

func (p *unixManagedProcess) Kill() error {
	p.killOnce.Do(func() {
		if p.cmd.Process == nil {
			p.killErr = os.ErrProcessDone
			return
		}
		err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			err = os.ErrProcessDone
		}
		p.killErr = err
	})
	return p.killErr
}

func (p *unixManagedProcess) Close() error { return nil }

func startManagedProcess(cmd *exec.Cmd) (managedProcess, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	process := &unixManagedProcess{cmd: cmd}
	cmd.Cancel = process.Kill
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return process, nil
}

func runningCodexDesktopCLIs() []string { return nil }

func registeredCodexDesktopCLIs() []string { return nil }

func codexDesktopDriveRoots() []string { return nil }
