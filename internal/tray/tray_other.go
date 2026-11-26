//go:build !windows

package tray

import "context"

func ConfigureLog(string) func() { return func() {} }

// Run keeps non-Windows agents console-based; the system tray is Windows-only.
func Run(ctx context.Context, _ string, _, _ func(), _ func() string, run func(context.Context) error) error {
	return run(ctx)
}
