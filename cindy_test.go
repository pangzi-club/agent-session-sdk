package agentsession

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestCindyClaudeTranscriptCandidates(t *testing.T) {
	home := t.TempDir()
	globalDir := filepath.Join(home, "Library", "Application Support", "CindyGlobal")
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
	globalDir := filepath.Join(home, "Library", "Application Support", "CindyGlobal")
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
	if len(sessions) != 3 {
		t.Fatalf("%d sessions, want 3", len(sessions))
	}
	claudeSession := sessions[0]
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

	codexSession := sessions[1]
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

	session := sessions[2]
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
	if len(usage.records) != 3 {
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
