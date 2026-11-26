package appserver

import (
	"errors"

	"golang.org/x/sys/windows"
)

func processExited(pid int) (bool, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_FOUND) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(process)
	status, err := windows.WaitForSingleObject(process, 0)
	if err != nil {
		return false, err
	}
	return status == windows.WAIT_OBJECT_0, nil
}
