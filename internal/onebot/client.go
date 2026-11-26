package onebot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// MaxResponseBytes limits HTTP API responses.
	MaxResponseBytes int64 = 4 << 20
	// MaxEventBytes limits WebSocket frames and HTTP event bodies. It leaves
	// room for four 5 MiB images after base64 expansion and JSON overhead.
	MaxEventBytes  int64 = 32 << 20
	maxWSReadBytes       = MaxEventBytes
	writeTimeout         = 15 * time.Second
)

var (
	ErrNotConnected     = errors.New("onebot: websocket is not connected")
	ErrAlreadyRunning   = errors.New("onebot: client is already running")
	ErrDisabled         = errors.New("onebot: client is disabled")
	ErrResponseTooLarge = errors.New("onebot: response exceeds size limit")
	actionPattern       = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

// Status is a race-safe snapshot of Client state.
type Status struct {
	Enabled         bool       `json:"enabled"`
	Mode            string     `json:"mode"`
	State           string     `json:"state"`
	Error           string     `json:"error,omitempty"`
	SelfID          int64      `json:"selfId"`
	LastConnectedAt *time.Time `json:"lastConnectedAt,omitempty"`
}

// APIError reports a OneBot response whose status or retcode indicates failure.
type APIError struct {
	Action  string
	Status  string
	Retcode int
	Message string
	Wording string
}

func (e *APIError) Error() string {
	detail := e.Wording
	if detail == "" {
		detail = e.Message
	}
	if detail == "" {
		detail = e.Status
	}
	return fmt.Sprintf("onebot: API %s failed (status=%q retcode=%d): %s", e.Action, e.Status, e.Retcode, detail)
}

// HTTPError reports a non-success HTTP API status. Body is bounded to 4 KiB.
type HTTPError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("onebot: HTTP API returned %s: %s", e.Status, e.Body)
}

type pendingCall struct {
	conn   *wsConnection
	action string
	ch     chan callResult
}

type callResult struct {
	data json.RawMessage
	err  error
}

// Client is safe for concurrent API calls and event delivery.
type Client struct {
	cfg        Config
	handler    func(context.Context, Event)
	httpClient *http.Client

	mu      sync.RWMutex
	status  Status
	conn    *wsConnection
	pending map[string]*pendingCall
	running bool

	echo        atomic.Uint64
	connections sync.WaitGroup
	handlers    sync.WaitGroup
}

// New validates cfg and constructs a client. A nil handler discards events.
func New(cfg Config, handler func(context.Context, Event)) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if handler == nil {
		handler = func(context.Context, Event) {}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := &Client{
		cfg:        cfg,
		handler:    handler,
		httpClient: &http.Client{Transport: transport},
		pending:    make(map[string]*pendingCall),
		status: Status{
			Enabled: cfg.Enabled,
			Mode:    cfg.Mode,
			State:   StateConnecting,
			SelfID:  cfg.SelfID,
		},
	}
	if !cfg.Enabled {
		client.status.State = StateDisabled
	}
	return client, nil
}

// Status returns a JSON-serializable point-in-time snapshot.
func (c *Client) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	status := c.status
	if c.status.LastConnectedAt != nil {
		t := *c.status.LastConnectedAt
		status.LastConnectedAt = &t
	}
	return status
}

// Run starts the configured transport and blocks until ctx is canceled or a
// listener fails. Context cancellation is a graceful stop and returns nil.
func (c *Client) Run(ctx context.Context) error {
	if !c.cfg.Enabled {
		return nil
	}
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return ErrAlreadyRunning
	}
	c.running = true
	c.status.Error = ""
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()

	var err error
	switch c.cfg.Mode {
	case ModeWS:
		err = c.runForwardWS(ctx)
	case ModeWSReverse, ModeHTTP:
		err = c.runServer(ctx)
	default:
		err = fmt.Errorf("onebot: unsupported mode %q", c.cfg.Mode)
	}
	c.disconnectCurrent(ErrNotConnected)
	if ctx.Err() != nil {
		c.setState(StateConnecting, nil)
	}
	c.connections.Wait()
	c.handlers.Wait()
	return err
}

// Call invokes a OneBot API. WebSocket actions ending in _async return after
// the request is written and intentionally do not wait for an API response.
func (c *Client) Call(ctx context.Context, action string, params any) (json.RawMessage, error) {
	if !c.cfg.Enabled {
		return nil, ErrDisabled
	}
	if !actionPattern.MatchString(action) {
		return nil, fmt.Errorf("onebot: invalid action %q", action)
	}
	switch c.cfg.Mode {
	case ModeWS, ModeWSReverse:
		return c.callWS(ctx, action, params)
	case ModeHTTP:
		return c.callHTTP(ctx, action, params)
	default:
		return nil, fmt.Errorf("onebot: unsupported mode %q", c.cfg.Mode)
	}
}

func (c *Client) callHTTP(ctx context.Context, action string, params any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("onebot: encode API params: %w", err)
	}
	endpoint := strings.TrimRight(c.cfg.URL, "/") + "/" + action
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("onebot: create API request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.cfg.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.AccessToken)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("onebot: HTTP API request: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := readLimited(resp.Body, MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body := responseBody
		if len(body) > 4096 {
			body = body[:4096]
		}
		return nil, &HTTPError{StatusCode: resp.StatusCode, Status: resp.Status, Body: string(body)}
	}
	return decodeAPIResponse(action, responseBody)
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("onebot: read response: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, ErrResponseTooLarge
	}
	return body, nil
}

type apiResponse struct {
	Status  string          `json:"status"`
	Retcode int             `json:"retcode"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
	Wording string          `json:"wording"`
	Echo    json.RawMessage `json:"echo"`
}

func decodeAPIResponse(action string, body []byte) (json.RawMessage, error) {
	var response apiResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("onebot: decode API response: %w", err)
	}
	asyncAccepted := response.Retcode == 1 && response.Status == "ok" && strings.HasSuffix(action, "_async")
	if (response.Retcode != 0 && !asyncAccepted) || response.Status != "ok" {
		return nil, &APIError{
			Action: action, Status: response.Status, Retcode: response.Retcode,
			Message: response.Message, Wording: response.Wording,
		}
	}
	if response.Data == nil {
		return json.RawMessage("null"), nil
	}
	return response.Data, nil
}

// Send uses send_msg, which supports both private and group destinations.
func (c *Client) Send(ctx context.Context, target Target, message Message) (int64, error) {
	if target.GroupID < 0 || target.UserID < 0 {
		return 0, fmt.Errorf("onebot: target IDs must not be negative")
	}
	params := map[string]any{"message": message}
	switch {
	case target.GroupID > 0:
		params["message_type"] = "group"
		params["group_id"] = target.GroupID
	case target.UserID > 0:
		params["message_type"] = "private"
		params["user_id"] = target.UserID
	default:
		return 0, fmt.Errorf("onebot: target requires a positive userId or groupId")
	}
	return c.sendAction(ctx, "send_msg", params)
}

// SendPrivate uses the dedicated send_private_msg action.
func (c *Client) SendPrivate(ctx context.Context, userID int64, message Message) (int64, error) {
	if userID <= 0 {
		return 0, fmt.Errorf("onebot: userID must be positive")
	}
	return c.sendAction(ctx, "send_private_msg", map[string]any{"user_id": userID, "message": message})
}

// SendGroup uses the dedicated send_group_msg action.
func (c *Client) SendGroup(ctx context.Context, groupID int64, message Message) (int64, error) {
	if groupID <= 0 {
		return 0, fmt.Errorf("onebot: groupID must be positive")
	}
	return c.sendAction(ctx, "send_group_msg", map[string]any{"group_id": groupID, "message": message})
}

// SendText sends a single text segment.
func (c *Client) SendText(ctx context.Context, target Target, text string) (int64, error) {
	return c.Send(ctx, target, Message{{Type: "text", Data: map[string]any{"text": text}}})
}

func (c *Client) sendAction(ctx context.Context, action string, params map[string]any) (int64, error) {
	data, err := c.Call(ctx, action, params)
	if err != nil {
		return 0, err
	}
	var result struct {
		MessageID json.RawMessage `json:"message_id"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return 0, fmt.Errorf("onebot: decode send response: %w", err)
	}
	if len(result.MessageID) == 0 {
		return 0, fmt.Errorf("onebot: send response has no message_id")
	}
	decoder := json.NewDecoder(bytes.NewReader(result.MessageID))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return 0, fmt.Errorf("onebot: decode message_id: %w", err)
	}
	id, ok := int64Value(value)
	if !ok {
		return 0, fmt.Errorf("onebot: invalid message_id %s", result.MessageID)
	}
	return id, nil
}

func (c *Client) acceptEvent(event Event) error {
	if event.SelfID <= 0 {
		return fmt.Errorf("onebot: event self_id must be positive")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	expected := c.cfg.SelfID
	if expected == 0 {
		expected = c.status.SelfID
	}
	if expected != 0 && event.SelfID != expected {
		return fmt.Errorf("onebot: event self_id %d does not match %d", event.SelfID, expected)
	}
	c.status.SelfID = event.SelfID
	return nil
}

func (c *Client) dispatch(ctx context.Context, event Event) {
	c.handlers.Add(1)
	go func() {
		defer c.handlers.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				c.setError(fmt.Sprintf("event handler panic: %v", recovered))
			}
		}()
		c.handler(ctx, event)
	}()
}

func (c *Client) setError(message string) {
	c.mu.Lock()
	c.status.Error = message
	c.mu.Unlock()
}

func (c *Client) setState(state string, err error) {
	c.mu.Lock()
	c.status.State = state
	c.status.Error = errorString(err)
	c.mu.Unlock()
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
