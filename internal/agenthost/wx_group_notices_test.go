package agenthost

import (
	"encoding/json"
	"strings"
	"testing"
)

func noticeJSON(t *testing.T, source string) map[string]json.RawMessage {
	t.Helper()
	var values map[string]json.RawMessage
	if err := json.Unmarshal([]byte(source), &values); err != nil {
		t.Fatal(err)
	}
	return values
}

func TestWXGroupNoticePatchKeepsExistingMediaAndUpdatesOnlyNotices(t *testing.T) {
	current := noticeJSON(t, `{
		"welcome_type":"image","welcome_image_url":"https://example.invalid/welcome.jpg",
		"leave_chat_room_alert_type":"url","leave_chat_room_alert_card_title":"再见",
		"leave_chat_room_alert_text":"原来的退群模板","chat_ai_enabled":true
	}`)
	input := noticeJSON(t, `{"welcome_enabled":true,"leave_chat_room_alert_enabled":false}`)
	patch, err := wxGroupNoticePatch("42@chatroom", 27, current, input, true)
	if err != nil {
		t.Fatal(err)
	}
	if patch["id"] != int64(27) || string(patch["welcome_type"].(json.RawMessage)) != `"image"` ||
		string(patch["welcome_image_url"].(json.RawMessage)) != `"https://example.invalid/welcome.jpg"` {
		t.Fatalf("unexpected notice update: %#v", patch)
	}
	for _, key := range []string{"chat_ai_enabled", "leave_chat_room_alert_type", "leave_chat_room_alert_text", "leave_chat_room_alert_card_title"} {
		if _, ok := patch[key]; ok {
			t.Fatalf("switching off a notice should leave existing content intact: %s", key)
		}
	}
}

func TestWXGroupNoticePatchInitializesFromEffectiveDefaults(t *testing.T) {
	global := noticeJSON(t, `{
		"chat_ai_enabled":true,"chat_api_key":"secret",
		"welcome_type":"url","welcome_text":"欢迎","welcome_url":"https://example.com","welcome_card_title":"群规",
		"leave_chat_room_alert_text":"{placeholder}退出了群聊"
	}`)
	input := noticeJSON(t, `{
		"welcome_enabled":true,"leave_chat_room_alert_enabled":true,"leave_chat_room_alert_type":"url",
		"leave_chat_room_alert_url":"https://example.com/leave","leave_chat_room_alert_card_description":""
	}`)
	patch, err := wxGroupNoticePatch("42@chatroom", 0, global, input, true)
	if err != nil {
		t.Fatal(err)
	}
	if patch["chat_room_id"] != "42@chatroom" {
		t.Fatalf("wrong group: %#v", patch)
	}
	for key, want := range map[string]string{
		"chat_ai_enabled": "true", "welcome_type": `"url"`, "welcome_card_title": `"群规"`,
		"leave_chat_room_alert_text": `"{placeholder}退出了群聊"`, "leave_chat_room_alert_card_description": `""`,
	} {
		got, ok := patch[key].(json.RawMessage)
		if !ok || string(got) != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	for _, key := range []string{"id", "chat_api_key"} {
		if _, ok := patch[key]; ok {
			t.Fatalf("new group notice settings must not copy %q", key)
		}
	}
}

func TestWXNoticeInputSupportsLegacyClients(t *testing.T) {
	legacy := `{"welcome_enabled":true,"welcome_text":"欢迎","leave_chat_room_alert_enabled":true,"leave_chat_room_alert_text":"再见"}`
	input, err := wxDecodeNoticeInput(strings.NewReader(legacy), false)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := wxGroupNoticePatch("42@chatroom", 1, nil, input, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(patch["welcome_type"].(json.RawMessage)) != `"text"` || patch["leave_chat_room_alert_type"] != nil {
		t.Fatalf("wrong legacy patch: %#v", patch)
	}
	newCard := `{"welcome_enabled":false,"leave_chat_room_alert_enabled":true,"leave_chat_room_alert_type":"url","leave_chat_room_alert_url":"https://example.com"}`
	if _, err := wxDecodeNoticeInput(strings.NewReader(newCard), false); err == nil {
		t.Fatal("expected old client to reject unsupported formats")
	}
	invalidImage := `{"welcome_enabled":true,"welcome_type":"image","welcome_image_url":"file:///tmp/image.jpg","leave_chat_room_alert_enabled":false}`
	if _, err := wxDecodeNoticeInput(strings.NewReader(invalidImage), true); err == nil {
		t.Fatal("expected invalid image URL to be rejected")
	}
}
