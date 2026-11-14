package agentsession

// codex.go mirrors src/main/usage/codex.ts:
// event_msg/token_count total_token_usage is a session cumulative value; take deltas between adjacent
// events (negative deltas clamped to 0); input includes cached_input_tokens, subtracted during
// normalization; the model name comes from the first turn_context.

import (
	"math"
	"path/filepath"
	"strings"
	"unicode"
)

func scanCodexUsage(agentDir string) (*usageModuleResult, error) {
	var files []string
	for _, year := range ListDirs(filepath.Join(agentDir, "sessions")) {
		for _, month := range ListDirs(year) {
			for _, day := range ListDirs(month) {
				files = append(files, ListFiles(day, ".jsonl", false)...)
			}
		}
	}
	results, err := MapLimit(files, FileConcurrency, scanCodexFileUsage)
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

type codexFileUsage struct {
	session SessionInfo
	records []UsageRecord
}

type codexTokenTotal struct {
	input, output, cacheRead, cacheWrite float64
}

func scanCodexFileUsage(file string) (*codexFileUsage, error) {
	session, err := ScanCodexFile(file)
	if err != nil || session == nil {
		return nil, err
	}
	records, err := scanCodexUsageRecords(file, session.ID, "codex")
	if err != nil {
		return nil, err
	}
	return &codexFileUsage{session: *session, records: records}, nil
}

func scanCodexSessionUsage(session SessionInfo, agentID string) ([]UsageRecord, error) {
	return scanCodexUsageRecords(*session.FilePath, session.ID, agentID)
}

func scanCodexUsageRecords(file, sessionID, agentID string) ([]UsageRecord, error) {
	lines := ReadAllLines(file)
	tally := &SessionTally{}
	records := []UsageRecord{}
	var model *string
	var prev *codexTokenTotal

	WalkLines(lines, func(obj map[string]any) {
		payload := GetMap(obj["payload"])

		// Model name: turn_context appears at the session head; take the first one
		if IsStr(obj["type"], "turn_context") {
			if model == nil {
				if m := AsString(payload["model"]); m != nil {
					model = m
				}
			}
			return
		}

		if IsStr(obj["type"], "response_item") {
			ts := AsTimestamp(obj["timestamp"])
			if ts == nil {
				return
			}
			if IsStr(payload["type"], "message") {
				if IsStr(payload["role"], "user") {
					if codexIsUserPrompt(payload["content"]) {
						tally.AddEvent(*ts, TallyPrompt, nil)
					}
				} else if IsStr(payload["role"], "assistant") {
					tally.AddEvent(*ts, TallyAssistant, nil)
				}
			} else if IsStr(payload["type"], "custom_tool_call") || IsStr(payload["type"], "function_call") {
				tally.AddEvent(*ts, TallyTool, AsString(payload["name"]))
			}
			return
		}

		if !IsStr(obj["type"], "event_msg") || !IsStr(payload["type"], "token_count") {
			return
		}
		ts := AsTimestamp(obj["timestamp"])
		if ts == nil {
			return
		}
		total := GetMap(GetMap(payload["info"])["total_token_usage"])
		if total == nil {
			return
		}

		tally.AddEvent(*ts, TallyAssistant, nil)
		cur := codexTokenTotal{
			input:      AsTokenCount(total["input_tokens"]),
			output:     AsTokenCount(total["output_tokens"]),
			cacheRead:  AsTokenCount(total["cached_input_tokens"]),
			cacheWrite: AsTokenCount(total["cache_write_input_tokens"]),
		}
		delta := cur
		if prev != nil {
			// The cumulative counter may fall back when the session is compacted or reset; treat negative deltas as 0
			delta = codexTokenTotal{
				input:      math.Max(0, cur.input-prev.input),
				output:     math.Max(0, cur.output-prev.output),
				cacheRead:  math.Max(0, cur.cacheRead-prev.cacheRead),
				cacheWrite: math.Max(0, cur.cacheWrite-prev.cacheWrite),
			}
		}
		prev = &cur
		record := UsageRecord{
			SessionID:        sessionID,
			AgentID:          agentID,
			Model:            model,
			Timestamp:        *ts,
			InputTokens:      math.Max(0, delta.input-delta.cacheRead),
			OutputTokens:     delta.output,
			CacheReadTokens:  delta.cacheRead,
			CacheWriteTokens: delta.cacheWrite,
		}
		record.ApplyInterval(tally.TakeUntil(*ts))
		records = append(records, record)
	})

	if len(records) > 0 {
		records[len(records)-1].MergeInterval(tally.Flush())
	}
	return records, nil
}

// codexIsUserPrompt mirrors isUserPrompt from usage/codex.ts:
// first input_text text; skip injections starting with < and AGENTS.md instruction injections.
func codexIsUserPrompt(content any) bool {
	blocks, ok := content.([]any)
	if !ok {
		return false
	}
	for _, part := range blocks {
		block := GetMap(part)
		if block == nil {
			continue
		}
		if !IsStr(block["type"], "input_text") {
			continue
		}
		text, ok := block["text"].(string)
		if !ok {
			continue
		}
		trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
		return len(trimmed) > 0 && !strings.HasPrefix(trimmed, "<") && !strings.HasPrefix(trimmed, "# AGENTS.md")
	}
	return false
}
