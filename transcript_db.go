package agentsession

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var sqlIdentifierRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// splitDBRef splits a "db.sqlite#session-id" file path.
func splitDBRef(info SessionInfo) (string, string, bool) {
	if info.FilePath == nil {
		return "", "", false
	}
	parts := strings.SplitN(*info.FilePath, "#", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func parseJSONMap(raw any) map[string]any {
	s := AsString(raw)
	if s == nil || *s == "" {
		return nil
	}
	m := map[string]any{}
	if json.Unmarshal([]byte(*s), &m) != nil {
		return nil
	}
	return m
}

// parseSessionDBTranscript reads OpenCode and ZCode stores, which keep message
// metadata and message parts in SQLite tables.
func parseSessionDBTranscript(info SessionInfo) ([]Entry, error) {
	dbPath, sessionID, ok := splitDBRef(info)
	if !ok {
		return nil, fmt.Errorf("session %q has no database reference", info.ID)
	}
	db, err := OpenSQLite(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := QueryMaps(db, `
		SELECT m.data AS message_data, m.time_created AS message_time, p.data AS part_data
		FROM message m
		JOIN part p ON p.message_id = m.id
		WHERE m.session_id = ?
		ORDER BY m.time_created, m.id, p.time_created, p.id`, sessionID)
	if err != nil {
		return nil, err
	}

	var out []Entry
	for _, row := range rows {
		message := parseJSONMap(row["message_data"])
		part := parseJSONMap(row["part_data"])
		if message == nil || part == nil {
			continue
		}
		role := roleFromString(firstString(message, "role"))
		msgTime := GetMap(message["time"])
		ts := AsEpochMs(row["message_time"])
		if msgTime != nil {
			if created := AsEpochMs(msgTime["created"]); created != nil {
				ts = created
			}
		}
		var timestamp float64
		if ts != nil {
			timestamp = *ts
		}
		// A text record's generation span; reasoning and tool parts carry
		// their own, more precise, part-level timing below.
		var textDuration float64
		if msgTime != nil {
			if completed := AsEpochMs(msgTime["completed"]); completed != nil && *completed > timestamp {
				textDuration = *completed - timestamp
			}
		}

		switch firstString(part, "type") {
		case "text", "":
			text := firstString(part, "text")
			if text == "" || AsBoolTrue(part["synthetic"]) {
				continue
			}
			if metadata := GetMap(part["metadata"]); metadata != nil && firstString(metadata, "visibility") == "model-only" {
				continue
			}
			if role == "" {
				role = RoleAssistant
			}
			entries := entriesFromContent(role, text, timestamp)
			for i := range entries {
				entries[i].DurationMs = textDuration
			}
			out = append(out, entries...)
		case "tool":
			name := firstString(part, "tool")
			if name == "" {
				continue
			}
			state := GetMap(part["state"])
			input := GetMap(state["input"])
			call := toolCall(name, input)
			if id := firstString(part, "callID"); id != "" {
				call.ID = id
			}
			if input != nil {
				call.Input = clipText(compactJSON(input), MaxToolInputChars)
			}
			if raw := state["output"]; raw != nil {
				if s := AsString(raw); s != nil {
					call.Output = clipText(*s, MaxToolOutputChars)
				} else {
					call.Output = clipText(compactJSON(raw), MaxToolOutputChars)
				}
			}
			status := firstString(state, "status")
			if status != "" && status != "completed" {
				call.Meta += " | " + status
			}
			call.Failed = status == "error"
			out = append(out, Entry{Role: RoleAssistant, Tools: []ToolCall{call}, Timestamp: timestamp, DurationMs: partDurationMs(state["time"])})
		case "reasoning":
			text := firstString(part, "text")
			if text == "" {
				continue
			}
			out = append(out, Entry{Role: RoleAssistant, Thinking: clipText(text, MaxThinkingChars), Timestamp: timestamp, DurationMs: partDurationMs(part["time"])})
		case "step-start", "step-finish", "file", "timeline":
			// Internal steps and attachments are not part of the chat view.
		}
	}
	return out, nil
}

// partDurationMs is a part's active span from its {start, end} epoch-ms time
// object; 0 when either bound is missing or out of order.
func partDurationMs(timeVal any) float64 {
	t := GetMap(timeVal)
	if t == nil {
		return 0
	}
	start := AsEpochMs(t["start"])
	end := AsEpochMs(t["end"])
	if start == nil || end == nil || *end <= *start {
		return 0
	}
	return *end - *start
}

// parseHermesTranscript looks for a messages table in Hermes' state.db. The
// schema varies between installs, so the columns are discovered at runtime.
func parseHermesTranscript(info SessionInfo) ([]Entry, error) {
	dbPath, sessionID, ok := splitDBRef(info)
	if !ok {
		return nil, fmt.Errorf("session %q has no database reference", info.ID)
	}
	db, err := OpenSQLite(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	tables, err := QueryMaps(db, `SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return nil, err
	}
	candidates := []string{"messages", "message", "conversation_messages", "chat_messages", "events"}
	tableNames := map[string]bool{}
	for _, row := range tables {
		if name := firstString(row, "name"); name != "" {
			tableNames[name] = true
		}
	}
	for _, table := range candidates {
		if !tableNames[table] || !sqlIdentifierRe.MatchString(table) {
			continue
		}
		entries, err := parseHermesTable(db, table, sessionID)
		if err != nil {
			continue
		}
		if len(entries) > 0 {
			return entries, nil
		}
	}
	return nil, ErrUnsupported
}

func parseHermesTable(db *sql.DB, table, sessionID string) ([]Entry, error) {
	columns, err := QueryMaps(db, `PRAGMA table_info("`+table+`")`)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, row := range columns {
		if name := firstString(row, "name"); name != "" {
			names[name] = true
		}
	}
	pick := func(options ...string) string {
		for _, option := range options {
			if names[option] {
				return option
			}
		}
		return ""
	}
	sessionCol := pick("session_id", "sessionId", "conversation_id", "thread_id")
	roleCol := pick("role", "sender", "author")
	contentCol := pick("content", "text", "message", "body", "data")
	if sessionCol == "" || roleCol == "" || contentCol == "" {
		return nil, ErrUnsupported
	}
	tsCol := pick("timestamp", "created_at", "createdAt", "time")

	rows, err := QueryMaps(db, `SELECT * FROM "`+table+`" WHERE "`+sessionCol+`" = ? ORDER BY rowid`, sessionID)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, row := range rows {
		role := roleFromString(firstString(row, roleCol))
		if role == "" {
			continue
		}
		ts := 0.0
		if tsCol != "" {
			ts = epochSecondsOrMs(row[tsCol])
		}
		content := row[contentCol]
		if raw := AsString(content); raw != nil && (strings.HasPrefix(*raw, "{") || strings.HasPrefix(*raw, "[")) {
			var parsed any
			if json.Unmarshal([]byte(*raw), &parsed) == nil {
				content = parsed
			}
		}
		out = append(out, entriesFromContent(role, content, ts)...)
		if role == RoleAssistant {
			out = append(out, hermesToolEntries(row["tool_calls"], ts)...)
		}
	}
	return out, nil
}

// hermesToolEntries converts the tool_calls JSON column into tool cards.
func hermesToolEntries(raw any, ts float64) []Entry {
	var calls []any
	switch v := raw.(type) {
	case string:
		if json.Unmarshal([]byte(v), &calls) != nil {
			return nil
		}
	case nil:
		return nil
	default:
		return nil
	}
	var out []Entry
	for _, item := range calls {
		call := GetMap(item)
		fn := GetMap(call["function"])
		name := firstString(fn, "name")
		if name == "" {
			continue
		}
		out = append(out, Entry{
			Role:      RoleAssistant,
			Tools:     []ToolCall{toolCall(name, argumentsMap(fn["arguments"]))},
			Timestamp: ts,
		})
	}
	return out
}

// epochSecondsOrMs accepts both second and millisecond epochs.
func epochSecondsOrMs(v any) float64 {
	if f, ok := AsNumber(v); ok {
		if f > 0 && f < 1e11 {
			return f * 1000
		}
		return f
	}
	if ts := AsEpochMs(v); ts != nil {
		return *ts
	}
	return 0
}
