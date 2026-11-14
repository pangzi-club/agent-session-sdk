package agentsession

// codex.go aligns with src/main/agents/codex.ts:
// ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl, streamed in two passes
// (session_meta metadata + title from the first real user message).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ScanCodex aligns with scanCodex.
func ScanCodex(agentDir string) ([]SessionInfo, error) {
	files, err := codexSessionFiles(agentDir)
	if err != nil {
		return nil, err
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

func codexSessionFiles(agentDir string) ([]string, error) {
	years, err := ListDirsE(filepath.Join(agentDir, "sessions"))
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	files := []string{}
	for _, year := range years {
		months, err := ListDirsE(year)
		if err != nil {
			return nil, err
		}
		for _, month := range months {
			days, err := ListDirsE(month)
			if err != nil {
				return nil, err
			}
			for _, day := range days {
				dayFiles, err := ListFilesE(day, ".jsonl", false)
				if err != nil {
					return nil, err
				}
				files = append(files, dayFiles...)
			}
		}
	}
	return files, nil
}

// ScanCodexFile reads session metadata and the first real user prompt. A file
// that disappears after enumeration is skipped; other filesystem errors remain visible.
func ScanCodexFile(file string) (*SessionInfo, error) {
	info, err := os.Stat(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	type codexMeta struct {
		cwd            string
		createdAt      *float64
		guardianReview bool
	}
	meta, err := ScanLinesE(file, func(obj map[string]any) *codexMeta {
		if !IsStr(obj["type"], "session_meta") {
			return nil
		}
		payload := GetMap(obj["payload"])
		cwd := AsString(payload["cwd"])
		if cwd == nil || *cwd == "" {
			return nil
		}
		return &codexMeta{
			cwd:            *cwd,
			createdAt:      AsEpochMs(payload["timestamp"]),
			guardianReview: IsStr(payload["thread_source"], "guardian_review"),
		}
	}, ScanMaxBytes)
	if err != nil {
		return nil, err
	}
	if meta == nil {
		return nil, nil
	}
	if meta.guardianReview {
		return nil, nil
	}

	title, err := ScanLinesE(file, func(obj map[string]any) *string {
		if !IsStr(obj["type"], "response_item") {
			return nil
		}
		payload := GetMap(obj["payload"])
		if !IsStr(payload["type"], "message") || !IsStr(payload["role"], "user") {
			return nil
		}
		return ToTitle(ContentToText(payload["content"]))
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
