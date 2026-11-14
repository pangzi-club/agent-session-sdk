# Cursor — session store format & parsing

How the `cursor` agent stores sessions on disk, and how this SDK turns that
store into `SessionInfo` and `Entry[]`. Cursor keeps **no local usage data**, so
there is no `UsageRecord[]` extraction and no cost accounting.

This doc follows the shared section order: at a glance → on-disk layout → record
format → session scanning → transcript parsing → aggregation & cost → quirks →
source map. The "Usage extraction" section is omitted because the agent has no
usage module.

## At a glance

| Property | Value |
|---|---|
| Agent id | `cursor` |
| Display name | Cursor |
| Store directory | `~/.cursor` |
| `canDeleteSessions` | `false` (the library only reports this; deletion is the consumer's job) |
| Session scanner | `ScanCursor` (`cursor.go`) |
| Transcript loader | `parseCursorTranscript` (`transcript_cursor.go`) |
| Usage module | none — no local usage data |
| Billing | n/a (no usage records; no token tuple, no cost, no credits) |
| Store format | one JSONL transcript per session, nested under a per-project directory |

## On-disk layout

```
~/.cursor/
  projects/
    Users-alice-code-app/            # one directory per project (encoded cwd)
      agent-transcripts/
        <uuid>/                      # one directory per session
          <uuid>.jsonl               # transcript, same name as its directory
        <uuid>/
          <uuid>.jsonl
    Users-alice-code-other/
      ...
```

- **One directory per project.** The name is the project's absolute path with
  the leading `/` dropped and every `/` replaced by `-`, so `/Users/alice/code/app`
  becomes `Users-alice-code-app`. `decodeCursorProjectDirName` (`cursor.go:68`)
  reverses it: prepend `/`, then replace every `-` with `/`. This is lossy — a
  real segment containing `-` decodes to extra separators (`my-app` → `my/app`).
  Unlike Claude's `DecodePathDirName`, it does **not** trim leading/trailing `--`
  and does **not** require a leading `-`.
- **One directory per session** under `agent-transcripts/`. The session id is the
  directory name (a uuid). The transcript file must be named `<id>.jsonl` inside
  it; a directory without that same-named file is skipped as a non-session /
  incomplete directory.
- Dot-prefixed project and session directories are skipped (`ListDirsE`). A
  missing `projects/` root is an empty store; any other listing error fails
  the Cursor scan and is reported per agent by `ScanSessionsDetailed`.

## Record format (JSONL)

Each line is one JSON object. Blank lines and lines that fail to parse are
silently skipped (`scanJSONLines`). The fields the SDK reads:

| Field | Type | Meaning |
|---|---|---|
| `role` | string | `user` / `assistant` / `system` (also `human`, `agent`, `model`, … via `roleFromString`); a record with no recognized role is skipped |
| `message` | object | `{ content }` wrapper |
| `message.content` | string \| array | Text, or an array of content blocks |
| `timestamp` | string (RFC3339) \| number | Optional event time (`mapTimestamp`); scanning uses file mtime instead |

Non-message records such as `{"type":"turn_ended","status":"success"}` carry no
role and are skipped by the transcript loader.

### Content blocks

`message.content` is a plain string or an array of typed blocks, handled by the
shared `entriesFromContent`:

| Block `type` | Transcript treatment |
|---|---|
| `text` / `input_text` / `output_text` | Rendered as text (consecutive text blocks joined with `\n`) |
| `tool_use` / `tool_call` / `toolCall` / `function_call` | Rendered as a tool card |
| `thinking` / `reasoning` / `redacted_thinking` | Dropped (reasoning is noise) |
| `tool_result` | Dropped (raw tool output is noise) |
| `image` | Dropped |

### Illustrative lines

Synthetic, for shape only:

```jsonl
{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Friday</timestamp>\n<user_query>\n# Add dark mode\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"On it."}]}}
{"type":"turn_ended","status":"success"}
```

The user text yields the entry `# Add dark mode` (timestamp stripped, query
unwrapped); the assistant text yields `On it.`; the `turn_ended` line is dropped.

## Session scanning → `SessionInfo`

`ScanCursor` (`cursor.go:27`) lists `projects/`, then each project's
`agent-transcripts/`, collecting `(projectDir, sessionDir)` pairs. It scans them
with `MapLimit` at `FileConcurrency` (16), preserving input order; any error
fails the whole batch. Per pair (`scanCursorSessionDir`, `cursor.go:47`):

- `id = basename(sessionDir)`; `file = sessionDir/<id>.jsonl`.
- `stat(file)`; if it is missing, skip the directory (return nil) — this is how
  non-session / incomplete directories are filtered out. Any other stat error
  fails the scan.
- `title = ScanLinesE(file, pickCursorTitle, ScanMaxBytes)` — stream the file
  (32 MiB cap) until the first user message yields a title; a read failure
  fails the scan.
- `workspace = decodeCursorProjectDirName(basename(projectDir))`.
- `createdAt = updatedAt = FloorMtimeMs(file)` — the file mtime is the **only**
  time source; record timestamps are not read for scanning.
- `sizeBytes` = file size; `filePath` = the transcript file.

### Title selection (`pickCursorTitle`, `cursor.go:73`)

- Require top-level `role == "user"` exactly (`IsStr`).
- `text = ContentToText(message.content)` (`shared.go:310`): a string, or an
  array of `{text}` blocks joined with a **space**; an empty result is nil.
- If `cursorUserQueryRe` (`(?s)<user_query>(.*?)</user_query>`, `cursor.go:19`)
  matches, take the inner group; otherwise keep the full text.
- `FirstLineTitle` (`shared.go:277`): first non-blank line, truncated to 120
  UTF-16 code units (117 + `...`). Unlike `ToTitle`, it does **not** discard
  injected text, so a title starting with `# ` or `<` is kept (e.g.
  `# Add dark mode`).
- The title path does **not** strip `<timestamp>`; only the transcript cleaner
  does. With no `<user_query>` tag, a leading timestamp line can become the
  title.

## Transcript parsing → `Entry[]`

`parseCursorTranscript` (`transcript_cursor.go:5`):

- Locate the file via `SessionInfo.FilePath` (`transcriptFile` rejects a nil /
  empty path or a `#` SQLite suffix).
- Stream it with `scanJSONLines` (per-line cap `ScanMaxTranscriptLine`, 32 MiB);
  blank and unparseable lines are skipped. Per record:
  - `role = messageRole(obj)` (`transcript.go:419`): top-level `role`, else
    `message.role`, else `payload.role`, mapped by `roleFromString`. An empty
    role skips the record (e.g. `turn_ended`).
  - `content = messageContent(obj)` (`transcript.go:437`): `message.content`,
    else top-level `content`, else `payload.content`. nil skips the record.
  - `entriesFromContent(role, content, mapTimestamp(obj, false))`.
- `mapTimestamp` reads `timestamp` / `time` / `createdAt` / `created_at` (number
  or RFC3339 string), recursing into `message` / `payload` / `data`; `0` when
  none is found.

### From content to entries (`entriesFromContent`, `transcript.go:238`)

- A **string** becomes one text entry (after cleaning).
- A **wrapped object** with `text` or `content` recurses into it.
- An **array of blocks** is walked in order: consecutive text blocks are joined
  with `\n` into one entry; each tool block first flushes pending text, then
  emits its own assistant entry carrying exactly one `ToolCall`.

An `Entry` is always either text or tools, never both. Text and tools from the
same record share that record's timestamp.

### User-text cleaning (`cleanUserText`, `transcript.go:217`)

Trim; strip a leading `<system-reminder>`; strip `<timestamp>…</timestamp>`
(`cursorTimestampRe`, `transcript.go:134` — global, dotall); unwrap the first
`<user_query>…</user_query>`; trim again; drop the result if empty or starting
with `<`; drop known scaffolding prefixes (`base directory for this skill:`,
`# agents.md instructions`, `please implement this plan:`). Assistant text is
only trimmed (`cleanAssistantText`).

### Tool cards (`toolCall`, `transcript.go:344`)

`toolFromBlock` reads the name from `name` / `tool` / `tool_name` and the input
from `input` / `arguments` / `args` (or a JSON-string `arguments`). Cursor tool
names map to kinds via the shared tables:

- **`file`** — `read_file`, `write_file`, `edit_file`, `apply_patch`,
  `str_replace_editor`, `create_file`, …
- **`command`** — `run_terminal_cmd`, `execute_command`, `run_command`, `bash`,
  `shell`, `terminal`.
- **`search`** — `grep_search`, `file_search`, `web_search`, `web_fetch`, `grep`,
  `glob`, …
- else **`other`**.

`title` is the path / command / query (falling back to
`description`/`prompt`/`skill`/`args`, then the tool name), clipped to 160 runes;
`meta` is the tool name, or `name | description` for commands (description
clipped to 80).

## Aggregation & cost

- Cursor has **no usage module**: it is absent from `usageModules`
  (`usage.go:25`), so `ScanAllUsage` produces no `UsageRecord`s and no
  `UsageSession` for it.
- `SessionInfo.TokenUsage` stays nil (omitted from JSON). There is no token
  5-tuple, no `EstimateTokenCost` pricing and no `credits`.
- The session list still comes from `ScanSessions` (`registry.go`), which
  reports `canDeleteSessions = false` for cursor.

## Quirks & edge cases

- **mtime is the only scan timestamp.** `createdAt` and `updatedAt` are both the
  transcript file's mtime; record timestamps are ignored when scanning.
- **Same-named file required.** A session directory without `<id>.jsonl` is
  skipped, not treated as an empty session.
- **Lossy workspace decode.** `-` in a real path segment becomes `/`; there is
  no `--` handling and no leading-`-` requirement (differs from Claude).
- **Title keeps injected text.** `FirstLineTitle` (not `ToTitle`) is used, so
  `# Add dark mode` is a valid title rather than being discarded.
- **Title vs. transcript cleaning differ.** The title path unwraps
  `<user_query>` but does not strip `<timestamp>`; the transcript cleaner does
  both.
- **Role-less records are skipped** in the transcript (`turn_ended`, etc.).
- **`<user_query>` unwrap is non-greedy, first match only**, in both the title
  and transcript paths.

## Source map

| Topic | Location |
|---|---|
| Registry entry | `registry.go:25` |
| Session scan | `cursor.go` (`ScanCursor`, `scanCursorSessionDir`) |
| Workspace decode | `cursor.go` (`decodeCursorProjectDirName`) |
| Title selection | `cursor.go` (`pickCursorTitle`, `cursorUserQueryRe`); `shared.go` (`FirstLineTitle`, `ContentToText`, `ScanLinesE`) |
| Transcript loader | `transcript_cursor.go` (`parseCursorTranscript`) |
| Role / content resolution | `transcript.go` (`messageRole`, `messageContent`, `roleFromString`) |
| Content → entries, tool cards, cleaning | `transcript.go` (`entriesFromContent`, `toolCall`, `cleanUserText`, `cursorTimestampRe`) |
| Filesystem / concurrency helpers | `shared.go` (`ListDirsE`, `MapLimit`, `FloorMtimeMs`) |
| Usage aggregation (cursor absent) | `usage.go` (`usageModules`, `ScanAllUsageDetailed`) |
| Output contract | `contract.go` (`SessionInfo`, `Entry`, `ToolCall`) |