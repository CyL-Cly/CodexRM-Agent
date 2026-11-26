package installation

import (
	"encoding/binary"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	phonePickerTitle = "CodexRM · 发送到手机"
	phoneIDSend      = 1 // IDOK
	phoneIDCancel    = 2 // IDCANCEL
	phoneIDFilename  = 100
	phoneIDCombo     = 101

	wmClose      = 0x0010
	wmInitDialog = 0x0110
	wmCommand    = 0x0111
	cbAddString  = 0x0143
	cbGetCurSel  = 0x0147
	cbSetCurSel  = 0x014e
)

var (
	phonePickerUser32 = windows.NewLazySystemDLL("user32.dll")
	phoneDialogBox    = phonePickerUser32.NewProc("DialogBoxIndirectParamW")
	phoneSetItemText  = phonePickerUser32.NewProc("SetDlgItemTextW")
	phoneSendItem     = phonePickerUser32.NewProc("SendDlgItemMessageW")
	phoneEndDialog    = phonePickerUser32.NewProc("EndDialog")
	phonePickerProc   = windows.NewCallback(onPhonePicker)
	phonePickerNextID atomic.Uintptr
	phonePickerInputs sync.Map // uintptr -> phonePickerInput (during WM_INITDIALOG)
)

type phonePickerInput struct {
	filename string
	names    []string
}

func ChoosePhoneFileTarget(filename string, names []string) (int, error) {
	if len(names) == 0 {
		return -1, nil
	}
	var module windows.Handle
	if err := windows.GetModuleHandleEx(windows.GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT, nil, &module); err != nil {
		return -1, err
	}
	id := phonePickerNextID.Add(1)
	phonePickerInputs.Store(id, phonePickerInput{filename, names})
	defer phonePickerInputs.Delete(id)

	template := phonePickerTemplate()
	// DialogBox calls back into Go. Keep the template at a stable address.
	var pin runtime.Pinner
	pin.Pin(&template[0])
	defer pin.Unpin()
	result, _, _ := phoneDialogBox.Call(uintptr(module), uintptr(unsafe.Pointer(&template[0])), 0, phonePickerProc, id)
	if result == 0 {
		return -1, nil // Cancel, Escape or close button.
	}
	if result > uintptr(len(names)) {
		return -1, errors.New("无法打开手机选择窗口")
	}
	return int(result) - 1, nil
}

func onPhonePicker(hwnd, message, wparam, lparam uintptr) uintptr {
	switch message {
	case wmInitDialog:
		raw, found := phonePickerInputs.LoadAndDelete(lparam)
		if !found || !fillPhonePicker(hwnd, raw.(phonePickerInput)) {
			phoneEndDialog.Call(hwnd, ^uintptr(0))
		}
		return 1 // Let Windows focus the first tab-stop (the drop-down).
	case wmCommand:
		if uint16(wparam>>16) == 0 { // Button click or keyboard equivalent.
			switch uint16(wparam) {
			case phoneIDSend:
				selected, _, _ := phoneSendItem.Call(hwnd, phoneIDCombo, cbGetCurSel, 0, 0)
				if int32(selected) >= 0 {
					phoneEndDialog.Call(hwnd, selected+1)
				}
				return 1
			case phoneIDCancel:
				phoneEndDialog.Call(hwnd, 0)
				return 1
			}
		}
	case wmClose:
		phoneEndDialog.Call(hwnd, 0)
		return 1
	}
	return 0
}

func fillPhonePicker(hwnd uintptr, input phonePickerInput) bool {
	filename, err := windows.UTF16PtrFromString("文件：" + input.filename)
	if err != nil {
		return false
	}
	ok, _, _ := phoneSetItemText.Call(hwnd, phoneIDFilename, uintptr(unsafe.Pointer(filename)))
	if ok == 0 {
		return false
	}
	for _, name := range input.names {
		text, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return false
		}
		index, _, _ := phoneSendItem.Call(hwnd, phoneIDCombo, cbAddString, 0, uintptr(unsafe.Pointer(text)))
		if int32(index) < 0 {
			return false
		}
	}
	selected, _, _ := phoneSendItem.Call(hwnd, phoneIDCombo, cbSetCurSel, 0, 0)
	return selected == 0
}

func phonePickerTemplate() []byte {
	const (
		wsPopup       = 0x80000000
		wsChild       = 0x40000000
		wsVisible     = 0x10000000
		wsCaption     = 0x00c00000
		wsVScroll     = 0x00200000
		wsSysMenu     = 0x00080000
		wsTabStop     = 0x00010000
		dsModalFrame  = 0x0004
		dsSetFont     = 0x0040
		dsCenter      = 0x0800
		ssNoPrefix    = 0x0080
		ssEndEllipsis = 0x4000
		cbsDropList   = 0x0003
		bsDefaultPush = 0x0001
		buttonClass   = 0x0080
		staticClass   = 0x0082
		comboClass    = 0x0085
	)
	controls := []struct {
		class, id, x, y, width, height uint16 // Bounds are in dialog units.
		style                          uint32
		text                           string
	}{
		{staticClass, phoneIDFilename, 12, 11, 266, 12, ssNoPrefix | ssEndEllipsis, ""},
		{staticClass, 0xffff, 12, 27, 266, 10, 0, "选择接收文件的手机(&P)："},
		{comboClass, phoneIDCombo, 12, 40, 266, 135, wsTabStop | wsVScroll | cbsDropList, ""},
		{buttonClass, phoneIDSend, 166, 67, 53, 15, wsTabStop | bsDefaultPush, "发送(&S)"},
		{buttonClass, phoneIDCancel, 225, 67, 53, 15, wsTabStop, "取消"},
	}

	// DLGTEMPLATE and each DLGITEMTEMPLATE begin at a DWORD boundary.
	var buf []byte
	word := func(values ...uint16) {
		for _, value := range values {
			buf = binary.LittleEndian.AppendUint16(buf, value)
		}
	}
	style := func(value uint32) {
		buf = binary.LittleEndian.AppendUint32(buf, value)
		buf = binary.LittleEndian.AppendUint32(buf, 0) // Extended style.
	}
	text := func(value string) { word(windows.StringToUTF16(value)...) }
	style(wsPopup | wsCaption | wsSysMenu | dsModalFrame | dsSetFont | dsCenter)
	word(uint16(len(controls)), 0, 0, 290, 91, 0, 0) // No menu or custom window class.
	text(phonePickerTitle)
	word(9) // Font point size, as required by DS_SETFONT.
	text("Segoe UI")
	for _, c := range controls {
		if len(buf)%4 != 0 {
			word(0)
		}
		style(wsChild | wsVisible | c.style)
		word(c.x, c.y, c.width, c.height, c.id, 0xffff, c.class)
		text(c.text)
		word(0) // No control creation data.
	}
	return buf
}
