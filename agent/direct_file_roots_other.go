//go:build !windows

package agent

func directFileRoots() ([]string, error) {
	return []string{"/"}, nil
}
