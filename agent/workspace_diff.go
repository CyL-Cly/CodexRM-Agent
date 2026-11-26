package agent

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
)

const workspaceDiffLimit = 512 << 10

type workspaceDiffBuffer struct {
	buffer   bytes.Buffer
	overflow bool
}

func (b *workspaceDiffBuffer) Write(p []byte) (int, error) {
	remaining := workspaceDiffLimit - b.buffer.Len()
	if len(p) > remaining {
		b.overflow = true
		return 0, errors.New("改动内容超过 512 KiB，请在电脑端查看")
	}
	return b.buffer.Write(p)
}

func (a *Agent) workspaceDiff(ctx context.Context, app *appserver.Client, params map[string]any) (map[string]any, error) {
	if !a.supportsDirectFiles() {
		return nil, errors.New("Agent 与 Codex 不在同一设备")
	}
	path, err := a.directWorkspace(ctx, app, params)
	if err != nil {
		return nil, err
	}
	run := func(args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "-c", "core.fsmonitor=false"}, args...)...)
		cmd.Dir = path
		return cmd
	}
	diffArgs := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--relative"}
	if run("rev-parse", "--verify", "HEAD").Run() == nil {
		diffArgs = append(diffArgs, "HEAD")
	} else {
		// Hash without writing any object. Git recognizes its empty tree for
		// either the SHA-1 or SHA-256 object format.
		empty, err := run("hash-object", "-t", "tree", "--stdin").Output()
		if err != nil {
			return nil, errors.New("无法读取 Git 空树")
		}
		diffArgs = append(diffArgs, strings.TrimSpace(string(empty)))
	}
	var output workspaceDiffBuffer
	cmd := run(append(diffArgs, "--", ".")...)
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		if output.overflow {
			return nil, errors.New("改动内容超过 512 KiB，请在电脑端查看")
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("无法读取 Git 改动，请检查此工作区及电脑上的 Git")
	}
	var untracked workspaceDiffBuffer
	cmd = run("ls-files", "--others", "--exclude-standard", "-z", "--", ".")
	cmd.Stdout = &untracked
	if err := cmd.Run(); err != nil {
		return nil, errors.New("无法读取未跟踪文件，或文件列表过大")
	}
	names := strings.Split(strings.TrimSuffix(untracked.buffer.String(), "\x00"), "\x00")
	if len(names) == 1 && names[0] == "" {
		names = nil
	}
	return map[string]any{"diff": output.buffer.String(), "untracked": names}, nil
}
