package agentsession

// opencode.go mirrors src/main/usage/opencode.ts:
// the message table's data column holds JSON (assistant carries tokens{input,output,reasoning,cache{read,write}}, reasoning ignored)
// + part rows with data.type='tool' as tool events, merged per session and walked in time order.
// Same as TS: db unopenable → empty result (no status downgrade); record points missing tokens still count toward the tally, then are skipped.

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sort"
	"strconv"
)

type opencodeEntryKind int

const (
	opencodePrompt opencodeEntryKind = iota
	opencodeTool
	opencodeRecord
)

type opencodeEntry struct {
	ts   float64
	kind opencodeEntryKind
	tool *string
	data map[string]any
}

func scanOpenCodeUsage(agentDir string) (*usageModuleResult, error) {
	dbPath := filepath.Join(agentDir, "opencode.db")
	db, err := OpenSQLite(dbPath)
	if err != nil {
		return &usageModuleResult{sessions: []SessionInfo{}, records: []UsageRecord{}}, nil
	}
	defer db.Close()

	result, err := scanOpenCodeDb(db, dbPath)
	if err != nil {
		return &usageModuleResult{sessions: []SessionInfo{}, records: []UsageRecord{}}, nil
	}
	return result, nil
}

func scanOpenCodeDb(db *sql.DB, dbPath string) (*usageModuleResult, error) {
	sessionRows, err := QueryMaps(db,
		"SELECT id, parent_id, directory, title, time_created, time_updated FROM session")
	if err != nil {
		return &usageModuleResult{sessions: []SessionInfo{}, records: []UsageRecord{}}, nil
	}
	sessions := []SessionInfo{}
	sessionIDs := map[string]bool{}
	for _, row := range sessionRows {
		if parentID, ok := row["parent_id"]; ok && parentID != nil {
			if s, isStr := parentID.(string); isStr && s != "" {
				continue
			}
		}
		id := sqlToString(row["id"])
		workspace := sqlToString(row["directory"])
		if workspace == "" {
			continue
		}
		createdAt := 0.0
		if f, ok := AsNumber(row["time_created"]); ok {
			createdAt = f
		}
		updatedAt := createdAt
		if f, ok := AsNumber(row["time_updated"]); ok {
			updatedAt = f
		}
		sessions = append(sessions, SessionInfo{
			ID:        id,
			Workspace: workspace,
			Title:     ToTitle(AsString(row["title"])),
			CreatedAt: createdAt,
			UpdatedAt: updatedAt,
			SizeBytes: 0,
			// Data source is not a single file: db path + session id (same convention as agents/opencode.ts)
			FilePath: strPtr(dbPath + "#" + id),
		})
		sessionIDs[id] = true
	}

	perSession := NewOrderedMap[[]opencodeEntry]()
	add := func(sessionID string, entry opencodeEntry) {
		if !sessionIDs[sessionID] {
			return
		}
		list, _ := perSession.Get(sessionID)
		perSession.Set(sessionID, append(list, entry))
	}

	messageRows, err := QueryMaps(db, "SELECT session_id, data FROM message")
	if err != nil {
		return &usageModuleResult{sessions: sessions, records: []UsageRecord{}}, nil
	}
	for _, row := range messageRows {
		var obj map[string]any
		if err := json.Unmarshal([]byte(sqlToString(row["data"])), &obj); err != nil {
			continue
		}
		time := GetMap(obj["time"])
		ts := 0.0
		if f, ok := AsNumber(time["created"]); ok {
			ts = f
		}
		if ts == 0 {
			continue
		}
		if IsStr(obj["role"], "user") {
			// Subagent messages have a non-empty parentID and don't count as main-session user prompts
			if parentID := AsString(obj["parentID"]); parentID != nil && *parentID != "" {
				continue
			}
			add(sqlToString(row["session_id"]), opencodeEntry{ts: ts, kind: opencodePrompt})
		} else if IsStr(obj["role"], "assistant") {
			add(sqlToString(row["session_id"]), opencodeEntry{ts: ts, kind: opencodeRecord, data: obj})
		}
	}

	partRows, err := QueryMaps(db, "SELECT session_id, time_created, data FROM part")
	if err != nil {
		return &usageModuleResult{sessions: sessions, records: []UsageRecord{}}, nil
	}
	for _, row := range partRows {
		var obj map[string]any
		if err := json.Unmarshal([]byte(sqlToString(row["data"])), &obj); err != nil {
			continue
		}
		if !IsStr(obj["type"], "tool") {
			continue
		}
		tool := AsString(obj["tool"])
		if tool == nil {
			continue
		}
		ts, ok := AsNumber(row["time_created"])
		if !ok || ts == 0 {
			continue
		}
		add(sqlToString(row["session_id"]), opencodeEntry{ts: ts, kind: opencodeTool, tool: tool})
	}

	records := []UsageRecord{}
	for _, sessionID := range perSession.Keys() {
		entries, _ := perSession.Get(sessionID)
		// Events at the same instant settle before record points
		sort.SliceStable(entries, func(a, b int) bool {
			if entries[a].ts != entries[b].ts {
				return entries[a].ts < entries[b].ts
			}
			return opencodeOrderOf(entries[a]) < opencodeOrderOf(entries[b])
		})
		tally := &SessionTally{}
		sessionRecords := []UsageRecord{}
		for _, entry := range entries {
			if entry.kind == opencodeRecord {
				tally.AddEvent(entry.ts, TallyAssistant, nil)
				tokens := GetMap(entry.data["tokens"])
				if tokens == nil {
					continue
				}
				cache := GetMap(tokens["cache"])
				record := UsageRecord{
					SessionID:        sessionID,
					AgentID:          "opencode",
					Model:            AsString(entry.data["modelID"]),
					Timestamp:        entry.ts,
					InputTokens:      AsTokenCount(tokens["input"]),
					OutputTokens:     AsTokenCount(tokens["output"]),
					CacheReadTokens:  AsTokenCount(cache["read"]),
					CacheWriteTokens: AsTokenCount(cache["write"]),
				}
				record.ApplyInterval(tally.TakeUntil(entry.ts))
				sessionRecords = append(sessionRecords, record)
			} else if entry.kind == opencodePrompt {
				tally.AddEvent(entry.ts, TallyPrompt, nil)
			} else {
				tally.AddEvent(entry.ts, TallyTool, entry.tool)
			}
		}
		if len(sessionRecords) > 0 {
			sessionRecords[len(sessionRecords)-1].MergeInterval(tally.Flush())
			records = append(records, sessionRecords...)
		}
	}
	return &usageModuleResult{sessions: sessions, records: records}, nil
}

func opencodeOrderOf(entry opencodeEntry) int {
	if entry.kind == opencodeRecord {
		return 1
	}
	return 0
}

// sqlToString mirrors TS String(...) coercion (number → "5", null → "null"? no — TS String(row.id ?? "")).
// Semantics per call site: id/directory are plain strings or numbers; data is TEXT. null becomes "".
func sqlToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}
