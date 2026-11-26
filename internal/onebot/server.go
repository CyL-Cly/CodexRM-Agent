package onebot

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var reverseUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     trustedOrigin,
}

// trustedOrigin permits non-browser clients without an Origin header, but does
// not let a browser use a listening bot as a cross-origin HTTP/WebSocket API.
// OneBot reverse WebSocket clients are authorized bots, not browsers, so their
// Origin header is advisory; the bearer token still gates every connection.
func trustedOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	return len(origins) == 1 && origins[0] != "" && !strings.ContainsAny(origins[0], "\r\n")
}

// eventMediaType accepts the JSON and form encodings used by OneBot event posts.
func eventMediaType(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && (mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"))
}

func (c *Client) runServer(ctx context.Context) error {
	if c.cfg.Listen == "" {
		err := fmt.Errorf("onebot: listen is required to run %s mode", c.cfg.Mode)
		c.setState(StateRetrying, err)
		return err
	}
	listener, err := net.Listen("tcp", c.cfg.Listen)
	if err != nil {
		c.setState(StateRetrying, fmt.Errorf("onebot: listen: %w", err))
		return err
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.serveHTTP(ctx, w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.Serve(listener)
	}()
	if c.cfg.Mode == ModeHTTP {
		c.setState(StateConnected, nil)
	} else {
		c.setState(StateConnecting, nil)
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		shutdownErr := server.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			_ = server.Close()
		}
		c.disconnectCurrent(context.Canceled)
		serveErr := <-serveResult
		if shutdownErr != nil {
			return fmt.Errorf("onebot: graceful HTTP shutdown: %w", shutdownErr)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return serveErr
		}
		return nil
	case serveErr := <-serveResult:
		c.disconnectCurrent(ErrNotConnected)
		if serveErr == nil || errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		c.setState(StateRetrying, serveErr)
		return fmt.Errorf("onebot: HTTP server: %w", serveErr)
	}
}

func (c *Client) serveHTTP(runCtx context.Context, w http.ResponseWriter, r *http.Request) {
	if c.cfg.Mode == ModeWSReverse && isReverseWSPath(r.URL.Path) && websocket.IsWebSocketUpgrade(r) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		c.serveReverseWS(runCtx, w, r)
		return
	}
	if r.URL.Path == "/" || r.URL.Path == "/onebot/" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		c.serveHTTPEvent(runCtx, w, r)
		return
	}
	if c.cfg.Mode == ModeWSReverse && isReverseWSPath(r.URL.Path) {
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
		return
	}
	http.NotFound(w, r)
}

func isReverseWSPath(path string) bool {
	switch path {
	case "/", "/onebot", "/onebot/", "/ws", "/ws/":
		return true
	default:
		return false
	}
}

func (c *Client) authorized(r *http.Request) bool {
	if c.cfg.AccessToken == "" {
		return true
	}
	authorization := r.Header.Get("Authorization")
	scheme, token, _ := strings.Cut(authorization, " ")
	providedHash := sha256.Sum256([]byte(token))
	expectedHash := sha256.Sum256([]byte(c.cfg.AccessToken))
	valid := subtle.ConstantTimeCompare(providedHash[:], expectedHash[:]) == 1
	return strings.EqualFold(scheme, "Bearer") && valid && token != ""
}

func (c *Client) handshakeSelfID(r *http.Request) (int64, error) {
	raw := r.Header.Get("X-Self-ID")
	if raw == "" {
		return 0, nil
	}
	selfID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || selfID <= 0 {
		return 0, fmt.Errorf("onebot: invalid X-Self-ID")
	}
	c.mu.RLock()
	expected := c.cfg.SelfID
	if expected == 0 {
		expected = c.status.SelfID
	}
	c.mu.RUnlock()
	if expected != 0 && selfID != expected {
		return 0, fmt.Errorf("onebot: X-Self-ID does not match configured selfId")
	}
	return selfID, nil
}

func (c *Client) serveReverseWS(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if !c.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="onebot"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	selfID, err := c.handshakeSelfID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	// Count the connection before hijacking: http.Server.Shutdown deliberately
	// does not wait for hijacked handlers, while Run does.
	c.connections.Add(1)
	defer c.connections.Done()
	conn, err := reverseUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	connection := newWSConnection(conn)
	c.installConnection(connection, selfID)
	_ = c.serveWS(ctx, connection)
}

func (c *Client) serveHTTPEvent(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if !c.authorized(r) || !eventMediaType(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="onebot"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxEventBytes)
	body, err := readLimited(r.Body, MaxEventBytes)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.Is(err, ErrResponseTooLarge) || errors.As(err, &maxBytesErr) {
			http.Error(w, "event body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read event", http.StatusBadRequest)
		return
	}
	event, err := ParseEvent(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := c.acceptEvent(event); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	c.dispatch(ctx, event)
	w.WriteHeader(http.StatusNoContent)
}
