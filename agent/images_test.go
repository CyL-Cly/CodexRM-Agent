package agent

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestReadImageChunksAndRelativePaths(t *testing.T) {
	root := t.TempDir()
	a := &Agent{cfg: Config{Cwd: root}}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aA3sAAAAASUVORK5CYII=")
	data := append(png, bytes.Repeat([]byte{0}, attachmentChunkSize+1)...)
	path := filepath.Join(root, "图片.png")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{path, "图片.png"} {
		var got []byte
		version := ""
		for offset := int64(0); offset < int64(len(data)); {
			result, err := a.readImage(map[string]any{"path": source, "offset": float64(offset), "version": version})
			if err != nil {
				t.Fatal(err)
			}
			value := result.(map[string]any)
			file := value["file"].(map[string]any)
			if file["mimeType"] != "image/png" || file["size"] != int64(len(data)) {
				t.Fatal(file)
			}
			version = file["version"].(string)
			chunk, err := base64.StdEncoding.DecodeString(value["data"].(string))
			if err != nil || len(chunk) > attachmentChunkSize {
				t.Fatal("invalid chunk", err)
			}
			got = append(got, chunk...)
			offset = value["offset"].(int64)
		}
		if !bytes.Equal(got, data) {
			t.Fatal("image changed in transit")
		}
	}
	if _, err := a.readImage(map[string]any{"path": path, "offset": float64(0), "version": "stale"}); err == nil {
		t.Fatal("accepted changed file")
	}
}

func TestReadImageRejectsOtherFilesAndInvalidOffsets(t *testing.T) {
	root := t.TempDir()
	a := &Agent{cfg: Config{Cwd: root}}
	text := filepath.Join(root, "fake.png")
	if err := os.WriteFile(text, []byte("private text, not an image"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", text, root, "missing.png", `\\host\share\image.png`, "//host/share/image.png"} {
		if _, err := a.readImage(map[string]any{"path": path, "offset": float64(0)}); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aA3sAAAAASUVORK5CYII=")
	if err := os.WriteFile(text, png, 0600); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []any{-1.0, 0.5, float64(len(png) + 1), "0"} {
		if _, err := a.readImage(map[string]any{"path": text, "offset": offset}); err == nil {
			t.Fatalf("accepted offset %v", offset)
		}
	}
	large := filepath.Join(root, "large.png")
	f, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(attachmentMaxSize + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := a.readImage(map[string]any{"path": large, "offset": float64(0)}); err == nil {
		t.Fatal("accepted oversized image")
	}
}
