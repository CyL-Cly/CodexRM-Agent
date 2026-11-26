//go:build !windows

package installation

func PackageFamilyName() string { return "" }
func IsPackaged() bool          { return false }
func PackageConfigPath() string { return "" }
