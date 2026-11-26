//go:build windows

package installation

import (
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

var getCurrentPackageFamilyName = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCurrentPackageFamilyName")

// PackageFamilyName is empty for the standalone EXE. Windows preserves this
// identity when a package is updated or moved to another drive.
func PackageFamilyName() string {
	var buffer [256]uint16
	length := uint32(len(buffer))
	result, _, _ := getCurrentPackageFamilyName.Call(uintptr(unsafe.Pointer(&length)), uintptr(unsafe.Pointer(&buffer[0])))
	if result != 0 {
		return ""
	}
	return windows.UTF16ToString(buffer[:])
}

func IsPackaged() bool { return PackageFamilyName() != "" }

// PackageConfigPath must never depend on the versioned, read-only WindowsApps
// installation path. LocalState survives both package updates and drive moves.
func PackageConfigPath() string {
	family := PackageFamilyName()
	if family == "" {
		return ""
	}
	local, err := os.UserCacheDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		local = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(local, "Packages", family, "LocalState", "connection.json")
}
