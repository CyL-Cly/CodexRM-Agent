package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/CyL-Cly/CodexRM-Server/release"
	"github.com/CyL-Cly/CodexRM-Server/server"
	"github.com/CyL-Cly/CodexRM-Server/updater"
)

func TestClientUpdateRoutes(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "updates")
	_ = os.MkdirAll(directory, 0700)
	s, err := server.New(server.Config{DataDir: root, AdminToken: "private-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	response, err := http.Get(h.URL + "/update.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 404 || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("unpublished update did not return an uncached 404")
	}
	response, err = http.Get(h.URL + "/android-update.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 404 || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("unpublished Android update did not return an uncached 404")
	}
	data := []byte("windows ZIP bytes")
	hash := sha256.Sum256(data)
	m := updater.Manifest{VersionCode: 2, VersionName: "0.6.1", DownloadURL: "updates/client.zip", ReleaseNotes: "更新", FileSize: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
	manifest, _ := json.Marshal(m)
	_ = os.WriteFile(filepath.Join(directory, "update.json"), append([]byte{0xef, 0xbb, 0xbf}, manifest...), 0600)
	_ = os.WriteFile(filepath.Join(directory, "client.zip"), data, 0600)
	response, err = http.Get(h.URL + "/update.json")
	if err != nil {
		t.Fatal(err)
	}
	var actual updater.Manifest
	err = json.NewDecoder(response.Body).Decode(&actual)
	_ = response.Body.Close()
	if err != nil || actual != m || response.StatusCode != 200 || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("published manifest: %+v, %v", actual, err)
	}
	response, err = http.Get(h.URL + "/updates/client.zip")
	if err != nil {
		t.Fatal(err)
	}
	downloaded, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 || string(downloaded) != string(data) || response.Header.Get("Content-Type") != "application/zip" {
		t.Fatal("public package download failed")
	}
	for _, path := range []string{"/updates/update.json", "/updates/android-update.json", "/updates/..%5cstate.json", "/updates/missing.zip", "/updates/client.zip/extra"} {
		response, err = http.Get(h.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != 404 {
			t.Fatalf("unexpected package path was served: %s", path)
		}
	}
	androidData := []byte("Android APK bytes")
	androidHash := sha256.Sum256(androidData)
	androidManifest := updater.Manifest{VersionCode: 4, VersionName: "1.0.3", DownloadURL: "updates/mobile.apk", ReleaseNotes: "Android 更新", FileSize: int64(len(androidData)), SHA256: hex.EncodeToString(androidHash[:])}
	androidJSON, _ := json.Marshal(androidManifest)
	_ = os.WriteFile(filepath.Join(directory, "android-update.json"), androidJSON, 0600)
	_ = os.WriteFile(filepath.Join(directory, "mobile.apk"), androidData, 0600)
	response, err = http.Get(h.URL + "/android-update.json")
	if err != nil {
		t.Fatal(err)
	}
	var actualAndroid updater.Manifest
	err = json.NewDecoder(response.Body).Decode(&actualAndroid)
	_ = response.Body.Close()
	if err != nil || actualAndroid != androidManifest || response.StatusCode != 200 || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("published Android manifest: %+v, %v", actualAndroid, err)
	}
	response, err = http.Get(h.URL + "/updates/mobile.apk")
	if err != nil {
		t.Fatal(err)
	}
	downloaded, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 || string(downloaded) != string(androidData) || response.Header.Get("Content-Type") != "application/vnd.android.package-archive" {
		t.Fatal("public APK download failed")
	}
	_ = os.WriteFile(filepath.Join(directory, "update.json"), []byte("invalid JSON"), 0600)
	response, err = http.Get(h.URL + "/update.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 500 {
		t.Fatal("invalid publication was accepted")
	}
}

func TestLinuxAgentUpdateManifest(t *testing.T) {
	directory := t.TempDir()
	s, err := server.New(server.Config{DataDir: t.TempDir(), UpdateDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	handler := s.Handler()

	linux := updater.Manifest{VersionCode: 16, VersionName: "0.6.15", DownloadURL: "https://storage.example/linux-amd64.zip", FileSize: 100, SHA256: strings.Repeat("a", 64)}
	windows := linux
	windows.DownloadURL = "https://storage.example/windows-amd64.zip"
	for _, check := range []struct {
		name string
		want updater.Manifest
	}{
		{"update.json", windows},
		{"linux-update.json", linux},
	} {
		path := "/" + check.name
		missing := httptest.NewRecorder()
		handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, path, nil))
		if missing.Code != http.StatusNotFound || missing.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unpublished %s returned %d with cache policy %q", path, missing.Code, missing.Header().Get("Cache-Control"))
		}

		body, err := json.Marshal(check.want)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, check.name), body, 0600); err != nil {
			t.Fatal(err)
		}
		published := httptest.NewRecorder()
		handler.ServeHTTP(published, httptest.NewRequest(http.MethodGet, path, nil))
		var actual updater.Manifest
		err = json.NewDecoder(published.Body).Decode(&actual)
		if err != nil || actual != check.want || published.Code != http.StatusOK || published.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("published %s returned %d and %+v: %v", path, published.Code, actual, err)
		}
	}
}

func TestCustomUpdateDirectory(t *testing.T) {
	directory := t.TempDir()
	s, err := server.New(server.Config{DataDir: t.TempDir(), UpdateDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = os.WriteFile(filepath.Join(directory, "client.zip"), []byte("custom release"), 0600)
	r := httptest.NewRecorder()
	s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/updates/client.zip", nil))
	if r.Code != 200 || !strings.Contains(r.Body.String(), "custom release") {
		t.Fatal("custom update directory was ignored")
	}
}

func TestServerVersionAndUpdateRoute(t *testing.T) {
	root := t.TempDir()
	clientDir := filepath.Join(root, "updates")
	serverDir := filepath.Join(root, "server-updates")
	for _, dir := range []string{clientDir, serverDir} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	m := updater.Manifest{VersionCode: 2, VersionName: "0.1.1", DownloadURL: "https://releases.example/server.zip", FileSize: 123, SHA256: strings.Repeat("a", 64)}
	manifest, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	// Never fall back to a Server manifest supplied from a client-writable directory.
	if err = os.WriteFile(filepath.Join(clientDir, release.ServerManifestName), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := server.New(server.Config{DataDir: root, UpdateDir: clientDir, ServerUpdateDir: serverDir, AdminToken: "private-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	handler := s.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		return res
	}
	path := "/" + release.ServerManifestName
	res := get(path)
	if res.Code != http.StatusNotFound || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unpublished Server manifest: %d %s", res.Code, res.Body)
	}
	if err = os.WriteFile(filepath.Join(serverDir, release.ServerManifestName), manifest, 0644); err != nil {
		t.Fatal(err)
	}
	res = get(path)
	actual, err := updater.ReadManifest(res.Body)
	if err != nil || actual != m || res.Code != http.StatusOK || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("published Server manifest: %v, %v, status %d", actual, err, res.Code)
	}
	res = get("/api/v1/health")
	var health release.ServerHealth
	if err = json.Unmarshal(res.Body.Bytes(), &health); err != nil || !health.OK || health.Service != "codexrm" || health.ProtocolVersion != protocol.Version || health.VersionCode != release.ServerVersionCode || health.VersionName != release.ServerVersionName {
		t.Fatalf("running Server health: %v, %v", health, err)
	}
}
