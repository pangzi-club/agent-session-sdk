package agentsession

// agentDef is one agent's store location, display name and capability bits.
// canDelete is reported to consumers as canDeleteSessions; deleting sessions is
// the consumer's job, this library only reads.
type agentDef struct {
	id        string
	name      string
	directory string
	canDelete bool
}

// agents is in display order, matching agent-view's AGENTS array. Agents that
// are only detected but never scanned (qwen) are not listed.
var agents = []agentDef{
	{id: "claude", name: "Claude Code", directory: ".claude", canDelete: true},
	{id: "codex", name: "Codex", directory: ".codex", canDelete: true},
	{id: "gemini", name: "Gemini CLI", directory: ".gemini"},
	{id: "cursor", name: "Cursor", directory: ".cursor"},
	{id: "opencode", name: "OpenCode", directory: ".local/share/opencode"},
	{id: "zcode", name: "ZCode", directory: ".zcode"},
	{id: "workbuddy", name: "WorkBuddy", directory: ".workbuddy"},
	{id: "pi", name: "Pi", directory: ".pi"},
	{id: "dsh", name: "Deepseek Harness", directory: ".dsh"},
	{id: "grok", name: "Grok Build", directory: ".grok"},
	{id: "qoder", name: "Qoder", directory: ".qoder"},
	{id: "qoder-cn", name: "Qoder CN", directory: ".qoder-cn"},
	{id: "kimi", name: "Kimi", directory: ".kimi"},
	{id: "hermes", name: "Hermes", directory: ".hermes"},
}

// KnownAgentIDs returns every registered agent id in registry order.
func KnownAgentIDs() []string {
	ids := make([]string, 0, len(agents))
	for _, def := range agents {
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
