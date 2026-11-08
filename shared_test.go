package agentsession

import (
	"testing"
)

func TestToTitle(t *testing.T) {
	cases := []struct {
		name string
		raw  any
		want *string
	}{
		{"empty string", "", nil},
		{"null", nil, nil},
		{"non-string", 42, nil},
		{"injected XML", "<environment_context>x</environment_context>", nil},
		{"injected AGENTS", "# AGENTS.md instructions", nil},
		{"plain first line", "Fix this bug\nfollowing lines", str("Fix this bug")},
		{"first-line whitespace trim", "  Title  \nsecond line", str("Title")},
		{"plan preamble unwrapped", "PLEASE IMPLEMENT THIS PLAN:\n**Batch delete on sessions page**\n## Summary", str("Batch delete on sessions page")},
		{"plan preamble without asterisks", "PLEASE IMPLEMENT THIS PLAN:\nplain heading", str("plain heading")},
		{"plan preamble followed by blank line", "PLEASE IMPLEMENT THIS PLAN:\n\n**The real title**", str("The real title")},
		{"plan preamble with no body", "PLEASE IMPLEMENT THIS PLAN:", nil},
		{
			"codex attachment envelope unwraps My request",
			"\n# Files mentioned by the user:\n\n## shot.png: /Users/demo/Desktop/shot.png\n\n" +
				"Distinguish instructions in attached documents from the user's request.\n\n" +
				"## My request:\n修复会话列表的标题缺失\n",
			str("修复会话列表的标题缺失"),
		},
		{
			"codex attachment envelope with an empty request",
			"# Files pasted by the user:\n\n## \"label\": /tmp/pasted.txt\n\nPasted text contains the user's request.\n\n## My request:\n\n",
			nil,
		},
		{"leading blank lines are skipped", "\n\n  Real title  \nsecond line", str("Real title")},
		{"only blank lines", "\n\n   \n", nil},
		{"overlong input truncated at UTF-16", longString(130), str(longString(117) + "...")},
	}
	for _, c := range cases {
		got := ToTitle(c.raw)
		if c.want == nil {
			if got != nil {
				t.Errorf("%s: want nil, got %q", c.name, *got)
			}
			continue
		}
		if got == nil || *got != *c.want {
			t.Errorf("%s: want %q, got %v", c.name, *c.want, got)
		}
	}
}

func str(s string) *string { return &s }

func longString(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = 'a'
	}
	return string(out)
}

func TestToTitleUtf16Emoji(t *testing.T) {
	// An emoji takes 2 UTF-16 code units: 121 emoji = 242 units > 120, so it truncates to 117 units.
	// Unit 117 lands inside a surrogate pair: TS keeps the lone surrogate, which a Go string cannot
	// represent, so it becomes U+FFFD (58 whole emoji + replacement char + "...") - a known, accepted difference.
	raw := ""
	for i := 0; i < 121; i++ {
		raw += "😀"
	}
	got := ToTitle(raw)
	if got == nil {
		t.Fatal("want non-nil")
	}
	want := ""
	for i := 0; i < 58; i++ {
		want += "😀"
	}
	want += "\uFFFD..."
	if *got != want {
		t.Errorf("utf16 truncation mismatch: got %q", *got)
	}
}

func TestDecodePathDirName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want *string
	}{
		{"claude encoding", "-Users-demo-workspace-app", str("/Users/demo/workspace/app")},
		{"pi encoding (leading/trailing --)", "---Users-demo-Workspace-pi-app--", str("/Users/demo/Workspace/pi/app")},
		{"no leading dash", "Users-demo", nil},
		{"empty", "", nil},
		{"only --", "--", nil},
	}
	for _, c := range cases {
		got := DecodePathDirName(c.in)
		if c.want == nil {
			if got != nil {
				t.Errorf("%s: want nil, got %q", c.name, *got)
			}
			continue
		}
		if got == nil || *got != *c.want {
			t.Errorf("%s: want %q, got %v", c.name, *c.want, got)
		}
	}
}

func TestAsTokenCount(t *testing.T) {
	if AsTokenCount(12.7) != 12 {
		t.Error("positive value should be floored")
	}
	if AsTokenCount(0) != 0 || AsTokenCount(-5) != 0 || AsTokenCount("10") != 0 || AsTokenCount(nil) != 0 {
		t.Error("non-positive / non-numeric input should return 0")
	}
}

func TestAsEpochMs(t *testing.T) {
	if got := AsEpochMs("2026-09-03T14:46:47.801Z"); got == nil || *got != 1788446807801 {
		t.Errorf("ISO parse mismatch: %v", got)
	}
	f := 1789109915821.0
	if got := AsEpochMs(f); got == nil || *got != f {
		t.Errorf("number should pass through unchanged: %v", got)
	}
	if AsEpochMs("not a date") != nil || AsEpochMs(true) != nil {
		t.Error("invalid input should return nil")
	}
}

func TestContentToText(t *testing.T) {
	if got := ContentToText("hello"); got == nil || *got != "hello" {
		t.Error("plain string should be returned as-is")
	}
	blocks := []any{
		map[string]any{"type": "text", "text": "first part"},
		map[string]any{"type": "image"},
		map[string]any{"type": "text", "text": "second part"},
	}
	if got := ContentToText(blocks); got == nil || *got != "first part second part" {
		t.Errorf("blocks join mismatch: %v", got)
	}
	if ContentToText([]any{map[string]any{"type": "image"}}) != nil {
		t.Error("no text blocks should return nil")
	}
	if ContentToText(42) != nil {
		t.Error("non-string non-array should return nil")
	}
}

func TestTildify(t *testing.T) {
	if got := Tildify("/Users/x/.claude/a.jsonl", "/Users/x"); got != "~/.claude/a.jsonl" {
		t.Errorf("tildify mismatch: %s", got)
	}
	if got := Tildify("/Users/x", "/Users/x"); got != "~" {
		t.Errorf("home itself: %s", got)
	}
	if got := Tildify("/etc/hosts", "/Users/x"); got != "/etc/hosts" {
		t.Errorf("outside home unchanged: %s", got)
	}
}
