package desktop

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

func objectName(stateDir, kind string) *uint16 {
	path, _ := filepath.Abs(stateDir)
	hash := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(path))))
	name, _ := windows.UTF16PtrFromString(fmt.Sprintf(`Local\CodexRM-%x-%s`, hash[:16], kind))
	return name
}

// Instance uses Windows events so opening settings does not need an HTTP port
// until the user actually opens the settings page.
type Instance struct{ lock, open, stop windows.Handle }

func Acquire(stateDir string, restart bool) (*Instance, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		lock, err := windows.CreateMutex(nil, false, objectName(stateDir, "instance"))
		if err != nil && err != windows.ERROR_ALREADY_EXISTS {
			return nil, err
		}
		if err == nil {
			i := &Instance{lock: lock}
			i.open, err = windows.CreateEvent(nil, 0, 0, objectName(stateDir, "settings"))
			if err == nil {
				i.stop, err = windows.CreateEvent(nil, 0, 0, objectName(stateDir, "stop"))
			}
			if err != nil {
				i.Close()
				return nil, err
			}
			return i, nil
		}
		windows.CloseHandle(lock)
		kind := "settings"
		if restart {
			kind = "stop"
		}
		event, openErr := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, objectName(stateDir, kind))
		if openErr == nil {
			_ = windows.SetEvent(event)
			windows.CloseHandle(event)
			if !restart {
				return nil, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("等待现有 CodexRM 退出超时")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func Stop(stateDir string) error {
	event, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, objectName(stateDir, "stop"))
	if err == windows.ERROR_FILE_NOT_FOUND {
		return nil
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(event)
	return windows.SetEvent(event)
}

func (i *Instance) Watch(ctx context.Context, open func(), stop func()) {
	for ctx.Err() == nil {
		if status, _ := windows.WaitForSingleObject(i.stop, 0); status == windows.WAIT_OBJECT_0 {
			stop()
			return
		}
		if status, _ := windows.WaitForSingleObject(i.open, 100); status == windows.WAIT_OBJECT_0 {
			open()
		}
	}
}

func (i *Instance) Close() {
	for _, handle := range []windows.Handle{i.stop, i.open, i.lock} {
		if handle != 0 {
			windows.CloseHandle(handle)
		}
	}
}

func OpenBrowser(raw string) error {
	verb, _ := windows.UTF16PtrFromString("open")
	url, err := windows.UTF16PtrFromString(raw)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, url, nil, nil, windows.SW_SHOWNORMAL)
}
