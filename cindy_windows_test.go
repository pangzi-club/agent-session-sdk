package agentsession

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestCindyWindowsAppData(t *testing.T) {
	home := t.TempDir()
	appData := filepath.Join(t.TempDir(), "Redirected Roaming")
	store := filepath.Join(appData, "CindyGlobal")
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", appData)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("AGENT_SESSION_INDEX_DB", "")

	transcript := filepath.Join(home, ".claude", "projects", "--tmp-project", "claude-fixture.jsonl")
	writeFile(t, transcript, `{"type":"assistant","timestamp":"2026-11-14T03:00:01.000Z","sessionId":"claude-fixture","message":{"role":"assistant","model":"test","content":[{"type":"text","text":"Done"}],"usage":{"input_tokens":100,"output_tokens":20}}}`+"\n")
	writeFile(t, filepath.Join(store, "owners", cindyOwnerDirName("profile"), "boundary"), "owner")
	database := filepath.Join(store, "cindy-profile.db")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT, working_dir TEXT, status TEXT, hidden_at INTEGER, created_at INTEGER, updated_at INTEGER, agent_kind TEXT, sdk_session_id TEXT)`,
		`INSERT INTO sessions VALUES ('cindy-fixture', 'Redirected profile', '/tmp/project', 'active', NULL, 1, 2, 'cc', 'claude-fixture')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}

	report := ScanSessionsDetailed(home)
	if len(report.Failures) != 0 {
		t.Fatalf("session scan failed: %v", report.Failures)
	}
	sessions := findResult(t, report.Results, "cindy").Sessions
	if len(sessions) != 1 || sessions[0].FilePath == nil || *sessions[0].FilePath != transcript {
		t.Fatalf("Cindy sessions = %+v, want the redirected profile with its shared Claude transcript", sessions)
	}
	usage := ScanAllUsageDetailed(home)
	if len(usage.Failures) != 0 {
		t.Fatalf("usage scan failed: %v", usage.Failures)
	}
	found := false
	for _, record := range usage.Scan.Records {
		if record.AgentID == "cindy" && record.SessionID == "cindy-fixture" && record.InputTokens == 100 {
			found = true
		}
	}
	if !found {
		t.Fatalf("usage records = %+v, want usage from the redirected Cindy profile", usage.Scan.Records)
	}

	index, err := OpenSessionIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if _, err := index.Refresh("cindy"); err != nil {
		t.Fatal(err)
	}
	session, err := index.Get("cindy", "cindy-fixture")
	if err != nil || session.FilePath == nil || *session.FilePath != transcript {
		t.Fatalf("indexed Cindy session = %+v, err = %v, want the redirected profile", session, err)
	}

	altHome := t.TempDir()
	if sessions := findResult(t, ScanSessions(altHome), "cindy").Sessions; len(sessions) != 0 {
		t.Fatalf("alternate home picked up the current user's Cindy sessions: %+v", sessions)
	}

	t.Setenv("APPDATA", "")
	def := agentDefByID("cindy")
	if got, want := agentStorePath(home, def), filepath.Join(home, "AppData", "Roaming", "CindyGlobal"); got != want {
		t.Fatalf("Cindy fallback path = %q, want %q", got, want)
	}
	if _, err := index.Refresh("cindy"); err != nil {
		t.Fatal(err)
	}
	results, err := index.List(SessionIndexQuery{AgentIDs: []string{"cindy"}})
	if err != nil {
		t.Fatal(err)
	}
	if sessions := findResult(t, results, "cindy").Sessions; len(sessions) != 0 {
		t.Fatalf("index kept Cindy sessions after switching store: %+v", sessions)
	}
}
