package main

import (
	"bytes"
	"strings"
	"testing"

	agentsession "github.com/pangzi-club/agent-session-sdk"
)

func TestWriteHumanSessions(t *testing.T) {
	title := "Investigate login bug"
	var out bytes.Buffer
	err := writeHuman(&out, "sessions", []agentsession.SessionScanResult{{
		AgentName: "Codex",
		Sessions: []agentsession.SessionInfo{{
			ID: "session-123", Title: &title, Workspace: "/work/app", UpdatedAt: 1795000000000,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Codex (1 sessions)", "Investigate login bug", "session-123", "/work/app"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("human output %q does not contain %q", out.String(), want)
		}
	}
	if strings.HasPrefix(strings.TrimSpace(out.String()), "[") {
		t.Fatalf("human output looks like JSON: %s", out.String())
	}
}

func TestWriteHumanTranscript(t *testing.T) {
	var out bytes.Buffer
	err := writeHumanTranscript(&out, []agentsession.Entry{{
		Role:  agentsession.RoleAssistant,
		Text:  "I found the cause.",
		Tools: []agentsession.ToolCall{{Name: "shell", Title: "go test ./...", Failed: true}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ASSISTANT", "I found the cause.", "Tool: shell (failed)", "go test ./..."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("human output %q does not contain %q", out.String(), want)
		}
	}
}

func TestWriteHumanUsageShowsCost(t *testing.T) {
	model := "gpt-test"
	title := "Usage session"
	amount := 0.0123
	var out bytes.Buffer
	err := writeHumanUsage(&out, costScan{
		Records: []costRecord{{
			UsageRecord: agentsession.UsageRecord{
				AgentID: "codex", SessionID: "session-1", Model: &model,
				InputTokens: 123, OutputTokens: 45, Timestamp: 1795000000000,
			},
			CostUsd: &amount,
		}},
		Sessions: []agentsession.UsageSession{{AgentID: "codex", ID: "session-1", Title: &title}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"AGENT", "codex", "gpt-test", "Usage session", "$0.0123", "123", "45"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("human output %q does not contain %q", out.String(), want)
		}
	}
}
