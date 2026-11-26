package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
)

func (h *Host) providerSettings(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil || !json.Valid(body) {
		failure(w, errors.New("无效的供应商设置请求"))
		return
	}
	active, ctx, done, err := h.current(r.Context())
	operation := r.PathValue("action")
	var result any
	if err == nil {
		defer done()
		result, err = active.LocalProviderSettings(ctx, operation, body)
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		result, err = agent.OfflineProviderSettings(ctx, h.base, operation, body)
	}
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, http.StatusOK, result)
}
