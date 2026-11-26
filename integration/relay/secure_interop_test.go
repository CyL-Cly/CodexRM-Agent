package server_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Server/securechannel"
	"github.com/CyL-Cly/CodexRM-Server/server"
)

func TestBrowserNoiseInteroperatesWithAgentThroughHTTP(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node 22+ is required for Web Crypto and WebSocket")
	}
	if b, err := exec.Command(node, "-p", `typeof WebSocket === "function" && !!crypto.subtle`).Output(); err != nil || strings.TrimSpace(string(b)) != "true" {
		t.Skip("Node runtime does not provide Web Crypto and WebSocket")
	}
	stateDir := t.TempDir()
	s, err := server.New(server.Config{DataDir: stateDir, RequireSecureClients: true})
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	handler := s.Handler()
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/client/route" || r.URL.Path == "/api/v1/agent/pair" {
			b, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(b))
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { s.Close(); h.Close() })
	cfg := agent.Config{Cwd: t.TempDir(), DataDir: t.TempDir(), ServerURL: h.URL, SecureRelay: true, Mock: true, NoAutoPair: true}
	issuer, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	invite, err := issuer.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The CLI --pair process may exit before the running Agent reads the code.
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- a.Run(ctx) }()
	defer func() { cancel(); <-finished }()
	for !a.Connected() {
		select {
		case <-ctx.Done():
			t.Fatal("Agent did not connect")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cmd := exec.CommandContext(ctx, node, "--input-type=module", "-", h.URL)
	cmd.Dir = serverWebDir(t)
	cmd.Stdin = strings.NewReader(strings.ReplaceAll(browserNoiseTest, "TEST_INVITATION", invite.PairingCode))
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("browser/Agent secure relay interop: %v\n%s", err, b)
	}
	route, _, err := securechannel.ParsePairingCode(invite.PairingCode)
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Split(invite.PairingCode, ".")[2]
	if !bytes.Contains(state, []byte(route)) || bytes.Contains(state, []byte(secret)) {
		t.Fatal("unexpected pairing state on the relay")
	}
	for _, body := range bodies {
		if strings.Contains(body, secret) {
			t.Fatal("invitation secret crossed the HTTP relay")
		}
	}
}

const browserNoiseTest = `
import assert from 'node:assert/strict';
import { parsePairingCode, verifyPairing, connectSecure } from './secure-relay.js';
const url = process.argv[2];
const endpoint = url.replace('http', 'ws') + '/api/v1/client/ws';
const invitation = parsePairingCode('TEST_INVITATION');
const request = path => fetch(url + path, {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({route: invitation.route})});
const response = await request('/api/v2/client/route');
assert.equal(response.status, 200);
const result = await response.json();
const key = await verifyPairing(endpoint, result, invitation);
assert.notEqual(key, invitation.secret);
assert.equal((await request('/api/v2/client/route')).status, 409);
assert.equal((await request('/api/v1/client/claim')).status, 426);
let sent = 0;
const rpc = () => new Promise((resolve, reject) => {
  const socket = new WebSocket(endpoint, ['codexrm', 'codexrm.token.' + result.token]);
  const timer = setTimeout(() => done(new Error('Secure RPC timeout')), 5000);
  function done(error) { clearTimeout(timer); socket.close(); error ? reject(error) : resolve(); }
  const secure = connectSecure(socket, result.clientId, key, {
    onReady() {
      assert.equal(secure.send({type: 'client_message', message: {id: ++sent, method: 'codexrm/info'}}), true);
    },
    onMessage(packet) {
      try { assert.equal(packet.message.id, sent); assert.equal(packet.message.result.mock, true); done(); }
      catch (error) { done(error); }
    },
    onFailure: done,
  });
  socket.onmessage = event => secure.consume(JSON.parse(event.data));
});
await rpc();
await rpc();
`
