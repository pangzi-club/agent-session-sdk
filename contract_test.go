package agentsession

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestPublicJSONContract(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		value any
		want  string
	}{
		{
			name: "sessions",
			value: []SessionScanResult{{AgentID: "codex", AgentName: "Codex", Sessions: []SessionInfo{{
				ID: "session", Workspace: "/tmp/project", CreatedAt: 1, UpdatedAt: 2, SizeBytes: 3,
				transcriptFormat: "codex",
			}}}},
			want: `[{"agentId":"codex","agentName":"Codex","sessions":[{"id":"session","workspace":"/tmp/project","title":null,"createdAt":1,"updatedAt":2,"sizeBytes":3}]}]`,
		},
		{
			name: "transcript",
			value: []Entry{
				{Role: RoleUser, Text: "hello", Timestamp: 1},
				{Role: RoleAssistant, Tools: []ToolCall{{Name: "Read", Title: "README.md", Meta: "Read", Kind: ToolFile, ID: "call", Failed: true}}, Timestamp: 2, DurationMs: 3},
			},
			want: `[{"role":"user","text":"hello","timestamp":1},{"role":"assistant","text":"","tools":[{"name":"Read","title":"README.md","meta":"Read","kind":"file","id":"call","failed":true}],"timestamp":2,"durationMs":3}]`,
		},
		{
			name: "usage",
			value: UsageScan{
				Records:  []UsageRecord{{SessionID: "session", AgentID: "zcode", InputTokens: -2, OutputTokens: 3}},
				Sessions: []UsageSession{{ID: "session", AgentID: "zcode", AgentName: "ZCode", Workspace: "/tmp/project"}},
			},
			want: `{"records":[{"sessionId":"session","agentId":"zcode","model":null,"timestamp":0,"inputTokens":-2,"outputTokens":3,"cacheReadTokens":0,"cacheWriteTokens":0,"userPrompts":0,"toolCalls":0,"toolCallsByName":null,"skillCallsByName":null,"activeMs":0}],"sessions":[{"id":"session","agentId":"zcode","agentName":"ZCode","workspace":"/tmp/project","title":null}]}`,
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			got, err := json.Marshal(fixture.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != fixture.want {
				t.Fatalf("JSON = %s\nwant   %s", got, fixture.want)
			}
		})
	}
}

func TestAgentOrderContract(t *testing.T) {
	wantSessions := []string{"claude", "codex", "gemini", "cursor", "opencode", "zcode", "cindy", "workbuddy", "pi", "dsh", "grok", "qoder", "qoder-cn", "kimi", "kimi-code", "hermes"}
	if got := KnownAgentIDs(); !slices.Equal(got, wantSessions) {
		t.Errorf("known agents = %v, want %v", got, wantSessions)
	}
	wantUsage := []string{"claude", "codex", "zcode", "workbuddy", "dsh", "pi", "cindy", "opencode", "grok", "qoder", "qoder-cn", "hermes"}
	if got := UsageAgentIDs(); !slices.Equal(got, wantUsage) {
		t.Errorf("usage agents = %v, want %v", got, wantUsage)
	}
	seenUsageOrders := map[int]string{}
	for _, def := range agents {
		if def.loadTranscript == nil {
			t.Errorf("agent %q has no transcript loader", def.id)
		}
		if def.usageScan == nil {
			if def.usageOrder != 0 {
				t.Errorf("agent %q has usage order %d without a usage scanner", def.id, def.usageOrder)
			}
			continue
		}
		if def.usageOrder < 1 || def.usageOrder > len(wantUsage) {
			t.Errorf("agent %q has invalid usage order %d", def.id, def.usageOrder)
			continue
		}
		if previous, exists := seenUsageOrders[def.usageOrder]; exists {
			t.Errorf("agents %q and %q share usage order %d", previous, def.id, def.usageOrder)
		}
		seenUsageOrders[def.usageOrder] = def.id
	}
}
