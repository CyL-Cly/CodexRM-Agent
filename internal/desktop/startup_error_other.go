//go:build !windows

package desktop

// ShowStartupError is a no-op off Windows; headless platforms use stderr and
// the agent log file instead of a modal dialog.
func ShowStartupError(string) {}
