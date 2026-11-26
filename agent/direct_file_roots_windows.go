package agent

import "golang.org/x/sys/windows"

func directFileRoots() ([]string, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, err
	}
	roots := make([]string, 0, 26)
	for i := uint32(0); i < 26; i++ {
		if mask&(1<<i) != 0 {
			roots = append(roots, string(rune('A'+i))+":\\")
		}
	}
	return roots, nil
}
