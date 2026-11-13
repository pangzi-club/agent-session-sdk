// The usage scan layer: one module per agent plus the ScanAllUsage
// aggregation. Aggregation semantics (token accumulation keyed by bare
// sessionId, the global status 3/2 flag, tildify, last-write-wins on the
// session map) mirror agent-view's usage/scan.ts step by step.
package agentsession

import (
	"path/filepath"
	"sync"
)

type usageModuleResult struct {
	sessions []SessionInfo
	records  []UsageRecord
}

type usageModule struct {
	id        string
	name      string
	directory string
	scan      func(string) (*usageModuleResult, error)
}

// usageModules mirrors the USAGE_MODULES order in usage/scan.ts.
var usageModules = []usageModule{
	{id: "claude", name: "Claude Code", directory: ".claude", scan: scanClaudeUsage},
	{id: "codex", name: "Codex", directory: ".codex", scan: scanCodexUsage},
	{id: "zcode", name: "ZCode", directory: ".zcode", scan: scanZcodeUsage},
	{id: "workbuddy", name: "WorkBuddy", directory: ".workbuddy", scan: scanWorkbuddyUsage},
	{id: "dsh", name: "Deepseek Harness", directory: ".dsh", scan: scanDshUsage},
	{id: "pi", name: "Pi", directory: ".pi", scan: scanPiUsage},
	{id: "cindy", name: "Cindy", directory: "Library/Application Support/CindyGlobal", scan: scanCindyUsage},
	{id: "opencode", name: "OpenCode", directory: ".local/share/opencode", scan: scanOpenCodeUsage},
	{id: "grok", name: "Grok Build", directory: ".grok", scan: scanGrokUsage},
	{id: "qoder", name: "Qoder", directory: ".qoder", scan: scanQoderUsage},
	{id: "qoder-cn", name: "Qoder CN", directory: ".qoder-cn", scan: scanQoderCnUsage},
	{id: "hermes", name: "Hermes", directory: ".hermes", scan: scanHermesUsage},
}

// ScanAllUsage mirrors scanAllUsage in usage/scan.ts: each module is fault
// isolated (a failure counts as empty — that agent contributes no sessions or
// records without affecting the others), records concatenate in module order,
// and sessions merge into the ordered map keyed by bare sessionId with
// last-write-wins. Status is per agent: every emitted session comes from a
// successfully scanned module, so it is always 3.
func ScanAllUsage(home string) UsageScan {
	results := make([]*usageModuleResult, len(usageModules))
	var wg sync.WaitGroup
	for i := range usageModules {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			module := usageModules[i]
			if result, err := module.scan(filepath.Join(home, module.directory)); err == nil {
				results[i] = result
			}
		}(i)
	}
	wg.Wait()

	records := []UsageRecord{}
	type tokenAgg struct{ input, output, cacheRead, cacheWrite float64 }
	aggs := map[string]*tokenAgg{}
	sessions := NewOrderedMap[UsageSession]()
	for i, result := range results {
		if result == nil {
			continue
		}
		def := usageModules[i]
		records = append(records, result.records...)
		for i := range result.records {
			record := &result.records[i]
			agg := aggs[record.SessionID]
			if agg == nil {
				agg = &tokenAgg{}
				aggs[record.SessionID] = agg
			}
			agg.input += record.InputTokens
			agg.output += record.OutputTokens
			agg.cacheRead += record.CacheReadTokens
			agg.cacheWrite += record.CacheWriteTokens
		}
		for _, session := range result.sessions {
			usage := UsageSession{
				ID:        session.ID,
				AgentID:   def.id,
				AgentName: def.name,
				Workspace: session.Workspace,
				Title:     session.Title,
				CreatedAt: &session.CreatedAt,
			}
			if session.FilePath != nil {
				tildified := Tildify(*session.FilePath, home)
				usage.FilePath = &tildified
			}
			if agg := aggs[session.ID]; agg != nil {
				usage.TokenUsage = &[5]float64{
					agg.input, agg.output, agg.cacheRead, agg.cacheWrite, 3,
				}
			}
			sessions.Set(session.ID, usage)
		}
	}
	return UsageScan{Records: records, Sessions: sessions.Values()}
}

// ApplyInterval expands the TS spread `...tally.takeUntil(ts)` onto the record
// fields.
func (r *UsageRecord) ApplyInterval(interval TallyInterval) {
	r.UserPrompts = interval.UserPrompts
	r.ToolCalls = interval.ToolCalls
	if interval.ToolCallsByName != nil {
		r.ToolCallsByName = &interval.ToolCallsByName
	} else {
		r.ToolCallsByName = nil
	}
	if interval.SkillCallsByName != nil {
		r.SkillCallsByName = &interval.SkillCallsByName
	} else {
		r.SkillCallsByName = nil
	}
	r.ActiveMs = interval.ActiveMs
}

// MergeInterval mirrors mergeInterval in usage/shared.ts: folds a tail
// increment into the last record of a session.
func (r *UsageRecord) MergeInterval(tail TallyInterval) {
	r.UserPrompts += tail.UserPrompts
	r.ToolCalls += tail.ToolCalls
	if tail.ToolCallsByName != nil {
		if r.ToolCallsByName != nil {
			for tool, count := range tail.ToolCallsByName {
				(*r.ToolCallsByName)[tool] += count
			}
		} else {
			r.ToolCallsByName = &tail.ToolCallsByName
		}
	}
	if tail.SkillCallsByName != nil {
		if r.SkillCallsByName != nil {
			for skill, count := range tail.SkillCallsByName {
				(*r.SkillCallsByName)[skill] += count
			}
		} else {
			r.SkillCallsByName = &tail.SkillCallsByName
		}
	}
	r.ActiveMs += tail.ActiveMs
}
