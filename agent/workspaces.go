package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
)

type workspace struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type directoryListing struct {
	Path        string      `json:"path"`
	Parent      string      `json:"parent"`
	Home        string      `json:"home"`
	Roots       []workspace `json:"roots"`
	Directories []workspace `json:"directories"`
	Truncated   bool        `json:"truncated"`
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func directoryPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimLeft(path[1:], `/\`))
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("请输入这台设备上的绝对目录路径")
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("无法打开目录 %s: %w", path, err)
	}
	if !info.IsDir() {
		return "", errors.New("所选路径不是文件夹")
	}
	return path, nil
}

// The list belongs to the local device, so paired browsers share the same projects.
// The mutex also prevents concurrent adds/removes from overwriting one another.
func (a *Agent) workspaceRPC(method string, params map[string]any) (any, error) {
	path, _ := params["path"].(string)
	if method == "codexrm/fs/list" {
		if !a.allowsFileManagement() {
			return nil, errFileManagementDisabled
		}
		if path == "" {
			path = a.cfg.Cwd
		}
		hidden, _ := params["showHidden"].(bool)
		return listDirectories(path, hidden)
	}
	if method != "codexrm/workspace/list" && method != "codexrm/workspace/add" && method != "codexrm/workspace/remove" {
		return nil, &appserver.RPCError{Code: -32601, Message: "Unknown CodexRM method"}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	file := filepath.Join(a.cfg.DataDir, "workspaces.json")
	workspaces := []workspace{}
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		workspaces = append(workspaces, workspace{Name: filepath.Base(a.cfg.Cwd), Path: a.cfg.Cwd})
	} else if err != nil {
		return nil, err
	} else if err = json.Unmarshal(data, &workspaces); err != nil {
		return nil, fmt.Errorf("读取工作区列表失败: %w", err)
	}
	if method == "codexrm/workspace/add" {
		path, err = directoryPath(path)
		if err != nil {
			return nil, err
		}
		found := false
		for _, w := range workspaces {
			found = found || samePath(w.Path, path)
		}
		if !found {
			if len(workspaces) >= 200 {
				return nil, errors.New("最多保存 200 个工作区")
			}
			name := filepath.Base(path)
			if name == "." || name == string(filepath.Separator) || name == "" {
				name = path
			}
			workspaces = append(workspaces, workspace{Name: name, Path: path})
		}
	} else if method == "codexrm/workspace/remove" {
		// A deleted/moved directory can still be removed from the saved list.
		filtered := []workspace{}
		for _, w := range workspaces {
			if !samePath(w.Path, path) {
				filtered = append(filtered, w)
			}
		}
		workspaces = filtered
	}
	if method != "codexrm/workspace/list" || errors.Is(err, os.ErrNotExist) {
		raw, err := json.MarshalIndent(workspaces, "", "  ")
		if err != nil {
			return nil, err
		}
		if err = os.WriteFile(file+".tmp", raw, 0600); err != nil {
			return nil, err
		}
		if err = os.Rename(file+".tmp", file); err != nil {
			return nil, err
		}
	}
	return map[string]any{"workspaces": workspaces}, nil
}

func listDirectories(path string, showHidden bool) (*directoryListing, error) {
	path, err := directoryPath(path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取目录: %w", err)
	}
	home, _ := os.UserHomeDir()
	out := &directoryListing{Path: path, Home: home, Parent: filepath.Dir(path), Roots: []workspace{}, Directories: []workspace{}}
	if samePath(out.Parent, path) {
		out.Parent = ""
	}
	if runtime.GOOS == "windows" {
		for letter := 'A'; letter <= 'Z'; letter++ {
			root := fmt.Sprintf("%c:\\", letter)
			if info, err := os.Stat(root); err == nil && info.IsDir() {
				out.Roots = append(out.Roots, workspace{Name: root, Path: root})
			}
		}
	} else {
		out.Roots = append(out.Roots, workspace{Name: "/", Path: "/"})
	}
	for _, entry := range entries {
		if !showHidden && strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		isDir := entry.IsDir()
		child := filepath.Join(path, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			info, err := os.Stat(child)
			isDir = err == nil && info.IsDir()
		}
		if isDir {
			if len(out.Directories) == 500 {
				out.Truncated = true
				break
			}
			out.Directories = append(out.Directories, workspace{Name: entry.Name(), Path: child})
		}
	}
	sort.Slice(out.Directories, func(i, j int) bool {
		return strings.ToLower(out.Directories[i].Name) < strings.ToLower(out.Directories[j].Name)
	})
	return out, nil
}
