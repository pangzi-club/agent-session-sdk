package agentsession

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestScanClaudeUsageRecords covers Claude usage semantics on a full session:
// user prompts and tool_use events attach to the record intervals, input
// excludes cache reads, and the tail interval merges into the last record.
func TestScanClaudeUsageRecords(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sess.jsonl")
	lines := []string{
		`{"type":"user","timestamp":"2026-11-06T02:25:30.000Z","message":{"role":"user","content":[{"type":"text","text":"跑一下测试"}]}}`,
		`{"type":"assistant","timestamp":"2026-11-06T02:25:31.000Z","message":{"role":"assistant","model":"claude-sonnet-5","content":[{"type":"tool_use","name":"Bash","id":"t1","input":{"command":"go test"}}]}}`,
		`{"type":"user","timestamp":"2026-11-06T02:25:32.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		`{"type":"assistant","timestamp":"2026-11-06T02:25:33.000Z","message":{"role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":120,"output_tokens":10,"cache_read_input_tokens":80,"cache_creation_input_tokens":5}}}`,
		// Sidechain (subagent) traffic must not count toward the main session.
		`{"type":"user","isSidechain":true,"timestamp":"2026-11-06T02:25:34.000Z","message":{"role":"user","content":[{"type":"text","text":"sub"}]}}`,
		`{"type":"assistant","isSidechain":true,"timestamp":"2026-11-06T02:25:35.000Z","message":{"role":"assistant","model":"claude-haiku-5","content":"sub answer","usage":{"input_tokens":999,"output_tokens":999}}}`,
	}
	writeFile(t, file, strings.Join(lines, "\n")+"\n")

	records, err := scanClaudeUsageRecords(file, "sess", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want exactly the main-session record", len(records))
	}
	record := records[0]
	if record.SessionID != "sess" || record.AgentID != "claude" ||
		record.Model == nil || *record.Model != "claude-sonnet-5" {
		t.Fatalf("record identity = %+v", record)
	}
	// input_tokens already excludes cache reads in Claude's own accounting.
	if record.InputTokens != 120 || record.OutputTokens != 10 ||
		record.CacheReadTokens != 80 || record.CacheWriteTokens != 5 {
		t.Fatalf("token fields = %+v", record)
	}
	if record.UserPrompts != 1 || record.ToolCalls != 1 {
		t.Fatalf("interval events prompts=%v tools=%v, want 1/1", record.UserPrompts, record.ToolCalls)
	}
	if record.ToolCallsByName == nil || (*record.ToolCallsByName)["Bash"] != 1 {
		t.Fatalf("toolCallsByName = %v, want Bash ×1", record.ToolCallsByName)
	}
	if record.ActiveMs != 3000 {
		t.Fatalf("activeMs = %v, want 3000 (last event at 02:25:33 − first at 02:25:30)", record.ActiveMs)
	}
}

// TestScanCodexUsageRecordsDelta covers Codex cumulative counters: adjacent
// deltas, input minus cached reads, and negative deltas (counter reset)
// clamped to zero.
func TestScanCodexUsageRecordsDelta(t *testing.T) {
	file := filepath.Join(t.TempDir(), "rollout.jsonl")
	lines := []string{
		`{"timestamp":"2026-11-14T02:00:00.000Z","type":"turn_context","payload":{"model":"gpt-test"}}`,
		`{"timestamp":"2026-11-14T02:00:02.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":15,"cached_input_tokens":40,"cache_write_input_tokens":10}}}}`,
		`{"timestamp":"2026-11-14T02:00:04.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":250,"output_tokens":45,"cached_input_tokens":90,"cache_write_input_tokens":30}}}}`,
		// Counter reset (compaction): every delta clamps to 0.
		`{"timestamp":"2026-11-14T02:00:06.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":30,"output_tokens":5,"cached_input_tokens":10,"cache_write_input_tokens":0}}}}`,
	}
	writeFile(t, file, strings.Join(lines, "\n")+"\n")

	records, err := scanCodexUsageRecords(file, "rollout", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3", len(records))
	}
	first, second, third := records[0], records[1], records[2]
	// First event: raw totals, input minus cached reads.
	if first.InputTokens != 60 || first.OutputTokens != 15 ||
		first.CacheReadTokens != 40 || first.CacheWriteTokens != 10 {
		t.Fatalf("first record = %+v", first)
	}
	// Second event: deltas (150 input − 50 cached growth = 100 net input).
	if second.InputTokens != 100 || second.OutputTokens != 30 ||
		second.CacheReadTokens != 50 || second.CacheWriteTokens != 20 {
		t.Fatalf("second record = %+v", second)
	}
	// Third event: counter fell back, all deltas clamp to 0.
	if third.InputTokens != 0 || third.OutputTokens != 0 ||
		third.CacheReadTokens != 0 || third.CacheWriteTokens != 0 {
		t.Fatalf("third record = %+v, want zeroed deltas", third)
	}
	for _, record := range records {
		if record.Model == nil || *record.Model != "gpt-test" {
			t.Fatalf("model = %v, want gpt-test from the first turn_context", record.Model)
		}
	}
}

// TestScanAllUsageDetailedAggregation covers the aggregation contract:
// records concatenate in module order, sessions merge by bare session id with
// last-write-wins (later module wins), token totals aggregate across every
// module's records for that id, and every emitted session carries status 3.
func TestScanAllUsageDetailedAggregation(t *testing.T) {
	home := t.TempDir()
	// A Claude session with one billed turn (Claude lines carry sessionId).
	writeFile(t, filepath.Join(home, ".claude", "projects", "-tmp-agg", "rollout-agg.jsonl"), strings.Join([]string{
		`{"type":"user","sessionId":"rollout-agg","cwd":"/tmp/agg","timestamp":"2026-11-14T03:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":"aggregate me"}]}}`,
		`{"type":"assistant","sessionId":"rollout-agg","timestamp":"2026-11-14T03:00:01.000Z","message":{"role":"assistant","model":"claude-agg","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":90,"output_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
	}, "\n")+"\n")
	// A Codex session reusing the same bare session id with two turns.
	writeFile(t, filepath.Join(home, ".codex", "sessions", "2026", "11", "14", "rollout-agg.jsonl"), strings.Join([]string{
		`{"timestamp":"2026-11-14T02:00:00.000Z","type":"session_meta","payload":{"cwd":"/tmp/agg","timestamp":"2026-11-14T02:00:00.000Z"}}`,
		`{"timestamp":"2026-11-14T02:00:00.000Z","type":"turn_context","payload":{"model":"codex-agg"}}`,
		`{"timestamp":"2026-11-14T02:00:02.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":10,"cached_input_tokens":0}}}}`,
		`{"timestamp":"2026-11-14T02:00:04.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":300,"output_tokens":40,"cached_input_tokens":0}}}}`,
	}, "\n")+"\n")

	report := ScanAllUsageDetailed(home)
	if len(report.Failures) != 0 {
		t.Fatalf("failures = %+v", report.Failures)
	}
	if len(report.Scan.Records) != 3 {
		t.Fatalf("records = %d, want 1 claude + 2 codex", len(report.Scan.Records))
	}
	// Module order: claude records first, then codex.
	if report.Scan.Records[0].AgentID != "claude" || report.Scan.Records[1].AgentID != "codex" {
		t.Fatalf("record order = %s, %s", report.Scan.Records[0].AgentID, report.Scan.Records[1].AgentID)
	}
	sessions := report.Scan.Sessions
	if len(sessions) != 1 || sessions[0].ID != "rollout-agg" {
		t.Fatalf("sessions = %+v, want one merged rollout-agg", sessions)
	}
	merged := sessions[0]
	// Codex is the later module in aggregation order, so its session entry wins
	// the map even though Claude listed the id first.
	if merged.AgentID != "codex" || merged.Title != nil {
		t.Fatalf("merged session identity = %+v, want the Codex last write", merged)
	}
	// Aggregated totals cover every module's records for that session id:
	// input 90 + 100 + 200, output 10 + 10 + 30, status 3.
	if merged.TokenUsage == nil {
		t.Fatal("merged session lost its token totals")
	}
	usage := *merged.TokenUsage
	if usage[0] != 390 || usage[1] != 50 || usage[4] != 3 {
		t.Fatalf("tokenUsage = %v, want [390 50 0 0 3]", usage)
	}

	summaries := Summarize(report.Scan)
	if len(summaries) != 2 {
		t.Fatalf("summaries = %+v", summaries)
	}
	byID := map[string]AgentUsageSummary{}
	for _, summary := range summaries {
		byID[summary.AgentID] = summary
	}
	// Only the merged session survives the session map, so it counts for the
	// winning agent; requests and tokens still attribute per record agent.
	if byID["claude"].Sessions != 0 || byID["claude"].Requests != 1 || byID["claude"].InputTokens != 90 {
		t.Fatalf("claude summary = %+v", byID["claude"])
	}
	if byID["codex"].Sessions != 1 || byID["codex"].Requests != 2 || byID["codex"].InputTokens != 300 {
		t.Fatalf("codex summary = %+v", byID["codex"])
	}
}
