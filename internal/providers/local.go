package providers

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// LocalStore keeps provider connection settings on the Agent. Its records use
// the same format as imported Codex rows to preserve all connection options.
type LocalStore struct {
	Dir string
}

var localProvidersMu sync.Mutex
var errProviderNotFound = errors.New("找不到该模型供应商")

const localProvidersFile = "model-providers.json"

type localProvider struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Notes    string `json:"notes,omitempty"`
	Settings string `json:"settings,omitempty"`
	Deleted  bool   `json:"deleted,omitempty"`
}

func (s LocalStore) read() ([]localProvider, error) {
	if s.Dir == "" {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, localProvidersFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var records []localProvider
	if err != nil || json.Unmarshal(data, &records) != nil {
		return nil, errors.New("无法读取 Agent 的供应商配置")
	}
	return records, nil
}

func (s LocalStore) write(records []localProvider) error {
	failed := errors.New("无法保存 Agent 的供应商配置")
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil || s.Dir == "" || os.MkdirAll(s.Dir, 0700) != nil {
		return failed
	}
	file, err := os.CreateTemp(s.Dir, "providers-*.tmp")
	if err != nil {
		return failed
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil || os.Rename(file.Name(), filepath.Join(s.Dir, localProvidersFile)) != nil {
		return failed
	}
	return nil
}

// Load returns the visible local providers and the IDs suppressed from CC Switch.
func (s LocalStore) Load() ([]Provider, map[string]bool, error) {
	localProvidersMu.Lock()
	defer localProvidersMu.Unlock()
	records, err := s.read()
	if err != nil {
		return nil, nil, err
	}
	items := make([]Provider, 0, len(records))
	deleted := make(map[string]bool)
	for _, record := range records {
		if record.Deleted {
			deleted[providerID(record.ID)] = true
			continue
		}
		p, err := parse(record.ID, record.Name, record.Settings)
		if err != nil {
			p = Provider{ID: providerID(record.ID), Name: record.Name, Warning: "Agent 供应商配置无法读取"}
		}
		p.Source, p.Notes = "Agent", strings.TrimSpace(record.Notes)
		items = append(items, p)
	}
	return items, deleted, nil
}

// Details returns only editable public fields and a flag indicating where the
// record came from. The provider key is write-only in both management UIs.
func (s LocalStore) Details(id string) (ProviderDetails, bool, error) {
	localProvidersMu.Lock()
	defer localProvidersMu.Unlock()
	records, err := s.read()
	if err != nil {
		return ProviderDetails{}, false, err
	}
	for _, record := range records {
		if providerID(record.ID) == id {
			if record.Deleted {
				return ProviderDetails{}, true, errProviderNotFound
			}
			details, err := detailsForRow(providerRow{id: record.ID, name: record.Name, notes: record.Notes, settings: record.Settings}, true)
			return details, true, err
		}
	}
	return ProviderDetails{}, false, nil
}

func (s LocalStore) Save(patch ProviderPatch) (id string, found bool, err error) {
	patch, err = validateProviderPatch(patch)
	if err != nil {
		return "", false, err
	}
	localProvidersMu.Lock()
	defer localProvidersMu.Unlock()
	records, err := s.read()
	if err != nil {
		return "", false, err
	}
	if patch.ID == "" {
		if patch.Name == nil || patch.BaseURL == nil {
			return "", false, errors.New("请输入名称和接入地址")
		}
		id, err := newProviderID()
		if err != nil {
			return "", false, err
		}
		record := localProvider{ID: id, Settings: newProviderConfig}
		if err := record.apply(patch); err != nil {
			return "", false, err
		}
		return providerID(id), true, s.write(append(records, record))
	}
	for index := range records {
		if providerID(records[index].ID) != patch.ID {
			continue
		}
		if records[index].Deleted {
			return "", true, errProviderNotFound
		}
		if err := records[index].apply(patch); err != nil {
			return "", true, err
		}
		return patch.ID, true, s.write(records)
	}
	return "", false, nil
}

// Delete removes local connection settings and records the ID so a legacy
// CC Switch entry cannot reappear on refresh or during a later bulk import.
// It never writes to the CC Switch database.
func (s LocalStore) Delete(ctx context.Context, dbPath, id string) error {
	if id == "" {
		return errProviderNotFound
	}
	localProvidersMu.Lock()
	defer localProvidersMu.Unlock()
	records, err := s.read()
	if err != nil {
		return err
	}
	for index, record := range records {
		if providerID(record.ID) != id {
			continue
		}
		if record.Deleted {
			return nil
		}
		records[index] = localProvider{ID: record.ID, Deleted: true}
		return s.write(records)
	}
	// Legacy entries are still listed even if they have never been imported.
	db, err := openEditor(dbPath, false)
	if err != nil {
		return err
	}
	defer db.Close()
	row, err := findProviderRow(ctx, db, id)
	if err != nil {
		return err
	}
	return s.write(append(records, localProvider{ID: row.id, Deleted: true}))
}

func (p *localProvider) apply(patch ProviderPatch) error {
	if patch.Name != nil {
		p.Name = *patch.Name
	}
	if patch.Notes != nil {
		p.Notes = *patch.Notes
	}
	if patch.changesConfig() {
		settings, err := patchProviderConfig(p.Settings, patch)
		if err != nil {
			return err
		}
		p.Settings = settings
	}
	return nil
}

// ImportCCSwitch copies third-party providers while preserving their public IDs
// so existing sessions and model settings continue to resolve after migration.
func (s LocalStore) ImportCCSwitch(ctx context.Context, dbPath string) (int, error) {
	db, err := openEditor(dbPath, false)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT id, name, COALESCE(notes, ''), settings_config FROM providers WHERE app_type = 'codex' ORDER BY COALESCE(sort_index, 2147483647), rowid")
	if err != nil {
		return 0, errors.New("无法读取 CC Switch 供应商")
	}
	defer rows.Close()
	var imported []localProvider
	for rows.Next() {
		var record localProvider
		if err := rows.Scan(&record.ID, &record.Name, &record.Notes, &record.Settings); err != nil {
			return 0, errors.New("无法读取 CC Switch 供应商记录")
		}
		p, err := parse(record.ID, record.Name, record.Settings)
		if err == nil && !p.Official() {
			imported = append(imported, record)
		}
	}
	if rows.Err() != nil {
		return 0, errors.New("无法读取 CC Switch 供应商记录")
	}
	localProvidersMu.Lock()
	defer localProvidersMu.Unlock()
	records, err := s.read()
	if err != nil {
		return 0, err
	}
	// Deleted records also count as seen: bulk import must not undo deletion.
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		seen[record.ID] = true
	}
	count := 0
	for _, record := range imported {
		if !seen[record.ID] {
			seen[record.ID] = true
			count++
			records = append(records, record)
		}
	}
	if count == 0 {
		return 0, nil
	}
	if err := s.write(records); err != nil {
		return 0, err
	}
	return count, nil
}
