# Hermes — session store format & parsing

How the `hermes` agent stores sessions on disk, and how this SDK turns that
store into `SessionInfo`, `Entry[]` and `UsageRecord[]`.

Hermes keeps everything in one SQLite database (`~/.hermes/state.db`). Unlike
OpenCode's fixed `message`/`part` schema, the Hermes **transcript** table is
discovered at runtime — the loader probes a list of candidate table names and
picks columns with `PRAGMA table_info`, because the schema varies between
installs. The scanner and usage extractor, by contrast, read fixed tables
(`sessions`, `messages`, `session_model_usage`).

The shared SQLite mechanics — `OpenSQLite` / `QueryMaps` (`shared.go:534`,
`shared.go:548`), the `#`-suffixed `FilePath` marker and `splitDBRef`, and the
`transcriptSignature` that watches the db file plus its `-wal` sidecar — are the
same as OpenCode's and are documented in [opencode.md](opencode.md); they are only
summarized here. Shared content helpers (`entriesFromContent`, `toolCall`,
`cleanUserText`, `SessionTally`, `ScanAllUsage`, `ToTitle`) are documented in
[claude.md](claude.md).

## At a glance

| Property | Value |
|---|---|
| Agent id | `hermes` |
| Display name | Hermes |
| Store directory | `~/.hermes` |
| `canDeleteSessions` | `false` (`registry.go:35` sets no `canDelete`; the library only reports it) |
| Session scanner | `ScanHermes` (`hermes.go:15`) |
| Transcript loader | `parseHermesTranscript` (`transcript_db.go:112`) — Hermes-only, not the OpenCode/ZCode loader |
| Usage module | `scanHermesUsage` (`usage_hermes.go:31`) |
| Billing | Token-metered against model rates; no credit accounting (`credits` omitted) |
| Store format | SQLite `state.db` (`sessions`, `messages`, `session_model_usage`, plus a discovered transcript table) |

## On-disk layout

```
~/.hermes/
  state.db                     # SQLite store: sessions, messages, session_model_usage, transcript table
  state.db-wal                 # write-ahead log sidecar (watched by the transcript cache)
```

- **One database, several tables.** `ScanHermes` reads `sessions`;
  `scanHermesUsage` reads `messages` and `session_model_usage`; the transcript
  loader discovers its table among `messages`, `message`, `conversation_messages`,
  `chat_messages`, `events` (first match wins). There is no per-session file and
  no legacy JSON store.
- **`FilePath` carries a `#` marker.** A session's `FilePath` is
  `<dbPath>#<sessionId>` (e.g. `~/.hermes/state.db#<id>`). The `#` tells the shared
  loader to split a SQLite reference (`splitDBRef`, `transcript_db.go:14`) rather
  than open a JSONL file. See [opencode.md](opencode.md) for the marker mechanics.
- **Most sessions have no filesystem workspace.** Hermes sessions originate from
  feishu / cron / desktop sources, so `cwd` is often absent; the scanner groups
  those under a `Hermes | <source>` pseudo-path (see below).
- The `-wal` sidecar is watched by the cache but may not exist; `fileSignature`
  returns `"-"` for a missing file (`transcript.go:124`). The `-shm` sidecar is not
  watched.

## Record format (SQLite)

`QueryMaps` returns each row as a column→value map, normalizing `INTEGER`→
`float64`, `TEXT`/`BLOB`→`string`, `NULL`→`nil` (see [opencode.md](opencode.md)).
All Hermes timestamp columns are **epoch seconds**, converted with `SecToMs`
(`shared.go:160`) in the scanner and usage paths.

### `sessions` table (scanner)

`SELECT * FROM sessions`. The columns the SDK reads:

| Column | Used for |
|---|---|
| `id` | session id — `AsString`; must be a non-empty string, else the row is skipped |
| `hidden` | flag: `1` or `true` ⇒ row skipped |
| `archived` | flag: `1` or `true` ⇒ row skipped |
| `cwd` | workspace (preferred; absolute project path) |
| `source` | workspace fallback ⇒ `Hermes | <source>` |
| `title` | session title (normalized by `ToTitle`) |
| `started_at` | epoch seconds → `createdAt` (`SecToMs`) |
| `last_activity_at` | epoch seconds → `updatedAt` (`SecToMs`) |

### `messages` table (usage)

`SELECT session_id, role, tool_name, timestamp, compacted FROM messages`:

| Column | Meaning |
|---|---|
| `session_id` | must be a string, and must belong to a scanned session, else the row is dropped |
| `role` | `user` ⇒ prompt (only when `tool_name` is NULL); `tool` ⇒ tool call |
| `tool_name` | NULL for a prompt; the tool name for a `tool` row (may be NULL) |
| `timestamp` | epoch seconds → event time (`SecToMs`); a missing/zero value drops the row |
| `compacted` | `1` ⇒ compaction replacement artifact, skipped entirely |

### `session_model_usage` table (usage record points)

`SELECT session_id, model, last_seen, input_tokens, output_tokens,
cache_read_tokens, cache_write_tokens FROM session_model_usage`. Each row is one
aggregated record point per **(session × model × task)**:

| Column | Maps to |
|---|---|
| `session_id` | must be a string and a scanned session, else dropped |
| `model` | `Model` (`AsString`) |
| `last_seen` | epoch seconds → `Timestamp` (`SecToMs`); missing/zero drops the row |
| `input_tokens` | `InputTokens` (non-cached input, read directly) |
| `output_tokens` | `OutputTokens` |
| `cache_read_tokens` | `CacheReadTokens` |
| `cache_write_tokens` | `CacheWriteTokens` |

There is no reasoning-token column and no credits column.

### Transcript table (discovered at runtime)

`parseHermesTable` (`transcript_db.go:149`) runs `PRAGMA table_info("<table>")`
and picks the first present column from each option list:

| Role | Candidate columns (in order) | Required |
|---|---|---|
| session key | `session_id`, `sessionId`, `conversation_id`, `thread_id` | yes |
| role | `role`, `sender`, `author` | yes |
| content | `content`, `text`, `message`, `body`, `data` | yes |
| timestamp | `timestamp`, `created_at`, `createdAt`, `time` | no (defaults to `0`) |
| tool calls | `tool_calls` (fixed name, not discovered) | no |

If any of the session/role/content columns is missing, the table is rejected
(`ErrUnsupported`) and the loader tries the next candidate.

### Illustrative rows (synthetic)

`sessions` row:

```json
{"id":"sess_abc","hidden":0,"archived":0,"cwd":"/Users/alice/code/app","source":"desktop","title":"Fix the bug","started_at":1762939000,"last_activity_at":1762940500}
```

`messages` rows (a prompt and a tool call):

```json
{"session_id":"sess_abc","role":"user","tool_name":null,"timestamp":1762940000,"compacted":0}
{"session_id":"sess_abc","role":"tool","tool_name":"read","timestamp":1762940002,"compacted":0}
```

`session_model_usage` row (`last_seen` is epoch seconds → `1762940002000` ms):

```json
{"session_id":"sess_abc","model":"claude-sonnet-4","last_seen":1762940002,"input_tokens":1200,"output_tokens":48,"cache_read_tokens":8000,"cache_write_tokens":0}
```

Transcript `content` (plain text or a JSON block array) and the `tool_calls`
column (a JSON **string** of OpenAI-style function calls):

```json
{"role":"assistant","content":"Reading the file.","timestamp":1762940002,"tool_calls":"[{\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"filePath\\\":\\\"/Users/alice/code/app/main.go\\\"}\"}}]"}
```

## Session scanning → `SessionInfo`

`ScanHermes` (`hermes.go:15`) opens `state.db` read-only and runs
`SELECT * FROM sessions`. **An unopenable db or a failed query returns an error**
(`nil, err`) — the caller (`ScanSessions`) treats that as "no Hermes sessions".
This is the opposite of the usage path, which degrades to empty (see Quirks).

Per row:

1. **Skip hidden/archived.** `isHermesFlagOn(hidden) || isHermesFlagOn(archived)`
   ⇒ skip. `isHermesFlagOn` (`hermes.go:63`) is true only for boolean `true` or the
   number `1` (`AsBoolTrue(v) || AsNumber(v) == 1`). Because `QueryMaps` normalizes
   `INTEGER`→`float64`, a stored `1` matches; `0`, `2`, a text `"1"`, or NULL do
   not.
2. **Require an id.** `id = AsString(row["id"])`; skip if nil or empty. `AsString`
   is strings-only, so a numeric id is dropped.
3. `createdAt = FirstOr(0, SecToMs(started_at))` — `SecToMs` rounds a positive
   finite number × 1000; anything else (string, zero, negative) yields nil → `0`.
4. `workspace = hermesWorkspace(row)` (below).
5. `title = ToTitle(AsString(title))` (`shared.go:211`; drops injected `<` / `# `
   first lines, truncates to 120 UTF-16 units).
6. `updatedAt = FirstOr(createdAt, SecToMs(last_activity_at))` — falls back to
   `createdAt` when `last_activity_at` is absent.
7. `sizeBytes = 0` (always — Hermes never computes a size).
8. `filePath = <dbPath>#<id>`.

### Workspace grouping (`hermesWorkspace`, `hermes.go:51`)

- `cwd` (a non-empty string) wins — the real project path.
- else `source` (a non-empty string) ⇒ the pseudo-path `Hermes | <source>`
  (e.g. `Hermes | feishu`, `Hermes | cron`).
- else the literal `Hermes`.

The result is **never empty**, so — unlike OpenCode, which skips a session with no
`directory` — Hermes never drops a session for a missing workspace. Sessions
without a filesystem cwd are grouped under these pseudo-paths.

## Transcript parsing → `Entry[]`

`parseHermesTranscript` (`transcript_db.go:112`) is Hermes-only (it is *not*
`parseSessionDBTranscript`, the OpenCode/ZCode loader).

1. **Split the reference.** `splitDBRef` (`transcript_db.go:14`) splits `FilePath`
   on the first `#`; both halves must be non-empty or the loader errors with
   `session %q has no database reference`.
2. **Open read-only** via `OpenSQLite`; a missing/unopenable db is an error here.
3. **Discover the table.** `SELECT name FROM sqlite_master WHERE type='table'`
   builds the set of table names. The loader walks the candidates
   `messages`, `message`, `conversation_messages`, `chat_messages`, `events` in
   that order. A candidate is skipped if it is absent or fails the identifier
   guard `sqlIdentifierRe` (`^[A-Za-z_][A-Za-z0-9_]*$`). For each surviving
   candidate it calls `parseHermesTable`; **on error it continues** to the next,
   and **the first candidate that yields ≥ 1 entry wins**. A table that exists but
   is empty for this session (0 entries) also falls through. If none produces
   entries, the loader returns `ErrUnsupported`.

### Per-table parsing (`parseHermesTable`, `transcript_db.go:149`)

After picking columns (see the record-format table), it runs
`SELECT * FROM "<table>" WHERE "<sessionCol>" = ? ORDER BY rowid`. **Ordering is
`rowid` (insertion order), not timestamp.** Per row:

- `role = roleFromString(firstString(row, roleCol))` (`transcript.go:406`):
  `user`/`human`/`input` → user, `assistant`/`agent`/`model`/`gemini`/`output` →
  assistant, `system`/`developer` → system, **anything else → empty**. An empty
  role skips the row — so a `tool`-role row (a tool result) is **dropped from the
  transcript**, even though usage counts it as a tool call.
- `ts = 0`, or `epochSecondsOrMs(row[tsCol])` when a timestamp column was found.
- `content = row[contentCol]`. If it is a string starting with `{` or `[`, the
  loader tries `json.Unmarshal`; on success the parsed object/array replaces the
  string (so JSON-encoded content is decoded, plain text is left as-is).
- `entriesFromContent(role, content, ts)` (`transcript.go:238`) emits the text/tool
  entries (cleaning, block joining and tool cards are the shared helpers — see
  [claude.md](claude.md)). A `system` role is kept and its text emitted.
- **Assistant rows also read `tool_calls`:** `hermesToolEntries(row["tool_calls"], ts)`
   appends one tool entry per call, after the content entries, sharing `ts`.

`epochSecondsOrMs` (`transcript_db.go:236`) is more permissive than the scanner's
`SecToMs`: a number `0 < f < 1e11` is treated as seconds (`× 1000`), a number
`≥ 1e11` (or `≤ 0`) passes through as-is (milliseconds), and a non-number falls
back to `AsEpochMs` (an RFC3339 string → ms), else `0`.

### Tool cards from `tool_calls` (`hermesToolEntries`, `transcript_db.go:206`)

The `tool_calls` value must be a **JSON string**: the type switch handles `string`
(unmarshal into an array) and `nil`; **any other type returns nil** (an already-
decoded array/map is not accepted — `QueryMaps` gives `TEXT` as a string, so this
is the normal case). Each element is an OpenAI-style function call:

```
[ { "function": { "name": "<tool>", "arguments": <object | JSON string> } }, ... ]
```

- `call = GetMap(item)`, `fn = GetMap(call["function"])`, `name = firstString(fn, "name")`;
  skip when the name is empty. There is no `type == "function"` check.
- The card is `toolCall(name, argumentsMap(fn["arguments"]))` (`transcript.go:344`,
  `argumentsMap` at `transcript_codex.go:49` decodes an object or its JSON-string
  form). The entry is **always `RoleAssistant`** with exactly one `ToolCall`.

Note that tool entries can therefore come from **two** places on an assistant row:
`tool_use` blocks inside `content` (via `entriesFromContent`) *and* the separate
`tool_calls` column.

### Cache invalidation

As for any `#` path, `transcriptSignature` (`transcript.go:111`) signs
`fileSignature(dbPath) + "+" + fileSignature(dbPath + "-wal")`, so appends still in
the WAL invalidate the cached transcript. See [opencode.md](opencode.md).

## Usage extraction → `UsageRecord[]`

`scanHermesUsage` (`usage_hermes.go:31`) opens `state.db`; **an unopenable db yields
an empty result, not an error** (Hermes not installed ⇒ no data, no module
failure). It then calls `ScanHermes` for the session list (a `ScanHermes` error
*does* propagate) and keeps only events whose `session_id` is in that filtered
list (`sessionIDs`). Events are collected into an insertion-ordered map of
`hermesEntry` per session.

### Events

Two queries feed three event kinds (`hermesPrompt`, `hermesTool`, `hermesRecord`):

- **Messages** — `SELECT session_id, role, tool_name, timestamp, compacted FROM messages`.
  Per row: skip when `compacted == 1` (`AsNumber`); require `session_id` to be a
  string; `ts = SecToMs(timestamp)`, skip when nil. Then:
  - `role == "user"` **and** `tool_name == nil` ⇒ a **prompt** event. (A `user` row
    that carries a `tool_name` is neither a prompt nor a tool — it is dropped.)
  - `role == "tool"` ⇒ a **tool** event with `tool = AsString(tool_name)` (may be
    nil). There is no text inspection and **no skill name** — Hermes never
    populates `SkillCallsByName`.
- **Record points** — `SELECT session_id, model, last_seen, input_tokens,
  output_tokens, cache_read_tokens, cache_write_tokens FROM session_model_usage`.
  Require a string `session_id`; `ts = SecToMs(last_seen)`, skip when nil; add a
  **record** event carrying the row.

Both queries use the **fixed** table names `messages` and `session_model_usage`
(no runtime discovery, unlike the transcript). A failed query returns an error,
which `ScanAllUsage` treats as an empty module.

### Ordering and the tally

Per session, events are stably sorted by `ts`, ties broken by `hermesOrderOf`
(`usage_hermes.go:147`: record = 1, prompt/tool = 0) so **activity at the same
instant settles before the record point**. Then a `SessionTally` (`tally.go:36`)
is walked:

- **record**: `AddEvent(ts, TallyAssistant, nil)` first, then emit a `UsageRecord`
  unconditionally — **there is no tokens nil-check** (unlike OpenCode), so every
  `session_model_usage` row produces a record, even one whose token columns are all
  zero/missing:

  | Record field | Source |
  |---|---|
  | `SessionID` | the session id |
  | `AgentID` | `"hermes"` |
  | `Model` | `AsString(model)` |
  | `Timestamp` | `ts` (`last_seen`, ms) |
  | `InputTokens` | `AsTokenCount(input_tokens)` |
  | `OutputTokens` | `AsTokenCount(output_tokens)` |
  | `CacheReadTokens` | `AsTokenCount(cache_read_tokens)` |
  | `CacheWriteTokens` | `AsTokenCount(cache_write_tokens)` |

  Input is the non-cached input already (read directly, no subtraction).
  `AsTokenCount` floors positive finite numbers, else `0`. Then
  `record.ApplyInterval(tally.TakeUntil(ts))`.
- **prompt**: `AddEvent(ts, TallyPrompt, nil)`.
- **tool**: `AddEvent(ts, TallyTool, entry.tool)`.

After the walk, the last record absorbs the tail via `MergeInterval(tally.Flush())`.
**A session with events but no record emits nothing** (`sessionRecords` is appended
only when non-empty).

`SessionTally` drops out-of-order events and caps each inter-event gap at
`ActiveGapCapMs` (120 000 ms = 2 min) so idle time never inflates `ActiveMs`; the
`TallyAssistant` event contributes only active time, not a prompt/tool count. See
[claude.md](claude.md) for the full model.

## Aggregation & cost

- `ScanAllUsage` (`usage.go:45`) concatenates records in module order (Hermes is
  last, `usage.go:36`) and merges sessions into an ordered map keyed by bare
  `sessionId` (last-write-wins). Each session's `tokenUsage` is
  `[input, output, cacheRead, cacheWrite, 3]` — input excludes cache traffic, `3`
  is the status flag. `filePath` is tildified against `home`
  (`~/.hermes/state.db#<id>`). Sessions with no records omit `tokenUsage`.
- Hermes is token-billed: it is **not** in `creditPriceUSD` (only `qoder` /
  `qoder-cn` are), so `EstimateTokenCost` (`pricing.go:189`) prices a record from
  model rates (`NormalizeModelId` → `LookupModelPrice`) and `credits` is omitted.
  An unresolvable model prices the record at `0` and counts all its tokens as
  unpriced.

## Quirks & edge cases

- **`canDeleteSessions` is `false`.** `registry.go:35` sets no `canDelete`; the
  library only reads, deletion is the consumer's job.
- **Scanner errors, usage degrades.** `ScanHermes` returns an error on an
  unopenable db / failed query (caller reports no sessions); `scanHermesUsage`
  returns an empty result on an unopenable db (no module failure). The usage path
  still propagates a `ScanHermes` error and a failed `messages` /
  `session_model_usage` query.
- **Workspace is never empty.** `cwd` → `Hermes | <source>` → `Hermes`. Sessions
  are never skipped for a missing workspace (unlike OpenCode); most Hermes
  sessions have no filesystem cwd and land under a pseudo-path.
- **Hidden/archived filter is exact.** Only `1` or boolean `true` hides a row;
  `0`, other numbers, a text `"1"`, or NULL do not.
- **Timestamps are epoch seconds, numeric-only in scan/usage.** `SecToMs` accepts
  a positive finite number only — a string timestamp yields `0`/nil and the row is
  dropped (usage) or falls back (scanner). The transcript's `epochSecondsOrMs` is
  more permissive (seconds, milliseconds, or RFC3339). This is an asymmetry to
  remember.
- **Transcript discovers its schema; usage does not.** The transcript probes five
  candidate tables and picks columns at runtime, but usage hardcodes `messages`
  and `session_model_usage`. An install that stores messages under a different
  table name can still load a transcript yet produce **no usage** (the fixed query
  fails ⇒ empty module).
- **Transcript orders by `rowid`, usage by `ts`.** The transcript preserves
  insertion order; usage sorts events by time (activity before record on ties).
- **Compaction is filtered only in usage.** `compacted == 1` rows are skipped by
  the usage extractor but **not** by the transcript — compaction replacement
  artifacts still appear as transcript entries.
- **`tool`-role rows are dropped from the transcript.** `roleFromString("tool")`
  is empty, so tool-result rows produce no entry; transcript tool cards come from
  assistant `tool_calls` (or `content` `tool_use` blocks), while the usage tool
  count comes from `messages.role == "tool"` rows — different sources.
- **`tool_calls` must be a JSON string.** An already-decoded array/map is ignored
  (`hermesToolEntries` handles only `string`/`nil`). The shape is OpenAI function
  calling (`function.name` / `function.arguments`), with no `type` check.
- **Every usage row emits a record.** No tokens nil-check, so an all-zero
  `session_model_usage` row still produces a `UsageRecord`.
- **Prompt requires `tool_name IS NULL`.** A `user` row with a `tool_name` is
  dropped; a `tool` row without a name still counts toward `ToolCalls` but not
  `ToolCallsByName`.
- **`sizeBytes` is always `0`** and **`SkillCallsByName` is always nil** for Hermes.
- **First non-empty candidate table wins.** A table that is empty for the session
  or lacks the required columns falls through to the next; if none yields entries
  the loader returns `ErrUnsupported`.

## Source map

| Topic | Location |
|---|---|
| Registry entry | `registry.go:35` |
| Usage module registration | `usage.go:36` |
| Session scan | `hermes.go` (`ScanHermes`, `hermesWorkspace`, `isHermesFlagOn`) |
| Transcript loader (Hermes-only) | `transcript_db.go:112` (`parseHermesTranscript`, `parseHermesTable`, `hermesToolEntries`, `epochSecondsOrMs`) |
| DB ref split / JSON map | `transcript_db.go:14` (`splitDBRef`), `transcript_db.go:25` (`parseJSONMap`) |
| Content → entries, tool cards, cleaning, roles | `transcript.go` (`entriesFromContent`, `toolCall`, `cleanUserText`, `roleFromString`) |
| Tool arguments decode | `transcript_codex.go:49` (`argumentsMap`) |
| Cache signature (db + `-wal`) | `transcript.go:111` (`transcriptSignature`, `fileSignature`) |
| Usage extraction | `usage_hermes.go` (`scanHermesUsage`, `hermesOrderOf`) |
| Activity tally | `tally.go:36` (`SessionTally`) |
| Aggregation | `usage.go:45` (`ScanAllUsage`, `ApplyInterval`, `MergeInterval`) |
| Pricing | `pricing.go:189` (`EstimateTokenCost`, `NormalizeModelId`, `LookupModelPrice`, `creditPriceUSD`) |
| SQLite access | `shared.go:534` (`OpenSQLite`), `shared.go:548` (`QueryMaps`) |
| Shared helpers | `shared.go` (`ToTitle`, `SecToMs`, `AsEpochMs`, `AsNumber`, `AsString`, `AsBoolTrue`, `AsTokenCount`, `FirstOr`) |
| Output contract | `contract.go` (`SessionInfo`, `UsageRecord`, `Entry`, `ToolCall`) |