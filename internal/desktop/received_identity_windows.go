package desktop

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// ReceivedFileIdentity prevents an old history entry from opening a different
// file when its Downloads name is reused. In-place edits keep the same ID.
func ReceivedFileIdentity(path string) (string, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return "", errors.New("文件已移走、删除或不再是普通文件")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil || !os.SameFile(before, current) {
		return "", errors.New("文件已更换")
	}
	var fileID [24]byte // FILE_ID_INFO: 64-bit volume serial followed by 128-bit file ID.
	if err := windows.GetFileInformationByHandleEx(windows.Handle(f.Fd()), windows.FileIdInfo, &fileID[0], uint32(len(fileID))); err == nil && [16]byte(fileID[8:]) != [16]byte{} {
		return fmt.Sprintf("id:%x", fileID), nil
	}
	// Some redirected Downloads volumes don't implement FileIdInfo.
	return fmt.Sprintf("stamp:%d:%d", current.Size(), current.ModTime().UnixNano()), nil
}
