package agenthost

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (h *Host) wxContacts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	account, err := h.wxRequestedAccount(ctx, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	query := url.Values{}
	if keyword := strings.TrimSpace(r.URL.Query().Get("keyword")); keyword != "" {
		query.Set("keyword", keyword)
	}
	if kind := strings.TrimSpace(r.URL.Query().Get("type")); kind != "" {
		query.Set("type", kind)
	}
	page := r.URL.Query().Get("page_index")
	if page == "" {
		page = "1"
	}
	if n, err := strconv.Atoi(page); err != nil || n < 1 || n > 10000 {
		failure(w, errors.New("页码无效"))
		return
	}
	size := r.URL.Query().Get("page_size")
	if size == "" {
		size = "20"
	}
	if n, err := strconv.Atoi(size); err != nil || n < 1 || n > 100 {
		failure(w, errors.New("分页大小无效"))
		return
	}
	query.Set("page_index", page)
	query.Set("page_size", size)
	var result map[string]any
	if err := account.call(ctx, http.MethodGet, "contacts?"+query.Encode(), nil, &result); err != nil {
		failure(w, err)
		return
	}
	if result == nil {
		result = map[string]any{}
	}
	if inst := h.loadWXInstance(); inst != nil && inst.LastContactSync != nil {
		if ts, ok := inst.LastContactSync[account.ID]; ok && !ts.IsZero() {
			result["syncedAt"] = ts.UTC().Format(time.RFC3339)
		}
	}
	respond(w, 200, result)
}

func (h *Host) wxSyncContacts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	account, err := h.wxRequestedAccount(ctx, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	ts, err := h.syncWXContacts(ctx, account)
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, map[string]any{"ok": true, "syncedAt": ts})
}

func (h *Host) syncWXContacts(ctx context.Context, account wxAccount) (string, error) {
	if err := account.call(ctx, http.MethodPost, "contacts/sync", map[string]any{"sync_members": true}, nil); err != nil {
		return "", err
	}
	synced := time.Now().UTC()
	err := h.updateWXInstance(func(inst *wxInstance) *wxInstance {
		if inst == nil {
			inst = &wxInstance{Mode: "linked"}
		}
		if inst.LastContactSync == nil {
			inst.LastContactSync = map[string]time.Time{}
		}
		inst.LastContactSync[account.ID] = synced
		return inst
	})
	if err != nil {
		return "", err
	}
	return synced.Format(time.RFC3339), nil
}

func (h *Host) wxRequestedAccount(ctx context.Context, r *http.Request) (wxAccount, error) {
	account := inspectWXAccount(ctx, r.URL.Query().Get("id"))
	if !account.LoggedIn || account.WxID != r.URL.Query().Get("wxid") {
		return wxAccount{}, errors.New("微信账号未登录或已更换")
	}
	return account, nil
}

func (h *Host) wxGroupMembers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("group")
	if len(id) > 64 || !strings.HasSuffix(id, "@chatroom") {
		failure(w, errors.New("群聊 ID 无效"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	account, err := h.wxRequestedAccount(ctx, r)
	if err != nil {
		failure(w, err)
		return
	}
	if r.Method == http.MethodPost {
		if err := account.call(ctx, http.MethodPost, "chat-room/members/sync", map[string]string{"chat_room_id": id}, nil); err != nil {
			failure(w, err)
			return
		}
	}
	var rows []struct {
		ID       string `json:"wechat_id"`
		Nickname string `json:"nickname"`
		Remark   string `json:"remark"`
	}
	if err := account.call(ctx, http.MethodGet, "chat-room/not-left-members?"+url.Values{"chat_room_id": {id}}.Encode(), nil, &rows); err != nil {
		failure(w, err)
		return
	}
	respond(w, 200, rows)
}
