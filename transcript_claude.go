package agentsession

// parseClaudeTranscript handles Claude Code and Qoder/Qoder CN JSONL, which
// share the same user/assistant/tool_use record shape.
func parseClaudeTranscript(info SessionInfo) ([]Entry, error) {
	file, err := transcriptFile(info)
	if err != nil {
		return nil, err
	}
	var out []Entry
	err = scanJSONLines(file, func(obj map[string]any) {
		ts := mapTimestamp(obj, false)
		switch firstString(obj, "type") {
		case "user":
			if AsBoolTrue(obj["isMeta"]) {
				return
			}
			var content any
			if message := GetMap(obj["message"]); message != nil {
				content = message["content"]
			}
			out = append(out, entriesFromContent(RoleUser, content, ts)...)
		case "assistant":
			message := GetMap(obj["message"])
			if message == nil {
				return
			}
			out = append(out, entriesFromContent(RoleAssistant, message["content"], ts)...)
		case "system":
			if firstString(obj, "subtype") == "compact_boundary" || AsBoolTrue(obj["isCompactSummary"]) {
				out = append(out, Entry{Role: RoleSystem, Text: "上下文已压缩", Timestamp: ts})
			}
		}
	})
	return out, err
}
