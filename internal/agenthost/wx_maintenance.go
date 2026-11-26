package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var wxBackupName = regexp.MustCompile(`^wxbot-[0-9]{8}T[0-9]{6}Z-[a-f0-9]{16}.tar$`)
var wxRobotContainer = regexp.MustCompile(`^(client|server)_[A-Za-z0-9]{16}$`)

func (h *Host) wxBackupsDir() string { return filepath.Join(h.StateDir(), "wxbot-backups") }

func (h *Host) wxMaintenance(w http.ResponseWriter, r *http.Request) {
	instance := h.loadWXInstance()
	managed := instance != nil && instance.Mode == "installed" && instance.ComposeDir == h.wxInstallDirectory() && wxOwnsCompose(instance.ComposeDir)
	adopted := instance != nil && instance.Mode == "taken-over" && instance.AdminVerified
	switch r.Method {
	case http.MethodGet:
		files, _ := os.ReadDir(h.wxBackupsDir())
		backups := make([]string, 0, len(files))
		for i := len(files) - 1; i >= 0; i-- {
			if files[i].Type().IsRegular() && wxBackupName.MatchString(files[i].Name()) {
				backups = append(backups, files[i].Name())
			}
		}
		respond(w, 200, map[string]any{"managed": managed, "adopted": adopted, "backups": backups})
	case http.MethodPost:
		if !managed && !adopted {
			failure(w, errors.New("请先安装或接管 wxbot"))
			return
		}
		var input struct {
			Action, Backup, ConfirmText string
			Confirm                     bool
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			failure(w, errors.New("操作参数无效"))
			return
		}
		labels := map[string]string{"start": "启动服务", "stop": "停止服务", "restart": "重启服务", "backup": "备份数据", "restore": "恢复备份", "uninstall": "卸载并保留数据", "purge": "卸载并清除安装数据", "upgrade": "升级公共服务和机器人客户端", "upgrade-adopted-clients": "升级已接管的机器人客户端"}
		name, ok := labels[input.Action]
		if !ok || ((input.Action == "restore" || input.Action == "uninstall" || input.Action == "upgrade" || input.Action == "upgrade-adopted-clients") && !input.Confirm) ||
			(input.Action == "purge" && input.ConfirmText != "清除微信机器人数据") {
			failure(w, errors.New("请确认操作"))
			return
		}
		if adopted && !wxTakeoverLifecycleAction(input.Action) && input.Action != "upgrade-adopted-clients" || managed && input.Action == "upgrade-adopted-clients" {
			failure(w, errors.New("该安装的备份、恢复、升级及卸载由原部署工具管理"))
			return
		}
		if input.Action == "restore" && !wxBackupName.MatchString(input.Backup) {
			failure(w, errors.New("请选择备份"))
			return
		}
		task, err := h.beginWXTask("maintenance", []wxTaskStep{{ID: "maintenance", Name: name, Status: "pending"}}, instance.ComposeDir, func(row *wxTask) {
			row.Action, row.Backup = input.Action, input.Backup
		})
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, task)
		h.startWXTask(task, h.runWXMaintenance)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *Host) runWXMaintenance(ctx context.Context, task *wxTask) error {
	if err := h.updateWXTask(task, func(row *wxTask) { setWXStep(row, "maintenance", "running", "") }); err != nil {
		return err
	}
	if instance := h.loadWXInstance(); instance != nil && instance.Mode == "taken-over" {
		if instance.ComposeDir != task.InstallDir || !instance.AdminVerified {
			return errors.New("未找到已接管的服务")
		}
		var err error
		if task.Action == "upgrade-adopted-clients" {
			err = h.wxUpgradeAdoptedClients(ctx, task)
		} else if wxTakeoverLifecycleAction(task.Action) {
			err = h.wxTakeoverLifecycle(ctx, instance, task.Action)
		} else {
			err = errors.New("未找到已接管的服务")
		}
		if err != nil {
			return err
		}
		return h.updateWXTask(task, func(row *wxTask) { setWXStep(row, "maintenance", "completed", "") })
	}
	dir := h.wxInstallDirectory()
	if dir != task.InstallDir {
		return errors.New("找不到 CodexRM 管理的数据目录")
	}
	if err := wxRecoverRestoreSwap(dir); err != nil {
		return err
	}
	if !wxOwnsCompose(dir) && task.Action != "purge" {
		return errors.New("找不到 CodexRM 管理的数据目录")
	}
	stop := func() error {
		if err := wxDynamicContainers(ctx, "stop"); err != nil {
			return err
		}
		return wxCompose(ctx, dir, "stop")
	}
	start := func() error {
		if err := wxCompose(ctx, dir, "up", "-d", "--pull", "never"); err != nil {
			return err
		}
		return wxDynamicContainers(ctx, "start")
	}
	var err error
	switch task.Action {
	case "start":
		err = start()
	case "stop":
		err = stop()
	case "restart":
		if err = stop(); err == nil {
			err = start()
		}
	case "uninstall":
		if err = stop(); err == nil {
			err = wxCompose(ctx, dir, "down")
		}
	case "purge":
		err = h.wxPurge(ctx, dir)
	case "backup", "restore", "upgrade":
		if err = stop(); err != nil {
			break
		}
		switch task.Action {
		case "backup":
			_, err = h.wxBackup(ctx, dir)
		case "restore":
			err = h.wxRestore(ctx, dir, task.Backup)
		case "upgrade":
			err = h.wxUpgrade(ctx, dir, task)
		}
		// An unfinished upgrade journal means rollback could not be completed.
		// Leave the stack stopped so that retry can recover the previous data.
		if _, journalErr := os.Lstat(h.wxUpgradeJournalPath()); !os.IsNotExist(journalErr) {
			break
		}
		err = errors.Join(err, start())
	default:
		err = errors.New("未知的维护操作")
	}
	if err == nil {
		return h.updateWXTask(task, func(row *wxTask) { setWXStep(row, "maintenance", "completed", "") })
	}
	return err
}

// Purge acts only on the managed installation directory. The archive and any
// user-created backups are separate from live service data.
func (h *Host) wxPurge(ctx context.Context, dir string) error {
	if _, err := os.Stat(h.wxUpgradeJournalPath()); !os.IsNotExist(err) {
		return errors.New("请先恢复中断的升级")
	}
	staging := dir + "-purging"
	imageState := filepath.Join(h.StateDir(), "wx-purge-image.json")
	if wxOwnsCompose(dir) {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() {
			return errors.New("无法确认本机安装目录")
		}
		if _, err := os.Lstat(staging); !os.IsNotExist(err) {
			return errors.New("清除目录已存在")
		}
		image, err := wxBackupImage(dir)
		if err != nil {
			return err
		}
		if err := writeJSONFile(imageState, image); err != nil {
			return err
		}
		if err := wxDynamicContainers(ctx, "rm"); err != nil {
			return err
		}
		if err := wxCompose(ctx, dir, "down"); err != nil {
			return err
		}
		if err := os.Rename(dir, staging); err != nil {
			return err
		}
	}
	if info, err := os.Lstat(staging); err == nil {
		image, imageErr := readJSONFile[string](imageState)
		if !info.IsDir() || imageErr != nil || image == nil || !wxImageRef.MatchString(*image) || strings.Contains(staging, ",") {
			return errors.New("无法验证待清除的数据目录")
		}
		docker, err := wxLookPath("docker")
		if err != nil {
			return err
		}
		args := wxDockerArgs("run", "--rm", "--pull=never", "--network=none", "--read-only", "--mount",
			"type=bind,src="+staging+",dst=/cleanup,bind-recursive=disabled", "--entrypoint", "find", *image,
			"/cleanup", "-mindepth", "1", "-maxdepth", "1", "-exec", "rm", "-rf", "--", "{}", ";")
		if out, err := wxCommandOutput(ctx, docker, args...); err != nil {
			return fmt.Errorf("清除安装数据失败: %s", strings.TrimSpace(string(out)))
		}
		if err := os.Remove(staging); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, path := range []string{h.wxAdminSecretPath(), h.wxInstancePath(), imageState} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func wxDynamicContainers(ctx context.Context, action string) error {
	docker, err := wxLookPath("docker")
	if err != nil {
		return err
	}
	raw, err := wxCommandOutput(ctx, docker, wxDockerArgs("ps", "-a", "--filter", "label=com.codexrm.wxbot.managed=true", "--format", "{{.Names}}")...)
	if err != nil {
		return errors.New("无法读取已管理的机器人容器")
	}
	args := []string{action}
	for _, name := range strings.Fields(string(raw)) {
		if wxRobotContainer.MatchString(name) {
			args = append(args, name)
		}
	}
	if len(args) == 1 {
		return nil
	}
	if action == "rm" {
		args = append([]string{"rm", "--force"}, args[1:]...)
	}
	out, err := wxCommandOutput(ctx, docker, wxDockerArgs(args...)...)
	if err != nil {
		return fmt.Errorf("机器人容器操作失败: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (h *Host) wxBackup(ctx context.Context, dir string) (string, error) {
	backupDir := h.wxBackupsDir()
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		return "", err
	}
	image, err := wxBackupImage(dir)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(backupDir, "incoming-")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = wxRunArchive(ctx, image, dir, false, file); err != nil {
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	name := "wxbot-" + time.Now().UTC().Format("20060102T150405Z") + "-" + newWXTaskID() + ".tar"
	if err = os.Rename(file.Name(), filepath.Join(backupDir, name)); err != nil {
		return "", err
	}
	return name, nil
}

func (h *Host) wxRestore(ctx context.Context, dir, name string) error {
	if !wxBackupName.MatchString(name) {
		return errors.New("备份名称无效")
	}
	image, err := wxBackupImage(dir)
	if err != nil {
		return err
	}
	filename := filepath.Join(h.wxBackupsDir(), name)
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("备份不存在")
	}
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	staging, err := os.MkdirTemp(filepath.Dir(dir), "wxbot-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err = wxRunArchive(ctx, image, staging, true, file); err != nil {
		return err
	}
	if !wxOwnsCompose(staging) {
		return errors.New("备份缺少 wxbot 管理标记")
	}
	if err = wxRecoverRestoreSwap(dir); err != nil {
		return err
	}
	token, err := wxInstallLoginToken(staging)
	if err != nil {
		return err
	}
	previous := dir + "-restoring"
	if err = os.Rename(dir, previous); err != nil {
		return err
	}
	if err = os.Rename(staging, dir); err != nil {
		return errors.Join(err, os.Rename(previous, dir))
	}
	if err := writeJSONFile(h.wxAdminSecretPath(), token); err != nil {
		return errors.Join(err, os.Rename(dir, staging), os.Rename(previous, dir))
	}
	return wxRecoverRestoreSwap(dir)
}

// A crash between the two renames leaves the previous directory at a known path.
func wxRecoverRestoreSwap(dir string) error {
	previous := dir + "-restoring"
	if _, err := os.Lstat(previous); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if !wxOwnsCompose(previous) {
		return errors.New("无法验证待恢复的数据目录")
	}
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return os.Rename(previous, dir)
	} else if err != nil {
		return err
	}
	if !wxOwnsCompose(dir) {
		return errors.New("无法验证恢复后的数据目录")
	}
	return os.Rename(previous, dir+"-previous-"+newWXTaskID())
}

func (h *Host) wxUpgradeJournalPath() string { return filepath.Join(h.StateDir(), "wx-upgrade.json") }

type wxUpgradeState struct {
	Backup  string
	Clients []wxUpgradeClient
}

type wxUpgradeClient struct {
	Name, ID string
}

func (h *Host) wxUpgrade(ctx context.Context, dir string, task *wxTask) error {
	journal := h.wxUpgradeJournalPath()
	rollback := func(state wxUpgradeState, cause error) error {
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		if err := wxCompose(recovery, dir, "stop"); err != nil {
			return errors.Join(cause, err)
		}
		if err := wxDynamicContainers(recovery, "stop"); err != nil {
			return errors.Join(cause, err)
		}
		if err := wxRollbackUpgradeClients(recovery, state.Clients); err != nil {
			return errors.Join(cause, err)
		}
		if err := h.wxRestore(recovery, dir, state.Backup); err != nil {
			return errors.Join(cause, err)
		}
		return errors.Join(cause, os.Remove(journal))
	}
	interrupted, err := readJSONFile[wxUpgradeState](journal)
	if err != nil {
		// Earlier Agents stored only the backup filename.
		old, readErr := readJSONFile[string](journal)
		if readErr != nil || old == nil || !wxBackupName.MatchString(*old) {
			return err
		}
		interrupted = &wxUpgradeState{Backup: *old}
	}
	if interrupted != nil {
		return rollback(*interrupted, errors.New("已尝试恢复中断的升级，请确认服务状态后重试"))
	}
	images, err := wxImageLock()
	if err != nil {
		return err
	}
	backup, err := h.wxBackup(ctx, dir)
	if err != nil {
		return err
	}
	state := wxUpgradeState{Backup: backup}
	if err := writeJSONFile(journal, state); err != nil {
		return err
	}
	compose := filepath.Join(dir, "docker-compose.yml")
	previous, err := os.ReadFile(compose)
	optionalServices := wxExistingOptionalServices(string(previous))
	if err == nil {
		err = os.WriteFile(compose, []byte(wxRenderCompose(images, optionalServices)), 0600)
	}
	if err == nil {
		err = h.pullWXImages(ctx, task, images, optionalServices)
	}
	if err != nil {
		// No candidate has started, so preserve the live data and restore only Compose.
		if len(previous) > 0 {
			if restoreErr := os.WriteFile(compose, previous, 0600); restoreErr != nil {
				return errors.Join(err, restoreErr)
			}
		}
		return errors.Join(err, os.Remove(journal))
	}
	if err := wxCompose(ctx, dir, "up", "-d", "--pull", "never"); err != nil {
		return rollback(state, err)
	}
	token, err := wxInstallLoginToken(dir)
	if err == nil {
		var client *wxAdminClient
		client, err = waitWXHealthy(ctx, token, 90*time.Second)
		if err == nil {
			err = wxUpgradeClients(ctx, client, images["client"], &state, journal)
		}
	}
	if err == nil {
		err = writeJSONFile(filepath.Join(dir, "images.json"), images)
	}
	if err != nil {
		return rollback(state, err)
	}
	if err := os.Remove(journal); err != nil {
		return rollback(state, err)
	}
	for _, old := range state.Clients {
		// The journal is already committed. A cleanup failure leaves a stopped
		// container, whose name excludes it from the normal maintenance set.
		_, _ = wxUpgradeDocker(ctx, "rm", old.ID)
	}
	return nil
}

func wxBackupImage(dir string) (string, error) {
	images, err := readJSONFile[map[string]string](filepath.Join(dir, "images.json"))
	if err != nil {
		return "", err
	}
	if images == nil || !wxImageRef.MatchString((*images)["backup"]) {
		return "", errors.New("缺少备份工具镜像")
	}
	return (*images)["backup"], nil
}

func wxRunArchive(ctx context.Context, image, dir string, restore bool, file *os.File) error {
	if strings.Contains(dir, ",") {
		return errors.New("数据目录不能包含逗号")
	}
	docker, err := wxLookPath("docker")
	if err != nil {
		return err
	}
	mount := "type=bind,src=" + dir + ",dst=/backup"
	mode := "-cf"
	if restore {
		mode = "-xf"
	} else {
		mount += ",readonly"
	}
	args := wxDockerArgs("run", "--rm", "-i", "--pull=never", "--network=none", "--read-only", "--mount", mount, "--entrypoint", "tar", image, mode, "-", "-C", "/backup")
	if !restore {
		args = append(args, ".")
	}
	cmd := exec.CommandContext(ctx, docker, args...)
	var output strings.Builder
	cmd.Stderr = &output
	if restore {
		cmd.Stdin = file
	} else {
		cmd.Stdout = file
	}
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("备份归档失败: %w (%s)", err, output.String())
	}
	return nil
}
