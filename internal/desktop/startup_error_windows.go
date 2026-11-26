//go:build windows

package desktop

import "golang.org/x/sys/windows"

// ShowStartupError surfaces a fatal early-startup failure when the Agent was
// launched from a GUI shortcut with no attached console. Console invocations
// already display stderr, so they must not be blocked by a modal dialog.
func ShowStartupError(message string) {
	handle, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if err == nil {
		var mode uint32
		if windows.GetConsoleMode(handle, &mode) == nil {
			return
		}
	}
	text, err := windows.UTF16PtrFromString("CodexRM 启动失败\n\n" + message + "\n\n详细错误已写入 .state/agent.log")
	if err != nil {
		return
	}
	caption, err := windows.UTF16PtrFromString("CodexRM")
	if err != nil {
		return
	}
	_, _ = windows.MessageBox(0, text, caption, windows.MB_ICONERROR|windows.MB_OK)
}
