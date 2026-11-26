package installation

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
	"golang.org/x/sys/windows"
)

func TestNativeInstallationCreatesOnlyDirectRestartShortcut(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	testRoot := t.TempDir()
	t.Setenv("CODEXRM_INSTALL_TEST_ROOT", testRoot)
	root := filepath.Join(t.TempDir(), "原生 Agent [test]")
	self, _ := os.Executable()
	if err := Install(context.Background(), self, root, Options{NoAutostart: true, NoStart: true}); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(filepath.Join(testRoot, "Desktop"))
	if err != nil || len(files) != 1 || files[0].Name() != "CodexRM 重启服务.lnk" {
		t.Fatalf("desktop entries: %v, %v", files, err)
	}
	target, args := readShortcut(t, filepath.Join(testRoot, "Desktop", files[0].Name()))
	wantTarget := filepath.Join(root, "bin", "codexrm-agent.exe")
	targetInfo, targetErr := os.Stat(target)
	wantInfo, wantErr := os.Stat(wantTarget)
	// Shell links may expand short paths or normalize path casing.
	if targetErr != nil || wantErr != nil || !os.SameFile(targetInfo, wantInfo) || args != "--restart" {
		t.Fatalf("shortcut target=%q (stat: %v), want=%q (stat: %v), args=%q", target, targetErr, wantTarget, wantErr, args)
	}
	if _, err := os.Stat(filepath.Join(root, "scripts")); !os.IsNotExist(err) {
		t.Fatal("native installation contains a script directory")
	}
	cfg, err := clientconfig.Load(filepath.Join(root, ".state", "connection.json"))
	if err != nil || cfg.Server != clientconfig.DefaultRelayServer || cfg.EnrollmentKey != "" {
		t.Fatalf("new installation should use the public relay without a key: %+v, %v", cfg, err)
	}
	cfg.Server, cfg.EnrollmentKey = "https://relay.example", "keep-secret"
	if err := clientconfig.Save(filepath.Join(root, ".state", "connection.json"), cfg); err != nil {
		t.Fatal(err)
	}
	if err := Install(context.Background(), self, root, Options{NoAutostart: true, NoStart: true}); err != nil {
		t.Fatal(err)
	}
	cfg, err = clientconfig.Load(filepath.Join(root, ".state", "connection.json"))
	if err != nil || cfg.EnrollmentKey != "keep-secret" || cfg.Server != "https://relay.example" {
		t.Fatal("reinstallation lost connection settings")
	}
	if err := Uninstall(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("uninstall left the installation directory")
	}
}

func readShortcut(t *testing.T, path string) (string, string) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ole := windows.NewLazySystemDLL("ole32.dll")
	r, _, _ := ole.NewProc("CoInitializeEx").Call(0, 2)
	if int32(r) < 0 {
		t.Fatalf("COM init: 0x%x", r)
	}
	defer ole.NewProc("CoUninitialize").Call()
	clsid, _ := windows.GUIDFromString("{00021401-0000-0000-C000-000000000046}")
	iid, _ := windows.GUIDFromString("{000214F9-0000-0000-C000-000000000046}")
	var link *comObject
	r, _, _ = ole.NewProc("CoCreateInstance").Call(uintptr(unsafe.Pointer(&clsid)), 0, 1, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&link)))
	if int32(r) < 0 {
		t.Fatalf("COM link: 0x%x", r)
	}
	defer comCall(link, 2)
	persistID, _ := windows.GUIDFromString("{0000010B-0000-0000-C000-000000000046}")
	var persist *comObject
	if err := comCall(link, 0, uintptr(unsafe.Pointer(&persistID)), uintptr(unsafe.Pointer(&persist))); err != nil {
		t.Fatal(err)
	}
	defer comCall(persist, 2)
	ptr, _ := windows.UTF16PtrFromString(path)
	if err := comCall(persist, 5, uintptr(unsafe.Pointer(ptr)), 0); err != nil {
		t.Fatal(err)
	}
	target, args := make([]uint16, 32768), make([]uint16, 32768)
	if err := comCall(link, 3, uintptr(unsafe.Pointer(&target[0])), uintptr(len(target)), 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := comCall(link, 10, uintptr(unsafe.Pointer(&args[0])), uintptr(len(args))); err != nil {
		t.Fatal(err)
	}
	return windows.UTF16ToString(target), windows.UTF16ToString(args)
}
