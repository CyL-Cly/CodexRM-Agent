package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceDiffReadsScopedGitChanges(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("Git is required for this test")
	}
	path := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = path
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(path, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "--quiet")
	write("example.txt", "before\n")
	run("add", "example.txt")
	run("-c", "user.email=qa@example.invalid", "-c", "user.name=CodexRM QA", "commit", "--quiet", "-m", "fixture")
	write("example.txt", "after\n")
	write("new.txt", "untracked\n")
	a := &Agent{cfg: Config{Cwd: path, DataDir: t.TempDir()}}
	ctx := context.Background()
	result, err := a.workspaceDiff(ctx, nil, map[string]any{"workspace": path})
	if err != nil {
		t.Fatal(err)
	}
	if patch := result["diff"].(string); !strings.Contains(patch, "+after") || !strings.Contains(patch, "-before") {
		t.Fatalf("unexpected patch: %q", patch)
	}
	if files := result["untracked"].([]string); len(files) != 1 || files[0] != "new.txt" {
		t.Fatalf("unexpected untracked files: %v", files)
	}
	if _, err := a.workspaceDiff(ctx, nil, map[string]any{"workspace": t.TempDir()}); err == nil {
		t.Fatal("accepted a path outside configured workspaces")
	}
	write("example.txt", strings.Repeat("long line\n", workspaceDiffLimit/8))
	if _, err := a.workspaceDiff(ctx, nil, map[string]any{"workspace": path}); err == nil {
		t.Fatal("accepted a patch over the size limit")
	}
}
