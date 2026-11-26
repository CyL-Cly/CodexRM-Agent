package appserver

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestResolveCodexExecutableFromDesktopInstall(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", filepath.Join(root, "empty-path"))
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "local"))
	t.Setenv("ProgramFiles", filepath.Join(root, "program-files"))
	t.Setenv("ProgramW6432", "")
	t.Setenv("ProgramFiles(x86)", "")
	t.Setenv("SystemDrive", "")
	want := filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Codex", "resources", "codex.exe")
	if err := os.MkdirAll(filepath.Dir(want), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("cli"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := resolveCodexExecutableFrom("", codexDesktopCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolved %q, want %q", got, want)
	}
}

func TestResolveCodexExecutableFromStorePackage(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", filepath.Join(root, "empty-path"))
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "local"))
	programFiles := filepath.Join(root, "program-files")
	t.Setenv("ProgramFiles", programFiles)
	t.Setenv("ProgramW6432", "")
	t.Setenv("ProgramFiles(x86)", "")
	t.Setenv("SystemDrive", "")
	want := filepath.Join(programFiles, "WindowsApps", "OpenAI.Codex_1.0_x64__test", "app", "resources", "codex.exe")
	if err := os.MkdirAll(filepath.Dir(want), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("cli"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := resolveCodexExecutableFrom("codex", func() []string {
		return codexDesktopPackageCandidates([]string{filepath.Join(programFiles, "WindowsApps")})
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolved %q, want %q", got, want)
	}
}

func TestResolveCodexExecutablePreservesExplicitPath(t *testing.T) {
	want := filepath.Join(t.TempDir(), "custom-codex.exe")
	got, err := resolveCodexExecutableFrom(want, func() []string {
		t.Fatal("desktop discovery must not override an explicit executable")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolved %q, want %q", got, want)
	}
}

func TestResolveCodexExecutablePrefersDesktopBeforePATH(t *testing.T) {
	root := t.TempDir()
	pathCLI := filepath.Join(root, "codex")
	if runtime.GOOS == "windows" {
		pathCLI += ".exe"
		t.Setenv("PATHEXT", ".EXE")
	}
	if err := os.WriteFile(pathCLI, []byte("cli on PATH"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	desktopCLI := filepath.Join(root, "resources", "codex.exe")
	if err := os.MkdirAll(filepath.Dir(desktopCLI), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(desktopCLI, []byte("desktop cli"), 0700); err != nil {
		t.Fatal(err)
	}
	names := []string{"", "codex"}
	if runtime.GOOS == "windows" {
		names = append(names, "codex.exe", "Codex.EXE")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			got, err := resolveCodexExecutableFrom(name, func() []string { return []string{desktopCLI} })
			if err != nil || got != desktopCLI {
				t.Fatalf("resolved %q, %v; want desktop CLI %q", got, err, desktopCLI)
			}
		})
	}
	got, err := resolveCodexExecutableFrom("codex", func() []string { return []string{root, desktopCLI + ".missing"} })
	if err != nil || got != pathCLI {
		t.Fatalf("resolved %q, %v; want PATH fallback %q", got, err, pathCLI)
	}
}

func TestResolveCodexExecutableFromOtherStoreVolume(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"ProgramW6432", "ProgramFiles", "ProgramFiles(x86)", "SystemDrive"} {
		t.Setenv(name, "")
	}
	t.Setenv("PATH", filepath.Join(root, "empty-path"))
	oldCLI := filepath.Join(root, "WindowsApps", "OpenAI.Codex_1.0_x64__test", "app", "resources", "codex.exe")
	want := filepath.Join(root, "WindowsApps", "OpenAI.Codex_2.0_x64__test", "resources", "codex.exe")
	for i, path := range []string{oldCLI, want} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("cli"), 0700); err != nil {
			t.Fatal(err)
		}
		modified := time.Unix(int64(i+1), 0)
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	got, err := resolveCodexExecutableFrom("", func() []string {
		return codexDesktopPackageCandidates(codexDesktopPackageRoots([]string{root}))
	})
	if err != nil || got != want {
		t.Fatalf("resolved %q, %v; want latest desktop CLI %q", got, err, want)
	}
}

func TestResolveCodexExecutableReportsMissingDesktop(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got, err := resolveCodexExecutableFrom("")
	if got != "" || err == nil || !strings.Contains(err.Error(), "Codex desktop") || !strings.Contains(err.Error(), "--codex") {
		t.Fatalf("resolved %q, %v; want desktop installation guidance", got, err)
	}
}
