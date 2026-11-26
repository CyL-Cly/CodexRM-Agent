package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Agent/internal/providers"
)

func nativeModels(ctx context.Context, app *appserver.Client, provider string) ([]json.RawMessage, error) {
	if app == nil {
		return nil, errors.New("Codex app-server 未启动")
	}
	models := []json.RawMessage{}
	params := map[string]any{"limit": 100}
	if provider != "" {
		params["modelProvider"] = provider
	}
	for {
		raw, err := app.Call(ctx, "model/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Data       []json.RawMessage `json:"data"`
			NextCursor *string           `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		models = append(models, page.Data...)
		if page.NextCursor == nil || *page.NextCursor == "" || params["cursor"] == *page.NextCursor {
			return models, nil
		}
		params["cursor"] = *page.NextCursor
	}
}

func (a *Agent) modelCatalog(ctx context.Context, app *appserver.Client) (any, error) {
	return a.modelCatalogWithSettings(ctx, app, false)
}

func (a *Agent) modelCatalogWithSettings(ctx context.Context, app *appserver.Client, management bool) (any, error) {
	groups := []any{}
	warnings := []string{}
	nativeCtx := ctx
	if a.localEvent != nil {
		// An offline/unconfigured official catalog must not block adding third-party keys.
		var cancel context.CancelFunc
		nativeCtx, cancel = context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
	}
	models, err := nativeModels(nativeCtx, app, "")
	if err != nil {
		warnings = append(warnings, "当前 Codex 模型列表暂不可用")
		models = []json.RawMessage{}
	}
	native := map[string]any{"id": "", "runtimeId": "", "name": "当前 Codex 配置", "source": "Codex", "models": models}
	groups = append(groups, native)
	var activeID string
	modelSettingsMu.Lock()
	settings, settingsErr := a.readModelSettings()
	modelSettingsMu.Unlock()
	if settingsErr != nil {
		return nil, settingsErr
	}
	native["models"] = customizeModels(models, settings[""], management)
	if !a.cfg.Mock && a.cfg.AppServerURL == "" {
		items, err := a.loadProviders(ctx)
		if err != nil {
			warnings = append(warnings, err.Error())
		} else {
			if id, customRuntimeID := providers.CurrentProvider(items, a.cfg.CCSwitchDB); id != "" {
				activeID = id
				native["aliasOf"] = id
				native["switchableRuntimeId"] = customRuntimeID
			}
			providers.Discover(ctx, items)
			for _, p := range items {
				catalog := make([]json.RawMessage, 0, len(p.Models))
				for _, model := range p.Models {
					catalog = append(catalog, mustJSON(model))
				}
				if p.ID == activeID && management {
					catalog = append(catalog, models...)
				}
				public := map[string]any{"id": p.ID, "runtimeId": p.RuntimeID(), "name": p.Name, "notes": p.Notes, "source": p.Source, "models": customizeModels(catalog, settings[p.ID], management), "defaultModel": p.DefaultModel, "warning": p.Warning}
				if p.Official() && len(catalog) == 0 {
					public["warning"] = "暂无官方模型列表，请先在 Codex 中登录"
				}
				groups = append(groups, public)
			}
			if management && activeID != "" {
				groups = groups[1:]
			} else {
				nativeSettings := settings[activeID]
				if activeID != "" {
					nativeSettings.Added = nil
				}
				native["models"] = customizeModels(models, nativeSettings, management)
			}
		}
	}
	result := map[string]any{"providers": groups, "warnings": warnings}
	if management {
		result["gptProviderSettings"] = true
	}
	return result, nil
}

var errProviderUnavailable = errors.New("请切换到可用的供应商后继续对话")

// Recover the provider credentials and restore any model saved by CodexRM.
func (a *Agent) prepareProvider(ctx context.Context, app *appserver.Client, method string, params map[string]any) error {
	value, supplied := params["codexrmProvider"]
	delete(params, "codexrmProvider")
	if method != "thread/start" && method != "thread/resume" {
		return nil
	}
	id, _ := value.(string)
	if a.cfg.Mock || a.cfg.AppServerURL != "" {
		if id != "" {
			return fmt.Errorf("此连接器不支持本机模型供应商")
		}
		return nil
	}
	var runtimeID string
	if !supplied && method == "thread/resume" {
		raw, err := app.Call(ctx, "thread/read", map[string]any{"threadId": params["threadId"], "includeTurns": false})
		if err != nil {
			return err
		}
		var result struct {
			Thread struct {
				ModelProvider string `json:"modelProvider"`
			} `json:"thread"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return err
		}
		threadID, _ := params["threadId"].(string)
		route, err := a.providerRoute(threadID)
		if err != nil {
			return err
		}
		if route.ProviderID != "" {
			id, runtimeID = route.ProviderID, result.Thread.ModelProvider
			if route.Model != "" && params["model"] == nil {
				params["model"] = route.Model
			}
		} else if strings.HasPrefix(result.Thread.ModelProvider, "ccswitch_") {
			id, runtimeID = result.Thread.ModelProvider, result.Thread.ModelProvider
		}
	}
	if id == "" {
		return nil
	}
	items, err := a.loadProviders(ctx)
	if err != nil {
		return err
	}
	for _, p := range items {
		if p.ID != id {
			continue
		}
		if p.Warning != "" {
			return fmt.Errorf("%s：%s", p.Name, p.Warning)
		}
		if runtimeID == "" {
			runtimeID = p.RuntimeID()
		}
		if runtimeID != p.RuntimeID() && p.Official() {
			return fmt.Errorf("此会话的接入点需使用第三方供应商")
		}
		params["modelProvider"] = runtimeID
		if method == "thread/start" && params["model"] == nil && p.DefaultModel != "" {
			params["model"] = p.DefaultModel
		}
		if overrides := p.OverridesFor(runtimeID); len(overrides) > 0 {
			config, _ := params["config"].(map[string]any)
			if config == nil {
				config = map[string]any{}
			}
			for key, value := range overrides {
				config[key] = value
			}
			params["config"] = config
		}
		return nil
	}
	return errProviderUnavailable
}
