package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Agent/internal/providers"
)

func providerFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TABLE providers (id TEXT, app_type TEXT, name TEXT, settings_config TEXT, sort_index INTEGER, notes TEXT)"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		raw := mustJSON(map[string]any{"auth": map[string]any{"OPENAI_API_KEY": fmt.Sprintf("secret-%d", i)}, "config": fmt.Sprintf("model = 'same-model'\nmodel_provider = 'custom'\n[model_providers.custom]\nname = 'Same'\nbase_url = 'https://example.test/v1'\nwire_api = 'responses'\n")})
		if _, err = db.Exec("INSERT INTO providers VALUES (?, 'codex', 'Same', ?, ?, NULL)", fmt.Sprint(i), string(raw), i); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestThreadProviderIsolationAndResume(t *testing.T) {
	ctx := context.Background()
	path := providerFixture(t)
	items, err := providers.Load(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	mock, err := appserver.StartMock("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	app, err := appserver.Dial(ctx, "ws://"+mock.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	a := &Agent{cfg: Config{CCSwitchDB: path, DataDir: t.TempDir()}}
	for i, provider := range items {
		params := map[string]any{"cwd": t.TempDir(), "codexrmProvider": provider.ID, "config": map[string]any{"model_reasoning_effort": "low"}}
		if err := a.prepareProvider(ctx, app, "thread/start", params); err != nil {
			t.Fatal(err)
		}
		config := params["config"].(map[string]any)
		connection := config["model_providers."+provider.ID].(map[string]any)
		if params["codexrmProvider"] != nil || params["modelProvider"] != provider.ID || connection["experimental_bearer_token"] != fmt.Sprintf("secret-%d", i+1) || config["model_reasoning_effort"] != "low" {
			t.Fatal("thread uses incorrect provider or loses other config overrides")
		}
		raw, err := app.Call(ctx, "thread/start", params)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		// Simulate a fresh agent: the rollout provider ID must restore the correct key.
		b := &Agent{cfg: a.cfg}
		resume := map[string]any{"threadId": result.Thread.ID}
		if err := b.prepareProvider(ctx, app, "thread/resume", resume); err != nil {
			t.Fatal(err)
		}
		restored := resume["config"].(map[string]any)["model_providers."+provider.ID].(map[string]any)
		if restored["experimental_bearer_token"] != connection["experimental_bearer_token"] || resume["modelProvider"] != provider.ID {
			t.Fatal("resume changed provider credentials")
		}
		if resume["model"] != nil || resume["config"].(map[string]any)["model_reasoning_effort"] != nil {
			t.Fatal("provider preparation overrode Codex thread settings")
		}
	}
	if err := a.prepareProvider(ctx, app, "thread/start", map[string]any{"codexrmProvider": "ccswitch_removed"}); err == nil {
		t.Fatal("missing provider silently fell back to another supplier")
	}
}

func TestOpenThreadWithRemovedProviderDoesNotResumeIt(t *testing.T) {
	a, app, events := queueTestAgent(t)
	a.cfg = Config{DataDir: t.TempDir(), CCSwitchDB: filepath.Join(t.TempDir(), "absent.db")}
	started := lifecycleCall(t, a, app, "thread/start", map[string]any{"modelProvider": "ccswitch_removed"})
	id := started["thread"].(map[string]any)["id"].(string)
	turn := lifecycleCall(t, a, app, "turn/start", queueInput(id, "approval wait"))["turn"].(map[string]any)
	queueEvent(t, events, "item/commandExecution/requestApproval")
	lifecycleCall(t, a, app, "turn/interrupt", map[string]any{"threadId": id, "turnId": turn["id"]})
	waitQueue(t, a, id, func(q *messageQueue) bool { return q.Detached && !q.Loaded })

	result := lifecycleCall(t, a, app, "thread/resume", map[string]any{"threadId": id})
	thread := result["thread"].(map[string]any)
	if thread["id"] != id || thread["modelProvider"] != "ccswitch_removed" || thread["codexrmProviderUnavailable"] != true {
		t.Fatalf("unexpected history-only thread: %v", thread)
	}
	if turns := thread["turns"].([]any); len(turns) != 1 || turns[0].(map[string]any)["id"] != turn["id"] {
		t.Fatalf("original turn missing: %v", turns)
	}
	metadata := lifecycleCall(t, a, app, "thread/resume", map[string]any{"threadId": id, "excludeTurns": true})["thread"].(map[string]any)
	if metadata["codexrmProviderUnavailable"] != true || metadata["turns"] != nil {
		t.Fatalf("metadata-only resume did not honor excludeTurns: %v", metadata)
	}
	if data := queueCall(t, app, "thread/loaded/list", nil)["data"].([]any); len(data) != 0 {
		t.Fatalf("opening without a provider loaded a thread: %v", data)
	}
	for _, method := range []string{"turn/start", "turn/steer"} {
		if _, err := a.callThread(a.queueCtx, app, "owner", method, queueInput(id, "must not send")); err != errProviderUnavailable {
			t.Errorf("%s was not blocked: %v", method, err)
		}
	}
	if _, err := a.queueRPC(a.queueCtx, app, "owner", "codexrm/queue/add", queueInput(id, "must not queue")); err != errProviderUnavailable {
		t.Fatalf("queue add was not blocked: %v", err)
	}
}
