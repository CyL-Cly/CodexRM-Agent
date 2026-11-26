package appserver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveCodexFromDesktopProcesses(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "app", "resources", "codex.exe")
	if err := os.MkdirAll(filepath.Dir(want), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("bundled cli"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, processPath := range []string{
		filepath.Join(root, "app", "ChatGPT.exe"),
		filepath.Join(root, "app", "Codex.exe"),
		filepath.Join(root, "Codex.exe"),
		want,
	} {
		t.Run(filepath.Base(filepath.Dir(processPath))+"/"+filepath.Base(processPath), func(t *testing.T) {
			got, err := resolveCodexExecutableFrom("", func() []string { return codexDesktopProcessCLIPaths(processPath) })
			if err != nil || got != want {
				t.Fatalf("resolved %q, %v; want bundled CLI %q", got, err, want)
			}
		})
	}
	if got := codexDesktopProcessCLIPaths(filepath.Join(root, "other.exe")); len(got) != 0 {
		t.Fatalf("unrelated process produced Codex candidates: %v", got)
	}
}
