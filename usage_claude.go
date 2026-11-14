package agentsession

// claude.go mirrors src/main/usage/claude.ts:
// assistant lines carry per-turn tokens in message.usage (input excludes cache); user lines = prompts,
// tool_use blocks in content = tool calls; isSidechain subagent lines are skipped entirely.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

func scanClaudeUsage(agentDir string) (*usageModuleResult, error) {
	sessions, err := ScanClaude(agentDir)
	if err != nil {
		return nil, err
	}
	projectDirs, err := ListDirsE(filepath.Join(agentDir, "projects"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var records []UsageRecord
	for _, projectDir := range projectDirs {
		projectRecords, err := scanClaudeProjectUsage(projectDir)
		if err != nil {
			return nil, err
		}
		records = append(records, projectRecords...)
	}
	return &usageModuleResult{sessions: sessions, records: records}, nil
}

func scanClaudeProjectUsage(projectDir string) ([]UsageRecord, error) {
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return nil, err
	}
	var out []UsageRecord
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".jsonl") {
			records, err := scanClaudeFileUsage(filepath.Join(projectDir, entry.Name()))
			if err != nil {
				return nil, err
			}
			out = append(out, records...)
		}
	}
	return out, nil
}

func scanClaudeFileUsage(file string) ([]UsageRecord, error) {
	return scanClaudeUsageRecords(file, "", "claude")
}

// scanClaudeSessionUsage lets embedded Claude stores replace the native session
// identity without changing token semantics.
func scanClaudeSessionUsage(session SessionInfo, agentID string) ([]UsageRecord, error) {
	return scanClaudeUsageRecords(*session.FilePath, session.ID, agentID)
}

func scanClaudeUsageRecords(file, sessionID, agentID string) ([]UsageRecord, error) {
	lines, err := ReadAllLinesE(file)
	if err != nil {
		return nil, err
	}
	tally := &SessionTally{}
	records := []UsageRecord{}
	WalkLines(lines, func(obj map[string]any) {
		// isSidechain = subagent (Task tool) request; not counted toward main-session usage
		if AsBoolTrue(obj["isSidechain"]) {
			return
		}
		timestamp := AsTimestamp(obj["timestamp"])
		if timestamp == nil {
			return
		}
		message := GetMap(obj["message"])

		if IsStr(obj["type"], "user") {
			if claudeIsUserPrompt(message) {
				tally.AddEvent(*timestamp, TallyPrompt, nil)
			}
			return
		}
		if !IsStr(obj["type"], "assistant") {
			return
		}

		tally.AddEvent(*timestamp, TallyAssistant, nil)
		if blocks := GetSlice(message["content"]); blocks != nil {
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
			}
		}

		usage := GetMap(message["usage"])
		if usage == nil {
			return
		}
		session := sessionID
		if session == "" {
			if id := AsString(obj["sessionId"]); id != nil {
				session = *id
			}
		}
		if session == "" {
			return
		}
		record := UsageRecord{
			SessionID:        session,
			AgentID:          agentID,
			Model:            AsString(message["model"]),
			Timestamp:        *timestamp,
			InputTokens:      AsTokenCount(usage["input_tokens"]),
			OutputTokens:     AsTokenCount(usage["output_tokens"]),
			CacheReadTokens:  AsTokenCount(usage["cache_read_input_tokens"]),
			CacheWriteTokens: AsTokenCount(usage["cache_creation_input_tokens"]),
		}
		record.ApplyInterval(tally.TakeUntil(*timestamp))
		records = append(records, record)
	})
	if len(records) > 0 {
		records[len(records)-1].MergeInterval(tally.Flush())
	}
	return records, nil
}

// claudeIsUserPrompt mirrors isUserPrompt from usage/claude.ts:
// content is a string or contains a text block, and is not an injection / Caveat tool-result line starting with <.
func claudeIsUserPrompt(message map[string]any) bool {
	var content any
	if message != nil {
		content = message["content"]
	}
	var text *string
	if s, ok := content.(string); ok {
		text = &s
	} else if blocks, ok := content.([]any); ok {
		for _, part := range blocks {
			block := GetMap(part)
			if block != nil && IsStr(block["type"], "text") {
				if t, ok := block["text"].(string); ok {
					text = &t
					break
				}
			}
		}
	}
	if text == nil {
		return false // non-prompt lines such as tool_result / attachment
	}
	trimmed := strings.TrimLeftFunc(*text, unicode.IsSpace)
	return len(trimmed) > 0 && !strings.HasPrefix(trimmed, "<") && !strings.HasPrefix(trimmed, "Caveat:")
}
