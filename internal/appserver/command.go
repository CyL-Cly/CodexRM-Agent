package appserver

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

var ErrCommandOutputLimit = errors.New("command output exceeds limit")

type commandOutput struct {
	bytes.Buffer
	max      int
	exceeded bool
	cancel   context.CancelFunc
}

func (w *commandOutput) Write(payload []byte) (int, error) {
	if len(payload) > w.max-w.Len() {
		w.exceeded = true
		w.cancel()
		return 0, ErrCommandOutputLimit
	}
	return w.Buffer.Write(payload)
}

// RunBoundedCommand captures a limited amount of combined output and uses the
// same process-tree lifetime rules as the managed Codex child.
func RunBoundedCommand(ctx context.Context, name string, args []string, cwd string, env []string, limit int) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	output := &commandOutput{max: limit, cancel: cancel}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = output, output
	cmd.WaitDelay = 2 * time.Second
	process, err := startManagedProcess(cmd)
	if err != nil {
		return nil, err
	}
	defer process.Close()
	err = cmd.Wait()
	_ = process.Kill() // Also dispose of descendants after a normal leader exit.
	if output.exceeded {
		return nil, ErrCommandOutputLimit
	}
	return output.Bytes(), err
}
