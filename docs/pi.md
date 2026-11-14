# Pi — session store format & parsing

How the `pi` agent stores sessions on disk, and how this SDK turns that store
into `SessionInfo`, `Entry[]` and `UsageRecord[]`.

Shared helpers (`entriesFromContent`, `toolCall`, `cleanUserText`, `ToTitle`,
`SessionTally`, aggregation) are documented in full in [claude.md](claude.md);
this doc covers them only where Pi relies on a specific behavior.

## At a glance

| Property | Value |
|---|---|
| Agent id | `pi` |
| Display name | Pi |
| Store directory | `~/.pi` |
| `canDeleteSessions` | `false` (the library only reports this; deletion is the consumer's job) |
| Session scanner | `ScanPi` (`pi.go`) |
| Transcript loader | `parsePiTranscript` (`transcript_cursor.go:27`) — lives in the Cursor loader's file, but is a separate function |
| Usage module | `scanPiUsage` (`usage_pi.go`) |
| Billing | Token-metered against model rates; no credit accounting (`credits` omitted) |
| Store format | Plain JSONL transcript per session; only the first 64 KiB is parsed for the session list |

## On-disk layout

```
~/.pi/
  agent/
    sessions/
      --Users-alice-code-app--/         # one directory per project (encoded cwd)
        <sessionFile>.jsonl             # one transcript per session
        <sessionFile>.jsonl
      --Users-alice-code-other--/
        ...
```

- **Session root is `agent/sessions`, scanned recursively.** `ScanPi` calls
  `ListFilesE(agentDir/agent/sessions, ".jsonl", true)` (`shared.go`): every
  regular `.jsonl` file at any depth is a candidate. Dot-prefixed *files* are
  skipped, but the recursion descends into every subdirectory, including
  dot-prefixed ones. A missing root is an empty store; any other listing error
  (including inside a nested directory) fails the Pi scan and is reported per
  agent by `ScanSessionsDetailed`.
- **Project directory names are encoded paths, but the SDK never decodes
  them.** They follow the same `--…--` convention as Claude, yet `pi.go` has no
  `DecodePathDirName` call site (only `claude.go` and `qoder.go` call it, despite
  the helper's comment mentioning Pi). The workspace comes exclusively from the
  `cwd` field of the session header *inside* the file; a file without a usable
  header is invisible no matter what its directory is named.
- **One JSONL file per session.** The session id is the file name minus
  `.jsonl`.

## Record format (JSONL)

Each line is one JSON object. Blank lines and lines that fail to parse are
silently skipped (`ParseLines` / `WalkJSONLFile`). The session list parses only
the first 64 KiB; the transcript and usage paths stream the whole file. The
fields the SDK reads:

| Field | Type | Meaning |
|---|---|---|
| `type` | string | `session` (header line) or `message`; other lines are ignored |
| `version` | number | On `session` lines: must equal exactly `3` |
| `cwd` | string | On `session` lines: project working directory (required, non-empty) |
| `timestamp` | string (RFC3339) or number | Line-level event time; on the header line it becomes `createdAt` |
| `message` | object | On `message` lines: the actual chat message |
| `message.role` | string | `user` / `assistant` (the transcript also accepts synonyms, see below) |
| `message.timestamp` | number (epoch ms) | Per-message time; preferred by the usage extractor |
| `message.content` | string \| array | Text, or an array of content blocks |
| `message.model` | string | Model id for the assistant turn |
| `message.usage` | object | Per-turn token counts (assistant only) |

### Content blocks

`message.content` goes through the shared `entriesFromContent`
(`transcript.go:238`). Pi's tool blocks use the camelCase kind `toolCall`:
`{ type: "toolCall", name, arguments }`. Text-like kinds are `text`,
`input_text`, `output_text`; tool-like kinds are `tool_use`, `tool_call`,
`toolCall`, `function_call`; `thinking`, `reasoning`, `redacted_thinking`,
`tool_result` and `image` blocks are dropped.

### `message.usage`

| Key | Maps to |
|---|---|
| `input` | `InputTokens` (non-cached input only) |
| `output` | `OutputTokens` |
| `cacheRead` | `CacheReadTokens` |
| `cacheWrite` | `CacheWriteTokens` |

Note the camelCase keys, unlike Claude's snake_case.

### Illustrative lines

Synthetic, for shape only:

```json
{"type":"session","version":3,"id":"0195c7a2-…","cwd":"/Users/alice/code/app","timestamp":"2026-03-18T14:12:58.960Z"}
{"type":"message","timestamp":"2026-03-18T14:13:01.000Z","message":{"role":"user","timestamp":1773843181000,"content":[{"type":"text","text":"Check the weather"}]}}
{"type":"message","timestamp":"2026-03-18T14:13:04.000Z","message":{"role":"assistant","timestamp":1773843184000,"model":"claude-sonnet-4","content":[{"type":"text","text":"Looking it up."},{"type":"toolCall","name":"bash","arguments":{"command":"curl wttr.in"}}],"usage":{"input":1200,"output":48,"cacheRead":8000,"cacheWrite":0}}}
```

## Session scanning → `SessionInfo`

`ScanPi` (`pi.go:15`) lists the files recursively, then runs `ScanPiFile` over
them with `MapLimit` at `FileConcurrency` (16, sharing the process-wide
file-work budget). Errors are **not** isolated per file: `MapLimit` fails the
whole batch on the first error, `ScanPi` returns it, and `ScanSessionsDetailed`
reports Pi as an `AgentScanFailure` while other agents keep their results. The
legacy `ScanSessions` shows an empty list for the failed agent.

`ScanPiFile` (`pi.go:31`):

1. `os.Stat` for size/mtime, then `ReadHeadLines(file, 64*1024)`
   (`shared.go`). Only the first 64 KiB is read and parsed; the last line may
   be cut mid-JSON and is then skipped by the parser (the cap bounds both I/O
   and parsing).
2. **Metadata pass** — `ParseLines` finds the first `type == "session"` line
   whose numeric `version` equals exactly `3` and whose `cwd` is a non-empty
   string; `createdAt = AsEpochMs(timestamp)`. No such line in the head → the
   file is skipped entirely (`nil, nil`, not an error).
3. **Title pass** — a second `ParseLines` over the same head lines: the first
   `type == "message"` line with `message.role == "user"` that yields a usable
   `ToTitle(ContentToText(message.content))`. `ContentToText` (`shared.go:310`)
   returns a string content as-is, or joins the `text` fields of array blocks
   with a single space. When `ToTitle` rejects a candidate (blank, or injected
   text starting with `<` or `# `), the pass keeps looking at later user
   messages — an injected first prompt does not end the search.
4. Field derivation:

| `SessionInfo` field | Source |
|---|---|
| `id` | file name minus `.jsonl` |
| `workspace` | header `cwd` (never the decoded directory name) |
| `title` | title pass result, else null |
| `createdAt` | header `timestamp` (`AsEpochMs`) ?? file mtime |
| `updatedAt` | file mtime in ms (`FloorMtimeMs`) |
| `sizeBytes` | full file size from `stat` — not the 64 KiB head |
| `filePath` | the JSONL file |

`blank` is never set for Pi.

## Transcript parsing → `Entry[]`

`parsePiTranscript` (`transcript_cursor.go:27`) locates the file via
`SessionInfo.FilePath` (a `#` suffix marks a SQLite store and is rejected),
then streams it with `scanJSONLines` (per-line cap `ScanMaxTranscriptLine`,
32 MiB). Per record:

- Only `type == "message"` lines with a `message` object are considered; the
  `session` header and every other line type produce nothing.
- `role = roleFromString(message.role)` (`transcript.go:406`): `user` / `human`
  / `input` → user; `assistant` / `agent` / `model` / `gemini` / `output` →
  assistant; `system` / `developer` → system (case-insensitive). Any other role
  is skipped.
- `ts = mapTimestamp(message, false)` — prefers `message.timestamp` (a finite
  number passes through as epoch ms; an RFC3339 string is parsed), then
  `message.time` / `createdAt` / `created_at`. When that yields `0`, it falls
  back to `mapTimestamp(line, false)`, which reads the line-level `timestamp`
  (typically the ISO string).
- `entriesFromContent(role, message.content, ts)`.

### From content to entries

The shared rules apply (see [claude.md](claude.md)): a string becomes one
cleaned text entry; consecutive text blocks are joined with `\n`; each tool
block flushes pending text and emits its own entry carrying exactly one
`ToolCall` — always with role `assistant`, even inside a user-role message.
Reasoning and raw tool output never appear.

Pi-specific notes:

- The tool block kind is `toolCall`; `toolFromBlock` (`transcript.go:311`)
  reads the name from `name` / `tool` / `tool_name` and the input from the
  `input` / `arguments` / `args` object (or a JSON-string `arguments`). Pi's
  `arguments` object is read directly.
- Tool cards (`toolCall`, `transcript.go:344`) classify by lowercased name
  against the shared file/command/search sets, else `other`; the title is the
  kind's primary input field (e.g. `command` for `bash`), falling back to
  `description` / `prompt` / `skill` / `args`, then the tool name, clipped to
  160 runes. `meta` is the tool name, or `name | description` for commands.
- User text goes through `cleanUserText` (`transcript.go:217`): strip leading
  `<system-reminder>` blocks and `<timestamp>…</timestamp>`, unwrap the Cursor
  `<user_query>` envelope, drop results that are empty or start with `<`, drop
  the known scaffolding prefixes. Assistant text is only trimmed.

## Usage extraction → `UsageRecord[]`

`scanPiUsage` (`usage_pi.go:13`) re-enumerates the same files with the same
recursive `ListFilesE` call and runs `scanPiFileUsage` at concurrency 16. Per
file (`usage_pi.go:36`):

1. **Session gate.** `ScanPiFile` must return a session (valid version-3
   header within the first 64 KiB); otherwise the file contributes nothing.
2. `WalkJSONLFile` streams the file line by line; open/read failures fail the
   module (skipped malformed lines are still tolerated). Only
   `type == "message"` records proceed.
4. **Timestamp.** Prefer `message.timestamp` when it is a number `> 0` (epoch
   ms); otherwise fall back to `AsEpochMs(line.timestamp)` (number or RFC3339
   string). A *string* `message.timestamp` is ignored here — an asymmetry with
   the transcript path, which accepts it. If the result is `0`, the line is
   skipped entirely: no tally event, no record.
5. **User line** — `message.role == "user"` feeds a prompt event, then stops.
   There is no content filtering: every user-role message counts as a prompt,
   including injected ones.
6. **Assistant line** — feeds an assistant event; each content block with
   `type == "toolCall"` and a string `name` feeds a tool event with that name.
   No skill accounting: `SkillOfToolUse` is never called, so
   `skillCallsByName` is always null.
7. **Emit a record** only when `message.usage` exists: `sessionId` = the
   file-derived session id, `agentId` = `pi`, `model` = `message.model` (null
   when absent), `timestamp` as above, and the four token fields mapped through
   `AsTokenCount` (floors positive finite numbers, else `0`).
8. **Attribute activity** — `record.ApplyInterval(tally.TakeUntil(timestamp))`;
   after the file, `lastRecord.MergeInterval(tally.Flush())` folds trailing
   events into the last record.

An assistant line without `usage` still contributes tally events; its counts
land on the next usage-bearing record (or the last one via `Flush`). Roles
other than `user` / `assistant` feed neither the tally nor records.

### Active time & the tally

The shared `SessionTally` (`tally.go:36`) applies: events must arrive in
ascending time order (out-of-order events are dropped), each gap between
consecutive events counts at most `ActiveGapCapMs` (120 000 ms), and
`TakeUntil` / `Flush` drain prompt/tool/active-time increments onto the record
(`userPrompts`, `toolCalls`, `toolCallsByName`, `activeMs`).

## Aggregation & cost

- `ScanAllUsage` (`usage.go:45`) concatenates records in module order (Pi is
  sixth) and merges sessions into an ordered map keyed by bare `sessionId`
  (last-write-wins). Each session's `tokenUsage` is the 5-tuple
  `[input, output, cacheRead, cacheWrite, 3]`, where `3` is the status flag and
  input excludes cache traffic. `filePath` is tildified against `home`.
- Pi is token-billed: `EstimateTokenCost` prices a record from model rates
  (`NormalizeModelId` → `LookupModelPrice`). `credits` is omitted. A null or
  unresolvable `model` prices to 0 with every token counted as unpriced.

## Quirks & edge cases

- **No directory-name decoding.** Despite the `DecodePathDirName` comment
  mentioning Pi, the scanner never calls it; workspace comes only from the
  header `cwd`. No valid header → no session, regardless of directory name.
- **Version pinned to exactly 3.** A header with `version` 2 or 4 makes the
  file invisible (contrast DSH, which accepts a minimum version).
- **64 KiB head.** A header or first user message beyond the head means no
  session or no title; `sizeBytes` still reports the full file size, and
  `ReadHeadLines` reads at most 64 KiB of it.
- **One bad file zeroes the agent.** `stat`/read errors propagate through
  `MapLimit` and fail the whole Pi scan; `ScanSessionsDetailed` reports the
  failure per agent. A missing store directory, by contrast, is simply empty.
- **Timestamp asymmetry.** Usage honors `message.timestamp` only when numeric;
  the transcript also accepts it as an RFC3339 string. Usage skips lines whose
  resolved timestamp is `0`; the transcript emits them with timestamp `0`.
- **Every user message is a prompt.** No `<` / `Caveat:` filtering like
  Claude's; the title pass skips injected text but the prompt count does not.
- **Tool entries are always assistant-role**, even when the tool block sits in
  a user-role message.
- **Shared source file, distinct loader.** `parsePiTranscript` lives in
  `transcript_cursor.go` next to `parseCursorTranscript` but shares no logic
  with it beyond the common helpers.

## Source map

| Topic | Location |
|---|---|
| Registry entry | `registry.go:29` |
| Session scan | `pi.go` (`ScanPi`, `ScanPiFile`) |
| Transcript loader | `transcript_cursor.go:27` (`parsePiTranscript`) |
| Content → entries, tool cards, cleaning | `transcript.go` (`entriesFromContent`, `toolFromBlock`, `toolCall`, `cleanUserText`, `roleFromString`, `mapTimestamp`) |
| Usage extraction | `usage_pi.go` (`scanPiUsage`, `scanPiFileUsage`) |
| Activity tally | `tally.go` (`SessionTally`) |
| Aggregation & failure reporting | `usage.go` (`ScanAllUsageDetailed`, `ApplyInterval`, `MergeInterval`), `registry.go` (`AgentScanFailure`) |
| Shared helpers | `shared.go` (`ListFilesE`, `ReadHeadLines`, `ParseLines`, `WalkJSONLFile`, `MapLimit`, `ContentToText`, `ToTitle`, `AsEpochMs`, `AsTokenCount`, `FloorMtimeMs`) |
| Output contract | `contract.go` (`SessionInfo`, `UsageRecord`, `Entry`, `ToolCall`) |