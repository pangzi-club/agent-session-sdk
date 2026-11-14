package agentsession

// dsh.go aligns with src/main/agents/dsh.ts:
// ~/.dsh/sessions/<--encoded path-->/session-<uuid>/session.v<N>.jsonl.zstd,
// decompressed in full before parsing.
// Same as TS: stat/readFile/decompression failures are not caught, so a single
// failing file zeroes out the whole dsh agent (likewise when fzstd throws).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

var zstdDecoder, _ = zstd.NewReader(nil)

// dshSessionFileRe aligns with DSH_SESSION_FILE_RE: the version number grows with
// each DSH iteration, so it must not be pinned to a specific version (that would
// make new-format sessions disappear from the list entirely).
var dshSessionFileRe = regexp.MustCompile(`^session\.v(\d+)\.jsonl\.zstd$`)

// dshMinSessionVersion aligns with DSH_MIN_SESSION_VERSION.
const dshMinSessionVersion = 3

// ListDshSessionFiles aligns with listDshSessionFiles: session directory → the
// highest-version session file inside it. When several versions coexist in one
// directory (in-place migration) only the highest is taken, so each session
// directory yields exactly one session.
func ListDshSessionFiles(agentDir string) []string {
	files, _ := listDshSessionFilesE(agentDir)
	return files
}

func listDshSessionFilesE(agentDir string) ([]string, error) {
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
	var files []string
	for _, sessionDir := range sessionDirs {
		file, err := pickDshSessionFileE(sessionDir)
		if err != nil {
			return nil, err
		}
		if file != "" {
			files = append(files, file)
		}
	}
	return files, nil
}

// pickDshSessionFile aligns with pickDshSessionFile.
func pickDshSessionFile(sessionDir string) string {
	file, _ := pickDshSessionFileE(sessionDir)
	return file
}

func pickDshSessionFileE(sessionDir string) (string, error) {
	files, err := ListFilesE(sessionDir, ".zstd", false)
	if err != nil {
		return "", err
	}
	best := ""
	bestVersion := -1
	for _, file := range files {
		matched := dshSessionFileRe.FindStringSubmatch(filepath.Base(file))
		if matched == nil {
			continue
		}
		version, err := strconv.Atoi(matched[1])
		if err != nil {
			continue
		}
		if version > bestVersion {
			best = file
			bestVersion = version
		}
	}
	return best, nil
}

// DecompressZstd is shared by the dsh session and usage scans (one
// decompression, two consumers).
func DecompressZstd(data []byte) ([]byte, error) {
	return zstdDecoder.DecodeAll(data, nil)
}

// ScanDsh mirrors scanDsh.
func ScanDsh(agentDir string) ([]SessionInfo, error) {
	files, err := listDshSessionFilesE(agentDir)
	if err != nil {
		return nil, err
	}
	results, err := MapLimit(files, FileConcurrency, ScanDshFile)
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

// ScanDshFile mirrors scanDshFile: the id is the session directory name and
// sizeBytes is the compressed file size.
func ScanDshFile(file string) (*SessionInfo, error) {
	info, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	compressed, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	decompressed, err := DecompressZstd(compressed)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(decompressed), "\n")

	type dshMeta struct {
		cwd       string
		createdAt *float64
	}
	meta := ParseLines(lines, func(obj map[string]any) *dshMeta {
		if !IsStr(obj["type"], "session") {
			return nil
		}
		version, ok := AsNumber(obj["version"])
		if !ok || version < dshMinSessionVersion {
			return nil
		}
		cwd := AsString(obj["cwd"])
		if cwd == nil || *cwd == "" {
			return nil
		}
		return &dshMeta{cwd: *cwd, createdAt: AsEpochMs(obj["createdAt"])}
	})
	if meta == nil {
		return nil, nil
	}

	// First user input (inserted[].role === "user" on agent/inbox/spliced
	// lines) becomes the fallback title; also track whether any user input
	// appeared at all — none means a blank session (created in DSH but never
	// messaged).
	title, sawUserInput := dshUserInput(lines)
	if t := dshSessionTitle(lines); t != nil {
		title = t
	}

	mtimeMs := FloorMtimeMs(info)
	createdAt := mtimeMs
	if meta.createdAt != nil {
		createdAt = *meta.createdAt
	}
	var blank *bool
	if !sawUserInput {
		value := true
		blank = &value
	}
	return &SessionInfo{
		ID:        filepath.Base(filepath.Dir(file)),
		Workspace: meta.cwd,
		Title:     title,
		Blank:     blank,
		CreatedAt: createdAt,
		UpdatedAt: mtimeMs,
		SizeBytes: float64(info.Size()),
		FilePath:  &file,
	}, nil
}

// dshSessionTitle returns the title of the last session/title event: DSH first
// writes a fallback (the truncated first prompt), then replaces it with the
// LLM-generated title. nil when the store keeps no title event.
func dshSessionTitle(lines []string) *string {
	var title *string
	WalkLines(lines, func(obj map[string]any) {
		if !IsStr(obj["type"], "session/title") {
			return
		}
		if t := FirstLineTitle(GetMap(obj["data"])["title"]); t != nil {
			title = t
		}
	})
	return title
}

// dshUserInput mirrors readDshUserInput in dsh.ts: the first user input text
// (the title source) plus whether any user input appeared (the blank-session
// check). The latter cannot be "title == nil": a user input can also be
// discarded injected text (<turn_aborted> and friends).
func dshUserInput(lines []string) (*string, bool) {
	var title *string
	sawUserInput := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue // truncated / corrupted line, skip it
		}
		if !IsStr(obj["type"], "agent/inbox/spliced") {
			continue
		}
		data := GetMap(obj["data"])
		var userInsert map[string]any
		for _, item := range GetSlice(data["inserted"]) {
			entry := GetMap(item)
			if IsStr(entry["role"], "user") {
				userInsert = entry
				break
			}
		}
		if userInsert == nil {
			continue
		}
		sawUserInput = true
		if title == nil {
			title = ToTitle(ContentToText(userInsert["content"]))
		}
		if title != nil {
			break
		}
	}
	return title, sawUserInput
}
