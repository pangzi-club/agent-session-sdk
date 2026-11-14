package agentsession

// grok.go mirrors src/main/usage/grok.ts:
// turn_completed events in updates.jsonl are record points via update.usage (cachedReadTokens = cache reads,
// cacheCreationTokens = cache writes, input excludes cache); modelUsage keys are model names (pick the one with
// the largest totalTokens, tie-break by lexicographically smallest — Go map iteration is unordered, so this rule
// is what keeps us aligned with the TS version);
// user_message_chunk = prompts, tool_call = tool calls (title is the tool name).
// Event timestamps prefer params._meta.agentTimestampMs (milliseconds), falling back to the line-top timestamp (seconds).
// The session list reuses ScanGrokSessionDir (the same summary.json); directories without summary.json are skipped entirely.

import (
	"errors"
	"math"
	"os"
	"path/filepath"
)

func scanGrokUsage(agentDir string) (*usageModuleResult, error) {
	dirs, err := listGrokSessionDirsE(agentDir)
	if err != nil {
		return nil, err
	}
	results, err := MapLimit(
		dirs,
		FileConcurrency,
		scanGrokSessionUsage,
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

type grokSessionUsage struct {
	session SessionInfo
	records []UsageRecord
}

func scanGrokSessionUsage(dir string) (*grokSessionUsage, error) {
	session, err := ScanGrokSessionDir(dir)
	if err != nil || session == nil {
		return nil, err
	}

	tally := &SessionTally{}
	records := []UsageRecord{}
	err = WalkJSONLFile(filepath.Join(dir, "updates.jsonl"), func(obj map[string]any) {
		params := GetMap(obj["params"])
		update := GetMap(params["update"])
		if update == nil {
			return
		}
		timestamp := grokEventTimestamp(obj, params)
		if timestamp == 0 {
			return
		}

		kind, _ := update["sessionUpdate"].(string)
		switch kind {
		case "user_message_chunk":
			tally.AddEvent(timestamp, TallyPrompt, nil)
			return
		case "tool_call":
			tally.AddEvent(timestamp, TallyTool, AsString(update["title"]))
			return
		case "turn_completed":
			// fall through to emit a record below
		default:
			return
		}

		tally.AddEvent(timestamp, TallyAssistant, nil)
		usage := GetMap(update["usage"])
		if usage == nil {
			return
		}
		record := UsageRecord{
			SessionID:        session.ID,
			AgentID:          "grok",
			Model:            pickGrokModel(usage),
			Timestamp:        timestamp,
			InputTokens:      AsTokenCount(usage["inputTokens"]),
			OutputTokens:     AsTokenCount(usage["outputTokens"]),
			CacheReadTokens:  AsTokenCount(usage["cachedReadTokens"]),
			CacheWriteTokens: AsTokenCount(usage["cacheCreationTokens"]),
		}
		record.ApplyInterval(tally.TakeUntil(timestamp))
		records = append(records, record)
	})
	// summary.json is the session marker; a session dir without updates.jsonl
	// simply has no usage events (it contributes the session list entry only).
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(records) > 0 {
		records[len(records)-1].MergeInterval(tally.Flush())
	}
	return &grokSessionUsage{session: *session, records: records}, nil
}

// grokEventTimestamp mirrors eventTimestamp: params._meta.agentTimestampMs takes priority, falling back to the line-top timestamp in seconds ×1000.
func grokEventTimestamp(obj, params map[string]any) float64 {
	meta := GetMap(params["_meta"])
	if ms := AsEpochMs(meta["agentTimestampMs"]); ms != nil {
		return *ms
	}
	if seconds, ok := AsNumber(obj["timestamp"]); ok {
		return math.Floor(seconds * 1000)
	}
	return 0
}

// pickGrokModel mirrors pickModel: the modelUsage entry with the largest totalTokens; on ties, the lexicographically smallest name.
func pickGrokModel(usage map[string]any) *string {
	modelUsage := GetMap(usage["modelUsage"])
	var best *string
	bestTotal := -1.0
	for name, value := range modelUsage {
		total := AsTokenCount(GetMap(value)["totalTokens"])
		if best == nil || total > bestTotal || (total == bestTotal && name < *best) {
			picked := name
			best = &picked
			bestTotal = total
		}
	}
	return best
}
