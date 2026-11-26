package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
)

// Calls and idle release share a per-thread lock. Never hold it in the reader
// callback: an RPC needs that reader to deliver its response.
func (a *Agent) threadQueue(threadID string) *messageQueue {
	a.queueMu.Lock()
	defer a.queueMu.Unlock()
	return a.queueLocked(threadID)
}

func (a *Agent) callThread(ctx context.Context, app *appserver.Client, owner, method string, params map[string]any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	if method == "thread/turns/list" {
		page, err := threadHistoryPage(ctx, app, params)
		if err != nil {
			return nil, err
		}
		return mustJSON(page), nil
	}
	id, _ := params["threadId"].(string)
	var q *messageQueue
	if id != "" {
		q = a.threadQueue(id)
		q.RPCMu.Lock()
		defer q.RPCMu.Unlock()
	}
	if id != "" && (method == "thread/resume" || method == "turn/start" || method == switchProviderMethod) {
		defer func() {
			a.textMu.Lock()
			textApp := a.textApp
			a.textMu.Unlock()
			if app == textApp {
				return
			}
			a.queueMu.Lock()
			lifetime := a.queueCtx
			a.queueMu.Unlock()
			if lifetime != nil {
				go a.releaseIdleThread(lifetime, app, id)
			}
		}()
	}
	if method == switchProviderMethod {
		return a.switchThreadProvider(ctx, app, id, q, params)
	}
	if q != nil && (method == "turn/start" || method == "turn/steer") {
		a.queueMu.Lock()
		unavailable := q.ProviderUnavailable
		a.queueMu.Unlock()
		if unavailable {
			return nil, errProviderUnavailable
		}
	}
	// Keep caller-supplied runtime settings, before provider preparation adds
	// credentials. Every automatic resume reloads current provider credentials.
	settings := resumeSettings(params)
	var route threadProviderRoute
	if method == "turn/start" && q != nil {
		var err error
		route, err = a.providerRoute(id)
		if err != nil {
			return nil, err
		}
		if route.Model != "" && params["model"] == nil {
			params["model"] = route.Model
		}
		if err := a.resumeDetachedThread(ctx, app, id, q); err != nil {
			return nil, requestNotSent(err)
		}
	}
	_, explicitProvider := params["codexrmProvider"]
	prepareUserInput(method, params)
	if err := a.prepareProvider(ctx, app, method, params); err != nil {
		if method == "thread/resume" && q != nil && !explicitProvider && errors.Is(err, errProviderUnavailable) {
			// Opening history is safe, but must not load the thread with the
			// current/default provider or enable sending through stale credentials.
			a.queueMu.Lock()
			q.ProviderUnavailable = true
			if q.ResumeParams == nil {
				q.ResumeParams = settings
			} else {
				for key, value := range settings {
					q.ResumeParams[key] = value
				}
			}
			a.queueMu.Unlock()
			return a.openWithoutProvider(ctx, app, id, params["excludeTurns"] == true)
		}
		return nil, requestNotSent(err)
	}
	if method == "turn/start" && id != "" {
		a.mu.Lock()
		a.owners[id] = owner
		a.mu.Unlock()
	}
	excludeTurns := params["excludeTurns"]
	loadHistory := method == "thread/resume" && id != "" && (excludeTurns == nil || excludeTurns == false)
	if loadHistory {
		params["excludeTurns"] = true
	}
	raw, err := app.Call(ctx, method, params)
	var historyErr error
	if err == nil && loadHistory {
		var hydrated json.RawMessage
		hydrated, historyErr = hydrateResumedThread(ctx, app, raw)
		if historyErr == nil {
			raw = hydrated
		}
	}
	if err == nil {
		switch method {
		case "thread/fork", "thread/archive", "thread/unarchive", "turn/start":
			threadID, _ := params["threadId"].(string)
			a.emptyForks(method, threadID, raw)
		}
		a.recordStartedTurn(method, params, raw)
		if model, _ := params["model"].(string); route.ProviderID != "" && model != "" && model != route.Model {
			route.Model = model
			if err := a.saveProviderRoute(id, route); err != nil {
				log.Printf("save model for thread %s: %v", id, err)
			}
		}
		if method == "thread/start" || method == "thread/resume" || method == "thread/fork" {
			var out struct {
				Thread struct {
					ID        string
					Ephemeral bool
					Status    struct{ Type string }
					Turns     []struct{ ID, Status string }
				}
			}
			if json.Unmarshal(raw, &out) == nil && out.Thread.ID != "" {
				id = out.Thread.ID
				a.queueMu.Lock()
				loaded := a.queueLocked(id)
				loaded.Loaded, loaded.Detached = true, false
				loaded.ProviderUnavailable = false
				loaded.Ephemeral = out.Thread.Ephemeral
				if loaded.ResumeParams == nil {
					loaded.ResumeParams = map[string]any{}
				}
				for key, value := range settings {
					loaded.ResumeParams[key] = value
				}
				// A resume can join an existing running thread. Do not release it
				// simply because its turn/started notification predates our join.
				if out.Thread.Status.Type == "active" {
					for _, turn := range out.Thread.Turns {
						if turn.Status == "inProgress" && loaded.LastCompleted != turn.ID {
							loaded.ActiveTurn = turn.ID
						}
					}
				}
				a.queueMu.Unlock()
				a.mu.Lock()
				a.owners[id] = owner
				a.mu.Unlock()
			}
		}
		if method == "turn/start" && q != nil {
			a.queueMu.Lock()
			q.Loaded = true
			a.queueMu.Unlock()
		}
		if method == "thread/start" || method == "thread/resume" || method == "thread/fork" || method == "turn/start" {
			a.publishThreadState("loaded", id)
		}
	}
	if historyErr != nil {
		return nil, historyErr
	}
	if err == nil && method == "thread/resume" {
		return a.withProviderRoute(id, raw)
	}
	return raw, err
}

func (a *Agent) openWithoutProvider(ctx context.Context, app *appserver.Client, id string, excludeTurns bool) (json.RawMessage, error) {
	params := map[string]any{"threadId": id, "includeTurns": false}
	var raw json.RawMessage
	var err error
	if excludeTurns {
		raw, err = app.Call(ctx, "thread/read", params)
		if err == nil {
			raw, err = a.withProviderRoute(id, raw)
		}
	} else {
		raw, err = a.threadSnapshot(ctx, app, params)
	}
	if err != nil {
		return nil, err
	}
	var result map[string]json.RawMessage
	var thread map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(result["thread"], &thread); err != nil {
		return nil, err
	}
	if thread == nil {
		return nil, errors.New("thread/read returned no thread")
	}
	thread["codexrmProviderUnavailable"] = mustJSON(true)
	result["thread"] = mustJSON(thread)
	return mustJSON(result), nil
}

// Uncertainty about a prerequisite RPC does not imply we submitted the requested operation.
func requestNotSent(err error) error {
	var unknown *appserver.OutcomeUnknown
	if errors.As(err, &unknown) {
		return &appserver.RPCError{Code: -32000, Message: err.Error()}
	}
	return err
}

func resumeSettings(params map[string]any) map[string]any {
	settings := map[string]any{}
	for _, key := range []string{"serviceTier", "cwd", "runtimeWorkspaceRoots", "approvalPolicy", "approvalsReviewer", "sandbox", "permissions", "config", "baseInstructions", "developerInstructions"} {
		if value, ok := params[key]; ok {
			settings[key] = value
		}
	}
	// Maps such as config will be mutated while preparing a provider.
	var copy map[string]any
	_ = json.Unmarshal(mustJSON(settings), &copy)
	// Model overrides only apply to the caller's request. Automatic resumes
	// recover the current model settings from Codex, just like a fresh open.
	if config, ok := copy["config"].(map[string]any); ok {
		delete(config, "model")
		delete(config, "model_provider")
		delete(config, "model_reasoning_effort")
	}
	return copy
}

// The caller holds q.RPCMu. The user's next message restores the thread from
// disk, including any turns completed by the desktop since our last release.
func (a *Agent) resumeDetachedThread(ctx context.Context, app *appserver.Client, id string, q *messageQueue) error {
	a.queueMu.Lock()
	detached := q.Detached
	params := resumeSettings(q.ResumeParams)
	a.queueMu.Unlock()
	if !detached {
		return nil
	}
	params["threadId"], params["excludeTurns"] = id, true
	prepareUserInput("thread/resume", params)
	if err := a.prepareProvider(ctx, app, "thread/resume", params); err != nil {
		return err
	}
	if _, err := app.Call(ctx, "thread/resume", params); err != nil {
		return err
	}
	a.queueMu.Lock()
	q.Loaded, q.Detached = true, false
	// Snapshots taken while detached may have observed a desktop-owned turn.
	q.Known, q.ActiveTurn = false, ""
	a.queueMu.Unlock()
	return nil
}

func (a *Agent) releaseIdleThread(ctx context.Context, app *appserver.Client, id string) {
	if app == nil || ctx.Err() != nil {
		return
	}
	q := a.threadQueue(id)
	q.RPCMu.Lock()
	defer q.RPCMu.Unlock()
	a.queueMu.Lock()
	// Codex keeps session approvals in the loaded thread. Its rollout does
	// not restore them after unsubscribe/resume.
	busy := !q.Loaded || q.Detached || q.Ephemeral || q.SessionApproval || q.ActiveTurn != "" || q.Starting || (!q.Paused && len(q.Entries) > 0)
	a.queueMu.Unlock()
	if busy || ctx.Err() != nil {
		return
	}
	// Optional questions can remain answerable after turn/completed.
	a.mu.Lock()
	for _, pending := range a.approvals {
		if pending.ThreadID == id {
			a.mu.Unlock()
			return
		}
	}
	a.mu.Unlock()
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// An excludeTurns resume can join an active thread without returning its
	// turn ID. Confirm the live state before unsubscribing in that case too.
	raw, err := app.Call(callCtx, "thread/read", map[string]any{"threadId": id, "includeTurns": false})
	if err != nil {
		log.Printf("read idle thread %s before release: %v", id, err)
		return
	}
	var snapshot struct {
		Thread struct{ Status struct{ Type string } }
	}
	if json.Unmarshal(raw, &snapshot) != nil || snapshot.Thread.Status.Type != "idle" {
		return
	}
	if err := a.unloadThread(callCtx, app, id, q); err != nil {
		log.Printf("release idle thread %s: %v", id, err)
	}
}

// The caller holds q.RPCMu and has verified the thread is idle.
func (a *Agent) unloadThread(ctx context.Context, app *appserver.Client, id string, q *messageQueue) error {
	a.queueMu.Lock()
	unloaded := make(chan struct{})
	q.Unloaded = unloaded
	a.queueMu.Unlock()
	defer func() {
		a.queueMu.Lock()
		if q.Unloaded == unloaded {
			q.Unloaded = nil
		}
		a.queueMu.Unlock()
	}()
	raw, err := app.Call(ctx, "thread/unsubscribe", map[string]any{"threadId": id})
	if err != nil {
		return err
	}
	var result struct{ Status string }
	_ = json.Unmarshal(raw, &result)
	if result.Status == "unsubscribed" && a.cfg.AppServerURL == "" {
		// Unsubscribe acknowledges before shutdown finishes. Keep the gate
		// until thread/closed so an immediate next message can safely resume.
		select {
		case <-unloaded:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	a.queueMu.Lock()
	q.Detached, q.Loaded, q.Known = true, false, false
	q.SessionApproval = false
	a.queueMu.Unlock()
	a.mu.Lock()
	delete(a.owners, id)
	a.mu.Unlock()
	a.publishThreadState("detached", id)
	return err
}
