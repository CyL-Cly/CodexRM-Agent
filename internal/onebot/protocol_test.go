package onebot

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	valid := []Config{
		{},
		{Enabled: false, Mode: ModeWS},
		{Enabled: true, Mode: ModeWS, URL: "ws://127.0.0.1:8080/onebot?x=1"},
		{Enabled: true, Mode: ModeWS, URL: "wss://bot.example/ws", Listen: ""},
		{Enabled: true, Mode: ModeWSReverse, Listen: "127.0.0.1:0"},
		{Enabled: true, Mode: ModeHTTP, URL: "http://127.0.0.1:8080/onebot/", Listen: ":8081", AccessToken: "secret"},
		{Enabled: true, Mode: ModeWSReverse, Listen: "localhost:8081"},
	}
	for _, cfg := range valid {
		if err := cfg.Validate(); err != nil {
			t.Errorf("valid config %+v: %v", cfg, err)
		}
	}

	invalid := []Config{
		{Enabled: true},
		{Enabled: true, Mode: "tcp"},
		{Enabled: true, Mode: ModeWS},
		{Enabled: true, Mode: ModeWS, URL: "http://localhost"},
		{Enabled: true, Mode: ModeWS, URL: "ws:///missing-host"},
		{Enabled: true, Mode: ModeWS, URL: "ws://user:pass@localhost"},
		{Enabled: true, Mode: ModeWS, URL: "ws://localhost/#fragment"},
		{Enabled: true, Mode: ModeHTTP, URL: "https://localhost/api?token=x"},
		{Enabled: true, Mode: ModeWSReverse, URL: "ws://localhost"},
		{Enabled: true, Mode: ModeWSReverse, Listen: "0.0.0.0:8080"},
		{Enabled: true, Mode: ModeWSReverse, Listen: ":8080"},
		{Enabled: true, Mode: ModeWSReverse, Listen: "[::]:8080"},
		{Enabled: true, Mode: ModeWSReverse, Listen: "192.168.1.2:8080"},
		{Enabled: true, Mode: ModeHTTP, URL: "http://127.0.0.1:8080", Listen: "0.0.0.0:8081"},
		{Enabled: true, Mode: ModeWSReverse, Listen: "localhost"},
		{Enabled: true, Mode: ModeWSReverse, Listen: "localhost:http"},
		{Enabled: true, Mode: ModeWSReverse, Listen: "localhost:65536"},
		{Enabled: true, Mode: ModeWSReverse, Listen: " 127.0.0.1:80"},
		{Enabled: true, Mode: ModeWS, URL: "ws://:8080"},
		{Enabled: true, Mode: ModeWS, URL: "ws://localhost:"},
		{Enabled: true, Mode: ModeWS, URL: "ws://localhost:0"},
		{Enabled: true, Mode: ModeWS, URL: "ws://localhost:65536"},
		{Enabled: true, Mode: ModeWSReverse, SelfID: -1},
		{Enabled: true, Mode: ModeWSReverse, AccessToken: " token"},
		{Enabled: true, Mode: ModeWSReverse, AccessToken: "a\nb"},
		{Enabled: true, Mode: ModeWSReverse, AccessToken: strings.Repeat("x", maxTokenBytes+1)},
		{Enabled: true, Mode: ModeWS, URL: "ws://localhost/" + strings.Repeat("x", maxURLBytes)},
	}
	for _, cfg := range invalid {
		if err := cfg.Validate(); err == nil {
			t.Errorf("invalid config unexpectedly accepted: %+v", cfg)
		}
	}
}

// go-cqhttp replies to *_async actions with retcode 1 ("async submitted")
// while keeping status ok. Only async actions may accept that retcode.
func TestDecodeAsyncAcceptedRetcode(t *testing.T) {
	body := []byte(`{"status":"ok","retcode":1,"data":null,"echo":"1","wording":""}`)
	if _, err := decodeAPIResponse("send_msg_async", body); err != nil {
		t.Fatalf("go-cqhttp async retcode was rejected: %v", err)
	}
	if _, err := decodeAPIResponse("send_msg", body); err == nil {
		t.Fatal("non-async action accepted retcode 1")
	}
}

func TestConfigJSONFields(t *testing.T) {
	cfg := Config{Enabled: true, Mode: ModeHTTP, URL: "http://localhost", Listen: ":80", AccessToken: "s", SelfID: 42}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"enabled":true,"mode":"http","url":"http://localhost","listen":":80","accessToken":"s","selfId":42}`
	if string(encoded) != want {
		t.Fatalf("Config JSON = %s, want %s", encoded, want)
	}
	var decoded Config
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != cfg {
		t.Fatalf("round trip = %+v, want %+v", decoded, cfg)
	}
}

func TestCQRoundTripAndEscaping(t *testing.T) {
	message := Message{
		{Type: "text", Data: map[string]any{"text": "A&[],B"}},
		{Type: "face", Data: map[string]any{"id": "1"}},
		{Type: "image", Data: map[string]any{"url": "https://x/a,b[c]&d", "file": "a,b"}},
		{Type: "custom-type", Data: map[string]any{"z": "=&#91;", "a": 12}},
	}
	encoded := EncodeCQ(message)
	want := `A&amp;&#91;&#93;,B[CQ:face,id=1][CQ:image,file=a&#44;b,url=https://x/a&#44;b&#91;c&#93;&amp;d][CQ:custom-type,a=12,z==&amp;#91;]`
	if encoded != want {
		t.Fatalf("EncodeCQ = %q, want %q", encoded, want)
	}
	decoded := DecodeCQ(encoded)
	if !reflect.DeepEqual(decoded, Message{
		{Type: "text", Data: map[string]any{"text": "A&[],B"}},
		{Type: "face", Data: map[string]any{"id": "1"}},
		{Type: "image", Data: map[string]any{"url": "https://x/a,b[c]&d", "file": "a,b"}},
		{Type: "custom-type", Data: map[string]any{"a": "12", "z": "=&#91;"}},
	}) {
		t.Fatalf("decoded mismatch: %#v", decoded)
	}
	if got := message.String(); got != encoded {
		t.Fatalf("String = %q, want %q", got, encoded)
	}
	if got := DecodeCQ("&#91;&amp;&#93;&#44;"); got.Text() != "[&]," {
		t.Fatalf("entity decode = %q", got.Text())
	}
	if got := DecodeCQ("&amp;#91;"); got.Text() != "&#91;" {
		t.Fatalf("entities were decoded more than once: %q", got.Text())
	}
}

func TestDecodeCQMalformedRetainsText(t *testing.T) {
	tests := []string{
		"plain [ text ]",
		"[CQ:image,file=x",
		"[CQ:,file=x]",
		"[CQ:image,bad]",
		"[CQ:image,=x]",
		"[not-a-cq]",
		"unknown &entity; &#x91;",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			message := DecodeCQ(input)
			if len(message) != 1 || message[0].Type != "text" || message.Text() != input {
				t.Fatalf("DecodeCQ(%q) = %#v", input, message)
			}
		})
	}
	if got := DecodeCQ(""); len(got) != 0 {
		t.Fatalf("empty CQ string = %#v", got)
	}
}

func TestParseMessageArrayStringAndFallback(t *testing.T) {
	array := `[
		{"type":"text","data":{"text":"hi"}},
		{"type":"face","data":{"id":1}},
		{"type":"image","data":{"file":"a.png"}},
		{"type":"record","data":{"file":"a.amr"}},
		{"type":"video","data":{"file":"a.mp4"}},
		{"type":"at","data":{"qq":"42"}},
		{"type":"reply","data":{"id":9}},
		{"type":"forward","data":{"id":"f"}},
		{"type":"file","data":{"name":"x"}},
		{"type":"new-kind","data":{"anything":{"x":true}}}
	]`
	message := ParseMessage(json.RawMessage(array), "ignored")
	if len(message) != 10 || message[9].Type != "new-kind" {
		t.Fatalf("array parse = %#v", message)
	}
	if message[9].Data["anything"].(map[string]any)["x"] != true {
		t.Fatalf("unknown segment data not preserved: %#v", message[9])
	}

	fromString := ParseMessage(json.RawMessage(`"hello[CQ:at,qq=42]"`), "ignored")
	if fromString.Text() != "hello" || !fromString.At(42) {
		t.Fatalf("CQ JSON string parse = %#v", fromString)
	}
	fallback := ParseMessage(nil, "fallback[CQ:face,id=2]")
	if fallback.Text() != "fallback" || len(fallback) != 2 {
		t.Fatalf("raw_message fallback = %#v", fallback)
	}
	malformed := ParseMessage(json.RawMessage(`[{"broken":`), "raw fallback")
	if malformed.Text() != "raw fallback" {
		t.Fatalf("malformed fallback = %#v", malformed)
	}
	retained := ParseMessage(json.RawMessage(`{"not":"a message"}`), "")
	if retained.Text() != `{"not":"a message"}` {
		t.Fatalf("malformed raw payload lost: %#v", retained)
	}
	if got := ParseMessage(json.RawMessage("null"), ""); len(got) != 0 {
		t.Fatalf("null message = %#v", got)
	}
}

func TestMessageHelpers(t *testing.T) {
	other := Segment{Type: "at", Data: map[string]any{"qq": float64(7)}}
	self := Segment{Type: "at", Data: map[string]any{"qq": "42"}}
	image := Segment{Type: "image", Data: map[string]any{"file": "x"}}
	message := Message{
		{Type: "text", Data: map[string]any{"text": "a"}},
		other, self,
		{Type: "text", Data: map[string]any{"text": "b"}}, image,
	}
	if !message.At(42) || message.At(43) || message.Text() != "ab" {
		t.Fatalf("unexpected helpers: At=%v Text=%q", message.At(42), message.Text())
	}
	without := message.WithoutAt(42)
	if len(without) != 4 || without.At(42) || !without.At(7) {
		t.Fatalf("WithoutAt = %#v", without)
	}
	if len(message) != 5 {
		t.Fatal("WithoutAt mutated its receiver")
	}
	images := message.Images()
	if !reflect.DeepEqual(images, []Segment{image}) {
		t.Fatalf("Images = %#v", images)
	}
}

func TestParseEventKinds(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		postType   string
		target     Target
		text       string
		messageID  int64
		checkExtra func(*testing.T, Event)
	}{
		{
			name: "private array", postType: "message", target: Target{UserID: 100}, text: "hello", messageID: 1,
			payload: `{"time":1,"self_id":42,"post_type":"message","message_type":"private","sub_type":"friend","message_id":1,"user_id":100,"message":[{"type":"text","data":{"text":"hello"}}],"sender":{"user_id":100,"nickname":"n"}}`,
		},
		{
			name: "group CQ", postType: "message", target: Target{GroupID: 200}, text: "hi", messageID: 2,
			payload: `{"self_id":42,"post_type":"message","message_type":"group","message_id":2,"group_id":200,"user_id":100,"message":"hi[CQ:at,qq=42]","raw_message":"hi[CQ:at,qq=42]"}`,
			checkExtra: func(t *testing.T, event Event) {
				if !event.Message.At(42) {
					t.Fatal("group mention not decoded")
				}
			},
		},
		{name: "message sent", postType: "message_sent", target: Target{GroupID: 200}, text: "echo", messageID: 3, payload: `{"self_id":42,"post_type":"message_sent","message_type":"group","message_id":3,"group_id":200,"user_id":42,"message":"echo"}`},
		{name: "friend request", postType: "request", target: Target{UserID: 100}, payload: `{"self_id":42,"post_type":"request","request_type":"friend","user_id":100,"comment":"add","flag":"f"}`},
		{name: "group request", postType: "request", target: Target{UserID: 100}, payload: `{"self_id":42,"post_type":"request","request_type":"group","sub_type":"invite","group_id":200,"user_id":100,"flag":"g"}`},
		{name: "notice", postType: "notice", target: Target{UserID: 100}, payload: `{"self_id":42,"post_type":"notice","notice_type":"group_ban","sub_type":"ban","group_id":200,"user_id":100,"operator_id":101,"duration":60}`},
		{name: "meta", postType: "meta_event", payload: `{"self_id":42,"post_type":"meta_event","meta_event_type":"heartbeat","status":{"online":true},"interval":5000}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := []byte(test.payload)
			event, err := ParseEvent(input)
			if err != nil {
				t.Fatal(err)
			}
			if event.PostType != test.postType || event.Target() != test.target || event.Message.Text() != test.text || event.MessageID != test.messageID {
				t.Fatalf("event = %+v", event)
			}
			if string(event.Raw) != test.payload {
				t.Fatalf("Raw = %s", event.Raw)
			}
			input[0] = '['
			if string(event.Raw) != test.payload {
				t.Fatal("Raw aliases input")
			}
			if test.checkExtra != nil {
				test.checkExtra(t, event)
			}
		})
	}
}

func TestParseEventPreservesOriginalBytes(t *testing.T) {
	raw := []byte(" \n{\"self_id\":42,\"post_type\":\"meta_event\",\"meta_event_type\":\"heartbeat\"}\n\t")
	event, err := ParseEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(event.Raw) != string(raw) {
		t.Fatalf("Raw = %q, want original %q", event.Raw, raw)
	}
}

func TestParseEventMalformed(t *testing.T) {
	for _, raw := range []string{"", "null", "[]", "{", `{"self_id":42}`, `{"post_type":[]}`} {
		if _, err := ParseEvent([]byte(raw)); err == nil {
			t.Errorf("ParseEvent(%q) unexpectedly succeeded", raw)
		}
	}
}
