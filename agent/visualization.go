package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/google/uuid"
)

const (
	visualizationRefPrefix = "\ue200visualize\ue202"
	visualizationRefSuffix = "\ue201"
	visualizationDirective = `::codex-inline-vis{`
	maxVisualizationBytes  = 1 << 20
)

var errVisualizationFile = errors.New("互动图表文件不可用或超出 1 MiB")

func parseVisualizationReference(line string) string {
	line = strings.TrimSpace(line)
	if len(line) > 8192 {
		return ""
	}
	path := ""
	if payload, ok := strings.CutPrefix(line, visualizationRefPrefix); ok {
		payload, ok = strings.CutSuffix(payload, visualizationRefSuffix)
		if !ok {
			return ""
		}
		var ref struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(payload), &ref) != nil || !filepath.IsAbs(ref.Path) {
			return ""
		}
		path = ref.Path
	} else if attributes, ok := strings.CutPrefix(line, visualizationDirective); ok {
		attributes, ok = strings.CutSuffix(attributes, "}")
		if !ok {
			return ""
		}
		value, ok := strings.CutPrefix(strings.TrimSpace(attributes), `file="`)
		if !ok {
			return ""
		}
		path, ok = strings.CutSuffix(value, `"`)
		if !ok || strings.Contains(path, `"`) {
			return ""
		}
	}
	return visualizationPathKey(path)
}

func visualizationPathKey(path string) string {
	key := mentionedPathKey(path)
	if !strings.EqualFold(filepath.Ext(key), ".html") {
		return ""
	}
	// Apply the same local-path rules to both absolute and relative references.
	if filepath.IsAbs(key) {
		if _, _, err := computerDirectFilePath(key); err != nil {
			return ""
		}
	} else if key == "" || !filepath.IsLocal(key) || strings.Contains(key, ":") {
		return ""
	}
	return key
}

func visualizationCodexHome() string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(user, ".codex")
	}
	if !filepath.IsAbs(home) || strings.HasPrefix(home, `\\`) || strings.HasPrefix(home, "//") {
		return ""
	}
	return filepath.Clean(home)
}

func threadVisualizationPath(home, thread, path string) (string, bool) {
	if home == "" || !filepath.IsAbs(path) {
		return "", false
	}
	id, err := uuid.Parse(thread)
	if err != nil || thread != id.String() {
		return "", false
	}
	rel, err := localDirectFilePath(home, path)
	if err != nil {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 6 || parts[0] != "visualizations" || parts[4] != thread {
		return "", false
	}
	if _, err := time.Parse("2006/01/02", strings.Join(parts[1:4], "/")); err != nil {
		return "", false
	}
	return rel, true
}

func (a *Agent) readVisualization(ctx context.Context, app *appserver.Client, params map[string]any) (any, error) {
	thread, _ := params["threadId"].(string)
	requested, _ := params["path"].(string)
	path := visualizationPathKey(requested)
	if !a.supportsDirectFiles() || path == "" || !a.mentionedFiles.containsKind(thread, path, true) {
		return nil, errVisualizationFile
	}

	home := visualizationCodexHome()
	realHome, _ := filepath.EvalSymlinks(home)
	if home == "" {
		realHome = ""
	}
	var base, rel string
	if local, ok := threadVisualizationPath(home, thread, path); ok {
		base, rel = home, local
	} else if local, ok := threadVisualizationPath(realHome, thread, path); ok {
		base, rel = realHome, local
	} else {
		// Some executors use a task-owned folder in the workspace instead of
		// CODEX_HOME. The client never supplies the workspace or a scope.
		if app == nil {
			return nil, errVisualizationFile
		}
		var err error
		base, rel, err = threadWorkspaceFile(ctx, app, thread, path)
		if err != nil || home != "" && pathInside(home, filepath.Join(base, rel)) ||
			realHome != "" && pathInside(realHome, filepath.Join(base, rel)) {
			return nil, errVisualizationFile
		}
	}
	root, info, err := checkedDirectRoot(base, rel, a.directPrivatePath())
	if err != nil {
		return nil, errVisualizationFile
	}
	defer root.Close()
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxVisualizationBytes {
		return nil, errVisualizationFile
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, errVisualizationFile
	}
	defer f.Close()
	if actual, err := f.Stat(); err != nil || !os.SameFile(info, actual) {
		return nil, errVisualizationFile
	}
	data, err := io.ReadAll(io.LimitReader(f, maxVisualizationBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxVisualizationBytes || !utf8.Valid(data) {
		return nil, errVisualizationFile
	}
	return map[string]string{"html": string(data)}, nil
}
