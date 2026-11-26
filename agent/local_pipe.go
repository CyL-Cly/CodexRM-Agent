package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

const localAndroidClientID = "android-local-client"

// RunLocal serves a single already authenticated client over inherited pipes.
// No listener, relay registration, transport token or network port is involved.
func RunLocal(ctx context.Context, cfg Config, input io.ReadCloser, output io.Writer) error {
	cfg.ServerURL = "https://local.invalid" // New validates storage identity; no connection is made.
	a, err := New(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	app, err := appserver.Launch(ctx, cfg.CodexPath, cfg.Cwd)
	if err != nil {
		return err
	}
	defer app.Close()
	go func() {
		select {
		case <-ctx.Done():
		case <-app.Done():
			cancel()
		}
		_ = input.Close()
	}()
	initCtx, stopInit := context.WithTimeout(ctx, 20*time.Second)
	_, err = app.Initialize(initCtx, "codexrm-android-local", protocol.Version)
	stopInit()
	if err != nil {
		return fmt.Errorf("initialize local Codex: %w", err)
	}
	a.queueCtx = ctx
	a.queues = make(map[string]*messageQueue)
	a.providerApp.Store(app)
	var writeMu sync.Mutex
	send := sendEnvelope(func(env protocol.Envelope) error {
		if env.ClientID != "" && env.ClientID != localAndroidClientID {
			return nil
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := json.NewEncoder(output).Encode(env); err != nil {
			cancel()
			return err
		}
		return nil
	})
	a.localEvent = send
	app.OnNotification(func(raw json.RawMessage) {
		a.forward(raw)
		a.queueNotification(ctx, app, raw)
	})
	if err := send(protocol.Envelope{Type: "welcome", Status: "connected"}); err != nil {
		return err
	}
	inputFrames := bufio.NewScanner(input)
	inputFrames.Buffer(make([]byte, 64*1024), protocol.MaxEnvelopeSize)
	slots := make(chan struct{}, 24)
	for inputFrames.Scan() {
		var envelope protocol.Envelope
		if json.Unmarshal(inputFrames.Bytes(), &envelope) != nil || envelope.Type != protocol.TypeClientMessage || !json.Valid(envelope.Message) {
			continue
		}
		envelope = protocol.Envelope{Type: protocol.TypeClientMessage, ClientID: localAndroidClientID, StreamID: envelope.StreamID, Message: envelope.Message}
		a.dispatch(ctx, app, send, slots, envelope, nil)
	}
	if ctx.Err() == nil {
		return inputFrames.Err()
	}
	return nil
}
