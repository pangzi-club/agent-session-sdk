package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunUsageReportsPartialFailureWithoutDroppingJSON(t *testing.T) {
	home := t.TempDir()
	writeCLICodexFixture(t, home)
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "projects"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	exitCode, stdout, stderr := captureRun(t, []string{"usage", "--home", home})
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", exitCode, stderr)
	}
	if !strings.Contains(stderr, "agent scan failed: claude") {
		t.Fatalf("stderr = %q, want the Claude failure", stderr)
	}
	var payload struct {
		Records []struct {
			AgentID   string `json:"agentId"`
			SessionID string `json:"sessionId"`
		} `json:"records"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if len(payload.Records) != 1 || payload.Records[0].AgentID != "codex" ||
		payload.Records[0].SessionID != "rollout-cli-error-report" {
		t.Fatalf("records = %+v, want the successful Codex record", payload.Records)
	}
}

func TestRunTranscriptReportsScanFailureInsteadOfNotFound(t *testing.T) {
	home := t.TempDir()
	writeCLICodexFixture(t, home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "projects"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	exitCode, _, stderr := captureRun(t, []string{"transcript", "--home", home, "--agent", "claude", "--session", "any"})
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", exitCode, stderr)
	}
	if !strings.Contains(stderr, "agent scan failed: claude") {
		t.Fatalf("stderr = %q, want the Claude scan failure", stderr)
	}
	if strings.Contains(stderr, "not found") {
		t.Fatalf("stderr = %q, a failed scan must not be reported as not-found", stderr)
	}
}

func TestRunTranscriptUnknownSessionStillSaysNotFound(t *testing.T) {
	home := t.TempDir()
	writeCLICodexFixture(t, home)

	exitCode, _, stderr := captureRun(t, []string{"transcript", "--home", home, "--agent", "codex", "--session", "missing"})
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", exitCode, stderr)
	}
	if !strings.Contains(stderr, `"missing" not found`) {
		t.Fatalf("stderr = %q, want the not-found message", stderr)
	}
}

func TestRunSessionsPrintsJSONPerAgent(t *testing.T) {
	home := t.TempDir()
	writeCLICodexFixture(t, home)

	exitCode, stdout, stderr := captureRun(t, []string{"sessions", "--home", home})
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", exitCode, stderr)
	}
	var results []struct {
		AgentID  string `json:"agentId"`
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
		CanDelete *bool `json:"canDeleteSessions"`
	}
	if err := json.Unmarshal([]byte(stdout), &results); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	var codexSeen bool
	for _, result := range results {
		if result.CanDelete == nil {
			t.Fatalf("agent %s omitted canDeleteSessions", result.AgentID)
		}
		if result.AgentID == "codex" {
			codexSeen = true
			if len(result.Sessions) != 1 || result.Sessions[0].ID != "rollout-cli-error-report" {
				t.Fatalf("codex sessions = %+v", result.Sessions)
			}
		}
	}
	if !codexSeen {
		t.Fatal("codex missing from session results")
	}
}

func TestRunSearchUsesSessionIndex(t *testing.T) {
	home := t.TempDir()
	writeCLICodexFixture(t, home)

	exitCode, stdout, stderr := captureRun(t, []string{"search", "--home", home, "rollout", "--limit", "5"})
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", exitCode, stderr)
	}
	var results []struct {
		AgentID  string `json:"agentId"`
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(stdout), &results); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	found := false
	for _, result := range results {
		for _, session := range result.Sessions {
			if session.ID == "rollout-cli-error-report" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("search results missed the indexed session: %s", stdout)
	}
}

func TestRunUsageCostAddsCostFields(t *testing.T) {
	home := t.TempDir()
	writeCLICodexFixture(t, home)

	exitCode, stdout, stderr := captureRun(t, []string{"usage", "--home", home, "--cost"})
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", exitCode, stderr)
	}
	var payload struct {
		Records []struct {
			CostUsd   *float64 `json:"costUsd"`
			AgentID   string   `json:"agentId"`
			SessionID string   `json:"sessionId"`
		} `json:"records"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if len(payload.Records) != 1 || payload.Records[0].CostUsd == nil {
		t.Fatalf("cost records = %+v, want one record with costUsd", payload.Records)
	}
}

func TestRunHelpAndFlagErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		exitCode int
		stderr   string
	}{
		{name: "help", args: []string{"help"}, exitCode: 0},
		{name: "no args", args: nil, exitCode: 2},
		{name: "unknown subcommand", args: []string{"frobnicate"}, exitCode: 2, stderr: "unknown subcommand"},
		{name: "unknown agent", args: []string{"sessions", "--agent", "nope"}, exitCode: 2, stderr: "unknown agent: nope"},
		{name: "missing search query", args: []string{"search"}, exitCode: 2, stderr: "search needs <query>"},
		{name: "bad flag", args: []string{"sessions", "--definitely-not-a-flag"}, exitCode: 2},
		{name: "negative limit", args: []string{"search", "--limit", "-1", "q"}, exitCode: 2, stderr: "cannot be negative"},
		{name: "index on usage", args: []string{"usage", "--index"}, exitCode: 2, stderr: "--index is valid for sessions and transcript only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exitCode, _, stderr := captureRun(t, tc.args)
			if exitCode != tc.exitCode {
				t.Fatalf("exit code = %d, want %d; stderr: %s", exitCode, tc.exitCode, stderr)
			}
			if tc.stderr != "" && !strings.Contains(stderr, tc.stderr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, tc.stderr)
			}
		})
	}
}

func captureRun(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	stdout, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		stdout.Close()
		t.Fatal(err)
	}
	exitCode := run(args, stdout, stderr)
	if err := stdout.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stderr.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	errOut, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	return exitCode, string(out), string(errOut)
}

func writeCLICodexFixture(t *testing.T, home string) {
	t.Helper()
	file := filepath.Join(home, ".codex", "sessions", "2026", "11", "14", "rollout-cli-error-report.jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	contents := strings.Join([]string{
		`{"timestamp":"2026-11-14T02:00:00.000Z","type":"session_meta","payload":{"cwd":"/tmp/project","timestamp":"2026-11-14T02:00:00.000Z"}}`,
		`{"timestamp":"2026-11-14T02:00:00.000Z","type":"turn_context","payload":{"model":"codex-test"}}`,
		`{"timestamp":"2026-11-14T02:00:02.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":15,"cached_input_tokens":40}}}}`,
	}, "\n")
	if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
