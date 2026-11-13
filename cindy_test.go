package agentsession

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanCindy(t *testing.T) {
	home := t.TempDir()
	globalDir := filepath.Join(home, "Library", "Application Support", "CindyGlobal")
	dataOwnerID := "cmwd-profile"
	database := filepath.Join(globalDir, "cindy-"+dataOwnerID+".db")
	ownerDir := filepath.Join(globalDir, "owners", cindyOwnerDirName(dataOwnerID))
	transcript := filepath.Join(globalDir, "pi-agent-home", "sessions", "pi-session.jsonl")

	writeFile(t, transcript, strings.Join([]string{
		`{"type":"session","version":3,"id":"pi-internal","timestamp":"2026-11-12T05:31:02.066Z","cwd":"` + ownerDir + `"}`,
		`{"type":"message","timestamp":1790000000000,"message":{"role":"user","content":[{"type":"text","text":"Check Cindy"}]}}`,
		`{"type":"message","timestamp":1790000001000,"message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"usage":{"input":10,"output":5,"cacheRead":3,"cacheWrite":2}}}`,
	}, "\n"))
	writeFile(t, filepath.Join(ownerDir, "boundary"), "owner")

	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT, working_dir TEXT, status TEXT, hidden_at INTEGER, created_at INTEGER, updated_at INTEGER, sdk_session_id TEXT)`,
		`INSERT INTO sessions VALUES ('hidden', 'Hidden', '` + ownerDir + `', 'active', 1, 1, 2, NULL)`,
		`INSERT INTO sessions VALUES ('deleted', 'Deleted', '` + ownerDir + `', 'deleted', NULL, 1, 2, NULL)`,
		`INSERT INTO sessions VALUES ('s1', 'Stored title', '` + ownerDir + `', 'active', NULL, 1790000000000, 1790000002000, '` + transcript + `')`,
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
	if len(sessions) != 1 {
		t.Fatalf("%d sessions, want 1", len(sessions))
	}
	session := sessions[0]
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
	if len(usage.records) != 1 {
		t.Fatalf("%d usage records, want 1", len(usage.records))
	}
	record := usage.records[0]
	if record.SessionID != "s1" || record.AgentID != "cindy" ||
		record.InputTokens != 10 || record.OutputTokens != 5 ||
		record.CacheReadTokens != 3 || record.CacheWriteTokens != 2 {
		t.Fatalf("usage record %+v, want Cindy session usage", record)
	}
}
