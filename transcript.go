package agentsession

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
)

// ErrUnsupported marks an agent whose transcript format has no loader yet.
var ErrUnsupported = errors.New("transcript format not supported")

// LoadTranscript parses a session's transcript without any caching.
func LoadTranscript(agentID string, info SessionInfo) ([]Entry, error) {
	switch agentID {
	case "claude", "qoder", "qoder-cn":
		return parseClaudeTranscript(info)
	case "codex":
		return parseCodexTranscript(info)
	case "gemini":
		return parseGeminiTranscript(info)
	case "cursor":
		return parseCursorTranscript(info)
	case "pi":
		return parsePiTranscript(info)
	case "cindy":
		return parsePiTranscript(info)
	case "kimi":
		return parseKimiTranscript(info)
	case "kimi-code":
		return parseKimiCodeTranscript(info)
	case "dsh":
		return parseDshTranscript(info)
	case "workbuddy":
		return parseWorkbuddyTranscript(info)
	case "opencode", "zcode":
		return parseSessionDBTranscript(info)
	case "hermes":
		return parseHermesTranscript(info)
	case "grok":
		return parseGenericTranscript(info)
	default:
		return parseGenericTranscript(info)
	}
}

type cacheItem struct {
	signature string
	entries   []Entry
}

// Cache keeps recently opened transcripts keyed by session. Entries are
// immutable; a session reloads when its backing store changes size or mtime.
type Cache struct {
	mu    sync.Mutex
	max   int
	items map[string]*cacheItem
	order []string
}

// NewCache builds a cache holding at most max transcripts.
func NewCache(max int) *Cache {
	if max < 1 {
		max = 1
	}
	return &Cache{max: max, items: map[string]*cacheItem{}}
}

// Load returns the cached transcript or parses and caches it.
func (c *Cache) Load(agentID string, info SessionInfo) ([]Entry, error) {
	key := agentID + ":" + info.ID
	signature := transcriptSignature(info)

	c.mu.Lock()
	if item, ok := c.items[key]; ok && item.signature == signature {
		c.touch(key)
		entries := item.entries
		c.mu.Unlock()
		return entries, nil
	}
	c.mu.Unlock()

	entries, err := LoadTranscript(agentID, info)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; !ok {
		c.order = append(c.order, key)
	}
	c.items[key] = &cacheItem{signature: signature, entries: entries}
	for len(c.order) > c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.items, oldest)
	}
	return entries, nil
}

func (c *Cache) touch(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(append(c.order[:i:i], c.order[i+1:]...), key)
			return
		}
	}
}

// transcriptSignature changes when the session store is appended to. SQLite
// stores are watched through the main file and its WAL.
func transcriptSignature(info SessionInfo) string {
	if info.FilePath != nil && *info.FilePath != "" {
		raw := *info.FilePath
		if i := strings.Index(raw, "#"); i >= 0 {
			signature := fileSignature(raw[:i])
			signature += "+" + fileSignature(raw[:i]+"-wal")
			return signature
		}
		return fileSignature(raw)
	}
	return fmt.Sprintf("u:%d:s:%d", int64(info.UpdatedAt), int64(info.SizeBytes))
}

func fileSignature(path string) string {
	if stat, err := os.Stat(path); err == nil {
		return fmt.Sprintf("%d:%d", stat.Size(), stat.ModTime().UnixMilli())
	}
	return "-"
}

// ---- shared helpers ----

var (
	cursorTimestampRe = regexp.MustCompile(`(?s)<timestamp>.*?</timestamp>`)
)

// ScanMaxTranscriptLine bounds one JSONL record; tool results can be large.
const ScanMaxTranscriptLine = 32 * 1024 * 1024

func scanJSONLines(file string, visit func(map[string]any)) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), ScanMaxTranscriptLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) == nil {
			visit(obj)
		}
	}
	return scanner.Err()
}

func transcriptFile(info SessionInfo) (string, error) {
	if info.FilePath == nil || *info.FilePath == "" || strings.Contains(*info.FilePath, "#") {
		return "", fmt.Errorf("session %q has no transcript file", info.ID)
	}
	return *info.FilePath, nil
}

func readJSONFileAny(file string) (any, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

func mapTimestamp(obj map[string]any, seconds bool) float64 {
	for _, key := range []string{"timestamp", "time", "createdAt", "created_at"} {
		if v, ok := obj[key]; ok {
			var ts *float64
			if seconds {
				ts = SecToMs(v)
			} else {
				ts = AsEpochMs(v)
			}
			if ts != nil {
				return *ts
			}
		}
	}
	if message := GetMap(obj["message"]); message != nil {
		return mapTimestamp(message, seconds)
	}
	if payload := GetMap(obj["payload"]); payload != nil {
		return mapTimestamp(payload, seconds)
	}
	if data := GetMap(obj["data"]); data != nil {
		return mapTimestamp(data, seconds)
	}
	return 0
}

func cleanAssistantText(raw string) string {
	return strings.TrimSpace(raw)
}

var userScaffoldingPrefixes = []string{
	"base directory for this skill:",
	"# agents.md instructions",
	"please implement this plan:",
}

// cleanUserText drops XML/command scaffolding that agents inject as user turns.
func cleanUserText(raw string) string {
	s := strings.TrimSpace(raw)
	s = stripLeadingSystemReminder(s)
	s = cursorTimestampRe.ReplaceAllString(s, "")
	if m := cursorUserQueryRe.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "<") {
		return ""
	}
	lower := strings.ToLower(s)
	for _, prefix := range userScaffoldingPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return ""
		}
	}
	return s
}

// entriesFromContent converts a message content value into transcript entries.
func entriesFromContent(role Role, content any, ts float64) []Entry {
	if content == nil {
		return nil
	}
	if text, ok := content.(string); ok {
		if role == RoleUser {
			if text = cleanUserText(text); text == "" {
				return nil
			}
		} else if text = cleanAssistantText(text); text == "" {
			return nil
		}
		return []Entry{{Role: role, Text: text, Timestamp: ts}}
	}
	if wrapped := GetMap(content); wrapped != nil {
		if inner, ok := wrapped["text"]; ok {
			return entriesFromContent(role, inner, ts)
		}
		if inner, ok := wrapped["content"]; ok {
			return entriesFromContent(role, inner, ts)
		}
		return nil
	}

	blocks := GetSlice(content)
	if blocks == nil {
		return nil
	}
	var out []Entry
	var texts []string
	flush := func() {
		if len(texts) == 0 {
			return
		}
		text := strings.Join(texts, "\n")
		if role == RoleUser {
			text = cleanUserText(text)
		} else {
			text = cleanAssistantText(text)
		}
		texts = nil
		if text != "" {
			out = append(out, Entry{Role: role, Text: text, Timestamp: ts})
		}
	}
	for _, raw := range blocks {
		if text, ok := raw.(string); ok {
			texts = append(texts, text)
			continue
		}
		block := GetMap(raw)
		if block == nil {
			continue
		}
		kind := firstString(block, "type")
		switch kind {
		case "text", "input_text", "output_text":
			if text := AsString(block["text"]); text != nil {
				texts = append(texts, *text)
			}
		case "thinking", "reasoning", "think":
			flush()
			if text := firstString(block, "thinking", "think", "reasoning", "text"); text != "" {
				out = append(out, Entry{Role: role, Thinking: clipText(text, MaxThinkingChars), Timestamp: ts})
			}
		case "tool_use", "tool_call", "toolCall", "tool-call", "function_call":
			flush()
			if call, ok := toolFromBlock(block); ok {
				out = append(out, Entry{Role: RoleAssistant, Tools: []ToolCall{call}, Timestamp: ts})
			}
		case "redacted_thinking", "tool_result", "image":
			// Not entries: redacted thinking carries no text, and raw tool
			// results are matched back onto their tool calls by callers
			// that know the pairing (see applyToolResults).
		}
	}
	flush()
	return out
}

func toolFromBlock(block map[string]any) (ToolCall, bool) {
	name := firstString(block, "name", "tool", "tool_name")
	if name == "" {
		return ToolCall{}, false
	}
	input := GetMap(block["input"])
	if input == nil {
		input = GetMap(block["arguments"])
	}
	if input == nil {
		input = GetMap(block["args"])
	}
	if input == nil {
		if raw := AsString(block["arguments"]); raw != nil {
			input = map[string]any{}
			_ = json.Unmarshal([]byte(*raw), &input)
		}
	}
	call := toolCall(name, input)
	if id := firstString(block, "id", "tool_use_id"); id != "" {
		call.ID = id
	}
	if input != nil {
		call.Input = clipText(compactJSON(input), MaxToolInputChars)
	}
	return call, true
}

// Bounds for the optional raw payloads kept on entries and tool calls, in
// runes: one giant result must not bloat the transcript cache.
const (
	MaxThinkingChars   = 2048
	MaxToolInputChars  = 2048
	MaxToolOutputChars = 4096
)

// clipText bounds a raw payload while keeping its line breaks, marking the cut.
func clipText(s string, limit int) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// compactJSON serializes a value as single-line JSON, or "" when it cannot.
func compactJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

var fileTools = map[string]bool{
	"read": true, "edit": true, "write": true, "multiedit": true, "notebookedit": true,
	"read_file": true, "write_file": true, "edit_file": true, "apply_patch": true,
	"str_replace_editor": true, "create_file": true,
}

var searchTools = map[string]bool{
	"grep": true, "glob": true, "websearch": true, "webfetch": true,
	"grep_search": true, "file_search": true, "web_search": true, "web_fetch": true,
}

// codexCommands are Codex rollout's shell tools: their arguments carry the
// command line, so they map to ToolCommand and a command title.
var codexCommands = map[string]bool{
	"exec_command": true, "local_shell": true, "shell": true,
}

// toolCall builds a display card from a tool name and its parsed input.
func toolCall(name string, input map[string]any) ToolCall {
	lower := strings.ToLower(name)
	kind := ToolOther
	switch {
	case fileTools[lower]:
		kind = ToolFile
	case lower == "bash" || lower == "shell" || lower == "terminal" ||
		lower == "run_terminal_cmd" || lower == "execute_command" || lower == "run_command" ||
		codexCommands[lower]:
		kind = ToolCommand
	case searchTools[lower]:
		kind = ToolSearch
	}

	var title string
	switch kind {
	case ToolFile:
		title = firstString(input, "file_path", "filePath", "path", "notebook_path", "target_file", "filename")
	case ToolCommand:
		title = firstString(input, "command", "cmd", "script", "commandLine")
		if title == "" {
			title = commandArrayTitle(input)
		}
	case ToolSearch:
		title = firstString(input, "pattern", "query", "url", "path", "glob")
	}
	if title == "" {
		title = firstString(input, "description", "prompt", "skill", "args")
	}
	if title == "" {
		title = name
	}
	title = clipLine(title, 160)

	meta := name
	if kind == ToolCommand {
		if desc := firstString(input, "description"); desc != "" {
			meta = name + " | " + clipLine(desc, 80)
		}
	}
	return ToolCall{Name: name, Title: title, Meta: meta, Kind: kind}
}

// commandArrayTitle joins Codex's cmd argument array ("cmd": ["ls", "-la"])
// into one command line.
func commandArrayTitle(input map[string]any) string {
	parts, ok := input["cmd"].([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for i, p := range parts {
		s, ok := p.(string)
		if !ok {
			return ""
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(s)
	}
	return b.String()
}

// firstString returns the first non-empty string field among keys.
func firstString(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, key := range keys {
		if s := AsString(m[key]); s != nil && strings.TrimSpace(*s) != "" {
			return *s
		}
	}
	return ""
}

func clipLine(s string, limit int) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\r", "\n")), " ")
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// roleFromString maps common role spellings.
func roleFromString(s string) Role {
	switch strings.ToLower(s) {
	case "user", "human", "input":
		return RoleUser
	case "assistant", "agent", "model", "gemini", "output":
		return RoleAssistant
	case "system", "developer":
		return RoleSystem
	}
	return ""
}

// messageRole picks a role from an object that may wrap the message.
func messageRole(obj map[string]any) Role {
	if role := roleFromString(firstString(obj, "role")); role != "" {
		return role
	}
	if message := GetMap(obj["message"]); message != nil {
		if role := roleFromString(firstString(message, "role")); role != "" {
			return role
		}
	}
	if payload := GetMap(obj["payload"]); payload != nil {
		if role := roleFromString(firstString(payload, "role")); role != "" {
			return role
		}
	}
	return ""
}

// messageContent picks the content value of an object that may wrap it.
func messageContent(obj map[string]any) any {
	if message := GetMap(obj["message"]); message != nil {
		if content, ok := message["content"]; ok && content != nil {
			return content
		}
	}
	if content, ok := obj["content"]; ok && content != nil {
		return content
	}
	if payload := GetMap(obj["payload"]); payload != nil {
		if content, ok := payload["content"]; ok && content != nil {
			return content
		}
	}
	return nil
}

// genericEntries extracts a message from loosely shaped JSONL records.
func genericEntries(obj map[string]any) []Entry {
	ts := mapTimestamp(obj, false)
	role := messageRole(obj)
	if role == "" {
		switch strings.ToLower(firstString(obj, "type")) {
		case "user", "user_message", "human", "human_message":
			role = RoleUser
		case "assistant", "agent", "agent_message", "model", "model_message":
			role = RoleAssistant
		case "system":
			role = RoleSystem
		}
	}
	if role == "" {
		return nil
	}
	content := messageContent(obj)
	if content == nil {
		if text, ok := obj["message"].(string); ok {
			content = text
		}
	}
	if content == nil {
		if text, ok := obj["text"].(string); ok {
			content = text
		}
	}
	if content == nil {
		return nil
	}
	return entriesFromContent(role, content, ts)
}
