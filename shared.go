package agentsession

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/sync/errgroup"
	// Pure-Go SQLite driver (no cgo, cross-compiles easily); registers the "sqlite" database/sql driver name
	_ "modernc.org/sqlite"
)

// FileConcurrency mirrors FILE_CONCURRENCY in agents/ts.
const FileConcurrency = 16

// fileWorkTokens bounds the total number of concurrently running file-worker
// functions across the whole process. Per-scan pools multiply (all agents scan
// in parallel, and each runs its own pool), so the per-pool limit alone does
// not cap disk-reading parallelism; this shared budget does. Every MapLimit
// worker holds one token while running, therefore fn must not call MapLimit
// itself — scanners enumerate nested work up front instead (see kimi_code.go).
var fileWorkTokens = make(chan struct{}, FileConcurrency)

// MapLimit mirrors mapLimit: bounded per-call concurrency, results keep input
// order, any failure fails the whole batch. Worker bodies additionally share
// the process-wide fileWorkTokens budget.
func MapLimit[T any, R any](items []T, limit int, fn func(T) (R, error)) ([]R, error) {
	results := make([]R, len(items))
	group, _ := errgroup.WithContext(context.Background())
	group.SetLimit(limit)
	for i, item := range items {
		i, item := i, item
		group.Go(func() error {
			fileWorkTokens <- struct{}{}
			defer func() { <-fileWorkTokens }()
			r, err := fn(item)
			if err != nil {
				return err
			}
			results[i] = r
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

// ActiveGapCapMs mirrors ACTIVE_GAP_CAP_MS in usage/ts.
const ActiveGapCapMs = 120_000

// ScanMaxBytes mirrors SCAN_MAX_BYTES in agents/ts (32MB streaming scan limit).
const ScanMaxBytes = 32 * 1024 * 1024

// ---- JSON value helpers (equivalent to the TS typeof checks on unknown) ----

// AsString returns non-nil only for strings (same as TS asString; empty strings are returned too).
func AsString(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// AsNumber returns finite numbers only (mirrors typeof value === "number" && Number.isFinite).
func AsNumber(v any) (float64, bool) {
	f, ok := v.(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// AsBoolTrue returns true only for boolean true (mirrors obj.isSidechain === true).
func AsBoolTrue(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

// GetMap returns a map for JSON objects (not arrays or scalars); in TS, a property read on a non-object yields undefined.
func GetMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// GetSlice returns a slice for JSON arrays.
func GetSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

// IsStr reports whether the value is a string equal to s (equivalent to the TS obj.type === "user" check).
func IsStr(v any, s string) bool {
	t, ok := v.(string)
	return ok && t == s
}

// SkillOfToolUse mirrors skillOfToolUse in usage/ts:
// the skill name carried by a Skill tool call (input.skill on tool_use); nil for any other tool.
func SkillOfToolUse(block map[string]any) *string {
	if !IsStr(block["name"], "Skill") {
		return nil
	}
	input := GetMap(block["input"])
	if input == nil {
		return nil
	}
	skill := AsString(input["skill"])
	if skill == nil || *skill == "" {
		return nil
	}
	return skill
}

// NonNil returns the first non-nil value (equivalent to the TS a ?? b ?? c chain).
func NonNil[T any](vs ...*T) *T {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

// NonZero returns the first non-nil value (a ?? chain for numeric pointers; the caller supplies the trailing 0 default).
func FirstOr[T any](fallback T, vs ...*T) T {
	for _, v := range vs {
		if v != nil {
			return *v
		}
	}
	return fallback
}

// ParseISO mirrors Date.parse (the data is always RFC3339-shaped; TS would read timezone-less strings
// as local time, which cannot be reproduced here, so they are treated as UTC).
func ParseISO(s string) (float64, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return float64(t.UnixMilli()), true
		}
	}
	return 0, false
}

// AsEpochMs mirrors asEpochMs in agents/ts: finite numbers pass through, strings go through Date.parse, otherwise null.
func AsEpochMs(v any) *float64 {
	if f, ok := AsNumber(v); ok {
		return &f
	}
	if s, ok := v.(string); ok {
		if ms, ok := ParseISO(s); ok {
			return &ms
		}
	}
	return nil
}

// AsTimestamp mirrors asTimestamp in usage/ts: only strings go through Date.parse.
func AsTimestamp(v any) *float64 {
	if s, ok := v.(string); ok {
		if ms, ok := ParseISO(s); ok {
			return &ms
		}
	}
	return nil
}

// AsTokenCount mirrors asTokenCount in usage/ts: floor finite positive numbers, otherwise 0.
func AsTokenCount(v any) float64 {
	if f, ok := AsNumber(v); ok && f > 0 {
		return math.Floor(f)
	}
	return 0
}

// SecToMs mirrors secToMs in agents/kimi.ts: round a positive finite number (epoch seconds) to milliseconds.
func SecToMs(v any) *float64 {
	if f, ok := AsNumber(v); ok && f > 0 {
		ms := math.Round(f * 1000)
		return &ms
	}
	return nil
}

// Md5Hex / Sha256Hex: hashes for reverse-looking-up directory names (kimi / gemini).
func Md5Hex(text string) string {
	sum := md5.Sum([]byte(text))
	return hex.EncodeToString(sum[:])
}

func Sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// ---- Title cleanup (agents/ts) ----

// IsInjectedText mirrors isInjectedText: a leading `<` or `# ` marks agent-injected text.
func IsInjectedText(text string) bool {
	return strings.HasPrefix(text, "<") || strings.HasPrefix(text, "# ")
}

const planPromptLine = "PLEASE IMPLEMENT THIS PLAN:"

// myRequestHeading mirrors MY_REQUEST_HEADING in agents/ts: the envelope separator codex emits when
// attachments (images / pasted text) are present, with the real request right after it. The envelope
// header starts with `# ` and counts as injected text, so reading only the first line loses everything.
const myRequestHeading = "## My request:"

var planHeadingRe = regexp.MustCompile(`^\*\*(.+)\*\*$`)

func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// utf16Prefix mirrors TS slice(0, n) (truncates by UTF-16 code units; an emoji counts as 2).
func utf16Prefix(s string, n int) string {
	units := utf16.Encode([]rune(s))
	if n > len(units) {
		n = len(units)
	}
	return string(utf16.Decode(units[:n]))
}

// ToTitle mirrors toTitle in agents/ts: skip leading blank lines, unwrap the codex attachment envelope
// and the plan preamble, treat injected text as null, truncate at 120/117. nil means this entry has no
// usable title and the caller should look at the next one. Accepts string or *string (As* helper results).
func ToTitle(raw any) *string {
	var s string
	switch v := raw.(type) {
	case string:
		s = v
	case *string:
		if v == nil {
			return nil
		}
		s = *v
	default:
		return nil
	}
	if s == "" {
		return nil
	}
	rawLines := strings.Split(s, "\n")
	lines := make([]string, len(rawLines))
	for i, l := range rawLines {
		lines[i] = strings.TrimSpace(l)
	}
	candidates := lines
	for i, l := range lines {
		if l == myRequestHeading {
			candidates = lines[i+1:]
			break
		}
	}
	start := -1
	for i, l := range candidates {
		if l != "" {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	first := candidates[start]
	if first == planPromptLine {
		heading := ""
		for _, l := range candidates[start+1:] {
			if l != "" {
				heading = l
				break
			}
		}
		if m := planHeadingRe.FindStringSubmatch(heading); m != nil {
			first = m[1]
		} else {
			first = heading
		}
	}
	if first == "" || IsInjectedText(first) {
		return nil
	}
	if utf16Len(first) > 120 {
		first = utf16Prefix(first, 117) + "..."
	}
	out := first
	return &out
}

// FirstLineTitle mirrors firstLineTitle in agents/ts: take the first non-blank line and truncate at
// 120/117, without unwrapping envelopes or checking for injection (for content that is already the
// user's own text, where a legitimate title starting with `# ` must not be discarded as injected).
func FirstLineTitle(raw any) *string {
	var s string
	switch v := raw.(type) {
	case string:
		s = v
	case *string:
		if v == nil {
			return nil
		}
		s = *v
	default:
		return nil
	}
	if s == "" {
		return nil
	}
	first := ""
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			first = trimmed
			break
		}
	}
	if first == "" {
		return nil
	}
	if utf16Len(first) > 120 {
		first = utf16Prefix(first, 117) + "..."
	}
	return &first
}

// ContentToText mirrors contentToText: join a string or an array of {text} blocks; an empty result is null.
func ContentToText(content any) *string {
	if s, ok := content.(string); ok {
		return &s
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if m := GetMap(block); m != nil {
			if t, ok := m["text"].(string); ok {
				parts = append(parts, t)
			}
		}
	}
	joined := strings.Join(parts, " ")
	if joined == "" {
		return nil
	}
	return &joined
}

// DecodePathDirName mirrors decodePathDirName (Claude/Pi project directory name -> path: strip leading/trailing --, then - -> /).
func DecodePathDirName(name string) *string {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(name, "--"), "--")
	if trimmed == "" || !strings.HasPrefix(trimmed, "-") {
		return nil
	}
	res := strings.ReplaceAll(trimmed, "-", "/")
	return &res
}

// Tildify mirrors tildify in usage/scan.ts: shorten paths inside home to a ~/ display path.
func Tildify(target, home string) string {
	if target == home {
		return "~"
	}
	prefix := home + string(os.PathSeparator)
	if strings.HasPrefix(target, prefix) {
		return "~" + target[len(home):]
	}
	return target
}

// ---- Filesystem helpers ----

// FileSize mirrors fileSize: returns 0 when stat fails.
func FileSize(file string) float64 {
	if info, err := os.Stat(file); err == nil {
		return float64(info.Size())
	}
	return 0
}

// FloorMtimeMs mirrors Math.floor(stat.mtimeMs).
func FloorMtimeMs(info os.FileInfo) float64 {
	return float64(info.ModTime().UnixNano() / int64(time.Millisecond))
}

// ListDirsE lists visible subdirectories and preserves filesystem errors.
func ListDirsE(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			out = append(out, filepath.Join(dir, entry.Name()))
		}
	}
	return out, nil
}

// ListDirs mirrors listDirs: list subdirectories, skip dot-prefixed ones, return empty on error.
func ListDirs(dir string) []string {
	out, _ := ListDirsE(dir)
	return out
}

// ListFilesE lists files with the given extension and preserves filesystem
// errors, including failures in recursively visited subdirectories.
func ListFilesE(dir, ext string, recursive bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	var subDirs []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ext) && !strings.HasPrefix(entry.Name(), ".") {
			files = append(files, filepath.Join(dir, entry.Name()))
		} else if entry.IsDir() {
			subDirs = append(subDirs, filepath.Join(dir, entry.Name()))
		}
	}
	if !recursive {
		return files, nil
	}
	for _, sub := range subDirs {
		nested, err := ListFilesE(sub, ext, true)
		if err != nil {
			return nil, err
		}
		files = append(files, nested...)
	}
	return files, nil
}

// ListFiles mirrors listFiles: list files with the given extension (dot-prefixed files are skipped);
// recursive descends into every subdirectory (including dot directories).
func ListFiles(dir, ext string, recursive bool) []string {
	files, _ := ListFilesE(dir, ext, recursive)
	return files
}

// ReadHeadLines reads only the first maxBytes bytes and splits them into lines
// (the last line may be truncated; the caller has to tolerate that). Open and
// read failures propagate as errors.
func ReadHeadLines(file string, maxBytes int) ([]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data := make([]byte, maxBytes)
	n, err := io.ReadFull(f, data)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return strings.Split(string(data[:n]), "\n"), nil
}

// ParseLines mirrors parseLines: the first JSON line that parses and satisfies pick; unparseable lines are skipped.
func ParseLines[T any](lines []string, pick func(map[string]any) *T) *T {
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var v any
		if json.Unmarshal([]byte(line), &v) != nil {
			continue
		}
		m := GetMap(v)
		if m == nil {
			continue
		}
		if result := pick(m); result != nil {
			return result
		}
	}
	return nil
}

// WalkLines mirrors walkLines in usage/ts: parse each line and feed it to the callback, silently skipping bad lines.
func WalkLines(lines []string, visit func(map[string]any)) {
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var v any
		if json.Unmarshal([]byte(line), &v) != nil {
			continue
		}
		if m := GetMap(v); m != nil {
			visit(m)
		}
	}
}

// ScanLinesE streams line by line, stops on the first match, skips malformed
// JSON and reports filesystem/read failures. Scanning stops at maxBytes.
func ScanLinesE[T any](file string, pick func(map[string]any) *T, maxBytes int) (*T, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 128*1024)
	bytes := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		content := strings.TrimSuffix(string(line), "\n")
		content = strings.TrimSuffix(content, "\r")
		bytes += len(content) + 1
		if bytes > maxBytes {
			return nil, nil
		}
		if strings.TrimSpace(content) != "" {
			var v any
			if json.Unmarshal([]byte(content), &v) == nil {
				if m := GetMap(v); m != nil {
					if result := pick(m); result != nil {
						return result, nil
					}
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil, nil
			}
			return nil, readErr
		}
	}
}

// ScanLines preserves the best-effort metadata helper. New scanners should use
// ScanLinesE when read failures must not look like an absent session.
func ScanLines[T any](file string, pick func(map[string]any) *T, maxBytes int) *T {
	result, _ := ScanLinesE(file, pick, maxBytes)
	return result
}

// ReadAllLinesE reads and splits a JSONL file, preserving filesystem errors.
// ReadAllLines remains for legacy best-effort metadata scans.
func ReadAllLinesE(file string) ([]string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(data), "\n"), nil
}

// ReadAllLines reads the whole file and splits it into lines; failures return
// empty for optional metadata paths. Usage accounting uses ReadAllLinesE.
func ReadAllLines(file string) []string {
	lines, _ := ReadAllLinesE(file)
	return lines
}

// ---- SQLite (read-only, equivalent to node:sqlite DatabaseSync readOnly) ----

// OpenSQLite opens the database read-only (busy_timeout 1000ms matches the TS timeout). A missing or
// unopenable file returns an error, matching node:sqlite readOnly behavior (the file is not created).
func OpenSQLite(dbPath string) (*sql.DB, error) {
	// The SQLite driver reports a missing read-only database as a generic
	// unable-to-open error. Stat first so callers can distinguish a normal
	// absent agent store from a corrupt or inaccessible one with errors.Is.
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(1000)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// QueryMaps runs the query and collects rows as column name -> value maps (matching the row shape of
// node:sqlite .all()). INTEGER -> float64, TEXT/[]byte -> string, NULL -> nil.
func QueryMaps(db *sql.DB, query string, args ...any) ([]map[string]any, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := make(map[string]any, len(cols))
		for i, col := range cols {
			m[col] = normalizeSQLValue(vals[i])
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func normalizeSQLValue(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case int64:
		return float64(t)
	default:
		return v
	}
}

// ---- Insertion-ordered map (TS Map semantics; Go map iteration is unordered, output must be deterministic) ----

type OrderedMap[V any] struct {
	keys []string
	vals map[string]V
}

func NewOrderedMap[V any]() *OrderedMap[V] {
	return &OrderedMap[V]{vals: map[string]V{}}
}

// Set keeps the original position when overwriting an existing key (mirrors TS Map.set).
func (m *OrderedMap[V]) Set(key string, value V) {
	if _, ok := m.vals[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.vals[key] = value
}

func (m *OrderedMap[V]) Get(key string) (V, bool) {
	v, ok := m.vals[key]
	return v, ok
}

func (m *OrderedMap[V]) Has(key string) bool {
	_, ok := m.vals[key]
	return ok
}

func (m *OrderedMap[V]) Len() int {
	return len(m.keys)
}

// Keys returns the keys in insertion order.
func (m *OrderedMap[V]) Keys() []string {
	return m.keys
}

func (m *OrderedMap[V]) Values() []V {
	out := make([]V, 0, len(m.keys))
	for _, k := range m.keys {
		out = append(out, m.vals[k])
	}
	return out
}
