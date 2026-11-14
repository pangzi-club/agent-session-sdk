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
