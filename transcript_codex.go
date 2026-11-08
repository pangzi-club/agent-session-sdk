package agentsession

import "encoding/json"

// parseCodexTranscript reads Codex rollout JSONL: response_item messages carry
// user/assistant text, function calls carry tool invocations.
func parseCodexTranscript(info SessionInfo) ([]Entry, error) {
	file, err := transcriptFile(info)
	if err != nil {
		return nil, err
	}
	var out []Entry
	err = scanJSONLines(file, func(obj map[string]any) {
		if firstString(obj, "type") != "response_item" {
			return
		}
		payload := GetMap(obj["payload"])
		if payload == nil {
			return
		}
		ts := mapTimestamp(obj, false)
		switch firstString(payload, "type") {
		case "message":
			role := roleFromString(firstString(payload, "role"))
			if role == "" {
				return
			}
			out = append(out, entriesFromContent(role, payload["content"], ts)...)
		case "function_call", "custom_tool_call":
			name := firstString(payload, "name")
			if name == "" {
				return
			}
			input := payload["arguments"]
			if input == nil {
				input = payload["input"]
			}
			out = append(out, Entry{
				Role:      RoleAssistant,
				Tools:     []ToolCall{toolCall(name, argumentsMap(input))},
				Timestamp: ts,
			})
		}
	})
	return out, err
}

// argumentsMap decodes a tool argument object or its JSON-string form.
func argumentsMap(v any) map[string]any {
	if m := GetMap(v); m != nil {
		return m
	}
	if s := AsString(v); s != nil {
		m := map[string]any{}
		if json.Unmarshal([]byte(*s), &m) == nil {
			return m
		}
	}
	return nil
}
