package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

func wxTakeoverLifecycleAction(action string) bool {
	return action == "start" || action == "stop" || action == "restart"
}

// Use Docker's recorded project identity and the verified account snapshot.
// Existing Compose files, bind mounts and account databases stay in place.
func (h *Host) wxTakeoverLifecycle(ctx context.Context, instance *wxInstance, action string) error {
	snapshot := instance.Snapshot
	if snapshot == nil || len(snapshot.Services) == 0 {
		return errors.New("请重新接管以识别 wxbot 服务")
	}
	type labels struct{ Project, Directory string }
	raw, err := wxUpgradeDocker(ctx, "inspect", "--type=container", "--format", `{{json .Config.Labels}}`, wxAdminContainer)
	if err != nil {
		return err
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	project := labels{values["com.docker.compose.project"], values["com.docker.compose.project.working_dir"]}
	if project.Project == "" || project.Directory == "" {
		return errors.New("找不到既有 wxbot 的 Compose 项目标识")
	}
	if action != "start" {
		// Refresh the account set before stopping the management API. A retry of an
		// interrupted stop uses the last verified set if that API is already down.
		if admin, err := h.wxAdmin(ctx); err == nil {
			rows, err := admin.robots(ctx)
			if err != nil {
				return err
			}
			snapshot.Robots = rows
			if err := h.saveWXInstance(instance); err != nil {
				return err
			}
		}
	}
	raw, err = wxUpgradeDocker(ctx, "ps", "-a", "--no-trunc", "--filter", "label=com.docker.compose.project="+project.Project, "--filter", "label=com.docker.compose.project.working_dir="+project.Directory, "--format", "{{.ID}} {{.Names}}")
	if err != nil {
		return err
	}
	allowed := make(map[string]bool, len(snapshot.Services))
	for _, service := range snapshot.Services {
		allowed[service.Name] = true
	}
	var public, adminIDs, entryIDs []string
	for _, line := range strings.Split(string(raw), "\n") {
		cols := strings.Fields(line)
		if len(cols) != 2 || !wxContainerID.MatchString(cols[0]) || !allowed[cols[1]] || wxRobotContainer.MatchString(cols[1]) {
			continue
		}
		switch cols[1] {
		case wxAdminContainer:
			adminIDs = append(adminIDs, cols[0])
		case "wechat-nginx":
			entryIDs = append(entryIDs, cols[0])
		default:
			public = append(public, cols[0])
		}
	}
	if len(adminIDs) != 1 {
		return errors.New("原管理服务已变更，请重新接管")
	}
	var clients, servers []string
	for _, account := range snapshot.Robots {
		for _, kind := range []string{"server", "client"} {
			name := kind + "_" + account.Code
			if !wxRobotContainer.MatchString(name) {
				return errors.New("机器人容器名称无效")
			}
			row, err := wxInspectUpgradeContainer(ctx, "name=^/"+name+"$")
			if err != nil {
				return err
			}
			if row == nil {
				continue
			}
			network, err := wxUpgradeDocker(ctx, "inspect", "--type=container", "--format", `{{with index .NetworkSettings.Networks "wechat-robot"}}{{.NetworkID}}{{end}}`, row.ID)
			if err != nil {
				return err
			}
			if !wxContainerID.MatchString(strings.TrimSpace(string(network))) {
				return errors.New("存在网络归属无法确认的机器人容器")
			}
			if kind == "server" {
				servers = append(servers, row.ID)
			} else {
				clients = append(clients, row.ID)
			}
		}
	}
	run := func(verb string, sets ...[]string) error {
		for _, ids := range sets {
			if len(ids) == 0 {
				continue
			}
			if _, err := wxUpgradeDocker(ctx, append([]string{verb}, ids...)...); err != nil {
				return err
			}
		}
		return nil
	}
	if action != "start" {
		if err := run("stop", clients, servers, entryIDs, adminIDs, public); err != nil {
			return err
		}
	}
	if action != "stop" {
		return run("start", public, adminIDs, entryIDs, servers, clients)
	}
	return nil
}
