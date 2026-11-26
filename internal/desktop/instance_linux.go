//go:build linux

package desktop

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Instance uses a state-directory file lock for ownership and a short Unix
// socket in the temp directory for open/stop messages. The socket name is
// derived from a hash so long Windows-style config paths cannot exceed the
// sun_path limit, and it is scoped by uid to avoid cross-user signalling.
type Instance struct {
	lockFile *os.File
	listener net.Listener
	socket   string
}

const instanceTimeout = 30 * time.Second

func instanceSocket(stateDir string) (string, error) {
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(abs))))
	return filepath.Join(os.TempDir(), fmt.Sprintf("codexrm-%d-%s.sock", os.Getuid(), hex.EncodeToString(sum[:8]))), nil
}

func tryLock(stateDir string) (*os.File, error) {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(stateDir, ".instance.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func notify(stateDir, action string) error {
	path, err := instanceSocket(stateDir)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	return json.NewEncoder(conn).Encode(struct {
		Action string `json:"action"`
	}{Action: action})
}

func socketMissing(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

func newPrimary(stateDir string, lock *os.File) (*Instance, error) {
	path, err := instanceSocket(stateDir)
	if err != nil {
		return nil, err
	}
	// Holding the lock proves no live owner, so an existing socket is stale.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return &Instance{lockFile: lock, listener: listener, socket: path}, nil
}

func Acquire(stateDir string, restart bool) (*Instance, error) {
	deadline := time.Now().Add(instanceTimeout)
	for {
		lock, err := tryLock(stateDir)
		if err == nil {
			instance, err := newPrimary(stateDir, lock)
			if err != nil {
				_ = lock.Close()
				return nil, err
			}
			return instance, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, err
		}
		action := "open"
		if restart {
			action = "stop"
		}
		notifyErr := notify(stateDir, action)
		if !restart && (notifyErr == nil || socketMissing(notifyErr) && time.Now().After(deadline)) {
			return nil, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("等待现有 CodexRM 退出超时")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func Stop(stateDir string) error {
	err := notify(stateDir, "stop")
	if err == nil || socketMissing(err) {
		return nil
	}
	return err
}

func (i *Instance) Watch(ctx context.Context, open func(), stop func()) {
	go func() {
		<-ctx.Done()
		_ = i.listener.Close()
	}()
	for {
		conn, err := i.listener.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			var message struct {
				Action string `json:"action"`
			}
			if err := json.NewDecoder(bufio.NewReader(c)).Decode(&message); err != nil {
				return
			}
			switch message.Action {
			case "open":
				open()
			case "stop":
				stop()
			}
		}(conn)
	}
}

func (i *Instance) Close() {
	if i.listener != nil {
		_ = i.listener.Close()
	}
	if i.socket != "" {
		_ = os.Remove(i.socket)
	}
	if i.lockFile != nil {
		_ = syscall.Flock(int(i.lockFile.Fd()), syscall.LOCK_UN)
		_ = i.lockFile.Close()
	}
}
