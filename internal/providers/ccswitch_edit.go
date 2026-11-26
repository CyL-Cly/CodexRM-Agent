package providers

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// ProviderDetails and ProviderPatch are separate so an existing credential can
// never be returned to a remote client.
type ProviderDetails struct {
	Name         string `json:"name"`
	Notes        string `json:"notes"`
	BaseURL      string `json:"baseUrl"`
	Official     bool   `json:"official"`
	GPTOfficial  bool   `json:"gptOfficial"`
	Local        bool   `json:"local"`
	DefaultModel string `json:"defaultModel"`
}

type ProviderPatch struct {
	ID           string  `json:"id"`
	Name         *string `json:"name"`
	Notes        *string `json:"notes"`
	BaseURL      *string `json:"baseUrl"`
	APIKey       string  `json:"apiKey"`
	GPTOfficial  *bool   `json:"gptOfficial"`
	DefaultModel *string `json:"defaultModel"`
}

func openEditor(path string, writable bool) (*sql.DB, error) {
	if path == "" {
		path = DefaultPath()
	}
	if _, err := os.Stat(path); err != nil {
		return nil, errors.New("找不到 CC Switch 数据库，请先在电脑上运行 CC Switch")
	}
	mode := "ro"
	if writable {
		mode = "rw"
	}
	return openCCSwitch(path, mode)
}

type providerRow struct {
	id, name, notes, settings string
}

func findProviderRow(ctx context.Context, db *sql.DB, publicID string) (providerRow, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, name, COALESCE(notes, ''), settings_config FROM providers WHERE app_type = 'codex'")
	if err != nil {
		return providerRow{}, errors.New("无法读取 CC Switch 供应商")
	}
	defer rows.Close()
	for rows.Next() {
		var row providerRow
		if err := rows.Scan(&row.id, &row.name, &row.notes, &row.settings); err != nil {
			break
		}
		if providerID(row.id) == publicID {
			return row, nil
		}
	}
	return providerRow{}, errors.New("CC Switch 中找不到该 Codex 供应商")
}

func EditDetails(ctx context.Context, path, id string) (ProviderDetails, error) {
	db, err := openEditor(path, false)
	if err != nil {
		return ProviderDetails{}, err
	}
	defer db.Close()
	row, err := findProviderRow(ctx, db, id)
	if err != nil {
		return ProviderDetails{}, err
	}
	return detailsForRow(row, false)
}

func detailsForRow(row providerRow, local bool) (ProviderDetails, error) {
	p, err := parse(row.id, row.name, row.settings)
	if err != nil {
		return ProviderDetails{}, errors.New("配置无法读取，请在电脑端 CC Switch 中检查")
	}
	base, _ := p.config["base_url"].(string)
	if !validProviderURL(base) {
		base = ""
	}
	return ProviderDetails{Name: row.name, Notes: row.notes, BaseURL: base, Official: p.Official(), Local: local, DefaultModel: p.DefaultModel, GPTOfficial: p.Official() || GPTOfficial(p.config)}, nil
}

func validProviderURL(base string) bool {
	u, err := url.Parse(base)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" &&
		u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func validateProviderPatch(patch ProviderPatch) (ProviderPatch, error) {
	if patch.Name != nil {
		name := strings.TrimSpace(*patch.Name)
		if name == "" || len(name) > 256 {
			return ProviderPatch{}, errors.New("请输入有效的供应商名称")
		}
		patch.Name = &name
	}
	if patch.Notes != nil && len(*patch.Notes) > 4096 {
		return ProviderPatch{}, errors.New("供应商备注过长")
	}
	if patch.BaseURL != nil {
		base := strings.TrimSpace(*patch.BaseURL)
		if len(base) > 2048 || !validProviderURL(base) {
			return ProviderPatch{}, errors.New("请输入有效的 HTTP 或 HTTPS 接入地址，地址中不能包含凭据、查询参数或片段")
		}
		patch.BaseURL = &base
	}
	if len(patch.APIKey) > 8192 || strings.ContainsAny(patch.APIKey, "\r\n") {
		return ProviderPatch{}, errors.New("请输入有效的 API Key")
	}
	if patch.DefaultModel != nil {
		model := strings.TrimSpace(*patch.DefaultModel)
		if len(model) > 256 || strings.ContainsAny(model, "\r\n\x00") {
			return ProviderPatch{}, errors.New("请输入有效的默认模型 ID")
		}
		patch.DefaultModel = &model
	}
	return patch, nil
}

func SaveProvider(ctx context.Context, path string, patch ProviderPatch) (string, error) {
	patch, err := validateProviderPatch(patch)
	if err != nil {
		return "", err
	}
	db, err := openEditor(path, true)
	if err != nil {
		return "", err
	}
	defer db.Close()
	if patch.ID == "" {
		if patch.Name == nil || patch.BaseURL == nil {
			return "", errors.New("请输入名称和接入地址")
		}
		id, err := newProviderID()
		if err != nil {
			return "", err
		}
		raw, err := patchProviderConfig(newProviderConfig, patch)
		if err != nil {
			return "", err
		}
		var notes string
		if patch.Notes != nil {
			notes = *patch.Notes
		}
		_, err = db.ExecContext(ctx, `INSERT INTO providers (id, app_type, name, settings_config, notes, category, created_at) VALUES (?, 'codex', ?, ?, ?, 'custom', ?)`, id, *patch.Name, raw, notes, time.Now().UnixMilli())
		if err != nil {
			return "", errors.New("无法在 CC Switch 中添加供应商")
		}
		return providerID(id), nil
	}
	row, err := findProviderRow(ctx, db, patch.ID)
	if err != nil {
		return "", err
	}
	updated := row
	if patch.Name != nil {
		updated.name = *patch.Name
	}
	if patch.Notes != nil {
		updated.notes = *patch.Notes
	}
	if patch.changesConfig() {
		updated.settings, err = patchProviderConfig(row.settings, patch)
		if err != nil {
			return "", err
		}
	}
	result, err := db.ExecContext(ctx, `UPDATE providers SET name = ?, notes = ?, settings_config = ? WHERE id = ? AND app_type = 'codex' AND name = ? AND COALESCE(notes, '') = ? AND settings_config = ?`,
		updated.name, updated.notes, updated.settings, row.id, row.name, row.notes, row.settings)
	if err != nil {
		return "", errors.New("无法保存 CC Switch 供应商")
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return "", errors.New("供应商已被其他操作修改，请重新打开后再编辑")
	}
	return patch.ID, nil
}

const newProviderConfig = `{"config":"model_provider = 'custom'\n\n[model_providers.custom]\nname = 'Custom'\nwire_api = 'responses'\nrequires_openai_auth = false\n","auth":{}}`

func newProviderID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", errors.New("无法创建供应商 ID")
	}
	return hex.EncodeToString(random[:]), nil
}

func (p ProviderPatch) changesConfig() bool {
	return p.BaseURL != nil || p.APIKey != "" || p.DefaultModel != nil || p.GPTOfficial != nil
}

func patchProviderConfig(raw string, patch ProviderPatch) (string, error) {
	invalid := errors.New("配置无法读取，请在电脑端 CC Switch 中检查")
	var settings map[string]json.RawMessage
	var config map[string]any
	var text string
	if json.Unmarshal([]byte(raw), &settings) != nil || json.Unmarshal(settings["config"], &text) != nil || toml.Unmarshal([]byte(text), &config) != nil {
		return "", invalid
	}
	if patch.DefaultModel != nil {
		if *patch.DefaultModel == "" {
			delete(config, "model")
		} else {
			config["model"] = *patch.DefaultModel
		}
	}
	var connection map[string]any
	if patch.BaseURL != nil || patch.APIKey != "" || patch.GPTOfficial != nil {
		id, _ := config["model_provider"].(string)
		if id == "" || id == "openai" {
			if patch.GPTOfficial != nil {
				return "", errors.New("Codex 内置官方供应商无需修改此开关")
			}
			return "", errors.New("官方供应商的登录凭据请在 Codex 中管理")
		}
		tables, _ := config["model_providers"].(map[string]any)
		connection, _ = tables[id].(map[string]any)
		if connection == nil {
			return "", invalid
		}
	}
	if patch.GPTOfficial != nil {
		capabilities, ok := connection["capabilities"].(map[string]any)
		if !ok {
			if _, exists := connection["capabilities"]; exists {
				return "", invalid
			}
			capabilities = make(map[string]any)
			connection["capabilities"] = capabilities
		}
		connection["supports_websockets"] = *patch.GPTOfficial
		capabilities["remote_compaction"] = RemoteCompactionValue(*patch.GPTOfficial)
	}
	if patch.BaseURL != nil {
		connection["base_url"] = *patch.BaseURL
	}
	if patch.APIKey != "" {
		var auth map[string]json.RawMessage
		if existing := settings["auth"]; len(existing) > 0 && json.Unmarshal(existing, &auth) != nil {
			return "", invalid
		}
		if auth == nil {
			auth = map[string]json.RawMessage{}
		}
		auth["OPENAI_API_KEY"], _ = json.Marshal(patch.APIKey)
		settings["auth"], _ = json.Marshal(auth)
		delete(connection, "experimental_bearer_token")
		delete(connection, "env_key")
		connection["requires_openai_auth"] = true
	}
	rewritten, err := toml.Marshal(config)
	if err != nil {
		return "", invalid
	}
	settings["config"], _ = json.Marshal(string(rewritten))
	result, err := json.Marshal(settings)
	if err != nil {
		return "", errors.New("无法保存供应商配置")
	}
	return string(result), nil
}
