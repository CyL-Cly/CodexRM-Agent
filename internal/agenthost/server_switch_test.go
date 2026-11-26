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
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/CyL-Cly/CodexRM-Server/server"
)

func TestServerSwitchPreservesExistingIdentityAndLocalRobotRelay(t *testing.T) {
	startRelay := func(key string) string {
		t.Helper()
		s, err := server.New(server.Config{DataDir: t.TempDir(), AdminToken: key})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		remote := httptest.NewServer(s.Handler())
		t.Cleanup(remote.Close)
		return remote.URL
	}
	local, remote := startRelay(""), startRelay("remote-key")
	base := agent.Config{Mock: true, Cwd: t.TempDir(), DataDir: t.TempDir(), ServerURL: local}
	legacy, err := agent.New(base)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	original, err := legacy.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	legacyState := filepath.Join(base.DataDir, "agent.json")
	originalState, err := os.ReadFile(legacyState)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(filepath.Join(t.TempDir(), "connection.json"), "", clientconfig.Connection{Server: local}, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.SetWXLocalRelay(local); err == nil {
		t.Fatal("loopback is not accessible to Docker robots")
	}
	// Use a loopback test relay after exercising the public option's validation.
	h.wxLocalServer = local
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Run(ctx) }()
	defer func() { cancel(); <-done }()
	var address string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		address, err = h.SettingsURL()
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	page := "http://" + u.Host
	token := testSettingsToken(t, h, page, u.Fragment)
	await := func(relay string) {
		t.Helper()
		for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); {
			state := h.snapshot()
			if state["server"] == relay && state["status"] == "已连接" {
				localAgent, _, done, err := h.currentWX(ctx)
				if err == nil {
					connected := localAgent.Connected()
					done()
					if connected {
						return
					}
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("relay did not connect: %+v", h.snapshot())
	}
	switchTo := func(relay, key string) {
		t.Helper()
		if code, body := request(t, page, token, http.MethodPut, "/api/settings", map[string]string{"server": relay, "enrollmentKey": key}); code != http.StatusOK {
			t.Fatalf("save: %d %s", code, body)
		}
		await(relay)
	}
	claim := func(relay, code string) protocol.PairClaimResponse {
		t.Helper()
		status, body := request(t, relay, "", http.MethodPost, "/api/v1/client/claim", map[string]string{"code": code, "clientName": "test client", "deviceType": "desktop"})
		var out protocol.PairClaimResponse
		if status != http.StatusOK || json.Unmarshal(body, &out) != nil || out.EnvironmentID == "" {
			t.Fatalf("claim on %s: %d", relay, status)
		}
		return out
	}
	pairMain := func(relay string) string {
		t.Helper()
		status, body := request(t, page, token, http.MethodPost, "/api/pair", nil)
		var pair struct{ Code string }
		if status != http.StatusOK || json.Unmarshal(body, &pair) != nil || pair.Code == "" {
			t.Fatalf("pair main: %d", status)
		}
		return claim(relay, pair.Code).EnvironmentID
	}
	await(local)
	switchTo(remote, "remote-key")
	remoteEnv := pairMain(remote)
	if remoteEnv == original.EnvironmentID {
		t.Fatal("main connection reused the local relay identity")
	}
	robotAgent, pairCtx, release, err := h.currentWX(ctx)
	if err != nil {
		t.Fatal(err)
	}
	robot, err := robotAgent.Pair(pairCtx)
	release()
	if err != nil {
		t.Fatal(err)
	}
	if got := claim(local, robot.PairingCode).EnvironmentID; got != original.EnvironmentID {
		t.Fatal("robot connection lost the existing local identity")
	}
	switchTo(local, "")
	if got := pairMain(local); got != original.EnvironmentID {
		t.Fatal("switching back lost the existing local identity")
	}
	switchTo(remote, "remote-key")
	if got := pairMain(remote); got != remoteEnv {
		t.Fatal("switching back lost the remote identity")
	}
	state, err := os.ReadFile(legacyState)
	if err != nil || string(state) != string(originalState) {
		t.Fatal("switching relays changed the original credentials")
	}
}
