package desktop

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

func directPeerName(stateDir, clientID string) *uint16 {
	return objectName(stateDir, fmt.Sprintf("direct-%x", sha256.Sum256([]byte(clientID))))
}

// TrackDirectPeer exposes session presence to the Explorer menu helper. The
// named event is never signaled; Windows removes it when its last handle closes,
// including on unexpected process exit. It is not used for authorization.
func TrackDirectPeer(ctx context.Context, stateDir, clientID string) func() {
	// A valid handle is returned even when the previous session still holds the event.
	handle, _ := windows.CreateEventEx(nil, directPeerName(stateDir, clientID), 0, windows.SYNCHRONIZE)
	if handle == 0 {
		return func() {}
	}
	closePeer := sync.OnceFunc(func() { windows.CloseHandle(handle) })
	stop := context.AfterFunc(ctx, closePeer)
	return func() {
		stop()
		closePeer()
	}
}

func IsDirectPeerActive(stateDir, clientID string) bool {
	handle, err := windows.OpenEvent(windows.SYNCHRONIZE, false, directPeerName(stateDir, clientID))
	if err != nil {
		return false
	}
	windows.CloseHandle(handle)
	return true
}
