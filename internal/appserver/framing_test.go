package appserver

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestOversizedStdioResponseDrainsToNextRecord(t *testing.T) {
	reader := bufio.NewReaderSize(io.MultiReader(
		strings.NewReader(`{"id":17,"result":{"text":"`),
		strings.NewReader(strings.Repeat("a", maxMessageBytes)),
		strings.NewReader("\"}}\r\n{\"id\":18,\"result\":{}}\n"),
	), 64*1024)
	_, err := readStdioMessage(reader)
	var oversized *oversizedMessage
	if !errors.As(err, &oversized) || oversized.responseID != "17" {
		t.Fatalf("oversized response lost its ID: %v", err)
	}
	next, err := readStdioMessage(reader)
	if err != nil || string(next) != `{"id":18,"result":{}}` {
		t.Fatalf("next response was lost: %s (%v)", next, err)
	}
}

func TestOversizedServerRequestIsNotACallResponse(t *testing.T) {
	var oversized *oversizedMessage
	err := oversizedResponse([]byte(`{"id":17,"method":"item/tool/call","params": {"input":`))
	if !errors.As(err, &oversized) || oversized.responseID != "" {
		t.Fatalf("server request was associated with a client RPC: %#v", oversized)
	}
}
