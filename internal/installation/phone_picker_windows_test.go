package installation

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Opt in on an interactive desktop: CODEXRM_TEST_PHONE_PICKER=1.
func TestPhonePickerDialog(t *testing.T) {
	if os.Getenv("CODEXRM_TEST_PHONE_PICKER") != "1" {
		t.Skip("requires an interactive Windows desktop")
	}
	for _, tc := range []struct {
		name     string
		names    []string
		selected uintptr
		message  uintptr
		button   uintptr
		want     int
	}{
		{"send to second", []string{"[P2P 已连接] 手机甲", "手机乙", "手机丙"}, 1, wmCommand, phoneIDSend, 1},
		{"single target", []string{"手机甲"}, 0, wmCommand, phoneIDSend, 0},
		{"cancel", []string{"手机甲"}, 0, wmCommand, phoneIDCancel, -1},
		{"close", []string{"手机甲"}, 0, wmClose, 0, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				hwnd := findTestPhonePicker()
				if hwnd == 0 {
					done <- fmt.Errorf("no phone picker dialog found")
					return
				}
				send := phonePickerUser32.NewProc("SendMessageW")
				if err := checkTestPhonePicker(hwnd, tc.names, tc.selected); err != nil {
					send.Call(hwnd, wmClose, 0, 0)
					done <- err
					return
				}
				send.Call(hwnd, tc.message, tc.button, 0)
				done <- nil
			}()
			got, err := ChoosePhoneFileTarget("测试 & 文件.txt", tc.names)
			if err != nil || got != tc.want {
				t.Errorf("picker result = (%d, %v), want (%d, nil)", got, err, tc.want)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func findTestPhonePicker() uintptr {
	find := phonePickerUser32.NewProc("FindWindowExW")
	visible := phonePickerUser32.NewProc("IsWindowVisible")
	windowPID := phonePickerUser32.NewProc("GetWindowThreadProcessId")
	title := windows.StringToUTF16Ptr(phonePickerTitle)
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		var hwnd uintptr
		for {
			hwnd, _, _ = find.Call(0, hwnd, 0, uintptr(unsafe.Pointer(title)))
			if hwnd == 0 {
				break
			}
			var pid uint32
			windowPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
			if ok, _, _ := visible.Call(hwnd); ok != 0 && pid == uint32(os.Getpid()) {
				return hwnd
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0
}

func checkTestPhonePicker(hwnd uintptr, names []string, selected uintptr) error {
	const cbGetCount = 0x0146
	const cbGetText = 0x0148
	count, _, _ := phoneSendItem.Call(hwnd, phoneIDCombo, cbGetCount, 0, 0)
	if count != uintptr(len(names)) {
		return fmt.Errorf("combo has %d items, want %d", count, len(names))
	}
	got := make([]string, count)
	for i := range got {
		buf := make([]uint16, 128)
		phoneSendItem.Call(hwnd, phoneIDCombo, cbGetText, uintptr(i), uintptr(unsafe.Pointer(&buf[0])))
		got[i] = windows.UTF16ToString(buf)
	}
	if !slices.Equal(got, names) {
		return fmt.Errorf("combo items = %q, want %q", got, names)
	}
	current, _, _ := phoneSendItem.Call(hwnd, phoneIDCombo, cbGetCurSel, 0, 0)
	if current != 0 {
		return fmt.Errorf("default selection = %d, want 0", current)
	}
	phoneSendItem.Call(hwnd, phoneIDCombo, cbSetCurSel, selected, 0)
	return nil
}
