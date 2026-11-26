package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Agent/internal/providers"
)

type modelCustomization struct {
	Added  []string `json:"added,omitempty"`
	Hidden []string `json:"hidden,omitempty"`
}

type modelCustomizations map[string]modelCustomization

var modelSettingsMu sync.Mutex

func (a *Agent) supportsProviderManagement() bool {
	return a.cfg.DataDir != "" && !a.cfg.Mock && a.cfg.AppServerURL == ""
}

// The caller holds modelSettingsMu while reading or changing the file.
func (a *Agent) readModelSettings() (modelCustomizations, error) {
	settings := make(modelCustomizations)
	if a.cfg.DataDir == "" {
		return settings, nil
	}
	data, err := os.ReadFile(filepath.Join(a.providerDir(), "model-customizations.json"))
	if errors.Is(err, os.ErrNotExist) && a.cfg.DataDir != a.providerDir() {
		// Read the pre-existing per-relay settings until the first machine-wide save.
		data, err = os.ReadFile(filepath.Join(a.cfg.DataDir, "model-customizations.json"))
	}
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil || json.Unmarshal(data, &settings) != nil || settings == nil {
		return nil, errors.New("无法读取模型自定义设置")
	}
	return settings, nil
}

type modelChange struct {
	Model  string `json:"model"`
	Action string `json:"action"`
}

type modelChangesRequest struct {
	ID      *string       `json:"id"`
	Changes []modelChange `json:"changes"`
}

func (a *Agent) changeModel(ctx context.Context, provider, model, action string) error {
	return a.changeModels(ctx, provider, []modelChange{{Model: model, Action: action}}, true)
}

func (a *Agent) changeModels(ctx context.Context, provider string, changes []modelChange, resolveNativeAlias bool) error {
	if len(changes) == 0 {
		return errors.New("没有要保存的模型改动")
	}
	for i := range changes {
		change := &changes[i]
		change.Model = strings.TrimSpace(change.Model)
		if change.Model == "" || len(change.Model) > 256 || strings.ContainsAny(change.Model, "\r\n\x00") {
			return errors.New("请输入有效的模型 ID")
		}
		switch change.Action {
		case "add", "hide", "restore":
		default:
			return errors.New("不支持的模型操作")
		}
	}
	items, err := a.loadProviders(ctx)
	if err != nil {
		return err
	}
	if provider == "" && resolveNativeAlias {
		provider = providers.CurrentProviderID(items, a.cfg.CCSwitchDB)
	}
	if provider != "" {
		found := false
		for _, item := range items {
			found = found || item.ID == provider
		}
		if !found {
			return errors.New("找不到该模型供应商")
		}
	}

	modelSettingsMu.Lock()
	defer modelSettingsMu.Unlock()
	settings, err := a.readModelSettings()
	if err != nil {
		return err
	}
	entry := settings[provider]
	for _, change := range changes {
		model := change.Model
		switch change.Action {
		case "add":
			if !containsModelID(entry.Added, model) {
				entry.Added = append(entry.Added, model)
			}
			entry.Hidden = withoutModelID(entry.Hidden, model)
		case "hide":
			if !containsModelID(entry.Hidden, model) {
				entry.Hidden = append(entry.Hidden, model)
			}
		case "restore":
			entry.Hidden = withoutModelID(entry.Hidden, model)
		}
	}
	settings[provider] = entry
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	dir := a.providerDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("无法保存模型自定义设置")
	}
	file, err := os.CreateTemp(dir, "model-customizations-*.tmp")
	if err != nil {
		return errors.New("无法保存模型自定义设置")
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil || os.Rename(file.Name(), filepath.Join(dir, "model-customizations.json")) != nil {
		return errors.New("无法保存模型自定义设置")
	}
	return nil
}

func containsModelID(ids []string, model string) bool {
	for _, id := range ids {
		if id == model {
			return true
		}
	}
	return false
}

func withoutModelID(ids []string, model string) []string {
	remaining := ids[:0]
	for _, id := range ids {
		if id != model {
			remaining = append(remaining, id)
		}
	}
	return remaining
}

func customizeModels(models []json.RawMessage, settings modelCustomization, includeHidden bool) []json.RawMessage {
	visible := make([]json.RawMessage, 0, len(models)+len(settings.Added))
	hidden := []json.RawMessage{}
	seen := make(map[string]bool)
	for _, raw := range models {
		var model map[string]json.RawMessage
		if json.Unmarshal(raw, &model) != nil || model == nil {
			continue
		}
		var id string
		_ = json.Unmarshal(model["model"], &id)
		if id == "" {
			_ = json.Unmarshal(model["id"], &id)
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if containsModelID(settings.Hidden, id) {
			if includeHidden {
				model["blacklisted"] = json.RawMessage("true")
				hidden = append(hidden, mustJSON(model))
			}
		} else if includeHidden || string(model["hidden"]) != "true" {
			visible = append(visible, raw)
		}
	}
	for _, id := range settings.Added {
		if seen[id] {
			continue
		}
		seen[id] = true
		if !containsModelID(settings.Hidden, id) {
			visible = append(visible, customModel(id, false))
		} else if includeHidden {
			hidden = append(hidden, customModel(id, true))
		}
	}
	if includeHidden {
		for _, id := range settings.Hidden {
			if !seen[id] {
				hidden = append(hidden, customModel(id, true))
			}
		}
	}
	return append(visible, hidden...)
}

func customModel(id string, blacklisted bool) json.RawMessage {
	return mustJSON(map[string]any{"id": id, "model": id, "displayName": id, "blacklisted": blacklisted})
}

func (a *Agent) providerDir() string {
	dir := a.cfg.ProviderDir
	if dir == "" {
		dir = a.cfg.DataDir
	}
	return dir
}

func (a *Agent) providerStore() providers.LocalStore {
	return providers.LocalStore{Dir: a.providerDir()}
}

func (a *Agent) loadProviders(ctx context.Context) ([]providers.Provider, error) {
	items, deleted, err := a.providerStore().Load()
	if err != nil {
		return nil, err
	}
	legacy, err := providers.Load(ctx, a.cfg.CCSwitchDB)
	if err != nil {
		if len(items) != 0 {
			return items, nil
		}
		return nil, err
	}
	ids := make(map[string]bool, len(items))
	for _, item := range items {
		ids[item.ID] = true
	}
	for _, item := range legacy {
		if !ids[item.ID] && !deleted[item.ID] {
			items = append(items, item)
		}
	}
	return items, nil
}

// OfflineProviderSettings handles local configuration before a relay agent is running.
func OfflineProviderSettings(ctx context.Context, cfg Config, operation string, params json.RawMessage) (any, error) {
	if cfg.DataDir == "" {
		cfg.DataDir = cfg.ProviderDir
	}
	return (&Agent{cfg: cfg}).LocalProviderSettings(ctx, operation, params)
}

// LocalProviderSettings is used by the authenticated Agent settings page.
func (a *Agent) LocalProviderSettings(ctx context.Context, operation string, params json.RawMessage) (any, error) {
	if operation == "import" {
		if !a.supportsProviderManagement() {
			return nil, errors.New("此 Agent 不支持管理本机供应商")
		}
		count, err := a.providerStore().ImportCCSwitch(ctx, a.cfg.CCSwitchDB)
		return map[string]int{"count": count}, err
	}
	return a.providerSettingsRPC(ctx, a.providerApp.Load(), "codexrm/provider/"+operation, params, true)
}

func (a *Agent) providerSettingsRPC(ctx context.Context, app *appserver.Client, method string, raw json.RawMessage, direct bool) (any, error) {
	if !a.supportsProviderManagement() {
		return nil, errors.New("此 Agent 不支持管理本机供应商")
	}
	switch method {
	case "codexrm/provider/catalog":
		return a.modelCatalogWithSettings(ctx, app, true)
	case "codexrm/provider/nativeCapabilities":
		return nativeProviderCapabilitiesRPC(ctx, app, raw)
	case "codexrm/provider/details":
		var params struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &params) != nil || params.ID == "" {
			return nil, errors.New("请选择供应商")
		}
		if details, found, err := a.providerStore().Details(params.ID); found || err != nil {
			return details, err
		}
		return providers.EditDetails(ctx, a.cfg.CCSwitchDB, params.ID)
	case "codexrm/provider/save":
		var patch providers.ProviderPatch
		if json.Unmarshal(raw, &patch) != nil {
			return nil, errors.New("无效的供应商设置")
		}
		if !direct && (patch.APIKey != "" || patch.ID != "" && patch.BaseURL != nil) {
			return nil, errors.New("修改现有接入地址或 API Key 需要 P2P 直连")
		}
		id, found, err := a.providerStore().Save(patch)
		if !found && err == nil {
			id, err = providers.SaveProvider(ctx, a.cfg.CCSwitchDB, patch)
		}
		return map[string]string{"id": id}, err
	case "codexrm/provider/delete":
		var params struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &params) != nil || params.ID == "" {
			return nil, errors.New("请选择要删除的供应商；当前 Codex 配置不能删除")
		}
		if err := a.providerStore().Delete(ctx, a.cfg.CCSwitchDB, params.ID); err != nil {
			return nil, err
		}
		return map[string]bool{"deleted": true}, nil
	case "codexrm/provider/models":
		var params modelChangesRequest
		if json.Unmarshal(raw, &params) != nil || params.ID == nil {
			return nil, errors.New("无效的模型设置")
		}
		// The batch is scoped to the provider that was open in the settings page.
		if err := a.changeModels(ctx, *params.ID, params.Changes, false); err != nil {
			return nil, err
		}
		return map[string]bool{"saved": true}, nil
	case "codexrm/provider/model":
		var params struct {
			ID     string `json:"id"`
			Model  string `json:"model"`
			Action string `json:"action"`
		}
		if json.Unmarshal(raw, &params) != nil {
			return nil, errors.New("无效的模型设置")
		}
		return map[string]bool{"saved": true}, a.changeModel(ctx, params.ID, params.Model, params.Action)
	default:
		return nil, &appserver.RPCError{Code: -32601, Message: "未知的供应商设置操作"}
	}
}
