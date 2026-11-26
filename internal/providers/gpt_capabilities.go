package providers

import "strings"

// GPTOfficial reports whether the two GPT transport/compaction options are
// enabled on a custom provider. It is not a claim about the provider's identity.
func GPTOfficial(connection map[string]any) bool {
	if connection["supports_websockets"] != true {
		return false
	}
	capabilities, _ := connection["capabilities"].(map[string]any)
	if mode, configured := capabilities["remote_compaction"]; configured {
		return mode == "v2"
	}
	// Match Codex's default for OpenAI- and Azure-named/hosted providers when
	// the remote compaction capability is not explicitly configured.
	name, _ := connection["name"].(string)
	if name == "OpenAI" || strings.EqualFold(name, "azure") {
		return true
	}
	baseURL, _ := connection["base_url"].(string)
	baseURL = strings.ToLower(baseURL)
	for _, part := range []string{"openai.azure.", "cognitiveservices.azure.", "aoai.azure.", "azure-api.", "azurefd.", "windows.net/openai"} {
		if strings.Contains(baseURL, part) {
			return true
		}
	}
	return false
}

func RemoteCompactionValue(enabled bool) string {
	if enabled {
		return "v2"
	}
	return "unsupported"
}
