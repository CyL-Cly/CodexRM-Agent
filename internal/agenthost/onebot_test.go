package agenthost

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
)

func TestOneBotSettingsAPIPreservesAndClearsToken(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("OneBot settings are exposed by the Linux Agent")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".state", "connection.json")
	h, err := New(path, "", clientconfig.Connection{}, agent.Config{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	put := func(body string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/api/onebot", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.oneBotSettings(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT returned %d: %s", rec.Code, rec.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	base := `{"enabled":false,"mode":"ws-reverse","listen":"127.0.0.1:6701","messageLimit":1800,"allowRemoteMedia":true,"allowPrivateMedia":false,"allowLocalMedia":true,"users":{"10001":{"enabled":true,"mode":"text"}},"groups":{}`
	result := put(strings.Replace(base, `"groups":{}`, `"groups":{},"accessToken":"secret"}`, 1))
	if result["accessTokenSet"] != true {
		t.Fatalf("token was not saved: %#v", result)
	}
	result = put(base + `}`)
	if result["accessTokenSet"] != true {
		t.Fatal("blank token did not preserve the saved token")
	}
	result = put(strings.Replace(base, `"groups":{}`, `"groups":{},"clearAccessToken":true}`, 1))
	if result["accessTokenSet"] != false {
		t.Fatal("clear token did not remove the saved token")
	}
	raw, err := agent.LoadOneBotSettings(h.oneBotPath())
	if err != nil || raw.AccessToken != "" || !raw.Users["10001"].Enabled {
		t.Fatalf("saved settings mismatch: %#v, %v", raw, err)
	}
	encoded, _ := json.Marshal(result)
	if bytes.Contains(encoded, []byte("secret")) {
		t.Fatal("API response exposed the access token")
	}
}

func TestOneBotSnapshotAllowsRepairingMalformedConfiguration(t *testing.T) {
	dir := t.TempDir()
	h, err := New(filepath.Join(dir, "connection.json"), "", clientconfig.Connection{}, agent.Config{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	h.base.OneBotSettingsPath = filepath.Join(dir, "onebot.json")
	if err := os.WriteFile(h.base.OneBotSettingsPath, []byte(`{"enabled":true,"typo":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := h.oneBotSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot["loadError"] == "" || snapshot["settings"].(agent.OneBotSettings).Mode != "ws-reverse" {
		t.Fatalf("repair snapshot mismatch: %#v", snapshot)
	}
}

func TestOneBotSettingsAPIRejectsUnknownFields(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("OneBot settings are exposed by the Linux Agent")
	}
	dir := t.TempDir()
	h, err := New(filepath.Join(dir, "connection.json"), "", clientconfig.Connection{}, agent.Config{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/onebot", strings.NewReader(`{"enabled":false,"unexpected":true}`))
	rec := httptest.NewRecorder()
	h.oneBotSettings(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field returned %d: %s", rec.Code, rec.Body.String())
	}
}
