package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Agent/internal/providers"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

// Opt in with a local Codex CLI. All credentials, endpoints and state are test-only.
func TestProviderRoutingWithLocalCodex(t *testing.T) {
	executable := os.Getenv("CODEXRM_TEST_CODEX")
	if executable == "" {
		t.Skip("set CODEXRM_TEST_CODEX to test the local Codex app-server")
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("RUST_LOG", "error")
	type request struct {
		Path, Token, Model string
		Input              json.RawMessage
	}
	requests := make(chan request, 20)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Model string
			Input json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid test request", 400)
			return
		}
		requests <- request{r.URL.Path, r.Header.Get("Authorization"), body.Model, body.Input}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-test\"}}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-test\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
	}))
	defer upstream.Close()
	config := fmt.Sprintf("model = 'same-model'\nmodel_provider = 'baseline'\n[model_providers.baseline]\nname = 'Baseline'\nbase_url = %q\nwire_api = 'responses'\nrequires_openai_auth = false\n", upstream.URL+"/baseline")
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	path := providerFixture(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	update := func(id, token string) {
		t.Helper()
		config := fmt.Sprintf("model = 'same-model'\nmodel_provider = 'custom'\n[model_providers.custom]\nname = 'Same'\nbase_url = %q\nwire_api = 'responses'\nrequires_openai_auth = true\n", upstream.URL+"/"+id)
		raw := mustJSON(map[string]any{"config": config, "auth": map[string]any{"OPENAI_API_KEY": token}})
		if _, err := db.Exec("UPDATE providers SET settings_config = ? WHERE id = ?", string(raw), id); err != nil {
			t.Fatal(err)
		}
	}
	update("1", "test-key-one")
	update("2", "test-key-two")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	items, err := providers.Load(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	type completion struct {
		ThreadID string                  `json:"threadId"`
		Turn     struct{ Status string } `json:"turn"`
	}
	completed := make(chan completion, 20)
	cwd := t.TempDir()
	launch := func(agent *Agent) *appserver.Client {
		t.Helper()
		app, err := appserver.Launch(ctx, executable, cwd)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = app.Close() })
		app.OnNotification(func(raw json.RawMessage) {
			agent.queueNotification(ctx, app, raw)
			var event struct {
				Method string     `json:"method"`
				Params completion `json:"params"`
			}
			if json.Unmarshal(raw, &event) == nil && event.Method == "turn/completed" {
				completed <- event.Params
			}
		})
		if _, err := app.Initialize(ctx, "codexrm-provider-test", "0.0.0"); err != nil {
			t.Fatal(err)
		}
		return app
	}
	a := &Agent{cfg: Config{CCSwitchDB: path, DataDir: t.TempDir()}, owners: map[string]string{}}
	app := launch(a)
	threads := []string{}
	for _, provider := range items {
		params := map[string]any{"cwd": cwd, "codexrmProvider": provider.ID, "approvalPolicy": "never", "sandbox": "read-only"}
		if err := a.prepareProvider(ctx, app, "thread/start", params); err != nil {
			t.Fatal(err)
		}
		raw, err := app.Call(ctx, "thread/start", params)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Thread struct{ ID string } `json:"thread"`
		}
		if err := json.Unmarshal(raw, &result); err != nil || result.Thread.ID == "" {
			t.Fatalf("start did not return a thread: %v", err)
		}
		threads = append(threads, result.Thread.ID)
	}
	turnWithModel := func(threadID, endpoint, token, model string, override bool) {
		t.Helper()
		text := "Return immediately. Do not use tools."
		if !override {
			text = "Continue this conversation."
		}
		params := queueInput(threadID, text)
		params["effort"] = "low"
		if override {
			params["model"] = model
		} else {
			delete(params, "model")
		}
		_, err := a.callThread(ctx, app, "owner", "turn/start", params)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-requests:
			if got.Path != endpoint+"/responses" || got.Token != "Bearer "+token || got.Model != model {
				t.Fatalf("wrong supplier request: path=%q matchingToken=%t model=%q", got.Path, got.Token == "Bearer "+token, got.Model)
			}
			if !override && !strings.Contains(string(got.Input), "Return immediately. Do not use tools.") {
				t.Fatal("the switched provider did not receive previous conversation context")
			}
		case <-ctx.Done():
			t.Fatal("Codex did not reach the local supplier")
		}
		select {
		case result := <-completed:
			if result.ThreadID != threadID || result.Turn.Status != "completed" {
				t.Fatalf("turn did not complete: %#v", result)
			}
		case <-ctx.Done():
			t.Fatal("Codex did not complete the test turn")
		}
	}
	turn := func(threadID, endpoint, token string) {
		t.Helper()
		turnWithModel(threadID, endpoint, token, "same-model", true)
	}
	turn(threads[0], "/1", "test-key-one")
	turn(threads[1], "/2", "test-key-two")
	turn(threads[0], "/1", "test-key-one")
	_ = app.Close()
	update("1", "rotated-test-key-one")
	a = &Agent{cfg: a.cfg, owners: map[string]string{}}
	app = launch(a)
	for i := range items {
		params := map[string]any{"threadId": threads[i]}
		if err := a.prepareProvider(ctx, app, "thread/resume", params); err != nil {
			t.Fatal(err)
		}
		if params["modelProvider"] != items[i].RuntimeID() || params["model"] != nil {
			t.Fatal("cold resume must restore the supplier and let Codex choose the model")
		}
		params["excludeTurns"] = true
		result := queueCall(t, app, "thread/resume", params)
		if result["model"] != "same-model" {
			t.Fatalf("Codex did not restore the thread model: %v", result["model"])
		}
	}
	turn(threads[0], "/1", "rotated-test-key-one")
	turn(threads[1], "/2", "test-key-two")
	params := map[string]any{"threadId": threads[0], "providerId": items[1].ID, "model": "another-model"}
	// An older provider-only route must still load and can be changed in place.
	if err := a.saveProviderRoute(threads[0], threadProviderRoute{ProviderID: items[0].ID}); err != nil {
		t.Fatal(err)
	}
	switchFromClient := func(params map[string]any) map[string]any {
		t.Helper()
		var reply struct {
			Result map[string]any      `json:"result"`
			Error  *appserver.RPCError `json:"error"`
		}
		a.handle(ctx, app, func(env protocol.Envelope) error {
			return json.Unmarshal(env.Message, &reply)
		}, protocol.Envelope{Type: protocol.TypeClientMessage, ClientID: "owner", Message: mustJSON(map[string]any{
			"id": "switch-model", "method": switchProviderMethod, "params": params,
		})}, nil)
		if reply.Error != nil || reply.Result == nil {
			t.Fatalf("client could not switch providers: %v", reply.Error)
		}
		return reply.Result
	}
	// A client may select a different account serving the exact same model.
	params["model"] = "same-model"
	sameModel := switchFromClient(params)
	if sameModel["threadId"] != threads[0] || sameModel["providerId"] != items[1].ID {
		t.Fatalf("same-model switch returned the wrong chat or supplier: %v", sameModel)
	}
	turnWithModel(threads[0], "/2", "test-key-two", "same-model", false)
	params["model"] = "another-model"
	changed := switchFromClient(params)
	if changed["threadId"] != threads[0] || changed["model"] != "another-model" {
		t.Fatalf("switch returned the wrong thread or model: %v", changed)
	}
	a.queueMu.Lock()
	detached := a.queues[threads[0]].Detached
	a.queueMu.Unlock()
	if !detached {
		t.Fatal("the old connection is still loaded after the switch")
	}
	turnWithModel(threads[0], "/2", "test-key-two", "another-model", false)

	_ = app.Close()
	if _, err := db.Exec("DELETE FROM providers WHERE id = '1'"); err != nil {
		t.Fatal(err)
	}
	a = &Agent{cfg: a.cfg, owners: map[string]string{}}
	app = launch(a)
	result := lifecycleCall(t, a, app, "thread/resume", map[string]any{"threadId": threads[0], "excludeTurns": true})
	thread := result["thread"].(map[string]any)
	if thread["id"] != threads[0] || thread["modelProvider"] != items[0].RuntimeID() || thread["codexrmProvider"] != items[1].ID {
		t.Fatalf("the restart changed the thread identity or lost the endpoint: %v", thread)
	}
	if result["model"] != "another-model" {
		t.Fatalf("the restart lost the selected model: %v", result["model"])
	}
	turnWithModel(threads[0], "/2", "test-key-two", "another-model", false)
	// The switch remains in effect after a turn and a second restart.
	_ = app.Close()
	a = &Agent{cfg: a.cfg, owners: map[string]string{}}
	app = launch(a)
	result = lifecycleCall(t, a, app, "thread/resume", map[string]any{"threadId": threads[0], "excludeTurns": true})
	if result["model"] != "another-model" {
		t.Fatalf("Codex did not restore the model used by the previous turn: %v", result["model"])
	}
	page := queueCall(t, app, "thread/turns/list", map[string]any{"threadId": threads[0], "limit": 10})
	if len(page["data"].([]any)) != 6 {
		t.Fatalf("switching endpoints lost conversation history: %v", page)
	}

	// CCSwitch also updates the active Codex profile. A session created with
	// that profile follows the new URL when its existing runtime ID is reused.
	_ = app.Close()
	writeActive := func(endpoint, token string) {
		t.Helper()
		config := fmt.Sprintf("model = 'same-model'\nmodel_provider = 'custom'\n[model_providers.custom]\nname = 'Same'\nbase_url = %q\nwire_api = 'responses'\nrequires_openai_auth = false\nexperimental_bearer_token = %q\n", upstream.URL+endpoint, token)
		if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeActive("/1", "rotated-test-key-one")
	a = &Agent{cfg: a.cfg, owners: map[string]string{}}
	app = launch(a)
	active := lifecycleCall(t, a, app, "thread/start", map[string]any{"cwd": cwd, "sandbox": "read-only", "approvalPolicy": "never"})
	activeID := active["thread"].(map[string]any)["id"].(string)
	turn(activeID, "/1", "rotated-test-key-one")
	a.releaseIdleThread(ctx, app, activeID)
	writeActive("/2", "test-key-two")
	turn(activeID, "/2", "test-key-two")

	if _, err := db.Exec("DELETE FROM providers WHERE id = '2'"); err != nil {
		t.Fatal(err)
	}
	if err := a.prepareProvider(ctx, app, "thread/resume", map[string]any{"threadId": threads[0]}); err == nil {
		t.Fatal("missing supplier silently fell back on resume")
	}

	// Opening without the original provider must only read history, even after
	// an Agent restart, and must let the user switch to a different endpoint.
	opened := lifecycleCall(t, a, app, "thread/resume", map[string]any{"threadId": threads[0]})
	thread = opened["thread"].(map[string]any)
	if thread["id"] != threads[0] || thread["codexrmProviderUnavailable"] != true || len(thread["turns"].([]any)) == 0 {
		t.Fatalf("missing supplier prevented reading the same thread: %v", thread)
	}
	if _, err := a.callThread(ctx, app, "owner", "turn/start", queueInput(threads[0], "must not send")); err != errProviderUnavailable {
		t.Fatalf("sending without a supplier was not explicitly blocked: %v", err)
	}
	if _, err := db.Exec("INSERT INTO providers (id, app_type, name, settings_config) VALUES ('1', 'codex', 'Same', '{}')"); err != nil {
		t.Fatal(err)
	}
	update("1", "rotated-test-key-one")
	switchFromClient(map[string]any{"threadId": threads[0], "providerId": items[0].ID, "model": "another-model"})
	turnWithModel(threads[0], "/1", "rotated-test-key-one", "another-model", false)

	// Migrate the selected provider, then remove the database from the Agent's
	// configuration. A fresh Agent must still restore and route the old session.
	if _, err := db.Exec("INSERT INTO providers (id, app_type, name, settings_config) VALUES ('2', 'codex', 'Same', '{}')"); err != nil {
		t.Fatal(err)
	}
	update("2", "test-key-two")
	switchFromClient(map[string]any{"threadId": threads[0], "providerId": items[1].ID, "model": "another-model"})
	if _, err := db.Exec("DELETE FROM providers WHERE id = '1'"); err != nil {
		t.Fatal(err)
	}
	if count, err := a.providerStore().ImportCCSwitch(ctx, path); err != nil || count != 1 {
		t.Fatalf("import count=%d, err=%v", count, err)
	}
	_ = app.Close()
	cfg := a.cfg
	cfg.CCSwitchDB = filepath.Join(t.TempDir(), "absent.db")
	a = &Agent{cfg: cfg, owners: map[string]string{}}
	app = launch(a)
	resumed := lifecycleCall(t, a, app, "thread/resume", map[string]any{"threadId": threads[0], "excludeTurns": true})
	if resumed["model"] != "another-model" {
		t.Fatalf("migration lost the selected model: %v", resumed["model"])
	}
	turnWithModel(threads[0], "/2", "test-key-two", "another-model", false)

	// A provider created directly in Agent settings also works without any CC
	// Switch records. Let its default model supply the thread and turn model.
	created, err := a.LocalProviderSettings(ctx, "save", mustJSON(map[string]string{
		"name": "Agent provider", "baseUrl": upstream.URL + "/local", "apiKey": "agent-local-test-key", "defaultModel": "local-model",
	}))
	if err != nil {
		t.Fatal(err)
	}
	localID := created.(map[string]string)["id"]
	started := lifecycleCall(t, a, app, "thread/start", map[string]any{
		"cwd": cwd, "codexrmProvider": localID, "approvalPolicy": "never", "sandbox": "read-only",
	})
	localThread := started["thread"].(map[string]any)["id"].(string)
	// Use the explicitly selected model on the first prompt (the test helper
	// checks for prior history on subsequent prompts with no model override).
	if started["model"] != "local-model" {
		t.Fatalf("local default model was not used: %v", started["model"])
	}
	turnWithModel(localThread, "/local", "agent-local-test-key", "local-model", true)
}
