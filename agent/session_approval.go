package agent

import (
	"encoding/json"
	"slices"
)

// Inspect the original request and the reply to determine whether releasing
// the thread would discard a session approval. Codex still enforces the grant.
func retainsSessionApproval(request, result json.RawMessage) bool {
	var req struct {
		Method string `json:"method"`
		Params struct {
			AvailableDecisions []json.RawMessage `json:"availableDecisions"`
		} `json:"params"`
	}
	var reply struct {
		Decision    string                     `json:"decision"`
		Scope       string                     `json:"scope"`
		Permissions map[string]json.RawMessage `json:"permissions"`
	}
	if json.Unmarshal(request, &req) != nil || json.Unmarshal(result, &reply) != nil {
		return false
	}
	switch req.Method {
	case "item/permissions/requestApproval":
		return reply.Scope == "session" && len(reply.Permissions) > 0
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		return reply.Decision == "acceptForSession" && (req.Params.AvailableDecisions == nil ||
			slices.ContainsFunc(req.Params.AvailableDecisions, func(raw json.RawMessage) bool {
				return string(raw) == `"acceptForSession"`
			}))
	default:
		return false
	}
}
