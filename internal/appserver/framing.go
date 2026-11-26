package appserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/gorilla/websocket"
)

const maxMessageBytes = 32 << 20

var ErrResponseTooLarge = errors.New("Codex app-server response exceeds 32 MiB")

type oversizedMessage struct{ responseID string }

func (e *oversizedMessage) Error() string { return ErrResponseTooLarge.Error() }

// Codex emits the response ID before its result. Inspect only the header and
// distinguish responses from server-initiated requests that also have an ID.
func oversizedResponse(header []byte) error {
	dec := json.NewDecoder(bytes.NewReader(header[:min(len(header), 4096)]))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return &oversizedMessage{}
	}
	var id json.Number
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			break
		}
		switch key {
		case "method":
			return &oversizedMessage{}
		case "result", "error":
			if id != "" {
				return &oversizedMessage{responseID: id.String()}
			}
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			break
		}
		if key == "id" {
			// Calls issued here use numeric IDs. A quoted ID is a different RPC.
			if len(value) > 0 && value[0] >= '0' && value[0] <= '9' {
				id = json.Number(value)
			}
		}
	}
	return &oversizedMessage{}
}

func readStdioMessage(r *bufio.Reader) ([]byte, error) {
	var data []byte
	var oversized error
	for {
		part, err := r.ReadSlice('\n')
		if err != nil && err != bufio.ErrBufferFull && err != io.EOF {
			return nil, err
		}
		part = bytes.TrimSuffix(part, []byte{'\n'})
		if oversized == nil {
			if len(data)+len(part) > maxMessageBytes {
				oversized = oversizedResponse(data)
				data = nil
			} else {
				data = append(data, part...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if oversized != nil {
			return nil, oversized
		}
		if len(data) == 0 && err == io.EOF {
			return nil, io.EOF
		}
		return data, nil
	}
}

func readWebSocketMessage(conn *websocket.Conn) ([]byte, error) {
	_, reader, err := conn.NextReader()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxMessageBytes+1))
	if err != nil || len(data) <= maxMessageBytes {
		return data, err
	}
	oversized := oversizedResponse(data)
	// Drain this frame without buffering it. The deadline caps the time a
	// stalled upstream can keep all RPCs waiting for the next message.
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Time{})
	return nil, oversized
}
