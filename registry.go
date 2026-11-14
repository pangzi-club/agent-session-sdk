package agentsession

import (
	"path/filepath"
	"sync"
)

// agentDef is one agent's store location, display name, session scanner and
// capability bits. canDelete is reported to consumers as canDeleteSessions;
// deleting sessions is the consumer's job, this library only reads.
type agentDef struct {
	id        string
	name      string
	directory string
	scan      func(string) ([]SessionInfo, error)
	usageScan func(string) (*usageModuleResult, error)
	canDelete bool
}

// agents is in display order, matching agent-view's AGENTS array. Agents that
// are only detected but never scanned (qwen) are not listed.
var agents = []agentDef{
	{id: "claude", name: "Claude Code", directory: ".claude", scan: ScanClaude, usageScan: scanClaudeUsage, canDelete: true},
	{id: "codex", name: "Codex", directory: ".codex", scan: ScanCodex, usageScan: scanCodexUsage, canDelete: true},
	{id: "gemini", name: "Gemini CLI", directory: ".gemini", scan: ScanGemini},
	{id: "cursor", name: "Cursor", directory: ".cursor", scan: ScanCursor},
	{id: "opencode", name: "OpenCode", directory: ".local/share/opencode", scan: ScanOpenCode, usageScan: scanOpenCodeUsage},
	{id: "zcode", name: "ZCode", directory: ".zcode", scan: ScanZcode, usageScan: scanZcodeUsage},
	{id: "cindy", name: "Cindy", directory: "Library/Application Support/CindyGlobal", scan: ScanCindy, usageScan: scanCindyUsage},
	{id: "workbuddy", name: "WorkBuddy", directory: ".workbuddy", scan: ScanWorkbuddy, usageScan: scanWorkbuddyUsage},
	{id: "pi", name: "Pi", directory: ".pi", scan: ScanPi, usageScan: scanPiUsage},
	{id: "dsh", name: "Deepseek Harness", directory: ".dsh", scan: ScanDsh, usageScan: scanDshUsage},
	{id: "grok", name: "Grok Build", directory: ".grok", scan: ScanGrok, usageScan: scanGrokUsage},
	{id: "qoder", name: "Qoder", directory: ".qoder", scan: ScanQoder, usageScan: scanQoderUsage},
	{id: "qoder-cn", name: "Qoder CN", directory: ".qoder-cn", scan: ScanQoder, usageScan: scanQoderCnUsage},
	{id: "kimi", name: "Kimi", directory: ".kimi", scan: ScanKimi},
	{id: "kimi-code", name: "Kimi Code", directory: ".kimi-code", scan: ScanKimiCode},
	{id: "hermes", name: "Hermes", directory: ".hermes", scan: ScanHermes, usageScan: scanHermesUsage},
}

// KnownAgentIDs returns every registered agent id in registry order.
func KnownAgentIDs() []string {
	ids := make([]string, 0, len(agents))
	for _, def := range agents {
		ids = append(ids, def.id)
	}
	return ids
}

// UsageAgentIDs returns agents with usage extraction in aggregation order.
func UsageAgentIDs() []string {
	ids := make([]string, 0, len(usageModules))
	for _, def := range usageModules {
		ids = append(ids, def.id)
	}
	return ids
}

// IsKnownAgent reports whether id is registered.
func IsKnownAgent(id string) bool {
	for _, def := range agents {
		if def.id == id {
			return true
		}
	}
	return false
}

// ScanSessions scans every registered agent under home in parallel, preserving
// registry order. A failing agent contributes no sessions instead of failing the
// whole scan.
func ScanSessions(home string) []SessionScanResult {
	results := make([]SessionScanResult, len(agents))
	var wg sync.WaitGroup
	for i := range agents {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			def := agents[i]
			result := SessionScanResult{
				AgentID:   def.id,
				AgentName: def.name,
				Sessions:  []SessionInfo{},
			}
			if sessions, err := def.scan(filepath.Join(home, def.directory)); err == nil && sessions != nil {
				result.Sessions = sessions
			}
			// Always emit the boolean: false means the agent cannot delete sessions.
			result.CanDeleteSessions = &def.canDelete
			results[i] = result
		}(i)
	}
	wg.Wait()
	return results
}
