package onebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPAPIEventsAndLimits(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/onebot/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		action := strings.TrimPrefix(r.URL.Path, "/onebot/")
		var params map[string]any
		if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
			http.Error(w, "bad JSON", http.StatusBadRequest)
			return
		}
		if _, wrapped := params["params"]; wrapped {
			t.Errorf("HTTP params were incorrectly wrapped: %#v", params)
		}
		w.Header().Set("Content-Type", "application/json")
		switch action {
		case "get_data":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "retcode": 0, "data": map[string]any{"value": params["value"]}})
		case "send_msg":
			if params["message_type"] != "group" || params["group_id"] != float64(200) {
				t.Errorf("send_msg params = %#v", params)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "retcode": 0, "data": map[string]any{"message_id": 321}})
		case "send_private_msg", "send_group_msg":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "retcode": 0, "data": map[string]any{"message_id": "322"}})
		case "fail":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "failed", "retcode": 1400, "message": "denied"})
		case "bad_status":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "unknown", "retcode": 0, "data": nil})
		case "http_error":
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte("teapot"))
		case "slow":
			<-r.Context().Done()
		case "large":
			_, _ = fmt.Fprint(w, `{"status":"ok","retcode":0,"data":"`)
			_, _ = fmt.Fprint(w, strings.Repeat("x", int(MaxResponseBytes)))
			_, _ = fmt.Fprint(w, `"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)

	address := reserveListenAddress(t)
	events := make(chan Event, 4)
	client, err := New(Config{
		Enabled: true, Mode: ModeHTTP, URL: api.URL + "/onebot/", Listen: address,
		AccessToken: "secret", SelfID: 77,
	}, func(_ context.Context, event Event) { events <- event })
	if err != nil {
		t.Fatal(err)
	}
	runClient(t, client)
	waitForListener(t, address)
	if client.Status().State != StateConnected {
		t.Fatalf("HTTP listener status = %+v", client.Status())
	}

	data, err := client.Call(context.Background(), "get_data", map[string]any{"value": "ok"})
	if err != nil || string(data) != `{"value":"ok"}` {
		t.Fatalf("HTTP Call data=%s err=%v", data, err)
	}
	if id, err := client.SendText(context.Background(), Target{GroupID: 200}, "hello"); err != nil || id != 321 {
		t.Fatalf("HTTP SendText id=%d err=%v", id, err)
	}
	if id, err := client.SendPrivate(context.Background(), 100, Message{}); err != nil || id != 322 {
		t.Fatalf("HTTP SendPrivate id=%d err=%v", id, err)
	}
	if id, err := client.SendGroup(context.Background(), 200, Message{}); err != nil || id != 322 {
		t.Fatalf("HTTP SendGroup id=%d err=%v", id, err)
	}
	if _, err := client.Send(context.Background(), Target{}, Message{}); err == nil {
		t.Fatal("empty target unexpectedly accepted")
	}

	_, err = client.Call(context.Background(), "fail", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Retcode != 1400 || apiErr.Action != "fail" {
		t.Fatalf("HTTP API error = %#v (%v)", apiErr, err)
	}
	if _, err := client.Call(context.Background(), "bad_status", nil); !errors.As(err, &apiErr) {
		t.Fatalf("bad API status = %v", err)
	}
	_, err = client.Call(context.Background(), "http_error", nil)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTeapot {
		t.Fatalf("HTTP status error = %#v (%v)", httpErr, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err = client.Call(ctx, "slow", nil)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("HTTP context timeout = %v", err)
	}
	if _, err := client.Call(context.Background(), "large", nil); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("large HTTP response = %v", err)
	}

	private := `{"self_id":77,"post_type":"message","message_type":"private","message_id":1,"user_id":100,"message":[{"type":"text","data":{"text":"private"}}]}`
	if status := postEvent(t, "http://"+address+"/", "secret", private, false); status != http.StatusNoContent {
		t.Fatalf("POST / = %d", status)
	}
	group := `{"self_id":77,"post_type":"message","message_type":"group","message_id":2,"group_id":200,"user_id":100,"message":"group[CQ:image,file=x]"}`
	if status := postEvent(t, "http://"+address+"/onebot/", "secret", group, false); status != http.StatusNoContent {
		t.Fatalf("POST /onebot/ = %d", status)
	}
	for i, want := range []Target{{UserID: 100}, {GroupID: 200}} {
		select {
		case event := <-events:
			if event.Target() != want || event.MessageID != int64(i+1) {
				t.Fatalf("HTTP event %d = %+v", i, event)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("HTTP event %d not received", i)
		}
	}

	if status := postEvent(t, "http://"+address+"/", "", private, true); status != http.StatusUnauthorized {
		t.Fatalf("Host/Origin spoof status = %d", status)
	}
	wrongSelf := strings.Replace(private, `"self_id":77`, `"self_id":78`, 1)
	if status := postEvent(t, "http://"+address+"/", "secret", wrongSelf, false); status != http.StatusForbidden {
		t.Fatalf("wrong self_id status = %d", status)
	}
	if status := postEvent(t, "http://"+address+"/ws", "secret", private, false); status != http.StatusNotFound {
		t.Fatalf("unknown event path status = %d", status)
	}
	prefix := `{"self_id":77,"post_type":"notice","notice_type":"x","comment":"`
	suffix := `"}`
	atLimit := prefix + strings.Repeat("x", int(MaxEventBytes)-len(prefix)-len(suffix)) + suffix
	if len(atLimit) != int(MaxEventBytes) {
		t.Fatalf("test event length = %d", len(atLimit))
	}
	if status := postEvent(t, "http://"+address+"/", "secret", atLimit, false); status != http.StatusNoContent {
		t.Fatalf("event at 32 MiB boundary status = %d", status)
	}
	select {
	case event := <-events:
		if event.PostType != "notice" || len(event.Comment) != int(MaxEventBytes)-len(prefix)-len(suffix) {
			t.Fatalf("boundary event was not preserved")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("boundary event not received")
	}
	if status := postEvent(t, "http://"+address+"/", "secret", atLimit+" ", false); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("event above 32 MiB boundary status = %d", status)
	}
}
