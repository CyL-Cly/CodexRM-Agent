package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
)

type conversationSummary struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Preview   string          `json:"preview"`
	Cwd       string          `json:"cwd"`
	Parent    string          `json:"forkedFromId"`
	Archived  bool            `json:"archived"`
	Status    json.RawMessage `json:"status,omitempty"`
	CreatedAt int64           `json:"createdAt,omitempty"`
	UpdatedAt int64           `json:"updatedAt,omitempty"`
}

func listConversations(ctx context.Context, app *appserver.Client, archived bool, cursor string) ([]conversationSummary, string, error) {
	params := map[string]any{"archived": archived, "limit": 64, "sortKey": "created_at"}
	if cursor != "" {
		params["cursor"] = cursor
	}
	raw, err := app.Call(ctx, "thread/list", params)
	if err != nil {
		return nil, "", err
	}
	var page struct {
		Data       []conversationSummary
		NextCursor string
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, "", err
	}
	if cursor != "" && cursor == page.NextCursor {
		return nil, "", errors.New("会话列表分页位置未更新")
	}
	for i := range page.Data {
		page.Data[i].Archived = archived
	}
	return page.Data, page.NextCursor, nil
}

func (a *Agent) conversationPins(id string, pinned *bool) ([]string, error) {
	a.conversationMu.Lock()
	defer a.conversationMu.Unlock()
	path := filepath.Join(a.cfg.DataDir, "conversation-pins.json")
	ids := []string{}
	raw, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(raw, &ids); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if pinned == nil || slices.Contains(ids, id) == *pinned {
		return ids, nil
	}
	if *pinned {
		if len(ids) >= 100 {
			return nil, errors.New("最多置顶 100 个会话")
		}
		ids = append([]string{id}, ids...)
	} else {
		ids = slices.DeleteFunc(ids, func(value string) bool { return value == id })
	}
	if err := os.WriteFile(path+".tmp", mustJSON(ids), 0600); err != nil {
		return nil, err
	}
	return ids, os.Rename(path+".tmp", path)
}

// Codex hides forks with no user turn from thread/list. Save their summaries
// so a first-turn fork can still be reached while its draft awaits confirmation.
func (a *Agent) emptyForks(method, id string, raw json.RawMessage) map[string]conversationSummary {
	a.conversationMu.Lock()
	defer a.conversationMu.Unlock()
	path := filepath.Join(a.cfg.DataDir, "conversation-forks.json")
	forks := map[string]conversationSummary{}
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &forks); err != nil {
			log.Printf("read conversation forks: %v", err)
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("read conversation forks: %v", err)
		return nil
	}
	switch method {
	case "thread/fork":
		var result struct {
			Thread struct {
				conversationSummary
				Ephemeral bool
			}
		}
		if json.Unmarshal(raw, &result) != nil || result.Thread.ID == "" || result.Thread.Parent == "" ||
			result.Thread.Preview != "" || result.Thread.Ephemeral {
			return forks
		}
		forks[result.Thread.ID] = result.Thread.conversationSummary
	case "thread/archive", "thread/unarchive":
		fork, ok := forks[id]
		if !ok {
			return forks
		}
		fork.Archived = method == "thread/archive"
		forks[id] = fork
	case "turn/start":
		if _, ok := forks[id]; !ok {
			return forks
		}
		// Once a user turn exists, Codex will include it in its own list.
		delete(forks, id)
	default:
		return forks
	}
	if err := os.WriteFile(path+".tmp", mustJSON(forks), 0600); err == nil {
		err = os.Rename(path+".tmp", path)
		if err != nil {
			log.Printf("save conversation forks: %v", err)
		}
	} else {
		log.Printf("save conversation forks: %v", err)
	}
	return forks
}

func (a *Agent) conversationInventory(ctx context.Context, app *appserver.Client, pins []string, relatedTo string) ([]conversationSummary, error) {
	summaries := map[string]conversationSummary{}
	for _, archived := range []bool{false, true} {
		if archived && relatedTo == "" {
			break
		}
		cursor := ""
		for {
			page, next, err := listConversations(ctx, app, archived, cursor)
			if err != nil {
				return nil, err
			}
			for _, thread := range page {
				if thread.ID != "" && (relatedTo != "" || slices.Contains(pins, thread.ID)) {
					summaries[thread.ID] = thread
				}
			}
			if next == "" || relatedTo == "" && len(summaries) == len(pins) {
				break
			}
			cursor = next
		}
	}
	for id, thread := range a.emptyForks("", "", nil) {
		if _, found := summaries[id]; !found && (relatedTo != "" || !thread.Archived && slices.Contains(pins, id)) {
			summaries[id] = thread
		}
	}
	result := []conversationSummary{}
	if relatedTo == "" {
		for _, id := range pins {
			if thread, ok := summaries[id]; ok {
				result = append(result, thread)
			}
		}
		return result, nil
	}
	// Build the connected group from the native parent links, including forks
	// already made on the desktop and siblings created before this feature.
	neighbors := map[string][]string{}
	for id, thread := range summaries {
		if thread.Parent != "" {
			neighbors[id] = append(neighbors[id], thread.Parent)
			neighbors[thread.Parent] = append(neighbors[thread.Parent], id)
		}
	}
	pending := []string{relatedTo}
	seen := map[string]bool{relatedTo: true}
	for len(pending) > 0 {
		id := pending[0]
		pending = pending[1:]
		if thread, ok := summaries[id]; ok {
			result = append(result, thread)
		}
		for _, neighbor := range neighbors[id] {
			if !seen[neighbor] {
				seen[neighbor] = true
				pending = append(pending, neighbor)
			}
		}
	}
	slices.SortFunc(result, func(a, b conversationSummary) int {
		if byName := strings.Compare(a.Name, b.Name); byName != 0 {
			return byName
		}
		return strings.Compare(a.ID, b.ID)
	})
	return result, nil
}

type conversationSearchCursor struct {
	Archived bool   `json:"archived,omitempty"`
	List     string `json:"list,omitempty"`
	Index    int    `json:"index,omitempty"`
	Thread   string `json:"thread,omitempty"`
	Turn     string `json:"turn,omitempty"`
	Item     int    `json:"item,omitempty"`
}

type conversationSearchHit struct {
	conversationSummary
	TurnID  string `json:"turnId"`
	ItemID  string `json:"itemId"`
	Snippet string `json:"snippet"`
}

type conversationSearchResult struct {
	Hits       []conversationSearchHit `json:"hits"`
	NextCursor string                  `json:"nextCursor,omitempty"`
	Skipped    int                     `json:"skipped,omitempty"`
}

func searchConversations(ctx context.Context, app *appserver.Client, query, token string) (conversationSearchResult, error) {
	result := conversationSearchResult{Hits: []conversationSearchHit{}}
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" || len(query) > 1024 || len(token) > 8192 {
		return result, &appserver.RPCError{Code: -32602, Message: "请输入有效的搜索关键词"}
	}
	var cursor conversationSearchCursor
	if token != "" {
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Index < 0 || cursor.Item < 0 {
			return result, &appserver.RPCError{Code: -32602, Message: "搜索分页位置无效"}
		}
	}
	var page []conversationSummary
	var nextList string
	for steps := 0; steps < 24; steps++ {
		if page == nil {
			var err error
			page, nextList, err = listConversations(ctx, app, cursor.Archived, cursor.List)
			if err != nil {
				return result, err
			}
			if cursor.Thread != "" {
				if index := slices.IndexFunc(page, func(t conversationSummary) bool { return t.ID == cursor.Thread }); index >= 0 {
					cursor.Index = index
				} else {
					cursor.Thread, cursor.Turn, cursor.Item = "", "", 0
				}
			}
		}
		if cursor.Index >= len(page) {
			if nextList == "" && cursor.Archived {
				return result, nil
			}
			cursor = conversationSearchCursor{Archived: cursor.Archived || nextList == "", List: nextList}
			page = nil
			continue
		}
		thread := page[cursor.Index]
		cursor.Thread = thread.ID
		params := map[string]any{"threadId": thread.ID, "limit": 1, "sortDirection": "desc", "itemsView": "full"}
		if cursor.Turn != "" {
			params["cursor"] = cursor.Turn
		}
		turns, err := readHistoryTurn(ctx, app, params)
		if err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			result.Skipped++
		}
		if len(turns.Data) != 0 {
			var turn struct {
				ID             string
				ItemsView      string
				HistoryOmitted bool `json:"codexrmHistoryOmitted"`
				Items          []struct {
					ID, Type, Text string
					Content        []struct{ Type, Text string }
				}
			}
			if err := json.Unmarshal(turns.Data[0], &turn); err != nil {
				return result, err
			}
			if turn.HistoryOmitted || turn.ItemsView == "notLoaded" {
				result.Skipped++
			}
			for i := cursor.Item; i < len(turn.Items); i++ {
				item := turn.Items[i]
				text := item.Text
				if item.Type == "userMessage" {
					parts := []string{}
					for _, content := range item.Content {
						if content.Type == "text" && !strings.HasPrefix(content.Text, "[CodexRM attachment] ") {
							parts = append(parts, content.Text)
						}
					}
					text = strings.Join(parts, "\n")
				} else if item.Type != "agentMessage" && item.Type != "planMessage" {
					continue
				}
				if snippet := conversationSnippet(text, query); snippet != "" {
					result.Hits = append(result.Hits, conversationSearchHit{thread, turn.ID, item.ID, snippet})
					if len(result.Hits) == 16 {
						cursor.Item = i + 1
						result.NextCursor = base64.RawURLEncoding.EncodeToString(mustJSON(cursor))
						return result, nil
					}
				}
			}
		}
		cursor.Item = 0
		if turns.NextCursor != nil && *turns.NextCursor != cursor.Turn {
			cursor.Turn = *turns.NextCursor
		} else {
			cursor.Index++
			cursor.Thread, cursor.Turn = "", ""
		}
	}
	result.NextCursor = base64.RawURLEncoding.EncodeToString(mustJSON(cursor))
	return result, nil
}

func conversationSnippet(text, query string) string {
	folded := strings.ToLower(text)
	index := strings.Index(folded, query)
	if index < 0 {
		return ""
	}
	// Lowercasing can change the UTF-8 byte length, but preserves rune positions.
	offset := len([]rune(folded[:index]))
	end := offset + len([]rune(query))
	characters := []rune(text)
	start := max(0, offset-45)
	stop := min(len(characters), end+100)
	excerpt := string(characters[start:stop])
	if start > 0 {
		excerpt = "…" + excerpt
	}
	if stop < len(characters) {
		excerpt += "…"
	}
	return excerpt
}

func (a *Agent) conversationRPC(ctx context.Context, app *appserver.Client, method string, params map[string]any) (any, error) {
	id, _ := params["threadId"].(string)
	switch method {
	case "codexrm/thread/search":
		query, _ := params["query"].(string)
		cursor, _ := params["cursor"].(string)
		return searchConversations(ctx, app, query, cursor)
	case "codexrm/thread/related":
		if id == "" || len(id) > 256 {
			return nil, &appserver.RPCError{Code: -32602, Message: "会话 ID 无效"}
		}
		data, err := a.conversationInventory(ctx, app, nil, id)
		return map[string]any{"data": data}, err
	case "codexrm/thread/pins", "codexrm/thread/pin":
		var pinned *bool
		if method == "codexrm/thread/pin" {
			value, ok := params["pinned"].(bool)
			if !ok || id == "" || len(id) > 256 {
				return nil, &appserver.RPCError{Code: -32602, Message: "置顶参数无效"}
			}
			pinned = &value
		}
		ids, err := a.conversationPins(id, pinned)
		if err != nil || pinned != nil || len(ids) == 0 {
			return map[string]any{"ids": ids, "data": []conversationSummary{}}, err
		}
		data, err := a.conversationInventory(ctx, app, ids, "")
		return map[string]any{"ids": ids, "data": data}, err
	default:
		return nil, &appserver.RPCError{Code: -32601, Message: "Unknown CodexRM conversation method"}
	}
}
