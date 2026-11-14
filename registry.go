package agentsession

import (
	"errors"
	"fmt"
	"os"
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

// AgentScanFailure identifies an agent whose store could not be scanned.
type AgentScanFailure struct {
	AgentID   string
	AgentName string
	Directory string
	Err       error
}

func (failure AgentScanFailure) Error() string {
	return fmt.Sprintf("%s (%s): %v", failure.AgentID, failure.Directory, failure.Err)
}

func (failure AgentScanFailure) Unwrap() error { return failure.Err }

// SessionScanReport retains partial success while making failed agents visible.
type SessionScanReport struct {
	Results  []SessionScanResult
	Failures []AgentScanFailure
}

// ScanSessionsDetailed scans every registered agent under home in parallel.
// A failure removes only that agent's sessions and is reported to the caller.
func ScanSessionsDetailed(home string) SessionScanReport {
	results := make([]SessionScanResult, len(agents))
	failures := make([][]AgentScanFailure, len(agents))
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
			directory := filepath.Join(home, def.directory)
			sessions, err := def.scan(directory)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				failures[i] = append(failures[i], AgentScanFailure{AgentID: def.id, AgentName: def.name, Directory: directory, Err: err})
			} else if sessions != nil {
				result.Sessions = sessions
			}
			result.CanDeleteSessions = &def.canDelete
			results[i] = result
		}(i)
	}
	wg.Wait()
	return SessionScanReport{Results: results, Failures: joinAgentFailures(failures)}
}

func joinAgentFailures(groups [][]AgentScanFailure) []AgentScanFailure {
	out := []AgentScanFailure{}
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}

// ScanSessions preserves the original empty-on-error convenience API. Use
// ScanSessionsDetailed when an empty store must be distinguished from a failure.
func ScanSessions(home string) []SessionScanResult {
	return ScanSessionsDetailed(home).Results
}
