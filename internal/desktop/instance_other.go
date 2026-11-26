//go:build !windows

package desktop

import (
	"os/exec"
	"runtime"
)

func EnsureP2PFirewall() error { return nil }
func OpenBrowser(url string) error {
	command := "xdg-open"
	if runtime.GOOS == "darwin" {
		command = "open"
	}
	cmd := exec.Command(command, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
