//go:build !windows

package appserver

import (
	"errors"
	"os"
	"syscall"
)

func processExited(pid int) (bool, error) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, err
	}
	err = process.Signal(syscall.Signal(0))
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return true, nil
	}
	if errors.Is(err, syscall.EPERM) {
		return false, nil
	}
	return false, err
}
