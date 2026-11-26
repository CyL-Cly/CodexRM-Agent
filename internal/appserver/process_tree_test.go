package appserver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLaunchKillsProcessTree(t *testing.T) {
	helper := buildProcessTreeHelper(t)
	for _, mode := range []string{"cancel", "close"} {
		t.Run(mode, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pids.txt")
			t.Setenv("CODEXRM_TEST_PID_FILE", pidFile)
			ctx, cancel := context.WithCancel(context.Background())
			client, err := Launch(ctx, helper, t.TempDir())
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			parentPID, childPID := readProcessTreePIDs(t, pidFile)
			t.Cleanup(func() {
				cancel()
				_ = client.Close()
				for _, pid := range []int{parentPID, childPID} {
					process, findErr := os.FindProcess(pid)
					if findErr == nil {
						_ = process.Kill()
					}
				}
			})

			if mode == "cancel" {
				cancel()
			} else if err = client.Close(); err != nil {
				t.Fatalf("close app-server: %v", err)
			}
			select {
			case <-client.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("app-server client did not close within 10 seconds")
			}
			assertProcessTreeExited(t, parentPID, childPID)
		})
	}
}

func buildProcessTreeHelper(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "main.go")
	code := `package main

import (
    "fmt"
    "os"
    "os/exec"
    "time"
)

func main() {
    if os.Getenv("CODEXRM_TEST_CHILD") == "1" {
        for { time.Sleep(time.Hour) }
    }
    child := exec.Command(os.Args[0])
    child.Env = append(os.Environ(), "CODEXRM_TEST_CHILD=1")
    child.Stdin = os.Stdin
    child.Stdout = os.Stdout
    child.Stderr = os.Stderr
    if err := child.Start(); err != nil { panic(err) }
    contents := fmt.Sprintf("%d\n%d\n", os.Getpid(), child.Process.Pid)
    if err := os.WriteFile(os.Getenv("CODEXRM_TEST_PID_FILE"), []byte(contents), 0600); err != nil { panic(err) }
    for { time.Sleep(time.Hour) }
}
`
	if err := os.WriteFile(source, []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "helper")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	output, err := exec.Command("go", "build", "-o", binary, source).CombinedOutput()
	if err != nil {
		t.Fatalf("build process helper: %v\n%s", err, output)
	}
	return binary
}

func readProcessTreePIDs(t *testing.T, path string) (int, int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			fields := strings.Fields(string(data))
			if len(fields) == 2 {
				parent, parentErr := strconv.Atoi(fields[0])
				child, childErr := strconv.Atoi(fields[1])
				if parentErr == nil && childErr == nil && parent > 0 && child > 0 {
					return parent, child
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("process helper did not write two PIDs to %s", path)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func assertProcessTreeExited(t *testing.T, parentPID, childPID int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		parentGone, err := processExited(parentPID)
		if err != nil {
			t.Fatal(err)
		}
		childGone, err := processExited(childPID)
		if err != nil {
			t.Fatal(err)
		}
		if parentGone && childGone {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(fmt.Sprintf("process tree still running: parent=%d gone=%v child=%d gone=%v", parentPID, parentGone, childPID, childGone))
		}
		time.Sleep(25 * time.Millisecond)
	}
}
