package desktop

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const ReceivesFiles = true

func Downloads() (string, error) {
	return windows.KnownFolderPath(windows.FOLDERID_Downloads, 0)
}

func MoveReceivedFile(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFile(from, to)
}

var receiveDialogMu sync.Mutex

func ShowReceivedFiles(sender string, files []ReceivedFile) {
	if len(files) == 0 {
		return
	}
	go func() {
		receiveDialogMu.Lock()
		defer receiveDialogMu.Unlock()
		title := windows.StringToUTF16Ptr("CodexRM · 文件接收")
		single := len(files) == 1
		path := files[0].Path
		heading := fmt.Sprintf("收到「%s」发来的 %d 个文件", sender, len(files))
		var summary strings.Builder
		for _, file := range files[:min(6, len(files))] {
			fmt.Fprintf(&summary, "%s · %s\n", file.Name, receivedSizeLabel(file.Size))
		}
		if len(files) > 6 {
			fmt.Fprintf(&summary, "另有 %d 个文件\n", len(files)-6)
		}
		summary.WriteString("已保存到下载文件夹")
		buttons := []taskDialogButton{{ID: 100, Text: windows.StringToUTF16Ptr("打开文件夹")}}
		if single {
			buttons = append(buttons, taskDialogButton{ID: 101, Text: windows.StringToUTF16Ptr("打开")})
		}
		buttons = append(buttons, taskDialogButton{ID: 102, Text: windows.StringToUTF16Ptr("关闭")})
		selected, displayed := receivedTaskDialog(title, windows.StringToUTF16Ptr(heading), windows.StringToUTF16Ptr(summary.String()), buttons)
		if !displayed {
			message := heading + "\n\n" + summary.String()
			flags := uint32(windows.MB_ICONINFORMATION | windows.MB_SETFOREGROUND | windows.MB_TOPMOST)
			if single {
				response, _ := windows.MessageBox(0, windows.StringToUTF16Ptr(message+"\n\n是：打开文件夹\n否：打开文件\n取消：关闭"), title,
					flags|windows.MB_YESNOCANCEL|windows.MB_DEFBUTTON3)
				if response == 6 {
					selected = 100
				} else if response == 7 {
					selected = 101
				}
			} else {
				response, _ := windows.MessageBox(0, windows.StringToUTF16Ptr(message+"\n\n确定：打开文件夹\n取消：关闭"), title,
					flags|windows.MB_OKCANCEL|windows.MB_DEFBUTTON2)
				if response == 1 {
					selected = 100
				}
			}
		}
		if selected != 100 && selected != 101 {
			return
		}
		target := path
		var err error
		if !single {
			target = filepath.Dir(path)
			err = openReceivedFile(target, false)
		} else {
			err = openReceivedFile(path, selected == 100)
			if selected == 100 {
				target = filepath.Dir(path)
			}
		}
		if err != nil {
			log.Printf("open received file target %q: %v", target, err)
			_, _ = windows.MessageBox(0, windows.StringToUTF16Ptr("无法打开 "+filepath.Base(target)), title,
				windows.MB_OK|windows.MB_ICONERROR|windows.MB_SETFOREGROUND|windows.MB_TOPMOST)
		}
	}()
}

func receivedSizeLabel(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

var (
	receiveShell32             = windows.NewLazySystemDLL("shell32.dll")
	receiveParseDisplayName    = receiveShell32.NewProc("SHParseDisplayName")
	receiveOpenFolderAndSelect = receiveShell32.NewProc("SHOpenFolderAndSelectItems")
)

func OpenReceivedFile(path string) error { return openReceivedFile(path, false) }

func openReceivedFile(path string, reveal bool) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// Shell operations can invoke COM shell extensions in a single-threaded apartment.
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE); err != nil && err != syscall.Errno(windows.S_FALSE) {
		return fmt.Errorf("initialize COM: %w", err)
	}
	defer windows.CoUninitialize()

	if !reveal {
		return shellOpenReceivedPath(path)
	}
	revealErr := revealReceivedFile(path)
	if revealErr == nil {
		return nil
	}
	// The user may have moved or deleted the file while the dialog was open.
	if err := shellOpenReceivedPath(filepath.Dir(path)); err != nil {
		return errors.Join(revealErr, err)
	}
	return nil
}

func shellOpenReceivedPath(path string) error {
	target, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, windows.StringToUTF16Ptr("open"), target, nil, nil, windows.SW_SHOWNORMAL)
}

func revealReceivedFile(path string) error {
	target, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	var item unsafe.Pointer
	code, _, _ := receiveParseDisplayName.Call(uintptr(unsafe.Pointer(target)), 0, uintptr(unsafe.Pointer(&item)), 0, 0)
	if int32(code) < 0 {
		return fmt.Errorf("resolve received file in Explorer: HRESULT %#08x", uint32(code))
	}
	defer windows.CoTaskMemFree(item)
	// With zero child items, Explorer selects the absolute item in its parent folder.
	code, _, _ = receiveOpenFolderAndSelect.Call(uintptr(item), 0, 0, 0)
	if int32(code) < 0 {
		return fmt.Errorf("select received file in Explorer: HRESULT %#08x", uint32(code))
	}
	return nil
}

const controlsManifest = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <assemblyIdentity version="1.0.0.0" processorArchitecture="*" name="CodexRM.ReceiveDialog" type="win32"/>
  <dependency><dependentAssembly>
    <assemblyIdentity type="win32" name="Microsoft.Windows.Common-Controls" version="6.0.0.0" processorArchitecture="*" publicKeyToken="6595b64144ccf1df" language="*"/>
  </dependentAssembly></dependency>
</assembly>`

type activationContext struct {
	Size, Flags                        uint32
	Source                             *uint16
	Architecture, Language             uint16
	AssemblyDir, Resource, Application *uint16
	Module                             windows.Handle
}

const (
	taskDialogCreated = 0 // TDN_CREATED
	receiveRaiseTimer = 0x4344524d
	hwndTopmost       = ^uintptr(0)              // HWND_TOPMOST
	hwndNotTopmost    = ^uintptr(1)              // HWND_NOTOPMOST
	keepWindowBounds  = 0x0001 | 0x0002 | 0x0010 // SWP_NOSIZE | SWP_NOMOVE | SWP_NOACTIVATE
)

var (
	receiveUser32          = windows.NewLazySystemDLL("user32.dll")
	receiveSetWindowPos    = receiveUser32.NewProc("SetWindowPos")
	receiveSetTimer        = receiveUser32.NewProc("SetTimer")
	receiveKillTimer       = receiveUser32.NewProc("KillTimer")
	receiveDialogCallback  = windows.NewCallback(onReceiveDialog)
	receiveRestoreCallback = windows.NewCallback(restoreReceiveDialog)
)

func onReceiveDialog(hwnd, notification, _, _, _ uintptr) uintptr {
	if notification != taskDialogCreated {
		return 0
	}
	// A tray process may be denied foreground activation. Raise the dialog
	// briefly so it can appear above the current window without requesting focus.
	if ok, _, _ := receiveSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, keepWindowBounds); ok == 0 {
		return 0
	}
	// The timer runs on the dialog thread and is removed if the dialog closes early.
	if id, _, _ := receiveSetTimer.Call(hwnd, receiveRaiseTimer, 1000, receiveRestoreCallback); id == 0 {
		receiveSetWindowPos.Call(hwnd, hwndNotTopmost, 0, 0, 0, 0, keepWindowBounds)
	}
	return 0
}

func restoreReceiveDialog(hwnd, _, timerID, _ uintptr) uintptr {
	receiveKillTimer.Call(hwnd, timerID)
	receiveSetWindowPos.Call(hwnd, hwndNotTopmost, 0, 0, 0, 0, keepWindowBounds)
	return 0
}

func receivedTaskDialog(title, heading, content *uint16, buttons []taskDialogButton) (int32, bool) {
	manifest, err := os.CreateTemp("", "codexrm-controls-*.manifest")
	if err != nil {
		return 0, false
	}
	defer os.Remove(manifest.Name())
	_, writeErr := manifest.WriteString(controlsManifest)
	closeErr := manifest.Close()
	if writeErr != nil || closeErr != nil {
		return 0, false
	}

	// The activation context is thread-local and selects Common Controls v6.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	act := activationContext{Source: windows.StringToUTF16Ptr(manifest.Name())}
	act.Size = uint32(unsafe.Sizeof(act))
	handle, _, _ := kernel.NewProc("CreateActCtxW").Call(uintptr(unsafe.Pointer(&act)))
	if handle == ^uintptr(0) {
		return 0, false
	}
	defer kernel.NewProc("ReleaseActCtx").Call(handle)
	var cookie uintptr
	ok, _, _ := kernel.NewProc("ActivateActCtx").Call(handle, uintptr(unsafe.Pointer(&cookie)))
	if ok == 0 {
		return 0, false
	}
	defer kernel.NewProc("DeactivateActCtx").Call(0, cookie)
	module, err := windows.LoadLibraryEx("comctl32.dll", 0, windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		return 0, false
	}
	defer windows.FreeLibrary(module)
	taskDialog, err := windows.GetProcAddress(module, "TaskDialogIndirect")
	if err != nil {
		return 0, false
	}

	// CommCtrl.h declares TASKDIALOGCONFIG and TASKDIALOG_BUTTON with pack(1).
	appendPtr := func(buffer []byte, pointer uintptr) []byte {
		if unsafe.Sizeof(pointer) == 8 {
			return binary.LittleEndian.AppendUint64(buffer, uint64(pointer))
		}
		return binary.LittleEndian.AppendUint32(buffer, uint32(pointer))
	}
	// TaskDialogIndirect calls back into Go, which can grow and move the stack.
	// Keep all the buffers referenced by the native dialog at stable addresses.
	var pins runtime.Pinner
	defer pins.Unpin()
	pins.Pin(title)
	pins.Pin(heading)
	pins.Pin(content)
	var packedButtons []byte
	for _, button := range buttons {
		pins.Pin(button.Text)
		packedButtons = binary.LittleEndian.AppendUint32(packedButtons, uint32(button.ID))
		packedButtons = appendPtr(packedButtons, uintptr(unsafe.Pointer(button.Text)))
	}
	pins.Pin(&packedButtons[0])
	config := make([]byte, 4, 160)
	config = appendPtr(config, 0)                        // Owner window
	config = appendPtr(config, 0)                        // Resource instance
	config = binary.LittleEndian.AppendUint32(config, 8) // Allow close via Escape or title bar.
	config = binary.LittleEndian.AppendUint32(config, 0) // No standard buttons
	config = appendPtr(config, uintptr(unsafe.Pointer(title)))
	config = appendPtr(config, 0xfffd) // TD_INFORMATION_ICON
	config = appendPtr(config, uintptr(unsafe.Pointer(heading)))
	config = appendPtr(config, uintptr(unsafe.Pointer(content)))
	config = binary.LittleEndian.AppendUint32(config, uint32(len(buttons)))
	config = appendPtr(config, uintptr(unsafe.Pointer(&packedButtons[0])))
	config = binary.LittleEndian.AppendUint32(config, 102)
	config = binary.LittleEndian.AppendUint32(config, 0) // Radio button count
	config = appendPtr(config, 0)                        // Radio button array
	config = binary.LittleEndian.AppendUint32(config, 0) // Default radio button
	for range 6 {                                        // Verification, expanded text and footer fields.
		config = appendPtr(config, 0)
	}
	config = appendPtr(config, receiveDialogCallback)
	config = appendPtr(config, 0)                        // Callback data
	config = binary.LittleEndian.AppendUint32(config, 0) // Automatic width
	binary.LittleEndian.PutUint32(config, uint32(len(config)))
	var selected int32
	pins.Pin(&config[0])
	pins.Pin(&selected)
	code, _, _ := syscall.SyscallN(taskDialog, uintptr(unsafe.Pointer(&config[0])), uintptr(unsafe.Pointer(&selected)), 0, 0)
	return selected, int32(code) >= 0
}
