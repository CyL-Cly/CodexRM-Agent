package agenthost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestWXComposeTemplatePinnedAndLoopback(t *testing.T) {
	images := map[string]string{}
	for _, key := range wxImageKeys {
		images[key] = "mirror.example/wxbot/" + key + "@sha256:" + strings.Repeat("a", 64)
	}
	output := wxRenderCompose(images, []string{"word-cloud", "jimeng", "mcp"})
	for _, key := range wxImageKeys {
		if key != "backup" && !strings.Contains(output, images[key]) {
			t.Fatalf("Compose does not use %s", key)
		}
	}
	if strings.Contains(output, "@@") || strings.Contains(output, ":latest") || !strings.Contains(output, "127.0.0.1:8443:443") {
		t.Fatal("unexpected unpinned or public Compose config")
	}
}

func TestWXComposeOptionalServicesAreOptIn(t *testing.T) {
	for _, selected := range [][]string{nil, {"word-cloud"}, {"jimeng", "mcp"}} {
		output := wxRenderCompose(nil, selected)
		actual := wxExistingOptionalServices(output)
		if !slices.Equal(actual, selected) {
			t.Fatalf("existing Compose optional services = %v, want %v", actual, selected)
		}
		for _, service := range wxEnabledComposeServices(selected) {
			if !strings.Contains(output, "\n  "+service.name+":\n") {
				t.Fatalf("image pull list contains %q, which is absent from Compose", service.name)
			}
		}
	}
	for _, invalid := range [][]string{{"mysql"}, {"mcp", "mcp"}, {"unknown"}} {
		if validateWXOptionalServices(invalid) == nil {
			t.Fatalf("invalid optional services %v accepted", invalid)
		}
	}
}

func TestWXAdminListUsesOnlyAccountSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/robot/list" || r.Header.Get("Cookie") != "session=test" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":200,"data":{"total":1,"items":[{"id":1,"robot_name":"test","db_password":"secret","proxy":{"password":"secret"}}]}}`))
	}))
	defer srv.Close()
	client := &wxAdminClient{base: srv.URL, cookies: []*http.Cookie{{Name: "session", Value: "test"}}}
	rows, err := client.robots(context.Background())
	if err != nil || len(rows) != 1 || rows[0].ID != 1 {
		t.Fatalf("invalid list: %+v %v", rows, err)
	}
	raw, _ := json.Marshal(rows)
	if strings.Contains(string(raw), "secret") {
		t.Fatal("admin credentials exposed")
	}
}

func TestWXSigningKeyChecksPrimaryFingerprint(t *testing.T) {
	fingerprint := "fpr:::::::::9DC858229FC7DD38854AE2D88D81803C0EBFCD88:"
	if !wxDockerSigningKey("pub::::::::::\n" + fingerprint) {
		t.Fatal("known key rejected")
	}
	if wxDockerSigningKey("pub::::::::::\nsub::::::::::\n"+fingerprint) || wxDockerSigningKey("pub::::::::::\n"+fingerprint+"\npub::::::::::") {
		t.Fatal("unexpected key accepted")
	}
}
