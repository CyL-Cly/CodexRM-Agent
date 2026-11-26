package onebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type wsRequest struct {
	Action string          `json:"action"`
	Params map[string]any  `json:"params"`
	Echo   json.RawMessage `json:"echo"`
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}

func runClient(t *testing.T, client *Client) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned an error on cancellation: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not stop after context cancellation")
		}
	})
}

func reserveListenAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func waitForListener(t *testing.T, address string) {
	t.Helper()
	waitFor(t, func() bool {
		conn, err := net.DialTimeout("tcp", address, 30*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		return false
	})
}

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func TestForwardWSAPIEventsAndConcurrentCalls(t *testing.T) {
	asyncSeen := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{
			"time": 1, "self_id": 42, "post_type": "message", "message_type": "private",
			"message_id": 9, "user_id": 100, "message": "incoming",
		})
		for {
			var request wsRequest
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
			response := map[string]any{"status": "ok", "retcode": 0, "echo": request.Echo}
			switch request.Action {
			case "get_data":
				response["data"] = map[string]any{"value": request.Params["value"]}
			case "send_msg":
				if request.Params["message_type"] != "private" || request.Params["user_id"] != float64(100) {
					t.Errorf("send_msg params = %#v", request.Params)
				}
				response["data"] = map[string]any{"message_id": 123}
			case "send_private_msg":
				response["data"] = map[string]any{"message_id": "124"}
			case "send_group_msg":
				response["data"] = map[string]any{"message_id": 125}
			case "parallel":
				response["data"] = map[string]any{"value": request.Params["value"]}
			case "fail":
				response["status"] = "failed"
				response["retcode"] = 400
				response["wording"] = "bad request"
			case "hold_async":
				asyncSeen <- struct{}{}
				continue
			default:
				response["status"] = "failed"
				response["retcode"] = 404
			}
			if err := conn.WriteJSON(response); err != nil {
				return
			}
		}
	}))
	t.Cleanup(api.Close)

	events := make(chan Event, 1)
	client, err := New(Config{
		Enabled: true, Mode: ModeWS, URL: wsURL(api.URL), AccessToken: "secret", SelfID: 42,
	}, func(_ context.Context, event Event) { events <- event })
	if err != nil {
		t.Fatal(err)
	}
	runClient(t, client)
	waitFor(t, func() bool { return client.Status().State == StateConnected })
	if err := client.Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Run = %v, want ErrAlreadyRunning", err)
	}

	select {
	case event := <-events:
		if event.Target() != (Target{UserID: 100}) || event.Message.Text() != "incoming" {
			t.Fatalf("event = %+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forward WS event not received")
	}

	data, err := client.Call(context.Background(), "get_data", map[string]any{"value": "ok"})
	if err != nil || string(data) != `{"value":"ok"}` {
		t.Fatalf("Call data=%s err=%v", data, err)
	}
	if _, err := client.Call(context.Background(), "get_data", map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("unsupported JSON params unexpectedly accepted")
	}
	if client.Status().State != StateConnected {
		t.Fatal("JSON encoding error disconnected a healthy websocket")
	}
	data, err = client.Call(context.Background(), "get_data", map[string]any{"value": "still-connected"})
	if err != nil || string(data) != `{"value":"still-connected"}` {
		t.Fatalf("Call after encoding error data=%s err=%v", data, err)
	}
	messageID, err := client.SendText(context.Background(), Target{UserID: 100}, "hello")
	if err != nil || messageID != 123 {
		t.Fatalf("SendText id=%d err=%v", messageID, err)
	}
	if id, err := client.SendPrivate(context.Background(), 100, Message{}); err != nil || id != 124 {
		t.Fatalf("SendPrivate id=%d err=%v", id, err)
	}
	if id, err := client.SendGroup(context.Background(), 200, Message{}); err != nil || id != 125 {
		t.Fatalf("SendGroup id=%d err=%v", id, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	data, err = client.Call(ctx, "hold_async", nil)
	if err != nil || data != nil {
		t.Fatalf("async Call data=%s err=%v", data, err)
	}
	select {
	case <-asyncSeen:
	case <-time.After(time.Second):
		t.Fatal("async request was not written")
	}

	_, err = client.Call(context.Background(), "fail", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Retcode != 400 || apiErr.Action != "fail" {
		t.Fatalf("API error = %#v (%v)", apiErr, err)
	}

	const callCount = 24
	errs := make(chan error, callCount)
	var wg sync.WaitGroup
	for i := 0; i < callCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data, err := client.Call(context.Background(), "parallel", map[string]any{"value": i})
			if err == nil {
				var decoded map[string]int
				if json.Unmarshal(data, &decoded) != nil || decoded["value"] != i {
					err = fmt.Errorf("mismatched echo response %s for %d", data, i)
				}
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}

	status := client.Status()
	if status.SelfID != 42 || status.LastConnectedAt == nil {
		t.Fatalf("Status = %+v", status)
	}
	encodedStatus, err := json.Marshal(status)
	if err != nil || !strings.Contains(string(encodedStatus), `"selfId":42`) || !strings.Contains(string(encodedStatus), `"lastConnectedAt"`) {
		t.Fatalf("Status JSON = %s, err=%v", encodedStatus, err)
	}
}

func TestForwardWSAcceptsEventAboveFourMiB(t *testing.T) {
	const textSize = 5 << 20
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if err := conn.WriteJSON(map[string]any{
			"self_id": 42, "post_type": "message", "message_type": "private", "user_id": 100,
			"message": []Segment{{Type: "text", Data: map[string]any{"text": strings.Repeat("x", textSize)}}},
		}); err != nil {
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(api.Close)
	events := make(chan Event, 1)
	client, err := New(Config{Enabled: true, Mode: ModeWS, URL: wsURL(api.URL)}, func(_ context.Context, event Event) {
		events <- event
	})
	if err != nil {
		t.Fatal(err)
	}
	runClient(t, client)
	select {
	case event := <-events:
		if len(event.Message.Text()) != textSize || event.SelfID != 42 {
			t.Fatalf("large event text length = %d, selfId = %d", len(event.Message.Text()), event.SelfID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event above the old 4 MiB limit was not delivered")
	}
	if got := client.Status().SelfID; got != 42 {
		t.Fatalf("selfId learned from event = %d", got)
	}
}

func TestForwardWSDisconnectFailsAllPending(t *testing.T) {
	const pendingCount = 8
	var connected atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connected.Add(1)
		for i := 0; i < pendingCount; i++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
		_ = conn.Close()
	}))
	t.Cleanup(api.Close)
	client, err := New(Config{Enabled: true, Mode: ModeWS, URL: wsURL(api.URL), SelfID: 42}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runClient(t, client)
	waitFor(t, func() bool { return connected.Load() == 1 && client.Status().State == StateConnected })

	results := make(chan error, pendingCount)
	for i := 0; i < pendingCount; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := client.Call(ctx, "never_answer", nil)
			results <- err
		}()
	}
	for i := 0; i < pendingCount; i++ {
		select {
		case err := <-results:
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("pending Call error = %v", err)
			}
		case <-time.After(4 * time.Second):
			t.Fatal("pending Call remained blocked after disconnect")
		}
	}
}

func TestForwardWSRunContextCancellation(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(api.Close)
	client, err := New(Config{Enabled: true, Mode: ModeWS, URL: wsURL(api.URL)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	waitFor(t, func() bool { return client.Status().State == StateConnected })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run cancellation = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forward Run did not stop")
	}
	if _, err := client.Call(context.Background(), "x", nil); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Call after stop = %v", err)
	}
}

func TestDisabledClientAndMissingListen(t *testing.T) {
	disabled, err := New(Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Status().State != StateDisabled || disabled.Run(context.Background()) != nil {
		t.Fatalf("disabled client status/run = %+v", disabled.Status())
	}
	if _, err := disabled.Call(context.Background(), "x", nil); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled Call = %v", err)
	}
	missing, err := New(Config{Enabled: true, Mode: ModeWSReverse}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("missing listen Run = %v", err)
	}
}
