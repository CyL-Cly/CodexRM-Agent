package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/pelletier/go-toml/v2"
)

// Run against the real app-server with a throwaway CODEX_HOME; no inference or
// real credentials are needed to exercise the config protocol.
func TestNativeProviderCapabilitiesWithLocalCodex(t *testing.T) {
	executable := os.Getenv("CODEXRM_TEST_CODEX")
	if executable == "" {
		t.Skip("set CODEXRM_TEST_CODEX to test the local Codex app-server")
	}
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("RUST_LOG", "error")
	path := filepath.Join(home, "config.toml")
	const config = `# keep my comment
model_provider = 'custom'
[model_providers.custom]
name = 'Test Provider'
base_url = 'https://unused.example/v1'
wire_api = 'responses'
[model_providers.custom.capabilities]
external_web_access = false
[model_providers.other]
name = 'Other'
supports_websockets = true
`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	app, err := appserver.Launch(ctx, executable, home)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if _, err = app.Initialize(ctx, "codexrm-test", "0.0.0"); err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: Config{DataDir: t.TempDir()}}
	call := func(params map[string]any) (nativeProviderCapabilities, error) {
		t.Helper()
		result, err := a.providerSettingsRPC(ctx, app, "codexrm/provider/nativeCapabilities", mustJSON(params), false)
		return result.(nativeProviderCapabilities), err
	}
	read := func(enabled bool) nativeProviderCapabilities {
		t.Helper()
		details, err := call(map[string]any{})
		if err != nil || !details.Editable || details.GPTOfficial != enabled || details.Version == "" {
			t.Fatalf("unexpected details: %+v, %v", details, err)
		}
		return details
	}
	initial := read(false)
	if _, err = call(map[string]any{"gptOfficial": true, "version": "stale"}); err == nil {
		t.Fatal("accepted stale version")
	}
	for _, step := range []struct {
		enabled bool
		mode    string
	}{{true, "v2"}, {false, "unsupported"}} {
		if _, err = call(map[string]any{"gptOfficial": step.enabled, "version": initial.Version}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(data), "# keep my comment") {
			t.Fatal("config comment was lost")
		}
		var cfg struct {
			Provider  string                    `toml:"model_provider"`
			Providers map[string]map[string]any `toml:"model_providers"`
		}
		if err := toml.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		custom := cfg.Providers["custom"]
		caps, _ := custom["capabilities"].(map[string]any)
		if cfg.Provider != "custom" || cfg.Providers["other"]["supports_websockets"] != true || custom["base_url"] != "https://unused.example/v1" ||
			custom["supports_websockets"] != step.enabled || caps["remote_compaction"] != step.mode || caps["external_web_access"] != false {
			t.Fatalf("unexpected provider config after save: %+v", cfg)
		}
		if _, err = call(map[string]any{"gptOfficial": !step.enabled, "version": initial.Version}); err == nil {
			t.Fatal("accepted version from before previous write")
		}
		initial = read(step.enabled)
	}
}
