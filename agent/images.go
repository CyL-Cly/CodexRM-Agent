package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
)

func (a *Agent) remoteImage(ctx context.Context, app *appserver.Client, params map[string]any) (any, error) {
	if a.allowsFileManagement() {
		return a.readImage(params)
	}
	base, path, err := a.mentionedFileLocation(ctx, app, params)
	if err != nil {
		return nil, err
	}
	root, before, err := checkedDirectRoot(base, path, a.directPrivatePath())
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("图片不存在或不是普通文件")
	}
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if actual, err := f.Stat(); err != nil || !os.SameFile(before, actual) {
		return nil, fmt.Errorf("图片已变化，请重试")
	}
	return readImageChunk(f, params)
}

func (a *Agent) readImage(params map[string]any) (any, error) {
	path, _ := params["path"].(string)
	if path == "" || strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") {
		return nil, fmt.Errorf("无效的本地图片路径")
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, strings.TrimLeft(path[1:], `/\`))
	}
	if !filepath.IsAbs(path) {
		cwd, _ := params["cwd"].(string)
		if cwd == "" {
			cwd = a.cfg.Cwd
		}
		if !filepath.IsAbs(cwd) || strings.HasPrefix(cwd, `\\`) || strings.HasPrefix(cwd, "//") {
			return nil, fmt.Errorf("图片工作目录无效")
		}
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	// Check before opening so a special file (for example a FIFO) cannot block a
	// reader. Recheck the opened handle below to cover a concurrent replacement.
	before, err := os.Stat(path)
	if err != nil || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("图片不存在或不是普通文件")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("图片不存在或无法读取")
	}
	defer f.Close()
	return readImageChunk(f, params)
}

func readImageChunk(f *os.File, params map[string]any) (any, error) {
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() <= 0 || stat.Size() > attachmentMaxSize {
		return nil, fmt.Errorf("图片不是普通文件或超过 20 MB")
	}
	header := make([]byte, 512)
	n, err := f.ReadAt(header, 0)
	if err != nil && err != io.EOF {
		return nil, err
	}
	mimeType := http.DetectContentType(header[:n])
	switch mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp", "image/x-icon":
	default:
		return nil, fmt.Errorf("此文件不是支持的图片格式")
	}
	version := fmt.Sprintf("%x-%x", stat.Size(), stat.ModTime().UnixNano())
	if expected, _ := params["version"].(string); expected != "" && expected != version {
		return nil, fmt.Errorf("图片已变化，请重试")
	}
	offset, err := attachmentNumber(params, "offset", attachmentMaxSize)
	if err != nil || offset > stat.Size() {
		return nil, fmt.Errorf("invalid image offset")
	}
	data := make([]byte, min(int64(attachmentChunkSize), stat.Size()-offset))
	if _, err := f.ReadAt(data, offset); err != nil {
		return nil, fmt.Errorf("图片内容不完整")
	}
	return map[string]any{
		"file": map[string]any{"name": stat.Name(), "size": stat.Size(), "mimeType": mimeType, "version": version},
		"data": base64.StdEncoding.EncodeToString(data), "offset": offset + int64(len(data)),
	}, nil
}
