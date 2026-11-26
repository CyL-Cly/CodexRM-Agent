package agenthost

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Agent/internal/clientconfig"
)

// SetWXLocalRelay configures a same-host relay before Run. It can keep existing
// robot authorizations usable while the main connection uses a different relay.
func (h *Host) SetWXLocalRelay(raw string) error {
	relay, err := clientconfig.ServerURL(raw)
	if err != nil {
		return err
	}
	if !wxLocalRelay(relay) {
		return errors.New("微信机器人中继必须是本机的私有 IP 地址")
	}
	h.wxLocalServer = relay
	return nil
}

func (h *Host) runWXConnection(ctx context.Context, version uint64, relay, key string) {
	cfg := h.base
	h.mu.Lock()
	selected := h.config
	h.mu.Unlock()
	cfg = withLocalSettings(cfg, selected)
	cfg.ServerURL, cfg.EnrollmentKey = relay, key
	cfg.OneBotSettingsPath = "" // The primary Agent owns the local OneBot listener.
	cfg.CheckForUpdate = nil
	cfg.NoAutoPair = true
	cfg.SecureRelay = false // Compatibility with the separate same-host robot relay.
	a, err := agent.NewForServer(cfg)
	if err != nil {
		log.Printf("WeChat relay: %v", err)
		return
	}
	h.mu.Lock()
	if h.version != version || ctx.Err() != nil {
		h.mu.Unlock()
		return
	}
	h.wxActive, h.wxActiveCtx, h.wxActiveVersion = a, ctx, version
	h.mu.Unlock()
	for ctx.Err() == nil {
		err := a.Run(ctx)
		if ctx.Err() != nil {
			return
		}
		log.Printf("WeChat relay: %v", err)
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

func (h *Host) currentWX(ctx context.Context) (*agent.Agent, context.Context, func(), error) {
	h.mu.Lock()
	if h.wxLocalServer == "" || h.wxLocalServer == h.config.Server {
		h.mu.Unlock()
		return h.current(ctx)
	}
	defer h.mu.Unlock()
	if h.wxActive == nil || h.wxActiveVersion != h.version || h.wxActiveCtx.Err() != nil {
		return nil, nil, nil, errors.New("微信机器人尚未连接到本机中继，请稍后重试")
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	stop := context.AfterFunc(h.wxActiveCtx, cancel)
	return h.wxActive, requestCtx, func() { stop(); cancel() }, nil
}

func (h *Host) waitWXAgent(ctx context.Context) (*agent.Agent, context.Context, func(), error) {
	for {
		a, requestCtx, done, err := h.currentWX(ctx)
		if err == nil {
			return a, requestCtx, done, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil, nil, err
		case <-time.After(300 * time.Millisecond):
		}
	}
}
