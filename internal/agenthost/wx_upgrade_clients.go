package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var wxContainerID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func wxUpgradeDocker(ctx context.Context, args ...string) ([]byte, error) {
	docker, err := wxLookPath("docker")
	if err != nil {
		return nil, err
	}
	out, err := wxCommandOutput(ctx, docker, wxDockerArgs(args...)...)
	if err != nil {
		return nil, fmt.Errorf("Docker %s 操作失败", args[0])
	}
	return out, nil
}

type wxUpgradeContainer struct {
	ID, Name, Image string
	Managed         bool
}

func wxInspectUpgradeContainer(ctx context.Context, selector string) (*wxUpgradeContainer, error) {
	out, err := wxUpgradeDocker(ctx, "ps", "-a", "--no-trunc", "--filter", selector, "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, nil
	}
	var raw struct{ ID, Names, Image, Labels string }
	if json.Unmarshal(out, &raw) != nil || !wxContainerID.MatchString(raw.ID) {
		return nil, errors.New("无法确认机器人容器身份")
	}
	return &wxUpgradeContainer{raw.ID, raw.Names, raw.Image, strings.Contains(","+raw.Labels+",", ",com.codexrm.wxbot.managed=true,")}, nil
}

func wxUpgradeClients(ctx context.Context, admin *wxAdminClient, image string, state *wxUpgradeState, journal string) error {
	robots, err := admin.robots(ctx)
	if err != nil {
		return err
	}
	for _, robot := range robots {
		name := "client_" + robot.Code
		if !wxRobotContainer.MatchString(name) {
			return errors.New("机器人容器名称无效")
		}
		existing, err := wxInspectUpgradeContainer(ctx, "name=^/"+name+"$")
		if err != nil {
			return err
		}
		if existing == nil || !existing.Managed || existing.Image == image {
			continue
		}
		server, err := wxInspectUpgradeContainer(ctx, "name=^/server_"+robot.Code+"$")
		if err != nil {
			return err
		}
		if server == nil || !server.Managed {
			return fmt.Errorf("%s 缺少受管协议容器", name)
		}
		state.Clients = append(state.Clients, wxUpgradeClient{name, existing.ID})
		if err := writeJSONFile(journal, state); err != nil {
			return err
		}
		if _, err := wxUpgradeDocker(ctx, "rename", existing.ID, name+"-previous-"+existing.ID[:12]); err != nil {
			return err
		}
		if _, err := wxUpgradeDocker(ctx, "start", server.ID); err != nil {
			return err
		}
		path := fmt.Sprintf("/robot/docker/container/client/start?id=%d", robot.ID)
		if err := admin.call(ctx, http.MethodPost, path, map[string]int64{"id": robot.ID}, nil); err != nil {
			return err
		}
		candidate, err := wxInspectUpgradeContainer(ctx, "name=^/"+name+"$")
		if err != nil {
			return err
		}
		if candidate == nil || !candidate.Managed || candidate.Image != image {
			return fmt.Errorf("%s 未使用锁定的客户端镜像", name)
		}
		if err := waitWXClientCapabilities(ctx, name, 90*time.Second); err != nil {
			return err
		}
	}
	return nil
}

func wxRollbackUpgradeClients(ctx context.Context, clients []wxUpgradeClient) error {
	for _, old := range clients {
		if !strings.HasPrefix(old.Name, "client_") || !wxRobotContainer.MatchString(old.Name) || !wxContainerID.MatchString(old.ID) {
			return errors.New("机器人容器恢复记录无效")
		}
		original, err := wxInspectUpgradeContainer(ctx, "id="+old.ID)
		if err != nil {
			return err
		}
		if original == nil || original.ID != old.ID || !original.Managed {
			return fmt.Errorf("找不到 %s 的原容器", old.Name)
		}
		if original.Name == old.Name {
			continue
		}
		if original.Name != old.Name+"-previous-"+old.ID[:12] {
			return fmt.Errorf("%s 的原容器名称已变更", old.Name)
		}
		replacement, err := wxInspectUpgradeContainer(ctx, "name=^/"+old.Name+"$")
		if err != nil {
			return err
		}
		if replacement != nil {
			if !replacement.Managed {
				return fmt.Errorf("%s 被其他容器占用", old.Name)
			}
			if _, err := wxUpgradeDocker(ctx, "rm", "--force", replacement.ID); err != nil {
				return err
			}
		}
		if _, err := wxUpgradeDocker(ctx, "rename", old.ID, old.Name); err != nil {
			return err
		}
	}
	return nil
}
