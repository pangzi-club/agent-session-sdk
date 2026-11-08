package agentsession

// zcode.go aligns with src/main/agents/zcode.ts:
// the session table in ~/.zcode/cli/db/db.sqlite (subagent_child rows are filtered out);
// the rollout file is used only for its size.
// Same as TS: failing to open the db returns an error (the caller treats it as "no sessions").

import (
	"path/filepath"
)

// ScanZcode aligns with scanZcode.
func ScanZcode(agentDir string) ([]SessionInfo, error) {
	dbPath := filepath.Join(agentDir, "cli/db/db.sqlite")
	db, err := OpenSQLite(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := QueryMaps(db, "SELECT * FROM session")
	if err != nil {
		return nil, err
	}
	out := []SessionInfo{}
	for _, row := range rows {
		taskType, hasType := row["task_type"]
		if hasType && taskType != nil && !IsStr(taskType, "interactive") {
			continue
		}
		id := AsString(row["id"])
		workspace := AsString(row["directory"])
		if id == nil || *id == "" || workspace == nil || *workspace == "" {
			continue
		}
		createdAt := FirstOr(0.0, AsEpochMs(row["time_created"]))
		out = append(out, SessionInfo{
			ID:        *id,
			Workspace: *workspace,
			Title:     ToTitle(AsString(row["title"])),
			CreatedAt: createdAt,
			UpdatedAt: FirstOr(createdAt, AsEpochMs(row["time_updated"])),
			SizeBytes: FileSize(filepath.Join(agentDir, "cli/rollout", "model-io-"+*id+".jsonl")),
			FilePath:  strPtr(dbPath + "#" + *id),
		})
	}
	return out, nil
}

func strPtr(s string) *string { return &s }
