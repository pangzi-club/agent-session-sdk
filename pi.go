package agentsession

// pi.go aligns with src/main/agents/pi.ts:
// ~/.pi/agent/sessions/**/*.jsonl, parsing only the first 64KB head
// (session header + first user message).
// Same as TS: stat is not caught, so a single failed file wipes out the whole pi agent.

import (
	"os"
	"path/filepath"
	"strings"
)

// ScanPi aligns with scanPi.
func ScanPi(agentDir string) ([]SessionInfo, error) {
	files := ListFiles(filepath.Join(agentDir, "agent", "sessions"), ".jsonl", true)
	results, err := MapLimit(files, FileConcurrency, ScanPiFile)
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

// ScanPiFile aligns with scanPiFile.
func ScanPiFile(file string) (*SessionInfo, error) {
	info, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	lines, err := ReadHeadLines(file, 64*1024)
	if err != nil {
		return nil, err
	}

	type piMeta struct {
		cwd       string
		createdAt *float64
	}
	meta := ParseLines(lines, func(obj map[string]any) *piMeta {
		if !IsStr(obj["type"], "session") {
			return nil
		}
		if version, ok := AsNumber(obj["version"]); !ok || version != 3 {
			return nil
		}
		cwd := AsString(obj["cwd"])
		if cwd == nil || *cwd == "" {
			return nil
		}
		return &piMeta{cwd: *cwd, createdAt: AsEpochMs(obj["timestamp"])}
	})
	if meta == nil {
		return nil, nil
	}

	title := ParseLines(lines, func(obj map[string]any) *string {
		if !IsStr(obj["type"], "message") {
			return nil
		}
		message := GetMap(obj["message"])
		if !IsStr(message["role"], "user") {
			return nil
		}
		return ToTitle(ContentToText(message["content"]))
	})

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
