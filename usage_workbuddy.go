package agentsession

// usage_workbuddy.go mirrors src/main/usage/workbuddy.ts:
// in projects/<encoded cwd>/<uuid>.jsonl, assistant message lines carry that
// turn's record point in providerData.usage / rawUsage (prompt_tokens includes
// cache hits and is normalized by subtraction → excludes cache;
// cache_creation_input_tokens / prompt_cache_write_tokens are cache writes;
// when only usage is present, cached tokens come from
// inputTokensDetails[0].cached_tokens). Lines with role=user that
// providerData does not flag as injected / compaction-internal are prompts;
// `function_call` lines are tool calls. The session list reuses ScanWorkbuddy
// (metadata and filePath come from the same store).

import (
	"math"
	"strings"
)

func scanWorkbuddyUsage(agentDir string) (*usageModuleResult, error) {
	sessions, err := ScanWorkbuddy(agentDir)
	if err != nil {
		return nil, err
	}
	results, err := MapLimit(sessions, FileConcurrency, scanWorkbuddySessionUsage)
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

type workbuddySessionUsage struct {
	session SessionInfo
	records []UsageRecord
}

func scanWorkbuddySessionUsage(session SessionInfo) (*workbuddySessionUsage, error) {
	if session.FilePath == nil {
		return &workbuddySessionUsage{session: session, records: []UsageRecord{}}, nil
	}
	tally := &SessionTally{}
	records := []UsageRecord{}
	if err := WalkJSONLFile(*session.FilePath, func(obj map[string]any) {
		timestamp := 0.0
		if f, ok := AsNumber(obj["timestamp"]); ok {
			timestamp = math.Floor(f)
		}
		if timestamp == 0 {
			return
		}

		if IsStr(obj["type"], "function_call") {
			tally.AddEvent(timestamp, TallyTool, AsString(obj["name"]))
			return
		}
		if !IsStr(obj["type"], "message") {
			return
		}

		if IsStr(obj["role"], "user") {
			if isWorkbuddyRealUserPrompt(obj) {
				tally.AddEvent(timestamp, TallyPrompt, nil)
			}
			return
		}
		if !IsStr(obj["role"], "assistant") {
			return
		}

		tally.AddEvent(timestamp, TallyAssistant, nil)
		providerData := GetMap(obj["providerData"])
		if providerData == nil {
			return
		}
		usage, ok := readWorkbuddyUsage(providerData)
		if !ok {
			return
		}
		record := UsageRecord{
			SessionID:        session.ID,
			AgentID:          "workbuddy",
			Model:            readWorkbuddyModel(providerData),
			Timestamp:        timestamp,
			InputTokens:      usage.input,
			OutputTokens:     usage.output,
			CacheReadTokens:  usage.cacheRead,
			CacheWriteTokens: usage.cacheWrite,
		}
		record.ApplyInterval(tally.TakeUntil(timestamp))
		records = append(records, record)
	}); err != nil {
		return nil, err
	}
	if len(records) > 0 {
		records[len(records)-1].MergeInterval(tally.Flush())
	}
	return &workbuddySessionUsage{session: session, records: records}, nil
}

// isWorkbuddyRealUserPrompt mirrors isRealUserPrompt (isRealUserMessage in
// workbuddy-core): lines flagged by providerData with skipRun / isMeta /
// isCompactInternal / compactType are injected or compaction-internal
// messages.
func isWorkbuddyRealUserPrompt(obj map[string]any) bool {
	if providerData := GetMap(obj["providerData"]); providerData != nil {
		if AsBoolTrue(providerData["skipRun"]) ||
			AsBoolTrue(providerData["isMeta"]) ||
			AsBoolTrue(providerData["isCompactInternal"]) {
			return false
		}
		if _, ok := providerData["compactType"].(string); ok {
			return false
		}
	}
	text := ContentToText(obj["content"])
	return text != nil && strings.TrimSpace(*text) != ""
}

type workbuddyTokens struct {
	input, output, cacheRead, cacheWrite float64
}

// readWorkbuddyUsage mirrors readUsage: returns ok=false when neither rawUsage
// nor usage exists (no record).
func readWorkbuddyUsage(providerData map[string]any) (workbuddyTokens, bool) {
	raw := GetMap(providerData["rawUsage"])
	usage := GetMap(providerData["usage"])
	if raw == nil && usage == nil {
		return workbuddyTokens{}, false
	}
	prompt := AsTokenCount(raw["prompt_tokens"])
	if prompt == 0 {
		prompt = AsTokenCount(usage["inputTokens"])
	}
	output := AsTokenCount(raw["completion_tokens"])
	if output == 0 {
		output = AsTokenCount(usage["outputTokens"])
	}
	// Cache-read-first accounting (prompt_tokens includes it), consistent with
	// inputTokensDetails.
	cacheRead := AsTokenCount(raw["prompt_cache_hit_tokens"])
	if cacheRead == 0 {
		cacheRead = AsTokenCount(raw["cache_read_input_tokens"])
	}
	if cacheRead == 0 {
		cacheRead = AsTokenCount(cachedTokensDetail(usage))
	}
	cacheWrite := AsTokenCount(raw["cache_creation_input_tokens"])
	if cacheWrite == 0 {
		cacheWrite = AsTokenCount(raw["prompt_cache_write_tokens"])
	}
	return workbuddyTokens{
		// The prompt includes cache hits; subtracting them yields the
		// "excludes cache traffic" figure (same as codex / zcode, no clamping).
		input:      prompt - cacheRead,
		output:     output,
		cacheRead:  cacheRead,
		cacheWrite: cacheWrite,
	}, true
}

func readWorkbuddyModel(providerData map[string]any) *string {
	if model := AsString(providerData["model"]); model != nil && *model != "" {
		return model
	}
	if model := AsString(providerData["requestModelId"]); model != nil && *model != "" {
		return model
	}
	return nil
}

// cachedTokensDetail reads usage.inputTokensDetails[0].cached_tokens (the
// fallback used only when cache hits are reported in usage).
func cachedTokensDetail(usage map[string]any) any {
	details := GetSlice(usage["inputTokensDetails"])
	if len(details) == 0 {
		return nil
	}
	return GetMap(details[0])["cached_tokens"]
}
