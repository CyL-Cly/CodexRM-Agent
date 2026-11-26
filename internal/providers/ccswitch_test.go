package providers

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func database(t *testing.T, entries ...[]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "供应商.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TABLE providers (id TEXT, app_type TEXT, name TEXT, settings_config TEXT, sort_index INTEGER, notes TEXT)"); err != nil {
		t.Fatal(err)
	}
	for i, entry := range entries {
		var notes any
		if len(entry) > 4 {
			notes = entry[4]
		}
		if _, err = db.Exec("INSERT INTO providers VALUES (?, ?, ?, ?, ?, ?)", entry[0], entry[1], entry[2], entry[3], i, notes); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func settings(base, key, model string) string {
	raw, _ := json.Marshal(map[string]any{
		"auth":   map[string]any{"OPENAI_API_KEY": key},
		"config": fmt.Sprintf("model = %q\nmodel_provider = 'custom'\n[model_providers.custom]\nname = 'shared'\nbase_url = %q\nrequires_openai_auth = true\nwire_api = 'responses'\n", model, base),
		"modelCatalog": map[string]any{"models": []any{
			map[string]any{"model": model, "reasoningLevels": []string{"low", "max"}},
			map[string]any{"model": model},
		}},
	})
	return string(raw)
}

func TestCCSwitchReadOnlyAndCredentialIsolation(t *testing.T) {
	path := database(t,
		[]string{"a", "codex", "Same", settings("https://one.example/v1", "secret-one", "shared-model"), "  备用账号  "},
		[]string{"b", "codex", "Same", settings("https://two.example/v1", "secret-two", "other-model")},
		[]string{"claude", "claude", "Ignore", `{}`},
		[]string{"broken", "codex", "Broken", `{"config":"secret-do-not-expose"}`},
	)
	before, _ := os.ReadFile(path)
	items, err := Load(context.Background(), path)
	if err != nil || len(items) != 3 {
		t.Fatalf("load: %v, count=%d", err, len(items))
	}
	if items[0].ID == items[1].ID || items[0].Name != "Same" || items[1].Name != "Same" {
		t.Fatal("same-named providers must retain their title and distinct IDs")
	}
	if items[0].Notes != "备用账号" || items[1].Notes != "" {
		t.Fatal("provider notes were not read correctly")
	}
	if len(items[0].Models) != 1 || len(items[0].Models[0].SupportedReasoningEfforts) != 2 {
		t.Fatal("duplicate models or reasoning metadata lost")
	}
	for i, key := range []string{"secret-one", "secret-two"} {
		cfg := items[i].Overrides()["model_providers."+items[i].ID].(map[string]any)
		if cfg["experimental_bearer_token"] != key || cfg["requires_openai_auth"] != false {
			t.Fatal("wrong provider credentials")
		}
	}
	public, _ := json.Marshal(items)
	if strings.Contains(string(public), "secret-") || strings.Contains(string(public), "base_url") || strings.Contains(string(public), "experimental_bearer_token") {
		t.Fatal("connection settings leaked into public metadata")
	}
	if items[2].Warning == "" {
		t.Fatal("invalid provider was not surfaced")
	}
	after, _ := os.ReadFile(path)
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("CCSwitch database changed")
	}
	again, err := Load(context.Background(), path)
	if err != nil || again[0].ID != items[0].ID {
		t.Fatal("provider IDs are not stable")
	}
}

func TestCurrentProviderUsesConnectionSettings(t *testing.T) {
	path := database(t,
		[]string{"a", "codex", "Same", settings("https://example.test/v1", "one", "shared")},
		[]string{"b", "codex", "Same", settings("https://example.test/v1", "two", "shared")},
		[]string{"c", "codex", "Same", settings("https://example.test/v1", "two", "alternate")},
	)
	items, err := Load(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	codexDir := t.TempDir()
	t.Setenv("CODEX_HOME", codexDir)
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(codexDir, "config.toml"), `model = 'different-live-model'
model_provider = 'custom'
[model_providers.custom]
name = 'shared'
base_url = 'https://example.test/v1'
wire_api = 'responses'
requires_openai_auth = true
`)
	auth := filepath.Join(codexDir, "auth.json")
	write(auth, `{"OPENAI_API_KEY":"one"}`)
	if id := CurrentProviderID(items, path); id != items[0].ID {
		t.Fatal("matching connection lost when using a different default model")
	}
	write(auth, `{"OPENAI_API_KEY":"unrelated"}`)
	if CurrentProviderID(items, path) != "" {
		t.Fatal("merged an unrelated connection with a saved provider")
	}
	write(auth, `{"OPENAI_API_KEY":"two"}`)
	if CurrentProviderID(items, path) != "" {
		t.Fatal("merged an ambiguous match without an active CC Switch selection")
	}
	write(filepath.Join(filepath.Dir(path), "settings.json"), `{"currentProviderCodex":"c"}`)
	if id := CurrentProviderID(items, path); id != items[2].ID {
		t.Fatal("did not prefer the active CC Switch entry among matching connections")
	}
}

func TestModelDiscoveryUsesEachProviderTokenAndKeepsSavedModels(t *testing.T) {
	var mu sync.Mutex
	requests := map[string]int{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		mu.Lock()
		requests[key]++
		mu.Unlock()
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if key == "Bearer first" {
			fmt.Fprint(w, `{"data":[{"id":"saved"},{"id":"api-only"},{"id":"api-only"}]}`)
		} else {
			w.WriteHeader(500)
			fmt.Fprint(w, "private-upstream-error-second")
		}
	}))
	defer s.Close()
	a, _ := parse("a", "A", settings(s.URL, "first", "saved"))
	b, _ := parse("b", "B", settings(s.URL, "second", "fallback"))
	items := []Provider{a, b}
	Discover(context.Background(), items)
	if len(items[0].Models) != 2 || items[0].Models[1].Model != "api-only" || items[0].Warning != "" {
		t.Fatal("API models not merged/deduplicated")
	}
	if len(items[1].Models) != 1 || items[1].Warning == "" || strings.Contains(items[1].Warning, "private-upstream") {
		t.Fatal("fallback lost or upstream response leaked")
	}
	if requests["Bearer first"] != 2 || requests["Bearer second"] != 2 {
		t.Fatalf("wrong per-provider discovery authentication: %v", requests)
	}
}

func TestDiscoveryDoesNotForwardTokenAcrossRedirect(t *testing.T) {
	var received bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received = true }))
	defer target.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer s.Close()
	p, _ := parse("a", "A", settings(s.URL+"/v1", "secret", "saved"))
	items := []Provider{p}
	Discover(context.Background(), items)
	if received || items[0].Warning == "" {
		t.Fatal("discovery followed a credential-bearing redirect")
	}
}

func TestMissingCCSwitchIsOptional(t *testing.T) {
	items, err := Load(context.Background(), filepath.Join(t.TempDir(), "absent.db"))
	if err != nil || len(items) != 0 {
		t.Fatal("missing optional configuration should not break Codex")
	}
}

func TestOfficialCatalogDoesNotReuseCustomModels(t *testing.T) {
	path := database(t, []string{"official", "codex", "Official", `{"config":"","auth":{}}`})
	cache := `{"older":{"fetched_at":1,"models":[{"slug":"outdated"}]},"newer":{"fetched_at":2,"models":[{"slug":"official-model","display_name":"Official model","default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"medium","description":"Medium"}]}]}}`
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "codex-official-models.json"), []byte(cache), 0600); err != nil {
		t.Fatal(err)
	}
	items, err := Load(context.Background(), path)
	if err != nil || len(items) != 1 || len(items[0].Models) != 1 || items[0].Models[0].Model != "official-model" || items[0].Models[0].SupportedReasoningEfforts[0].ReasoningEffort != "medium" {
		t.Fatalf("official cache: %v, %#v", err, items)
	}
	if items[0].RuntimeID() != "openai" || len(items[0].Overrides()) != 0 {
		t.Fatal("official login should use the native OpenAI provider")
	}
}

func TestOfficialCatalogIncludesCacheAlongsideSavedDefault(t *testing.T) {
	raw, err := json.Marshal(map[string]any{"config": "model = 'saved-default'\nmodel_provider = 'openai'\n"})
	if err != nil {
		t.Fatal(err)
	}
	path := database(t, []string{"official", "codex", "Official", string(raw)})
	cache := `{"official":{"fetched_at":1,"models":[{"slug":"saved-default","display_name":"Saved default","default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"medium","description":"Medium"}]},{"slug":"another-official-model"}]}}`
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "codex-official-models.json"), []byte(cache), 0600); err != nil {
		t.Fatal(err)
	}
	items, err := Load(context.Background(), path)
	if err != nil || len(items) != 1 || len(items[0].Models) != 2 {
		t.Fatalf("official model cache was hidden by the saved default: %v, %#v", err, items)
	}
	model := items[0].Models[0]
	if !model.IsDefault || model.DefaultReasoningEffort != "medium" || len(model.SupportedReasoningEfforts) != 1 {
		t.Fatalf("default model lost its cached metadata: %#v", model)
	}
}
