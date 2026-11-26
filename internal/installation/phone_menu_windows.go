package installation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const phoneMenuKey = `Software\Classes\*\shell\CodexRM.SendToPhone`

func phoneMenuCommand(binary string) string { return `"` + binary + `" --send-to-phone "%1"` }

// The single-selection verb is scoped to files, not folders or background clicks.
func RegisterPhoneMenu(binary string) error {
	if !filepath.IsAbs(binary) || strings.ContainsAny(binary, `"%`) {
		return errors.New("无效的 Agent 程序路径")
	}
	if IsPackaged() || (os.Getenv("CODEXRM_INSTALL_TEST_ROOT") != "") {
		return nil
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, phoneMenuKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	for name, value := range map[string]string{"MUIVerb": "发送到手机", "MultiSelectModel": "Single", "Icon": binary} {
		if err := key.SetStringValue(name, value); err != nil {
			return err
		}
	}
	command, _, err := registry.CreateKey(registry.CURRENT_USER, phoneMenuKey+`\command`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer command.Close()
	return command.SetStringValue("", phoneMenuCommand(binary))
}

func unregisterPhoneMenu(binary string) error {
	if os.Getenv("CODEXRM_INSTALL_TEST_ROOT") != "" {
		return nil
	}
	command, err := registry.OpenKey(registry.CURRENT_USER, phoneMenuKey+`\command`, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	value, _, err := command.GetStringValue("")
	command.Close()
	if err != nil || !strings.EqualFold(value, phoneMenuCommand(binary)) {
		return err
	}
	if err := registry.DeleteKey(registry.CURRENT_USER, phoneMenuKey+`\command`); err != nil {
		return err
	}
	return registry.DeleteKey(registry.CURRENT_USER, phoneMenuKey)
}

func ShowPhoneQueued(name, phone string) {
	text, _ := windows.UTF16PtrFromString(fmt.Sprintf("%s 已加入发往 %s 的缓存。\n手机与此电脑建立 P2P 后可接收。", name, phone))
	if text != nil {
		_, _ = windows.MessageBox(0, text, windows.StringToUTF16Ptr("CodexRM · 文件传输助手"), windows.MB_OK|windows.MB_ICONINFORMATION|windows.MB_SETFOREGROUND)
	}
}
