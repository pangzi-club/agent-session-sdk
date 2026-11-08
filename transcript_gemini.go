package agentsession

import "strings"

// parseGeminiTranscript reads Gemini CLI's logs.json, which only records user
// messages (one file can contain several sessions).
func parseGeminiTranscript(info SessionInfo) ([]Entry, error) {
	file, err := transcriptFile(info)
	if err != nil {
		return nil, err
	}
	parsed, err := readJSONFileAny(file)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, raw := range GetSlice(parsed) {
		entry := GetMap(raw)
		if entry == nil {
			continue
		}
		if id := firstString(entry, "sessionId"); id != "" && id != info.ID {
			continue
		}
		message := firstString(entry, "message")
		if message == "" {
			continue
		}
		role := RoleUser
		switch strings.ToLower(firstString(entry, "type")) {
		case "gemini", "assistant", "model":
			role = RoleAssistant
		}
		out = append(out, entriesFromContent(role, message, mapTimestamp(entry, false))...)
	}
	return out, nil
}
