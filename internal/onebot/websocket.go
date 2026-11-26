package onebot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	initialReconnectDelay = 500 * time.Millisecond
	maxReconnectDelay     = 30 * time.Second
	wsPingInterval        = 30 * time.Second
	wsReadTimeout         = 90 * time.Second
)

type wsConnection struct {
	conn      *websocket.Conn
	writeMu   sync.Mutex
	done      chan struct{}
	closeOnce sync.Once
}

func newWSConnection(conn *websocket.Conn) *wsConnection {
	return &wsConnection{conn: conn, done: make(chan struct{})}
}

func (w *wsConnection) writeMessage(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-w.done:
		return ErrNotConnected
	default:
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	select {
	case <-w.done:
		return ErrNotConnected
	default:
	}
	deadline := time.Now().Add(writeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := w.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return w.conn.WriteMessage(websocket.TextMessage, payload)
}

func (c *Client) callWS(ctx context.Context, action string, params any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	async := strings.HasSuffix(action, "_async")
	echo := strconv.FormatUint(c.echo.Add(1), 10)
	request := map[string]any{"action": action, "params": params, "echo": echo}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("onebot: encode API params: %w", err)
	}
	pending := &pendingCall{action: action, ch: make(chan callResult, 1)}

	c.mu.Lock()
	connection := c.conn
	if connection == nil {
		c.mu.Unlock()
		return nil, ErrNotConnected
	}
	pending.conn = connection
	if !async {
		c.pending[echo] = pending
	}
	c.mu.Unlock()

	if err := connection.writeMessage(ctx, payload); err != nil {
		c.removePending(echo, pending)
		c.disconnect(connection, err)
		return nil, fmt.Errorf("onebot: write API request: %w", err)
	}
	if async {
		return nil, nil
	}
	defer c.removePending(echo, pending)
	select {
	case result := <-pending.ch:
		return result.data, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Client) removePending(echo string, pending *pendingCall) {
	c.mu.Lock()
	if c.pending[echo] == pending {
		delete(c.pending, echo)
	}
	c.mu.Unlock()
}

func (c *Client) installConnection(connection *wsConnection, selfID int64) {
	now := time.Now().UTC()
	c.mu.Lock()
	old := c.conn
	c.conn = connection
	c.status.State = StateConnected
	c.status.Error = ""
	c.status.LastConnectedAt = &now
	if selfID > 0 {
		c.status.SelfID = selfID
	}
	c.mu.Unlock()
	if old != nil && old != connection {
		c.disconnect(old, errors.New("onebot: replaced by a newer websocket connection"))
	}
}

func (c *Client) disconnectCurrent(err error) {
	c.mu.RLock()
	connection := c.conn
	c.mu.RUnlock()
	if connection != nil {
		c.disconnect(connection, err)
	}
}

func (c *Client) disconnect(connection *wsConnection, cause error) {
	connection.closeOnce.Do(func() {
		close(connection.done)
		_ = connection.conn.Close()
		if cause == nil {
			cause = ErrNotConnected
		}
		failed := make(map[string]*pendingCall)
		c.mu.Lock()
		if c.conn == connection {
			c.conn = nil
			if c.running {
				c.status.State = StateConnecting
			}
			c.status.Error = errorString(cause)
		}
		for echo, pending := range c.pending {
			if pending.conn == connection {
				failed[echo] = pending
				delete(c.pending, echo)
			}
		}
		c.mu.Unlock()
		for _, pending := range failed {
			pending.ch <- callResult{err: cause}
		}
	})
}

func (c *Client) runForwardWS(ctx context.Context) error {
	delay := initialReconnectDelay
	for {
		if ctx.Err() != nil {
			return nil
		}
		c.setState(StateConnecting, nil)
		dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
		header := make(http.Header)
		if c.cfg.AccessToken != "" {
			header.Set("Authorization", "Bearer "+c.cfg.AccessToken)
		}
		conn, response, err := dialer.DialContext(ctx, c.cfg.URL, header)
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.setState(StateRetrying, fmt.Errorf("onebot: websocket dial: %w", err))
			if !waitReconnect(ctx, delay) {
				return nil
			}
			delay = nextDelay(delay)
			continue
		}

		connection := newWSConnection(conn)
		c.installConnection(connection, 0)
		delay = initialReconnectDelay
		readErr := c.serveWS(ctx, connection)
		if ctx.Err() != nil {
			return nil
		}
		c.setState(StateRetrying, readErr)
		if !waitReconnect(ctx, delay) {
			return nil
		}
		delay = nextDelay(delay)
	}
}

func waitReconnect(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextDelay(delay time.Duration) time.Duration {
	delay *= 2
	if delay > maxReconnectDelay {
		return maxReconnectDelay
	}
	return delay
}

func (c *Client) serveWS(ctx context.Context, connection *wsConnection) error {
	var pingWG sync.WaitGroup
	pingWG.Add(1)
	go func() {
		defer pingWG.Done()
		c.pingLoop(ctx, connection)
	}()
	err := c.readWS(ctx, connection)
	c.disconnect(connection, err)
	pingWG.Wait()
	return err
}

func (c *Client) readWS(ctx context.Context, connection *wsConnection) error {
	conn := connection.conn
	conn.SetReadLimit(maxWSReadBytes)
	if err := conn.SetReadDeadline(time.Now().Add(wsReadTimeout)); err != nil {
		return err
	}
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	})
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("onebot: websocket read: %w", err)
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			continue
		}
		c.handleWSMessage(ctx, payload)
	}
}

func (c *Client) pingLoop(ctx context.Context, connection *wsConnection) {
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = connection.conn.Close()
			return
		case <-connection.done:
			return
		case <-ticker.C:
			connection.writeMu.Lock()
			err := connection.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout))
			connection.writeMu.Unlock()
			if err != nil {
				_ = connection.conn.Close()
				return
			}
		}
	}
}

func (c *Client) handleWSMessage(ctx context.Context, payload []byte) {
	var envelope struct {
		PostType string          `json:"post_type"`
		Echo     json.RawMessage `json:"echo"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		c.setError(fmt.Sprintf("invalid websocket JSON: %v", err))
		return
	}
	if envelope.PostType != "" {
		event, err := ParseEvent(payload)
		if err != nil {
			c.setError(err.Error())
			return
		}
		if err := c.acceptEvent(event); err != nil {
			c.setError(err.Error())
			return
		}
		c.dispatch(ctx, event)
		return
	}
	echo, ok := echoString(envelope.Echo)
	if !ok {
		return
	}
	c.mu.Lock()
	pending := c.pending[echo]
	if pending != nil {
		delete(c.pending, echo)
	}
	c.mu.Unlock()
	if pending == nil {
		return
	}
	data, err := decodeAPIResponse(pending.action, payload)
	pending.ch <- callResult{data: data, err: err}
}

func echoString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", false
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}
	switch value := value.(type) {
	case string:
		return value, value != ""
	case json.Number:
		return value.String(), true
	default:
		return "", false
	}
}
