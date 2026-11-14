package agentsession

// usage_zcode.go mirrors src/main/usage/zcode.ts:
// model_usage (completed only) provides the record points, message (user
// prompt semantics) and tool_usage the events; the three tables are merged
// per session and walked in time order. Input includes cacheRead and is
// normalized by subtraction (no clamping, may go negative — same as TS).
// Skill names for Skill tool calls are joined from the part table's
// state.input.skill via (session_id, callID ↔ tool_call_id); when the part
// table is missing (older db) only the tool-call count survives (the same
// degradation as TS). Same as TS: a missing/unopenable db (zcode not
// installed) counts as no data (empty result, not a module failure, like
// opencode); query/missing-table failures also yield an empty result.

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
)

type zcodeEntryKind int

const (
	zcodePrompt zcodeEntryKind = iota
	zcodeTool
	zcodeRecord
)

type zcodeEntry struct {
	ts    float64
	kind  zcodeEntryKind
	tool  *string
	skill *string
	row   map[string]any
}

func scanZcodeUsage(agentDir string) (*usageModuleResult, error) {
	dbPath := filepath.Join(agentDir, "cli/db/db.sqlite")
	db, err := OpenSQLite(dbPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		// zcode not installed: treat as no data (same as opencode), do not
		// demote the global status to 2
		return &usageModuleResult{
			sessions: []SessionInfo{},
			records:  []UsageRecord{},
		}, nil
	}
	defer db.Close()

	// Session list (same filter as agents/zcode.ts: excludes sub-agent
	// sessions)
	sessionRows, err := QueryMaps(db,
		"SELECT id, directory, title, time_created, time_updated FROM session WHERE task_type IS NULL OR task_type = 'interactive'")
	if err != nil {
		return nil, err
	}
	sessions := []SessionInfo{}
	sessionIDs := map[string]bool{}
	for _, row := range sessionRows {
		id, idOK := row["id"].(string)
		directory, dirOK := row["directory"].(string)
		created, createdOK := AsNumber(row["time_created"])
		if !idOK || !dirOK || !createdOK {
			continue
		}
		var title *string
		if t, ok := row["title"].(string); ok {
			title = &t
		}
		sessions = append(sessions, SessionInfo{
			ID:        id,
			Workspace: directory,
			Title:     title,
			CreatedAt: created,
			UpdatedAt: FirstOr(created, numberPtr(row["time_updated"])),
			SizeBytes: 0,
			FilePath:  strPtr(dbPath + "#" + id),
		})
		sessionIDs[id] = true
	}

	// Merge the three row kinds per session (sub-agent session rows are not in
	// sessionIDs and drop out naturally)
	perSession := NewOrderedMap[[]zcodeEntry]()
	add := func(sessionID string, entry zcodeEntry) {
		if !sessionIDs[sessionID] {
			return
		}
		list, _ := perSession.Get(sessionID)
		perSession.Set(sessionID, append(list, entry))
	}

	messageRows, err := QueryMaps(db,
		`SELECT session_id, time_created,
			json_extract(data, '$.role') AS role, json_extract(data, '$.semantics.kind') AS kind
		 FROM message`)
	if err != nil {
		return &usageModuleResult{sessions: sessions, records: []UsageRecord{}}, nil
	}
	for _, row := range messageRows {
		if !IsStr(row["role"], "user") || !IsStr(row["kind"], "user_prompt") {
			continue
		}
		if sessionID, ok := row["session_id"].(string); ok {
			if ts, tsOK := AsNumber(row["time_created"]); tsOK {
				add(sessionID, zcodeEntry{ts: ts, kind: zcodePrompt})
			}
		}
	}

	// Skill-call names: tool_usage has no parameter column; only the part
	// table's tool parts carry state.input.skill, joined via (session_id,
	// callID ↔ tool_call_id); on query failure or a missing table degrade to
	// counting tool calls without skill names
	skillByCall := map[string]string{}
	if partRows, err := QueryMaps(db,
		`SELECT session_id, json_extract(data, '$.callID') AS call_id,
			json_extract(data, '$.state.input.skill') AS skill
		 FROM part
		 WHERE json_extract(data, '$.tool') = 'Skill'
			AND json_extract(data, '$.state.input.skill') IS NOT NULL`); err == nil {
		for _, row := range partRows {
			sessionID, idOK := row["session_id"].(string)
			callID, callOK := row["call_id"].(string)
			skill, skillOK := row["skill"].(string)
			if idOK && callOK && skillOK && skill != "" {
				skillByCall[sessionID+"#"+callID] = skill
			}
		}
	}

	toolRows, err := QueryMaps(db, "SELECT session_id, tool_call_id, tool_name, started_at FROM tool_usage")
	if err != nil {
		return &usageModuleResult{sessions: sessions, records: []UsageRecord{}}, nil
	}
	for _, row := range toolRows {
		sessionID, ok := row["session_id"].(string)
		if !ok {
			continue
		}
		if ts, tsOK := AsNumber(row["started_at"]); tsOK {
			tool := AsString(row["tool_name"])
			var skill *string
			if tool != nil && *tool == "Skill" {
				if callID, callOK := row["tool_call_id"].(string); callOK {
					if name, found := skillByCall[sessionID+"#"+callID]; found {
						skill = &name
					}
				}
			}
			add(sessionID, zcodeEntry{ts: ts, kind: zcodeTool, tool: tool, skill: skill})
		}
	}

	// Per-request usage (completed requests only; error/cancelled requests
	// have their tokens counted already but the semantics are unclear, so they
	// are excluded)
	usageRows, err := QueryMaps(db,
		`SELECT session_id, model_id, completed_at,
			input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens
		 FROM model_usage WHERE status = 'completed'`)
	if err != nil {
		return &usageModuleResult{sessions: sessions, records: []UsageRecord{}}, nil
	}
	for _, row := range usageRows {
		sessionID, ok := row["session_id"].(string)
		if !ok {
			continue
		}
		if ts, tsOK := AsNumber(row["completed_at"]); tsOK {
			add(sessionID, zcodeEntry{ts: ts, kind: zcodeRecord, row: row})
		}
	}

	records := []UsageRecord{}
	for _, sessionID := range perSession.Keys() {
		entries, _ := perSession.Get(sessionID)
		// Events at the same instant settle before the record point
		sort.SliceStable(entries, func(a, b int) bool {
			if entries[a].ts != entries[b].ts {
				return entries[a].ts < entries[b].ts
			}
			return zcodeOrderOf(entries[a]) < zcodeOrderOf(entries[b])
		})
		tally := &SessionTally{}
		sessionRecords := []UsageRecord{}
		for _, entry := range entries {
			if entry.kind == zcodeRecord {
				tally.AddEvent(entry.ts, TallyAssistant, nil)
				row := entry.row
				cacheRead := AsTokenCount(row["cache_read_input_tokens"])
				record := UsageRecord{
					SessionID:        sessionID,
					AgentID:          "zcode",
					Model:            AsString(row["model_id"]),
					Timestamp:        entry.ts,
					InputTokens:      AsTokenCount(row["input_tokens"]) - cacheRead,
					OutputTokens:     AsTokenCount(row["output_tokens"]),
					CacheReadTokens:  cacheRead,
					CacheWriteTokens: AsTokenCount(row["cache_creation_input_tokens"]),
				}
				record.ApplyInterval(tally.TakeUntil(entry.ts))
				sessionRecords = append(sessionRecords, record)
			} else if entry.kind == zcodePrompt {
				tally.AddEvent(entry.ts, TallyPrompt, nil)
			} else {
				tally.AddEvent(entry.ts, TallyTool, entry.tool, entry.skill)
			}
		}
		if len(sessionRecords) > 0 {
			sessionRecords[len(sessionRecords)-1].MergeInterval(tally.Flush())
			records = append(records, sessionRecords...)
		}
	}
	return &usageModuleResult{sessions: sessions, records: records}, nil
}

func zcodeOrderOf(entry zcodeEntry) int {
	if entry.kind == zcodeRecord {
		return 1
	}
	return 0
}

func numberPtr(v any) *float64 {
	if f, ok := AsNumber(v); ok {
		return &f
	}
	return nil
}
