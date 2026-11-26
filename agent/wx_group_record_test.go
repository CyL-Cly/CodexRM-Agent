package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWxGroupIdentityAndAccess(t *testing.T) {
	revision := strings.Repeat("0", 32)
	group := wxGroupPolicy{Session: "shared", Mode: "text", Members: []string{"alice", "bob"}, Admins: []string{"alice"}, Revision: revision}
	policy := wxPolicy{Access: map[string]wxAccess{"bob": {Enabled: true, Mode: "admin"}}, Groups: map[string]wxGroupPolicy{"123@chatroom": group, "456@chatroom": group}}
	params := func(id, actor string) map[string]any {
		return map[string]any{"wxId": actor, "groupId": id, "groupRevision": revision}
	}
	alice, mode, ok := wxThreadIdentity(policy, "turn/start", params("123@chatroom", "alice"))
	if !ok || mode != "text" {
		t.Fatalf("shared member not authorized: %s %v", mode, ok)
	}
	bob, mode, ok := wxThreadIdentity(policy, "turn/start", params("123@chatroom", "bob"))
	if !ok || mode != "text" || alice != bob {
		t.Fatalf("private admin leaked into group or shared key changed")
	}
	other, _, _ := wxThreadIdentity(policy, "turn/start", params("456@chatroom", "alice"))
	if other == alice {
		t.Fatal("two groups share the same thread identity")
	}
	if _, _, ok := wxThreadIdentity(policy, "turn/start", params("123@chatroom", "stranger")); ok {
		t.Fatal("stranger authorized")
	}
	if _, _, ok := wxThreadIdentity(policy, "turn/interrupt", params("123@chatroom", "bob")); ok {
		t.Fatal("non-admin can stop shared thread")
	}
	if _, _, ok := wxThreadIdentity(policy, "turn/interrupt", params("123@chatroom", "alice")); !ok {
		t.Fatal("configured admin cannot stop shared thread")
	}
	group.Session = "member"
	policy.Groups["123@chatroom"] = group
	alice, _, _ = wxThreadIdentity(policy, "turn/start", params("123@chatroom", "alice"))
	bob, _, _ = wxThreadIdentity(policy, "turn/start", params("123@chatroom", "bob"))
	if alice == bob {
		t.Fatal("per-member threads collide")
	}
	group.Revision = strings.Repeat("1", 32)
	policy.Groups["123@chatroom"] = group
	if _, _, ok := wxThreadIdentity(policy, "turn/start", params("123@chatroom", "alice")); ok {
		t.Fatal("outdated queue job authorized")
	}
}

func TestWxRecordWritesScopedMediaAndReusesData(t *testing.T) {
	agent := &Agent{cfg: Config{DataDir: t.TempDir()}}
	record := wxRecord{Source: strings.Repeat("a", 64), Entries: []wxRecordEntry{
		{Index: "1", Nickname: "发言人", Time: "2026-11-20", Text: "验收消息", Kind: 1},
		{Index: "2", Name: "../../avatar.png", Kind: 2, Status: "thumbnail", Media: []byte("\x89PNG\r\n\x1a\nimage")},
		{Index: "3", Name: "expired.wav", Kind: 3, Status: "unavailable"},
	}}
	raw, _ := json.Marshal(record)
	path, doc, image, err := agent.storeWxRecord("robotA", "groupA", record, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) || !strings.Contains(doc, "验收消息") || !strings.HasPrefix(image, "data:image/png;") {
		t.Fatal("record not available to both backends")
	}
	root := filepath.Dir(path)
	indexed, err := os.ReadFile(filepath.Join(root, "media.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(indexed), "media/002.png") || !strings.Contains(string(indexed), "unavailable") || !strings.Contains(string(indexed), "thumbnail") {
		t.Fatalf("bad index: %s", indexed)
	}
	file, err := os.ReadFile(filepath.Join(root, "media", "002.png"))
	if err != nil || string(file) != string(record.Entries[1].Media) {
		t.Fatal("stored media differs")
	}
	expired := record
	expired.Entries = append([]wxRecordEntry(nil), record.Entries...)
	expired.Entries[1].Media, expired.Entries[1].Status = nil, "unavailable"
	expiredRaw, _ := json.Marshal(expired)
	same, savedDoc, savedImage, err := agent.storeWxRecord("robotA", "groupA", expired, expiredRaw)
	if err != nil || same != path || savedDoc != doc || savedImage != image {
		t.Fatal("same forwarded content did not reuse path")
	}
	other, _, _, err := agent.storeWxRecord("robotB", "groupA", record, raw)
	if err != nil || other == path {
		t.Fatal("different robots share cache directory")
	}
	old := time.Now().Add(-48 * time.Hour)
	if err = os.Chtimes(root, old, old); err != nil {
		t.Fatal(err)
	}
	pin := agent.pinWxRecord(nil, "thread", root)
	usage, err := agent.WXRecordCache(true, 1)
	if err != nil || usage.Removed != 0 || usage.Count != 2 {
		t.Fatalf("active record was removed: %+v %v", usage, err)
	}
	agent.releaseWxRecordTurn("thread", "turn")
	agent.bindWxRecordTurn("thread", pin, json.RawMessage(`{"turn":{"id":"turn","status":"inProgress"}}`))
	usage, err = agent.WXRecordCache(true, 1)
	if err != nil || usage.Removed != 1 || usage.Count != 1 {
		t.Fatalf("unexpected retention result: %+v %v", usage, err)
	}
}

func TestWxRecordInjectsOnlyAllowedContext(t *testing.T) {
	agent := &Agent{cfg: Config{DataDir: t.TempDir()}}
	for _, mode := range []string{"text", "tools"} {
		input := []any{
			map[string]any{"type": "text", "text": "群历史"},
			map[string]any{"type": "text", "text": "提问"},
		}
		params := map[string]any{"input": input, "wxRecord": wxRecord{Entries: []wxRecordEntry{{Index: "1", Text: "only-from-record", Kind: 1}}}}
		if _, err := agent.applyWxRecord("robot", "actor", mode, params, true); err != nil {
			t.Fatal(err)
		}
		if _, ok := params["wxRecord"]; ok {
			t.Fatal("raw record escaped to app-server")
		}
		if got := input[0].(map[string]any)["text"]; got != "群历史" {
			t.Fatalf("group history changed: %v", got)
		}
		text := input[1].(map[string]any)["text"].(string)
		if mode == "text" && (!strings.Contains(text, "only-from-record") || strings.Contains(text, "transcript.md")) {
			t.Fatalf("text mode received wrong context: %s", text)
		}
		if mode == "tools" && (!strings.Contains(text, "transcript.md") || strings.Contains(text, "only-from-record")) {
			t.Fatalf("tools mode received wrong context: %s", text)
		}
	}
}
