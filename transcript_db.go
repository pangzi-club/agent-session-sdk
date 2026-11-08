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
		ts := AsEpochMs(row["message_time"])
		if cron := GetMap(message["time"]); cron != nil {
			if created := AsEpochMs(cron["created"]); created != nil {
				ts = created
			}
		}
		var timestamp float64
		if ts != nil {
			timestamp = *ts
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
			out = append(out, entriesFromContent(role, text, timestamp)...)
		case "tool":
			name := firstString(part, "tool")
			if name == "" {
				continue
			}
			state := GetMap(part["state"])
			call := toolCall(name, GetMap(state["input"]))
			if status := firstString(state, "status"); status != "" && status != "completed" {
				call.Meta += " | " + status
			}
			out = append(out, Entry{Role: RoleAssistant, Tools: []ToolCall{call}, Timestamp: timestamp})
		case "reasoning", "step-start", "step-finish", "file", "timeline":
			// Internal steps and attachments are not part of the chat view.
		}
	}
	return out, nil
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
