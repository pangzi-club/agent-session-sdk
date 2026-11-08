package agentsession

// opencode.go aligns with src/main/agents/opencode.ts:
// XDG ~/.local/share/opencode; the newer opencode.db (SQLite, sizeBytes = sum of the
// message + part data column lengths) coexists with the legacy storage JSON, deduped
// by id (db wins).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ScanOpenCode aligns with scanOpenCode.
func ScanOpenCode(agentDir string) ([]SessionInfo, error) {
	byID := NewOrderedMap[SessionInfo]()

	// Read the legacy storage JSON first; db results overwrite entries with the same id
	legacy, err := scanOpenCodeLegacyStorage(agentDir)
	if err != nil {
		return nil, err
	}
	for _, session := range legacy {
		byID.Set(session.ID, session)
	}
	dbSessions, err := scanOpenCodeDatabase(agentDir)
	if err != nil {
		return nil, err
	}
	for _, session := range dbSessions {
		byID.Set(session.ID, session)
	}
	return byID.Values(), nil
}

func scanOpenCodeDatabase(agentDir string) ([]SessionInfo, error) {
	dbPath := filepath.Join(agentDir, "opencode.db")
	db, err := OpenSQLite(dbPath)
	if err != nil {
		return nil, nil
	}
	defer db.Close()
	rows, err := QueryMaps(db, `SELECT s.id, s.parent_id, s.directory, s.title, s.time_created, s.time_updated,
		(SELECT COALESCE(SUM(LENGTH(p.data)), 0) FROM part p WHERE p.session_id = s.id) +
		(SELECT COALESCE(SUM(LENGTH(m.data)), 0) FROM message m WHERE m.session_id = s.id) AS size_bytes
		FROM session s`)
	if err != nil {
		// Missing table (db not fully created) and the like: return empty so the storage fallback results stand
		return nil, nil
	}
	out := []SessionInfo{}
	for _, row := range rows {
		if parentID, ok := row["parent_id"]; ok && parentID != nil {
			if s, isStr := parentID.(string); isStr && s != "" {
				continue
			}
		}
		id := AsString(row["id"])
		workspace := AsString(row["directory"])
		if id == nil || *id == "" || workspace == nil || *workspace == "" {
			continue
		}
		createdAt := FirstOr(0.0, AsEpochMs(row["time_created"]))
		sizeBytes := 0.0
		if f, ok := AsNumber(row["size_bytes"]); ok {
			sizeBytes = f
		}
		filePath := dbPath + "#" + *id
		out = append(out, SessionInfo{
			ID:        *id,
			Workspace: *workspace,
			Title:     ToTitle(AsString(row["title"])),
			CreatedAt: createdAt,
			UpdatedAt: FirstOr(createdAt, AsEpochMs(row["time_updated"])),
			SizeBytes: sizeBytes,
			FilePath:  &filePath,
		})
	}
	return out, nil
}

// scanOpenCodeLegacyStorage aligns with scanLegacyStorage: storage/session/<projectID>/<id>.json.
func scanOpenCodeLegacyStorage(agentDir string) ([]SessionInfo, error) {
	files := ListFiles(filepath.Join(agentDir, "storage", "session"), ".json", true)
	out := []SessionInfo{}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var parsed any
		if json.Unmarshal(raw, &parsed) != nil {
			continue
		}
		m := GetMap(parsed)
		if m == nil {
			continue
		}
		id := AsString(m["id"])
		if id == nil {
			stem := strings.TrimSuffix(filepath.Base(file), ".json")
			id = &stem
		}
		workspace := AsString(m["directory"])
		if workspace == nil || *workspace == "" {
			continue
		}
		time := GetMap(m["time"])
		createdAt := FirstOr(0.0, AsEpochMs(time["created"]))
		info, err := os.Stat(file)
		if err != nil {
			// The TS side does not catch stat: a single failed file wipes out the whole
			// opencode agent, so keep that behavior and propagate the error
			return nil, err
		}
		out = append(out, SessionInfo{
			ID:        *id,
			Workspace: *workspace,
			Title:     ToTitle(AsString(m["title"])),
			CreatedAt: createdAt,
			UpdatedAt: FirstOr(createdAt, AsEpochMs(time["updated"])),
			SizeBytes: float64(info.Size()),
			FilePath:  &file,
		})
	}
	return out, nil
}
