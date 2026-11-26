package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var (
	errFileManagementDisabled = errors.New("此电脑未允许远程管理文件")
	errFileNotMentioned       = errors.New("只能下载此会话中 AI 回复链接的工作区文件")
)

func (a *Agent) allowsFileManagement() bool {
	return a.cfg.AllowFileManagement && a.supportsDirectFiles()
}

// Do not expose app-server's unrestricted filesystem/shell APIs, or allow a
// remote peer to forge an assistant history that authorizes file downloads.
func remoteRPCPermission(method string, params map[string]any) error {
	config, _ := params["config"].(map[string]any)
	switch {
	case strings.HasPrefix(method, "fs/"), strings.HasPrefix(method, "fuzzyFileSearch"),
		strings.HasPrefix(method, "command/exec"), strings.HasPrefix(method, "process/"), method == "thread/shellCommand":
		return &appserver.RPCError{Code: -32601, Message: "不支持直接调用 Codex 文件系统或命令执行接口"}
	case (method == "config/value/write" || method == "config/batchWrite") && params["filePath"] != nil:
		return &appserver.RPCError{Code: -32602, Message: "远程配置写入不能指定本机文件路径"}
	case method == "thread/inject_items", method == "thread/resume" && params["history"] != nil,
		(method == "thread/resume" || method == "thread/fork") && (params["path"] != nil || params["cwd"] != nil || config["cwd"] != nil):
		return &appserver.RPCError{Code: -32602, Message: "远程会话只能按 ID 打开，不允许注入历史、指定记录路径或更换历史工作目录"}
	default:
		return nil
	}
}

const maxMentionedPaths = 4096

// Only called with actual app-server results and completed item notifications.
// This bounded, in-memory index is rebuilt as clients view conversation history.
type mentionedFileIndex struct {
	mu   sync.RWMutex
	seen map[mentionedFile]struct{}
	ring []mentionedFile
	next int
}

type mentionedFile struct{ thread, path string }

type assistantFileItem struct{ Type, Text string }

type assistantFileTurn struct{ Items []assistantFileItem }

func (s *mentionedFileIndex) addHistory(method string, params map[string]any, result json.RawMessage) {
	switch method {
	case "thread/read", "thread/resume", "thread/fork":
		s.addThread(result)
	case "thread/turns/list":
		id, _ := params["threadId"].(string)
		var page struct{ Data []assistantFileTurn }
		if json.Unmarshal(result, &page) == nil {
			s.addTurns(id, page.Data)
		}
	}
}

func (s *mentionedFileIndex) addThread(raw json.RawMessage) {
	var result struct {
		Thread struct {
			ID    string
			Turns []assistantFileTurn
		}
	}
	if json.Unmarshal(raw, &result) == nil {
		s.addTurns(result.Thread.ID, result.Thread.Turns)
	}
}

func (s *mentionedFileIndex) addTurns(id string, turns []assistantFileTurn) {
	for _, turn := range turns {
		for _, item := range turn.Items {
			s.addItem(id, item)
		}
	}
}

var mentionLine = regexp.MustCompile(`#L[0-9]+(?:C[0-9]+)?(?:-L[0-9]+(?:C[0-9]+)?)?$`)
var mentionWindowsLine = regexp.MustCompile(`:[0-9]+(?::[0-9]+)?$`)
var mentionScheme = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

func localMentionDestination(raw []byte) string {
	dest := mentionLine.ReplaceAllString(string(util.URLEscape(raw, true)), "")
	var path string
	if strings.HasPrefix(strings.ToLower(dest), "file:") {
		u, err := url.Parse(dest)
		if err != nil || u.Host != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return ""
		}
		path = u.Path
	} else {
		var err error
		dest = strings.TrimPrefix(dest, "sandbox:")
		if strings.ContainsAny(dest, "?#") {
			return ""
		}
		path, err = url.PathUnescape(dest)
		if err != nil {
			return ""
		}
	}
	if runtime.GOOS == "windows" {
		path = filepath.FromSlash(path)
		if p := strings.TrimPrefix(path, `\`); len(filepath.VolumeName(p)) == 2 && filepath.IsAbs(p) {
			path = mentionWindowsLine.ReplaceAllString(p, "")
			return mentionedPathKey(path)
		}
	}
	if mentionScheme.MatchString(path) {
		return ""
	}
	return mentionedPathKey(path)
}

func mentionedPathKey(path string) string {
	if len(path) > 4096 || strings.TrimSpace(path) == "" || strings.HasPrefix(path, "//") ||
		strings.HasPrefix(path, `\`) || strings.HasSuffix(path, "/") || strings.HasSuffix(path, `\`) ||
		strings.ContainsFunc(path, unicode.IsControl) {
		return ""
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return ""
		}
	}
	return filepath.Clean(path)
}

func (s *mentionedFileIndex) addItem(thread string, item assistantFileItem) {
	if thread == "" || len(thread) > 128 || item.Type != "agentMessage" || !strings.Contains(item.Text, "[") {
		return
	}
	markdown := []byte(item.Text[:min(len(item.Text), 65536)])
	var paths []string
	_ = ast.Walk(goldmark.DefaultParser().Parse(text.NewReader(markdown)), func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if enter {
			var dest []byte
			switch node := n.(type) {
			case *ast.Link:
				dest = node.Destination
			case *ast.Image:
				dest = node.Destination
			}
			if p := localMentionDestination(dest); p != "" {
				paths = append(paths, p)
				if len(paths) == 64 {
					return ast.WalkStop, nil
				}
			}
		}
		return ast.WalkContinue, nil
	})
	if len(paths) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = make(map[mentionedFile]struct{})
	}
	for _, p := range paths {
		key := mentionedFile{thread, p}
		if _, ok := s.seen[key]; ok {
			continue
		}
		if len(s.ring) == maxMentionedPaths {
			delete(s.seen, s.ring[s.next])
			s.ring[s.next] = key
			s.next = (s.next + 1) % maxMentionedPaths
		} else {
			s.ring = append(s.ring, key)
		}
		s.seen[key] = struct{}{}
	}
}

func (s *mentionedFileIndex) contains(thread, path string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.seen[mentionedFile{thread, mentionedPathKey(path)}]
	return ok
}

func (a *Agent) mentionedFileLocation(ctx context.Context, app *appserver.Client, params map[string]any) (base, rel string, err error) {
	thread, _ := params["threadId"].(string)
	path, _ := params["path"].(string)
	// A client-provided workspace/cwd never determines where a reference points.
	if app == nil || !a.supportsDirectFiles() || params["scope"] == "computer" || !a.mentionedFiles.contains(thread, path) {
		return "", "", errFileNotMentioned
	}
	callCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	raw, err := app.Call(callCtx, "thread/read", map[string]any{"threadId": thread, "includeTurns": false})
	var result struct{ Thread struct{ ID, Cwd string } }
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Thread.ID != thread || !filepath.IsAbs(result.Thread.Cwd) ||
		strings.HasPrefix(result.Thread.Cwd, `\\`) || strings.HasPrefix(result.Thread.Cwd, "//") {
		return "", "", errFileNotMentioned
	}
	rel, err = localDirectFilePath(result.Thread.Cwd, path)
	if err != nil || rel == "." {
		return "", "", errFileNotMentioned
	}
	base, err = filepath.EvalSymlinks(result.Thread.Cwd)
	if err != nil || strings.HasPrefix(base, `\\`) || strings.HasPrefix(base, "//") {
		return "", "", errFileNotMentioned
	}
	return base, rel, nil
}
