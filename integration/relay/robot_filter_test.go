package server_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/CyL-Cly/CodexRM-Agent/agent"
	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

func TestRobotAutomationCompletesThroughRelay(t *testing.T) {
	_, h := newServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := agent.New(agent.Config{
		ServerURL: h.URL, DataDir: t.TempDir(), Cwd: t.TempDir(), Mock: true,
		EnrollmentKey: "test-key", NoAutoPair: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := a.Pair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var claimed protocol.PairClaimResponse
	if status := post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{
		Code: pair.ManualPairingCode, ClientName: "wx", DeviceType: protocol.DeviceTypeRobot,
	}, &claimed); status != 200 {
		t.Fatalf("claim returned %d", status)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Agent did not stop")
		}
	})
	client := ws(t, h, "client", claimed.Token)
	until(t, client, func(e protocol.Envelope) bool { return e.Status == "connected" })
	rpc(t, client, 1, "codexrm/wx/policy/set", map[string]any{"summaries": map[string]any{"123@chatroom": map[string]any{}}})
	response(t, client, 1)
	rpc(t, client, 2, "codexrm/wx/task/start", map[string]any{
		"kind": "summary", "groupId": "123@chatroom", "instructions": "Summarize",
		"input": []any{map[string]any{"type": "text", "text": "Fixture messages"}},
	})
	var result struct{ ThreadID, TurnID string }
	var eventThread, eventTurn string
	for result.TurnID == "" || eventTurn == "" {
		env := read(t, client)
		var message struct {
			ID     int
			Method string
			Result json.RawMessage
			Error  json.RawMessage
			Params struct {
				ThreadID string
				Turn     struct{ ID, Status string }
			}
		}
		if json.Unmarshal(env.Message, &message) != nil {
			continue
		}
		if message.ID == 2 {
			if len(message.Error) != 0 || json.Unmarshal(message.Result, &result) != nil {
				t.Fatalf("automation start failed: %s", env.Message)
			}
		}
		if message.Method == "turn/completed" && message.Params.Turn.Status == "completed" {
			eventThread, eventTurn = message.Params.ThreadID, message.Params.Turn.ID
		}
	}
	if result.ThreadID != eventThread || result.TurnID != eventTurn {
		t.Fatalf("automation response %#v did not match the notification", result)
	}
	rpc(t, client, 3, "codexrm/wx/task/close", map[string]string{"threadId": result.ThreadID})
	response(t, client, 3)
}

func TestRobotClientCannotCallDesktopOrReceiveBroadcasts(t *testing.T) {
	_, h := newServer(t)
	var enrolled protocol.EnrollResponse
	if status := post(t, h, "/api/v1/agent/enroll", "", "test-key", protocol.EnrollRequest{Name: "robot-test", InstallationID: "robot-install"}, &enrolled); status != 200 {
		t.Fatalf("enroll %d", status)
	}
	var pair protocol.PairStartResponse
	post(t, h, "/api/v1/agent/pair", enrolled.Token, "", map[string]any{}, &pair)
	var browser protocol.PairClaimResponse
	post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.ManualPairingCode, ClientName: "browser", DeviceType: "browser"}, &browser)
	var pairRobot protocol.PairStartResponse
	post(t, h, "/api/v1/agent/pair", enrolled.Token, "", map[string]any{}, &pairRobot)
	var robot protocol.PairClaimResponse
	post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pairRobot.ManualPairingCode, ClientName: "wx", DeviceType: protocol.DeviceTypeRobot}, &robot)

	agentConn := ws(t, h, "agent", enrolled.Token)
	browserConn := ws(t, h, "client", browser.Token)
	robotConn := ws(t, h, "client", robot.Token)
	_ = read(t, browserConn)
	_ = read(t, robotConn)
	rpc(t, robotConn, 1, "command/exec", map[string]any{})
	denied := until(t, robotConn, func(env protocol.Envelope) bool { return env.Type == protocol.TypeError })
	if denied.Error != "method not allowed" {
		t.Fatalf("robot command call: %#v", denied)
	}
	rpc(t, robotConn, 2, "turn/steer", map[string]any{"threadId": "t", "expectedTurnId": "active"})
	forwarded := until(t, agentConn, func(env protocol.Envelope) bool { return env.Type == protocol.TypeClientMessage })
	var request struct{ Method string }
	if err := json.Unmarshal(forwarded.Message, &request); err != nil || request.Method != "turn/steer" || forwarded.ClientKind != protocol.ClientKindRobot {
		t.Fatalf("robot steer was not forwarded: %#v, %v", forwarded, err)
	}
	write(t, agentConn, protocol.Envelope{Type: protocol.TypeServerMessage, Message: raw(map[string]any{"method": "thread/started", "params": map[string]any{"thread": map[string]any{"id": "t"}}})})
	gotBrowser := until(t, browserConn, func(env protocol.Envelope) bool {
		return env.Type == protocol.TypeServerMessage && len(env.Message) > 0
	})
	if !json.Valid(gotBrowser.Message) {
		t.Fatal("browser missed broadcast")
	}
	_ = robotConn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	var leaked protocol.Envelope
	if err := robotConn.ReadJSON(&leaked); err == nil && leaked.Type == protocol.TypeServerMessage {
		t.Fatalf("robot received broadcast: %#v", leaked)
	}
}

func TestRobotProfileCannotEscalate(t *testing.T) {
	_, h := newServer(t)
	var enrolled protocol.EnrollResponse
	post(t, h, "/api/v1/agent/enroll", "", "test-key", protocol.EnrollRequest{Name: "robot-test", InstallationID: "robot-profile"}, &enrolled)
	var pair protocol.PairStartResponse
	post(t, h, "/api/v1/agent/pair", enrolled.Token, "", map[string]any{}, &pair)
	var robot protocol.PairClaimResponse
	post(t, h, "/api/v1/client/claim", "", "", protocol.PairClaimRequest{Code: pair.ManualPairingCode, DeviceType: protocol.DeviceTypeRobot}, &robot)
	if status := post(t, h, "/api/v1/client/profile", robot.Token, "", protocol.ClientProfileRequest{ClientName: "wx", DeviceType: "browser"}, nil); status != 400 {
		t.Fatalf("escalation status %d", status)
	}
}
