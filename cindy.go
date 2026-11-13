package agentsession

// cindy.go reads Cindy's local profile database and its embedded Pi session
// transcripts. Sessions are stored in <CindyGlobal>/cindy-<dataOwnerId>.db,
// while visible turns live in the Pi JSONL file recorded by sdk_session_id.
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
		file := cindyTranscript(row["sdk_session_id"])
		out = append(out, SessionInfo{
			ID:        *id,
			Workspace: *workspace,
			Title:     ToTitle(row["title"]),
			CreatedAt: createdAt,
			UpdatedAt: updatedAt,
			SizeBytes: FileSize(file),
			FilePath:  filePathOrNil(file),
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

func cindyTranscript(value any) string {
	if file := AsString(value); file != nil && *file != "" {
		return *file
	}
	return ""
}

func filePathOrNil(file string) *string {
	if file == "" {
		return nil
	}
	return &file
}
