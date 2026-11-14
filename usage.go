// The usage scan layer: one module per agent plus the ScanAllUsage
// aggregation. Aggregation semantics (token accumulation keyed by bare
// sessionId, the global status 3/2 flag, tildify, last-write-wins on the
// session map) mirror agent-view's usage/scan.ts step by step.
package agentsession

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

type usageModuleResult struct {
	sessions []SessionInfo
	records  []UsageRecord
}

// usageModules mirrors the USAGE_MODULES order in usage/scan.ts. Agent identity,
// store paths and capabilities live in registry.go.
var usageModules = func() []agentDef {
	modules := []agentDef{}
	for _, id := range []string{"claude", "codex", "zcode", "workbuddy", "dsh", "pi", "cindy", "opencode", "grok", "qoder", "qoder-cn", "hermes"} {
		for _, def := range agents {
			if def.id == id {
				modules = append(modules, def)
				break
			}
		}
	}
	return modules
}()

// ScanAllUsage mirrors scanAllUsage in usage/scan.ts: each module is fault
// isolated (a failure counts as empty — that agent contributes no sessions or
// records without affecting the others), records concatenate in module order,
// and sessions merge into the ordered map keyed by bare sessionId with
// last-write-wins. Status is per agent: every emitted session comes from a
// successfully scanned module, so it is always 3.
// UsageScanReport includes per-agent failures alongside successfully read data.
type UsageScanReport struct {
	Scan     UsageScan
	Failures []AgentScanFailure
}

// ScanAllUsageDetailed retains successful modules and reports broken stores.
func ScanAllUsageDetailed(home string) UsageScanReport {
	results := make([]*usageModuleResult, len(usageModules))
	failures := make([][]AgentScanFailure, len(usageModules))
	var wg sync.WaitGroup
	for i := range usageModules {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			module := usageModules[i]
			directory := filepath.Join(home, module.directory)
			result, err := module.usageScan(directory)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				failures[i] = append(failures[i], AgentScanFailure{AgentID: module.id, AgentName: module.name, Directory: directory, Err: err})
			} else {
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
	return UsageScanReport{Scan: UsageScan{Records: records, Sessions: sessions.Values()}, Failures: joinAgentFailures(failures)}
}

// ScanAllUsage preserves the original empty-on-error convenience API. Use
// ScanAllUsageDetailed for diagnostics and non-empty-on-failure CLI behavior.
func ScanAllUsage(home string) UsageScan {
	return ScanAllUsageDetailed(home).Scan
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
