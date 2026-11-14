package agentsession

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetailedScansRetainSuccessAndReportFailures(t *testing.T) {
	home := t.TempDir()
	writeCodexErrorReportFixture(t, home)
	writeFile(t, filepath.Join(home, ".claude", "projects"), "not a directory")

	sessions := ScanSessionsDetailed(home)
	if len(sessions.Failures) != 1 || sessions.Failures[0].AgentID != "claude" {
		t.Fatalf("session failures = %+v, want one Claude failure", sessions.Failures)
	}
	codex := findResult(t, sessions.Results, "codex")
	if len(codex.Sessions) != 1 || codex.Sessions[0].ID != "rollout-error-report" {
		t.Fatalf("Codex sessions = %+v, want the successful result", codex.Sessions)
	}

	usage := ScanAllUsageDetailed(home)
	if len(usage.Failures) != 1 || usage.Failures[0].AgentID != "claude" {
		t.Fatalf("usage failures = %+v, want one Claude failure", usage.Failures)
	}
	if len(usage.Scan.Records) != 1 || usage.Scan.Records[0].AgentID != "codex" ||
		usage.Scan.Records[0].SessionID != "rollout-error-report" {
		t.Fatalf("usage records = %+v, want the successful Codex record", usage.Scan.Records)
	}
}

func TestDetailedScansTreatMissingStoresAsEmpty(t *testing.T) {
	home := t.TempDir()
	if report := ScanSessionsDetailed(home); len(report.Failures) != 0 {
		t.Fatalf("missing session stores produced failures: %+v", report.Failures)
	}
	if report := ScanAllUsageDetailed(home); len(report.Failures) != 0 {
		t.Fatalf("missing usage stores produced failures: %+v", report.Failures)
	}
}

// A corrupt Codex store must fail Codex only; the Claude session and usage
// records next to it survive, and the failure list names Codex.
func TestDetailedScansIsolateCodexEnumerationFailure(t *testing.T) {
	home := t.TempDir()
	writeClaudeIndexFixture(t, home, "sess-keep", "Still here", "2026-11-06T02:25:32.506Z")
	writeFile(t, filepath.Join(home, ".codex", "sessions"), "not a directory")

	sessions := ScanSessionsDetailed(home)
	if len(sessions.Failures) != 1 || sessions.Failures[0].AgentID != "codex" {
		t.Fatalf("session failures = %+v, want one Codex failure", sessions.Failures)
	}
	claude := findResult(t, sessions.Results, "claude")
	if len(claude.Sessions) != 1 || claude.Sessions[0].ID != "sess-keep" {
		t.Fatalf("Claude sessions = %+v, want the surviving session", claude.Sessions)
	}

	usage := ScanAllUsageDetailed(home)
	if len(usage.Failures) != 1 || usage.Failures[0].AgentID != "codex" {
		t.Fatalf("usage failures = %+v, want one Codex failure", usage.Failures)
	}
	for _, record := range usage.Scan.Records {
		if record.AgentID == "codex" {
			t.Fatalf("Codex record survived a failed store scan: %+v", record)
		}
	}
}

func writeCodexErrorReportFixture(t *testing.T, home string) {
	t.Helper()
	file := filepath.Join(home, ".codex", "sessions", "2026", "11", "14", "rollout-error-report.jsonl")
	writeFile(t, file, strings.Join([]string{
		`{"timestamp":"2026-11-14T02:00:00.000Z","type":"session_meta","payload":{"cwd":"/tmp/project","timestamp":"2026-11-14T02:00:00.000Z"}}`,
		`{"timestamp":"2026-11-14T02:00:00.000Z","type":"turn_context","payload":{"model":"codex-test"}}`,
		`{"timestamp":"2026-11-14T02:00:02.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":15,"cached_input_tokens":40}}}}`,
	}, "\n"))
}

func replaceClaudeProjectsWithFile(t *testing.T, home string) {
	t.Helper()
	projects := filepath.Join(home, ".claude", "projects")
	if err := os.RemoveAll(projects); err != nil {
		t.Fatal(err)
	}
	writeFile(t, projects, "not a directory")
}
