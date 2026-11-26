package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
	"github.com/CyL-Cly/CodexRM-Agent/internal/installation"
	"github.com/CyL-Cly/CodexRM-Agent/internal/release"
	"github.com/CyL-Cly/CodexRM-Server/updater"
)

func TestMain(m *testing.M) {
	// The same test executable is used as the downloaded Agent fixture.
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"versionCode": release.VersionCode, "versionName": release.VersionName,
		})
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestManifestURLValidation(t *testing.T) {
	for _, valid := range []string{
		"https://updates.example/CodexRM-update.json",
		"http://127.0.0.1:8080/update.json",
		"https://updates.example/feed.json?key=public",
	} {
		if _, err := validateManifestURL(valid); err != nil {
			t.Errorf("rejected %q: %v", valid, err)
		}
	}
	for _, invalid := range []string{
		"", "updates/update.json", "file:///C:/update.json",
		"https://user:password@updates.example/update.json",
		"https://updates.example/update.json#fragment",
	} {
		if _, err := validateManifestURL(invalid); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
}

func TestParseOptionsRequiresExplicitManifest(t *testing.T) {
	t.Setenv("CODEXRM_UPDATE_MANIFEST", "")
	opts, err := parseOptions([]string{"--non-interactive", "--no-start"}, io.Discard)
	if err != nil || opts.manifestURL != "" || !opts.nonInteractive || !opts.noStart {
		t.Fatalf("unexpected options: %+v, %v", opts, err)
	}
	t.Setenv("CODEXRM_UPDATE_MANIFEST", " https://updates.example/update.json ")
	opts, err = parseOptions(nil, io.Discard)
	if err != nil || opts.manifestURL != "https://updates.example/update.json" {
		t.Fatalf("unexpected environment override: %+v, %v", opts, err)
	}
	opts, err = parseOptions([]string{"--manifest", "https://other.example/update.json"}, io.Discard)
	if err != nil || opts.manifestURL != "https://other.example/update.json" {
		t.Fatalf("unexpected flag override: %+v, %v", opts, err)
	}
}

func TestOnlineInstallFromManifest(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows installation integration test")
	}
	archive := setupArchive(t)
	manifest := manifestFor(archive)
	manifest.DownloadURL = "client.zip"
	feed := newFeed(t, manifest, archive)
	defer feed.Close()

	localAppData := t.TempDir()
	t.Setenv("LOCALAPPDATA", localAppData)
	t.Setenv("CODEXRM_INSTALL_TEST_ROOT", t.TempDir())
	installDir := filepath.Join(t.TempDir(), "installed app [test] 中文")
	workspace := t.TempDir()
	codexCLI := filepath.Join(t.TempDir(), "codex fixture.cmd")
	if err := os.WriteFile(codexCLI, []byte("@echo off\r\necho codex-cli test\r\nexit /b 0\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := install(context.Background(), options{
		manifestURL: feed.URL + "/base/update.json",
		installDir:  filepath.Join(localAppData, "CodexRM"), workspace: workspace, codexPath: codexCLI,
		noShortcuts: true, noAutostart: true, noStart: true,
	}, strings.NewReader(installDir+"\r\n"), &output, &output)
	if err != nil {
		t.Fatalf("online install: %v\n%s", err, output.String())
	}
	for _, relative := range []string{
		"bin/codexrm-agent.exe", ".state/installation.json",
	} {
		if info, err := os.Stat(filepath.Join(installDir, filepath.FromSlash(relative))); err != nil || info.IsDir() {
			t.Errorf("installed file %s: %v", relative, err)
		}
	}
	configData, err := os.ReadFile(filepath.Join(installDir, ".state", "connection.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Server         string `json:"server"`
		Workspace      string `json:"workspace"`
		UpdateManifest string `json:"updateManifestUrl"`
		UpdateServer   string `json:"updateServer"`
		AutoUpdate     bool   `json:"autoUpdate"`
		EnrollmentKey  any    `json:"enrollmentKey"`
	}
	configData = bytes.TrimPrefix(configData, []byte{0xef, 0xbb, 0xbf})
	if err := json.Unmarshal(configData, &config); err != nil {
		t.Fatal(err)
	}
	if config.Server != clientconfig.DefaultRelayServer || config.EnrollmentKey != nil || config.Workspace != workspace ||
		config.UpdateManifest != "" || config.UpdateServer != "" || !config.AutoUpdate {
		t.Fatalf("unexpected installed configuration: %+v", config)
	}
	if _, err := os.Stat(filepath.Join(localAppData, "CodexRM", "installation.json")); err != nil {
		t.Fatalf("installation record: %v", err)
	}
	stages, _ := filepath.Glob(filepath.Join(installDir, ".state", "update-*"))
	if len(stages) != 0 {
		t.Fatalf("installer left staged payloads: %v", stages)
	}
	if !strings.Contains(output.String(), "SHA-256") || !strings.Contains(output.String(), manifest.VersionName) ||
		!strings.Contains(output.String(), "安装目录") {
		t.Fatalf("missing installation details in output:\n%s", output.String())
	}

	if err := installation.Uninstall(context.Background(), installDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(installDir); !os.IsNotExist(err) {
		t.Fatalf("installation directory was not removed: %v", err)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("external workspace was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(localAppData, "CodexRM", "installation.json")); !os.IsNotExist(err) {
		t.Fatalf("installation record was not removed: %v", err)
	}
}

func TestOnlineInstallRejectsWrongHashWithoutTouchingConfiguration(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows installation integration test")
	}
	archive := []byte("downloaded package")
	manifest := manifestFor(archive)
	manifest.SHA256 = strings.Repeat("0", 64)
	feed := newFeed(t, manifest, archive)
	defer feed.Close()
	installDir := t.TempDir()
	configPath := filepath.Join(installDir, ".state", "connection.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("local connection and credentials")
	if err := os.WriteFile(configPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := install(context.Background(), options{
		manifestURL: feed.URL + "/base/update.json", installDir: installDir,
		nonInteractive: true, noShortcuts: true, noAutostart: true, noStart: true,
	}, strings.NewReader(""), &output, &output)
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("wrong hash result: %v\n%s", err, output.String())
	}
	current, readErr := os.ReadFile(configPath)
	if readErr != nil || !bytes.Equal(current, original) {
		t.Fatalf("local configuration changed: %q, %v", current, readErr)
	}
}

func manifestFor(data []byte) updater.Manifest {
	hash := sha256.Sum256(data)
	return updater.Manifest{
		VersionCode: release.VersionCode, VersionName: release.VersionName,
		DownloadURL: "client.zip", ReleaseNotes: "online installer test",
		FileSize: int64(len(data)), SHA256: hex.EncodeToString(hash[:]),
	}
}

func newFeed(t *testing.T, manifest updater.Manifest, archive []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/base/update.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Enrollment-Key") != "" {
			t.Error("installer leaked connection credentials to the update source")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manifest)
	})
	mux.HandleFunc("/base/client.zip", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Enrollment-Key") != "" {
			t.Error("installer leaked connection credentials to the package source")
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
		_, _ = w.Write(archive)
	})
	return httptest.NewServer(mux)
}

func setupArchive(t *testing.T) []byte {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	files := []string{
		"bin/codexrm-agent.exe",
	}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for _, relative := range files {
		source := self
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		entry, err := archive.Create("CodexRM/" + relative)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = entry.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if buffer.Len() == 0 {
		t.Fatal(fmt.Errorf("empty setup archive"))
	}
	return buffer.Bytes()
}
