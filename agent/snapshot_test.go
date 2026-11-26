package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRolloutTurnStatesDistinguishExternalProgressFromExplicitStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	data := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"done"}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"done"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"stopped"}}
{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"stopped"}}
{"type":"event_msg","payload":{"type":"turn_started","turn_id":"running"}}
{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"stopped"}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"partial"
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	states, err := rolloutTurnStates(path)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"done": "completed", "stopped": "interrupted", "running": "inProgress"} {
		if states[id] != want {
			t.Errorf("%s: got %q, want %q", id, states[id], want)
		}
	}
	if _, ok := states["partial"]; ok {
		t.Fatal("partial append must not prove a terminal status")
	}
}

func TestRolloutTurnStatesBoundedTailAndLegacyAbort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	data := fmt.Sprintf("{\"padding\":\"%s\"}\n", strings.Repeat("x", 3<<20)) +
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"legacy"}}
{"type":"event_msg","payload":{"type":"turn_aborted"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"latest"}}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	states, err := rolloutTurnStates(path)
	if err != nil || states["legacy"] != "interrupted" || states["latest"] != "inProgress" {
		t.Fatalf("states=%v err=%v", states, err)
	}
}
