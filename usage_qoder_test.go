package agentsession

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScanQoderFileUsage mirrors the qoder-cn fixture in usage/fixtures.ts:
// lines sharing one message.id merge into a single request, input =
// ratio × contextWindow (a window switch takes effect), output is a weighted
// estimate over CJK / non-CJK characters, credits pass through (including
// billable=false), and old lines without a ratio fall back to the token
// fields.
func TestScanQoderFileUsage(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sess.jsonl")
	lines := []string{
		`{"type":"runtime-config","model":"qmodel_38max","contextWindow":200000,"timestamp":1790000000000}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"统计一下用量"}]},"timestamp":"2026-10-16T12:17:00.000Z","sessionId":"sess-qoder"}`,
		`{"type":"assistant","message":{"id":"msg-1","role":"assistant","model":"qmodel_38max","content":[{"type":"thinking","thinking":"Let me look."}]},"timestamp":"2026-10-16T12:17:01.000Z","sessionId":"sess-qoder"}`,
		`{"type":"assistant","message":{"id":"msg-1","role":"assistant","model":"qmodel_38max","content":[{"type":"tool_use","name":"Bash","id":"t1"},{"type":"tool_use","name":"Read","id":"t2"},{"type":"tool_use","name":"Skill","id":"t4","input":{"skill":"add-agent"}}]},"timestamp":"2026-10-16T12:17:02.000Z","sessionId":"sess-qoder"}`,
		`{"type":"assistant","message":{"id":"msg-1","role":"assistant","model":"qmodel_38max","content":[{"type":"text","text":"已完成检查。"}],"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"context_usage_ratio":0.05,"credits":9.21627828,"original_credits":9.21627828,"billable":true}},"timestamp":"2026-10-16T12:17:02.000Z","sessionId":"sess-qoder"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},"timestamp":"2026-10-16T12:17:03.000Z","sessionId":"sess-qoder"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"再看一眼"}]},"timestamp":"2026-10-16T12:17:05.000Z","sessionId":"sess-qoder"}`,
		`{"type":"runtime-config","model":"auto","contextWindow":128000,"timestamp":1790000000000}`,
		`{"type":"assistant","message":{"id":"msg-2","role":"assistant","model":"auto","content":[{"type":"tool_use","name":"Grep","id":"t3"},{"type":"tool_use","name":"Skill","id":"t5","input":{"skill":"release"}}]},"timestamp":"2026-10-16T12:17:06.000Z","sessionId":"sess-qoder"}`,
		`{"type":"assistant","message":{"id":"msg-2","role":"assistant","model":"auto","content":"done","usage":{"context_usage_ratio":0.1,"credits":1.5,"billable":false}},"timestamp":"2026-10-16T12:17:07.000Z","sessionId":"sess-qoder"}`,
		`{"type":"assistant","message":{"id":"msg-3","role":"assistant","model":"auto","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":77,"output_tokens":8,"credits":0.25}},"timestamp":"2026-10-16T12:17:09.000Z","sessionId":"sess-qoder"}`,
	}
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	records := scanQoderFileUsage(file, "qoder-cn")
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3", len(records))
	}

	first := records[0]
	if first.SessionID != "sess-qoder" || first.Model == nil || *first.Model != "qmodel_38max" {
		t.Fatalf("first record session/model = %q/%v", first.SessionID, first.Model)
	}
	if first.InputTokens != 10000 {
		t.Errorf("first input = %v, want 10000 (0.05 × 200000)", first.InputTokens)
	}
	if first.OutputTokens != 14 {
		t.Errorf("first output = %v, want 14", first.OutputTokens)
	}
	if first.Credits == nil || math.Abs(*first.Credits-9.21627828) > 1e-12 {
		t.Errorf("first credits = %v, want 9.21627828", first.Credits)
	}
	if first.UserPrompts != 1 || first.ToolCalls != 3 {
		t.Errorf("first prompts/tools = %v/%v, want 1/3", first.UserPrompts, first.ToolCalls)
	}
	if first.SkillCallsByName == nil || (*first.SkillCallsByName)["add-agent"] != 1 {
		t.Errorf("first skill calls = %v, want add-agent ×1", first.SkillCallsByName)
	}
	if first.ActiveMs != 2000 {
		t.Errorf("first activeMs = %v, want 2000", first.ActiveMs)
	}

	second := records[1]
	if second.InputTokens != 12800 {
		t.Errorf("second input = %v, want 12800 (0.1 × 128000)", second.InputTokens)
	}
	if second.OutputTokens != 2 {
		t.Errorf("second output = %v, want 2", second.OutputTokens)
	}
	if second.Credits == nil || math.Abs(*second.Credits-1.5) > 1e-12 {
		t.Errorf("second credits = %v, want 1.5 (billable=false still retained)", second.Credits)
	}
	if second.ToolCalls != 2 || second.ActiveMs != 5000 {
		t.Errorf("second tools/activeMs = %v/%v, want 2/5000", second.ToolCalls, second.ActiveMs)
	}

	third := records[2]
	if third.InputTokens != 77 || third.OutputTokens != 8 {
		t.Errorf("third tokens = %v/%v, want 77/8 (fallback)", third.InputTokens, third.OutputTokens)
	}
	if third.Credits == nil || *third.Credits != 0.25 {
		t.Errorf("third credits = %v, want 0.25 (missing billable still retained)", third.Credits)
	}
	if third.ActiveMs != 2000 {
		t.Errorf("third activeMs = %v, want 2000", third.ActiveMs)
	}
}
