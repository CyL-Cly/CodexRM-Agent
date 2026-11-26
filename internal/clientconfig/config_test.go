package clientconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultRelayRequiresMissingSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connection.json")
	fresh, err := Load(path)
	if err != nil || fresh.Server != "https://codexrm.ooth.tech" || fresh.EnrollmentKey != "" || !fresh.AutoUpdate {
		t.Fatalf("unexpected defaults: %+v, %v", fresh, err)
	}
	if err := Save(path, Connection{}); err != nil {
		t.Fatal(err)
	}
	saved, err := Load(path)
	if err != nil || saved.Server != "" || saved.AutoUpdate {
		t.Fatalf("existing empty settings changed: %+v, %v", saved, err)
	}
}

func TestLegacyUpdateFieldsDoNotConfigureUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connection.json")
	legacy := `{"server":"https://relay.example","enrollmentKey":"saved-key","autoUpdate":true,"updateServer":"not a URL","updateManifestUrl":"https://old.example/custom.json"}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "https://relay.example" || cfg.EnrollmentKey != "saved-key" {
		t.Fatal("migration changed connection settings")
	}
	if err = Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil || bytes.Contains(saved, []byte("updateServer")) || bytes.Contains(saved, []byte("updateManifestUrl")) {
		t.Fatalf("legacy update fields were not removed: %s, %v", saved, err)
	}
}

func TestUpdateFeedRequiresExplicitHTTPSConfiguration(t *testing.T) {
	t.Setenv("CODEXRM_AGENT_UPDATE_SERVER", "")
	if base, manifest, err := UpdateFeed(false); err != nil || base != "" || manifest != "" {
		t.Fatalf("unconfigured feed: %q %q %v", base, manifest, err)
	}
	t.Setenv("CODEXRM_AGENT_UPDATE_SERVER", " https://updates.example/base/ ")
	for _, linux := range []bool{false, true} {
		name := "update.json"
		if linux {
			name = "linux-update.json"
		}
		base, manifest, err := UpdateFeed(linux)
		if err != nil || base != "https://updates.example/base" || manifest != base+"/"+name {
			t.Errorf("wrong feed for Linux=%t: %q %q %v", linux, base, manifest, err)
		}
	}
	for _, invalid := range []string{"http://updates.example", "https://@updates.example", "https://updates.example?query", "https://updates.example#fragment"} {
		t.Setenv("CODEXRM_AGENT_UPDATE_SERVER", invalid)
		if _, _, err := UpdateFeed(false); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
}
