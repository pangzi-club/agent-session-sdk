package agentsession

// cindy.go reads Cindy's local profile database and its Pi, Codex or Claude Code
// session transcripts. Sessions are stored in <CindyGlobal>/cindy-<dataOwnerId>.db,
// while visible turns live in the transcript file recorded by sdk_session_id.
// The owner directory is keyed by the first 20 hex characters of
// SHA-256(dataOwnerId), so selecting the newest profile database and its
// matching owner directory preserves Cindy's account boundary.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func ScanCindy(agentDir string) ([]SessionInfo, error) {
	database, ownerDir := activeCindyProfile(agentDir)
	if database == "" || ownerDir == "" {
		return []SessionInfo{}, nil
	}

	db, err := OpenSQLite(database)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := QueryMaps(db, "SELECT * FROM sessions ORDER BY updated_at DESC")
	if err != nil {
		return nil, err
	}

	out := []SessionInfo{}
	for _, row := range rows {
		if isCindyHidden(row) {
			continue
		}
		id := AsString(row["id"])
		workspace := AsString(row["working_dir"])
		if id == nil || *id == "" || workspace == nil || *workspace == "" {
			continue
		}

		createdAt := FirstOr(0.0, AsEpochMs(row["created_at"]))
		updatedAt := FirstOr(createdAt, AsEpochMs(row["updated_at"]))
		file := cindyTranscript(agentDir, workspace, AsString(row["agent_kind"]), row["sdk_session_id"])
		out = append(out, SessionInfo{
			transcriptFormat: cindyTranscriptFormat(FirstOr("", AsString(row["agent_kind"])), FirstOr("", AsString(row["sdk_session_id"]))),
			ID:               *id,
			Workspace:        *workspace,
			Title:            ToTitle(row["title"]),
			CreatedAt:        createdAt,
			UpdatedAt:        updatedAt,
			SizeBytes:        FileSize(file),
			FilePath:         filePathOrNil(file),
		})
	}
	return out, nil
}

// activeCindyProfile picks the profile database touched most recently. Cindy
// owns one database per dataOwnerId and the matching owner directory proves
// that the profile's local namespace exists on this machine.
func activeCindyProfile(agentDir string) (string, string) {
	databases := ListFiles(agentDir, ".db", false)
	var database string
	var databaseActivity time.Time
	for _, candidate := range databases {
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		activity := cindyProfileActivity(candidate, info)
		if database == "" || activity.After(databaseActivity) {
			database = candidate
			databaseActivity = activity
		}
	}
	if database == "" {
		return "", ""
	}

	dataOwnerID := strings.TrimPrefix(strings.TrimSuffix(filepath.Base(database), ".db"), "cindy-")
	if dataOwnerID == "" {
		return "", ""
	}
	ownerDir := filepath.Join(agentDir, "owners", cindyOwnerDirName(dataOwnerID))
	if info, err := os.Stat(ownerDir); err != nil || !info.IsDir() {
		return "", ""
	}
	return database, ownerDir
}

// Cindy may leave recent session metadata in WAL before a checkpoint, so
// profile activity includes the database and its WAL timestamps.
func cindyProfileActivity(database string, info os.FileInfo) time.Time {
	activity := info.ModTime()
	if wal, err := os.Stat(database + "-wal"); err == nil && wal.ModTime().After(activity) {
		return wal.ModTime()
	}
	return activity
}

func cindyOwnerDirName(dataOwnerID string) string {
	sum := sha256.Sum256([]byte(dataOwnerID))
	return hex.EncodeToString(sum[:])[:20]
}

func isCindyHidden(row map[string]any) bool {
	if row["hidden_at"] != nil {
		return true
	}
	if status := AsString(row["status"]); status != nil {
		return *status == "deleted" || *status == "hidden"
	}
	return false
}

func cindyTranscript(agentDir string, workspace, agentKind *string, value any) string {
	file := AsString(value)
	if file == nil || *file == "" {
		return ""
	}
	if agentKind == nil || filepath.IsAbs(*file) {
		return *file
	}
	switch *agentKind {
	case "codex":
		return cindyCodexTranscript(agentDir, *file)
	case "cc":
		return cindyClaudeTranscript(agentDir, workspace, *file)
	}
	return *file
}

func cindyTranscriptFormat(agentKind, file string) string {
	if filepath.IsAbs(file) {
		return "pi"
	}
	switch agentKind {
	case "cc":
		return "claude"
	case "codex":
		return "codex"
	case "pi":
		return "pi"
	default:
		return ""
	}
}

// cindyClaudeTranscript follows Cindy's Claude config candidates. The current
// CLI writes to CLAUDE_CONFIG_DIR or ~/.claude; claude-home is the legacy dev
// fallback. A session may move with its workspace, so a direct miss is followed
// by a filename lookup.
func cindyClaudeTranscript(agentDir string, workspace *string, sessionID string) string {
	if workspace == nil || *workspace == "" || sessionID == "" {
		return ""
	}
	home := filepath.Dir(filepath.Dir(filepath.Dir(agentDir)))
	configDirs := []string{}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		configDirs = append(configDirs, dir)
	}
	configDirs = append(configDirs, filepath.Join(home, ".claude"), filepath.Join(agentDir, "claude-home"))
	for _, configDir := range configDirs {
		root := filepath.Join(configDir, "projects")
		file := filepath.Join(root, cindyClaudeProjectKey(*workspace), sessionID+".jsonl")
		if regularFile(file) {
			return file
		}
		for _, candidate := range ListFiles(root, ".jsonl", true) {
			if filepath.Base(candidate) == sessionID+".jsonl" {
				return candidate
			}
		}
	}
	return ""
}

func cindyClaudeProjectKey(workspace string) string {
	var key strings.Builder
	for _, r := range filepath.ToSlash(workspace) {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			key.WriteRune(r)
		} else {
			key.WriteByte('-')
		}
	}
	value := key.String()
	if len(value) > 200 {
		value = value[:200]
	}
	return value
}

func regularFile(file string) bool {
	info, err := os.Stat(file)
	return err == nil && info.Mode().IsRegular()
}

// cindyCodexTranscript maps a stored Codex session ID onto the rollout file in
// Cindy's embedded Codex home. Existing profiles store absolute Pi paths, so
// the recursive lookup only runs for newer Codex-backed sessions.
func cindyCodexTranscript(agentDir, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	for _, file := range ListFiles(filepath.Join(agentDir, "codex-home", "sessions"), ".jsonl", true) {
		if strings.HasSuffix(filepath.Base(file), sessionID+".jsonl") {
			return file
		}
	}
	return ""
}

func filePathOrNil(file string) *string {
	if file == "" {
		return nil
	}
	return &file
}
