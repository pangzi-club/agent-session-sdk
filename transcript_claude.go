package agentsession

import "strings"

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
			applyToolResults(out, content)
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

// applyToolResults matches tool_result blocks in a user record back onto the
// tool calls they answer, filling Output and Failed. Results without a known
// call ID are dropped. Tool calls live in already-emitted entries, so the match
// walks out; IDs are unique per call.
func applyToolResults(out []Entry, content any) {
	blocks := GetSlice(content)
	if blocks == nil {
		return
	}
	type result struct {
		id, output string
		failed     bool
	}
	var results []result
	for _, raw := range blocks {
		block := GetMap(raw)
		if block == nil || !IsStr(block["type"], "tool_result") {
			continue
		}
		id := firstString(block, "tool_use_id", "toolUseId", "id")
		if id == "" {
			continue
		}
		results = append(results, result{
			id:     id,
			output: toolResultText(block),
			failed: AsBoolTrue(block["is_error"]),
		})
	}
	if len(results) == 0 {
		return
	}
	for i := range out {
		tools := out[i].Tools
		for j := range tools {
			if tools[j].ID == "" {
				continue
			}
			for _, res := range results {
				if tools[j].ID == res.id && tools[j].Output == "" {
					tools[j].Output = clipText(res.output, MaxToolOutputChars)
					tools[j].Failed = res.failed
				}
			}
		}
	}
}

// toolResultText flattens a tool_result content value: a plain string, or a
// block array whose text blocks are joined with newlines.
func toolResultText(block map[string]any) string {
	switch content := block["content"].(type) {
	case string:
		return content
	case []any:
		var parts []string
		for _, raw := range content {
			if text := firstString(GetMap(raw), "text"); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
