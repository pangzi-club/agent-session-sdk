package agentsession

// grok.go aligns with src/main/agents/grok.ts:
// ~/.grok/sessions/<URL-encoded cwd>/<uuid>/summary.json; the updates.jsonl body is used only for its size.
// Same as TS: a decodeURIComponent failure rejects (wiping out the whole grok agent).

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
)

// ScanGrok aligns with scanGrok.
func ScanGrok(agentDir string) ([]SessionInfo, error) {
	dirs, err := listGrokSessionDirsE(agentDir)
	if err != nil {
		return nil, err
	}
	results, err := MapLimit(
		dirs,
		FileConcurrency,
		ScanGrokSessionDir,
	)
	if err != nil {
		return nil, err
	}
	out := make([]SessionInfo, 0, len(results))
	for _, session := range results {
		if session != nil {
			out = append(out, *session)
		}
	}
	return out, nil
}

// ListGrokSessionDirs aligns with listGrokSessionDirs: enumerates the
// sessions/<URL-encoded cwd>/<uuid> session dirs. The usage scan reuses it so the
// layout rules live in only one place.
func ListGrokSessionDirs(agentDir string) []string {
	dirs, _ := listGrokSessionDirsE(agentDir)
	return dirs
}

func listGrokSessionDirsE(agentDir string) ([]string, error) {
	projectDirs, err := ListDirsE(filepath.Join(agentDir, "sessions"))
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var sessionDirs []string
	for _, projectDir := range projectDirs {
		dirs, err := ListDirsE(projectDir)
		if err != nil {
			return nil, err
		}
		sessionDirs = append(sessionDirs, dirs...)
	}
	return sessionDirs, nil
}

// ScanGrokSessionDir aligns with scanGrokSessionDir: a dir without summary.json is not a session dir, so return nil.
func ScanGrokSessionDir(dir string) (*SessionInfo, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if errors.Is(err, os.ErrNotExist) {
		// No summary.json: not a session dir, skip it
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var parsed any
	if json.Unmarshal(raw, &parsed) != nil {
		return nil, nil
	}
	summary := GetMap(parsed)

	info := GetMap(summary["info"])
	id := AsString(info["id"])
	if id == nil {
		base := filepath.Base(dir)
		id = &base
	}
	workspace := AsString(info["cwd"])
	if workspace == nil {
		decoded, err := url.PathUnescape(filepath.Base(filepath.Dir(dir)))
		if err != nil {
			return nil, err
		}
		workspace = &decoded
	}
	dirStat, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	updatesFile := filepath.Join(dir, "updates.jsonl")
	return &SessionInfo{
		ID:        *id,
		Workspace: *workspace,
		Title:     ToTitle(AsString(summary["session_summary"])),
		CreatedAt: FirstOr(FloorMtimeMs(dirStat), AsEpochMs(summary["created_at"])),
		UpdatedAt: FirstOr(FloorMtimeMs(dirStat), AsEpochMs(summary["updated_at"])),
		SizeBytes: FileSize(updatesFile),
		FilePath:  &updatesFile,
	}, nil
}
