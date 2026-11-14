package agentsession

// kimi.go mirrors src/main/agents/kimi.ts:
// ~/.kimi/sessions/<md5(cwd)>/<uuid>/{wire,context}.jsonl + metadata.json.
// The workspace comes from an md5 reverse-lookup table built from kimi.json work_dirs; the
// title prefers metadata.json and falls back to wire/context.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// ScanKimi mirrors scanKimi.
func ScanKimi(agentDir string) ([]SessionInfo, error) {
	workspaces := loadKimiWorkspaces(filepath.Join(agentDir, "kimi.json"))
	projectDirs, err := ListDirsE(filepath.Join(agentDir, "sessions"))
	if errors.Is(err, os.ErrNotExist) {
		return []SessionInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	results, err := MapLimit(projectDirs, FileConcurrency, func(projectDir string) ([]SessionInfo, error) {
		workspace, ok := workspaces[filepath.Base(projectDir)]
		if !ok {
			return nil, nil
		}
		sessionDirs, err := ListDirsE(projectDir)
		if err != nil {
			return nil, err
		}
		sessions, err := MapLimit(sessionDirs, FileConcurrency, func(dir string) (*SessionInfo, error) {
			return scanKimiSession(dir, workspace)
		})
		if err != nil {
			return nil, err
		}
		out := []SessionInfo{}
		for _, session := range sessions {
			if session != nil {
				out = append(out, *session)
			}
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	out := []SessionInfo{}
	for _, sessions := range results {
		out = append(out, sessions...)
	}
	return out, nil
}

// loadKimiWorkspaces mirrors loadWorkspaces: kimi.json work_dirs → md5(path) → path.
// A missing file or a parse failure returns an empty map (every session is then skipped).
func loadKimiWorkspaces(kimiJson string) map[string]string {
	raw, err := os.ReadFile(kimiJson)
	if err != nil {
		return map[string]string{}
	}
	var parsed any
	if json.Unmarshal(raw, &parsed) != nil {
		return map[string]string{}
	}
	root := GetMap(parsed)
	dirs := GetSlice(root["work_dirs"])
	out := make(map[string]string, len(dirs))
	for _, entry := range dirs {
		if path := AsString(GetMap(entry)["path"]); path != nil {
			out[Md5Hex(*path)] = *path
		}
	}
	return out
}

func scanKimiSession(dir, workspace string) (*SessionInfo, error) {
	wireFile := filepath.Join(dir, "wire.jsonl")
	contextFile := filepath.Join(dir, "context.jsonl")
	wireStat, wireErr := os.Stat(wireFile)
	if wireErr != nil {
		wireStat = nil
	}
	contextStat, contextErr := os.Stat(contextFile)
	if contextErr != nil {
		contextStat = nil
	}
	if wireStat == nil && contextStat == nil {
		return nil, nil
	}

	// metadata.json: the official title plus the wire update time (epoch seconds)
	var title *string
	var updatedAt *float64
	if raw, err := os.ReadFile(filepath.Join(dir, "metadata.json")); err == nil {
		var parsed any
		if json.Unmarshal(raw, &parsed) == nil {
			meta := GetMap(parsed)
			title = ToTitle(AsString(meta["title"]))
			updatedAt = SecToMs(meta["wire_mtime"])
		}
	}

	// wire.jsonl: the first timestamp gives the creation time, TurnBegin's user_input the title
	var createdAt *float64
	if wireStat != nil {
		lines, err := ReadHeadLines(wireFile, 64*1024)
		if err != nil {
			return nil, err
		}
		createdAt = ParseLines(lines, func(obj map[string]any) *float64 {
			return SecToMs(obj["timestamp"])
		})
		if title == nil {
			title = ParseLines(lines, func(obj map[string]any) *string {
				message := GetMap(obj["message"])
				if !IsStr(message["type"], "TurnBegin") {
					return nil
				}
				payload := GetMap(message["payload"])
				return ToTitle(ContentToText(payload["user_input"]))
			})
		}
	}

	// context.jsonl: title fallback when there is no wire file ({"role":"user","content":...})
	if title == nil && contextStat != nil {
		lines, err := ReadHeadLines(contextFile, 64*1024)
		if err != nil {
			return nil, err
		}
		title = ParseLines(lines, func(obj map[string]any) *string {
			if !IsStr(obj["role"], "user") {
				return nil
			}
			return ToTitle(ContentToText(obj["content"]))
		})
	}

	// Time fallback: wire's mtime takes precedence over context's
	baseStat := contextStat
	if wireStat != nil {
		baseStat = wireStat
	}
	baseMtime := FloorMtimeMs(baseStat)
	size := float64(0)
	if wireStat != nil {
		size += float64(wireStat.Size())
	}
	if contextStat != nil {
		size += float64(contextStat.Size())
	}
	createdAtOut := baseMtime
	if createdAt != nil {
		createdAtOut = *createdAt
	}
	updatedAtOut := baseMtime
	if updatedAt != nil {
		updatedAtOut = *updatedAt
	}
	filePath := wireFile
	if wireStat == nil {
		filePath = contextFile
	}
	return &SessionInfo{
		ID:        filepath.Base(dir),
		Workspace: workspace,
		Title:     title,
		CreatedAt: createdAtOut,
		UpdatedAt: updatedAtOut,
		SizeBytes: size,
		FilePath:  &filePath,
	}, nil
}
