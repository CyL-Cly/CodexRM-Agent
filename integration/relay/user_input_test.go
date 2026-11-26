package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
	"github.com/gorilla/websocket"
)

func questionAgent(t *testing.T, cfg agent.Config) (*httptest.Server, *websocket.Conn, string) {
	t.Helper()
	_, h := newServer(t)
	cfg.ServerURL, cfg.DataDir, cfg.EnrollmentKey = h.URL, t.TempDir(), "test-key"
	if cfg.Cwd == "" {
		cfg.Cwd = t.TempDir()
	}
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	pair, err := a.Pair(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var claimed protocol.PairClaimResponse
	post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.ManualPairingCode}, &claimed)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("question agent did not stop")
		}
	})
	c := ws(t, h, "client", claimed.Token)
	until(t, c, func(e protocol.Envelope) bool { return e.Status == "connected" })
	return h, c, claimed.Token
}

func questionEvent(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	e := until(t, c, func(e protocol.Envelope) bool {
		var m map[string]any
		_ = json.Unmarshal(e.Message, &m)
		if m["method"] == "turn/completed" {
			t.Fatalf("turn completed without delivering a question: %s", e.Message)
		}
		return m["method"] == "item/tool/requestUserInput"
	})
	var m map[string]any
	_ = json.Unmarshal(e.Message, &m)
	return m
}

func TestQuestionAnswersAndSkipSurviveBrowserReconnect(t *testing.T) {
	for _, skip := range []bool{false, true} {
		t.Run(fmt.Sprintf("skip=%t", skip), func(t *testing.T) {
			h, c, token := questionAgent(t, agent.Config{Mock: true})
			rpc(t, c, 1, "thread/start", map[string]any{})
			tid := response(t, c, 1)["thread"].(map[string]any)["id"]
			rpc(t, c, 2, "turn/start", map[string]any{"threadId": tid, "input": []any{map[string]any{"type": "text", "text": "演示提问"}}})
			question := questionEvent(t, c)
			params := question["params"].(map[string]any)
			if len(params["questions"].([]any)) != 2 || params["threadId"] != tid {
				t.Fatalf("question payload lost: %#v", params)
			}
			_ = c.Close()
			c = ws(t, h, "client", token)
			rpc(t, c, 3, "codexrm/info", map[string]any{})
			replayed := questionEvent(t, c)
			if replayed["id"] != question["id"] {
				t.Fatal("reconnect changed the pending question ID")
			}
			answers := map[string]any{}
			if !skip {
				answers["approach"] = map[string]any{"answers": []string{"直接实现（推荐）"}}
				answers["details"] = map[string]any{"answers": []string{"保留中文\n第二行自定义要求"}}
			}
			result := map[string]any{"answers": answers}
			write(t, c, protocol.Envelope{Type: protocol.TypeClientMessage, Message: raw(map[string]any{"id": question["id"], "result": result})})
			acknowledged := false
			e := until(t, c, func(e protocol.Envelope) bool {
				var m map[string]any
				_ = json.Unmarshal(e.Message, &m)
				if m["method"] == "codexrm/request/resolved" {
					acknowledged = m["params"].(map[string]any)["id"] == question["id"]
				}
				return m["method"] == "turn/completed"
			})
			var completed struct {
				Params struct {
					Turn struct {
						Status string                  `json:"status"`
						Items  []struct{ Text string } `json:"items"`
					} `json:"turn"`
				} `json:"params"`
			}
			_ = json.Unmarshal(e.Message, &completed)
			items := completed.Params.Turn.Items
			if !acknowledged || completed.Params.Turn.Status != "completed" || len(items) < 2 || !strings.Contains(items[len(items)-1].Text, string(raw(result))) {
				t.Fatalf("question result did not round trip (ack=%t): %s", acknowledged, e.Message)
			}
		})
	}
}

// Exercise the installed CLI against a local Responses fixture, including the
// actual Default-mode tool gate. No account, provider key or real model is used.
func TestQuestionRoundTripWithLocalCodex(t *testing.T) {
	t.Run("default request", func(t *testing.T) { testQuestionRoundTripWithLocalCodex(t, false) })
	t.Run("async message", func(t *testing.T) { testQuestionRoundTripWithLocalCodex(t, true) })
}

func testQuestionRoundTripWithLocalCodex(t *testing.T, async bool) {
	executable := os.Getenv("CODEXRM_TEST_CODEX")
	if executable == "" {
		t.Skip("set CODEXRM_TEST_CODEX to test the local Codex app-server")
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("RUST_LOG", "error")
	const answerText = "真实 CLI 的测试回答"
	const asyncAnswer = "<send_user_message_question_reply>\n" +
		`[{"questionItemId":"[\"request_user_input_async\",\"question-call\",0]","question":"选择答案？","answer":"真实 CLI 的测试回答"}]` +
		"\n</send_user_message_question_reply>"
	answers := make(chan string, 4)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Input []struct {
				Type    string          `json:"type"`
				CallID  string          `json:"call_id"`
				Output  string          `json:"output"`
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid test body", http.StatusBadRequest)
			return
		}
		for _, input := range body.Input {
			if input.Type == "function_call_output" && input.CallID == "question-call" {
				select {
				case answers <- input.Output:
				default:
				}
			}
			if async && input.Role == "user" && strings.Contains(string(input.Content), answerText) {
				select {
				case answers <- string(input.Content):
				default:
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(value map[string]any) {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", value["type"], raw(value))
		}
		emit(map[string]any{"type": "response.created", "response": map[string]any{"id": "question-response"}})
		if calls.Add(1) == 1 {
			arguments := map[string]any{"questions": []any{map[string]any{
				"id": "confirm", "header": "确认", "question": "选择答案？", "options": []any{
					map[string]any{"label": "继续", "description": "继续任务。"},
					map[string]any{"label": "调整", "description": "调整任务。"},
				},
			}}}
			name := "request_user_input"
			if async {
				name = "request_user_input_async"
				arguments["questions"] = []any{map[string]any{"title": "选择答案？", "options": []string{"继续", "调整"}}}
			}
			emit(map[string]any{"type": "response.output_item.done", "item": map[string]any{
				"type": "function_call", "call_id": "question-call", "name": name, "arguments": string(raw(arguments)),
			}})
		}
		emit(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "question-response", "usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		}})
	}))
	defer upstream.Close()
	model := "gpt-5.4"
	if async {
		model = "gpt-6.2-sol"
	}
	config := fmt.Sprintf("model = %q\nmodel_provider = 'question_test'\n[model_providers.question_test]\nname = 'Local question test'\nbase_url = %q\nwire_api = 'responses'\nrequires_openai_auth = false\n", model, upstream.URL)
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	_, c, _ := questionAgent(t, agent.Config{CodexPath: executable})
	rpc(t, c, 1, "thread/start", map[string]any{"approvalPolicy": "never", "sandbox": "read-only"})
	tid := response(t, c, 1)["thread"].(map[string]any)["id"]
	rpc(t, c, 2, "turn/start", map[string]any{"threadId": tid, "effort": "low", "input": []any{map[string]any{"type": "text", "text": "Ask the fixture question."}}})
	if async {
		asyncItem := func(item map[string]any) bool {
			return item["id"] == "question-call" && item["type"] == "agentMessage" && item["delivery"] == "async" &&
				strings.Contains(string(raw(item["questions"])), `"options":["继续","调整"]`)
		}
		found := false
		until(t, c, func(e protocol.Envelope) bool {
			var msg struct {
				Method string `json:"method"`
				Params struct {
					Item map[string]any `json:"item"`
				} `json:"params"`
			}
			_ = json.Unmarshal(e.Message, &msg)
			if msg.Method == "item/completed" && asyncItem(msg.Params.Item) {
				found = true
			}
			return msg.Method == "turn/completed"
		})
		if !found {
			t.Fatal("CLI did not send an async question with selectable options")
		}
		rpc(t, c, 3, "thread/turns/list", map[string]any{"threadId": tid, "itemsView": "full"})
		history := response(t, c, 3)
		if !strings.Contains(string(raw(history)), `"options":["继续","调整"]`) {
			t.Fatalf("async question missing from history: %s", raw(history))
		}
		rpc(t, c, 4, "turn/start", map[string]any{"threadId": tid, "input": []any{map[string]any{"type": "text", "text": asyncAnswer}}})
		response(t, c, 4)
		timeout := time.After(5 * time.Second)
		for {
			select {
			case input := <-answers:
				if strings.Contains(input, answerText) && strings.Contains(input, "questionItemId") {
					return
				}
			case <-timeout:
				t.Fatal("CLI did not forward the async answer")
			}
		}
	}
	question := questionEvent(t, c)
	if question["params"].(map[string]any)["isBlocking"] != false {
		t.Fatalf("expected the actual Default-mode question: %#v", question)
	}
	write(t, c, protocol.Envelope{Type: protocol.TypeClientMessage, Message: raw(map[string]any{
		"id": question["id"], "result": map[string]any{"answers": map[string]any{"confirm": map[string]any{"answers": []string{answerText}}}},
	})})
	select {
	case output := <-answers:
		var got struct {
			Answers map[string]struct{ Answers []string } `json:"answers"`
		}
		if json.Unmarshal([]byte(output), &got) != nil || len(got.Answers["confirm"].Answers) != 1 || got.Answers["confirm"].Answers[0] != answerText {
			t.Fatalf("CLI did not send the answer to its model: %s", output)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("local Codex did not forward the question answer")
	}
	e := until(t, c, func(e protocol.Envelope) bool {
		var message map[string]any
		_ = json.Unmarshal(e.Message, &message)
		return message["method"] == "turn/completed"
	})
	var completed struct {
		Params struct {
			Turn struct{ Status string } `json:"turn"`
		} `json:"params"`
	}
	_ = json.Unmarshal(e.Message, &completed)
	if completed.Params.Turn.Status != "completed" {
		t.Fatalf("local Codex failed to continue after the answer: %s", e.Message)
	}
}
