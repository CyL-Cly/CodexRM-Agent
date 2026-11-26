//go:build windows

package desktop

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// EnsureP2PFirewall allows the random UDP port selected by each P2P session.
func EnsureP2PFirewall() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位 Agent 程序: %w", err)
	}
	return ensureP2PFirewall(executable, firewallRulePresent, registerFirewallRule, firewallFailurePrompt)
}

func ensureP2PFirewall(executable string, present func(string) bool, register func(string, string) error, retry func() bool) error {
	name := firewallRuleName(executable)
	for !present(name) {
		_ = register(name, executable)
		if present(name) {
			return nil
		}
		if !retry() {
			return ErrP2PFirewallRequired
		}
	}
	return nil
}

func firewallRuleName(executable string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(executable))))
	return fmt.Sprintf("CodexRM P2P %x", sum[:8])
}

func netshPath() string {
	return filepath.Join(os.Getenv("SystemRoot"), "System32", "netsh.exe")
}

func firewallRulePresent(name string) bool {
	cmd := exec.Command(netshPath(), "advfirewall", "firewall", "show", "rule", "name="+name)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run() == nil
}

func registerFirewallRule(name, executable string) error {
	args := []string{
		"advfirewall", "firewall", "add", "rule",
		"name=" + name,
		"description=CodexRM Android P2P UDP/QUIC",
		"dir=in", "action=allow", "protocol=UDP", "localport=any",
		"program=" + executable, "enable=yes", "profile=any",
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = syscall.EscapeArg(arg)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(netshPath())
	parameters, _ := windows.UTF16PtrFromString(strings.Join(quoted, " "))
	info := shellExecuteInfo{
		FMask:      0x40, // SEE_MASK_NOCLOSEPROCESS
		Verb:       verb,
		File:       file,
		Parameters: parameters,
		Show:       windows.SW_HIDE,
	}
	info.CbSize = uint32(unsafe.Sizeof(info))
	result, _, callErr := windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW").Call(uintptr(unsafe.Pointer(&info)))
	if result == 0 {
		return fmt.Errorf("启动防火墙注册: %w", callErr)
	}
	if info.Process == 0 {
		return errors.New("防火墙注册进程不可用")
	}
	process := windows.Handle(info.Process)
	defer windows.CloseHandle(process)
	if status, err := windows.WaitForSingleObject(process, windows.INFINITE); err != nil || status != windows.WAIT_OBJECT_0 {
		if err != nil {
			return fmt.Errorf("等待防火墙注册: %w", err)
		}
		return errors.New("等待防火墙注册失败")
	}
	var code uint32
	if err := windows.GetExitCodeProcess(process, &code); err != nil {
		return fmt.Errorf("读取防火墙注册结果: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("netsh 退出码 %d", code)
	}
	return nil
}

type shellExecuteInfo struct {
	CbSize        uint32
	FMask         uint32
	HWnd          uintptr
	Verb          *uint16
	File          *uint16
	Parameters    *uint16
	Directory     *uint16
	Show          int32
	InstApp       uintptr
	IDList        uintptr
	Class         *uint16
	HKeyClass     uintptr
	DwHotKey      uint32
	IconOrMonitor uintptr
	Process       uintptr
}

type taskDialogButton struct {
	ID   int32
	Text *uint16
}

type taskDialogConfig struct {
	CbSize               uint32
	HWndParent           uintptr
	HInstance            uintptr
	Flags                uint32
	CommonButtons        uint32
	WindowTitle          *uint16
	MainIcon             uintptr
	MainInstruction      *uint16
	Content              *uint16
	CButtons             uint32
	Buttons              *taskDialogButton
	DefaultButton        int32
	CRadioButtons        uint32
	RadioButtons         *taskDialogButton
	VerificationText     *uint16
	ExpandedInformation  *uint16
	ExpandedControlText  *uint16
	CollapsedControlText *uint16
	FooterIcon           uintptr
	Footer               *uint16
	Callback             uintptr
	CallbackData         uintptr
	CxWidth              uint32
}

func firewallFailurePrompt() bool {
	title, _ := windows.UTF16PtrFromString("CodexRM")
	instruction, _ := windows.UTF16PtrFromString("需要 P2P 防火墙规则")
	content, _ := windows.UTF16PtrFromString("P2P 规则注册失败。CodexRM 需要允许 UDP 入站连接才能建立直连。请重试并在 UAC 提示中允许，或退出软件。")
	retryText, _ := windows.UTF16PtrFromString("重试")
	exitText, _ := windows.UTF16PtrFromString("退出软件")
	buttons := []taskDialogButton{{ID: 100, Text: retryText}, {ID: 101, Text: exitText}}
	config := taskDialogConfig{
		HWndParent:      0,
		WindowTitle:     title,
		MainIcon:        0xFFFE, // TD_ERROR_ICON
		MainInstruction: instruction,
		Content:         content,
		CButtons:        uint32(len(buttons)),
		Buttons:         &buttons[0],
		DefaultButton:   100,
	}
	config.CbSize = uint32(unsafe.Sizeof(config))
	var selected int32
	result, _, _ := windows.NewLazySystemDLL("comctl32.dll").NewProc("TaskDialogIndirect").Call(
		uintptr(unsafe.Pointer(&config)),
		uintptr(unsafe.Pointer(&selected)),
		0,
		0,
	)
	if int32(result) >= 0 {
		return selected == 100
	}

	message, _ := windows.UTF16PtrFromString("P2P 规则注册失败。选择“重试”重新注册；取消将退出软件。")
	answer, _ := windows.MessageBox(0, message, title, windows.MB_RETRYCANCEL|windows.MB_ICONERROR|windows.MB_SETFOREGROUND)
	return answer == 4 // IDRETRY
}
