# Codex — session store format & parsing

How the `codex` agent stores sessions on disk, and how this SDK turns that
store into `SessionInfo`, `Entry[]` and `UsageRecord[]`.

Codex differs from Claude in two structural ways: every record is an envelope
(`type` + `payload`) rather than a flat `user`/`assistant`/`message` object, and
its token counts are **session-cumulative**, so usage is recovered by diffing
adjacent events rather than reading per-turn numbers.

## At a glance

| Property | Value |
|---|---|
| Agent id | `codex` |
| Display name | Codex |
| Store directory | `~/.codex` |
| `canDeleteSessions` | `true` (the library only reports this; deletion is the consumer's job) |
| Session scanner | `ScanCodex` (`codex.go`) |
| Transcript loader | `parseCodexTranscript` (`transcript_codex.go`) |
| Usage module | `scanCodexUsage` (`usage_codex.go`) |
| Billing | Token-metered against model rates; no credit accounting (`credits` omitted) |
| Store format | One rollout JSONL per session, filed under a `sessions/YYYY/MM/DD` date tree |

## On-disk layout

```
~/.codex/
  sessions/
    2026/
      11/
        06/
          rollout-2026-11-06T03-00-00-<uuid>.jsonl   # one rollout per session
          rollout-2026-11-06T05-12-31-<uuid>.jsonl
        07/
          ...
```

- **A three-level date tree.** `ScanCodex` (`codex.go:14`) walks
  `sessions/` → year → month → day with `ListDirsE` at each level, then
  `ListFilesE(day, ".jsonl", false)` — non-recursive inside the day directory.
  A missing `sessions/` tree is an empty store; any other listing error fails
  the whole `codex` scan (reported per agent by `ScanSessionsDetailed`).
  The file name above is illustrative; the SDK never parses it.
- **One rollout JSONL per session.** The file name minus `.jsonl` is the session
  id (`strings.TrimSuffix(filepath.Base(file), ".jsonl")`). The whole base name
  (e.g. `rollout-…-<uuid>`) is treated as an opaque id.
- **No index, no encoded-path directories.** Unlike Claude there is no
  `sessions-index.json` fast path and no `DecodePathDirName` heuristic: the
  date tree carries no project identity, so the workspace is read from the
  `session_meta` payload's `cwd` (see below).
- Dot-prefixed directories and files are skipped (`ListDirsE` / `ListFilesE`).

## Record format (JSONL)

Each line is one JSON object. Blank lines and lines that fail to parse are
silently skipped. Every record is an envelope: a top-level `type`, a `payload`
object, and (on most lines) a top-level `timestamp`. The SDK reads four
top-level types:

| `type` | Read by | Meaning |
|---|---|---|
| `session_meta` | scan | `payload.cwd` + `payload.timestamp` → workspace + createdAt |
| `response_item` | scan, transcript, usage | `payload` is the actual item (message / function call / reasoning / …) |
| `turn_context` | usage | `payload.model` → model name (first one wins) |
| `event_msg` | usage | `payload.type == "token_count"` → cumulative token totals |

Common fields:

| Field | Type | Meaning |
|---|---|---|
| `timestamp` | string (RFC3339) | Event time, e.g. `2026-11-06T03:00:05.000Z` (top level) |
| `payload` | object | The record body; its own `type` selects the shape |

### `response_item` payloads

`payload.type` selects the item. The transcript switch handles `message`,
`reasoning`, `function_call`/`custom_tool_call` and
`function_call_output`/`custom_tool_call_output`; everything else is ignored.

| `payload.type` | Shape | Treatment |
|---|---|---|
| `message` | `{ type, role, content[] }` | Transcript text; scan title (role `user`); usage prompt/assistant event |
| `function_call` | `{ type, name, call_id, arguments }` | Tool card (`arguments` is usually a JSON string) |
| `custom_tool_call` | `{ type, name, call_id, arguments \| input }` | Tool card |
| `reasoning` | `{ type, summary[] }` | Thinking entry — the summary paragraphs join into `Entry.Thinking` |
| `function_call_output` | `{ type, call_id, output }` | Paired back onto the matching call's card: `Output`, `Failed` |
| `custom_tool_call_output` | `{ type, call_id, output }` | Same pairing |

`message.content` is an array of typed blocks, each `{ type, text }`:
`input_text` for user turns, `output_text` for assistant turns (plain `text` is
also accepted). These are the text-like kinds `entriesFromContent` renders.

### `event_msg` / `token_count`

`payload.info.total_token_usage` holds **cumulative session totals**, not
per-turn counts:

| Key | Maps to |
|---|---|
| `input_tokens` | Input **including** cached input (cache read is subtracted later) |
| `output_tokens` | `OutputTokens` |
| `cached_input_tokens` | `CacheReadTokens` |
| `cache_write_input_tokens` | `CacheWriteTokens` |

### Illustrative lines

Synthetic, for shape only:

```json
{"type":"session_meta","payload":{"cwd":"/Users/alice/code/app","timestamp":"2026-11-06T03:00:00.000Z"}}
{"type":"turn_context","payload":{"model":"gpt-5.3-codex"}}
{"timestamp":"2026-11-06T03:00:05.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Add a regression test"}]}}
{"timestamp":"2026-11-06T03:00:09.000Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":\"go test ./...\"}"}}
{"timestamp":"2026-11-06T03:00:12.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":12000,"output_tokens":300,"cached_input_tokens":8000,"cache_write_input_tokens":0}}}}
```

## Session scanning → `SessionInfo`

`ScanCodex` collects every rollout file from the date tree and scans up to
`FileConcurrency` (16) at once via `MapLimit`. Per file (`ScanCodexFile`,
`codex.go:40`) there are two independent streaming passes, each bounded by
`ScanMaxBytes` (32 MiB) through `ScanLines`:

1. **Metadata pass** — find the first `type == "session_meta"` line whose
   `payload.cwd` is a non-empty string; capture `cwd` and
   `createdAt = AsEpochMs(payload.timestamp)`. No such line → the file is
   skipped (no session). A failed `os.Stat` (file deleted after enumeration)
   also skips, not fatal.
2. **Title pass** — a separate scan for the first `type == "response_item"`
   whose `payload.type == "message"` and `payload.role == "user"`, returning
   `ToTitle(ContentToText(payload.content))`. `ScanLines` stops at the first
   non-nil result, so injected prompts (which `ToTitle` rejects → nil) are
   skipped automatically and the scan keeps looking for the first real prompt.

Output fields:

- `id` = file base name minus `.jsonl`.
- `workspace` = `meta.cwd` (authoritative; never decoded from the path).
- `title` = the title-pass result (may be nil).
- `createdAt` = `meta.createdAt` ?? file mtime (ms).
- `updatedAt` = `FloorMtimeMs(info)` (file mtime in ms).
- `sizeBytes` = file size; `filePath` = the file.

There is no fast path: every session is two streaming passes over the rollout.

### Title normalization (`ToTitle`, `shared.go:211`)

`ContentToText` (`shared.go:310`) first joins the `text` field of each content
block with a space. `ToTitle` then normalizes that string — this is where the
two Codex-specific envelopes are unwrapped:

- Split into lines and trim each. If any line equals `## My request:` exactly,
  keep only the lines **after** it. This unwraps the attachment envelope Codex
  emits when images / pasted text are present: the envelope header
  (`# Files mentioned by the user:`) starts with `# ` and would otherwise be
  treated as injected, losing the real request that follows the separator.
- Skip leading blank lines. If the first candidate line is exactly
  `PLEASE IMPLEMENT THIS PLAN:`, take the next non-blank line as the heading and
  unwrap a `**bold**` heading to its inner text (`planHeadingRe`).
- A first line starting with `<` or `# ` is injected → return nil (the scan
  moves on to the next user message). This rejects `<environment_context>…`
  and `# AGENTS.md instructions` turns.
- Truncate to 120 UTF-16 code units (117 + `...`). Truncation counts UTF-16
  units, so an emoji counts as 2, matching the original TypeScript `slice`.

Net effect: a session whose first user turn is an attachment envelope or a plan
preamble still gets a human title from the real request / plan heading, while
purely injected turns yield no title.

## Transcript parsing → `Entry[]`

`parseCodexTranscript` (`transcript_codex.go:7`) locates the file via
`transcriptFile` (`SessionInfo.FilePath`; a `#` suffix marks a SQLite store and
is rejected), then streams it with `scanJSONLines` (per-line cap
`ScanMaxTranscriptLine`, 32 MiB). Only `type == "response_item"` records are
considered; a nil `payload` is skipped. Per record:

- `ts = mapTimestamp(obj, false)` — reads `timestamp` / `time` / `createdAt` /
  `created_at`, recursing into `message` / `payload` / `data`; accepts a finite
  number or an RFC3339 string (`AsEpochMs`). `0` when none is found.
- **`payload.type == "message"`** — `role = roleFromString(payload.role)`
  (`user`/`assistant`/`system`/`developer` map to roles; anything else → skip),
  then `entriesFromContent(role, payload.content, ts)`.
- **`payload.type == "reasoning"`** — the `summary[]` paragraphs
  (`{type: "summary_text", text}`) join with a blank line into one assistant
  thinking entry (`Entry.Thinking`, clipped to `MaxThinkingChars`); no
  summaries → no entry.
- **`payload.type == "function_call"` or `"custom_tool_call"`** — read `name`
  (empty → skip); `input = payload.arguments ?? payload.input`; decode it with
  `argumentsMap`; emit one assistant `Entry` carrying exactly one
  `toolCall(name, input)` with `ID` from `payload.call_id` (falling back to
  `payload.id`).
- **`payload.type == "function_call_output"` or
  `"custom_tool_call_output"`** — `applyCodexToolOutput` matches `call_id`
  against the emitted cards and fills the first unpaired one: `Output` from
  the string (or `{output}` / first text of `{content[]}`), clipped to
  `MaxToolOutputChars`; `Failed` from the `Process exited with code N` header
  being nonzero. No matching call, empty id or empty text → no-op.
- **Any other payload type** produces no entry.

Unlike Claude there is **no compaction marker**: Codex emits no
`compact_boundary`-style system entry (Claude's fixed `上下文已压缩` literal has
no Codex counterpart). A `system`/`developer`-role message would simply render
as system text via `entriesFromContent`.

### From content to entries (`entriesFromContent`, `transcript.go:238`)

- A **string** becomes one text entry (after cleaning).
- A **wrapped object** with `text` or `content` recurses into it.
- An **array of blocks** is walked in order: consecutive text blocks are joined
  with `\n` into a single entry; each tool block first flushes pending text,
  then emits its own assistant entry carrying one `ToolCall`.
- Text-like kinds: `text`, `input_text`, `output_text` (Codex uses `input_text`
  for user content and `output_text` for assistant content). Tool-like kinds:
  `tool_use`, `tool_call`, `toolCall`, `function_call`.
- Dropped kinds: `thinking`, `reasoning`, `redacted_thinking`, `tool_result`,
  `image`.

An `Entry` is always either text or tools, never both. Text and tools from the
same record share that record's timestamp.

### User-text cleaning (`cleanUserText`, `transcript.go:217`)

Trim; strip a leading `<system-reminder>`; strip `<timestamp>…</timestamp>`;
unwrap the Cursor `<user_query>` envelope; drop the result if it is empty or
starts with `<`; drop known scaffolding prefixes (`base directory for this
skill:`, `# agents.md instructions`, `please implement this plan:`). Assistant
text is only trimmed (`cleanAssistantText`).

Codex-specific: the `<environment_context>…` injection is dropped here (it
starts with `<`). Note the `## My request:` envelope is **not** unwrapped in the
transcript — only `ToTitle` does that — so the transcript keeps the full user
text including the envelope, provided it does not start with `<` or a
scaffolding prefix.

### Tool cards (`toolCall`, `transcript.go:344`)

For Codex the input is `argumentsMap(payload.arguments ?? payload.input)`
(`transcript_codex.go:49`): a JSON object passes through; a JSON string is
unmarshalled; a parse failure yields nil. The card:

- **`kind`** — `file` (`apply_patch`, `read_file`, `write_file`, `edit_file`, …),
  `command` (`exec_command`, `local_shell`, `shell`, `bash`, `run_terminal_cmd`,
  `execute_command`, …), `search` (`grep`, `glob`, `web_search`, …), else
  `other`.
- **`title`** — for file: `file_path`/`path`/…; command: `command`/`cmd`/…
  (an array-valued `cmd`, Codex's usual shape, joins into one command line);
  search: `pattern`/`query`/`url`/…; falling back to
  `description`/`prompt`/`skill`/`args`, then the tool name. Clipped to 160 runes.
- **`meta`** — the tool name; for commands, `name | description` (description
  clipped to 80).

`firstString` accepts **string values only**; the array-valued `cmd` goes
through `commandArrayTitle` instead, which requires every element to be a
string or it yields no title.

## Usage extraction → `UsageRecord[]`

`scanCodexUsage` (`usage_codex.go:15`) re-walks the same date tree and runs
`scanCodexFileUsage` (`usage_codex.go:49`) under `MapLimit` (16). Per file:

1. Call `ScanCodexFile` for the session; nil → skip (a file without a
   `session_meta` + `cwd` contributes nothing).
2. `WalkJSONLFile` streams the file record by record (malformed lines are
   skipped, one record is capped at 32 MiB, open/read failures fail the
   module):
   - **`turn_context`** — the first `payload.model` wins (`model` pointer); return.
   - **`response_item`** — `ts = AsTimestamp(obj.timestamp)` (strings only; nil →
     skip). `message` + role `user` → if `codexIsUserPrompt(content)`, a
     `TallyPrompt` event; `message` + role `assistant` → a `TallyAssistant`
     event; `custom_tool_call` / `function_call` → a `TallyTool` event with
     `payload.name`. Return.
   - **`event_msg` + `payload.type == "token_count"`** — `ts = AsTimestamp`
     (nil → skip); `total = payload.info.total_token_usage` (nil → skip). Feed a
     `TallyAssistant` event, then build the record (below).
   - Any other type is ignored.
3. After the file, `records[last].MergeInterval(tally.Flush())` folds trailing
   events into the last record.

Each `token_count` event emits one `UsageRecord`:

- `SessionID` = `session.ID`, `AgentID` = `"codex"`, `Model` = the first
  `turn_context` model (nil if none), `Timestamp` = `ts`.
- `cur` = `{ input, output, cacheRead = cached_input_tokens,
  cacheWrite = cache_write_input_tokens }`, each via `AsTokenCount` (floor of a
  positive finite number, else 0).
- `delta` = `cur` for the first event; otherwise per-field
  `max(0, cur - prev)`. `prev` is then set to `cur` (per file).
- `InputTokens = max(0, delta.input - delta.cacheRead)` — cached input is
  subtracted because `input_tokens` includes it.
- `OutputTokens = delta.output`, `CacheReadTokens = delta.cacheRead`,
  `CacheWriteTokens = delta.cacheWrite`.
- `record.ApplyInterval(tally.TakeUntil(ts))` folds in the prompt/tool/active-time
  increment accrued up to this timestamp.

### Cumulative deltas (the core quirk)

`total_token_usage` is a session-running cumulative counter, not a per-turn
count. The SDK diffs adjacent `token_count` events to recover per-record usage.
When the counter falls back (compaction / reset), the negative delta is clamped
to 0 **per field**. The first event's delta is the full cumulative value.
`prev` resets per file, so deltas never cross sessions.

### Prompt detection (`codexIsUserPrompt`, `usage_codex.go:144`)

`content` must be an array (a plain string → false). Walk it to the **first**
`input_text` block with a string `text`, trim **leading** whitespace only, and
accept it iff it is non-empty and does **not** start with `<` or `# AGENTS.md`.
Only that first block is considered (the function returns immediately). This
excludes `<environment_context>` and AGENTS.md instruction injections from the
prompt count. (Contrast Claude's `claudeIsUserPrompt`, which accepts a string or
a `text` block and excludes `<` and `Caveat:`.)

### Active time & the tally (`SessionTally`, `tally.go:36`)

Events must arrive in ascending time order; out-of-order events are dropped.
`ActiveMs` is the sum of gaps between consecutive events, each gap capped at
`ActiveGapCapMs` (120 000 ms = 2 min), so idle time never inflates active time.
`TakeUntil(ts)` drains events with `ts <= ts`; `Flush()` drains the rest. Codex
feeds a `TallyAssistant` event for **both** assistant messages and `token_count`
events; assistant events increment no counter but do advance the gap clock.
Codex never passes a skill name, so `SkillCallsByName` stays nil — there is no
skill accounting (unlike Claude's `Skill` tool).

## Aggregation & cost

- `ScanAllUsage` (`usage.go:45`) concatenates records in module order (Codex is
  module #2, after Claude) and merges sessions into an ordered map keyed by bare
  `sessionId` (last-write-wins). Each session's `tokenUsage` is the 5-tuple
  `[input, output, cacheRead, cacheWrite, 3]`, where `3` is the status flag and
  input excludes cache traffic. `filePath` is tildified against `home`.
- Codex is token-billed: `EstimateTokenCost` (`pricing.go:189`) prices a record
  from model rates (`NormalizeModelId` → `LookupModelPrice`); `credits` is
  omitted (Codex is not in `creditPriceUSD`). The model is the first
  `turn_context`'s `payload.model`; `NormalizeModelId` strips provider/vendor
  prefixes and version/date suffixes (e.g. `openai/gpt-5.3-codex` →
  `gpt-5.3-codex`, which is listed). An unlisted or internal name (e.g.
  `codex-auto-review`) or a nil model → usd 0 and every token counted as
  unpriced. `input_tokens` is priced as the non-cached input; cache reads/writes
  are priced separately.

## Quirks & edge cases

- **Envelope-wrapped records.** Everything is `type` + `payload`, unlike
  Claude's flat shape. Only `response_item` produces transcript entries.
- **Cumulative token counters.** Usage diffs adjacent `token_count` events;
  negative deltas clamp to 0 per field; the first event takes the full total.
- **Cached input is subtracted.** `input_tokens` includes `cached_input_tokens`,
  so `InputTokens = max(0, delta.input - delta.cacheRead)`.
- **Workspace from `session_meta.cwd`, not the path.** The `YYYY/MM/DD` tree
  carries no project identity; there is no `DecodePathDirName` and no index.
- **No `session_meta` + `cwd` → no session.** Both the scan and the usage module
  skip the file entirely.
- **Title unwrapping is title-only.** `ToTitle` unwraps `## My request:` and
  `PLEASE IMPLEMENT THIS PLAN:`; the transcript does **not** unwrap the
  envelope (`cleanUserText` leaves it intact).
- **Two different AGENTS.md literals.** The title/transcript path treats `# ` as
  injected and drops the lowercase `# agents.md instructions` prefix; usage
  prompt detection excludes the uppercase `# AGENTS.md`.
- **Reasoning is summary-only.** The rollout keeps redacted raw reasoning; the
  transcript surfaces only the `summary[]` paragraphs of a `reasoning` item.
- **Tool output arrives separately.** `function_call_output` records follow
  their calls; `applyCodexToolOutput` pairs them by `call_id` (an unpaired or
  empty output is dropped).
- **No compaction marker entry** (no Codex counterpart to Claude's
  `上下文已压缩`).
- **Timestamp asymmetry.** The transcript (`mapTimestamp`) accepts numeric or
  string timestamps; usage (`AsTimestamp`) accepts strings only.
- **Scan and usage both stream.** The scan is byte-bounded (`ScanMaxBytes`,
  32 MiB per pass); usage streams line by line with a 32 MiB per-record cap.
- **Model is session-wide.** The first `turn_context` wins; records emitted
  before it (or with no `turn_context`) carry that model or nil.
- **`arguments` is a JSON string.** `argumentsMap` parses it; a failure yields
  nil input and the tool card title falls back to the tool name. Non-string
  argument values (e.g. an array `command`) likewise yield no title.
- **No skill accounting.** `SkillCallsByName` is always nil for Codex.

## Source map

| Topic | Location |
|---|---|
| Registry entry | `registry.go:23` |
| Usage module registration | `usage.go:27` |
| Session scan (date tree + two passes) | `codex.go` (`ScanCodex`, `ScanCodexFile`) |
| Transcript loader | `transcript_codex.go` (`parseCodexTranscript`, `argumentsMap`) |
| Content → entries, tool cards, cleaning | `transcript.go` (`entriesFromContent`, `toolCall`, `cleanUserText`, `mapTimestamp`, `scanJSONLines`) |
| Usage extraction | `usage_codex.go` (`scanCodexUsage`, `scanCodexFileUsage`, `codexIsUserPrompt`) |
| Activity tally | `tally.go` (`SessionTally`) |
| Aggregation & failure reporting | `usage.go` (`ScanAllUsageDetailed`, `ApplyInterval`, `MergeInterval`), `registry.go` (`AgentScanFailure`) |
| Pricing | `pricing.go` (`EstimateTokenCost`, `NormalizeModelId`, `LookupModelPrice`) |
| Shared helpers | `shared.go` (`ToTitle`, `ContentToText`, `AsEpochMs`, `AsTimestamp`, `AsTokenCount`, `ScanLinesE`, `WalkJSONLFile`, `ListDirsE`, `ListFilesE`) |
| Output contract | `contract.go` (`SessionInfo`, `UsageRecord`, `Entry`, `ToolCall`) |