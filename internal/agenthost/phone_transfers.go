package agenthost

import (
	"errors"
	"net/http"
	"net/netip"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Agent/internal/desktop"
)

// The Windows shell action and transfer cache are local to the Agent computer,
// even if the rest of the settings page was deliberately exposed on the LAN.
func localFileTransferSettings(r *http.Request) bool {
	peer, peerErr := netip.ParseAddrPort(r.RemoteAddr)
	host, hostErr := netip.ParseAddrPort(r.Host)
	return desktop.ReceivesFiles && peerErr == nil && hostErr == nil &&
		peer.Addr().Unmap().IsLoopback() && host.Addr().Unmap().IsLoopback()
}

func (h *Host) registerPhoneTransferRoutes(mux *http.ServeMux) {
	if !desktop.ReceivesFiles {
		return
	}
	local := func(handler http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !localFileTransferSettings(r) {
				http.Error(w, "请从本机托盘打开文件传输助手", http.StatusForbidden)
				return
			}
			handler(w, r)
		}
	}
	mux.HandleFunc("GET /api/phone-transfers", local(h.phoneTransferLists))
	mux.HandleFunc("DELETE /api/phone-transfers/outbox/{bucket}/{id}", local(h.cancelPhoneTransfer))
	mux.HandleFunc("POST /api/phone-transfers/inbox/{id}/open", local(h.openReceivedPhoneTransfer))
}

func (h *Host) phoneTransferLists(w http.ResponseWriter, r *http.Request) {
	a, _, cancel, err := h.current(r.Context())
	if err != nil {
		failure(w, err)
		return
	}
	defer cancel()
	outbox, err := a.PendingPhoneTransfers()
	if err != nil {
		failure(w, err)
		return
	}
	inbox, err := a.ReceivedPhoneFiles()
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"outbox": outbox, "inbox": inbox})
}

func (h *Host) cancelPhoneTransfer(w http.ResponseWriter, r *http.Request) {
	a, ctx, cancel, err := h.current(r.Context())
	if err != nil {
		failure(w, err)
		return
	}
	defer cancel()
	if ctx.Err() != nil {
		failure(w, ctx.Err())
		return
	}
	if err := a.CancelPhoneTransfer(r.PathValue("bucket"), r.PathValue("id")); err != nil {
		if errors.Is(err, agent.ErrPhoneTransferNotPending) {
			respond(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		failure(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{})
}

func (h *Host) openReceivedPhoneTransfer(w http.ResponseWriter, r *http.Request) {
	a, ctx, cancel, err := h.current(r.Context())
	if err != nil {
		failure(w, err)
		return
	}
	defer cancel()
	if ctx.Err() != nil {
		failure(w, ctx.Err())
		return
	}
	if err := a.OpenReceivedPhoneFile(r.PathValue("id")); err != nil {
		failure(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{})
}
