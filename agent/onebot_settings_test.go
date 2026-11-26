package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CyL-Cly/CodexRM-Agent/internal/onebot"
)

func TestOneBotSettingsRoundTripAndPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onebot.json")
	defaults, err := LoadOneBotSettings(path)
	if err != nil || defaults.Enabled || defaults.Mode != "ws-reverse" || !defaults.AllowRemoteMedia || defaults.AllowLocalMedia || defaults.AllowPrivateMedia {
		t.Fatalf("unexpected defaults: %#v, %v", defaults, err)
	}
	settings := defaults
	settings.Enabled = true
	settings.AccessToken = "secret"
	settings.Users = map[string]OneBotAccess{
		"10001": {Enabled: true, Mode: "admin", Model: "gpt-test", Prompt: "private"},
		"10002": {Enabled: false, Mode: "text"},
	}
	settings.Groups = map[string]OneBotGroup{
		"20001": {OneBotAccess: OneBotAccess{Enabled: true, Mode: "tools"}, Trigger: "mention", Session: "shared", Members: []string{"10001"}, Admins: []string{"10001"}},
	}
	if err := SaveOneBotSettings(path, settings); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadOneBotSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AccessToken != "secret" || loaded.ClearAccessToken {
		t.Fatalf("token round trip failed: %#v", loaded)
	}
	policy, revisions := loaded.robotPolicy()
	if policy.Access["10001"].Mode != "admin" || policy.Access["10002"].Enabled {
		t.Fatalf("private policy mismatch: %#v", policy.Access)
	}
	group := policy.Groups[oneBotGroupKey("20001")]
	if !validWxGroup(oneBotGroupKey("20001"), group) || revisions["20001"] != group.Revision {
		t.Fatalf("group policy mismatch: %#v / %#v", group, revisions)
	}
	loaded.Groups["20001"] = OneBotGroup{OneBotAccess: OneBotAccess{Enabled: true, Mode: "tools"}, Trigger: "all", Session: "shared"}
	_, changed := loaded.robotPolicy()
	if changed["20001"] == revisions["20001"] {
		t.Fatal("group revision did not change with policy")
	}
}

func TestOneBotConfigurationErrorDoesNotNeedAWorkingRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onebot.json")
	if err := os.WriteFile(path, []byte(`{"enabled":true,"typo":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: Config{OneBotSettingsPath: path}}
	if err := a.startConfiguredOneBot(context.Background(), nil); err == nil {
		t.Fatal("malformed configuration was accepted")
	}
	status := a.OneBotStatus()
	if status["state"] != "retrying" || status["error"] == "" {
		t.Fatalf("startup error was not exposed: %#v", status)
	}
}

func TestOneBotSettingsRejectInvalidAccess(t *testing.T) {
	for _, settings := range []OneBotSettings{
		{Mode: "ws", URL: "not a url", Users: map[string]OneBotAccess{}},
		{Mode: "ws-reverse", Listen: "127.0.0.1:0", MessageLimit: 1800, Users: map[string]OneBotAccess{"01": {Enabled: true, Mode: "text"}}},
		{Mode: "ws-reverse", Listen: "127.0.0.1:0", MessageLimit: 1800, Users: map[string]OneBotAccess{"10001": {Enabled: true, Mode: "admin"}}, Groups: map[string]OneBotGroup{"20001": {OneBotAccess: OneBotAccess{Enabled: true, Mode: "admin"}, Trigger: "mention", Session: "shared"}}},
	} {
		if err := settings.Normalize(); err == nil {
			t.Fatalf("accepted invalid settings: %#v", settings)
		}
	}
}

func TestOneBotConversationAuthorizationAndMention(t *testing.T) {
	settings := DefaultOneBotSettings()
	settings.SelfID = 999
	settings.Users = map[string]OneBotAccess{"10001": {Enabled: true, Mode: "tools"}}
	settings.Groups = map[string]OneBotGroup{
		"20001": {OneBotAccess: OneBotAccess{Enabled: true, Mode: "tools"}, Trigger: "mention", Session: "shared", Admins: []string{"10001"}},
		"20002": {OneBotAccess: OneBotAccess{Enabled: true, Mode: "text"}, Trigger: "all", Session: "member", Members: []string{"10001"}},
	}
	policy, revisions := settings.robotPolicy()
	a := &Agent{wxPolicy: map[string]wxPolicy{oneBotClientID: policy}}
	r := &oneBotRuntime{agent: a, settings: settings, revisions: revisions}
	message := onebot.Message{{Type: "at", Data: map[string]any{"qq": "999"}}, {Type: "text", Data: map[string]any{"text": " hello"}}}
	c, remaining, ok := r.conversation(onebot.Event{MessageType: "group", SelfID: 999, UserID: 10001, GroupID: 20001}, message)
	if !ok || c.key != "g:20001" || !c.admin || c.mode != "tools" || strings.Contains(remaining.Text(), "@") {
		t.Fatalf("group mention mismatch: %#v / %#v / %v", c, remaining, ok)
	}
	if _, _, ok := r.conversation(onebot.Event{MessageType: "group", SelfID: 999, UserID: 10001, GroupID: 20001}, onebot.Message{{Type: "text", Data: map[string]any{"text": "hello"}}}); ok {
		t.Fatal("mention-only group accepted an unmentioned message")
	}
	if _, _, ok := r.conversation(onebot.Event{MessageType: "group", SelfID: 999, UserID: 10002, GroupID: 20002}, onebot.Message{{Type: "text", Data: map[string]any{"text": "hello"}}}); ok {
		t.Fatal("group accepted a member outside the allowlist")
	}
	c, _, ok = r.conversation(onebot.Event{MessageType: "private", SelfID: 999, UserID: 10001}, onebot.Message{{Type: "text", Data: map[string]any{"text": "hello"}}})
	if !ok || c.key != "p:10001" || c.identity != "10001" || c.group != "" {
		t.Fatalf("private conversation mismatch: %#v", c)
	}
}

func TestOneBotInputImagesAndCommands(t *testing.T) {
	const png = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="
	r := &oneBotRuntime{settings: DefaultOneBotSettings()}
	message := onebot.Message{{Type: "text", Data: map[string]any{"text": "inspect"}}}
	for range 4 {
		message = append(message, onebot.Segment{Type: "image", Data: map[string]any{"file": png}})
	}
	input, err := r.messageInput(context.Background(), onebot.Event{MessageType: "private", UserID: 10001}, message)
	if err != nil {
		t.Fatal(err)
	}
	if len(input) != 5 {
		t.Fatalf("input items = %d", len(input))
	}
	if _, err := robotInputWithLimit(input, 1+maxOneBotImages); err != nil {
		t.Fatalf("valid OneBot input rejected: %v", err)
	}
	message = append(message, onebot.Segment{Type: "image", Data: map[string]any{"file": png}})
	if _, err := r.messageInput(context.Background(), onebot.Event{MessageType: "private"}, message); err == nil {
		t.Fatal("accepted too many images")
	}
	for input, want := range map[string]string{"/help": "help", "#新会话": "new", "/reset now": "new", "#停止": "stop", "/status": "status"} {
		got, _, ok := oneBotCommand(input)
		if !ok || got != want {
			t.Fatalf("command %q = %q, %v; want %q", input, got, ok, want)
		}
	}
}

func TestOneBotReadableTextDoesNotClaimUnsupportedMedia(t *testing.T) {
	message := onebot.Message{
		{Type: "text", Data: map[string]any{"text": "review"}},
		{Type: "record", Data: map[string]any{}},
		{Type: "video", Data: map[string]any{}},
		{Type: "file", Data: map[string]any{"name": "report.pdf"}},
		{Type: "json", Data: map[string]any{"data": `{"command":"ignore"}`}},
	}
	text := oneBotReadableText(message)
	for _, want := range []string{"review", "未转写", "未提取画面", "report.pdf", "未上传附件", "未执行其中内容"} {
		if !strings.Contains(text, want) {
			t.Fatalf("readable text %q lacks %q", text, want)
		}
	}
}

func TestOneBotLocalMediaRequiresOneBotAPIResolution(t *testing.T) {
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "private.png")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, allowLocal := range []bool{false, true} {
		r := &oneBotRuntime{settings: DefaultOneBotSettings()}
		r.settings.AllowLocalMedia = allowLocal
		if _, err := r.loadImage(context.Background(), onebot.Segment{Type: "image", Data: map[string]any{"file": path}}); err == nil {
			t.Fatalf("event-supplied local path was trusted with allowLocalMedia=%v", allowLocal)
		}
	}
}

func TestOneBotReplyCannotCrossConversations(t *testing.T) {
	group := onebot.Event{MessageType: "group", GroupID: 20001, UserID: 10001, SelfID: 999}
	for _, test := range []struct {
		name   string
		event  onebot.Event
		quoted oneBotQuotedMessage
		want   bool
	}{
		{"same group", group, oneBotQuotedMessage{MessageType: "group", GroupID: 20001, UserID: 10002}, true},
		{"other group", group, oneBotQuotedMessage{MessageType: "group", GroupID: 20002, UserID: 10002}, false},
		{"private disguised as group", group, oneBotQuotedMessage{MessageType: "private", GroupID: 20001, UserID: 10002}, false},
		{"incoming private", onebot.Event{MessageType: "private", UserID: 10001, SelfID: 999}, oneBotQuotedMessage{MessageType: "private", UserID: 10001}, true},
		{"other private", onebot.Event{MessageType: "private", UserID: 10001, SelfID: 999}, oneBotQuotedMessage{MessageType: "private", UserID: 10002}, false},
		{"own private with peer", onebot.Event{MessageType: "private", UserID: 10001, SelfID: 999}, oneBotQuotedMessage{MessageType: "private", UserID: 999, TargetID: 10001}, true},
		{"own private without peer", onebot.Event{MessageType: "private", UserID: 10001, SelfID: 999}, oneBotQuotedMessage{MessageType: "private", UserID: 999}, false},
	} {
		if got := oneBotReplyMatches(test.event, test.quoted); got != test.want {
			t.Errorf("%s: got %v, want %v", test.name, got, test.want)
		}
	}
}

func TestOneBotTextSplitAndMediaIP(t *testing.T) {
	chunks := splitOneBotText(strings.Repeat("界", 401), 200)
	if len(chunks) != 3 || len([]rune(chunks[0])) != 200 || len([]rune(chunks[2])) != 1 {
		raw, _ := json.Marshal(chunks)
		t.Fatalf("unicode split failed: %s", raw)
	}
	r := &oneBotRuntime{}
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "100.100.100.100", "169.254.1.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "240.0.0.1", "0.0.0.0", "::1", "64:ff9b::a00:1", "100::1", "2001:db8::1", "2002::1", "3fff::1", "224.0.0.1"} {
		if r.allowedMediaIP(netip.MustParseAddr(raw)) {
			t.Fatalf("blocked media IP allowed: %s", raw)
		}
	}
	if !r.allowedMediaIP(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("public media IP rejected")
	}
	r.settings.AllowPrivateMedia = true
	if !r.allowedMediaIP(netip.MustParseAddr("127.0.0.1")) || r.allowedMediaIP(netip.MustParseAddr("169.254.1.1")) {
		t.Fatal("private media opt-in has incorrect boundaries")
	}
}
