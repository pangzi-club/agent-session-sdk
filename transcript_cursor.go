package agentsession

// parseCursorTranscript reads Cursor agent transcripts, which are JSONL
// records holding a role and a message content value.
func parseCursorTranscript(info SessionInfo) ([]Entry, error) {
	file, err := transcriptFile(info)
	if err != nil {
		return nil, err
	}
	var out []Entry
	err = scanJSONLines(file, func(obj map[string]any) {
		role := messageRole(obj)
		if role == "" {
			return
		}
		content := messageContent(obj)
		if content == nil {
			return
		}
		out = append(out, entriesFromContent(role, content, mapTimestamp(obj, false))...)
	})
	return out, err
}

// parsePiTranscript reads Pi session JSONL, where each message event wraps the
// actual message object.
func parsePiTranscript(info SessionInfo) ([]Entry, error) {
	file, err := transcriptFile(info)
	if err != nil {
		return nil, err
	}
	var out []Entry
	err = scanJSONLines(file, func(obj map[string]any) {
		if firstString(obj, "type") != "message" {
			return
		}
		message := GetMap(obj["message"])
		if message == nil {
			return
		}
		role := roleFromString(firstString(message, "role"))
		if role == "" {
			return
		}
		ts := mapTimestamp(message, false)
		if ts == 0 {
			ts = mapTimestamp(obj, false)
		}
		out = append(out, entriesFromContent(role, message["content"], ts)...)
	})
	return out, err
}
