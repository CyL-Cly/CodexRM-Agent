package agent

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

const (
	// Bound each client-facing page by turn count and serialized size.
	historyPageTurns = 4
	historyPageBytes = 8 << 20
	// Leave room for the RPC ID and envelope, on both relay and direct paths.
	maxRPCPayloadBytes = protocol.MaxEnvelopeSize - (64 << 10)
)

type historyPage struct {
	Data            []json.RawMessage `json:"data"`
	NextCursor      *string           `json:"nextCursor"`
	BackwardsCursor *string           `json:"backwardsCursor"`
}

func threadHistoryPage(ctx context.Context, app *appserver.Client, params map[string]any) (historyPage, error) {
	limit := historyPageTurns
	if requested, ok := params["limit"].(float64); ok && requested >= 1 && requested < float64(limit) {
		limit = int(requested)
	}
	params = maps.Clone(params)
	params["limit"] = 1
	result := historyPage{Data: []json.RawMessage{}}
	bytes := 0
	for len(result.Data) < limit {
		page, err := readHistoryTurn(ctx, app, params)
		if err != nil {
			return historyPage{}, err
		}
		if len(page.Data) == 0 {
			result.NextCursor = page.NextCursor
			break
		}
		turn := page.Data[0]
		if len(result.Data) == 0 {
			result.BackwardsCursor = page.BackwardsCursor
		} else if bytes+len(turn) > historyPageBytes {
			// Keep the last included turn's cursor. The omitted turn is the
			// starting point of the next page and will be fetched on demand.
			break
		}
		result.Data = append(result.Data, turn)
		result.NextCursor = page.NextCursor
		bytes += len(turn)
		if page.NextCursor == nil || bytes >= historyPageBytes {
			break
		}
		if params["cursor"] == *page.NextCursor {
			return historyPage{}, errors.New("thread history cursor did not advance")
		}
		params["cursor"] = *page.NextCursor
	}
	return result, nil
}

func readHistoryTurn(ctx context.Context, app *appserver.Client, params map[string]any) (historyPage, error) {
	// A single upstream fetch per turn prevents multiple images or long tool
	// outputs from jointly exceeding app-server's receive limit in CodexRM.
	raw, err := app.Call(ctx, "thread/turns/list", params)
	omitted := errors.Is(err, appserver.ErrResponseTooLarge) || err == nil && len(raw) > maxRPCPayloadBytes
	if omitted {
		tooLarge := &appserver.RPCError{Code: -32000, Message: "单轮历史内容过大，无法传输。可在 Codex 桌面端查看。"}
		if params["itemsView"] == "notLoaded" {
			return historyPage{}, tooLarge
		}
		metadata := maps.Clone(params)
		metadata["itemsView"] = "notLoaded"
		raw, err = app.Call(ctx, "thread/turns/list", metadata)
		if err != nil || len(raw) > maxRPCPayloadBytes {
			return historyPage{}, tooLarge
		}
	} else if err != nil {
		return historyPage{}, err
	}
	var page historyPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return historyPage{}, err
	}
	if len(page.Data) > 1 {
		return historyPage{}, errors.New("app-server returned more than one history turn")
	}
	if omitted && len(page.Data) == 1 {
		var turn map[string]json.RawMessage
		if err := json.Unmarshal(page.Data[0], &turn); err != nil || turn == nil {
			return historyPage{}, errors.New("invalid thread history turn")
		}
		turn["codexrmHistoryOmitted"] = json.RawMessage("true")
		page.Data[0] = mustJSON(turn)
	}
	return page, nil
}

// Returns turns in transcript order. Older app-servers may only support
// thread/read, in which case the RPC size guard still protects the transport.
func initialThreadHistory(ctx context.Context, app *appserver.Client, id string, allowLegacyRead bool) ([]json.RawMessage, json.RawMessage, error) {
	page, err := threadHistoryPage(ctx, app, map[string]any{"threadId": id, "sortDirection": "desc", "itemsView": "full"})
	if err == nil {
		slices.Reverse(page.Data)
		return page.Data, mustJSON(page.NextCursor), nil
	}
	var rpc *appserver.RPCError
	if !allowLegacyRead || !errors.As(err, &rpc) || rpc.Code != -32601 {
		return nil, nil, err
	}
	raw, err := app.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": true})
	if errors.Is(err, appserver.ErrResponseTooLarge) || err == nil && len(raw) > maxRPCPayloadBytes {
		return nil, nil, &appserver.RPCError{Code: -32000, Message: "会话历史内容过大，当前 Codex 版本无法分页读取。可在 Codex 桌面端查看。"}
	}
	if err != nil {
		return nil, nil, err
	}
	var result struct {
		Thread struct{ Turns []json.RawMessage }
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return nil, nil, err
	}
	return result.Thread.Turns, nil, nil
}

func hydrateResumedThread(ctx context.Context, app *appserver.Client, raw json.RawMessage) (json.RawMessage, error) {
	var result map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	var thread map[string]json.RawMessage
	if err := json.Unmarshal(result["thread"], &thread); err != nil {
		return nil, err
	}
	if thread == nil {
		return nil, errors.New("thread/resume returned no thread")
	}
	if string(thread["historyMode"]) == `"paginated"` {
		// Existing clients already request a page for native paginated threads.
		return raw, nil
	}
	var id string
	if err := json.Unmarshal(thread["id"], &id); err != nil || id == "" {
		return nil, errors.New("thread/resume returned no thread ID")
	}
	turns, cursor, err := initialThreadHistory(ctx, app, id, true)
	if err != nil {
		return nil, err
	}
	thread["turns"] = mustJSON(turns)
	result["thread"] = mustJSON(thread)
	result["nextCursor"] = mustJSON(cursor)
	return mustJSON(result), nil
}
