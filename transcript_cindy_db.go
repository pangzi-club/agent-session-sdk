package agentsession

// transcript_cindy_db.go parses the messages table mirror inside Cindy's
// profile database. It backs sessions whose engine transcript never reached
// this machine: remote-agent sessions run on another device, and legacy rows
// whose stored transcript path belongs to a different home. The scanner marks
// those sessions with the "cindy-db" format and a "<db>#<session-id>" file
// path, the same database-reference convention the OpenCode/Zcode loaders use.

import (
	"fmt"
	"strings"
)

// parseCindyDBTranscript reads one session's mirrored messages from the
// profile database referenced by FilePath ("<db>#<session-id>").
func parseCindyDBTranscript(info SessionInfo) ([]Entry, error) {
	dbPath, sessionID, ok := splitDBRef(info)
	if !ok {
		return nil, fmt.Errorf("session %q has no database reference", info.ID)
	}
	db, err := OpenSQLite(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := QueryMaps(db, `SELECT role, content, tool_use_id, created_at
		FROM messages
		WHERE session_id = ? AND rewind_at IS NULL
		ORDER BY created_at, rowid`, sessionID)
	if err != nil {
		return nil, err
	}

	// Tool results arrive as separate rows and are matched back onto their
	// tool call by tool_use_id, mirroring applyToolResults for Claude JSONL.
	type toolResult struct {
		id, output string
		failed     bool
	}
	results := map[string]toolResult{}
	for _, row := range rows {
		if !IsStr(row["role"], "tool_result") {
			continue
		}
		id := firstString(row, "tool_use_id")
		if id == "" {
			continue
		}
		output, _ := row["content"].(string)
		results[id] = toolResult{
			id:     id,
			output: output,
			failed: strings.Contains(output, "<tool_use_error>"),
		}
	}

	var out []Entry
	for _, row := range rows {
		content, _ := row["content"].(string)
		var timestamp float64
		if ts := AsEpochMs(row["created_at"]); ts != nil {
			timestamp = *ts
		}
		switch role := firstString(row, "role"); role {
		case "user":
			// User rows store either plain text or a JSON envelope with
			// text/images/files fields.
			if entry, ok := cindyDBEntry(RoleUser, content, timestamp); ok {
				out = append(out, entry)
			}
		case "assistant":
			// Assistant rows store plain text; JSON-wrapped payloads fall
			// through entriesFromContent's map branch.
			if entry, ok := cindyDBEntry(RoleAssistant, content, timestamp); ok {
				out = append(out, entry)
			}
		case "thinking":
			obj := parseJSONMap(content)
			if obj == nil {
				continue
			}
			if text := firstString(obj, "text", "thinking"); text != "" {
				out = append(out, Entry{
					Role:       RoleAssistant,
					Thinking:   clipText(text, MaxThinkingChars),
					Timestamp:  timestamp,
					DurationMs: firstNumber(obj, "durationMs"),
				})
			}
		case "tool_use":
			obj := parseJSONMap(content)
			if obj == nil {
				continue
			}
			call, ok := toolFromBlock(map[string]any{
				"name":  obj["toolName"],
				"id":    obj["toolUseId"],
				"input": obj["input"],
			})
			if !ok {
				continue
			}
			if res, matched := results[call.ID]; matched {
				call.Output = clipText(res.output, MaxToolOutputChars)
				call.Failed = res.failed
			}
			out = append(out, Entry{Role: RoleAssistant, Tools: []ToolCall{call}, Timestamp: timestamp})
		case "error":
			// Engine errors (model refused, unsupported input) surface as
			// system notices so the transcript does not end mid-turn.
			obj := parseJSONMap(content)
			message := content
			if obj != nil {
				message = firstString(obj, "message")
			}
			if message = cleanAssistantText(message); message != "" {
				out = append(out, Entry{Role: RoleSystem, Text: message, Timestamp: timestamp})
			}
		case "system", "agent_switch", "ask_user", "progress", "tool_result":
			// Internal bookkeeping: agent switches, clarification cards and
			// raw tool results are not chat turns.
		}
	}
	return out, nil
}

// cindyDBEntry renders one message row's content through the shared content
// mapper, after unwrapping a JSON envelope if the payload is one.
func cindyDBEntry(role Role, content string, ts float64) (Entry, bool) {
	if content == "" {
		return Entry{}, false
	}
	entries := entriesFromContent(role, cindyDBUnwrapContent(role, content), ts)
	if len(entries) == 0 {
		return Entry{}, false
	}
	return entries[0], true
}

// cindyDBUnwrapContent accepts plain text as-is and parses JSON envelopes
// into the shared block shapes. A user row that stores {"text": ...} becomes
// its text; unknown envelopes are dropped rather than shown as raw JSON.
func cindyDBUnwrapContent(role Role, content string) any {
	if obj := parseJSONMap(content); obj != nil {
		if role == RoleUser {
			// Images and files are attachments, not text; only a text field
			// carries visible words.
			if text := firstString(obj, "text"); text != "" {
				return text
			}
			return nil
		}
		return obj
	}
	return content
}

// firstNumber reads a numeric field, defaulting to 0.
func firstNumber(m map[string]any, key string) float64 {
	if f, ok := AsNumber(m[key]); ok {
		return f
	}
	return 0
}
