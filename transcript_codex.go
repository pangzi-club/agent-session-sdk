package agentsession

import (
	"encoding/json"
	"strings"
)

// parseCodexTranscript reads Codex rollout JSONL: response_item messages carry
// user/assistant text, reasoning summaries carry the model's thinking, function
// calls carry tool invocations, and function_call_output records pair their
// results back onto the calls by call id.
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
		case "reasoning":
			// A reasoning item keeps one or more summary paragraphs; the raw
			// content is redacted, the summaries are what a viewer can show.
			var text string
			for _, raw := range GetSlice(payload["summary"]) {
				block := GetMap(raw)
				if s := AsString(block["text"]); s != nil && *s != "" {
					if text != "" {
						text += "\n\n"
					}
					text += *s
				}
			}
			if text == "" {
				return
			}
			out = append(out, Entry{
				Role:      RoleAssistant,
				Thinking:  clipText(text, MaxThinkingChars),
				Timestamp: ts,
			})
		case "function_call", "custom_tool_call":
			name := firstString(payload, "name")
			if name == "" {
				return
			}
			input := payload["arguments"]
			if input == nil {
				input = payload["input"]
			}
			call := toolCall(name, argumentsMap(input))
			call.ID = firstString(payload, "call_id", "id")
			out = append(out, Entry{
				Role:      RoleAssistant,
				Tools:     []ToolCall{call},
				Timestamp: ts,
			})
		case "function_call_output", "custom_tool_call_output":
			applyCodexToolOutput(out, firstString(payload, "call_id", "id"), payload["output"])
		}
	})
	return out, err
}

// applyCodexToolOutput pairs one output record onto its call by call id. The
// output is a string (or an {output|content:[...]} object) whose header
// reports the process exit: "Process exited with code N" — nonzero marks a
// failed call.
func applyCodexToolOutput(out []Entry, id string, output any) {
	if id == "" {
		return
	}
	text := ""
	switch v := output.(type) {
	case string:
		text = v
	case map[string]any:
		if s := AsString(v["output"]); s != nil {
			text = *s
		} else {
			for _, raw := range GetSlice(v["content"]) {
				if s := AsString(raw); s != nil {
					text = *s
				} else if block := GetMap(raw); block != nil {
					if s := AsString(block["text"]); s != nil {
						text = *s
					}
				}
				if text != "" {
					break
				}
			}
		}
	}
	if text == "" {
		return
	}
	failed := codexExitFailed(text)
	for i := range out {
		for j := range out[i].Tools {
			tool := &out[i].Tools[j]
			if tool.ID != id || tool.Output != "" {
				continue
			}
			tool.Output = clipText(text, MaxToolOutputChars)
			tool.Failed = failed
			return
		}
	}
}

// codexExitFailed reads "Process exited with code N" out of an output header;
// nonzero means a failed call. The marker is Codex-specific: outputs without
// it are not judged.
func codexExitFailed(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "Process exited with code "); ok {
			return rest != "0"
		}
	}
	return false
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
