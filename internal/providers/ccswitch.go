// Package providers reads local provider settings without changing the active CLI login.
package providers

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
	_ "modernc.org/sqlite"
)

type Model struct {
	ID                        string            `json:"id"`
	Model                     string            `json:"model"`
	DisplayName               string            `json:"displayName"`
	IsDefault                 bool              `json:"isDefault"`
	SupportedReasoningEfforts []ReasoningEffort `json:"supportedReasoningEfforts"`
	DefaultReasoningEffort    string            `json:"defaultReasoningEffort,omitempty"`
}

type ReasoningEffort struct {
	ReasoningEffort string `json:"reasoningEffort"`
	Description     string `json:"description"`
}

// Only these public fields may cross the relay. Connection settings stay local.
type Provider struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Notes        string  `json:"notes,omitempty"`
	Source       string  `json:"source"`
	DefaultModel string  `json:"defaultModel,omitempty"`
	Models       []Model `json:"models"`
	Warning      string  `json:"warning,omitempty"`
	config       map[string]any
	builtin      string
}

func (p Provider) RuntimeID() string {
	if p.builtin != "" {
		return p.builtin
	}
	return p.ID
}

// Overrides are sent over local app-server stdin, never written into global config.
func (p Provider) Overrides() map[string]any {
	return p.OverridesFor(p.RuntimeID())
}

// OverridesFor allows an existing thread to keep its persisted provider ID
// while using another CCSwitch endpoint's connection settings.
func (p Provider) OverridesFor(runtimeID string) map[string]any {
	if p.builtin != "" {
		return nil
	}
	return map[string]any{"model_providers." + runtimeID: p.config}
}

func (p Provider) Official() bool { return p.builtin == "openai" }

func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cc-switch", "cc-switch.db")
}

func openCCSwitch(path, mode string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uriPath := filepath.ToSlash(abs)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=" + mode + "&_pragma=busy_timeout(3000)"}
	return sql.Open("sqlite", u.String())
}

func Load(ctx context.Context, path string) ([]Provider, error) {
	if path == "" {
		path = DefaultPath()
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return []Provider{}, nil
	} else if err != nil {
		return nil, fmt.Errorf("无法读取 CCSwitch 数据库")
	}
	db, err := openCCSwitch(path, "ro")
	if err != nil {
		return nil, fmt.Errorf("无法打开 CCSwitch 数据库")
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT id, name, settings_config, COALESCE(notes, '') FROM providers WHERE app_type = 'codex' ORDER BY COALESCE(sort_index, 2147483647), rowid")
	if err != nil {
		return nil, fmt.Errorf("无法读取 CCSwitch 的 Codex 供应商")
	}
	defer rows.Close()
	out := []Provider{}
	for rows.Next() {
		var id, name, settings, notes string
		if err := rows.Scan(&id, &name, &settings, &notes); err != nil {
			return nil, fmt.Errorf("无法读取 CCSwitch 供应商记录")
		}
		p, err := parse(id, name, settings)
		if err != nil {
			// A broken provider must not hide the other providers or expose config values.
			p = Provider{ID: providerID(id), Name: name, Source: "CCSwitch", Models: []Model{}, Warning: "供应商配置无效，请在 CCSwitch 中检查"}
		}
		p.Notes = strings.TrimSpace(notes)
		if p.Official() {
			cached := officialModels(filepath.Join(filepath.Dir(path), "codex-official-models.json"))
			if len(cached) > 0 {
				indices := map[string]int{}
				for i, model := range p.Models {
					indices[model.Model] = i
				}
				for _, model := range cached {
					if i, exists := indices[model.Model]; exists {
						saved := &p.Models[i]
						if saved.DisplayName == saved.Model {
							saved.DisplayName = model.DisplayName
						}
						if len(saved.SupportedReasoningEfforts) == 0 {
							saved.SupportedReasoningEfforts = model.SupportedReasoningEfforts
						}
						if saved.DefaultReasoningEffort == "" {
							saved.DefaultReasoningEffort = model.DefaultReasoningEffort
						}
					} else {
						indices[model.Model] = len(p.Models)
						model.IsDefault = model.Model == p.DefaultModel
						p.Models = append(p.Models, model)
					}
				}
				p.Source = "CCSwitch 官方模型缓存"
			}
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取 CCSwitch 供应商失败")
	}
	return out, nil
}

type codexConfig struct {
	Model     string                    `toml:"model"`
	Provider  string                    `toml:"model_provider"`
	Providers map[string]map[string]any `toml:"model_providers"`
}

// CurrentProviderID identifies a saved provider whose connection settings match
// the local Codex configuration. Model names alone do not identify a provider.
func CurrentProviderID(items []Provider, dbPath string) string {
	id, _ := CurrentProvider(items, dbPath)
	return id
}

// CurrentProvider returns the matching CCSwitch ID and the active custom
// provider ID in Codex's own configuration. Built-in providers have no custom ID.
func CurrentProvider(items []Provider, dbPath string) (id, customRuntimeID string) {
	if len(items) == 0 {
		return "", ""
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", ""
		}
		home = filepath.Join(userHome, ".codex")
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil && !os.IsNotExist(err) {
		return "", ""
	}
	var cfg codexConfig
	if toml.Unmarshal(data, &cfg) != nil {
		return "", ""
	}
	live := cfg.Providers[cfg.Provider]
	if len(live) > 0 && live["experimental_bearer_token"] == nil && live["requires_openai_auth"] == true && live["env_key"] == nil {
		if auth, err := os.ReadFile(filepath.Join(home, "auth.json")); err == nil {
			var credentials map[string]any
			if json.Unmarshal(auth, &credentials) == nil {
				applyAPIKey(live, credentials)
			}
		}
	}
	if dbPath == "" {
		dbPath = DefaultPath()
	}
	var settings struct {
		Current string `json:"currentProviderCodex"`
	}
	var preferred string
	if data, err := os.ReadFile(filepath.Join(filepath.Dir(dbPath), "settings.json")); err == nil && json.Unmarshal(data, &settings) == nil && settings.Current != "" {
		preferred = providerID(settings.Current)
	}
	var matched string
	matches := 0
	for _, p := range items {
		if p.Warning != "" {
			continue
		}
		same := p.Official() && (cfg.Provider == "" || cfg.Provider == "openai")
		if !p.Official() && live != nil {
			same = reflect.DeepEqual(live, p.config)
		}
		if same {
			if p.ID == preferred {
				return p.ID, customProviderRuntime(cfg)
			}
			matched = p.ID
			matches++
		}
	}
	if matches == 1 {
		return matched, customProviderRuntime(cfg)
	}
	return "", ""
}

func customProviderRuntime(cfg codexConfig) string {
	switch cfg.Provider {
	case "", "openai", "ollama", "lmstudio":
		return ""
	}
	return cfg.Provider
}

func providerID(id string) string {
	hash := sha256.Sum256([]byte(id))
	return "ccswitch_" + hex.EncodeToString(hash[:8])
}

func applyAPIKey(config, auth map[string]any) {
	if key, _ := auth["OPENAI_API_KEY"].(string); key != "" {
		delete(config, "env_key")
		config["experimental_bearer_token"] = key
		config["requires_openai_auth"] = false
	}
}

func officialModels(path string) []Model {
	data, err := os.ReadFile(path)
	if err != nil {
		return []Model{}
	}
	var entries map[string]struct {
		FetchedAt int64 `json:"fetched_at"`
		Models    []struct {
			Slug   string `json:"slug"`
			Name   string `json:"display_name"`
			Levels []struct {
				Effort      string `json:"effort"`
				Description string `json:"description"`
			} `json:"supported_reasoning_levels"`
			DefaultLevel string `json:"default_reasoning_level"`
		} `json:"models"`
	}
	if json.Unmarshal(data, &entries) != nil {
		return []Model{}
	}
	models := []Model{}
	var latest int64
	for _, entry := range entries {
		if entry.FetchedAt < latest || len(entry.Models) == 0 {
			continue
		}
		latest = entry.FetchedAt
		models = []Model{}
		for _, m := range entry.Models {
			if m.Slug == "" {
				continue
			}
			model := Model{ID: m.Slug, Model: m.Slug, DisplayName: m.Name, DefaultReasoningEffort: m.DefaultLevel, SupportedReasoningEfforts: []ReasoningEffort{}}
			for _, r := range m.Levels {
				model.SupportedReasoningEfforts = append(model.SupportedReasoningEfforts, ReasoningEffort{ReasoningEffort: r.Effort, Description: r.Description})
			}
			models = append(models, model)
		}
	}
	return models
}

func parse(id, name, raw string) (Provider, error) {
	p := Provider{ID: providerID(id), Name: name, Source: "CCSwitch", Models: []Model{}}
	var settings struct {
		Auth    map[string]any `json:"auth"`
		Config  string         `json:"config"`
		Catalog struct {
			Models []struct {
				Model        string   `json:"model"`
				DisplayName  string   `json:"displayName"`
				Levels       []string `json:"reasoningLevels"`
				DefaultLevel string   `json:"defaultReasoningLevel"`
			} `json:"models"`
		} `json:"modelCatalog"`
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return p, err
	}
	var cfg codexConfig
	if err := toml.Unmarshal([]byte(settings.Config), &cfg); err != nil {
		return p, err
	}
	p.DefaultModel = cfg.Model
	if cfg.Provider == "" || cfg.Provider == "openai" {
		p.builtin = "openai"
	} else {
		p.config = cfg.Providers[cfg.Provider]
		if p.config == nil {
			return p, fmt.Errorf("missing provider")
		}
		wire, _ := p.config["wire_api"].(string)
		if wire != "" && wire != "responses" {
			return p, fmt.Errorf("unsupported wire API")
		}
		// Each provider owns its bearer token even when CCSwitch calls all of them custom.
		applyAPIKey(p.config, settings.Auth)
	}
	seen := map[string]bool{}
	for _, entry := range settings.Catalog.Models {
		if entry.Model == "" || seen[entry.Model] {
			continue
		}
		seen[entry.Model] = true
		m := Model{ID: entry.Model, Model: entry.Model, DisplayName: entry.DisplayName, IsDefault: entry.Model == cfg.Model, DefaultReasoningEffort: entry.DefaultLevel, SupportedReasoningEfforts: []ReasoningEffort{}}
		if m.DisplayName == "" {
			m.DisplayName = m.Model
		}
		for _, level := range entry.Levels {
			m.SupportedReasoningEfforts = append(m.SupportedReasoningEfforts, ReasoningEffort{ReasoningEffort: level, Description: level})
		}
		p.Models = append(p.Models, m)
	}
	if cfg.Model != "" && !seen[cfg.Model] {
		p.Models = append(p.Models, Model{ID: cfg.Model, Model: cfg.Model, DisplayName: cfg.Model, IsDefault: true, SupportedReasoningEfforts: []ReasoningEffort{}})
	}
	return p, nil
}

// Discover merges the advertised API models with configured metadata. Failure keeps
// the saved catalog usable. Each request uses only that provider's credentials.
func Discover(ctx context.Context, items []Provider) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for i := range items {
		if items[i].Official() || items[i].Warning != "" {
			continue
		}
		wg.Add(1)
		go func(p *Provider) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			if err := p.discover(ctx); err != nil {
				p.Warning = "在线模型列表暂不可用，显示 CCSwitch 已保存的模型"
			}
		}(&items[i])
	}
	wg.Wait()
}

func (p *Provider) discover(ctx context.Context) error {
	base, _ := p.config["base_url"].(string)
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("invalid endpoint")
	}
	paths := []string{strings.TrimRight(u.Path, "/") + "/models"}
	if u.Path == "" || u.Path == "/" {
		paths = append(paths, "/v1/models")
	}
	if params, ok := p.config["query_params"].(map[string]any); ok {
		query := u.Query()
		for name, value := range params {
			if text, ok := value.(string); ok {
				query.Set(name, text)
			}
		}
		u.RawQuery = query.Encode()
	}
	client := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, path := range paths {
		u.Path = path
		req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if err != nil {
			return fmt.Errorf("invalid request")
		}
		if headers, ok := p.config["http_headers"].(map[string]any); ok {
			for name, value := range headers {
				if text, ok := value.(string); ok {
					req.Header.Set(name, text)
				}
			}
		}
		if headers, ok := p.config["env_http_headers"].(map[string]any); ok {
			for name, value := range headers {
				if variable, ok := value.(string); ok {
					if text := os.Getenv(variable); text != "" {
						req.Header.Set(name, text)
					}
				}
			}
		}
		key, _ := p.config["experimental_bearer_token"].(string)
		if envKey, _ := p.config["env_key"].(string); key == "" && envKey != "" {
			key = os.Getenv(envKey)
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("discovery unavailable")
		}
		var data struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&data)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil || len(data.Data) == 0 {
			continue
		}
		seen := map[string]bool{}
		for _, m := range p.Models {
			seen[m.Model] = true
		}
		for _, m := range data.Data {
			if m.ID != "" && !seen[m.ID] {
				seen[m.ID] = true
				p.Models = append(p.Models, Model{ID: m.ID, Model: m.ID, DisplayName: m.ID, SupportedReasoningEfforts: []ReasoningEffort{}})
			}
		}
		return nil
	}
	return fmt.Errorf("no model catalog")
}
