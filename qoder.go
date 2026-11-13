package agentsession

// qoder.go mirrors src/main/agents/qoder.ts:
// ~/.qoder[-cn]/projects/<encoded workspace>/<session uuid>.jsonl; transcripts share Claude
// Code's format — the first workspace-directories line gives the workspace, and the following
// user / assistant lines carry cwd and an ISO timestamp.
// Subagent transcripts live under <uuid>/subagents/ and are not standalone sessions; ListFiles
// does not recurse, so they are skipped naturally.
// The international and Chinese editions share one format, so both registry defs use ScanQoder
// (see also the qoder-cn entry).

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// ScanQoder mirrors scanQoder.
func ScanQoder(agentDir string) ([]SessionInfo, error) {
	// The desktop app's catalog holds the sidebar titles; open it once for the
	// whole scan (a per-file open would cost one sqlite handle per session).
	titleDBs := qoderTitleDBs()
	defer func() {
		for _, db := range titleDBs {
			db.Close()
		}
	}()
	out := make([]SessionInfo, 0)
	for _, projectDir := range ListDirs(filepath.Join(agentDir, "projects")) {
		files := ListFiles(projectDir, ".jsonl", false)
		results, err := MapLimit(files, FileConcurrency, func(file string) (*SessionInfo, error) {
			return scanQoderFile(file, projectDir, titleDBs)
		})
		if err != nil {
			return nil, err
		}
		for _, session := range results {
			if session != nil {
				out = append(out, *session)
			}
		}
	}
	return out, nil
}

func scanQoderFile(file, projectDir string, titleDBs []*sql.DB) (*SessionInfo, error) {
	// The file may have been deleted after the directory was enumerated: skip it rather than
	// failing the whole agent scan
	info, err := os.Stat(file)
	if err != nil {
		return nil, nil
	}

	// Gather metadata in a single pass (stop once both are found): the workspace-directories line
	// has no timestamp, and only the timestamped user / assistant lines carry cwd. The two usually
	// land in the first two lines, so remember the first hit for each
	type qoderMeta struct {
		workspace *string
		createdAt *float64
	}
	meta := &qoderMeta{}
	ScanLines(file, func(obj map[string]any) *qoderMeta {
		if meta.createdAt == nil {
			meta.createdAt = AsEpochMs(obj["timestamp"])
		}
		if meta.workspace == nil {
			meta.workspace = qoderRecordWorkspace(obj)
		}
		if meta.workspace != nil && meta.createdAt != nil {
			return meta
		}
		return nil
	}, ScanMaxBytes)
	// Not one parsable record (empty file / no read permission): do not treat it as a session
	if meta.createdAt == nil {
		return nil, nil
	}

	// Fall back to decoding the project directory name (same encoding as Claude: / → -)
	workspace := meta.workspace
	if workspace == nil {
		workspace = DecodePathDirName(filepath.Base(projectDir))
	}
	if workspace == nil || *workspace == "" {
		return nil, nil
	}

	sessionID := strings.TrimSuffix(filepath.Base(file), ".jsonl")
	// Prefer the title the Qoder app itself shows (its desktop catalog records
	// one per session); fall back to the first user prompt.
	title := qoderAppTitle(titleDBs, sessionID)
	if title == nil {
		title = ScanLines(file, func(obj map[string]any) *string {
			if !IsStr(obj["type"], "user") {
				return nil
			}
			return ToTitle(QoderUserPromptText(GetMap(obj["message"])))
		}, ScanMaxBytes)
	}

	filePath := file
	return &SessionInfo{
		ID:        sessionID,
		Workspace: *workspace,
		Title:     title,
		CreatedAt: *meta.createdAt,
		UpdatedAt: FloorMtimeMs(info),
		SizeBytes: float64(info.Size()),
		FilePath:  &filePath,
	}, nil
}

// qoderTitleDBs opens the Qoder desktop app's session catalogs. The app keeps
// the sidebar titles in its own sqlite store; the CLI's .jsonl files record
// none, so the scan falls back to the first user prompt. Both editions are
// probed, and a missing or locked catalog is not an error.
func qoderTitleDBs() []*sql.DB {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var dbs []*sql.DB
	for _, rel := range []string{
		"Library/Application Support/com.qodercn.app.stable/main.sqlite",
		"Library/Application Support/com.qoder.app.stable/main.sqlite",
		"Library/Application Support/Qoder/main.sqlite",
	} {
		path := filepath.Join(home, rel)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if db, err := OpenSQLite(path); err == nil {
			dbs = append(dbs, db)
		}
	}
	return dbs
}

// qoderAppTitle returns the title the Qoder app shows for the session, or nil
// when none of the catalogs lists it.
func qoderAppTitle(dbs []*sql.DB, sessionID string) *string {
	for _, db := range dbs {
		rows, err := QueryMaps(db,
			`SELECT title FROM chat_sessions WHERE session_id = ? AND title != '' LIMIT 1`, sessionID)
		if err != nil || len(rows) == 0 {
			continue
		}
		if title := firstString(rows[0], "title"); title != "" {
			return &title
		}
	}
	return nil
}

// qoderRecordWorkspace mirrors recordWorkspace: the first directory on the workspace-directories
// line wins, then the cwd of a user / assistant line.
func qoderRecordWorkspace(obj map[string]any) *string {
	if dirs := GetSlice(obj["directories"]); dirs != nil {
		for _, dir := range dirs {
			if s := AsString(dir); s != nil && *s != "" {
				return s
			}
		}
	}
	return AsString(obj["cwd"])
}

// QoderUserPromptText mirrors userPromptText in agents/qoder.ts: content must be an array of
// blocks containing a non-injected text block (in pasted-image sessions the text block may be an
// `[Image: ...]` placeholder, which still counts as user-authored).
// Plain string content is an echoed skill / command body (`Base directory for this skill: ...`),
// and tool_result blocks are excluded for the same reason. Shared by the session-title path and
// the usage-side prompt counting (scanusage/qoder.go).
func QoderUserPromptText(message map[string]any) *string {
	if message == nil {
		return nil
	}
	blocks, ok := message["content"].([]any)
	if !ok {
		return nil
	}
	for _, part := range blocks {
		block := GetMap(part)
		if block == nil || !IsStr(block["type"], "text") {
			continue
		}
		text := AsString(block["text"])
		if text == nil {
			continue
		}
		trimmed := strings.TrimLeftFunc(*text, unicode.IsSpace)
		// Qoder prepends the system reminder to the user's prompt inside the
		// same text block; strip it before the injected-text check so the real
		// prompt still becomes the session title.
		trimmed = stripLeadingSystemReminder(trimmed)
		if trimmed != "" && !IsInjectedText(trimmed) {
			return &trimmed
		}
	}
	return nil
}

// stripLeadingSystemReminder removes leading <system-reminder> blocks and the
// whitespace after them.
func stripLeadingSystemReminder(text string) string {
	s := text
	for strings.HasPrefix(s, "<system-reminder>") {
		end := strings.Index(s, "</system-reminder>")
		if end < 0 {
			break
		}
		s = strings.TrimLeftFunc(s[end+len("</system-reminder>"):], unicode.IsSpace)
	}
	return s
}
