package agentsession

// dsh.go mirrors src/main/usage/dsh.ts:
// zstd JSONL: assistant/message lines carry per-turn tokens in data.usage (input excludes cache, cacheWrite is always 0),
// user/message = prompts, tool/call = tool calls, and the line's time field is a millisecond timestamp.
// File enumeration reuses ListDshSessionFiles so the version-matching rule lives in one place only.

import (
	"os"
	"strings"
)

func scanDshUsage(agentDir string) (*usageModuleResult, error) {
	files, err := listDshSessionFilesE(agentDir)
	if err != nil {
		return nil, err
	}
	results, err := MapLimit(
		files,
		FileConcurrency,
		scanDshFileUsage,
	)
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

type dshFileUsage struct {
	session SessionInfo
	records []UsageRecord
}

func scanDshFileUsage(file string) (*dshFileUsage, error) {
	session, err := ScanDshFile(file)
	if err != nil || session == nil {
		return nil, err
	}
	compressed, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	decompressed, err := DecompressZstd(compressed)
	if err != nil {
		return nil, err
	}

	tally := &SessionTally{}
	records := []UsageRecord{}
	WalkLines(strings.Split(string(decompressed), "\n"), func(obj map[string]any) {
		time := 0.0
		if f, ok := AsNumber(obj["time"]); ok {
			time = f
		}
		if time == 0 {
			return
		}

		if IsStr(obj["type"], "user/message") {
			tally.AddEvent(time, TallyPrompt, nil)
			return
		}
		if IsStr(obj["type"], "tool/call") {
			data := GetMap(obj["data"])
			tally.AddEvent(time, TallyTool, AsString(data["name"]))
			return
		}
		if !IsStr(obj["type"], "assistant/message") {
			return
		}

		tally.AddEvent(time, TallyAssistant, nil)
		data := GetMap(obj["data"])
		usage := GetMap(data["usage"])
		if usage == nil {
			return
		}
		source := GetMap(GetMap(data["message"])["source"])
		record := UsageRecord{
			SessionID:        session.ID,
			AgentID:          "dsh",
			Model:            AsString(source["model"]),
			Timestamp:        time,
			InputTokens:      AsTokenCount(usage["inputTokens"]),
			OutputTokens:     AsTokenCount(usage["outputTokens"]),
			CacheReadTokens:  AsTokenCount(usage["cacheReadTokens"]),
			CacheWriteTokens: 0,
		}
		record.ApplyInterval(tally.TakeUntil(time))
		records = append(records, record)
	})
	if len(records) > 0 {
		records[len(records)-1].MergeInterval(tally.Flush())
	}
	return &dshFileUsage{session: *session, records: records}, nil
}
