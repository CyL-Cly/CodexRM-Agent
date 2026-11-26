package agenthost

import (
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"strings"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Agent/internal/onebot"
)

const oneBotSupported = runtime.GOOS == "linux"

func (h *Host) oneBotPath() string { return h.base.OneBotSettingsPath }

func (h *Host) oneBotSnapshot() (map[string]any, error) {
	settings, err := agent.LoadOneBotSettings(h.oneBotPath())
	loadError := ""
	if err != nil {
		// Keep the settings page usable so a malformed local file can be replaced.
		loadError = err.Error()
		settings = agent.DefaultOneBotSettings()
	}
	tokenSet := settings.AccessToken != ""
	settings.AccessToken = ""
	settings.ClearAccessToken = false
	status := map[string]any{"enabled": false, "state": "disabled"}
	h.mu.Lock()
	active := h.active
	if h.activeVersion != h.version {
		active = nil
	}
	h.mu.Unlock()
	if active != nil {
		status = active.OneBotStatus()
	} else if settings.Enabled {
		status = map[string]any{"enabled": true, "state": "connecting", "error": "等待 Codex app-server 就绪"}
	}
	return map[string]any{"settings": settings, "accessTokenSet": tokenSet, "status": status, "loadError": loadError}, nil
}

func (h *Host) oneBotSettings(w http.ResponseWriter, r *http.Request) {
	if !oneBotSupported {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		snapshot, err := h.oneBotSnapshot()
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, http.StatusOK, snapshot)
	case http.MethodPut:
		h.opMu.Lock()
		defer h.opMu.Unlock()
		existing, err := agent.LoadOneBotSettings(h.oneBotPath())
		if err != nil {
			existing = agent.DefaultOneBotSettings()
		}
		var input agent.OneBotSettings
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			failure(w, err)
			return
		}
		if input.ClearAccessToken {
			existing.AccessToken = ""
		}
		if input.AccessToken == "" {
			input.AccessToken = existing.AccessToken
		}
		input.ClearAccessToken = false
		if err := agent.SaveOneBotSettings(h.oneBotPath(), input); err != nil {
			failure(w, err)
			return
		}
		h.mu.Lock()
		active := h.active
		if h.activeVersion != h.version {
			active = nil
		}
		h.mu.Unlock()
		reloadError := ""
		if active != nil {
			if err := active.ReloadOneBot(input); err != nil {
				reloadError = err.Error()
			}
		}
		snapshot, err := h.oneBotSnapshot()
		if err != nil {
			failure(w, err)
			return
		}
		if reloadError != "" {
			snapshot["reloadError"] = reloadError
		}
		respond(w, http.StatusOK, snapshot)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *Host) oneBotTest(w http.ResponseWriter, r *http.Request) {
	if !oneBotSupported {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		UserID  int64  `json:"userId"`
		GroupID int64  `json:"groupId"`
		Text    string `json:"text"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		failure(w, err)
		return
	}
	input.Text = strings.TrimSpace(input.Text)
	if input.Text == "" || len([]rune(input.Text)) > 4000 {
		failure(w, errors.New("测试消息必须为 1 到 4000 字"))
		return
	}
	a, ctx, cancel, err := h.current(r.Context())
	if err != nil {
		failure(w, err)
		return
	}
	defer cancel()
	if err := a.SendOneBotTest(ctx, onebot.Target{UserID: input.UserID, GroupID: input.GroupID}, input.Text); err != nil {
		failure(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]bool{"ok": true})
}
