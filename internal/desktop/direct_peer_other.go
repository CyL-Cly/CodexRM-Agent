//go:build !windows

package desktop

import "context"

func TrackDirectPeer(context.Context, string, string) func() { return func() {} }
func IsDirectPeerActive(string, string) bool                 { return false }
