package agentsession

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionIndexSchemaVersion  = 1
	sessionIndexScannerVersion = 2
)

// SessionIndex is a derived SQLite index over an effective home directory. The
// source stores remain authoritative; Refresh only re-runs an agent scanner
// when that agent's store file signature changes.
type SessionIndex struct {
	db   *sql.DB
	home string
}

// SessionIndexQuery selects indexed sessions. Empty AgentIDs means all agents,
// and a non-positive Limit means unlimited. Search matches title, workspace,
// session id and agent id as a substring (including CJK substrings).
type SessionIndexQuery struct {
	AgentIDs []string
	Search   string
	Limit    int
	Offset   int
}

// RefreshStats reports how many registered agents were considered and how many
// were re-scanned because their store signature changed.
type RefreshStats struct {
	Agents   int `json:"agents"`
	Scanned  int `json:"scanned"`
	Skipped  int `json:"skipped"`
	Failures int `json:"failures"`
}

// SessionIndexPath returns <home>/.agent-session-sdk/home.sqlite. AGENT_SESSION_INDEX_DB
// overrides it for callers that want a test or explicitly shared database.
func SessionIndexPath(home string) string {
	if path := strings.TrimSpace(os.Getenv("AGENT_SESSION_INDEX_DB")); path != "" {
		return path
	}
	return filepath.Join(home, ".agent-session-sdk", "home.sqlite")
}

// OpenSessionIndex opens (and creates if needed) the index for home.
func OpenSessionIndex(home string) (*SessionIndex, error) {
	return OpenSessionIndexAtPath(home, SessionIndexPath(home))
}

// OpenSessionIndexAtPath opens an index at an explicit path. An incompatible
// derived index is rebuilt; source session stores are never modified.
func OpenSessionIndexAtPath(home, dbPath string) (*SessionIndex, error) {
	home = filepath.Clean(home)
	dbPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	if err := ensureSessionIndexSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &SessionIndex{db: db, home: home}, nil
}

// Close releases the SQLite connection.
func (index *SessionIndex) Close() error {
	if index == nil || index.db == nil {
		return nil
	}
	return index.db.Close()
}

// Refresh updates changed agents. With no agentIDs every registered agent is
// checked. Scanner failures leave an agent's old indexed rows and signature
// intact so the next Refresh retries that agent.
func (index *SessionIndex) Refresh(agentIDs ...string) (RefreshStats, error) {
	selected, err := selectAgentDefs(agentIDs)
	if err != nil {
		return RefreshStats{}, err
	}
	stats := RefreshStats{Agents: len(selected)}

	type scanResult struct {
		def          agentDef
		before       string
		after        string
		skipped      bool
		missingStore bool
		signatureErr error
		sessions     []SessionInfo
		scanErr      error
	}
	results := make([]scanResult, len(selected))
	var wg sync.WaitGroup
	for i, def := range selected {
		storeRoot := filepath.Join(index.home, def.directory)
		result := scanResult{def: def}
		result.before, result.signatureErr = agentStoreSignature(storeRoot)
		results[i] = result
		if result.signatureErr != nil {
			stats.Failures++
			continue
		}
		known, err := index.agentSignatureMatches(def.id, result.before)
		if err != nil {
			return stats, err
		}
		if known {
			results[i].skipped = true
			stats.Skipped++
			continue
		}
		if storeRootAbsent(storeRoot) {
			results[i].missingStore = true
			results[i].after = result.before
			continue
		}
		wg.Add(1)
		go func(i int, def agentDef, before string) {
			defer wg.Done()
			result := scanResult{def: def, before: before}
			storeRoot := filepath.Join(index.home, def.directory)
			result.sessions, result.scanErr = def.scan(storeRoot)
			result.after, _ = agentStoreSignature(storeRoot)
			if result.after == "" {
				result.after = before
			}
			results[i] = result
		}(i, def, result.before)
	}
	wg.Wait()

	for _, result := range results {
		if result.skipped {
			continue
		}
		if result.signatureErr != nil || result.scanErr != nil {
			if result.scanErr != nil {
				stats.Failures++
			}
			continue
		}
		signature := result.after
		if signature == "" {
			signature = result.before
		}
		sessions := result.sessions
		if result.missingStore {
			sessions = []SessionInfo{}
		}
		if err := index.replaceAgentSessions(result.def.id, signature, sessions); err != nil {
			return stats, err
		}
		stats.Scanned++
	}
	return stats, nil
}

// List returns indexed sessions grouped by registered agent order.
func (index *SessionIndex) List(query SessionIndexQuery) ([]SessionScanResult, error) {
	rows, err := index.querySessions(query)
	if err != nil {
		return nil, err
	}
	byAgent := map[string][]SessionInfo{}
	for _, row := range rows {
		byAgent[row.agentID] = append(byAgent[row.agentID], row.session)
	}
	out := make([]SessionScanResult, 0, len(agents))
	for _, def := range agents {
		if !agentSelected(query.AgentIDs, def.id) {
			continue
		}
		canDelete := def.canDelete
		out = append(out, SessionScanResult{
			AgentID:           def.id,
			AgentName:         def.name,
			Sessions:          withEmptySessions(byAgent[def.id]),
			CanDeleteSessions: &canDelete,
		})
	}
	return out, nil
}

// Get returns one indexed session.
func (index *SessionIndex) Get(agentID, sessionID string) (*SessionInfo, error) {
	sessions, err := index.querySessions(SessionIndexQuery{AgentIDs: []string{agentID}})
	if err != nil {
		return nil, err
	}
	for _, row := range sessions {
		if row.session.ID == sessionID {
			value := row.session
			return &value, nil
		}
	}
	return nil, fmt.Errorf("session %q not found for agent %q", sessionID, agentID)
}

type indexedSession struct {
	agentID string
	session SessionInfo
}

func (index *SessionIndex) querySessions(query SessionIndexQuery) ([]indexedSession, error) {
	where := []string{}
	args := []any{}
	if len(query.AgentIDs) != 0 {
		placeholders := make([]string, 0, len(query.AgentIDs))
		for _, id := range query.AgentIDs {
			placeholders = append(placeholders, "?")
			args = append(args, id)
		}
		where = append(where, "s.agent_id IN ("+strings.Join(placeholders, ",")+")")
	}
	if search := strings.TrimSpace(query.Search); search != "" {
		pattern := "%" + escapeSQLiteLike(search) + "%"
		where = append(where, "(s.title LIKE ? ESCAPE '\\' OR s.workspace LIKE ? ESCAPE '\\' OR s.id LIKE ? ESCAPE '\\' OR s.agent_id LIKE ? ESCAPE '\\')")
		args = append(args, pattern, pattern, pattern, pattern)
	}
	statement := "SELECT s.agent_id, s.id, s.workspace, s.title, s.blank, s.created_at, s.updated_at, s.size_bytes, s.file_path, s.token_usage, s.transcript_format FROM sessions s"
	if len(where) != 0 {
		statement += " WHERE " + strings.Join(where, " AND ")
	}
	statement += " ORDER BY s.updated_at DESC, s.agent_id COLLATE NOCASE, s.id COLLATE NOCASE"
	if query.Limit > 0 {
		statement += " LIMIT ? OFFSET ?"
		args = append(args, query.Limit, max(0, query.Offset))
	}
	rows, err := index.db.Query(statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []indexedSession{}
	for rows.Next() {
		var item indexedSession
		var title, filePath, tokenUsage *string
		var blank *bool
		var transcriptFormat string
		if err := rows.Scan(&item.agentID, &item.session.ID, &item.session.Workspace,
			&title, &blank, &item.session.CreatedAt, &item.session.UpdatedAt,
			&item.session.SizeBytes, &filePath, &tokenUsage, &transcriptFormat); err != nil {
			return nil, err
		}
		item.session.Title = title
		item.session.Blank = blank
		item.session.transcriptFormat = transcriptFormat
		if filePath != nil && *filePath != "" {
			item.session.FilePath = filePath
		}
		if tokenUsage != nil && *tokenUsage != "" {
			values := [5]float64{}
			if json.Unmarshal([]byte(*tokenUsage), &values) == nil {
				item.session.TokenUsage = &values
			}
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (index *SessionIndex) agentSignatureMatches(agentID, signature string) (bool, error) {
	var known string
	err := index.db.QueryRow("SELECT signature FROM agent_state WHERE agent_id = ?", agentID).Scan(&known)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && known == signature, err
}

func (index *SessionIndex) replaceAgentSessions(agentID, signature string, sessions []SessionInfo) error {
	tx, err := index.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM sessions WHERE agent_id = ?", agentID); err != nil {
		return err
	}
	for _, session := range sessions {
		if session.ID == "" {
			continue
		}
		var tokenUsage any
		if session.TokenUsage != nil {
			raw, err := json.Marshal(session.TokenUsage)
			if err != nil {
				return err
			}
			tokenUsage = string(raw)
		}
		var filePath any
		if session.FilePath != nil {
			filePath = *session.FilePath
		}
		var blank any
		if session.Blank != nil {
			blank = *session.Blank
		}
		if _, err := tx.Exec(`INSERT INTO sessions
			(agent_id, id, workspace, title, blank, created_at, updated_at, size_bytes, file_path, token_usage, transcript_format)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			agentID, session.ID, session.Workspace, session.Title, blank,
			session.CreatedAt, session.UpdatedAt, session.SizeBytes, filePath, tokenUsage, session.transcriptFormat); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO agent_state(agent_id, signature, refreshed_at) VALUES (?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET signature = excluded.signature, refreshed_at = excluded.refreshed_at`,
		agentID, signature, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func ensureSessionIndexSchema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS meta(
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`); err != nil {
		return err
	}
	var userVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		return err
	}
	var schemaVersion, scannerVersion sql.NullString
	_ = db.QueryRow("SELECT value FROM meta WHERE key = 'schema_version'").Scan(&schemaVersion)
	_ = db.QueryRow("SELECT value FROM meta WHERE key = 'scanner_version'").Scan(&scannerVersion)
	compatible := (userVersion == 0 || userVersion == sessionIndexSchemaVersion) &&
		(!schemaVersion.Valid || schemaVersion.String == strconv.Itoa(sessionIndexSchemaVersion)) &&
		(!scannerVersion.Valid || scannerVersion.String == strconv.Itoa(sessionIndexScannerVersion))
	if !compatible {
		if _, err := db.Exec("DROP TABLE IF EXISTS sessions_fts"); err != nil {
			return err
		}
		if _, err := db.Exec("DROP TABLE IF EXISTS sessions"); err != nil {
			return err
		}
		if _, err := db.Exec("DROP TABLE IF EXISTS agent_state"); err != nil {
			return err
		}
	}

	statements := []string{
		`CREATE TABLE IF NOT EXISTS agent_state(
			agent_id TEXT PRIMARY KEY,
			signature TEXT NOT NULL,
			refreshed_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sessions(
			agent_id TEXT NOT NULL,
			id TEXT NOT NULL,
			workspace TEXT NOT NULL,
			title TEXT,
			blank INTEGER,
			created_at REAL NOT NULL,
			updated_at REAL NOT NULL,
			size_bytes REAL NOT NULL,
			file_path TEXT,
			token_usage TEXT,
			transcript_format TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(agent_id, id)
		)`,
		`CREATE INDEX IF NOT EXISTS sessions_updated_at ON sessions(updated_at DESC)`,
		`PRAGMA user_version = ` + strconv.Itoa(sessionIndexSchemaVersion),
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("INSERT OR REPLACE INTO meta(key, value) VALUES('schema_version', ?)",
		sessionIndexSchemaVersion); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT OR REPLACE INTO meta(key, value) VALUES('scanner_version', ?)",
		sessionIndexScannerVersion); err != nil {
		return err
	}
	return tx.Commit()
}

func agentStoreSignature(root string) (string, error) {
	hasher := sha256.New()
	if _, err := os.Lstat(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(hasher, "missing\x00")
			return hex.EncodeToString(hasher.Sum(nil)), nil
		}
		return "", err
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(hasher, "%s\x00%d\x00%d\x00\n",
			filepath.ToSlash(relative), info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func storeRootAbsent(root string) bool {
	_, err := os.Stat(root)
	return errors.Is(err, os.ErrNotExist)
}

func selectAgentDefs(agentIDs []string) ([]agentDef, error) {
	if len(agentIDs) == 0 {
		return agents, nil
	}
	byID := map[string]agentDef{}
	for _, def := range agents {
		byID[def.id] = def
	}
	out := make([]agentDef, 0, len(agentIDs))
	for _, id := range agentIDs {
		def, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("unknown agent: %s", id)
		}
		out = append(out, def)
	}
	return out, nil
}

func agentSelected(selected []string, id string) bool {
	if len(selected) == 0 {
		return true
	}
	for _, candidate := range selected {
		if candidate == id {
			return true
		}
	}
	return false
}

func withEmptySessions(sessions []SessionInfo) []SessionInfo {
	if sessions == nil {
		return []SessionInfo{}
	}
	return sessions
}

func escapeSQLiteLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}
