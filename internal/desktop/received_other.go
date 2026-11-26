//go:build !windows

package desktop

import "errors"

const ReceivesFiles = false

func OpenReceivedFile(string) error               { return errors.ErrUnsupported }
func ReceivedFileIdentity(string) (string, error) { return "", errors.ErrUnsupported }

func Downloads() (string, error)                            { return "", errors.New("当前 Agent 不支持接收桌面文件") }
func MoveReceivedFile(source, target string) error          { return errors.ErrUnsupported }
func ShowReceivedFiles(sender string, files []ReceivedFile) {}
