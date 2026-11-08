package agentsession

// hermes.go aligns with src/main/agents/hermes.ts:
// the sessions table in ~/.hermes/state.db (hidden / archived rows are filtered out).
// Most sessions are not tied to a filesystem workspace (feishu / cron / desktop sources),
// so workspace groups them under the `Hermes | <source>` pseudo path.
// Same as TS: failing to open the db returns an error (the caller treats it as "no sessions").

import (
	"fmt"
	"path/filepath"
)

// ScanHermes aligns with scanHermes.
func ScanHermes(agentDir string) ([]SessionInfo, error) {
	dbPath := filepath.Join(agentDir, "state.db")
	db, err := OpenSQLite(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := QueryMaps(db, "SELECT * FROM sessions")
	if err != nil {
		return nil, err
	}
	out := []SessionInfo{}
	for _, row := range rows {
		if isHermesFlagOn(row["hidden"]) || isHermesFlagOn(row["archived"]) {
			continue
		}
		id := AsString(row["id"])
		if id == nil || *id == "" {
			continue
		}
		createdAt := FirstOr(0.0, SecToMs(row["started_at"]))
		out = append(out, SessionInfo{
			ID:        *id,
			Workspace: hermesWorkspace(row),
			Title:     ToTitle(AsString(row["title"])),
			CreatedAt: createdAt,
			UpdatedAt: FirstOr(createdAt, SecToMs(row["last_activity_at"])),
			SizeBytes: 0,
			FilePath:  strPtr(dbPath + "#" + *id),
		})
	}
	return out, nil
}

// hermesWorkspace aligns with hermes.ts: cwd wins; otherwise group by source
// (source missing too, fall back to "Hermes").
func hermesWorkspace(row map[string]any) string {
	if cwd := AsString(row["cwd"]); cwd != nil && *cwd != "" {
		return *cwd
	}
	if source := AsString(row["source"]); source != nil && *source != "" {
		return fmt.Sprintf("Hermes | %s", *source)
	}
	return "Hermes"
}

// isHermesFlagOn aligns with the hidden !== 1 && !== true filter in hermes.ts
// (node:sqlite returns INTEGER as the number 1).
func isHermesFlagOn(v any) bool {
	if AsBoolTrue(v) {
		return true
	}
	if f, ok := AsNumber(v); ok && f == 1 {
		return true
	}
	return false
}
