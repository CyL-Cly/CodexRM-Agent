package agenthost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"time"

	"github.com/CyL-Cly/CodexRM-Server/server"
)

const wxRelayPort = "17332"

// Start a relay on the host end of the dedicated Docker bridge so containers
// can pair even when the Agent's main connection uses a public relay.
func (h *Host) ensureWXRelay(ctx context.Context) error {
	h.wxRelayMu.Lock()
	defer h.wxRelayMu.Unlock()
	h.mu.Lock()
	configured, runCtx := h.wxLocalServer != "", h.ctx
	h.mu.Unlock()
	if configured {
		return nil
	}
	if runCtx == nil || runCtx.Err() != nil {
		return errors.New("Agent 尚未启动")
	}
	docker, err := wxLookPath("docker")
	if err != nil {
		return err
	}
	raw, err := wxCommandOutput(ctx, docker, wxDockerArgs("network", "inspect", "--format", `{{range .IPAM.Config}}{{println .Gateway .Subnet}}{{end}}`, "wechat-robot")...)
	if err != nil {
		return errors.New("无法读取 wxbot 专用 Docker 网络")
	}
	var gateway netip.Addr
	var subnet netip.Prefix
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		addr, addrErr := netip.ParseAddr(fields[0])
		prefix, prefixErr := netip.ParsePrefix(fields[1])
		if addrErr == nil && prefixErr == nil && addr.Is4() && addr.IsPrivate() && prefix.Contains(addr) {
			gateway, subnet = addr, prefix
			break
		}
	}
	if !gateway.IsValid() {
		return errors.New("wxbot Docker 网络缺少可用的 IPv4 网关")
	}
	bind := net.JoinHostPort(gateway.String(), wxRelayPort)
	listener, err := net.Listen("tcp", bind)
	if err != nil {
		return fmt.Errorf("无法启动 wxbot 本机中继: %w", err)
	}
	key := settingsSecret()
	relay, err := server.New(server.Config{DataDir: filepath.Join(h.StateDir(), "wx-relay"), AdminToken: key})
	if err != nil {
		_ = listener.Close()
		return err
	}
	handler := relay.Handler()
	httpServer := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, parseErr := netip.ParseAddrPort(r.RemoteAddr)
		if parseErr != nil || !subnet.Contains(ip.Addr()) {
			http.NotFound(w, r)
			return
		}
		path := r.URL.Path
		switch path {
		case "/api/v1/health", "/api/v1/client/claim", "/api/v1/client/ws", "/api/v1/client/revoke":
		default:
			if ip.Addr() != gateway || !strings.HasPrefix(path, "/api/v1/agent/") {
				http.NotFound(w, r)
				return
			}
		}
		handler.ServeHTTP(w, r)
	})}
	h.mu.Lock()
	h.wxLocalServer, h.wxLocalEnrollment = "http://"+bind, key
	h.mu.Unlock()
	go func() { _ = httpServer.Serve(listener) }()
	go func() { <-runCtx.Done(); _ = httpServer.Close(); relay.Close() }()
	select {
	case h.changed <- struct{}{}:
	default:
	}
	return nil
}
