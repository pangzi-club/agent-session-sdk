package agentsession

// usage_qoder.go mirrors src/main/usage/qoder.ts: the transcript has the same
// source as Claude Code, but Qoder bills by credits and the four token fields
// in message.usage are always 0. Heuristic reconstruction:
//   - input tokens = context_usage_ratio × runtime-config.contextWindow (the
//     window is the latest non-empty config, defaulting to 200k);
//   - output tokens = a weighted estimate over reply characters (CJK 1.5
//     tokens per character, otherwise 0.22 tokens per character); multiple
//     lines sharing one message.id (thinking / text / tool_use flushed in
//     batches) are merged and counted once;
//   - credits pass through untouched (free requests with billable === false
//     count too — the price represents the request's API cost), and the TS
//     side converts them to USD via the price table.
//
// Sub-agent transcripts nested under <uuid>/subagents/ are not descended
// into, for the same reason Claude skips isSidechain lines. The global and CN
// editions share this file; the registry has two defs that pass their own
// agentID.

import (
	"math"
	"os"
	"path/filepath"
	"strings"
)

const (
	// qoderDefaultContextWindow mirrors DEFAULT_CONTEXT_WINDOW.
	qoderDefaultContextWindow = 200_000
	qoderCjkTokensPerChar     = 1.5
	qoderOtherTokensPerChar   = 0.22
)

type qoderCharCounts struct {
	cjk   float64
	other float64
}

type qoderAssistantGroup struct {
	messageID     string
	hasMessageID  bool
	sessionID     *string
	model         *string
	timestamp     *float64
	usage         map[string]any
	contextWindow float64
	chars         qoderCharCounts
}

func scanQoderUsage(agentDir string) (*usageModuleResult, error) {
	return scanQoderUsageAs(agentDir, "qoder")
}

func scanQoderCnUsage(agentDir string) (*usageModuleResult, error) {
	return scanQoderUsageAs(agentDir, "qoder-cn")
}

func scanQoderUsageAs(agentDir, agentID string) (*usageModuleResult, error) {
	sessions, err := ScanQoder(agentDir)
	if err != nil {
		return nil, err
	}
	var records []UsageRecord
	projectDirs, err := qoderProjectDirs(agentDir)
	if err != nil {
		return nil, err
	}
	for _, projectDir := range projectDirs {
		projectRecords, err := scanQoderProjectUsage(projectDir, agentID)
		if err != nil {
			return nil, err
		}
		records = append(records, projectRecords...)
	}
	return &usageModuleResult{sessions: sessions, records: records}, nil
}

func scanQoderProjectUsage(projectDir, agentID string) ([]UsageRecord, error) {
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return nil, err
	}
	var out []UsageRecord
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".jsonl") {
			records, err := scanQoderFileUsage(filepath.Join(projectDir, entry.Name()), agentID)
			if err != nil {
				return nil, err
			}
			out = append(out, records...)
		}
	}
	return out, nil
}

func scanQoderFileUsage(file, agentID string) ([]UsageRecord, error) {
	lines, err := ReadAllLinesE(file)
	if err != nil {
		return nil, err
	}
	tally := &SessionTally{}
	records := []UsageRecord{}
	contextWindow := float64(qoderDefaultContextWindow)
	var group *qoderAssistantGroup

	flushGroup := func() {
		if group == nil {
			return
		}
		finished := group
		group = nil
		if finished.usage == nil || finished.sessionID == nil || finished.timestamp == nil {
			return
		}
		var inputTokens, outputTokens float64
		if ratio, ok := AsNumber(finished.usage["context_usage_ratio"]); ok && ratio >= 0 {
			if ratio > 1 {
				ratio = 1
			}
			inputTokens = math.Round(ratio * finished.contextWindow)
			outputTokens = math.Round(
				finished.chars.cjk*qoderCjkTokensPerChar + finished.chars.other*qoderOtherTokensPerChar,
			)
		} else {
			inputTokens = AsTokenCount(finished.usage["input_tokens"])
			outputTokens = AsTokenCount(finished.usage["output_tokens"])
		}
		record := UsageRecord{
			SessionID:        *finished.sessionID,
			AgentID:          agentID,
			Model:            finished.model,
			Timestamp:        *finished.timestamp,
			InputTokens:      inputTokens,
			OutputTokens:     outputTokens,
			CacheReadTokens:  AsTokenCount(finished.usage["cache_read_input_tokens"]),
			CacheWriteTokens: AsTokenCount(finished.usage["cache_creation_input_tokens"]),
		}
		if credits := qoderRequestCredits(finished.usage); credits > 0 {
			record.Credits = &credits
		}
		record.ApplyInterval(tally.TakeUntil(*finished.timestamp))
		records = append(records, record)
	}

	WalkLines(lines, func(obj map[string]any) {
		// Sub-agent lines interleaved in the main transcript are skipped too
		// (same as Claude).
		if AsBoolTrue(obj["isSidechain"]) {
			return
		}
		if IsStr(obj["type"], "runtime-config") {
			if window, ok := AsNumber(obj["contextWindow"]); ok && window > 0 {
				contextWindow = window
			}
			return
		}
		timestamp := AsTimestamp(obj["timestamp"])
		if timestamp == nil {
			return
		}
		message := GetMap(obj["message"])

		if IsStr(obj["type"], "user") {
			flushGroup()
			if QoderUserPromptText(message) != nil {
				tally.AddEvent(*timestamp, TallyPrompt, nil)
			}
			return
		}
		if !IsStr(obj["type"], "assistant") {
			return
		}

		tally.AddEvent(*timestamp, TallyAssistant, nil)
		rawID := AsString(message["id"])
		hasID := rawID != nil && *rawID != ""
		if group == nil || !hasID || !group.hasMessageID || group.messageID != *rawID {
			flushGroup()
			group = &qoderAssistantGroup{contextWindow: contextWindow}
			if hasID {
				group.messageID = *rawID
				group.hasMessageID = true
			}
		}
		active := group
		if sessionID := AsString(obj["sessionId"]); sessionID != nil && *sessionID != "" {
			active.sessionID = sessionID
		}
		if model := AsString(message["model"]); model != nil {
			active.model = model
		}
		if content, ok := message["content"].(string); ok {
			qoderAddTextChars(&active.chars, content)
		} else if blocks := GetSlice(message["content"]); blocks != nil {
			for _, part := range blocks {
				block := GetMap(part)
				if block == nil {
					continue
				}
				if IsStr(block["type"], "tool_use") {
					if name := AsString(block["name"]); name != nil {
						tally.AddEvent(*timestamp, TallyTool, name, SkillOfToolUse(block))
					}
				}
				qoderAddBlockChars(&active.chars, block)
			}
		}
		if usage := GetMap(message["usage"]); usage != nil {
			active.usage = usage
			active.timestamp = timestamp
			active.contextWindow = contextWindow
		}
		// Old transcripts without message.id cannot merge multi-line turns:
		// each line is its own group, closed immediately after processing.
		if !hasID {
			flushGroup()
		}
	})
	flushGroup()
	if len(records) > 0 {
		records[len(records)-1].MergeInterval(tally.Flush())
	}
	return records, nil
}

// qoderRequestCredits mirrors requestCredits: free requests
// (billable === false) count too.
func qoderRequestCredits(usage map[string]any) float64 {
	for _, key := range []string{"credits", "original_credits"} {
		if credits, ok := AsNumber(usage[key]); ok && credits > 0 {
			return credits
		}
	}
	return 0
}

func qoderAddBlockChars(counts *qoderCharCounts, block map[string]any) {
	switch {
	case IsStr(block["type"], "text"):
		if text, ok := block["text"].(string); ok {
			qoderAddTextChars(counts, text)
		}
	case IsStr(block["type"], "thinking"):
		if text, ok := block["thinking"].(string); ok {
			qoderAddTextChars(counts, text)
		}
	case IsStr(block["type"], "tool_use"):
		qoderAddValueChars(counts, block["input"])
	}
}

// qoderAddValueChars counts the string values of tool_use.input recursively
// (without serializing JSON: key order and escaping differ between TS and Go
// and would break parity).
func qoderAddValueChars(counts *qoderCharCounts, value any) {
	switch v := value.(type) {
	case string:
		qoderAddTextChars(counts, v)
	case []any:
		for _, item := range v {
			qoderAddValueChars(counts, item)
		}
	case map[string]any:
		for _, item := range v {
			qoderAddValueChars(counts, item)
		}
	}
}

func qoderAddTextChars(counts *qoderCharCounts, text string) {
	for _, r := range text {
		if qoderIsCjkRune(r) {
			counts.cjk++
		} else {
			counts.other++
		}
	}
}

// qoderIsCjkRune mirrors isCjkCodePoint: the ranges match segment by segment
// (a Go rune is a code point; the TS side iterates by codePoint).
func qoderIsCjkRune(r rune) bool {
	return (r >= 0x2e80 && r <= 0x2fdf) ||
		(r >= 0x3000 && r <= 0x303f) ||
		(r >= 0x3040 && r <= 0x30ff) ||
		(r >= 0x3400 && r <= 0x4dbf) ||
		(r >= 0x4e00 && r <= 0x9fff) ||
		(r >= 0xac00 && r <= 0xd7af) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xff00 && r <= 0xffef) ||
		(r >= 0x20000 && r <= 0x2fa1f)
}
