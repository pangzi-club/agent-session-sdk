package agentsession

// cursor.go aligns with src/main/agents/cursor.ts:
// ~/.cursor/projects/<encoded cwd>/agent-transcripts/<uuid>/<uuid>.jsonl
// One directory per session; the only metadata is the file mtime (the same value
// fills in createdAt / updatedAt). The title is the <user_query> content of the
// first user message, falling back to the full text when the tag is missing.
// A directory with no same-named transcript file is skipped as a non-session
// directory. Same as TS: decoding the directory name is lossy (path segments
// that already contain - cannot be recovered).

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var cursorUserQueryRe = regexp.MustCompile(`(?s)<user_query>(.*?)</user_query>`)

type cursorSessionDir struct {
	projectDir string
	sessionDir string
}

// ScanCursor aligns with scanCursor.
func ScanCursor(agentDir string) ([]SessionInfo, error) {
	var dirs []cursorSessionDir
	for _, projectDir := range ListDirs(filepath.Join(agentDir, "projects")) {
		for _, sessionDir := range ListDirs(filepath.Join(projectDir, "agent-transcripts")) {
			dirs = append(dirs, cursorSessionDir{projectDir: projectDir, sessionDir: sessionDir})
		}
	}
	results, err := MapLimit(dirs, FileConcurrency, scanCursorSessionDir)
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

func scanCursorSessionDir(item cursorSessionDir) (*SessionInfo, error) {
	id := filepath.Base(item.sessionDir)
	file := filepath.Join(item.sessionDir, id+".jsonl")
	info, err := os.Stat(file)
	if err != nil {
		// No same-named transcript file (incomplete directory / not a session directory): skip
		return nil, nil
	}
	title := ScanLines(file, pickCursorTitle, ScanMaxBytes)
	return &SessionInfo{
		ID:        id,
		Workspace: decodeCursorProjectDirName(filepath.Base(item.projectDir)),
		Title:     title,
		CreatedAt: FloorMtimeMs(info),
		UpdatedAt: FloorMtimeMs(info),
		SizeBytes: float64(info.Size()),
		FilePath:  &file,
	}, nil
}

// decodeCursorProjectDirName aligns with decodeCursorProjectDirName: the inverse of stripping the leading / and replacing / with -.
func decodeCursorProjectDirName(name string) string {
	return "/" + strings.ReplaceAll(name, "-", "/")
}

// pickCursorTitle aligns with pickCursorTitle: the <user_query> content of the first user message; injected text is not filtered out.
func pickCursorTitle(obj map[string]any) *string {
	if !IsStr(obj["role"], "user") {
		return nil
	}
	text := ContentToText(GetMap(obj["message"])["content"])
	if text == nil {
		return nil
	}
	raw := *text
	if m := cursorUserQueryRe.FindStringSubmatch(raw); m != nil {
		raw = m[1]
	}
	return FirstLineTitle(raw)
}
