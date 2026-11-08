package agentsession

import (
	"path/filepath"
	"strings"
)

// parseKimiTranscript reads Kimi's wire.jsonl turn events, falling back to the
// context.jsonl transcript when the session has no wire file.
func parseKimiTranscript(info SessionInfo) ([]Entry, error) {
	file, err := transcriptFile(info)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(file, "context.jsonl") {
		return parseKimiContext(file)
	}

	var out []Entry
	err = scanJSONLines(file, func(obj map[string]any) {
		message := GetMap(obj["message"])
		if message == nil {
			return
		}
		ts := mapTimestamp(obj, true)
		payload := GetMap(message["payload"])
		if payload == nil {
			return
		}
		switch firstString(message, "type") {
		case "TurnBegin":
			out = append(out, entriesFromContent(RoleUser, payload["user_input"], ts)...)
		default:
			role := roleFromString(firstString(payload, "role"))
			if role == "" {
				switch firstString(message, "type") {
				case "TextPart", "TextChunk", "AssistantText", "Text":
					role = RoleAssistant
				}
			}
			if role == "" {
				return
			}
			content := payload["content"]
			if content == nil {
				content = payload["text"]
			}
			out = append(out, entriesFromContent(role, content, ts)...)
		}
	})
	if err != nil || len(out) > 0 {
		return out, err
	}
	contextFile := filepath.Join(filepath.Dir(file), "context.jsonl")
	if entries, contextErr := parseKimiContext(contextFile); contextErr == nil {
		return entries, nil
	}
	return out, nil
}

func parseKimiContext(file string) ([]Entry, error) {
	var out []Entry
	err := scanJSONLines(file, func(obj map[string]any) {
		out = append(out, genericEntries(obj)...)
	})
	return out, err
}
