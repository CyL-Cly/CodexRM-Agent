package agent

import "testing"

func TestPrepareUserInputEnablesNormalConversations(t *testing.T) {
	for _, method := range []string{"thread/start", "thread/resume", "thread/fork"} {
		t.Run(method, func(t *testing.T) {
			params := map[string]any{"config": map[string]any{"model_reasoning_effort": "high"}}
			prepareUserInput(method, params)
			config := params["config"].(map[string]any)
			if config["features.default_mode_request_user_input"] != true || config["model_reasoning_effort"] != "high" {
				t.Fatalf("question support missing or existing config lost: %#v", config)
			}
		})
	}
	params := map[string]any{"config": map[string]any{"features.default_mode_request_user_input": false}}
	prepareUserInput("thread/start", params)
	if params["config"].(map[string]any)["features.default_mode_request_user_input"] != false {
		t.Fatal("explicit feature override was replaced")
	}
	params = map[string]any{}
	prepareUserInput("turn/start", params)
	if len(params) != 0 {
		t.Fatal("thread config was attached to an unrelated method")
	}
}

func TestQuestionLifetimeFollowsBlockingFlagAndTurnStatus(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		blocking     any
		keep         bool
	}{
		{"nonblocking completed", "completed", false, true},
		{"nonblocking interrupted", "interrupted", false, false},
		{"nonblocking failed", "failed", false, false},
		{"blocking completed", "completed", true, false},
		{"legacy completed", "completed", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{owners: map[string]string{"thread": "client"}, approvals: map[string]approval{}}
			params := map[string]any{"threadId": "thread", "turnId": "turn", "questions": []any{}}
			if tc.blocking != nil {
				params["isBlocking"] = tc.blocking
			}
			a.forward(mustJSON(map[string]any{"id": 17, "method": "item/tool/requestUserInput", "params": params}))
			a.forward(mustJSON(map[string]any{"method": "turn/completed", "params": map[string]any{
				"threadId": "other-thread", "turn": map[string]any{"id": "turn", "status": tc.status},
			}}))
			if len(a.approvals) != 1 {
				t.Fatal("another thread cleared the question")
			}
			a.forward(mustJSON(map[string]any{"method": "turn/completed", "params": map[string]any{
				"threadId": "thread", "turn": map[string]any{"id": "turn", "status": tc.status},
			}}))
			if (len(a.approvals) == 1) != tc.keep {
				t.Fatalf("pending questions after %s: %#v", tc.status, a.approvals)
			}
			a.forward(mustJSON(map[string]any{"method": "serverRequest/resolved", "params": map[string]any{"threadId": "thread", "requestId": 17}}))
			if len(a.approvals) != 0 {
				t.Fatal("resolved question would be replayed")
			}
		})
	}
}
