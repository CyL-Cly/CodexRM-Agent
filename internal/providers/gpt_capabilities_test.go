package providers

import "testing"

func TestGPTOfficialReadsBothCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, baseURL, compaction string
		websockets, enabled       bool
	}{
		{"Custom", "", "", true, false},
		{"Custom", "", "v2", true, true},
		{"Custom", "", "v2", false, false},
		{"OpenAI", "", "", true, true},
		{"OpenAI", "", "unsupported", true, false},
		{"Custom", "https://example.openai.azure.com", "", true, true},
	} {
		connection := map[string]any{"name": tc.name, "base_url": tc.baseURL, "supports_websockets": tc.websockets}
		if tc.compaction != "" {
			connection["capabilities"] = map[string]any{"remote_compaction": tc.compaction}
		}
		if got := GPTOfficial(connection); got != tc.enabled {
			t.Errorf("%+v: got %v", tc, got)
		}
	}
}
