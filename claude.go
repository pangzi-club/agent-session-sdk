package agentsession

// claude.go aligns with src/main/agents/claude.ts:
// ~/.claude/projects/<encoded path>/, preferring the sessions-index.json fast
// path and falling back to a two-pass streaming scan per JSONL when there is no index.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ScanClaude aligns with scanClaude.
func ScanClaude(agentDir string) ([]SessionInfo, error) {
	return scanClaudeProjectsDir(filepath.Join(agentDir, "projects"))
}

func scanClaudeProjectsDir(projectsDir string) ([]SessionInfo, error) {
	projectDirs, err := ListDirsE(projectsDir)
	if errors.Is(err, os.ErrNotExist) {
		return []SessionInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	// Enumerate every project before scanning so one bounded pool covers all
	// files instead of one pool per project directory.
	out := []SessionInfo{}
	var files []string
	for _, dir := range projectDirs {
		indexed, projectFiles, err := claudeProjectSessions(dir)
		if err != nil {
			return nil, err
		}
		out = append(out, indexed...)
		files = append(files, projectFiles...)
	}
	results, err := MapLimit(files, FileConcurrency, scanClaudeFile)
	if err != nil {
		return nil, err
	}
	for _, session := range results {
		if session != nil {
			out = append(out, *session)
		}
	}
	return out, nil
}

// claudeProjectSessions returns the sessions-index.json fast path for one
// project directory, plus the JSONL files that need a streaming scan when the
// index is missing or empty.
func claudeProjectSessions(projectDir string) ([]SessionInfo, []string, error) {
	if raw, err := os.ReadFile(filepath.Join(projectDir, "sessions-index.json")); err == nil {
		var root any
		if json.Unmarshal(raw, &root) == nil {
			if entries, ok := GetMap(root)["entries"].([]any); ok {
				// workspace prefers projectPath; if the index lacks it, decode the directory name (encoding rule: / → -)
				fallbackWorkspace := DecodePathDirName(filepath.Base(projectDir))
				fromIndex := make([]SessionInfo, 0, len(entries))
				for _, entry := range entries {
					m := GetMap(entry)
					var fullPath *string
					if m != nil {
						fullPath = AsString(m["fullPath"])
					}
					if fullPath != nil && strings.Contains(*fullPath, "/memory/") {
						continue
					}
					var workspace *string
					if m != nil {
						workspace = AsString(m["projectPath"])
					}
					if workspace == nil {
						workspace = fallbackWorkspace
					}
					if workspace == nil || *workspace == "" {
						continue
					}
					var fileMtime *float64
					if m != nil {
						if f, ok := AsNumber(m["fileMtime"]); ok {
							fileMtime = &f
						}
					}
					var createdAtRaw, modifiedRaw *float64
					if m != nil {
						createdAtRaw = AsEpochMs(m["created"])
						modifiedRaw = AsEpochMs(m["modified"])
					}
					createdAt := FirstOr(0.0, createdAtRaw, fileMtime)
					updatedAt := FirstOr(createdAt, modifiedRaw, fileMtime)
					// Tiered fallback: summary may be an empty string or injected text, so short-circuiting firstPrompt would lose the title
					var title, firstPrompt *string
					if m != nil {
						title = ToTitle(AsString(m["summary"]))
						if title == nil {
							firstPrompt = ToTitle(AsString(m["firstPrompt"]))
						}
					}
					id := ""
					if m != nil {
						if sessionID := AsString(m["sessionId"]); sessionID != nil {
							id = *sessionID
						}
					}
					if id == "" && fullPath != nil {
						id = strings.TrimSuffix(filepath.Base(*fullPath), ".jsonl")
					}
					size := 0.0
					if fullPath != nil && *fullPath != "" {
						size = FileSize(*fullPath)
					}
					fromIndex = append(fromIndex, SessionInfo{
						ID:        id,
						Workspace: *workspace,
						Title:     NonNil(title, firstPrompt),
						CreatedAt: createdAt,
						UpdatedAt: updatedAt,
						SizeBytes: size,
						FilePath:  fullPath,
					})
				}
				if len(fromIndex) > 0 {
					return fromIndex, nil, nil
				}
			}
		}
	}
	// Fallback: stream-scan every JSONL in the project directory (stop at the first hit, never read the whole file)
	files, err := ListFilesE(projectDir, ".jsonl", false)
	if err != nil {
		return nil, nil, err
	}
	return nil, files, nil
}

func scanClaudeFile(file string) (*SessionInfo, error) {
	// The file may vanish after the directory listing: skip it instead of
	// letting one disappearing file take down the whole agent.
	info, err := os.Stat(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type claudeMeta struct {
		cwd       string
		createdAt *float64
	}
	// Metadata: the first user line carrying a cwd (streamed scan, tolerant of a
	// very long pasted first line).
	meta, err := ScanLinesE(file, func(obj map[string]any) *claudeMeta {
		if !IsStr(obj["type"], "user") {
			return nil
		}
		cwd := AsString(obj["cwd"])
		if cwd == nil || *cwd == "" {
			return nil
		}
		return &claudeMeta{cwd: *cwd, createdAt: AsEpochMs(obj["timestamp"])}
	}, ScanMaxBytes)
	if err != nil {
		return nil, err
	}
	if meta == nil {
		return nil, nil
	}
	// The title needs its own pass: in slash-command sessions the first user
	// line is an injected echo, so keep looking for the first real prompt.
	title, err := ScanLinesE(file, func(obj map[string]any) *string {
		if !IsStr(obj["type"], "user") {
			return nil
		}
		message := GetMap(obj["message"])
		var content any
		if message != nil {
			content = message["content"]
		}
		return ToTitle(ContentToText(content))
	}, ScanMaxBytes)
	if err != nil {
		return nil, err
	}

	mtimeMs := FloorMtimeMs(info)
	createdAt := mtimeMs
	if meta.createdAt != nil {
		createdAt = *meta.createdAt
	}
	return &SessionInfo{
		ID:        strings.TrimSuffix(filepath.Base(file), ".jsonl"),
		Workspace: meta.cwd,
		Title:     title,
		CreatedAt: createdAt,
		UpdatedAt: mtimeMs,
		SizeBytes: float64(info.Size()),
		FilePath:  &file,
	}, nil
}
