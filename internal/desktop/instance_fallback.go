//go:build !windows && !linux

package desktop

import "context"

// Instance is a no-op on platforms without native single-instance IPC. Each
// invocation runs independently; callers still get the same API shape.
type Instance struct{}

func Acquire(string, bool) (*Instance, error)                   { return &Instance{}, nil }
func Stop(string) error                                         { return nil }
func (*Instance) Watch(ctx context.Context, _ func(), _ func()) { <-ctx.Done() }
func (*Instance) Close()                                        {}
