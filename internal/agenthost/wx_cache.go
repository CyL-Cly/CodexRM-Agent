package agenthost

import (
	"encoding/json"
	"errors"
	"net/http"
)

func (h *Host) wxRecordCache(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Clean bool
		Days  int
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			failure(w, errors.New("缓存参数无效"))
			return
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	a, _, done, err := h.currentWX(r.Context())
	if err != nil {
		failure(w, err)
		return
	}
	defer done()
	usage, err := a.WXRecordCache(input.Clean, input.Days)
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, usage)
}
