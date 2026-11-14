// Package agentsession reads local coding-agent session stores: session lists,
// usage statistics and transcripts.
//
// Session and usage output structs are aligned field for field with agent-view's
// src/ipc-contract.ts, so a consumer can swap its scan engine without changing
// its TypeScript types. Conventions:
//
//   - field order follows the TS declaration order, giving a stable JSON key
//     order that is easy to diff against cached payloads;
//   - numbers are float64: a JS number is a float64, so arithmetic and JSON
//     serialization agree bit for bit (including zcode's negative input usage);
//   - an omitted key and an explicit null stay distinct: filePath / tokenUsage /
//     blank / canDeleteSessions / createdAt (usage sessions) are omitted when
//     unset (omitempty), while title / model are explicit null (pointer, no
//     omitempty).
//
// Pricing is a separate, optional layer: SessionInfo carries no costUsd or
// costUnpricedTokens, keeping the base output identical to the existing scanner.
package agentsession

// SessionInfo aligns with SessionInfo in ipc-contract.ts.
type SessionInfo struct {
	transcriptFormat string

	ID         string      `json:"id"`
	Workspace  string      `json:"workspace"`
	Title      *string     `json:"title"`
	Blank      *bool       `json:"blank,omitempty"`
	CreatedAt  float64     `json:"createdAt"`
	UpdatedAt  float64     `json:"updatedAt"`
	SizeBytes  float64     `json:"sizeBytes"`
	FilePath   *string     `json:"filePath,omitempty"`
	TokenUsage *[5]float64 `json:"tokenUsage,omitempty"`
}

// SessionScanResult aligns with SessionScanResult in ipc-contract.ts.
type SessionScanResult struct {
	AgentID           string        `json:"agentId"`
	AgentName         string        `json:"agentName"`
	Sessions          []SessionInfo `json:"sessions"`
	CanDeleteSessions *bool         `json:"canDeleteSessions,omitempty"`
}

// UsageRecord aligns with UsageRecord in agent-view's usage/types.ts. All fields
// are emitted; credits is omitted when the agent has no credit accounting.
type UsageRecord struct {
	SessionID        string              `json:"sessionId"`
	AgentID          string              `json:"agentId"`
	Model            *string             `json:"model"`
	Timestamp        float64             `json:"timestamp"`
	InputTokens      float64             `json:"inputTokens"`
	OutputTokens     float64             `json:"outputTokens"`
	CacheReadTokens  float64             `json:"cacheReadTokens"`
	CacheWriteTokens float64             `json:"cacheWriteTokens"`
	UserPrompts      float64             `json:"userPrompts"`
	ToolCalls        float64             `json:"toolCalls"`
	ToolCallsByName  *map[string]float64 `json:"toolCallsByName"`
	SkillCallsByName *map[string]float64 `json:"skillCallsByName"`
	ActiveMs         float64             `json:"activeMs"`
	Credits          *float64            `json:"credits,omitempty"`
}

// UsageSession aligns with the sessions array of agent-view's usage cache v5
// payload, where the map key is inlined as the first field.
type UsageSession struct {
	ID         string      `json:"id"`
	AgentID    string      `json:"agentId"`
	AgentName  string      `json:"agentName"`
	Workspace  string      `json:"workspace"`
	Title      *string     `json:"title"`
	CreatedAt  *float64    `json:"createdAt,omitempty"`
	FilePath   *string     `json:"filePath,omitempty"`
	TokenUsage *[5]float64 `json:"tokenUsage,omitempty"`
}

// UsageScan is the usage output shape: records plus the sessions they belong to.
type UsageScan struct {
	Records  []UsageRecord  `json:"records"`
	Sessions []UsageSession `json:"sessions"`
}

// AgentUsageSummary is per-agent usage. Sessions counts sessions without records
// too; token accounting matches UsageRecord, so input excludes cache traffic.
type AgentUsageSummary struct {
	AgentID          string  `json:"agentId"`
	Sessions         int     `json:"sessions"`
	Requests         int     `json:"requests"`
	InputTokens      float64 `json:"inputTokens"`
	OutputTokens     float64 `json:"outputTokens"`
	CacheReadTokens  float64 `json:"cacheReadTokens"`
	CacheWriteTokens float64 `json:"cacheWriteTokens"`
	ActiveMs         float64 `json:"activeMs"`
}

// Role is who produced a transcript entry.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// ToolKind groups tool calls so a consumer can pick an icon without knowing
// every agent's tool names.
type ToolKind string

const (
	ToolFile    ToolKind = "file"
	ToolCommand ToolKind = "command"
	ToolSearch  ToolKind = "search"
	ToolOther   ToolKind = "other"
)

// ToolCall is one tool invocation. Title is the single line shown on the card
// (a path, a command, a query); Meta is the tool name plus an optional hint.
// ID links the call to its tool_result record where the store provides one;
// Input and Output carry the bounded raw arguments and result for consumers
// that show them; Failed marks a result that reported an error.
type ToolCall struct {
	Name   string   `json:"name"`
	Title  string   `json:"title"`
	Meta   string   `json:"meta"`
	Kind   ToolKind `json:"kind"`
	ID     string   `json:"id,omitempty"`
	Input  string   `json:"input,omitempty"`
	Output string   `json:"output,omitempty"`
	Failed bool     `json:"failed,omitempty"`
}

// Entry is one visible item of a transcript: either text, thinking or tool
// calls, never more than one. Thinking is the bounded reasoning text of an
// assistant record; consumers that treat reasoning as noise can ignore it.
// Text and tools parsed from the same assistant record share a timestamp
// (epoch ms, 0 when the store records none). DurationMs is the entry's active
// span: a reasoning block's thinking time, a tool call's run time, or a text
// record's generation time (completed - created); 0 when the store keeps no
// timing.
type Entry struct {
	Role       Role       `json:"role"`
	Text       string     `json:"text"`
	Thinking   string     `json:"thinking,omitempty"`
	Tools      []ToolCall `json:"tools,omitempty"`
	Timestamp  float64    `json:"timestamp"`
	DurationMs float64    `json:"durationMs,omitempty"`
}
