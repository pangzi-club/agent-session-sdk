package agentsession

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func transcriptInfo(t *testing.T, id, file string) SessionInfo {
	t.Helper()
	return SessionInfo{ID: id, FilePath: &file}
}

func TestParseClaudeTranscript(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "s1.jsonl")
	writeFile(t, file, strings.Join([]string{
		`{"type":"user","timestamp":"2026-11-06T02:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>noise</system-reminder>\n\nFix it"}]}}`,
		`{"type":"assistant","timestamp":"2026-11-06T02:00:01.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Plan first."},{"type":"text","text":"On it."},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/tmp/a.go"}},{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"go test"}},{"type":"text","text":"Done."}]}}`,
		`{"type":"user","timestamp":"2026-11-06T02:00:02.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"},{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":[{"type":"text","text":"boom"}]}]}}`,
		`{"type":"system","subtype":"compact_boundary","timestamp":"2026-11-06T02:00:03.000Z"}`,
	}, "\n"))

	entries, err := parseClaudeTranscript(transcriptInfo(t, "s1", file))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 7 {
		t.Fatalf("%d entries, want 7: %+v", len(entries), entries)
	}
	if entries[0].Role != RoleUser || entries[0].Text != "Fix it" {
		t.Errorf("first entry %+v, want the cleaned user prompt", entries[0])
	}
	if entries[1].Role != RoleAssistant || entries[1].Thinking != "Plan first." {
		t.Errorf("second entry %+v, want the thinking block", entries[1])
	}
	if entries[2].Role != RoleAssistant || entries[2].Text != "On it." {
		t.Errorf("third entry %+v, want assistant text", entries[2])
	}
	read := entries[3].Tools[0]
	if len(entries[3].Tools) != 1 || read.Name != "Read" || read.Title != "/tmp/a.go" ||
		read.ID != "t1" || read.Output != "ok" || read.Failed {
		t.Errorf("read call %+v, want the result matched by id", read)
	}
	if !strings.Contains(read.Input, "/tmp/a.go") {
		t.Errorf("read input %q, want the raw arguments", read.Input)
	}
	bash := entries[4].Tools[0]
	if bash.Name != "Bash" || bash.ID != "t2" || !bash.Failed || bash.Output != "boom" {
		t.Errorf("bash call %+v, want the failed result matched", bash)
	}
	if entries[5].Role != RoleAssistant || entries[5].Text != "Done." {
		t.Errorf("sixth entry %+v, want the trailing assistant text", entries[5])
	}
	if entries[6].Role != RoleSystem || entries[6].Text != "上下文已压缩" {
		t.Errorf("last entry %+v, want the compact notice", entries[6])
	}
}

func TestQoderAppTitle(t *testing.T) {
	dir := t.TempDir()
	dbFile := filepath.Join(dir, "main.sqlite")
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE chat_sessions (session_id TEXT PRIMARY KEY, title TEXT NOT NULL)`,
		`INSERT INTO chat_sessions (session_id, title) VALUES ('s1', 'App 标题'), ('s2', '')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	ro, err := OpenSQLite(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	dbs := []*sql.DB{ro}

	if title := qoderAppTitle(dbs, "s1"); title == nil || *title != "App 标题" {
		t.Fatalf("title %v, want the catalog title", title)
	}
	for _, id := range []string{"s2", "missing"} {
		if title := qoderAppTitle(dbs, id); title != nil {
			t.Fatalf("title %v for %q, want nil", title, id)
		}
	}

	// scanQoderFile prefers the catalog title over the first user prompt.
	src := filepath.Join(dir, "s1.jsonl")
	writeFile(t, src, strings.Join([]string{
		`{"directories":["/tmp/w"],"timestamp":"2026-11-06T02:00:00.000Z"}`,
		`{"type":"user","timestamp":"2026-11-06T02:00:00.000Z","cwd":"/tmp/w","message":{"role":"user","content":[{"type":"text","text":"First prompt"}]}}`,
	}, "\n"))
	session, err := scanQoderFile(src, dir, dbs)
	if err != nil {
		t.Fatal(err)
	}
	if session == nil || session.Title == nil || *session.Title != "App 标题" {
		t.Fatalf("session %+v, want the catalog title", session)
	}
}

func TestParseCodexTranscript(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "rollout.jsonl")
	writeFile(t, file, strings.Join([]string{
		`{"timestamp":"2026-11-06T02:00:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>x</environment_context>"}]}}`,
		`{"timestamp":"2026-11-06T02:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Implement it"}]}}`,
		`{"timestamp":"2026-11-06T02:00:02.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}}`,
		`{"timestamp":"2026-11-06T02:00:03.000Z","type":"response_item","payload":{"type":"function_call","name":"Bash","arguments":"{\"command\":\"go test ./...\"}"}}`,
	}, "\n"))

	entries, err := parseCodexTranscript(transcriptInfo(t, "x1", file))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("%d entries, want 3: %+v", len(entries), entries)
	}
	if entries[0].Text != "Implement it" || entries[1].Text != "Done." {
		t.Errorf("texts %q / %q, want the injected record skipped", entries[0].Text, entries[1].Text)
	}
	call := entries[2].Tools[0]
	if call.Name != "Bash" || call.Title != "go test ./..." || call.Kind != ToolCommand {
		t.Errorf("tool %+v, want a Bash command card", call)
	}
}

func TestParseCursorTranscript(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cu.jsonl")
	writeFile(t, file, strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Friday</timestamp>\n<user_query>\n# Add dark mode\n</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"On it."}]}}`,
		`{"type":"turn_ended","status":"success"}`,
	}, "\n"))

	entries, err := parseCursorTranscript(transcriptInfo(t, "cu", file))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Text != "# Add dark mode" || entries[1].Text != "On it." {
		t.Fatalf("entries %+v, want the unwrapped query", entries)
	}
}

func TestParsePiTranscript(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "pi.jsonl")
	writeFile(t, file, strings.Join([]string{
		`{"type":"session","id":"pi-1"}`,
		`{"type":"message","timestamp":"2026-03-18T14:12:58.960Z","message":{"role":"user","content":[{"type":"text","text":"Check the weather"}]}}`,
	}, "\n"))

	entries, err := parsePiTranscript(transcriptInfo(t, "pi-1", file))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Text != "Check the weather" || entries[0].Timestamp == 0 {
		t.Fatalf("entries %+v, want the user message with its timestamp", entries)
	}
}

func TestParseKimiTranscript(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "wire.jsonl")
	writeFile(t, file, strings.Join([]string{
		`{"type":"metadata","protocol_version":"1.2"}`,
		`{"timestamp":1770264596.95,"message":{"type":"TurnBegin","payload":{"user_input":[{"type":"text","text":"Refactor the login module"}]}}}`,
	}, "\n"))

	entries, err := parseKimiTranscript(transcriptInfo(t, "k1", file))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Text != "Refactor the login module" {
		t.Fatalf("entries %+v, want the turn input", entries)
	}
	if entries[0].Timestamp < 1.7e12 {
		t.Errorf("timestamp %v, want seconds converted to ms", entries[0].Timestamp)
	}
}

func TestParseDshTranscript(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "session.v4.jsonl.zstd")
	content := strings.Join([]string{
		`{"type":"session","version":4,"id":"d1","time":1789109916000}`,
		`{"type":"agent/inbox/spliced","time":1789109916500,"data":{"inserted":[{"role":"user","content":[{"type":"text","text":"Read the session format"}]}]}}`,
	}, "\n")
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, enc.EncodeAll([]byte(content), nil), 0o644); err != nil {
		t.Fatal(err)
	}
	enc.Close()

	entries, err := parseDshTranscript(transcriptInfo(t, "d1", file))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Text != "Read the session format" {
		t.Fatalf("entries %+v, want the inserted user message", entries)
	}
}

func TestParseGeminiTranscript(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "logs.json")
	writeFile(t, file, `[
		{"sessionId":"gem-1","type":"user","message":"Create the page","timestamp":"2025-08-18T04:47:40.519Z"},
		{"sessionId":"gem-1","type":"user","message":"Continue","timestamp":"2025-08-18T04:49:02.711Z"},
		{"sessionId":"gem-2","type":"user","message":"Other session","timestamp":"2025-08-18T04:50:00.000Z"}
	]`)

	entries, err := parseGeminiTranscript(transcriptInfo(t, "gem-1", file))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].Text != "Continue" {
		t.Fatalf("entries %+v, want only gem-1 messages", entries)
	}
}

func TestParseSessionDBTranscript(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, data TEXT);
		CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, time_created INTEGER, data TEXT);
		INSERT INTO message VALUES ('m1','s1',1000,'{"role":"user","time":{"created":1000}}');
		INSERT INTO message VALUES ('m2','s1',2000,'{"role":"assistant","time":{"created":2000}}');
		INSERT INTO part VALUES ('p1','m1','s1',1000,'{"type":"text","text":"List the files"}');
		INSERT INTO part VALUES ('p2','m1','s1',1000,'{"type":"text","text":"env","synthetic":true}');
		INSERT INTO part VALUES ('p3','m2','s1',2000,'{"type":"text","text":"Sure."}');
		INSERT INTO part VALUES ('p4','m2','s1',2000,'{"type":"tool","tool":"Bash","state":{"status":"completed","input":{"command":"ls"}}}');
	`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	ref := dbPath + "#s1"
	entries, err := parseSessionDBTranscript(SessionInfo{ID: "s1", FilePath: &ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("%d entries, want 3: %+v", len(entries), entries)
	}
	if entries[0].Role != RoleUser || entries[0].Text != "List the files" {
		t.Errorf("first entry %+v, want the user text with synthetic part skipped", entries[0])
	}
	if entries[2].Tools[0].Title != "ls" {
		t.Errorf("tool %+v, want the Bash command", entries[2].Tools[0])
	}
}

func TestParseSessionDBTranscriptThinkingAndPayloads(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "zcode.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, data TEXT);
		CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, time_created INTEGER, data TEXT);
		INSERT INTO message VALUES ('m1','s1',1000,'{"role":"assistant","time":{"created":1000,"completed":4000}}');
		INSERT INTO part VALUES ('p1','m1','s1',1000,'{"type":"reasoning","text":"Let me think.","time":{"start":1000,"end":3000}}');
		INSERT INTO part VALUES ('p2','m1','s1',1000,'{"type":"tool","tool":"Read","callID":"call_1","state":{"status":"completed","title":"Read","input":{"file_path":"/a/b.swift"},"output":"file body","time":{"start":3000,"end":3500}}}');
		INSERT INTO part VALUES ('p3','m1','s1',1000,'{"type":"text","text":"Done."}');
	`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	ref := dbPath + "#s1"
	entries, err := parseSessionDBTranscript(SessionInfo{ID: "s1", FilePath: &ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("%d entries, want 3: %+v", len(entries), entries)
	}
	if entries[0].Thinking != "Let me think." || entries[0].DurationMs != 2000 {
		t.Errorf("reasoning entry %+v, want thinking text and a 2000ms span", entries[0])
	}
	tool := entries[1].Tools[0]
	if tool.ID != "call_1" || tool.Input == "" || tool.Output != "file body" || entries[1].DurationMs != 500 {
		t.Errorf("tool entry %+v (dur %v), want id, input, output and a 500ms span", entries[1], entries[1].DurationMs)
	}
	if entries[2].Text != "Done." || entries[2].DurationMs != 3000 {
		t.Errorf("text entry %+v, want Done. and a 3000ms generation span", entries[2])
	}
}

func TestParseHermesTranscript(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT);
		CREATE TABLE messages (session_id TEXT, role TEXT, content TEXT, tool_calls TEXT, created_at REAL);
		INSERT INTO sessions VALUES ('h1','Fix the deploy script');
		INSERT INTO messages VALUES ('h1','user','Fix it',NULL,1793934000.5);
		INSERT INTO messages VALUES ('h1','assistant','Running it',NULL,1793934001.5);
		INSERT INTO messages VALUES ('h1','assistant','', '[{"function":{"name":"terminal","arguments":"{\"command\":\"ls\"}"}}]',1793934002.5);
		INSERT INTO messages VALUES ('h2','user','Other',NULL,1793934003.5);
	`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	ref := dbPath + "#h1"
	entries, err := parseHermesTranscript(SessionInfo{ID: "h1", FilePath: &ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].Text != "Fix it" || entries[1].Text != "Running it" {
		t.Fatalf("entries %+v, want the messages for h1", entries)
	}
	if entries[0].Timestamp < 1.7e12 {
		t.Errorf("timestamp %v, want seconds converted to ms", entries[0].Timestamp)
	}
	if entries[2].Tools[0].Title != "ls" {
		t.Errorf("tool %+v, want the Hermes tool card", entries[2].Tools[0])
	}
}

func TestParseGenericTranscript(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "workbuddy.jsonl")
	writeFile(t, file, strings.Join([]string{
		`{"type":"user_message","message":"Hello"}`,
		`{"type":"agent_message","message":"World"}`,
	}, "\n"))
	entries, err := parseGenericTranscript(transcriptInfo(t, "wb", file))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Text != "Hello" || entries[1].Text != "World" {
		t.Fatalf("entries %+v, want the generic messages", entries)
	}
}

func TestTranscriptCacheInvalidation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "s1.jsonl")
	info := transcriptInfo(t, "s1", file)

	writeFile(t, file, `{"type":"user","message":{"role":"user","content":"first"}}`)
	cache := NewCache(4)
	entries, err := cache.Load("claude", info)
	if err != nil || len(entries) != 1 || entries[0].Text != "first" {
		t.Fatalf("first load: %+v, %v", entries, err)
	}

	writeFile(t, file, strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"first"}}`,
		`{"type":"user","message":{"role":"user","content":"second"}}`,
	}, "\n"))
	entries, err = cache.Load("claude", info)
	if err != nil || len(entries) != 2 {
		t.Fatalf("second load: %d entries, %v; want the file re-read", len(entries), err)
	}
}
