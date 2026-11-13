package agentsession

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseKimiCodeTranscript(t *testing.T) {
	file := filepath.Join(t.TempDir(), "wire.jsonl")
	writeFile(t, file, strings.Join([]string{
		`{"type":"agent.message.appended","time":1790000000000,"message":{"message":{"role":"user","content":[{"type":"text","text":"Read the format"}]}}}`,
		`{"type":"agent.message.appended","time":1790000001000,"message":{"message":{"role":"assistant","content":[{"type":"think","think":"Plan first."},{"type":"text","text":"On it."}],"toolCalls":[{"type":"function","id":"tool-1","name":"Read","arguments":"{\"path\":\"README.md\"}"}]}}}`,
		`{"type":"agent.message.appended","time":1790000002000,"message":{"message":{"role":"tool","toolCallId":"tool-1","content":[{"type":"text","text":"body"}]}}}`,
		`{"type":"turn.ended","durationMs":65000,"time":1790000003000}`,
	}, "\n"))

	entries, err := parseKimiCodeTranscript(transcriptInfo(t, "session_s1", file))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("%d entries, want 4: %+v", len(entries), entries)
	}
	if entries[0].Role != RoleUser || entries[0].Text != "Read the format" || entries[0].Timestamp != 1790000000000 {
		t.Errorf("first entry %+v, want the user prompt with its millisecond time", entries[0])
	}
	if entries[1].Thinking != "Plan first." {
		t.Errorf("thinking entry %+v, want the think part", entries[1])
	}
	if entries[2].Text != "On it." {
		t.Errorf("text entry %+v, want the assistant text", entries[2])
	}
	read := entries[3].Tools[0]
	if len(entries[3].Tools) != 1 || read.Name != "Read" || read.Title != "README.md" ||
		read.ID != "tool-1" || read.Output != "body" || read.Failed {
		t.Errorf("read call %+v, want the result matched by toolCallId", read)
	}
	if entries[3].DurationMs != 65000 {
		t.Errorf("duration %v, want the turn duration on its final entry", entries[3].DurationMs)
	}
}
