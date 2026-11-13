package agentsession

// usage_pi.go mirrors src/main/usage/pi.ts:
// lines with type=message and role=assistant carry that turn's tokens in
// message.usage (input excludes cache); the timestamp prefers
// message.timestamp (milliseconds) and falls back to the line-level ISO
// timestamp.

import (
	"path/filepath"
)

func scanPiUsage(agentDir string) (*usageModuleResult, error) {
	files := ListFiles(filepath.Join(agentDir, "agent", "sessions"), ".jsonl", true)
	results, err := MapLimit(files, FileConcurrency, scanPiFileUsage)
	if err != nil {
		return nil, err
	}
	sessions := []SessionInfo{}
	records := []UsageRecord{}
	for _, result := range results {
		if result == nil {
			continue
		}
		sessions = append(sessions, result.session)
		records = append(records, result.records...)
	}
	return &usageModuleResult{sessions: sessions, records: records}, nil
}

type piFileUsage struct {
	session SessionInfo
	records []UsageRecord
}

func scanPiFileUsage(file string) (*piFileUsage, error) {
	session, err := ScanPiFile(file)
	if err != nil || session == nil {
		return nil, err
	}
	return scanPiSessionUsage(*session, "pi")
}

func scanPiSessionUsage(session SessionInfo, agentID string) (*piFileUsage, error) {
	if session.FilePath == nil || *session.FilePath == "" {
		return &piFileUsage{session: session, records: []UsageRecord{}}, nil
	}
	lines := ReadAllLines(*session.FilePath)
	tally := &SessionTally{}
	records := []UsageRecord{}
	WalkLines(lines, func(obj map[string]any) {
		if !IsStr(obj["type"], "message") {
			return
		}
		message := GetMap(obj["message"])
		// message.timestamp is in milliseconds; fall back to the line-level
		// ISO timestamp when missing
		timestamp := 0.0
		if f, ok := AsNumber(message["timestamp"]); ok && f > 0 {
			timestamp = f
		} else {
			timestamp = FirstOr(0.0, AsEpochMs(obj["timestamp"]))
		}
		if timestamp == 0 {
			return
		}

		if IsStr(message["role"], "user") {
			tally.AddEvent(timestamp, TallyPrompt, nil)
			return
		}
		if !IsStr(message["role"], "assistant") {
			return
		}

		tally.AddEvent(timestamp, TallyAssistant, nil)
		if blocks := GetSlice(message["content"]); blocks != nil {
			for _, part := range blocks {
				block := GetMap(part)
				if block == nil {
					continue
				}
				if IsStr(block["type"], "toolCall") {
					if name := AsString(block["name"]); name != nil {
						tally.AddEvent(timestamp, TallyTool, name)
					}
				}
			}
		}

		usage := GetMap(message["usage"])
		if usage == nil {
			return
		}
		record := UsageRecord{
			SessionID:        session.ID,
			AgentID:          agentID,
			Model:            AsString(message["model"]),
			Timestamp:        timestamp,
			InputTokens:      AsTokenCount(usage["input"]),
			OutputTokens:     AsTokenCount(usage["output"]),
			CacheReadTokens:  AsTokenCount(usage["cacheRead"]),
			CacheWriteTokens: AsTokenCount(usage["cacheWrite"]),
		}
		record.ApplyInterval(tally.TakeUntil(timestamp))
		records = append(records, record)
	})
	if len(records) > 0 {
		records[len(records)-1].MergeInterval(tally.Flush())
	}
	return &piFileUsage{session: session, records: records}, nil
}
