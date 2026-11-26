package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const defaultWriteTimeout = 15 * time.Second

// Client multiplexes RPC calls and server-initiated approvals over stdio or WS.
type managedProcess interface {
	Kill() error
	Close() error
}

type Client struct {
	mu           sync.Mutex
	writeGate    chan struct{}
	once         sync.Once
	nextID       atomic.Int64
	pending      map[string]chan reply
	notify       func(json.RawMessage)
	closed       chan struct{}
	shutdownDone chan struct{}
	read         func() ([]byte, error)
	write        func([]byte) error
	shutdown     func() error
	writeTimeout time.Duration
	closeErr     error
}
type reply struct {
	message json.RawMessage
	err     error
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// OutcomeUnknown means the request may have reached Codex without a usable reply.
type OutcomeUnknown struct{ Cause error }

func (e *OutcomeUnknown) Error() string { return e.Cause.Error() }
func (e *OutcomeUnknown) Unwrap() error { return e.Cause }

func newClient() *Client {
	return &Client{
		pending: map[string]chan reply{}, closed: make(chan struct{}),
		shutdownDone: make(chan struct{}), writeGate: make(chan struct{}, 1),
		writeTimeout: defaultWriteTimeout,
	}
}
func Dial(ctx context.Context, rawURL string, token ...string) (*Client, error) {
	var accessToken string
	if len(token) > 0 {
		accessToken = token[0]
	}
	return dial(ctx, rawURL, accessToken, false)
}

func DialTextOnly(ctx context.Context, rawURL, token string) (*Client, error) {
	return dial(ctx, rawURL, token, true)
}

func dial(ctx context.Context, rawURL, token string, textOnly bool) (*Client, error) {
	headers := make(http.Header)
	if token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	if textOnly {
		headers.Set("X-CodexRM-TextOnly", "1")
	}
	conn, _, err := (&websocket.Dialer{HandshakeTimeout: 10 * time.Second}).DialContext(ctx, rawURL, headers)
	if err != nil {
		return nil, fmt.Errorf("dial app-server: %w", err)
	}
	c := newClient()
	if textOnly {
		conn.SetReadLimit(maxMessageBytes)
		c.read = func() ([]byte, error) { _, b, err := conn.ReadMessage(); return b, err }
	} else {
		c.read = func() ([]byte, error) { return readWebSocketMessage(conn) }
	}
	c.write = func(b []byte) error {
		_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		return conn.WriteMessage(websocket.TextMessage, b)
	}
	c.shutdown = conn.Close
	go c.readLoop()
	return c, nil
}

// Launch inherits the local login and config. Relay never receives login secrets.
func Launch(ctx context.Context, executable, cwd string) (*Client, error) {
	return launchStdio(ctx, executable, cwd, false)
}

// LaunchTextOnly starts a separate Codex process with selected tools disabled via process-local overrides.
func LaunchTextOnly(ctx context.Context, executable, cwd string) (*Client, error) {
	return launchStdio(ctx, executable, cwd, true)
}

func launchStdio(ctx context.Context, executable, cwd string, textOnly bool) (*Client, error) {
	executable, err := resolveCodexExecutable(executable)
	if err != nil {
		return nil, err
	}
	// Release the durable writer when the Agent unsubscribes an idle thread.
	args := []string{"-c", "thread_unload_delay_secs=0"}
	if textOnly {
		args = append(args, "-c", `web_search="disabled"`, "-c", "mcp_servers={}")
		for _, feature := range []string{"shell_tool", "unified_exec", "unified_exec_tty", "browser_use", "computer_use", "view_image", "shell_snapshot", "shell_snapshot_v2"} {
			args = append(args, "--disable", feature)
		}
	}
	args = append(args, "app-server", "--listen", "stdio://")
	cmd := exec.CommandContext(ctx, executable, args...)
	hideChildProcess(cmd)
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = cwd
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	process, err := startManagedProcess(cmd)
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		if textOnly {
			return nil, fmt.Errorf("start text-only Codex: %w", err)
		}
		return nil, fmt.Errorf("start Codex (install Codex desktop or pass --codex): %w", err)
	}
	c := newClient()
	if textOnly {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), maxMessageBytes)
		c.read = func() ([]byte, error) {
			if scanner.Scan() {
				return append([]byte(nil), scanner.Bytes()...), nil
			}
			if err := scanner.Err(); err != nil {
				return nil, err
			}
			return nil, errors.New("Codex process exited")
		}
	} else {
		reader := bufio.NewReaderSize(stdout, 64*1024)
		c.read = func() ([]byte, error) { return readStdioMessage(reader) }
	}
	c.write = func(b []byte) error { _, e := stdin.Write(append(b, '\n')); return e }
	c.shutdown = func() error {
		err := process.Kill() // Exit the reader before closing a potentially blocked pipe.
		_ = stdin.Close()
		return err
	}
	go func() { _ = cmd.Wait(); _ = c.Close(); _ = process.Close() }()
	go c.readLoop()
	return c, nil
}

func (c *Client) Done() <-chan struct{} { return c.closed }
func (c *Client) initiateClose() {
	c.once.Do(func() {
		close(c.closed)
		go func() {
			c.closeErr = c.shutdown()
			close(c.shutdownDone)
		}()
	})
}

func (c *Client) Close() error {
	c.initiateClose()
	<-c.shutdownDone
	return c.closeErr
}
func (c *Client) OnNotification(fn func(json.RawMessage)) { c.mu.Lock(); c.notify = fn; c.mu.Unlock() }
func (c *Client) readLoop() {
	defer c.Close()
	for {
		data, err := c.read()
		if err != nil {
			var oversized *oversizedMessage
			if !errors.As(err, &oversized) {
				return
			}
			c.mu.Lock()
			ch := c.pending[oversized.responseID]
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- reply{err: ErrResponseTooLarge}:
				default:
				}
			} else if oversized.responseID == "" {
				log.Print("discarded oversized Codex app-server message without a recognized response ID")
			}
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		c.mu.Lock()
		ch := c.pending[string(msg.ID)]
		fn := c.notify
		c.mu.Unlock()
		if msg.Method == "" && ch != nil {
			select {
			case ch <- reply{message: data}:
			default:
			}
			continue
		}
		if msg.Method != "" && fn != nil {
			fn(json.RawMessage(data))
		}
	}
}
func (c *Client) WriteRaw(raw json.RawMessage) error {
	return c.writeRaw(context.Background(), raw)
}

func (c *Client) writeRaw(ctx context.Context, raw json.RawMessage) error {
	ctx, cancel := context.WithTimeout(ctx, c.writeTimeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return errors.New("app-server connection closed")
	case c.writeGate <- struct{}{}:
	}
	if err := ctx.Err(); err != nil {
		<-c.writeGate
		return err
	}
	select {
	case <-c.closed:
		<-c.writeGate
		return errors.New("app-server connection closed")
	default:
	}
	done := make(chan error, 1)
	go func() {
		err := c.write(raw)
		if err != nil {
			c.initiateClose()
		}
		<-c.writeGate
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			return &OutcomeUnknown{Cause: err}
		}
		return nil
	case <-ctx.Done():
		// A canceled write may have left a partial frame on the connection.
		c.initiateClose()
		return &OutcomeUnknown{Cause: ctx.Err()}
	case <-c.closed:
		return &OutcomeUnknown{Cause: errors.New("app-server connection closed")}
	}
}
func (c *Client) Notify(method string, params any) error {
	b, e := json.Marshal(map[string]any{"method": method, "params": params})
	if e != nil {
		return e
	}
	return c.WriteRaw(b)
}
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := fmt.Sprint(c.nextID.Add(1))
	ch := make(chan reply, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	raw, err := json.Marshal(map[string]any{"id": json.Number(id), "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if err = c.writeRaw(ctx, raw); err != nil {
		return nil, err
	}
	var response reply
	select {
	case response = <-ch:
	case <-ctx.Done():
		response.err = ctx.Err()
	case <-c.closed:
		response.err = errors.New("app-server connection closed")
	}
	if response.err != nil {
		// Prefer an already delivered reply if it raced with cancellation or close.
		select {
		case response = <-ch:
		default:
		}
	}
	if response.err != nil {
		return nil, &OutcomeUnknown{Cause: response.err}
	}
	var msg struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(response.message, &msg); err != nil {
		return nil, &OutcomeUnknown{Cause: err}
	}
	if msg.Error != nil {
		return nil, msg.Error
	}
	if len(msg.Result) == 0 {
		return nil, &OutcomeUnknown{Cause: errors.New("app-server response has neither a result nor an error")}
	}
	return msg.Result, nil
}
func (c *Client) Initialize(ctx context.Context, name, version string) (json.RawMessage, error) {
	result, err := c.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": name, "title": "CodexRM", "version": version}, "capabilities": map[string]any{"experimentalApi": true}})
	if err != nil {
		return nil, err
	}
	return result, c.Notify("initialized", nil)
}
