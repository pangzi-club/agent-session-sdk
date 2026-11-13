package agentsession

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile creates a fixture file, creating parent directories as needed.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findResult(t *testing.T, results []SessionScanResult, agentID string) SessionScanResult {
	t.Helper()
	for _, r := range results {
		if r.AgentID == agentID {
			return r
		}
	}
	t.Fatalf("agent %q missing from scan results", agentID)
	return SessionScanResult{}
}

// Claude's JSONL fallback: cwd and timestamp come from the first user line.
func TestScanClaude(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, ".claude", "projects", "-Users-demo-app", "sess-1.jsonl")
	writeFile(t, file, strings.Join([]string{
		`{"type":"user","cwd":"/Users/demo/app","timestamp":"2026-11-06T02:25:32.506Z","message":{"role":"user","content":[{"type":"text","text":"Fix the login bug"}]}}`,
		`{"type":"assistant","cwd":"/Users/demo/app","message":{"role":"assistant","content":[{"type":"text","text":"Done."}]}}`,
	}, "\n"))

	sessions, err := ScanClaude(filepath.Join(home, ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("%d sessions, want 1", len(sessions))
	}
	s := sessions[0]
	if s.ID != "sess-1" || s.Workspace != "/Users/demo/app" {
		t.Errorf("id/workspace = %q/%q, want sess-1//Users/demo/app", s.ID, s.Workspace)
	}
	if s.Title == nil || *s.Title != "Fix the login bug" {
		t.Errorf("title = %v, want the first user prompt", s.Title)
	}
	if s.FilePath == nil || *s.FilePath != file {
		t.Errorf("filePath = %v, want %q", s.FilePath, file)
	}
	if s.UpdatedAt == 0 {
		t.Error("updatedAt should use the file mtime")
	}
}

// Qoder's workspace-directories line wins over the cwd fallback.
func TestScanQoderCN(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, ".qoder-cn", "projects", "-Users-demo-app", "q1.jsonl")
	writeFile(t, file, strings.Join([]string{
		`{"type":"workspace-directories","sessionId":"q1","directories":["/Users/demo/app"]}`,
		`{"type":"runtime-config","sessionId":"q1","model":"demo","timestamp":1793931932461}`,
		`{"type":"user","cwd":"/Users/demo/other","timestamp":"2026-11-06T02:25:32.506Z","message":{"role":"user","content":[{"type":"text","text":"Scan the repository"}]}}`,
	}, "\n"))

	results := ScanSessions(home)
	r := findResult(t, results, "qoder-cn")
	if len(r.Sessions) != 1 {
		t.Fatalf("%d sessions, want 1", len(r.Sessions))
	}
	s := r.Sessions[0]
	if s.Workspace != "/Users/demo/app" {
		t.Errorf("workspace = %q, want /Users/demo/app", s.Workspace)
	}
	if s.Title == nil || *s.Title != "Scan the repository" {
		t.Errorf("title = %v, want the user prompt", s.Title)
	}
}

// A system reminder embedded before the real prompt must not hide the title.
func TestScanQoderTitleStripsSystemReminder(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, ".qoder-cn", "projects", "-Users-demo-app", "q2.jsonl")
	writeFile(t, file, strings.Join([]string{
		`{"type":"workspace-directories","sessionId":"q2","directories":["/Users/demo/app"]}`,
		`{"type":"user","cwd":"/Users/demo/app","timestamp":"2026-11-06T02:25:32.506Z","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>\nFor browser tasks, prefer remote-browser-use.\n</system-reminder>\n\nFix the title extraction"}]}}`,
	}, "\n"))

	sessions, err := ScanQoder(filepath.Join(home, ".qoder-cn"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("%d sessions, want 1", len(sessions))
	}
	if sessions[0].Title == nil || *sessions[0].Title != "Fix the title extraction" {
		t.Errorf("title = %v, want the real prompt after the reminder", sessions[0].Title)
	}
}

// Codex metadata lives in the session_meta event and the title in the first
// user response_item.
func TestScanCodex(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, ".codex", "sessions", "2026", "11", "06", "rollout-1.jsonl")
	writeFile(t, file, strings.Join([]string{
		`{"type":"session_meta","payload":{"cwd":"/Users/demo/app","timestamp":"2026-11-06T03:00:00.000Z"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Add a regression test"}]}}`,
	}, "\n"))

	sessions, err := ScanCodex(filepath.Join(home, ".codex"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("%d sessions, want 1", len(sessions))
	}
	s := sessions[0]
	if s.Workspace != "/Users/demo/app" {
		t.Errorf("workspace = %q, want /Users/demo/app", s.Workspace)
	}
	if s.Title == nil || *s.Title != "Add a regression test" {
		t.Errorf("title = %v, want the user prompt", s.Title)
	}
}

// Guardian review threads are internal approval-assessment rollouts, not
// standalone conversations, so they must not appear in the session list.
func TestScanCodexSkipsGuardianReviews(t *testing.T) {
	home := t.TempDir()
	sessionDir := filepath.Join(home, ".codex", "sessions", "2026", "11", "06")
	writeFile(t, filepath.Join(sessionDir, "rollout-main.jsonl"), strings.Join([]string{
		`{"type":"session_meta","payload":{"cwd":"/Users/demo/app","thread_source":"user","timestamp":"2026-11-06T03:00:00.000Z"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Ship the fix"}]}}`,
	}, "\n"))
	writeFile(t, filepath.Join(sessionDir, "rollout-guardian.jsonl"), strings.Join([]string{
		`{"type":"session_meta","payload":{"cwd":"/Users/demo/app","thread_source":"guardian_review","timestamp":"2026-11-06T03:01:00.000Z"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"The following is the Codex agent history whose request action you are assessing"}]}}`,
	}, "\n"))

	sessions, err := ScanCodex(filepath.Join(home, ".codex"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("%d sessions, want only the main session", len(sessions))
	}
	if sessions[0].ID != "rollout-main" {
		t.Errorf("session ID = %q, want rollout-main", sessions[0].ID)
	}
}

// Missing agent directories are normal: every agent still reports an empty list.
func TestScanKimiCode(t *testing.T) {
	home := t.TempDir()
	sessionDir := filepath.Join(home, ".kimi-code", "sessions", "wd_app_demo", "session_s1")
	file := filepath.Join(sessionDir, "agents", "main", "wire.jsonl")
	writeFile(t, filepath.Join(home, ".kimi-code", "session_index.jsonl"), strings.Join([]string{
		`{"sessionId":"session_s1","sessionDir":"` + sessionDir + `","workDir":"/Users/demo/app"}`,
	}, "\n"))
	writeFile(t, filepath.Join(sessionDir, "state.json"), `{
		"id":"session_s1","cwd":"/Users/demo/app","title":"Stored title",
		"createdAt":1790000000000,"updatedAt":1790000060000
	}`)
	writeFile(t, file, strings.Join([]string{
		`{"type":"metadata","protocol_version":"1.5","created_at":1790000000000}`,
		`{"type":"agent.message.appended","time":1790000000000,"message":{"message":{"role":"user","content":[{"type":"text","text":"Read the new store"}]}}}`,
	}, "\n"))

	sessions, err := ScanKimiCode(filepath.Join(home, ".kimi-code"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("%d sessions, want 1", len(sessions))
	}
	s := sessions[0]
	if s.ID != "session_s1" || s.Workspace != "/Users/demo/app" ||
		s.Title == nil || *s.Title != "Stored title" || s.FilePath == nil || *s.FilePath != file {
		t.Errorf("session %+v, want the state metadata and wire path", s)
	}
	if s.CreatedAt != 1790000000000 || s.UpdatedAt != 1790000060000 {
		t.Errorf("timestamps %v/%v, want the state timestamps", s.CreatedAt, s.UpdatedAt)
	}
}

func TestScanSessionsEmptyHome(t *testing.T) {
	results := ScanSessions(t.TempDir())
	if len(results) != 16 {
		t.Fatalf("%d results, want 16", len(results))
	}
	for _, r := range results {
		if r.AgentName == "" {
			t.Errorf("agent %q has no display name", r.AgentID)
		}
		if len(r.Sessions) != 0 {
			t.Errorf("agent %q returned sessions from an empty home", r.AgentID)
		}
	}
}

// Files without a usable metadata line are not sessions.
func TestScanClaudeIgnoresUnparsableFile(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".claude", "projects", "-Users-demo-app", "empty.jsonl"), "{}\nnot json\n")
	sessions, err := ScanClaude(filepath.Join(home, ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("%d sessions, want 0", len(sessions))
	}
}

// DSH titles come from the last session/title event (the LLM-generated one
// replaces the fallback); without title events the first user input is used.
func TestScanDshFileTitle(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "session-x", "session.v4.jsonl.zstd")
	writeDshSession(t, file, []string{
		`{"type":"session","version":4,"id":"session-x","cwd":"/app","createdAt":1000,"time":1000}`,
		`{"type":"agent/inbox/spliced","time":1001,"data":{"inserted":[{"id":"u1","role":"user","content":[{"type":"text","text":"First prompt text"}]}]}}`,
		`{"type":"session/title","time":1002,"data":{"source":{"kind":"fallback"},"title":"First prompt"}}`,
		`{"type":"session/title","time":1003,"data":{"source":{"kind":"provider"},"title":"Generated title"}}`,
	})
	info, err := ScanDshFile(file)
	if err != nil || info == nil {
		t.Fatalf("ScanDshFile = %v, %v", info, err)
	}
	if info.Title == nil || *info.Title != "Generated title" {
		t.Errorf("title %v, want the last session/title event", info.Title)
	}

	noTitle := filepath.Join(dir, "session-y", "session.v4.jsonl.zstd")
	writeDshSession(t, noTitle, []string{
		`{"type":"session","version":4,"id":"session-y","cwd":"/app","createdAt":1000,"time":1000}`,
		`{"type":"agent/inbox/spliced","time":1001,"data":{"inserted":[{"id":"u1","role":"user","content":[{"type":"text","text":"First prompt text"}]}]}}`,
	})
	info, err = ScanDshFile(noTitle)
	if err != nil || info == nil {
		t.Fatalf("ScanDshFile = %v, %v", info, err)
	}
	if info.Title == nil || *info.Title != "First prompt text" {
		t.Errorf("title %v, want the first user input fallback", info.Title)
	}
}
