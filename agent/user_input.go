package agent

// Enable questions for normal remote conversations without changing the user's
// global Codex config or switching the conversation to Plan mode.
func prepareUserInput(method string, params map[string]any) {
	if method != "thread/start" && method != "thread/resume" && method != "thread/fork" {
		return
	}
	config, _ := params["config"].(map[string]any)
	if config == nil {
		config = map[string]any{}
	}
	const feature = "features.default_mode_request_user_input"
	if _, supplied := config[feature]; !supplied {
		config[feature] = true
	}
	params["config"] = config
}
