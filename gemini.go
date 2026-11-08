package agentsession

// gemini.go aligns with src/main/agents/gemini.ts:
// ~/.gemini/tmp/<sha256(cwd)>/logs.json (one file holds many sessionIds); the
// workspace is recovered by walking home and matching directory hashes.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
)

var geminiHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Pruning rules for the home walk (aligns with gemini.ts)
const geminiMaxDepth = 6

var geminiSkipDirs = map[string]bool{
	"Library":      true,
	"Applications": true,
	"node_modules": true,
	"Music":        true,
	"Movies":       true,
	"Pictures":     true,
	"Public":       true,
	// macOS TCC-protected directories: walking them triggers the system
	// "access Desktop / Documents / Downloads" permission prompt. Failing to
	// resolve projects here (their sessions are skipped) beats bothering the user.
	"Desktop":   true,
	"Documents": true,
	"Downloads": true,
}

type geminiAgg struct {
	workspace string
	title     *string
	createdAt *float64
	updatedAt float64
	sizeBytes float64
	filePath  string
}

// ScanGemini aligns with scanGemini.
func ScanGemini(agentDir string) ([]SessionInfo, error) {
	entries, err := os.ReadDir(filepath.Join(agentDir, "tmp"))
	if err != nil {
		entries = nil
	}
	var hashDirs []string
	for _, entry := range entries {
		if entry.IsDir() && geminiHashRe.MatchString(entry.Name()) {
			hashDirs = append(hashDirs, filepath.Join(agentDir, "tmp", entry.Name()))
		}
	}
	if len(hashDirs) == 0 {
		return []SessionInfo{}, nil
	}

	// No need to read the files when every reverse lookup failed (home unreadable, etc.)
	workspaceByHash := resolveGeminiWorkspaces(agentDir, hashDirs)
	if len(workspaceByHash) == 0 {
		return []SessionInfo{}, nil
	}

	sessions := NewOrderedMap[geminiAgg]()
	for _, dir := range hashDirs {
		workspace, ok := workspaceByHash[filepath.Base(dir)]
		if !ok {
			continue
		}
		logsFile := filepath.Join(dir, "logs.json")
		logsStat, err := os.Stat(logsFile)
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(logsFile)
		if err != nil {
			continue
		}
		var parsed any
		if json.Unmarshal(raw, &parsed) != nil {
			continue
		}
		entries, ok := parsed.([]any)
		if !ok {
			continue
		}
		for _, entry := range entries {
			m := GetMap(entry)
			if m == nil {
				continue
			}
			sessionID := AsString(m["sessionId"])
			if sessionID == nil {
				continue
			}
			timestampMs := AsEpochMs(m["timestamp"])
			if existing, ok := sessions.Get(*sessionID); ok {
				newUpdated := 0.0
				if timestampMs != nil {
					newUpdated = *timestampMs
				}
				if newUpdated > existing.updatedAt {
					if timestampMs != nil {
						existing.updatedAt = *timestampMs
					}
					sessions.Set(*sessionID, existing)
				}
			} else {
				updatedAt := 0.0
				if timestampMs != nil {
					updatedAt = *timestampMs
				}
				sessions.Set(*sessionID, geminiAgg{
					workspace: workspace,
					title:     ToTitle(AsString(m["message"])),
					createdAt: timestampMs,
					updatedAt: updatedAt,
					sizeBytes: float64(logsStat.Size()),
					filePath:  logsFile,
				})
			}
		}
	}

	out := make([]SessionInfo, 0, sessions.Len())
	for _, id := range sessions.Keys() {
		info, _ := sessions.Get(id)
		createdAt := info.updatedAt
		if info.createdAt != nil {
			createdAt = *info.createdAt
		}
		out = append(out, SessionInfo{
			ID:        id,
			Workspace: info.workspace,
			Title:     info.title,
			CreatedAt: createdAt,
			UpdatedAt: info.updatedAt,
			SizeBytes: info.sizeBytes,
			FilePath:  &info.filePath,
		})
	}
	return out, nil
}

// resolveGeminiWorkspaces mirrors resolveWorkspaces: walk home (the parent of
// .gemini); sha256(absolute directory path) matches a tmp directory name.
func resolveGeminiWorkspaces(agentDir string, hashDirs []string) map[string]string {
	targets := make(map[string]bool, len(hashDirs))
	for _, dir := range hashDirs {
		targets[filepath.Base(dir)] = true
	}
	home := filepath.Dir(agentDir)
	found := map[string]string{}
	geminiWalk(home, targets, found, 0)
	return found
}

func geminiWalk(dir string, targets map[string]bool, found map[string]string, depth int) {
	if len(targets) == 0 || depth > geminiMaxDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name()[0] == '.' || geminiSkipDirs[entry.Name()] {
			continue
		}
		full := filepath.Join(dir, entry.Name())
		hash := Sha256Hex(full)
		if targets[hash] {
			found[hash] = full
			delete(targets, hash)
		}
		geminiWalk(full, targets, found, depth+1)
	}
}
