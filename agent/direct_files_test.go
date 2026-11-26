package agent

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

func TestWorkspaceFileDownloadBoundaries(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	private := filepath.Join(root, "agent-state")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "agent.json"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "产物.apk")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	const largeOffset = 21 << 20
	if _, err := f.WriteAt([]byte("end"), largeOffset); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: Config{Cwd: root, DataDir: private, AllowFileManagement: true}}
	d := &directSession{}
	t.Cleanup(d.files.close)
	ctx := context.Background()
	call := func(d *directSession, method string, p map[string]any) (any, error) {
		return a.directFileRPC(ctx, nil, d, protocol.DirectFilePrefix+method, p)
	}
	params := func(path string) map[string]any { return map[string]any{"workspace": root, "path": path} }
	if _, err := call(nil, "open", params(file)); err == nil {
		t.Fatal("relay allowed a file read")
	}
	for _, path := range []string{"../outside", outside, "agent-state/agent.json"} {
		if _, err := call(d, "open", params(path)); err == nil {
			t.Fatalf("opened protected path %q", path)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err == nil {
		if _, err := call(d, "list", params("escape")); err == nil {
			t.Fatal("followed a link outside the workspace")
		}
	}
	listing, err := call(d, "list", params(""))
	if err != nil {
		t.Fatal(err)
	}
	entries := listing.(map[string]any)["entries"].([]directFileEntry)
	if len(entries) != 1 || entries[0].Name != "产物.apk" || entries[0].Size != largeOffset+3 {
		t.Fatalf("listing: %+v", entries)
	}
	started, err := call(d, "open", params(file))
	if err != nil {
		t.Fatal(err)
	}
	id := started.(map[string]any)["id"]
	read := func(session *directSession, offset float64) (any, error) {
		return call(session, "read", map[string]any{"id": id, "offset": offset})
	}
	if _, err := read(&directSession{}, 0); err == nil {
		t.Fatal("another connection read an unrelated handle")
	}
	if _, err := read(d, 1); err == nil {
		t.Fatal("accepted a nonsequential offset")
	}
	for offset := 0; offset <= largeOffset; offset += attachmentChunkSize {
		result, err := read(d, float64(offset))
		if err != nil {
			t.Fatalf("read at %d: %v", offset, err)
		}
		if offset == largeOffset {
			data, err := base64.StdEncoding.DecodeString(result.(map[string]any)["data"].(string))
			if err != nil || string(data) != "end" {
				t.Fatalf("last chunk: %q, %v", data, err)
			}
		}
	}
	if d.files.read != nil {
		t.Fatal("completed handle was not released")
	}
}

func TestComputerFileScope(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	private := filepath.Join(outside, "agent-data")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(outside, "工作区外.txt")
	secretFile := filepath.Join(private, "private.txt")
	for _, path := range []string{file, secretFile} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a := &Agent{cfg: Config{Cwd: workspace, DataDir: private, AllowFileManagement: true}}
	d := &directSession{}
	t.Cleanup(d.files.close)
	call := func(d *directSession, method string, params map[string]any) (any, error) {
		return a.directFileRPC(context.Background(), nil, d, protocol.DirectFilePrefix+method, params)
	}
	params := func(path string) map[string]any { return map[string]any{"scope": "computer", "path": path} }
	for _, method := range []string{"roots", "list", "stat", "open"} {
		if _, err := call(nil, method, params(file)); err == nil {
			t.Fatalf("relay accepted %s", method)
		}
	}
	roots, err := call(d, "roots", nil)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.VolumeName(outside) + string(filepath.Separator)
	if actual := roots.(map[string]any)["roots"].([]string); !slices.Contains(actual, root) {
		t.Fatalf("roots %q do not contain %q", actual, root)
	}
	if _, err := call(d, "list", params(root)); err != nil {
		t.Fatalf("browse %q: %v", root, err)
	}
	listing, err := call(d, "list", params(outside))
	if err != nil {
		t.Fatal(err)
	}
	entries := listing.(map[string]any)["entries"].([]directFileEntry)
	if len(entries) != 1 || entries[0].Name != filepath.Base(file) {
		t.Fatalf("unexpected listing: %+v", entries)
	}
	if _, err := call(d, "open", map[string]any{"workspace": workspace, "path": file}); err == nil {
		t.Fatal("legacy workspace request opened a file outside the workspace")
	}
	for _, target := range []string{"", "relative", secretFile, outside + string(filepath.Separator) + ".."} {
		if _, err := call(d, "list", params(target)); err == nil {
			t.Fatalf("listed invalid or protected path %q", target)
		}
	}
	if _, err := call(d, "open", params(secretFile)); err == nil {
		t.Fatal("opened Agent state in computer mode")
	}
	if runtime.GOOS == "windows" {
		for _, target := range []string{root[:2], root + "NUL", file + ":stream", "\\\\host\\share\\file", "\\\\?\\" + file} {
			if _, _, err := computerDirectFilePath(target); err == nil {
				t.Fatalf("accepted invalid drive path %q", target)
			}
		}
	}
	if _, err := call(d, "stat", params(file)); err != nil {
		t.Fatal(err)
	}
	opened, err := call(d, "open", params(file))
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := call(d, "read", map[string]any{"id": opened.(map[string]any)["id"], "offset": float64(0)})
	if err != nil || chunk.(map[string]any)["data"] != base64.StdEncoding.EncodeToString([]byte("fixture")) {
		t.Fatalf("computer download = %v, %v", chunk, err)
	}
}
