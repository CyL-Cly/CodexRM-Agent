package agenthost

import "net/http"

func (h *Host) wxWorkspaces(w http.ResponseWriter, r *http.Request) {
	a, _, finish, err := h.currentWX(r.Context())
	if err != nil {
		failure(w, err)
		return
	}
	defer finish()
	data, err := a.WXWorkspaces()
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, data)
}
