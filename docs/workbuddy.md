# WorkBuddy — session store format & parsing

How the `workbuddy` agent stores sessions on disk, and how this SDK turns that
store into `SessionInfo`, `Entry[]` and `UsageRecord[]`.

WorkBuddy is a split store: session **metadata** lives in a SQLite database,
while the **transcript and usage body** live in a per-session JSONL file. The
scanner reads only the database; the JSONL contributes its size and path at
scan time and is parsed later by the WorkBuddy transcript loader and the usage
extractor. This doc follows the shared section order: at a glance → on-disk
layout → record format → session scanning → transcript parsing → usage
extraction → aggregation & cost → quirks → source map.

## At a glance

| Property | Value |
|---|---|
| Agent id | `workbuddy` |
| Display name | WorkBuddy |
| Store directory | `~/.workbuddy` |
| `canDeleteSessions` | `false` (`registry.go:28`; the library only reports this — deletion is the consumer's job) |
| Session scanner | `ScanWorkbuddy` (`workbuddy.go:18`) — reads the `sessions` table of `workbuddy.db` |
| Transcript loader | `parseWorkbuddyTranscript` (`transcript_workbuddy.go:10`) |
| Usage module | `scanWorkbuddyUsage` (`usage_workbuddy.go:19`) |
| Billing | Token-metered against model rates; no credit accounting (`credits` omitted) |
| Store format | SQLite metadata DB (`workbuddy.db`) + one JSONL transcript per session under `projects/` |

## On-disk layout

```
~/.workbuddy/
  workbuddy.db                          # SQLite: the sessions table is the metadata source
  projects/
    Users-alice-code-app/               # one directory per project (encoded cwd)
      <uuid>.jsonl                      # one transcript + usage body per session
      <uuid>.jsonl
    Users-alice-code-other/
      ...
```

- **The database is authoritative for metadata.** `ScanWorkbuddy` opens
  `workbuddy.db` read-only and runs `SELECT * FROM sessions`. id, workspace,
  title and timestamps all come from the row, never from the JSONL.
- **One JSONL file per session**, at
  `projects/<workbuddyProjectDirName(cwd)>/<id>.jsonl`. The scanner builds this
  path but does not open the file; it only stats it for `sizeBytes` and records
  it as `filePath`. The transcript loader and usage extractor read it later.
- **Workspace is the raw `cwd` from the database**, used verbatim — there is no
  directory-name decoding (unlike Claude). The encoding below is one-way and
  only used to locate the file.
- A missing JSONL is not fatal: `FileSize` returns `0`, `filePath` is still set,
  and the session is emitted from the database row alone.

### Project directory encoding (`workbuddyProjectDirName`, `workbuddy.go:95`)

Mirrors workbuddy-core's `compressWorkspacePathName`:

1. Replace every `/`, `\` and `:` with `-`.
2. Trim leading/trailing `-` and collapse runs of `-` (implemented as
   `strings.FieldsFunc` on `-` then rejoin), so `/Users/alice/code/app` →
   `Users-alice-code-app`.
3. If the result is ≤ 255 **bytes**, use it as-is. Otherwise truncate to 180
   bytes on a code-point boundary (`truncateToByteLimitOnCodePoint`) and append
   `-<djb2Base36(base)>`, where `djb2Base36` (`workbuddy.go:133`) is the djb2 hash
   `h = h*33 ^ byte` wrapping as int32, rendered as unsigned base36.

This is a different scheme from Claude's `DecodePathDirName`; it is not
reversible, which is fine because the workspace path comes from the DB.

## Record format (JSONL)

Each line of `<uuid>.jsonl` is one JSON object. The usage extractor recognizes
two `type` values; the generic transcript loader keys off `role`/`content`
instead (see below). Blank lines and lines that fail to parse are skipped.

| Field | Type | Meaning |
|---|---|---|
| `type` | string | `message` (a chat turn) or `function_call` (a tool invocation) |
| `role` | string | On `message` lines: `user` or `assistant` |
| `timestamp` | number (epoch ms) | Event time. Usage requires a finite number (floored); `0`/absent skips the line |
| `content` | string \| array | Message text, or an array of content blocks |
| `name` | string | On `function_call` lines: the tool name |
| `providerData` | object | Assistant-turn metadata: token usage, model id, prompt-injection flags |

### `providerData` token sources

Usage is read from `providerData.rawUsage` (provider-native snake_case) with
`providerData.usage` (camelCase) as fallback. A record is emitted only when at
least one of the two objects exists.

| Quantity | Primary (`rawUsage`) | Fallback |
|---|---|---|
| prompt (input + cache) | `prompt_tokens` | `usage.inputTokens` |
| output | `completion_tokens` | `usage.outputTokens` |
| cache read | `prompt_cache_hit_tokens` → `cache_read_input_tokens` | `usage.inputTokensDetails[0].cached_tokens` |
| cache write | `cache_creation_input_tokens` → `prompt_cache_write_tokens` | — |

`InputTokens` is then `prompt - cacheRead` (the prompt figure includes cache
hits; subtracting them yields the "excludes cache traffic" number). Model id is
`providerData.model`, else `providerData.requestModelId`, else null.

### Illustrative lines

Synthetic, for shape only:

```json
{"type":"message","role":"user","timestamp":1762939800000,"content":"Refactor the parser."}
{"type":"message","role":"assistant","timestamp":1762939802000,"content":[{"type":"text","text":"Reading the file."}],"providerData":{"model":"deepseek-v4-pro","usage":{"inputTokens":9200,"outputTokens":48,"inputTokensDetails":[{"cached_tokens":8000}]}}}
{"type":"function_call","name":"read_file","timestamp":1762939803000}
```

## Session scanning → `SessionInfo`

`ScanWorkbuddy` (`workbuddy.go:18`) opens `workbuddy.db` read-only
(`OpenSQLite`, `shared.go:534`; a missing/unopenable DB errors out and the agent
contributes nothing) and reads every row of `sessions` as a column→value map
(`QueryMaps`, `shared.go:548`; INTEGER→float64, TEXT→string, NULL→nil). Rows are
processed sequentially. Per row:

1. **Skip background automation** — `isBackgroundAutomation`
   (`workbuddy.go:61`) drops the row when `is_background_automation` is boolean
   `true` or the number `1`. An absent column keeps the row.
2. **Skip soft-deleted** — `isWorkbuddyDeleted` (`workbuddy.go:74`) keeps only
   `deleted_at` of NULL (local active) or `-1` (cloud active). **Every other
   value counts as deleted, including `0`** and any non-number.
3. **Require id and cwd** — both must be non-empty strings (`AsString`), else
   the row is skipped.
4. Derive the output fields:

| `SessionInfo` field | Source |
|---|---|
| `ID` | row `id` |
| `Workspace` | row `cwd`, verbatim (no decoding) |
| `Title` | `ToTitle(custom_title ?? title)`, where `workbuddyAsText` treats a blank string as null so `title` takes over when `custom_title` is empty |
| `CreatedAt` | `AsEpochMs(created_at)` ?? `0` |
| `UpdatedAt` | `AsEpochMs(updated_at)` ?? `CreatedAt` |
| `SizeBytes` | `FileSize(projects/<encoded cwd>/<id>.jsonl)` (0 if missing) |
| `FilePath` | that same JSONL path (always set) |

`AsEpochMs` (`shared.go:129`) passes a finite number through as-is (epoch ms) or
parses an RFC3339 string; there is **no file-mtime fallback** for timestamps.

### Title normalization (`ToTitle`, `shared.go:211`)

`workbuddyAsText` (`workbuddy.go:85`) returns a non-blank string as-is (no trim),
else null; `NonNil` picks `custom_title` first, then `title`. `ToTitle` then
skips leading blank lines, unwraps the Codex `## My request:` envelope and the
`PLEASE IMPLEMENT THIS PLAN:` preamble, treats a first line starting with `<`
or `# ` as injected (→ null), and truncates to 120 UTF-16 code units (117 +
`...`). Note this filter also applies to a user-set `custom_title`: one starting
with `# ` or `<` is discarded as injected.

## Transcript parsing → `Entry[]`

`parseWorkbuddyTranscript` (`transcript_workbuddy.go:10`) locates the file via
`transcriptFile` (`transcript.go:161`; rejects a nil/empty path or one containing
`#`, the SQLite-store marker), then streams it with `scanJSONLines`
(`transcript.go:140`, per-line cap `ScanMaxTranscriptLine` = 32 MiB). Per record:

1. `message` — `roleFromString(obj.role)` selects the role, and
   `entriesFromContent(role, obj.content, ts)` emits text and embedded thinking
   or tool blocks with the shared cleaners and cards.
2. `reasoning` — `rawContent` is preferred over `content`; text blocks are
   joined with blank lines, emitted as one assistant thinking entry, and clipped
   to `MaxThinkingChars`.
3. `function_call` — `name` and the decoded object or JSON-string arguments
   become a `ToolCall`; `callId` / `call_id` / `id` is kept for result matching.
4. `function_call_result` — the matching call is updated in place with the
   bounded output; `status`, `failed`, `is_error`, or result-level `failed`
   marks it failed.

### From content to entries (`entriesFromContent`, `transcript.go:238`)

- A **string** becomes one text entry (after cleaning).
- A **wrapped object** with `text` or `content` recurses into it.
- An **array of blocks** is walked in order: consecutive text blocks join with
  `\n` into one entry; each tool block first flushes pending text, then emits an
  assistant entry carrying exactly one `ToolCall`.
- Text-like kinds: `text`, `input_text`, `output_text`. Tool-like kinds:
  `tool_use`, `tool_call`, `toolCall`, `function_call`.
- Dropped kinds: `thinking`, `reasoning`, `redacted_thinking`, `tool_result`,
  `image`.

An `Entry` is always either text or tools, never both; text and tools from the
same record share its timestamp.

### User-text cleaning (`cleanUserText`, `transcript.go:217`)

Trim; strip a leading `<system-reminder>` block; strip `<timestamp>…</timestamp>`;
unwrap the Cursor `<user_query>` envelope; drop the result if empty or starting
with `<`; drop known scaffolding prefixes (`base directory for this skill:`,
`# agents.md instructions`, `please implement this plan:`). Assistant text is
only trimmed (`cleanAssistantText`).

### Tool cards (`toolCall`, `transcript.go:344`)

`toolFromBlock` reads the name from `name` / `tool` / `tool_name` and input from
`input` / `arguments` / `args` (or a JSON-string `arguments`). The card:

- **`kind`** — `file` (read/edit/write/multiedit/notebookedit/read_file/…),
  `command` (bash/shell/terminal/run_terminal_cmd/…), `search`
  (grep/glob/websearch/webfetch/…), else `other`.
- **`title`** — file: `file_path`/`path`/…; command: `command`/`cmd`/…; search:
  `pattern`/`query`/`url`/…; falling back to `description`/`prompt`/`skill`/`args`,
  then the tool name. Clipped to 160 runes.
- **`meta`** — the tool name; for commands, `name | description` (description
  clipped to 80).

## Usage extraction → `UsageRecord[]`

`scanWorkbuddyUsage` (`usage_workbuddy.go:19`) reuses `ScanWorkbuddy` for the
session list (so the same DB filters apply), then streams each session's JSONL
with `WalkJSONLFile` (malformed lines skipped, 32 MiB per-record cap, read
failures fail the module), bounded to `FileConcurrency` (16) sessions at once.
`scanWorkbuddySessionUsage` (`usage_workbuddy.go`) processes each record:

1. **Timestamp gate.** `timestamp = floor(obj.timestamp)` only when it is a
   finite **number** (`AsNumber`); a string timestamp is not accepted here.
   `0`/absent → skip the line. (Asymmetry: the transcript path also accepts
   RFC3339 strings.)
2. **`type == "function_call"`** → `tally.AddEvent(ts, TallyTool, obj.name)`;
   stop. No skill name is tracked, so `SkillCallsByName` stays null.
3. **`type != "message"`** → skip.
4. **`role == "user"`** → if `isWorkbuddyRealUserPrompt`, feed a prompt event;
   stop.
5. **`role == "assistant"`** → feed an assistant event; read
   `providerData`. If absent, or `readWorkbuddyUsage` reports no usage object,
   stop (the assistant event still accrues to the next record).
6. **Emit a record** with `SessionID = session.ID`, `AgentID = "workbuddy"`,
   `Model = readWorkbuddyModel(providerData)`, the floored `timestamp`, and the
   four token fields below.
7. `record.ApplyInterval(tally.TakeUntil(ts))` folds in activity accrued up to
   this timestamp. After the file, `lastRecord.MergeInterval(tally.Flush())`
   folds any trailing events into the last record.

### Token normalization (`readWorkbuddyUsage`, `usage_workbuddy.go:128`)

Returns `ok=false` when neither `rawUsage` nor `usage` exists (no record).
Otherwise, with `AsTokenCount` (floor positive finite numbers, else 0):

- `prompt` = `rawUsage.prompt_tokens`, else `usage.inputTokens`.
- `output` = `rawUsage.completion_tokens`, else `usage.outputTokens`.
- `cacheRead` = `rawUsage.prompt_cache_hit_tokens`, else
  `rawUsage.cache_read_input_tokens`, else
  `usage.inputTokensDetails[0].cached_tokens` (`cachedTokensDetail`).
- `cacheWrite` = `rawUsage.cache_creation_input_tokens`, else
  `rawUsage.prompt_cache_write_tokens`.
- **`InputTokens = prompt - cacheRead`** — no clamping, so it can go negative
  when cache hits exceed the reported prompt (same convention as codex/zcode).

### Prompt detection (`isWorkbuddyRealUserPrompt`, `usage_workbuddy.go:107`)

A user line is a prompt unless `providerData` flags it as injected or
compaction-internal: `skipRun`, `isMeta` or `isCompactInternal` true
(`AsBoolTrue`), or `compactType` present as a string. Otherwise
`ContentToText(obj.content)` (`shared.go:310`, joins a string or `{text}` blocks
with spaces) must be non-empty after trimming. Unlike Claude, there is **no
leading-`<` / `Caveat:` rejection** — an injected XML user line without a
`providerData` flag still counts as a prompt.

### Active time & the tally (`SessionTally`, `tally.go:36`)

Events must arrive in ascending time order; out-of-order events are dropped.
`ActiveMs` sums the gaps between consecutive events, each capped at
`ActiveGapCapMs` (120 000 ms = 2 min) so idle time never inflates active time.
`TakeUntil(ts)` drains events with `ts <= ts`; `Flush()` drains the rest. The
interval carries `UserPrompts`, `ToolCalls`, `ToolCallsByName` and `ActiveMs`
onto the record (`SkillCallsByName` is always null for WorkBuddy).

## Aggregation & cost

- `ScanAllUsageDetailed` (`usage.go`) concatenates records in module order and
  merges sessions into an ordered map keyed by bare `sessionId`
  (last-write-wins; the later module in aggregation order wins a shared id).
  Each session's `tokenUsage` is the 5-tuple
  `[input, output, cacheRead, cacheWrite, 3]` (`3` = status flag; input excludes
  cache traffic). `filePath` is tildified against `home`. A module error is
  fault-isolated: WorkBuddy then contributes no sessions or records, and the
  failure is reported as an `AgentScanFailure` (the legacy `ScanAllUsage`
  keeps the old empty-on-error behavior).
- WorkBuddy is token-billed: `EstimateTokenCost` (`pricing.go:189`) prices a
  record from model rates (`NormalizeModelId` → `LookupModelPrice`); `credits`
  is omitted. Negative `InputTokens` participates in the arithmetic as-is. A
  model missing from the price table makes the whole record unpriced (cost 0,
  all four token counts reported as unpriced).

## Quirks & edge cases

- **Metadata and body are separate stores.** The DB row alone produces a
  session; a missing JSONL just means `sizeBytes = 0` and an empty transcript /
  no usage records.
- **`deleted_at = 0` is treated as deleted.** Only NULL and `-1` are active;
  every other value (including `0` and non-numbers) is filtered out.
- **Background filter is exact.** `is_background_automation` must be boolean
  `true` or the number `1`; an absent column keeps the row.
- **Custom titles pass through the injected-text filter.** A `custom_title`
  starting with `# ` or `<` is discarded by `ToTitle`.
- **No timestamp mtime fallback.** `createdAt`/`updatedAt` come from the DB
  (`created_at` ?? 0; `updated_at` ?? `createdAt`).
- **Tool calls are usage-only.** Standalone `function_call` lines are not
  rendered by the generic transcript loader.
- **Usage timestamps must be numeric.** The transcript accepts numeric or
  RFC3339 timestamps; usage accepts numbers only.
- **Input can be negative** (`prompt - cacheRead`, unclamped).
- **Prompt detection ignores leading `<`.** Only `providerData` flags exclude
  injected/compaction user lines.
- **One-way path encoding.** `workbuddyProjectDirName` compresses (and hashes
  long paths); the workspace is never decoded from it.

## Source map

| Topic | Location |
|---|---|
| Registry entry | `registry.go:28` |
| Session scan (SQLite) | `workbuddy.go` (`ScanWorkbuddy`, `isBackgroundAutomation`, `isWorkbuddyDeleted`, `workbuddyAsText`) |
| Project dir encoding | `workbuddy.go` (`workbuddyProjectDirName`, `truncateToByteLimitOnCodePoint`, `djb2Base36`) |
| Transcript loader | `transcript_workbuddy.go` (`parseWorkbuddyTranscript`, `workbuddyReasoning`, `applyWorkbuddyToolOutput`, `workbuddyOutputText`) |
| Record timestamp | `transcript.go` (`mapTimestamp`) |
| Content → entries, tool cards, cleaning | `transcript.go` (`entriesFromContent`, `toolCall`, `cleanUserText`) |
| Usage extraction | `usage_workbuddy.go` (`scanWorkbuddyUsage`, `scanWorkbuddySessionUsage`, `isWorkbuddyRealUserPrompt`, `readWorkbuddyUsage`, `readWorkbuddyModel`, `cachedTokensDetail`) |
| Activity tally | `tally.go` (`SessionTally`) |
| Aggregation & failure reporting | `usage.go` (`ScanAllUsageDetailed`, `ApplyInterval`, `MergeInterval`), `registry.go` (`AgentScanFailure`) |
| Pricing | `pricing.go` (`EstimateTokenCost`, `NormalizeModelId`, `LookupModelPrice`) |
| Shared helpers | `shared.go` (`ToTitle`, `AsEpochMs`, `AsNumber`, `AsTokenCount`, `ContentToText`, `FileSize`, `OpenSQLite`, `QueryMaps`, `WalkJSONLFile`, `MapLimit`) |
| Output contract | `contract.go` (`SessionInfo`, `UsageRecord`, `Entry`, `ToolCall`) |
