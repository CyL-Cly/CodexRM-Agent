package agenthost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const wxAdoptedUpgradeLabel = "com.codexrm.wxbot.upgrade-from"

type wxAdoptedClientUpgrade struct {
	wxUpgradeClient
	WasRunning bool
}

// Recreate only the known client of a verified account. Use its exact host
// configuration and mounts so external deployments keep their database and skills.
func (h *Host) wxUpgradeAdoptedClients(ctx context.Context, task *wxTask) error {
	journal := filepath.Join(h.StateDir(), "wx-adopted-clients-upgrade.json")
	rollback := func(rows []wxAdoptedClientUpgrade, cause error) error {
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		if err := wxRollbackAdoptedClients(recovery, rows); err != nil {
			return errors.Join(cause, err)
		}
		return errors.Join(cause, os.Remove(journal))
	}
	interrupted, err := readJSONFile[[]wxAdoptedClientUpgrade](journal)
	if err != nil {
		return err
	}
	if interrupted != nil {
		return rollback(*interrupted, errors.New("已尝试恢复中断的客户端升级，请确认服务状态后重试"))
	}
	images, err := wxImageLock()
	if err != nil {
		return err
	}
	image := images["client"]
	h.updateWXTask(task, func(row *wxTask) {
		row.Images = []wxImageProgress{{Name: "client", Image: image, Status: "pulling"}}
	})
	_, err = wxUpgradeDocker(ctx, "pull", image)
	h.updateWXTask(task, func(row *wxTask) {
		if err != nil {
			row.Images[0].Status, row.Images[0].Error = "failed", err.Error()
		} else {
			row.Images[0].Status = "completed"
		}
	})
	if err != nil {
		return fmt.Errorf("拉取接管客户端镜像失败: %w", err)
	}
	admin, err := h.wxAdmin(ctx)
	if err != nil {
		return err
	}
	rows, err := admin.robots(ctx)
	if err != nil {
		return err
	}
	var changed []wxAdoptedClientUpgrade
	for _, account := range rows {
		name := "client_" + account.Code
		if !wxRobotContainer.MatchString(name) {
			return wxAdoptedUpgradeFailure(rollback, changed, errors.New("机器人容器名称无效"))
		}
		existing, err := wxInspectUpgradeContainer(ctx, "name=^/"+name+"$")
		if err != nil {
			return wxAdoptedUpgradeFailure(rollback, changed, err)
		}
		if existing == nil || existing.Image == image {
			continue
		}
		if existing.Name != name {
			return wxAdoptedUpgradeFailure(rollback, changed, errors.New("机器人容器身份不匹配"))
		}
		request, running, err := wxAdoptedClientCreateRequest(ctx, existing.ID, account, image)
		if err != nil {
			return wxAdoptedUpgradeFailure(rollback, changed, err)
		}
		old := wxAdoptedClientUpgrade{wxUpgradeClient: wxUpgradeClient{existing.Name, existing.ID}, WasRunning: running}
		if err := writeJSONFile(journal, append(changed, old)); err != nil {
			return wxAdoptedUpgradeFailure(rollback, changed, err)
		}
		changed = append(changed, old)
		if running {
			if _, err := wxUpgradeDocker(ctx, "stop", old.ID); err != nil {
				return rollback(changed, err)
			}
		}
		if _, err := wxUpgradeDocker(ctx, "rename", old.ID, wxAdoptedPreviousName(old)); err != nil {
			return rollback(changed, err)
		}
		if err := wxCreateDockerClient(ctx, old.Name, request); err != nil {
			return rollback(changed, err)
		}
		if running {
			if _, err := wxUpgradeDocker(ctx, "start", old.Name); err != nil {
				return rollback(changed, err)
			}
			if err := waitWXClientCapabilities(ctx, old.Name, 90*time.Second); err != nil {
				return rollback(changed, err)
			}
		}
	}
	if len(changed) == 0 {
		return nil
	}
	if err := os.Remove(journal); err != nil {
		return rollback(changed, err)
	}
	for _, old := range changed {
		_, _ = wxUpgradeDocker(ctx, "rm", old.ID)
	}
	return nil
}

func wxAdoptedUpgradeFailure(rollback func([]wxAdoptedClientUpgrade, error) error, changed []wxAdoptedClientUpgrade, err error) error {
	if len(changed) == 0 {
		return err
	}
	return rollback(changed, err)
}

func wxAdoptedPreviousName(old wxAdoptedClientUpgrade) string {
	return old.Name + "-adopted-" + old.ID[:12]
}

func wxAdoptedClientCreateRequest(ctx context.Context, id string, account wxAdminRobot, image string) (map[string]any, bool, error) {
	raw, err := wxUpgradeDocker(ctx, "inspect", "--type=container", id)
	if err != nil {
		return nil, false, err
	}
	var objects []struct {
		ID, Name, Path  string
		Config          map[string]any
		HostConfig      json.RawMessage
		State           struct{ Running bool }
		Mounts          []struct{ Type, Destination string }
		NetworkSettings struct {
			Networks map[string]struct {
				IPAMConfig json.RawMessage
				Aliases    []string
			}
		}
	}
	if err := json.Unmarshal(raw, &objects); err != nil || len(objects) != 1 {
		return nil, false, errors.New("无法读取原客户端配置")
	}
	old := objects[0]
	network, connected := old.NetworkSettings.Networks["wechat-robot"]
	if old.ID != id || old.Name != "/client_"+account.Code || old.Path != "/app/wechat-robot-client" || old.Config == nil || !connected || len(old.NetworkSettings.Networks) != 1 {
		return nil, false, errors.New("原客户端的进程或网络布局不支持自动迁移")
	}
	// A recreated anonymous volume would lose data. Supported clients persist
	// skills through a bind mount and use an external account database.
	for _, mount := range old.Mounts {
		if mount.Type != "bind" {
			return nil, false, errors.New("原客户端包含需手动迁移的 Docker 卷")
		}
	}
	if volumes, _ := old.Config["Volumes"].(map[string]any); len(volumes) != 0 {
		return nil, false, errors.New("原客户端包含需手动迁移的 Docker 卷")
	}
	environment, _ := old.Config["Env"].([]any)
	expected := []string{"ROBOT_CODE=" + account.Code, "ROBOT_ID=" + strconv.FormatInt(account.ID, 10)}
	for _, key := range expected {
		found := false
		for _, entry := range environment {
			if entry == key {
				found = true
				break
			}
		}
		if !found {
			return nil, false, errors.New("原客户端实例与当前 wxbot 账号不匹配")
		}
	}
	config := old.Config
	labels, _ := config["Labels"].(map[string]any)
	if labels == nil {
		labels = map[string]any{}
	}
	labels[wxAdoptedUpgradeLabel] = id
	config["Labels"] = labels
	config["Image"] = image
	config["HostConfig"] = old.HostConfig
	// Existing aliases remain available. Docker supplies the original client name
	// when the replacement is created, so the protocol endpoint needs no update.
	endpoint := map[string]any{"Aliases": network.Aliases}
	if len(network.IPAMConfig) != 0 {
		endpoint["IPAMConfig"] = network.IPAMConfig
	}
	config["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{"wechat-robot": endpoint}}
	return config, old.State.Running, nil
}

func wxCreateDockerClient(ctx context.Context, name string, payload map[string]any) error {
	if !strings.HasPrefix(name, "client_") || !wxRobotContainer.MatchString(name) {
		return errors.New("机器人容器名称无效")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		dialer := net.Dialer{Timeout: 5 * time.Second}
		return dialer.DialContext(ctx, "unix", strings.TrimPrefix(wxDockerHost, "unix://"))
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/containers/create?"+url.Values{"name": {name}}.Encode(), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := transport.RoundTrip(request)
	if err != nil {
		return errors.New("无法创建替换客户端")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("无法创建替换客户端，Docker 返回 HTTP %d", response.StatusCode)
	}
	return nil
}

func wxRollbackAdoptedClients(ctx context.Context, clients []wxAdoptedClientUpgrade) error {
	for _, old := range clients {
		if !strings.HasPrefix(old.Name, "client_") || !wxRobotContainer.MatchString(old.Name) || !wxContainerID.MatchString(old.ID) {
			return errors.New("客户端恢复记录无效")
		}
		original, err := wxInspectUpgradeContainer(ctx, "id="+old.ID)
		if err != nil {
			return err
		}
		if original == nil || original.ID != old.ID {
			return errors.New("找不到原客户端容器")
		}
		if original.Name != old.Name {
			if original.Name != wxAdoptedPreviousName(old) {
				return errors.New("原客户端容器名称已变更")
			}
			replacement, err := wxInspectUpgradeContainer(ctx, "name=^/"+old.Name+"$")
			if err != nil {
				return err
			}
			if replacement != nil {
				label, err := wxUpgradeDocker(ctx, "inspect", "--type=container", "--format", `{{index .Config.Labels "com.codexrm.wxbot.upgrade-from"}}`, replacement.ID)
				if err != nil {
					return err
				}
				if strings.TrimSpace(string(label)) != old.ID {
					return errors.New("原客户端名称被另一容器占用")
				}
				if _, err := wxUpgradeDocker(ctx, "rm", "--force", replacement.ID); err != nil {
					return err
				}
			}
			if _, err := wxUpgradeDocker(ctx, "rename", old.ID, old.Name); err != nil {
				return err
			}
		}
		if old.WasRunning {
			if _, err := wxUpgradeDocker(ctx, "start", old.ID); err != nil {
				return err
			}
		}
	}
	return nil
}
