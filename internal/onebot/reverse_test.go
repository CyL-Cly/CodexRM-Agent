package onebot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type testBot struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	seen    chan string
	done    chan struct{}
}

func startTestBot(conn *websocket.Conn) *testBot {
	bot := &testBot{conn: conn, seen: make(chan string, 128), done: make(chan struct{})}
	go func() {
		defer close(bot.done)
		for {
			var request wsRequest
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
			bot.seen <- request.Action
			if request.Action == "hold" || strings.HasSuffix(request.Action, "_async") {
				continue
			}
			response := map[string]any{"status": "ok", "retcode": 0, "echo": request.Echo, "data": map[string]any{"action": request.Action}}
			if strings.HasPrefix(request.Action, "send") {
				response["data"] = map[string]any{"message_id": 900}
			}
			if bot.writeJSON(response) != nil {
				return
			}
		}
	}()
	return bot
}

func (b *testBot) writeJSON(value any) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return b.conn.WriteJSON(value)
}

func waitBotAction(t *testing.T, bot *testBot, action string) {
	t.Helper()
	for {
		select {
		case seen := <-bot.seen:
			if seen == action {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("bot did not receive action %q", action)
		}
	}
}

func dialReverse(address, path, token string, selfID int64, origin string) (*websocket.Conn, *http.Response, error) {
	header := make(http.Header)
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	if selfID != 0 {
		header.Set("X-Self-ID", fmt.Sprint(selfID))
	}
	if origin != "" {
		header.Set("Origin", origin)
	}
	return websocket.DefaultDialer.Dial("ws://"+address+path, header)
}

func closeDialResponse(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}

func TestReverseWSPathsAndAuthentication(t *testing.T) {
	address := reserveListenAddress(t)
	client, err := New(Config{
		Enabled: true, Mode: ModeWSReverse, Listen: address, AccessToken: "secret",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runClient(t, client)
	waitForListener(t, address)

	conn, response, err := dialReverse(address, "/onebot/", "", 88, "http://Bearer-secret.invalid")
	if err == nil {
		_ = conn.Close()
		t.Fatal("unauthenticated websocket connected")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		closeDialResponse(response)
		t.Fatalf("unauthenticated response = %+v, err=%v", response, err)
	}
	closeDialResponse(response)

	for _, path := range []string{"/", "/onebot", "/onebot/", "/ws", "/ws/"} {
		t.Run("path"+path, func(t *testing.T) {
			conn, response, err := dialReverse(address, path, "secret", 88, "https://untrusted-origin.invalid")
			closeDialResponse(response)
			if err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return client.Status().State == StateConnected })
			if client.Status().SelfID != 88 {
				t.Fatalf("selfId = %d", client.Status().SelfID)
			}
			_ = conn.Close()
			waitFor(t, func() bool { return client.Status().State == StateConnecting })
		})
	}

	conn, response, err = dialReverse(address, "/ws", "secret", 99, "")
	if err == nil {
		_ = conn.Close()
		t.Fatal("mismatched X-Self-ID connected")
	}
	if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("self_id response = %+v, err=%v", response, err)
	}
	closeDialResponse(response)

	conn, response, err = dialReverse(address, "/ws", "wrong", 88, "")
	if err == nil {
		_ = conn.Close()
		t.Fatal("wrong token connected")
	}
	if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token response = %+v, err=%v", response, err)
	}
	closeDialResponse(response)
}

func TestReverseWSEventsAPIAndReplacement(t *testing.T) {
	address := reserveListenAddress(t)
	type callbackResult struct {
		data json.RawMessage
		err  error
	}
	callbacks := make(chan callbackResult, 4)
	var client *Client
	client, err := New(Config{
		Enabled: true, Mode: ModeWSReverse, Listen: address, AccessToken: "secret", SelfID: 99,
	}, func(ctx context.Context, event Event) {
		if event.PostType != "message" {
			return
		}
		data, err := client.Call(ctx, "from_handler", map[string]any{"user": event.UserID})
		callbacks <- callbackResult{data: data, err: err}
	})
	if err != nil {
		t.Fatal(err)
	}
	runClient(t, client)
	waitForListener(t, address)

	conn1, response, err := dialReverse(address, "/onebot/", "secret", 99, "")
	closeDialResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	bot1 := startTestBot(conn1)
	t.Cleanup(func() { _ = conn1.Close() })
	waitFor(t, func() bool { return client.Status().State == StateConnected })
	if bot1.writeJSON(map[string]any{
		"self_id": 99, "post_type": "message", "message_type": "private", "user_id": 100,
		"message": "reverse",
	}) != nil {
		t.Fatal("send reverse event")
	}
	select {
	case result := <-callbacks:
		if result.err != nil || string(result.data) != `{"action":"from_handler"}` {
			t.Fatalf("handler Call data=%s err=%v", result.data, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler deadlocked while calling the API")
	}
	data, err := client.Call(context.Background(), "get_data", nil)
	if err != nil || string(data) != `{"action":"get_data"}` {
		t.Fatalf("reverse Call data=%s err=%v", data, err)
	}

	status := postEvent(t, "http://"+address+"/", "secret", `{"self_id":99,"post_type":"message","message_type":"private","user_id":101,"message":"http"}`, false)
	if status != http.StatusNoContent {
		t.Fatalf("shared-listener HTTP event status = %d", status)
	}
	select {
	case result := <-callbacks:
		if result.err != nil {
			t.Fatalf("HTTP event handler Call = %v", result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP event callback was canceled with the request")
	}
	if status := postEvent(t, "http://"+address+"/onebot", "secret", `{}`, false); status != http.StatusUpgradeRequired && status != http.StatusNotFound {
		t.Fatalf("POST /onebot status = %d", status)
	}

	pendingResult := make(chan error, 1)
	go func() {
		_, err := client.Call(context.Background(), "hold", nil)
		pendingResult <- err
	}()
	waitBotAction(t, bot1, "hold")

	conn2, response, err := dialReverse(address, "/ws", "secret", 99, "")
	closeDialResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	bot2 := startTestBot(conn2)
	t.Cleanup(func() { _ = conn2.Close() })
	select {
	case <-bot1.done:
	case <-time.After(2 * time.Second):
		t.Fatal("old reverse connection was not replaced")
	}
	select {
	case err := <-pendingResult:
		if err == nil {
			t.Fatal("pending call on replaced connection succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending call on replaced connection did not return")
	}
	if data, err := client.Call(context.Background(), "get_data", nil); err != nil || string(data) != `{"action":"get_data"}` {
		t.Fatalf("new reverse connection Call data=%s err=%v", data, err)
	}
	waitBotAction(t, bot2, "get_data")

	_ = conn2.Close()
	waitFor(t, func() bool { return client.Status().State == StateConnecting })
	if _, err := client.Call(context.Background(), "get_data", nil); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Call with no reverse connection = %v", err)
	}
}

func postEvent(t *testing.T, endpoint, token, body string, spoofHost bool) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Origin", "https://untrusted.invalid")
	if spoofHost {
		req.Host = "Bearer secret"
	}
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}
