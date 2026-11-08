package agentsession

// hermes.go mirrors src/main/usage/hermes.ts:
// each row of session_model_usage is a record point per (session × model × task) (timestamp = last_seen, epoch seconds,
// token columns are read directly and separately, input excludes cache reads/writes); in messages, user rows with
// compacted = 0 (no tool_name) are prompts, tool rows are tool calls (compacted = 1 rows are compaction
// replacement artifacts and don't count).
// The session list reuses ScanHermes (same hidden / archived filtering and workspace grouping).
// Same as TS: db missing/unopenable (hermes not installed) → empty; query failure → error (caller treats the agent as having no data).

import (
	"path/filepath"
	"sort"
)

type hermesEntryKind int

const (
	hermesPrompt hermesEntryKind = iota
	hermesTool
	hermesRecord
)

type hermesEntry struct {
	ts   float64
	kind hermesEntryKind
	tool *string
	row  map[string]any
}

func scanHermesUsage(agentDir string) (*usageModuleResult, error) {
	dbPath := filepath.Join(agentDir, "state.db")
	db, err := OpenSQLite(dbPath)
	if err != nil {
		// hermes not installed: treat as no data (same as zcode); don't mark this module as failed
		return &usageModuleResult{
			sessions: []SessionInfo{},
			records:  []UsageRecord{},
		}, nil
	}
	defer db.Close()

	sessions, err := ScanHermes(agentDir)
	if err != nil {
		return nil, err
	}
	sessionIDs := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		sessionIDs[session.ID] = true
	}

	perSession := NewOrderedMap[[]hermesEntry]()
	add := func(sessionID string, entry hermesEntry) {
		if !sessionIDs[sessionID] {
			return
		}
		list, _ := perSession.Get(sessionID)
		perSession.Set(sessionID, append(list, entry))
	}

	// Event stream: rows with compacted = 1 are compaction replacement artifacts and produce no prompt / tool events
	messageRows, err := QueryMaps(db,
		"SELECT session_id, role, tool_name, timestamp, compacted FROM messages")
	if err != nil {
		return nil, err
	}
	for _, row := range messageRows {
		if f, ok := AsNumber(row["compacted"]); ok && f == 1 {
			continue
		}
		sessionID, ok := row["session_id"].(string)
		if !ok {
			continue
		}
		ts := SecToMs(row["timestamp"])
		if ts == nil {
			continue
		}
		if IsStr(row["role"], "user") && row["tool_name"] == nil {
			add(sessionID, hermesEntry{ts: *ts, kind: hermesPrompt})
		} else if IsStr(row["role"], "tool") {
			add(sessionID, hermesEntry{ts: *ts, kind: hermesTool, tool: AsString(row["tool_name"])})
		}
	}

	// Record points: one aggregated row per (session × model × task); timestamp comes from last_seen
	usageRows, err := QueryMaps(db,
		`SELECT session_id, model, last_seen,
			input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
		 FROM session_model_usage`)
	if err != nil {
		return nil, err
	}
	for _, row := range usageRows {
		sessionID, ok := row["session_id"].(string)
		if !ok {
			continue
		}
		ts := SecToMs(row["last_seen"])
		if ts == nil {
			continue
		}
		add(sessionID, hermesEntry{ts: *ts, kind: hermesRecord, row: row})
	}

	records := []UsageRecord{}
	for _, sessionID := range perSession.Keys() {
		entries, _ := perSession.Get(sessionID)
		// Events at the same instant settle before record points
		sort.SliceStable(entries, func(a, b int) bool {
			if entries[a].ts != entries[b].ts {
				return entries[a].ts < entries[b].ts
			}
			return hermesOrderOf(entries[a]) < hermesOrderOf(entries[b])
		})
		tally := &SessionTally{}
		sessionRecords := []UsageRecord{}
		for _, entry := range entries {
			if entry.kind == hermesRecord {
				tally.AddEvent(entry.ts, TallyAssistant, nil)
				record := UsageRecord{
					SessionID:        sessionID,
					AgentID:          "hermes",
					Model:            AsString(entry.row["model"]),
					Timestamp:        entry.ts,
					InputTokens:      AsTokenCount(entry.row["input_tokens"]),
					OutputTokens:     AsTokenCount(entry.row["output_tokens"]),
					CacheReadTokens:  AsTokenCount(entry.row["cache_read_tokens"]),
					CacheWriteTokens: AsTokenCount(entry.row["cache_write_tokens"]),
				}
				record.ApplyInterval(tally.TakeUntil(entry.ts))
				sessionRecords = append(sessionRecords, record)
			} else if entry.kind == hermesPrompt {
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

func hermesOrderOf(entry hermesEntry) int {
	if entry.kind == hermesRecord {
		return 1
	}
	return 0
}
