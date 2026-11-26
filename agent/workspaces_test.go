package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWorkspacesPersistAndConcurrentUpdates(t *testing.T) {
	root := t.TempDir()
	data := t.TempDir()
	a := &Agent{cfg: Config{Cwd: root, DataDir: data}}
	var wg sync.WaitGroup
	for _, name := range []string{"project-a", "项目乙", "project-c"} {
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3; i++ {
				if _, err := a.workspaceRPC("codexrm/workspace/add", map[string]any{"path": path}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	// A fresh Agent must recover the same device's list without browser storage.
	b := &Agent{cfg: a.cfg}
	result, err := b.workspaceRPC("codexrm/workspace/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	list := result.(map[string]any)["workspaces"].([]workspace)
	if len(list) != 4 {
		t.Fatalf("lost concurrent adds or kept duplicates: %#v", list)
	}
	path := filepath.Join(root, "项目乙")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := b.workspaceRPC("codexrm/workspace/remove", map[string]any{"path": path}); err != nil {
		t.Fatalf("cannot remove stale directory from list: %v", err)
	}
	// Removing metadata for a live project must not remove its files.
	path = filepath.Join(root, "project-a")
	if _, err := b.workspaceRPC("codexrm/workspace/remove", map[string]any{"path": path}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("project directory was removed: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(data, "workspaces.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &list); err != nil || len(list) != 2 {
		t.Fatalf("invalid persisted list: %s, %v", raw, err)
	}
}

func TestDirectoryBrowsingAndInvalidPaths(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"project-a", ".hidden", "项目乙"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(root, "not-a-directory.txt")
	if err := os.WriteFile(file, []byte("keep this file"), 0600); err != nil {
		t.Fatal(err)
	}
	listing, err := listDirectories(root, false)
	if err != nil || len(listing.Directories) != 2 || listing.Parent != filepath.Dir(root) {
		t.Fatalf("invalid listing: %#v, %v", listing, err)
	}
	for _, entry := range listing.Directories {
		if !filepath.IsAbs(entry.Path) {
			t.Fatalf("browser cannot navigate relative result: %#v", entry)
		}
	}
	listing, err = listDirectories(root, true)
	if err != nil || len(listing.Directories) != 3 {
		t.Fatalf("hidden directory omitted: %#v, %v", listing, err)
	}
	a := &Agent{cfg: Config{Cwd: root, DataDir: t.TempDir()}}
	for _, path := range []string{"relative-path", file, filepath.Join(root, "missing")} {
		if _, err := a.workspaceRPC("codexrm/workspace/add", map[string]any{"path": path}); err == nil {
			t.Fatalf("accepted invalid directory %q", path)
		}
	}
	// Removing every saved workspace remains empty after reconnecting.
	if _, err := a.workspaceRPC("codexrm/workspace/remove", map[string]any{"path": root}); err != nil {
		t.Fatal(err)
	}
	result, err := a.workspaceRPC("codexrm/workspace/list", nil)
	if err != nil || len(result.(map[string]any)["workspaces"].([]workspace)) != 0 {
		t.Fatalf("deleted default was re-added: %#v, %v", result, err)
	}
}
