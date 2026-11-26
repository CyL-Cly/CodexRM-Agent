package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
	"github.com/CyL-Cly/CodexRM-Agent/internal/providers"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

const switchProviderMethod = "codexrm/thread/switchProvider"

func (a *Agent) providerRoutePath(id string) string {
	return filepath.Join(a.cfg.DataDir, "provider-routes", fmt.Sprintf("%x.json", sha256.Sum256([]byte(id))))
}

type threadProviderRoute struct {
	ProviderID string `json:"providerId"`
	Model      string `json:"model,omitempty"`
}

func (a *Agent) providerRoute(id string) (threadProviderRoute, error) {
	var route threadProviderRoute
	if id == "" || a.cfg.DataDir == "" || a.cfg.Mock || a.cfg.AppServerURL != "" {
		return route, nil
	}
	raw, err := os.ReadFile(a.providerRoutePath(id))
	if errors.Is(err, os.ErrNotExist) {
		return route, nil
	}
	if err != nil {
		return route, fmt.Errorf("无法读取会话接入点: %w", err)
	}
	// Older agents saved just the provider ID.
	if err := json.Unmarshal(raw, &route.ProviderID); err != nil {
		if err := json.Unmarshal(raw, &route); err != nil {
			return route, fmt.Errorf("无法解析会话接入点: %w", err)
		}
	}
	return route, nil
}

func (a *Agent) saveProviderRoute(id string, route threadProviderRoute) error {
	if route.Model == "" {
		return clientconfig.WriteJSON(a.providerRoutePath(id), route.ProviderID)
	}
	return clientconfig.WriteJSON(a.providerRoutePath(id), route)
}

func (a *Agent) withProviderRoute(id string, raw json.RawMessage) (json.RawMessage, error) {
	route, err := a.providerRoute(id)
	if route.ProviderID == "" || err != nil {
		return raw, err
	}
	var result map[string]json.RawMessage
	var thread map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(result["thread"], &thread); err != nil {
		return nil, err
	}
	thread["codexrmProvider"] = mustJSON(route.ProviderID)
	result["thread"] = mustJSON(thread)
	return mustJSON(result), nil
}

// The caller holds q.RPCMu, so no turn can start between checking the model,
// unloading the old connection, and saving the new route.
func (a *Agent) switchThreadProvider(ctx context.Context, app *appserver.Client, id string, q *messageQueue, params map[string]any) (json.RawMessage, error) {
	if a.cfg.Mock || a.cfg.AppServerURL != "" || a.cfg.DataDir == "" {
		return nil, errors.New("此 Agent 无法切换本机供应商接入点")
	}
	providerID, _ := params["providerId"].(string)
	model, _ := params["model"].(string)
	if id == "" || providerID == "" || strings.TrimSpace(model) == "" {
		return nil, errors.New("切换接入点需要指定会话、供应商和模型")
	}
	a.queueMu.Lock()
	busy := q.ActiveTurn != "" || q.Starting || len(q.Entries) != 0
	resume := resumeSettings(q.ResumeParams)
	a.queueMu.Unlock()
	a.mu.Lock()
	for _, pending := range a.approvals {
		if pending.ThreadID == id {
			busy = true
			break
		}
	}
	a.mu.Unlock()
	if busy {
		return nil, errors.New("请在当前任务、审批和排队消息处理完后切换接入点")
	}
	items, err := a.loadProviders(ctx)
	if err != nil {
		return nil, err
	}
	var target providers.Provider
	for _, p := range items {
		if p.ID == providerID && p.Warning == "" && !p.Official() {
			target = p
			break
		}
	}
	if target.ID == "" {
		return nil, errors.New("所选接入点不是可用的第三方供应商")
	}
	resume["threadId"], resume["excludeTurns"] = id, true
	prepareErr := a.prepareProvider(ctx, app, "thread/resume", resume)
	missingProvider := errors.Is(prepareErr, errProviderUnavailable)
	var raw json.RawMessage
	if missingProvider {
		// Validate the existing thread without reattaching the unavailable
		// provider. Not-loaded threads still have a durable provider identity.
		raw, err = app.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false})
	} else if prepareErr != nil {
		return nil, prepareErr
	} else {
		raw, err = app.Call(ctx, "thread/resume", resume)
		if err == nil {
			a.queueMu.Lock()
			q.Loaded, q.Detached = true, false
			a.queueMu.Unlock()
		}
	}
	if err != nil {
		return nil, err
	}
	var current struct {
		Thread struct {
			ID            string
			ModelProvider string
			Ephemeral     bool
			Status        struct{ Type string }
		}
	}
	if err := json.Unmarshal(raw, &current); err != nil {
		return nil, err
	}
	status := current.Thread.Status.Type
	if current.Thread.ID != id || current.Thread.Ephemeral || (status != "idle" && !(missingProvider && status == "notLoaded")) {
		return nil, errors.New("当前会话暂时无法切换接入点")
	}
	_, nativeRuntimeID := providers.CurrentProvider(items, a.cfg.CCSwitchDB)
	if current.Thread.ModelProvider == "" || (!missingProvider && resume["modelProvider"] == nil && current.Thread.ModelProvider != nativeRuntimeID) {
		return nil, errors.New("仅支持在第三方供应商会话中切换接入点")
	}
	unloadCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if status != "notLoaded" {
		if err := a.unloadThread(unloadCtx, app, id, q); err != nil {
			return nil, err
		}
	}
	// Persist both settings: Codex can restore the original model when it
	// reopens a thread whose provider identifier has not changed.
	route := threadProviderRoute{ProviderID: target.ID, Model: model}
	if err := a.saveProviderRoute(id, route); err != nil {
		return nil, fmt.Errorf("无法保存会话接入点: %w", err)
	}
	a.queueMu.Lock()
	q.Loaded, q.Detached, q.Known, q.ProviderUnavailable = false, true, false, false
	a.queueMu.Unlock()
	result := map[string]string{"threadId": id, "providerId": target.ID, "model": model}
	a.publishEvent(protocol.Envelope{Type: protocol.TypeServerMessage, Message: mustJSON(map[string]any{
		"method": "codexrm/thread/providerChanged", "params": result,
	})})
	return mustJSON(result), nil
}
