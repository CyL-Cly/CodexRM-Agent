package providers

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCCSwitchProviderEditorPersistsOnlyRequestedFields(t *testing.T) {
	ctx := context.Background()
	path := database(t,
		[]string{"shared", "codex", "Before", settings("https://before.example/v1", "before-key", "original-model")},
		[]string{"shared", "claude", "Other app", `{}`},
	)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, ddl := range []string{
		"ALTER TABLE providers ADD COLUMN category TEXT",
		"ALTER TABLE providers ADD COLUMN created_at INTEGER",
		"ALTER TABLE providers ADD COLUMN is_current BOOLEAN NOT NULL DEFAULT 0",
		"UPDATE providers SET is_current = 1 WHERE app_type = 'codex'",
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	id := providerID("shared")
	name, notes, base := "After", "Edited on phone", "https://after.example/v1"
	if _, err := SaveProvider(ctx, path, ProviderPatch{ID: id, Name: &name, Notes: &notes, BaseURL: &base}); err != nil {
		t.Fatal(err)
	}
	details, err := EditDetails(ctx, path, id)
	if err != nil || details.Name != name || details.Notes != notes || details.BaseURL != base {
		t.Fatalf("wrong editable details: %+v, %v", details, err)
	}
	public, _ := json.Marshal(details)
	if strings.Contains(string(public), "before-key") {
		t.Fatal("details exposed the provider key")
	}
	assertConfig := func(expectedKey string) {
		t.Helper()
		items, err := Load(ctx, path)
		if err != nil || len(items) != 1 {
			t.Fatalf("updated provider missing: %v", err)
		}
		config := items[0].Overrides()["model_providers."+id].(map[string]any)
		if config["experimental_bearer_token"] != expectedKey || config["base_url"] != base {
			t.Fatal("wrong connection settings")
		}
		if items[0].DefaultModel != "original-model" || len(items[0].Models) != 1 || len(items[0].Models[0].SupportedReasoningEfforts) != 2 {
			t.Fatal("original model catalog metadata lost")
		}
	}
	assertConfig("before-key")
	if _, err := SaveProvider(ctx, path, ProviderPatch{ID: id, APIKey: "new-key"}); err != nil {
		t.Fatal(err)
	}
	assertConfig("new-key")
	newName := "New Provider"
	newID, err := SaveProvider(ctx, path, ProviderPatch{Name: &newName, BaseURL: &base, APIKey: "create-key"})
	if err != nil || newID == id {
		t.Fatalf("create failed: %v", err)
	}
	var otherName string
	var current int
	if err := db.QueryRow("SELECT name FROM providers WHERE app_type = 'claude'").Scan(&otherName); err != nil || otherName != "Other app" {
		t.Fatal("edited another app's provider")
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM providers WHERE app_type = 'codex' AND is_current = 1 AND id = 'shared'").Scan(&current); err != nil || current != 1 {
		t.Fatal("edited current provider state")
	}
	items, err := Load(ctx, path)
	if err != nil || len(items) != 2 || items[1].ID != newID || items[1].Official() {
		t.Fatalf("new provider not visible: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := SaveProvider(ctx, missing, ProviderPatch{Name: &newName, BaseURL: &base}); err == nil {
		t.Fatal("created an uninitialized database")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("missing database was unexpectedly created")
	}
}
