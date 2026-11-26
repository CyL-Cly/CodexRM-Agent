package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"

	"github.com/CyL-Cly/CodexRM-Agent/internal/appserver"
)

// A separate app-server normalizes unloaded in-progress turns to interrupted.
// Keep its snapshot intact and attach durable lifecycle evidence for the UI.
func (a *Agent) threadSnapshot(ctx context.Context, app *appserver.Client, params map[string]any) (json.RawMessage, error) {
	id, _ := params["threadId"].(string)
	if id == "" {
		return nil, &appserver.RPCError{Code: -32602, Message: "threadId is required"}
	}
	read := func() (map[string]any, error) {
		raw, err := app.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false})
		if err != nil {
			return nil, err
		}
		var result struct {
			Thread map[string]any `json:"thread"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, err
		}
		if result.Thread == nil {
			return nil, errors.New("thread/read returned no thread")
		}
		return result.Thread, nil
	}
	var thread map[string]any
	var turns []json.RawMessage
	var cursor json.RawMessage
	for range 3 {
		before, err := read()
		if err != nil {
			return nil, err
		}
		turns, cursor, err = initialThreadHistory(ctx, app, id, before["historyMode"] != "paginated")
		if err != nil {
			return nil, err
		}
		after, err := read()
		if err != nil {
			return nil, err
		}
		if reflect.DeepEqual(before["status"], after["status"]) &&
			reflect.DeepEqual(before["updatedAt"], after["updatedAt"]) &&
			reflect.DeepEqual(before["historyMode"], after["historyMode"]) {
			thread = after
			break
		}
	}
	if thread == nil {
		return nil, errors.New("thread changed while reading its history; retry the snapshot")
	}
	thread["turns"] = turns
	route, err := a.providerRoute(id)
	if err != nil {
		return nil, err
	}
	if route.ProviderID != "" {
		thread["codexrmProvider"] = route.ProviderID
		if route.Model != "" {
			thread["model"] = route.Model
		}
	}
	// Paths from a remote app-server belong to that machine, never the agent.
	if a.cfg.AppServerURL == "" {
		path, _ := thread["path"].(string)
		if states, err := rolloutTurnStates(path); err == nil {
			thread["codexrmTurnStates"] = states
		}
	}
	status, _ := thread["status"].(map[string]any)
	statusType, _ := status["type"].(string)
	return mustJSON(map[string]any{"thread": thread, "nextCursor": cursor, "active": statusType == "active"}), nil
}

// Read a bounded tail. Missing evidence stays unknown; it never proves a stop.
func rolloutTurnStates(path string) (map[string]string, error) {
	if !strings.HasSuffix(path, ".jsonl") {
		return nil, errors.New("no plain rollout available")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const tailBytes int64 = 2 << 20
	offset := max(int64(0), info.Size()-tailBytes)
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(io.LimitReader(f, info.Size()-offset))
	scanner.Buffer(make([]byte, 64*1024), int(tailBytes)+1)
	if offset > 0 {
		scanner.Scan() // The first line may begin in the middle of a JSON record.
	}
	states := map[string]string{}
	current := ""
	for scanner.Scan() {
		var record struct {
			Type    string `json:"type"`
			Payload struct {
				Type   string `json:"type"`
				TurnID string `json:"turn_id"`
			} `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) != nil || record.Type != "event_msg" {
			continue
		}
		p := record.Payload
		id := p.TurnID
		if id == "" {
			id = current
		}
		if id == "" {
			continue
		}
		switch p.Type {
		case "task_started", "turn_started":
			current, states[id] = id, "inProgress"
		case "task_complete", "turn_complete":
			states[id] = "completed"
		case "turn_aborted":
			states[id] = "interrupted"
		}
	}
	return states, scanner.Err()
}
