package agentsession

import (
	"encoding/json"
	"strings"
)

// parseWorkbuddyTranscript reads WorkBuddy JSONL: message records carry the
// visible turns, reasoning records keep the model's raw thinking, and the
// function_call/function_call_result pair records a tool invocation and its
// bounded output.
func parseWorkbuddyTranscript(info SessionInfo) ([]Entry, error) {
	file, err := transcriptFile(info)
	if err != nil {
		return nil, err
	}
	var out []Entry
	err = scanJSONLines(file, func(obj map[string]any) {
		ts := mapTimestamp(obj, false)
		switch firstString(obj, "type") {
		case "message":
			role := roleFromString(firstString(obj, "role"))
			if role == "" {
				return
			}
			out = append(out, entriesFromContent(role, obj["content"], ts)...)
		case "reasoning":
			if text := workbuddyReasoning(obj); text != "" {
				out = append(out, Entry{
					Role:      RoleAssistant,
					Thinking:  clipText(text, MaxThinkingChars),
					Timestamp: ts,
				})
			}
		case "function_call":
			name := firstString(obj, "name")
			if name == "" {
				return
			}
			input := obj["arguments"]
			if input == nil {
				input = obj["input"]
			}
			arguments := workbuddyArgumentsMap(input)
			call := toolCall(name, arguments)
			call.ID = firstString(obj, "callId", "call_id", "id")
			if arguments != nil {
				call.Input = clipText(compactJSON(arguments), MaxToolInputChars)
			}
			out = append(out, Entry{
				Role:      RoleAssistant,
				Tools:     []ToolCall{call},
				Timestamp: ts,
			})
		case "function_call_result":
			applyWorkbuddyToolOutput(out, firstString(obj, "callId", "call_id", "id"), obj)
		}
	})
	return out, err
}

// workbuddyReasoning reads raw reasoning blocks. WorkBuddy keeps the unshown
// text in rawContent, while some records also carry it in content.
func workbuddyReasoning(obj map[string]any) string {
	for _, key := range []string{"rawContent", "content"} {
		var parts []string
		for _, raw := range GetSlice(obj[key]) {
			block := GetMap(raw)
			if block == nil {
				continue
			}
			if text := firstString(block, "text", "reasoning", "thinking"); text != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n\n")
		}
	}
	return ""
}

// applyWorkbuddyToolOutput pairs a later result record back onto its call by
// call id, filling the output and failure marker in place.
func applyWorkbuddyToolOutput(out []Entry, id string, result map[string]any) {
	if id == "" {
		return
	}
	text := workbuddyOutputText(result["output"])
	failed := firstString(result, "status") == "error" || AsBoolTrue(result["failed"])
	if block := GetMap(result["output"]); block != nil {
		failed = failed || AsBoolTrue(block["is_error"]) || AsBoolTrue(block["failed"])
	}
	if text == "" && !failed {
		return
	}
	for i := len(out) - 1; i >= 0; i-- {
		for j := range out[i].Tools {
			tool := &out[i].Tools[j]
			if tool.ID == id && tool.Output == "" {
				tool.Output = clipText(text, MaxToolOutputChars)
				tool.Failed = failed
				return
			}
		}
	}
}

// workbuddyOutputText flattens a result payload: a string, a text block, or a
// wrapped object containing one of those shapes.
func workbuddyOutputText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		for _, key := range []string{"text", "output", "content"} {
			if text := AsString(typed[key]); text != nil && *text != "" {
				return *text
			}
		}
		for _, raw := range GetSlice(typed["content"]) {
			if text := workbuddyOutputText(raw); text != "" {
				return text
			}
		}
		if inner := GetMap(typed["result"]); inner != nil {
			return workbuddyOutputText(inner)
		}
	}
	return ""
}

// argumentsMap decodes a tool argument object or its JSON-string form.
func workbuddyArgumentsMap(value any) map[string]any {
	if m := GetMap(value); m != nil {
		return m
	}
	if raw := AsString(value); raw != nil {
		m := map[string]any{}
		if json.Unmarshal([]byte(*raw), &m) == nil {
			return m
		}
	}
	return nil
}
