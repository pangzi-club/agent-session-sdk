package agentsession

// workbuddy.go mirrors src/main/agents/workbuddy.ts:
// metadata comes from the sessions table in ~/.workbuddy/workbuddy.db (custom_title wins, with
// title as the fallback when it is empty; background tasks and soft-deleted rows are filtered
// out), while the transcript at projects/<encoded cwd>/<uuid>.jsonl (encoding: see
// workbuddyProjectDirName) only contributes its size and serves as filePath. As in TS, the
// workspace path is taken straight from the database's cwd, without decoding.

import (
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ScanWorkbuddy mirrors scanWorkbuddy.
func ScanWorkbuddy(agentDir string) ([]SessionInfo, error) {
	db, err := OpenSQLite(filepath.Join(agentDir, "workbuddy.db"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := QueryMaps(db, "SELECT * FROM sessions")
	if err != nil {
		return nil, err
	}
	out := []SessionInfo{}
	for _, row := range rows {
		// Background automation tasks are not user sessions; keep every row if the column is absent
		if isBackgroundAutomation(row["is_background_automation"]) {
			continue
		}
		// Soft-deleted rows are already gone from the UI; deleted_at semantics: NULL / -1 = active,
		// positive ms = soft-deleted (cloud rows reuse -1)
		if isWorkbuddyDeleted(row["deleted_at"]) {
			continue
		}
		id := AsString(row["id"])
		cwd := AsString(row["cwd"])
		if id == nil || *id == "" || cwd == nil || *cwd == "" {
			continue
		}
		createdAt := FirstOr(0.0, AsEpochMs(row["created_at"]))
		// custom_title is often an empty string; treating empty as null lets title take over
		title := ToTitle(NonNil(workbuddyAsText(row["custom_title"]), workbuddyAsText(row["title"])))
		file := filepath.Join(agentDir, "projects", workbuddyProjectDirName(*cwd), *id+".jsonl")
		out = append(out, SessionInfo{
			ID:        *id,
			Workspace: *cwd,
			Title:     title,
			CreatedAt: createdAt,
			UpdatedAt: FirstOr(createdAt, AsEpochMs(row["updated_at"])),
			SizeBytes: FileSize(file),
			FilePath:  &file,
		})
	}
	return out, nil
}

func isBackgroundAutomation(v any) bool {
	if AsBoolTrue(v) {
		return true
	}
	if f, ok := AsNumber(v); ok && f == 1 {
		return true
	}
	return false
}

// isWorkbuddyDeleted mirrors the soft-delete check in workbuddy.ts (workbuddy-core's deletedAt
// semantics): NULL (local active) and -1 (cloud active) are both kept; anything else (positive
// ms, or a column that exists but holds an unexpected value) counts as deleted.
func isWorkbuddyDeleted(v any) bool {
	if v == nil {
		return false
	}
	if f, ok := AsNumber(v); ok {
		return f != -1
	}
	return true
}

// workbuddyAsText mirrors asText in workbuddy.ts: return non-blank strings as-is (no trim), else null.
func workbuddyAsText(v any) *string {
	if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
		return &s
	}
	return nil
}

// workbuddyProjectDirName mirrors workbuddyProjectDirName (workbuddy-core's
// compressWorkspacePathName): `/` `\` `:` → `-`, strip leading/trailing `-` and collapse runs of
// `-`; beyond 255 bytes, truncate to 180 bytes + `-<djb2 base36>`.
func workbuddyProjectDirName(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		if r == '/' || r == '\\' || r == ':' {
			b.WriteByte('-')
			continue
		}
		b.WriteRune(r)
	}
	// FieldsFunc drops empty segments: equivalent to trimming leading/trailing - then collapsing runs of -
	base := strings.Join(strings.FieldsFunc(b.String(), func(r rune) bool { return r == '-' }), "-")
	if len(base) <= 255 {
		return base
	}
	return truncateToByteLimitOnCodePoint(base, 180) + "-" + djb2Base36(base)
}

// truncateToByteLimitOnCodePoint mirrors the TS helper of the same name: truncate to a UTF-8 byte
// limit without splitting a code point.
func truncateToByteLimitOnCodePoint(value string, maxBytes int) string {
	bytes := 0
	var b strings.Builder
	for _, r := range value {
		size := utf8.RuneLen(r)
		if size < 0 {
			size = 0
		}
		if bytes+size > maxBytes {
			break
		}
		bytes += size
		b.WriteRune(r)
	}
	return b.String()
}

// djb2Base36 mirrors the TS helper of the same name: `h * 33 ^ byte` wrapping as int32, then
// rendered as unsigned base36.
func djb2Base36(input string) string {
	var h int32 = 5381
	for _, b := range []byte(input) {
		h = h*33 ^ int32(b)
	}
	return strconv.FormatUint(uint64(uint32(h)), 36)
}
