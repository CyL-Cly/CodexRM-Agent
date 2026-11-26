package server_test

import (
	"testing"

	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/gorilla/websocket"
)

func assertRelayFileDenied(t *testing.T, c *websocket.Conn, method string, params any) {
	t.Helper()
	const stream = "blocked-file"
	write(t, c, protocol.Envelope{Type: protocol.TypeClientMessage, StreamID: stream,
		Message: raw(map[string]any{"id": 99, "method": method, "params": params}),
	})
	denied := until(t, c, func(e protocol.Envelope) bool { return e.StreamID == stream })
	if denied.Type != protocol.TypeError || denied.Error != protocol.FileTransferDirectRequired || len(denied.Message) != 0 {
		t.Fatalf("%s over relay: %+v", method, denied)
	}
}
