package appserver

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

func resolveCodexExecutable(executable string) (string, error) {
	if runtime.GOOS != "windows" {
		return resolveCodexExecutableFrom(executable)
	}
	return resolveCodexExecutableFrom(executable,
		runningCodexDesktopCLIs, registeredCodexDesktopCLIs, codexDesktopCandidates,
		func() []string {
			return codexDesktopPackageCandidates(codexDesktopPackageRoots(codexDesktopDriveRoots()))
		},
	)
}

// Query each desktop source only when the preceding source has no usable CLI.
// Explicit paths and custom command names always take precedence.
func resolveCodexExecutableFrom(executable string, desktopSources ...func() []string) (string, error) {
	if executable == "" {
		executable = "codex"
	}
	isDefault := strings.EqualFold(executable, "codex") ||
		(runtime.GOOS == "windows" && strings.EqualFold(executable, "codex.exe"))
	if isDefault {
		for _, source := range desktopSources {
			for _, candidate := range source() {
				if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
					return candidate, nil
				}
			}
		}
	}
	if path, err := exec.LookPath(executable); err == nil {
		return path, nil
	}
	if !isDefault {
		return executable, nil
	}
	return "", fmt.Errorf("Codex CLI not found in Codex desktop installation or PATH; install Codex desktop or pass --codex")
}

func codexDesktopCandidates() []string {
	var candidates []string
	for _, base := range []string{os.Getenv("LOCALAPPDATA"), os.Getenv("ProgramW6432"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if base == "" {
			continue
		}
		for _, relative := range []string{
			filepath.Join("Programs", "Codex", "resources", "codex.exe"),
			filepath.Join("Programs", "Codex", "app", "resources", "codex.exe"),
			filepath.Join("Codex", "resources", "codex.exe"),
			filepath.Join("Codex", "app", "resources", "codex.exe"),
		} {
			candidates = append(candidates, filepath.Join(base, relative))
		}
	}
	return candidates
}

func codexDesktopPackageRoots(drives []string) []string {
	var roots []string
	for _, name := range []string{"ProgramW6432", "ProgramFiles", "ProgramFiles(x86)"} {
		if base := os.Getenv(name); base != "" {
			roots = append(roots, filepath.Join(base, "WindowsApps"))
		}
	}
	if systemDrive := os.Getenv("SystemDrive"); systemDrive != "" {
		drives = append(drives, systemDrive+string(filepath.Separator))
	}
	for _, drive := range drives {
		roots = append(roots, filepath.Join(drive, "Program Files", "WindowsApps"), filepath.Join(drive, "WindowsApps"))
	}
	return roots
}

func codexDesktopPackageCandidates(roots []string) []string {
	type candidate struct {
		path string
		info os.FileInfo
	}
	var found []candidate
	for _, root := range roots {
		for _, relative := range []string{filepath.Join("app", "resources", "codex.exe"), filepath.Join("resources", "codex.exe")} {
			matches, _ := filepath.Glob(filepath.Join(root, "OpenAI.Codex_*", relative))
			for _, path := range matches {
				if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
					found = append(found, candidate{path: path, info: info})
				}
			}
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].info.ModTime().After(found[j].info.ModTime()) })
	paths := make([]string, 0, len(found))
	for _, item := range found {
		paths = append(paths, item.path)
	}
	return paths
}
