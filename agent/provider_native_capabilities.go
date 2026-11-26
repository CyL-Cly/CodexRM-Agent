package agent

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Agent/internal/providers"
)

// The app-server config edit API uses dotted key paths. Only accept standalone
// table keys here; the caller never supplies a file path or a TOML key.
var nativeProviderKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type nativeProviderCapabilities struct {
	Editable    bool   `json:"editable"`
	GPTOfficial bool   `json:"gptOfficial"`
	Version     string `json:"version,omitempty"`
}

func nativeProviderCapabilitiesRPC(ctx context.Context, app *appserver.Client, raw json.RawMessage) (nativeProviderCapabilities, error) {
	var params struct {
		GPTOfficial *bool  `json:"gptOfficial"`
		Version     string `json:"version"`
	}
	var result nativeProviderCapabilities
	if json.Unmarshal(raw, &params) != nil {
		return result, errors.New("无效的供应商设置")
	}
	if app == nil {
		return result, errors.New("Codex 尚未连接，请稍后重试")
	}
	data, err := app.Call(ctx, "config/read", map[string]bool{"includeLayers": true})
	var response struct {
		Layers []struct {
			Name struct {
				Type    string `json:"type"`
				Profile string `json:"profile"`
			} `json:"name"`
			Version        string `json:"version"`
			DisabledReason string `json:"disabledReason"`
			Config         struct {
				Provider  string                    `json:"model_provider"`
				Providers map[string]map[string]any `json:"model_providers"`
			} `json:"config"`
		} `json:"layers"`
	}
	if err != nil || json.Unmarshal(data, &response) != nil {
		return result, errors.New("无法读取 Codex 配置，请检查电脑端 Codex")
	}
layers:
	for _, layer := range response.Layers {
		if layer.Name.Type != "user" || layer.Name.Profile != "" || layer.DisabledReason != "" {
			continue
		}
		id := layer.Config.Provider
		switch id {
		case "", "openai", "amazon-bedrock", "amazon-bedrock-runtime", "gcp", "ollama", "lmstudio":
			break layers
		}
		connection := layer.Config.Providers[id]
		wire, _ := connection["wire_api"].(string)
		if connection == nil || !nativeProviderKey.MatchString(id) || (wire != "" && wire != "responses") || layer.Version == "" {
			break
		}
		result = nativeProviderCapabilities{Editable: true, GPTOfficial: providers.GPTOfficial(connection), Version: layer.Version}
		if params.GPTOfficial == nil {
			return result, nil
		}
		if params.Version != layer.Version {
			return result, errors.New("Codex 配置已变更，请重新打开后再保存")
		}
		key := "model_providers." + id + "."
		enabled := *params.GPTOfficial
		_, err := app.Call(ctx, "config/batchWrite", map[string]any{
			"expectedVersion": layer.Version,
			"edits": []map[string]any{
				{"keyPath": key + "supports_websockets", "value": enabled, "mergeStrategy": "replace"},
				{"keyPath": key + "capabilities.remote_compaction", "value": providers.RemoteCompactionValue(enabled), "mergeStrategy": "replace"},
			},
		})
		if err != nil {
			return result, errors.New("无法写入 Codex 配置，请重新打开设置或更新电脑端 Codex 后重试")
		}
		return nativeProviderCapabilities{Editable: true, GPTOfficial: enabled}, nil
	}
	if params.GPTOfficial != nil {
		return result, errors.New("当前 Codex 配置没有可编辑的自定义供应商")
	}
	return result, nil
}
