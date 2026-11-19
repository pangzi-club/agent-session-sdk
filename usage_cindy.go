package agentsession

import (
	"errors"
	"fmt"
	"os"
)

// Cindy sessions can use Pi, Codex or Claude Code. Each engine owns its token
// format, while records retain Cindy's profile ID and agent ID. Sessions that
// fall back to the messages mirror ("cindy-db" format) aggregate usage from
// the per-row agent_meta usage objects instead of an engine transcript.

func scanCindyUsage(agentDir string) (*usageModuleResult, error) {
	sessions, err := ScanCindy(agentDir)
	if err != nil {
		return nil, err
	}
	results, err := MapLimit(sessions, FileConcurrency, scanCindySessionUsage)
	if err != nil {
		return nil, err
	}
	outSessions := []SessionInfo{}
	records := []UsageRecord{}
	for _, result := range results {
		outSessions = append(outSessions, result.session)
		records = append(records, result.records...)
	}
	return &usageModuleResult{sessions: outSessions, records: records}, nil
}

type cindySessionUsage struct {
	session SessionInfo
	records []UsageRecord
}

// Cindy session transcripts are stored as absolute paths in the profile DB and
// can drift out of sync with disk (archived or deleted while the row stays
// active). A missing or unreadable transcript skips that one session instead
// of failing the whole module: a stale row must not wipe Cindy's usage for
// every other session, and the aggregation layer must not mistake that for
// "Cindy not installed".
// scanCindySessionUsage routes one session to its engine's usage extractor.
// The messages-mirror format reads usage from the profile database itself.
func scanCindySessionUsage(session SessionInfo) (*cindySessionUsage, error) {
	result := &cindySessionUsage{session: session, records: []UsageRecord{}}
	if session.transcriptFormat == "cindy-db" {
		records, err := scanCindyMirrorUsage(session)
		if err != nil {
			return nil, err
		}
		result.records = records
		return result, nil
	}
	if session.FilePath == nil || *session.FilePath == "" {
		return result, nil
	}
	if info, err := os.Stat(*session.FilePath); err != nil {
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) {
			return nil, err
		}
		return result, nil
	} else if !info.Mode().IsRegular() {
		return result, nil
	}
	switch session.transcriptFormat {
	case "pi":
		usage, err := scanPiSessionUsage(session, "cindy")
		if err != nil {
			return nil, err
		}
		result.records = usage.records
	case "codex":
		usage, err := scanCodexSessionUsage(session, "cindy")
		if err != nil {
			return nil, err
		}
		result.records = usage
	case "claude":
		usage, err := scanClaudeSessionUsage(session, "cindy")
		if err != nil {
			return nil, err
		}
		result.records = usage
	default:
		return nil, fmt.Errorf("session %q has unsupported Cindy usage format %q", session.ID, session.transcriptFormat)
	}
	return result, nil
}

// scanCindyMirrorUsage aggregates a mirror-backed session's usage from the
// messages table. Every row of one model step repeats that step's usage object
// in agent_meta under a shared requestId (non-assistant rows carry a zero
// placeholder), so usage counts once per distinct requestId and zero rows are
// skipped. The usage keys differ per engine shape (input/output/cacheRead/
// cacheWrite or inputTokens/outputTokens/cacheReadTokens/cacheWriteTokens).
func scanCindyMirrorUsage(session SessionInfo) ([]UsageRecord, error) {
	dbPath, _, ok := splitDBRef(session)
	if !ok {
		return []UsageRecord{}, nil
	}
	db, err := OpenSQLite(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := QueryMaps(db, `SELECT role, content, created_at, agent_meta
		FROM messages
		WHERE session_id = ? AND rewind_at IS NULL
		ORDER BY created_at, rowid`, session.ID)
	if err != nil {
		return nil, err
	}

	tally := &SessionTally{}
	records := []UsageRecord{}
	seen := map[string]bool{}
	for _, row := range rows {
		var timestamp float64
		if ts := AsEpochMs(row["created_at"]); ts != nil {
			timestamp = *ts
		}
		// Activity events do not depend on agent_meta: tool rows usually carry
		// no meta, and usage rows repeat one step's meta on every row.
		meta := parseJSONMap(row["agent_meta"])
		switch firstString(row, "role") {
		case "user":
			tally.AddEvent(timestamp, TallyPrompt, nil)
		case "tool_use":
			if content := parseJSONMap(fmtString(row["content"])); content != nil {
				if name := firstString(content, "toolName"); name != "" {
					tally.AddEvent(timestamp, TallyTool, &name)
				}
			}
		}
		if meta == nil {
			continue
		}
		model := AsString(meta["model"])
		usage := GetMap(meta["usage"])
		if usage == nil {
			continue
		}
		input := firstNumber(usage, "input", "inputTokens")
		output := firstNumber(usage, "output", "outputTokens")
		if input == 0 && output == 0 {
			continue
		}
		// One model step repeats its usage on every row of the step; the
		// requestId identifies the step, and rows without one dedupe by
		// timestamp+tokens (older mirrors repeated without requestId).
		requestID := firstString(meta, "requestId")
		if requestID != "" {
			if seen[requestID] {
				continue
			}
			seen[requestID] = true
		} else {
			key := fmt.Sprintf("%d:%.0f:%.0f", int64(timestamp), input, output)
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		record := UsageRecord{
			SessionID:        session.ID,
			AgentID:          "cindy",
			Model:            model,
			Timestamp:        timestamp,
			InputTokens:      input,
			OutputTokens:     output,
			CacheReadTokens:  firstNumber(usage, "cacheRead", "cacheReadTokens"),
			CacheWriteTokens: firstNumber(usage, "cacheWrite", "cacheWriteTokens"),
		}
		record.ApplyInterval(tally.TakeUntil(timestamp))
		records = append(records, record)
	}
	if len(records) > 0 {
		records[len(records)-1].MergeInterval(tally.Flush())
	}
	return records, nil
}

// fmtString returns the value as a string when it is text, else "".
func fmtString(v any) string {
	if s := AsString(v); s != nil {
		return *s
	}
	return ""
}
