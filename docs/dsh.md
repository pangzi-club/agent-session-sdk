# Deepseek Harness — session store format & parsing

How the `dsh` agent stores sessions on disk, and how this SDK turns that store
into `SessionInfo`, `Entry[]` and `UsageRecord[]`.

Shared helpers (`entriesFromContent`, `toolCall`, `cleanUserText`, `ToTitle`,
`SessionTally`, aggregation) are documented in full in [claude.md](claude.md);
this doc covers them only where DSH relies on a specific behavior.

## At a glance

| Property | Value |
|---|---|
| Agent id | `dsh` |
| Display name | Deepseek Harness |
| Store directory | `~/.dsh` |
| `canDeleteSessions` | `false` (the library only reports this; deletion is the consumer's job) |
| Session scanner | `ScanDsh` (`dsh.go`) |
| Transcript loader | `parseDshTranscript` (`transcript_dsh.go`) |
| Usage module | `scanDshUsage` (`usage_dsh.go`) |
| Billing | Token-metered against model rates; `cacheWriteTokens` is always 0; no credit accounting (`credits` omitted) |
| Store format | zstd-compressed JSONL event stream; one versioned file per session directory |

## On-disk layout

```
~/.dsh/
  sessions/
    --Users-alice-code-app--/           # one directory per project (encoded cwd)
      session-<uuid>/                   # one directory per session
        session.v3.jsonl.zstd           # versioned, zstd-compressed JSONL
        session.v4.jsonl.zstd           # newer version after in-place migration
      session-<uuid>/
        session.v4.jsonl.zstd
    --Users-alice-code-other--/
      ...
```

- **Two directory levels.** `ListDshSessionFiles` (`dsh.go:34`) lists project
  directories under `sessions/`, then session directories inside each, both
  via `ListDirs` (dot-prefixed directories are skipped).
- **Versioned file names, highest wins.** `pickDshSessionFile` (`dsh.go:49`)
  matches `^session\.v(\d+)\.jsonl\.zstd$` (`dshSessionFileRe`) among the
  non-recursive `.zstd` files of a session directory and keeps the highest
  numeric version. The regex is deliberately not pinned to a specific version
  — DSH bumps it as the format evolves. When several versions coexist
  (in-place migration), only the highest is read, so each session directory
  yields exactly one session.
- **Project directory names are encoded paths, but the SDK never decodes
  them.** Workspace comes from the `cwd` field of the session header inside
  the file; the encoded directory is only a grouping level.
- **The session id is the session directory name** (e.g. `session-<uuid>`),
  not the file name.

## Record format (zstd JSONL)

The file is read and decompressed in full before parsing — there is no head
cap and no streaming. All three paths (scan, transcript, usage) share the
package-level zstd decoder via `DecompressZstd` (`dsh.go:71`). After
decompression the store is JSONL: one JSON object per line, blank and
unparseable lines silently skipped. Every event carries a numeric millisecond
`time` field. The line types the SDK reads:

| `type` | Meaning | Fields read |
|---|---|---|
| `session` | Session header (first line) | `version` (number, ≥ 3), `cwd` (required, non-empty), `createdAt` |
| `agent/inbox/spliced` | Messages queued into the agent inbox | `data.inserted[]`, each `{ id, role, content, source.kind }` |
| `user/message` | A user-turn echo; also carries injected context | `data.id`, `data.role`, `data.content`, `data.source.kind` |
| `assistant/message` | An assistant turn | `data.message.content` blocks, `data.usage`, `data.message.source.model` |
| `tool/call` | A tool invocation | `data.callId`, `data.name`, `data.arguments` |
| `tool/result` | A tool result | `data.message` (`content`, `toolCallId` / `source.callId`, `isError`) |
| `session/title` | Session title, fallback then LLM | `data.title` (the last event wins) |

The usage extractor reads only `time` from `user/message`, `data.name` from
`tool/call`, and `data.usage` / `data.message.source.model` from
`assistant/message`; the transcript reads the rest.

### `data.usage` (on `assistant/message`)

| Key | Maps to |
|---|---|
| `inputTokens` | `InputTokens` (non-cached input only) |
| `outputTokens` | `OutputTokens` |
| `cacheReadTokens` | `CacheReadTokens` |
| — | `CacheWriteTokens` is hard-coded to `0`; no cache-write field is read |

### Illustrative lines

Synthetic, for shape only (shown decompressed):

```json
{"type":"session","version":4,"id":"session-7f3a…","cwd":"/Users/alice/code/app","createdAt":1789109916000,"time":1789109916000}
{"type":"agent/inbox/spliced","time":1789109916500,"data":{"inserted":[{"id":"u1","role":"user","content":[{"type":"text","text":"Read the session format"}]}]}}
{"type":"user/message","time":1789109916600,"data":{"id":"u1","role":"user","content":[{"type":"text","text":"Read the session format"}]}}
{"type":"session/title","time":1789109916650,"data":{"title":"Session format"}}
{"type":"assistant/message","time":1789109918000,"data":{"message":{"role":"assistant","content":[{"type":"reasoning","text":"Check the docs."},{"type":"text","text":"Done."},{"type":"tool-call","id":"c1","name":"read","arguments":"{\"path\":\"README.md\"}"}],"source":{"model":"deepseek-chat"}},"usage":{"inputTokens":1200,"outputTokens":48,"cacheReadTokens":8000}}}
{"type":"tool/call","time":1789109918100,"data":{"callId":"c1","name":"read","arguments":"{\"path\":\"README.md\"}"}}
{"type":"tool/result","time":1789109918300,"data":{"message":{"toolCallId":"c1","isError":false,"content":[{"type":"text","text":"# README"}]}}}
```

## Session scanning → `SessionInfo`

`ScanDsh` (`dsh.go:76`) runs `ScanDshFile` over `ListDshSessionFiles` with
`MapLimit` at `FileConcurrency` (16). Errors are **not** isolated per file:
`stat`, read and decompression failures propagate, fail the whole batch, and
`ScanSessions` reports DSH with zero sessions. A missing store directory is
simply empty (`ListDirs`/`ListFiles` return nothing on error).

`ScanDshFile` (`dsh.go:92`):

1. `os.Stat`, `os.ReadFile`, `DecompressZstd`, split on `\n` — the whole
   session, every time.
2. **Metadata pass** — `ParseLines` finds the first `type == "session"` line
   whose numeric `version` is ≥ `dshMinSessionVersion` (3) and whose `cwd` is a
   non-empty string; `createdAt = AsEpochMs(createdAt)` (a finite number passes
   through as ms; an RFC3339 string is parsed). No such line → the file is
   skipped (`nil, nil`, not an error).
3. **User-input pass** — `dshUserInput` (`dsh.go:180`) scans *all* lines for
   `agent/inbox/spliced` events carrying an `inserted` item with
   `role == "user"`. The first such item sets `sawUserInput`; the title is
   `ToTitle(ContentToText(item.content))` (`ContentToText` joins block `text`
   fields with a space). When `ToTitle` rejects the text (injected content
   such as `<turn_aborted>`), the scan continues to later spliced lines until
   a usable title appears.
4. **Title pass** — `dshSessionTitle` (`dsh.go:163`) scans *all* lines for
   `session/title` events and keeps the last non-empty `data.title`
   (`FirstLineTitle` caps it at 120 UTF-16 units). DSH first writes a
   fallback title (the truncated first prompt) and later replaces it with the
   LLM-generated one, so last-wins yields the real title. When it exists it
   overrides the user-input fallback.
5. Field derivation:

| `SessionInfo` field | Source |
|---|---|
| `id` | session directory name (`basename(dirname(file))`) |
| `workspace` | header `cwd` (never the decoded project directory name) |
| `title` | last `session/title` title, else first usable user-input title, else null |
| `blank` | `true` when no user input appeared anywhere; omitted otherwise |
| `createdAt` | header `createdAt` (`AsEpochMs`) ?? file mtime |
| `updatedAt` | mtime of the `.zstd` file in ms (`FloorMtimeMs`) |
| `sizeBytes` | **compressed** file size |
| `filePath` | the `.zstd` file |

**Two distinct version gates.** The file-name version selects which file to
read (highest wins); the header `version` field decides whether the session
counts at all (≥ 3). A `session.v9.jsonl.zstd` whose header says `version: 2`
is picked and then skipped.

**Blank vs titleless.** `blank` cannot be inferred from `title == nil`: a
session whose only user inputs are injected text is *not* blank (it was
messaged) but can still have a null title.

## Transcript parsing → `Entry[]`

`parseDshTranscript` (`transcript_dsh.go:16`) locates the file via
`SessionInfo.FilePath` (a `#` suffix marks a SQLite store and is rejected),
reads it whole, decompresses with the shared zstd decoder, and scans lines
with a `bufio.Scanner` (64 KiB initial buffer, per-line cap
`ScanMaxTranscriptLine`, 32 MiB). Per line (`ts = mapTimestamp(obj, false)` —
DSH lines carry a top-level millisecond `time`):

- **`agent/inbox/spliced`** — every `data.inserted` item whose role
  `roleFromString` recognizes (`user`/`human`/`input`,
  `assistant`/`agent`/`model`/`gemini`/`output`, `system`/`developer`) emits
  `entriesFromContent(role, item.content, ts)`. User inserts are skipped when
  the source kind is injected context (below) or when the message id was
  already rendered.
- **`user/message`** — the echo of a spliced user turn, plus injected context
  that never passes through the inbox. Skipped when `data.source.kind` is one
  of `agent-instructions`, `skill-catalog`, `skill-invocation`,
  `runtime-context`, `plugin` (harness context, not conversation). Real input
  (`user`), background-job and approval notices (`tool-jobs`,
  `user-approval`) and model-change notices stay. Rendered once per id:
  splices and echoes share the message id and later duplicates are dropped.
- **`assistant/message`** — `entriesFromContent(RoleAssistant,
  data.message.content, ts)`: text blocks, reasoning blocks (bounded
  `Thinking`) and `tool-call` blocks (one card each, `arguments` parsed from
  its JSON string, `id` kept for result pairing).
- **`tool/call`** — records `data.callId` → `time` for duration and emits a
  card only when no assistant content block carried that call. Both real
  versions embed the block, so the emission is a guard for stores where the
  event is the sole source.
- **`tool/result`** — pairs onto the emitted card by call id
  (`applyDshToolResults`): `Output` (joined text blocks, clipped), `Failed`
  (`isError`) and `DurationMs` (`tool/call` time → result time, falling back
  to the assistant message time). v4 keeps the id and flag on `data.message`;
  v3 wraps each result in a `tool-result` content block with its own
  `toolCallId` / `isError`.
- **Everything else** falls through to `genericEntries`
  (`transcript.go:496`), which never matches DSH's `data`-nested shapes — so
  `session`, `step/*`, `turn/*`, `system/message`, `todo/write` and the
  header/config events add nothing. The system prompt is deliberately not
  rendered.

### Deduplication and injected context

User text exists twice on disk (inbox splice plus `user/message` echo, same
message id) and tool calls twice (assistant content block plus `tool/call`
event, same call id); both are deduplicated by id, first occurrence wins.
`user/message` also carries harness-injected context that never passed
through the inbox — workspace instructions, skill catalogs, runtime
snapshots and plugin context. Those source kinds are skipped;
`<system-reminder>`-shaped text is additionally dropped by `cleanUserText`.
Background-job and approval notices (`tool-jobs`, `user-approval`) are kept:
they were visible through the splice path before this parsing existed.

### From content to entries

Content uses the shared rules (see [claude.md](claude.md)): a string becomes
one cleaned text entry; consecutive text blocks join with `\n`; each tool-like
block (`tool_use`, `tool_call`, `toolCall`, `tool-call`, `function_call`)
flushes pending text and emits an assistant-role entry with one `ToolCall`;
`thinking` / `reasoning` blocks become bounded `Thinking` entries (empty ones
are skipped — DSH writes an empty reasoning block on tool-only steps);
`tool_result` and `image` blocks are dropped. Tool cards classify by
lowercased name (file/command/search/other) with the shared title/meta rules;
user text goes through `cleanUserText`, assistant text is only trimmed.

## Usage extraction → `UsageRecord[]`

`scanDshUsage` (`usage_dsh.go:13`) reuses `ListDshSessionFiles` — the
version-matching rule lives in one place only — and runs `scanDshFileUsage`
at concurrency 16. Per file (`usage_dsh.go:39`):

1. **Session gate.** `ScanDshFile` must return a session (valid header,
   version ≥ 3, non-empty `cwd`). Blank sessions still produce a session
   entry, just no records.
2. `os.ReadFile` + `DecompressZstd` again — the usage scan decompresses the
   file a second time (once inside `ScanDshFile`, once here).
3. `WalkLines` over every parseable line:
   - **Timestamp gate.** `time = AsNumber(obj["time"])` — strictly the
     top-level numeric millisecond field. No ISO-string fallback, no
     recursion into `data`. `time == 0` → the line is skipped entirely.
   - **`user/message`** → prompt event, then stop. No content filtering.
   - **`tool/call`** → tool event with `data.name`; a missing name still
     increments `toolCalls` but adds nothing to `toolCallsByName`. Then stop.
   - **`assistant/message`** → assistant event; emit a record only when
     `data.usage` exists. `model` = `data.message.source.model` (null when
     absent); tokens map `inputTokens` / `outputTokens` / `cacheReadTokens`
     through `AsTokenCount` (floors positive finite numbers, else `0`);
     `cacheWriteTokens` is hard-coded to `0`.
   - All other line types (including `session` and `agent/inbox/spliced`)
     feed nothing.
4. **Attribute activity** — `record.ApplyInterval(tally.TakeUntil(time))`;
   after the file, `lastRecord.MergeInterval(tally.Flush())` folds trailing
   events into the last record.

An `assistant/message` without `data.usage` still contributes tally events;
its counts land on the next usage-bearing record (or the last one via
`Flush`). No skill accounting: `skillCallsByName` is always null.

Note the split feed: prompts are counted from `user/message` events, while
the transcript deduplicates splices and `user/message` echoes by message id.

### Active time & the tally

The shared `SessionTally` (`tally.go:36`) applies: events must arrive in
ascending time order (out-of-order events are dropped), each gap between
consecutive events counts at most `ActiveGapCapMs` (120 000 ms), and
`TakeUntil` / `Flush` drain prompt/tool/active-time increments onto the record
(`userPrompts`, `toolCalls`, `toolCallsByName`, `activeMs`).

## Aggregation & cost

- `ScanAllUsage` (`usage.go:45`) concatenates records in module order (DSH is
  fifth) and merges sessions into an ordered map keyed by bare `sessionId`
  (last-write-wins). Each session's `tokenUsage` is the 5-tuple
  `[input, output, cacheRead, cacheWrite, 3]`, where `3` is the status flag and
  input excludes cache traffic; DSH's cache-write slot is always 0. `filePath`
  (the `.zstd` path) is tildified against `home`.
- DSH is token-billed: `EstimateTokenCost` prices a record from model rates
  (`NormalizeModelId` → `LookupModelPrice`). `credits` is omitted. A null or
  unresolvable `model` prices to 0 with every token counted as unpriced.

## Quirks & edge cases

- **Two version gates.** File name: highest `session.v<N>` wins, regex
  unpinned. Header: `version >= 3` or the session is skipped. The two numbers
  are independent.
- **One session per directory.** Older coexisting versions are never read,
  even if the newest file is corrupt (the corruption then fails the scan).
- **Full decompression everywhere.** Scan, transcript and usage each read and
  decompress the whole file; the usage path does it twice per file.
  `sizeBytes` and `updatedAt` describe the compressed file.
- **`blank` is about user input, not title.** Injected-only input
  (`<turn_aborted>` and friends) marks the session non-blank but can leave the
  title null; the title search keeps scanning later splices.
- **The id is the directory name**, so renaming the session directory changes
  the session id.
- **User text and tool calls are stored twice.** Splice + `user/message`
  echo share a message id; assistant content block + `tool/call` share a call
  id. The transcript keeps the first occurrence and drops the duplicate.
- **Injected context is filtered by source kind, not text.** `user/message`
  and spliced inserts whose `source.kind` is `agent-instructions`,
  `skill-catalog`, `skill-invocation`, `runtime-context` or `plugin` are
  harness context and never render. A real user message always arrives with
  `source.kind == "user"`.
- **Tool durations span call → result.** A tool interrupted before its
  `tool/result` keeps `DurationMs` 0 and an empty `Output`.
- **Empty reasoning blocks are dropped.** DSH writes `{"type":"reasoning",
  "text":""}` on tool-only steps; `entriesFromContent` skips empty thinking
  and text.
- **Usage time is numeric-only.** A line without a top-level numeric `time`
  is dropped, even if it carries an ISO timestamp elsewhere.
- **`cacheWriteTokens` is always 0** — hard-coded, not read from the store.
- **One bad file zeroes the agent.** `stat`/read/decompression errors
  propagate through `MapLimit`; `ScanSessions` then reports an empty list.
- **Workspace from the header only.** The encoded project directory name is
  never decoded (`DecodePathDirName` is not called for DSH).

## Source map

| Topic | Location |
|---|---|
| Registry entry | `registry.go:30` |
| File discovery & version pick | `dsh.go` (`ListDshSessionFiles`, `pickDshSessionFile`, `dshSessionFileRe`, `dshMinSessionVersion`) |
| Session scan | `dsh.go` (`ScanDsh`, `ScanDshFile`, `dshUserInput`, `dshSessionTitle`, `DecompressZstd`) |
| Transcript loader | `transcript_dsh.go` (`parseDshTranscript`, `firstUserID`, `dshInjectedUser`, `applyDshToolResults`) |
| Generic fallback, content → entries, tool cards | `transcript.go` (`genericEntries`, `entriesFromContent`, `toolCall`, `cleanUserText`, `roleFromString`, `mapTimestamp`) |
| Usage extraction | `usage_dsh.go` (`scanDshUsage`, `scanDshFileUsage`) |
| Activity tally | `tally.go` (`SessionTally`) |
| Aggregation | `usage.go` (`ScanAllUsage`, `ApplyInterval`, `MergeInterval`) |
| Shared helpers | `shared.go` (`ListDirs`, `ListFiles`, `MapLimit`, `ParseLines`, `WalkLines`, `ContentToText`, `ToTitle`, `AsEpochMs`, `AsNumber`, `AsTokenCount`, `FloorMtimeMs`) |
| Output contract | `contract.go` (`SessionInfo`, `UsageRecord`, `Entry`, `ToolCall`) |