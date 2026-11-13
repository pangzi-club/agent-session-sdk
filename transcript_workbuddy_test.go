package agentsession

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseWorkbuddyTranscript(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "s1.jsonl")
	writeFile(t, file, strings.Join([]string{
		`{"type":"session-meta","id":"meta","timestamp":100}`,
		`{"type":"message","role":"user","timestamp":1000,"content":"<system-reminder>noise</system-reminder>\n\n分析技术栈"}`,
		`{"type":"reasoning","timestamp":1100,"content":[],"rawContent":[{"type":"reasoning_text","text":"先读核心配置"}]}`,
		`{"type":"message","role":"assistant","timestamp":1200,"content":[{"type":"output_text","text":"我先读配置文件。"}]}`,
		`{"type":"function_call","name":"Read","timestamp":1300,"callId":"call-1","arguments":"{\"file_path\":\"/tmp/go.mod\"}"}`,
		`{"type":"function_call_result","name":"Read","timestamp":1400,"callId":"call-1","status":"completed","output":{"type":"text","text":"module demo"}}`,
		`{"type":"message","role":"assistant","timestamp":1500,"content":[{"type":"output_text","text":"这是一个 Go 项目。"}]}`,
	}, "\n"))

	entries, err := parseWorkbuddyTranscript(transcriptInfo(t, "s1", file))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("%d entries, want 5: %+v", len(entries), entries)
	}
	if entries[0].Role != RoleUser || entries[0].Text != "分析技术栈" {
		t.Errorf("first entry %+v, want the cleaned user prompt", entries[0])
	}
	if entries[1].Thinking != "先读核心配置" || entries[1].Timestamp != 1100 {
		t.Errorf("reasoning entry %+v, want the raw reasoning text", entries[1])
	}
	if entries[2].Text != "我先读配置文件。" {
		t.Errorf("assistant text entry %+v, want the visible narration", entries[2])
	}
	tool := entries[3].Tools[0]
	if len(entries[3].Tools) != 1 || tool.Name != "Read" || tool.Title != "/tmp/go.mod" ||
		tool.ID != "call-1" || tool.Output != "module demo" || tool.Failed {
		t.Errorf("tool call %+v, want the matched successful result", tool)
	}
	if !strings.Contains(tool.Input, "/tmp/go.mod") {
		t.Errorf("tool input %q, want the raw arguments", tool.Input)
	}
	if entries[4].Text != "这是一个 Go 项目。" {
		t.Errorf("final entry %+v, want the final reply", entries[4])
	}
}

func TestWorkbuddyToolOutput(t *testing.T) {
	out := []Entry{{Role: RoleAssistant, Tools: []ToolCall{{
		Name: "Bash", Title: "go test", Meta: "Bash", Kind: ToolCommand, ID: "call-1",
	}}}}
	applyWorkbuddyToolOutput(out, "call-1", map[string]any{
		"status": "error",
		"output": map[string]any{"type": "text", "text": "boom"},
	})
	if out[0].Tools[0].Output != "boom" || !out[0].Tools[0].Failed {
		t.Errorf("tool %+v, want the failed output", out[0].Tools[0])
	}
}
