package agenthost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Agent/internal/desktop"
)

func TestOpenPhoneTransfersKeepsSettingsToken(t *testing.T) {
	var opened string
	h := &Host{ctx: context.Background(), page: settingsPage{server: &http.Server{}, url: "http://127.0.0.1:9123/#settings-token"}, openBrowser: func(s string) error {
		opened = s
		return nil
	}}
	h.OpenPhoneTransfers()
	u, err := url.Parse(opened)
	if err != nil || u.Query().Get("view") != "phone-transfers" || u.Fragment != "settings-token" {
		t.Fatalf("tray target: %q, %v", opened, err)
	}
}

func TestPhoneTransfersSettingsIsLocalAndAuthenticated(t *testing.T) {
	if !desktop.ReceivesFiles {
		t.Skip("Windows-only settings")
	}
	ctx := context.Background()
	a, err := agent.New(agent.Config{ServerURL: "http://127.0.0.1:1", DataDir: t.TempDir(), Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "example.txt")
	if err := os.WriteFile(path, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.QueuePhoneFile("some-phone", path, "测试手机"); err != nil {
		t.Fatal(err)
	}
	h := &Host{ctx: ctx, active: a, activeCtx: ctx}
	mux := http.NewServeMux()
	h.registerPhoneTransferRoutes(mux)
	const localHost = "127.0.0.1:8301"
	guard := guardSettings(mux, localHost, "secret", false, false)
	request := func(method, path, token, peer, origin string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://"+localHost+path, nil)
		req.RemoteAddr = peer
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp := httptest.NewRecorder()
		guard.ServeHTTP(resp, req)
		return resp
	}
	const root = "/api/phone-transfers"
	const local = "127.0.0.1:11223"
	const remote = "192.0.2.10:12345"
	if got := request(http.MethodGet, root, "", local, "").Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: %d", got)
	}
	resp := request(http.MethodGet, root, "secret", local, "")
	var list struct {
		Outbox []agent.PendingPhoneTransfer
		Inbox  []agent.ReceivedPhoneFile
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &list); resp.Code != http.StatusOK || err != nil || len(list.Outbox) != 1 || list.Inbox == nil || list.Outbox[0].Recipient != "测试手机" {
		t.Fatalf("local list: %d %s", resp.Code, resp.Body.String())
	}
	entry := list.Outbox[0]
	cancelPath := root + "/outbox/" + entry.Bucket + "/" + entry.ID
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, root}, {http.MethodDelete, cancelPath}, {http.MethodPost, root + "/inbox/" + entry.ID + "/open"},
	} {
		if got := request(tc.method, tc.path, "secret", remote, "").Code; got != http.StatusForbidden {
			t.Fatalf("LAN %s %s: %d", tc.method, tc.path, got)
		}
	}
	if got := request(http.MethodDelete, cancelPath, "secret", local, "https://example.invalid").Code; got != http.StatusForbidden {
		t.Fatalf("foreign origin cancellation: %d", got)
	}
	if resp := request(http.MethodPost, root+"/inbox/"+entry.ID+"/open", "secret", local, ""); resp.Code != http.StatusBadRequest {
		t.Fatalf("opened a file without a matching receive record: %d %s", resp.Code, resp.Body.String())
	}
	if got := request(http.MethodDelete, cancelPath, "secret", local, "").Code; got != http.StatusOK {
		t.Fatalf("local cancellation without relay: %d", got)
	}
	if got := request(http.MethodDelete, cancelPath, "secret", local, "").Code; got != http.StatusConflict {
		t.Fatalf("already finished/canceled: %d", got)
	}
}
