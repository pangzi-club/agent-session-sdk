package agentsession

// codex.go aligns with src/main/agents/codex.ts:
// ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl, streamed in two passes
// (session_meta metadata + title from the first real user message).

import (
	"os"
	"path/filepath"
	"strings"
)

// ScanCodex aligns with scanCodex.
func ScanCodex(agentDir string) ([]SessionInfo, error) {
	var files []string
	for _, year := range ListDirs(filepath.Join(agentDir, "sessions")) {
		for _, month := range ListDirs(year) {
			for _, day := range ListDirs(month) {
				files = append(files, ListFiles(day, ".jsonl", false)...)
			}
		}
	}
	results, err := MapLimit(files, FileConcurrency, ScanCodexFile)
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

// ScanCodexFile aligns with scanCodexFile: a file may be deleted after enumeration,
// so a failed stat only skips that file. Metadata (the session_meta first line) and
// title (the first real user message, possibly past injected prompts and over-long
// lines) each get their own scan pass.
func ScanCodexFile(file string) (*SessionInfo, error) {
	info, err := os.Stat(file)
	if err != nil {
		return nil, nil
	}

	type codexMeta struct {
		cwd       string
		createdAt *float64
	}
	meta := ScanLines(file, func(obj map[string]any) *codexMeta {
		if !IsStr(obj["type"], "session_meta") {
			return nil
		}
		payload := GetMap(obj["payload"])
		cwd := AsString(payload["cwd"])
		if cwd == nil || *cwd == "" {
			return nil
		}
		return &codexMeta{cwd: *cwd, createdAt: AsEpochMs(payload["timestamp"])}
	}, ScanMaxBytes)
	if meta == nil {
		return nil, nil
	}

	title := ScanLines(file, func(obj map[string]any) *string {
		if !IsStr(obj["type"], "response_item") {
			return nil
		}
		payload := GetMap(obj["payload"])
		if !IsStr(payload["type"], "message") || !IsStr(payload["role"], "user") {
			return nil
		}
		return ToTitle(ContentToText(payload["content"]))
	}, ScanMaxBytes)

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
