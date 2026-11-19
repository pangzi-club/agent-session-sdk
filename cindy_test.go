package agentsession

import (
	"database/sql"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The store directory follows Electron's appData convention per platform;
// Windows is where the previous macOS-only path made Cindy invisible.
func TestCindyStoreDirectory(t *testing.T) {
	if got := cindyStoreDirectory("windows"); got != `AppData/Roaming/CindyGlobal` {
		t.Fatalf("windows store dir = %q", got)
	}
	if got := cindyStoreDirectory("linux"); got != `.config/CindyGlobal` {
		t.Fatalf("linux store dir = %q", got)
	}
	if got := cindyStoreDirectory("darwin"); got != `Library/Application Support/CindyGlobal` {
		t.Fatalf("darwin store dir = %q", got)
	}
	if cindyStoreDirectory(runtime.GOOS) != agents[6].directory {
		t.Fatalf("registry cindy directory %q does not match cindyStoreDirectory", agents[6].directory)
	}
}

// Legacy rows store POSIX transcript paths; on Windows filepath.IsAbs is
// false for them, yet they must still be treated as paths, not session ids.
func TestCindyStoredPathIsAbsolute(t *testing.T) {
	for _, path := range []string{
		`/Users/demo/Library/Application Support/CindyGlobal/pi-agent-home/sessions/s.jsonl`,
		`C:\Users\demo\AppData\Roaming\CindyGlobal\pi-agent-home\sessions\s.jsonl`,
	} {
		if !cindyStoredPathIsAbsolute(path) {
			t.Errorf("stored path %q not detected as absolute", path)
		}
	}
	for _, id := range []string{"", "d7294fcd-bf22-4371-86a4-c9337edc2421", "codex-internal", "relative/path.jsonl"} {
		if cindyStoredPathIsAbsolute(id) {
			t.Errorf("session id %q misdetected as absolute path", id)
		}
	}
}

func TestCindyClaudeTranscriptCandidates(t *testing.T) {
	home := t.TempDir()
	globalDir := filepath.Join(home, filepath.FromSlash(cindyStoreDirectory(runtime.GOOS)))
	workspace := "/tmp/work.space"
	custom := filepath.Join(home, "custom-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", custom)
	file := filepath.Join(custom, "projects", "-tmp-work-space", "claude-id.jsonl")
	writeFile(t, file, "{}\n")
	if got := cindyClaudeTranscript(globalDir, &workspace, "claude-id"); got != file {
		t.Fatalf("explicit Claude config path = %q, want %q", got, file)
	}

	legacy := filepath.Join(globalDir, "claude-home", "projects", "moved-project", "legacy-id.jsonl")
	writeFile(t, legacy, "{}\n")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got := cindyClaudeTranscript(globalDir, &workspace, "legacy-id"); got != legacy {
		t.Fatalf("legacy Claude fallback = %q, want %q", got, legacy)
	}
}

func TestScanCindy(t *testing.T) {
	home := t.TempDir()
	globalDir := filepath.Join(home, filepath.FromSlash(cindyStoreDirectory(runtime.GOOS)))
	dataOwnerID := "cmwd-profile"
	database := filepath.Join(globalDir, "cindy-"+dataOwnerID+".db")
	ownerDir := filepath.Join(globalDir, "owners", cindyOwnerDirName(dataOwnerID))
	transcript := filepath.Join(globalDir, "pi-agent-home", "sessions", "pi-session.jsonl")
	codexTranscript := filepath.Join(globalDir, "codex-home", "sessions", "2026", "11", "14", "rollout-codex-internal.jsonl")
	claudeTranscript := filepath.Join(home, ".claude", "projects", "--tmp-project", "claude-internal.jsonl")

	writeFile(t, transcript, strings.Join([]string{
		`{"type":"session","version":3,"id":"pi-internal","timestamp":"2026-11-12T05:31:02.066Z","cwd":"` + ownerDir + `"}`,
		`{"type":"message","timestamp":1790000000000,"message":{"role":"user","content":[{"type":"text","text":"Check Cindy"}]}}`,
		`{"type":"message","timestamp":1790000001000,"message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"usage":{"input":10,"output":5,"cacheRead":3,"cacheWrite":2}}}`,
	}, "\n"))
	writeFile(t, claudeTranscript, strings.Join([]string{
		`{"type":"user","timestamp":"2026-11-14T03:00:00.000Z","cwd":"/tmp/project","message":{"role":"user","content":[{"type":"text","text":"Check Claude"}]}}`,
		`{"type":"assistant","timestamp":"2026-11-14T03:00:01.000Z","sessionId":"claude-internal","message":{"role":"assistant","model":"claude-test","content":[{"type":"text","text":"Done."}],"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":4}}}`,
	}, "\n"))
	writeFile(t, codexTranscript, strings.Join([]string{
		`{"timestamp":"2026-11-14T02:00:00.000Z","type":"turn_context","payload":{"model":"codex-test"}}`,
		`{"timestamp":"2026-11-14T02:00:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Implement it"}]}}`,
		`{"timestamp":"2026-11-14T02:00:01.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}}`,
		`{"timestamp":"2026-11-14T02:00:02.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":15,"cached_input_tokens":40,"cache_write_input_tokens":3}}}}`,
	}, "\n"))
	writeFile(t, filepath.Join(ownerDir, "boundary"), "owner")
	// A renamed backup database must not be treated as a profile candidate;
	// its stale mtime would otherwise lose anyway, but the name check makes it
	// deterministic.
	writeFile(t, database+".bak.2026-11-18T06-11-29-538Z", "backup")

	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT, working_dir TEXT, status TEXT, hidden_at INTEGER, created_at INTEGER, updated_at INTEGER, agent_kind TEXT, sdk_session_id TEXT)`,
		`INSERT INTO sessions VALUES ('hidden', 'Hidden', '` + ownerDir + `', 'active', 1, 1, 2, 'cc', NULL)`,
		`INSERT INTO sessions VALUES ('deleted', 'Deleted', '` + ownerDir + `', 'deleted', NULL, 1, 2, 'cc', NULL)`,
		`INSERT INTO sessions VALUES ('s1', 'Stored title', '` + ownerDir + `', 'active', NULL, 1790000000000, 1790000002000, 'cc', '` + transcript + `')`,
		`INSERT INTO sessions VALUES ('s2', 'Codex title', '/tmp/project', 'active', NULL, 1790000003000, 1790000004000, 'codex', 'codex-internal')`,
		`INSERT INTO sessions VALUES ('s3', 'Claude title', '/tmp/project', 'active', NULL, 1790000005000, 1790000006000, 'cc', 'claude-internal')`,
		`INSERT INTO sessions VALUES ('stale', 'Stale transcript', '/tmp/gone', 'active', NULL, 1789999000000, 1789999001000, 'pi', '` + filepath.Join(globalDir, "pi-agent-home", "sessions", "deleted-elsewhere.jsonl") + `')`,
		// Windows shape: sdk_session_id stores a POSIX path that
		// filepath.IsAbs rejects, yet it is still a transcript path.
		`INSERT INTO sessions VALUES ('posix', 'Windows legacy path', '` + ownerDir + `', 'active', NULL, 1790000007000, 1790000008000, 'pi', '` + filepath.ToSlash(transcript) + `')`,
		// Remote-agent shape: the Claude transcript stayed on the other
		// device, but the messages mirror backs the session locally.
		`INSERT INTO sessions VALUES ('remote', 'Remote Claude session', '/tmp/remote', 'active', NULL, 1790000009000, 1790000010000, 'cc', 'd7294fcd-bf22-4371-86a4-c9337edc2421')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	// The messages mirror: one remote cc session with the full turn mix, one
	// rewound row that must not surface, and an ask_user card that must not
	// become a chat turn.
	for _, statement := range []string{
		`CREATE TABLE messages (id TEXT PRIMARY KEY, client_id TEXT, session_id TEXT, role TEXT, content TEXT, tool_use_id TEXT, created_at INTEGER, agent_meta TEXT, rewind_at INTEGER, agent_kind TEXT)`,
		`INSERT INTO messages VALUES ('m1', NULL, 'remote', 'user', '{"text":"克隆仓库","images":[],"files":[]}', NULL, 1790000009000, NULL, NULL, 'cc')`,
		`INSERT INTO messages VALUES ('m2', NULL, 'remote', 'thinking', '{"kind":"thinking","text":"先看环境","durationMs":27744,"finishedAt":1790000035968}', NULL, 1790000010000, NULL, NULL, 'cc')`,
		`INSERT INTO messages VALUES ('m3', NULL, 'remote', 'tool_use', '{"toolUseId":"call-1","toolName":"Bash","input":{"command":"git clone https://example.invalid/repo.git","description":"克隆仓库"}}', 'call-1', 1790000020000, NULL, NULL, 'cc')`,
		`INSERT INTO messages VALUES ('m4', NULL, 'remote', 'tool_result', 'Cloned into repo', 'call-1', 1790000030000, NULL, NULL, 'cc')`,
		`INSERT INTO messages VALUES ('m5', NULL, 'remote', 'tool_use', '{"toolUseId":"call-2","toolName":"Read","input":{"file_path":"/tmp/repo/go.mod"}}', 'call-2', 1790000040000, NULL, NULL, 'cc')`,
		`INSERT INTO messages VALUES ('m6', NULL, 'remote', 'tool_result', '<tool_use_error>no such file</tool_use_error>', 'call-2', 1790000050000, NULL, NULL, 'cc')`,
		`INSERT INTO messages VALUES ('m7', NULL, 'remote', 'assistant', '克隆完成', NULL, 1790000060000, NULL, NULL, 'cc')`,
		`INSERT INTO messages VALUES ('m8', NULL, 'remote', 'user', 'rewound away', NULL, 1790000070000, NULL, 1790000070000, 'cc')`,
		`INSERT INTO messages VALUES ('m9', NULL, 'remote', 'ask_user', '{"requestId":"q1","questions":[{"question":"继续吗"}]}', NULL, 1790000080000, NULL, NULL, 'cc')`,
		`INSERT INTO messages VALUES ('m10', NULL, 'remote', 'error', '{"message":"[PI_IMAGE_INPUT_UNSUPPORTED] unsupported","dismissed":true}', NULL, 1790000090000, NULL, NULL, 'cc')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if database, owner := activeCindyProfile(globalDir); database != filepath.Join(globalDir, "cindy-"+dataOwnerID+".db") || owner != ownerDir {
		t.Fatalf("active profile = %q/%q, want %q/%q", database, owner, filepath.Join(globalDir, "cindy-"+dataOwnerID+".db"), ownerDir)
	}

	sessions, err := ScanCindy(globalDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 6 {
		t.Fatalf("%d sessions, want 6", len(sessions))
	}

	// The remote session falls back to the messages mirror: a db#session-id
	// reference, the cindy-db format and a nonzero mirror size.
	remote := sessions[0]
	if remote.ID != "remote" || remote.transcriptFormat != "cindy-db" ||
		remote.FilePath == nil || *remote.FilePath != database+"#remote" ||
		remote.SizeBytes == 0 {
		t.Fatalf("session %+v, want the messages-mirror database reference", remote)
	}
	remoteEntries, err := LoadTranscript("cindy", remote)
	if err != nil {
		t.Fatal(err)
	}
	if len(remoteEntries) != 6 {
		t.Fatalf("%d mirror entries, want 6 (user, thinking, two tools, reply, error)", len(remoteEntries))
	}
	if remoteEntries[0].Role != RoleUser || remoteEntries[0].Text != "克隆仓库" {
		t.Fatalf("mirror entry 0 = %+v, want the unwrapped user text", remoteEntries[0])
	}
	if remoteEntries[1].Thinking != "先看环境" || remoteEntries[1].DurationMs != 27744 {
		t.Fatalf("mirror entry 1 = %+v, want the thinking text and duration", remoteEntries[1])
	}
	firstTool := remoteEntries[2].Tools[0]
	if firstTool.Name != "Bash" || firstTool.ID != "call-1" ||
		firstTool.Output != "Cloned into repo" || firstTool.Failed {
		t.Fatalf("first tool call = %+v, want its matched result", firstTool)
	}
	secondTool := remoteEntries[3].Tools[0]
	if secondTool.Name != "Read" || !secondTool.Failed || secondTool.Output == "" {
		t.Fatalf("second tool call = %+v, want the failed result", secondTool)
	}
	if remoteEntries[4].Text != "克隆完成" {
		t.Fatalf("mirror entry 4 = %+v, want the assistant reply", remoteEntries[4])
	}
	if remoteEntries[5].Role != RoleSystem || !strings.Contains(remoteEntries[5].Text, "PI_IMAGE_INPUT_UNSUPPORTED") {
		t.Fatalf("mirror entry 5 = %+v, want the engine error as a system notice", remoteEntries[5])
	}

	posixSession := sessions[1]
	if posixSession.ID != "posix" || posixSession.transcriptFormat != "pi" ||
		posixSession.FilePath == nil || filepath.Clean(*posixSession.FilePath) != filepath.Clean(transcript) {
		t.Fatalf("session %+v, want the POSIX-stored path resolved to its transcript", posixSession)
	}
	claudeSession := sessions[2]
	if claudeSession.ID != "s3" || claudeSession.Workspace != "/tmp/project" || claudeSession.transcriptFormat != "claude" ||
		claudeSession.Title == nil || *claudeSession.Title != "Claude title" ||
		claudeSession.SizeBytes == 0 ||
		claudeSession.FilePath == nil || *claudeSession.FilePath != claudeTranscript {
		t.Fatalf("session %+v, want stored metadata and its Claude transcript", claudeSession)
	}

	claudeEntries, err := LoadTranscript("cindy", claudeSession)
	if err != nil {
		t.Fatal(err)
	}
	if len(claudeEntries) != 2 || claudeEntries[0].Text != "Check Claude" || claudeEntries[1].Text != "Done." {
		t.Fatalf("Claude entries %+v, want its visible turns", claudeEntries)
	}

	codexSession := sessions[3]
	if codexSession.ID != "s2" || codexSession.Workspace != "/tmp/project" ||
		codexSession.Title == nil || *codexSession.Title != "Codex title" ||
		codexSession.SizeBytes == 0 ||
		codexSession.FilePath == nil || *codexSession.FilePath != codexTranscript {
		t.Fatalf("session %+v, want stored metadata and its Codex transcript", codexSession)
	}

	codexEntries, err := LoadTranscript("cindy", codexSession)
	if err != nil {
		t.Fatal(err)
	}
	if len(codexEntries) != 2 || codexEntries[0].Text != "Implement it" || codexEntries[1].Text != "Done." {
		t.Fatalf("Codex entries %+v, want its visible prompt", codexEntries)
	}

	session := sessions[4]
	if session.ID != "s1" || session.Workspace != ownerDir ||
		session.Title == nil || *session.Title != "Stored title" ||
		session.CreatedAt != 1790000000000 || session.UpdatedAt != 1790000002000 ||
		session.FilePath == nil || *session.FilePath != transcript {
		t.Fatalf("session %+v, want stored metadata and its Pi transcript", session)
	}

	entries, err := LoadTranscript("cindy", session)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Text != "Check Cindy" || entries[1].Text != "Done." {
		t.Fatalf("entries %+v, want both visible turns", entries)
	}

	usage, err := scanCindyUsage(globalDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage.sessions) != 6 {
		t.Fatalf("%d usage sessions, want 6 (the stale-transcript row must not fail the module)", len(usage.sessions))
	}
	if len(usage.records) != 4 {
		t.Fatalf("%d usage records, want one per engine", len(usage.records))
	}
	bySession := map[string]UsageRecord{}
	for _, record := range usage.records {
		bySession[record.SessionID] = record
	}
	if record := bySession["s3"]; record.AgentID != "cindy" || record.InputTokens != 100 || record.OutputTokens != 20 ||
		record.CacheReadTokens != 30 || record.CacheWriteTokens != 4 || record.Model == nil || *record.Model != "claude-test" {
		t.Fatalf("Claude usage record %+v", record)
	}
	if record := bySession["s2"]; record.AgentID != "cindy" || record.InputTokens != 60 || record.OutputTokens != 15 ||
		record.CacheReadTokens != 40 || record.CacheWriteTokens != 3 || record.Model == nil || *record.Model != "codex-test" {
		t.Fatalf("Codex usage record %+v", record)
	}
	record := bySession["s1"]
	if record.AgentID != "cindy" ||
		record.InputTokens != 10 || record.OutputTokens != 5 ||
		record.CacheReadTokens != 3 || record.CacheWriteTokens != 2 {
		t.Fatalf("usage record %+v, want Cindy session usage", record)
	}

	index, err := OpenSessionIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if _, err := index.Refresh("cindy"); err != nil {
		t.Fatal(err)
	}
	indexed, err := index.Get("cindy", "s2")
	if err != nil {
		t.Fatal(err)
	}
	entries, err = LoadTranscript("cindy", *indexed)
	if err != nil || len(entries) != 2 {
		t.Fatalf("indexed Codex transcript = %+v, %v", entries, err)
	}
}
