package agentsession

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSessionIndexIncrementalSearchAndGet(t *testing.T) {
	home := t.TempDir()
	first := writeClaudeIndexFixture(t, home, "sess-1", "修复登录问题", "2026-11-06T02:25:32.506Z")

	index, err := OpenSessionIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	stats, err := index.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Agents != len(agents) || stats.Scanned != len(agents) || stats.Skipped != 0 || stats.Failures != 0 {
		t.Fatalf("first refresh stats = %+v", stats)
	}
	results, err := index.List(SessionIndexQuery{AgentIDs: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	r := findResult(t, results, "claude")
	if len(r.Sessions) != 1 || r.Sessions[0].ID != "sess-1" ||
		r.Sessions[0].Title == nil || *r.Sessions[0].Title != "修复登录问题" {
		t.Fatalf("indexed sessions = %+v", r.Sessions)
	}
	if r.Sessions[0].FilePath == nil || *r.Sessions[0].FilePath != first {
		t.Errorf("filePath = %v, want %q", r.Sessions[0].FilePath, first)
	}

	stats, err = index.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 0 || stats.Skipped != len(agents) || stats.Failures != 0 {
		t.Fatalf("unchanged refresh stats = %+v", stats)
	}

	results, err = index.List(SessionIndexQuery{Search: "登录"})
	if err != nil {
		t.Fatal(err)
	}
	r = findResult(t, results, "claude")
	if len(r.Sessions) != 1 {
		t.Fatalf("search returned %d sessions, want 1", len(r.Sessions))
	}
	session, err := index.Get("claude", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "sess-1" || session.Workspace != "/Users/demo/app" {
		t.Errorf("get returned %+v", *session)
	}

	second := writeClaudeIndexFixture(t, home, "sess-2", "Add SQLite index", "2026-11-07T02:25:32.506Z")
	setMtime(t, second, time.Now().Add(time.Minute))
	stats, err = index.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 1 || stats.Skipped != len(agents)-1 || stats.Failures != 0 {
		t.Fatalf("changed refresh stats = %+v", stats)
	}
	results, err = index.List(SessionIndexQuery{AgentIDs: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	r = findResult(t, results, "claude")
	if len(r.Sessions) != 2 {
		t.Fatalf("indexed sessions = %d, want 2", len(r.Sessions))
	}
}

func TestSessionIndexRefreshReportsFailureAndRetainsRows(t *testing.T) {
	home := t.TempDir()
	writeClaudeIndexFixture(t, home, "sess-1", "Keep me", "2026-11-06T02:25:32.506Z")
	index, err := OpenSessionIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if _, err := index.Refresh("claude"); err != nil {
		t.Fatal(err)
	}

	replaceClaudeProjectsWithFile(t, home)
	stats, err := index.Refresh("claude")
	if err == nil || !strings.Contains(err.Error(), "claude") {
		t.Fatalf("refresh error = %v, want a Claude scan failure", err)
	}
	if stats.Failures != 1 || stats.Scanned != 0 {
		t.Fatalf("refresh stats = %+v, want exactly one failure", stats)
	}
	session, getErr := index.Get("claude", "sess-1")
	if getErr != nil || session.Title == nil || *session.Title != "Keep me" {
		t.Fatalf("previous indexed session = %+v, %v", session, getErr)
	}
}

func TestSessionIndexScannerVersionRebuilds(t *testing.T) {
	home := t.TempDir()
	writeClaudeIndexFixture(t, home, "sess-1", "Indexed title", "2026-11-06T02:25:32.506Z")

	index, err := OpenSessionIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := index.Refresh("claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Get("claude", "sess-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := index.db.Exec("UPDATE meta SET value = '0' WHERE key = 'scanner_version'"); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}

	index, err = OpenSessionIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if _, err := index.Get("claude", "sess-1"); err == nil {
		t.Fatal("stale session survived scanner-version invalidation")
	}
	if _, err := index.Refresh("claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Get("claude", "sess-1"); err != nil {
		t.Fatalf("session did not return after rebuild: %v", err)
	}
}

// Churn in Electron caches must not invalidate the index.
func TestCindySignatureRootsPruneElectronCaches(t *testing.T) {
	def := agentDefByID("cindy")
	home := t.TempDir()
	store := filepath.Join(home, cindyStoreDirectory(runtime.GOOS))
	if def.signatureRoots == nil {
		t.Fatal("cindy must declare signatureRoots")
	}

	before, err := agentStoreSignatureFor(def, store)
	if err != nil {
		t.Fatal(err)
	}
	// Cache churn: an Electron cache directory changes constantly.
	writeFile(t, filepath.Join(store, "Cache", "data_1"), strings.Repeat("x", 4096))
	writeFile(t, filepath.Join(store, "cindy-profile.db.bak"), "backup")
	writeFile(t, filepath.Join(store, "cindy-profile.db-shm"), "shared memory")
	after, err := agentStoreSignatureFor(def, store)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("cache churn changed the Cindy store signature")
	}

	// Session data: a new owner file must invalidate.
	writeFile(t, filepath.Join(store, "owners", hash20("profile"), "boundary"), "x")
	changed, err := agentStoreSignatureFor(def, store)
	if err != nil {
		t.Fatal(err)
	}
	if before == changed {
		t.Fatal("a new owner file did not change the Cindy store signature")
	}
}

func TestSessionIndexRefreshesCindyDatabaseAndWAL(t *testing.T) {
	home := t.TempDir()
	store := filepath.Join(home, cindyStoreDirectory(runtime.GOOS))
	database := filepath.Join(store, "cindy-profile.db")
	writeFile(t, filepath.Join(store, "owners", cindyOwnerDirName("profile"), "boundary"), "owner")
	index, err := OpenSessionIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	if _, err := index.Refresh("cindy"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT, working_dir TEXT, status TEXT, hidden_at INTEGER, created_at INTEGER, updated_at INTEGER, agent_kind TEXT, sdk_session_id TEXT)`,
		`INSERT INTO sessions VALUES ('session', 'Before', '/tmp/work', 'active', NULL, 1, 2, 'pi', NULL)`,
		`PRAGMA journal_mode = WAL`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	assertTitle := func(want string) {
		t.Helper()
		if stats, err := index.Refresh("cindy"); err != nil || stats.Scanned != 1 {
			t.Fatalf("refresh stats = %+v, err = %v", stats, err)
		}
		session, err := index.Get("cindy", "session")
		if err != nil || session.Title == nil || *session.Title != want {
			t.Fatalf("indexed session = %+v, err = %v, want title %q", session, err, want)
		}
	}
	assertTitle("Before")
	if _, err := db.Exec(`UPDATE sessions SET title = 'After' WHERE id = 'session'`); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(database + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("expected a nonempty WAL after update, got %v, err = %v", info, err)
	}
	assertTitle("After")
	if stats, err := index.Refresh("cindy"); err != nil || stats.Skipped != 1 {
		t.Fatalf("unchanged refresh stats = %+v, err = %v", stats, err)
	}
}

// An agent without signatureRoots keeps signing its whole store directory.
func TestAgentStoreSignatureForDefaultsToWholeDirectory(t *testing.T) {
	def := agentDefByID("claude")
	if def.signatureRoots != nil {
		t.Fatalf("claude must not declare signatureRoots")
	}
	home := t.TempDir()
	file := writeClaudeIndexFixture(t, home, "sess-1", "t", "2026-11-06T02:25:32.506Z")
	// Pin mtimes so a watcher touching the fixture between the two walks
	// cannot make the comparison flaky.
	fixed := time.Date(2026, 11, 6, 2, 25, 32, 506000000, time.UTC)
	setMtime(t, file, fixed)
	setMtime(t, filepath.Join(home, def.directory, "projects", "-Users-demo-app"), fixed)
	setMtime(t, filepath.Join(home, def.directory, "projects"), fixed)
	setMtime(t, filepath.Join(home, def.directory), fixed)
	store := filepath.Join(home, def.directory)
	signature, err := agentStoreSignatureFor(def, store)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := agentStoreSignature(store)
	if err != nil {
		t.Fatal(err)
	}
	if signature != direct {
		t.Fatalf("default signature %q != whole-directory signature %q", signature, direct)
	}
}

func agentDefByID(id string) agentDef {
	for _, def := range agents {
		if def.id == id {
			return def
		}
	}
	panic("unknown agent " + id)
}

func hash20(s string) string { return cindyOwnerDirName(s) }

func TestSessionIndexPathOverride(t *testing.T) {
	home := t.TempDir()
	if got := SessionIndexPath(home); got != filepath.Join(home, ".agent-session-sdk", "home.sqlite") {
		t.Fatalf("default path = %q", got)
	}
	override := filepath.Join(t.TempDir(), "index.sqlite")
	t.Setenv("AGENT_SESSION_INDEX_DB", override)
	if got := SessionIndexPath(home); got != override {
		t.Fatalf("override path = %q", got)
	}
}

func writeClaudeIndexFixture(t *testing.T, home, id, title, timestamp string) string {
	t.Helper()
	file := filepath.Join(home, ".claude", "projects", "-Users-demo-app", id+".jsonl")
	writeFile(t, file, strings.Join([]string{
		`{"type":"user","cwd":"/Users/demo/app","timestamp":"` + timestamp + `","message":{"role":"user","content":[{"type":"text","text":"` + title + `"}]}}`,
	}, "\n"))
	return file
}

func setMtime(t *testing.T, path string, modified time.Time) {
	t.Helper()
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}
