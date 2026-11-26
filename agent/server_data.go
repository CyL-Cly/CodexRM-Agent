package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// NewForServer treats an explicit data directory as a state root when switching
// relays. Keep existing credentials in place for their original relay.
func NewForServer(cfg Config) (*Agent, error) {
	if cfg.DataDir == "" {
		return New(cfg)
	}
	server := strings.TrimRight(cfg.ServerURL, "/")
	state, err := os.ReadFile(filepath.Join(cfg.DataDir, "agent.json"))
	if err == nil {
		var existing creds
		if err := json.Unmarshal(state, &existing); err != nil {
			return nil, fmt.Errorf("read existing agent credentials: %w", err)
		}
		if existing.ServerURL == "" || existing.ServerURL == server {
			return New(cfg)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	hash := sha256.Sum256([]byte(server))
	cfg.DataDir = filepath.Join(cfg.DataDir, "servers", hex.EncodeToString(hash[:]))
	return New(cfg)
}
