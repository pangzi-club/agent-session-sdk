# ZCode — session store format & parsing

How the `zcode` agent stores sessions on disk, and how this SDK turns that store
into `SessionInfo`, `Entry[]` and `UsageRecord[]`.

ZCode keeps everything in a single SQLite database (`~/.zcode/cli/db/db.sqlite`);
there is no legacy JSON store. The transcript loader is shared with `opencode`
(`parseSessionDBTranscript`, `transcript_db.go:39`) — see [opencode.md](opencode.md)
for the full SQLite mechanics. This doc focuses on what is **specific to zcode**:
the `session.task_type` subagent filter, a rollout file used only for its size,
and a usage extractor built on dedicated `model_usage` / `tool_usage` tables that
can emit **negative input usage**. Shared helpers (`entriesFromContent`,
`toolCall`, `SessionTally`, `ScanAllUsage`, `ToTitle`) are described in
[claude.md](claude.md) and only summarized here.

## At a glance

| Property | Value |
|---|---|
| Agent id | `zcode` |
| Display name | ZCode |
| Store directory | `~/.zcode` |
| `canDeleteSessions` | `false` (`registry.go:27` sets no `canDelete`; the library only reports it) |
| Session scanner | `ScanZcode` (`zcode.go:13`) |
| Transcript loader | `parseSessionDBTranscript` (`transcript_db.go:39`), shared with `opencode` |
| Usage module | `scanZcodeUsage` (`usage_zcode.go:36`) |
| Billing | Token-metered against model rates; no credit accounting (`credits` omitted). Input can be negative. |
| Store format | SQLite `db.sqlite` (`session` / `message` / `part` / `model_usage` / `tool_usage`), plus a per-session rollout JSONL used only for `sizeBytes` |

## On-disk layout

```
~/.zcode/
  cli/
    db/
      db.sqlite                 # SQLite store: session, message, part, model_usage, tool_usage
      db.sqlite-wal             # write-ahead log sidecar (watched by the cache)
    rollout/
      model-io-<id>.jsonl       # per-session model I/O log; stat'd for sizeBytes only, never parsed
```

- **One SQLite store, no legacy fallback.** `ScanZcode` opens `cli/db/db.sqlite`
  read-only; the transcript loader and the usage extractor open the same file.
  Unlike `opencode`, there is no second JSON store to merge or dedup against.
- **`FilePath` carries a `#` marker.** A session's `FilePath` is
  `<dbPath>#<id>` (e.g. `~/.zcode/cli/db/db.sqlite#zc_abc`). The `#` is what tells
  the shared loader to split a SQLite reference (`splitDBRef`) rather than open a
  JSONL file.
- **The rollout file is size-only.** `cli/rollout/model-io-<id>.jsonl` is `stat`ed
  via `FileSize` for the scanner's `sizeBytes`; it is never read for a transcript
  or for usage. A missing rollout file yields `sizeBytes = 0`.
- **Subagent sessions are filtered by `session.task_type`** (see scanning), not by
  a directory layout or a message-level flag.

## Record format (SQLite)

`QueryMaps` (`shared.go:548`) returns each row as a column→value map, normalizing
`INTEGER`→`float64`, `TEXT`/`BLOB`→`string`, `NULL`→`nil`. zcode reads five tables.
The scanner uses `SELECT * FROM session`; the usage path uses explicit columns and
SQL `json_extract` on the `data` columns. The `message` / `part` tables are read
identically to `opencode` (see [opencode.md](opencode.md)); `model_usage` and
`tool_usage` are zcode-specific.

### `session` table

| Column | Used for |
|---|---|
| `id` | session id (string) |
| `directory` | workspace (absolute project path); required |
| `title` | session title (scanner: `ToTitle`; usage: the raw string) |
| `task_type` | subagent filter: kept only when `NULL`/absent or `'interactive'` |
| `time_created` | epoch ms → `createdAt` |
| `time_updated` | epoch ms → `updatedAt` |

### `message` table

Columns: `id`, `session_id`, `time_created` (epoch ms), `data` (JSON). Read by the
shared transcript loader and by the usage prompt query. The `message.data` fields
the SDK reads:

| Field | Where | Meaning |
|---|---|---|
| `role` | transcript + usage | `user` / `assistant` via `roleFromString` (transcript); usage requires exactly `"user"` |
| `time.created` | transcript | epoch ms; overrides the `time_created` column for the entry timestamp |
| `semantics.kind` | usage | must equal `"user_prompt"` for a prompt event |

### `part` table

Columns: `id`, `message_id`, `session_id`, `time_created` (epoch ms), `data`
(JSON). Read by the shared transcript loader and by the usage skill-name query.
The `part.data` fields the SDK reads:

| Field | Where | Meaning |
|---|---|---|
| `type`, `text`, `synthetic`, `metadata.visibility` | transcript | text body and drop filters (see opencode.md) |
| `tool`, `state.input`, `state.status` | transcript | tool card (see opencode.md) |
| `callID` | usage | joins to `tool_usage.tool_call_id` to attach a skill name |
| `state.input.skill` | usage | the skill name carried by a `Skill` tool part |

### `model_usage` table (usage records — zcode-specific)

One row per model request. Columns the SDK reads:

| Column | Used for |
|---|---|
| `session_id` | owning session |
| `model_id` | model id for the request |
| `completed_at` | epoch ms → record timestamp |
| `status` | only `'completed'` rows are read |
| `input_tokens` | total input (assumed to **include** cache reads) |
| `output_tokens` | output |
| `cache_read_input_tokens` | cache read; **subtracted from input** |
| `cache_creation_input_tokens` | cache write |

### `tool_usage` table (usage tool events — zcode-specific)

One row per tool call. Columns the SDK reads:

| Column | Used for |
|---|---|
| `session_id` | owning session |
| `tool_call_id` | joins to `part.data.callID` to attach a skill name |
| `tool_name` | tool name (`Skill` triggers the skill join) |
| `started_at` | epoch ms → tool event timestamp |

### Illustrative rows (synthetic)

`session` row as returned by `QueryMaps` (column→value):

```json
{"id":"zc_abc","directory":"/Users/alice/code/app","title":"Fix the bug","task_type":"interactive","time_created":1762939000000,"time_updated":1762940500000}
```

`message.data` for a user prompt (note `semantics.kind`, read only by usage):

```json
{"role":"user","semantics":{"kind":"user_prompt"},"time":{"created":1762940000000}}
```

`part.data` for a `Skill` tool part (note `callID` and `state.input.skill`, read
only by usage):

```json
{"type":"tool","tool":"Skill","callID":"call_abc","state":{"status":"completed","input":{"skill":"pdf"}}}
```

`model_usage` row (column→value) — the negative-input example:

```json
{"session_id":"zc_abc","model_id":"claude-sonnet-4","completed_at":1762940002000,"status":"completed","input_tokens":1200,"output_tokens":48,"cache_read_input_tokens":8000,"cache_creation_input_tokens":0}
```

`tool_usage` row (column→value):

```json
{"session_id":"zc_abc","tool_call_id":"call_abc","tool_name":"Skill","started_at":1762940001000}
```

## Session scanning → `SessionInfo`

`ScanZcode` (`zcode.go:13`) is a single database pass — no parallelism, no legacy
merge.

1. `dbPath = <agentDir>/cli/db/db.sqlite`. Open read-only via `OpenSQLite`
   (`shared.go:534`, `mode=ro`, `busy_timeout=1000`). **An open failure returns an
   error** (`nil, err`); `ScanSessions` (`registry.go:73`) treats that as "no
   sessions" for zcode without failing the other agents. There is no fallback
   store.
2. `SELECT * FROM session` via `QueryMaps`. **A query failure also returns an
   error.**
3. Per row:
   - **`task_type` filter** — read `row["task_type"]`; skip when the column is
     present **and** non-nil **and** not the string `"interactive"` (`IsStr`). So
     `NULL`/absent `task_type` and `'interactive'` are kept; anything else (e.g.
     `'subagent_child'`) is dropped. Because the query is `SELECT *`, a schema
     without the column keeps every row.
   - `id = AsString(row["id"])`, `workspace = AsString(row["directory"])` (strings
     only). **Skip if either is nil/empty.**
   - `createdAt = FirstOr(0, AsEpochMs(time_created))` — a finite number or an
     RFC3339 string, else `0`.
   - `updatedAt = FirstOr(createdAt, AsEpochMs(time_updated))`.
   - `title = ToTitle(AsString(row["title"]))`.
   - `sizeBytes = FileSize(<agentDir>/cli/rollout/model-io-<id>.jsonl)` — the real
     file size, `0` if missing.
   - `filePath = <dbPath>#<id>`.

Timestamps come from the db columns, not file mtime. `AsEpochMs` accepts a finite
number or an RFC3339 string; the columns are numeric epoch ms.

## Transcript parsing → `Entry[]`

`parseSessionDBTranscript` (`transcript_db.go:39`) is shared with `opencode`; see
[opencode.md](opencode.md) for the full mechanics. In brief:

1. **Split the reference.** `splitDBRef` (`transcript_db.go:14`) splits `FilePath`
   on the first `#` into `dbPath` and `sessionID`; both halves must be non-empty or
   the loader errors with `session %q has no database reference`. (This is the
   mirror image of `transcriptFile`, `transcript.go:161`, which *rejects* a `#`
   path for the JSONL loaders.)
2. **Open read-only**; a missing/unopenable db is an error here.
3. **Join messages to parts:**

```sql
SELECT m.data AS message_data, m.time_created AS message_time, p.data AS part_data
FROM message m
JOIN part p ON p.message_id = m.id
WHERE m.session_id = ?
ORDER BY m.time_created, m.id, p.time_created, p.id
```

The inner join means a message with no parts yields no rows. Per row, parse
`message_data` and `part_data` as JSON objects (**skip the row if either fails**).
`role = roleFromString(message.role)`; `timestamp = AsEpochMs(message_time)` (the
`m.time_created` column), **overridden by `message.time.created`** when it parses.

The part `type` drives the entry: `text`/missing → a text entry (dropped when
empty, `synthetic == true`, or `metadata.visibility == "model-only"`); `tool` → one
**always-assistant** tool entry built from `part.tool` + `part.state.input`, with
`state.status` appended to `Meta` when it is neither `""` nor `completed`;
`reasoning` / `step-start` / `step-finish` / `file` / `timeline` and anything else →
dropped. Because each part is its own row, multiple text parts of one message
become separate entries sharing the message timestamp.

**zcode-specific note.** The transcript reads **only `message` and `part`**. It
never touches `model_usage`, `tool_usage`, or `message.semantics.kind` — those are
usage-only. The transcript and the usage extractor therefore read different
aspects of the same database.

### Cache invalidation (`transcriptSignature`, `transcript.go:111`)

For a `#` path the signature is `fileSignature(dbPath) + "+" +
fileSignature(dbPath + "-wal")` — the main db file's `size:mtimeMs` plus its
write-ahead log sidecar, so appends still sitting in the WAL invalidate the cached
transcript. The `-shm` sidecar is not watched. Identical to `opencode`.

## Usage extraction → `UsageRecord[]`

This is where zcode diverges most from `opencode`. `scanZcodeUsage`
(`usage_zcode.go:36`) reads **four** tables — `session` (list), `message`
(prompts), `tool_usage` (tools) joined to `part` (skill names), and `model_usage`
(records) — merges three event kinds per session, and walks them in time order.

**Degradation.** An unopenable db yields an **empty result, not an error** (no
global status downgrade) — the opposite of `ScanZcode`, which errors. Each later
query failure degrades gracefully (noted per query below).

### Session list

```sql
SELECT id, directory, title, time_created, time_updated FROM session
WHERE task_type IS NULL OR task_type = 'interactive'
```

- **Query failure → empty result.**
- Per row: `id` and `directory` must be strings (a direct `.(string)` assertion,
  not `AsString`/`sqlToString`); `time_created` must be a finite number
  (`AsNumber`). **If any is missing the session is skipped** — note this *requires*
  a numeric `time_created`, whereas the scanner defaults `createdAt` to `0`.
- `title` = the **raw** `row["title"]` string (**no `ToTitle`** — unlike the scanner
  and unlike opencode usage); a non-string title stays `nil`.
- `createdAt = time_created`; `updatedAt = FirstOr(created, numberPtr(time_updated))`
  (numeric only); `sizeBytes = 0`; `filePath = <dbPath>#<id>`.
- Only these ids go into `sessionIDs`; events for any other session id drop out.
  This is the **sole subagent filter** — there is no message-level `parentID`
  filter as in opencode.

### Events

Three kinds are merged per session (`zcodeEntry`): **prompt**, **tool**,
**record**. All timestamps come from numeric **columns** via `AsNumber` (not from
the JSON `data`).

- **Prompts — `message` table:**

```sql
SELECT session_id, time_created,
  json_extract(data, '$.role') AS role, json_extract(data, '$.semantics.kind') AS kind
FROM message
```

  Keep only `role == "user"` **and** `semantics.kind == "user_prompt"` (both via
  `IsStr`). `ts = time_created` column. Add a **prompt** event. Query failure →
  sessions kept, records empty. Prompt detection is by the `semantics.kind` tag —
  not text inspection (claude) and not "every non-subagent user message"
  (opencode).

- **Skill names — `part` table (best-effort):**

```sql
SELECT session_id, json_extract(data, '$.callID') AS call_id,
  json_extract(data, '$.state.input.skill') AS skill
FROM part
WHERE json_extract(data, '$.tool') = 'Skill'
  AND json_extract(data, '$.state.input.skill') IS NOT NULL
```

  Builds `skillByCall[session_id + "#" + callID] = skill` (all three must be
  strings, `skill` non-empty). **On any error — including a missing `part` table on
  an older db — the whole block is skipped**, so `skillByCall` stays empty and tool
  calls are counted without skill names.

- **Tools — `tool_usage` table:**

```sql
SELECT session_id, tool_call_id, tool_name, started_at FROM tool_usage
```

  `ts = started_at` column; `tool = AsString(tool_name)`. When `tool_name == "Skill"`,
  look up `skillByCall[session_id + "#" + tool_call_id]` and attach the skill name.
  Add a **tool** event. Query failure → sessions kept, records empty. **zcode does
  populate `SkillCallsByName`** (opencode never does).

- **Records — `model_usage` table:**

```sql
SELECT session_id, model_id, completed_at,
  input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens
FROM model_usage WHERE status = 'completed'
```

  Only **completed** requests (error/cancelled rows are excluded even though their
  tokens exist). `ts = completed_at` column. Add a **record** event carrying the
  row. Query failure → sessions kept, records empty.

### Ordering and the tally

Per session, events are stably sorted by `ts`, ties broken by `zcodeOrderOf`
(record = 1, prompt/tool = 0) so **activity at the same instant settles before the
record point**. Then a `SessionTally` (`tally.go:36`) is walked:

- **record**: `AddEvent(ts, TallyAssistant)` first, *then* build a `UsageRecord`:

  | Record field | Source |
  |---|---|
  | `Model` | `AsString(model_id)` |
  | `Timestamp` | `ts` (`completed_at`) |
  | `InputTokens` | `AsTokenCount(input_tokens)` − `AsTokenCount(cache_read_input_tokens)` |
  | `OutputTokens` | `AsTokenCount(output_tokens)` |
  | `CacheReadTokens` | `AsTokenCount(cache_read_input_tokens)` |
  | `CacheWriteTokens` | `AsTokenCount(cache_creation_input_tokens)` |

  Then `record.ApplyInterval(tally.TakeUntil(ts))`. **Every completed row emits a
  record** — there is no "tokens missing → skip" guard (opencode has one);
  `AsTokenCount` defaults missing/non-positive fields to `0`.
- **prompt**: `AddEvent(ts, TallyPrompt)`.
- **tool**: `AddEvent(ts, TallyTool, tool, skill)`.

After the walk, the last record absorbs the tail via `MergeInterval(tally.Flush())`.
**A session with events but no completed `model_usage` row emits nothing.**
`SessionTally` drops out-of-order/non-positive events and caps each inter-event gap
at `ActiveGapCapMs` (120 000 ms) so idle time never inflates `ActiveMs`; see
[claude.md](claude.md) for the full model.

### Negative input usage (the headline quirk)

`InputTokens = AsTokenCount(input_tokens) − cacheRead` (`usage_zcode.go:196`), with
**no clamping**.

- `AsTokenCount` (`shared.go:152`) floors a positive finite number, else `0` — so
  each operand is individually non-negative, but their **difference is not**.
- The subtraction assumes zcode's `input_tokens` **includes** cache reads, and
  normalizes it to the contract's "input excludes cache traffic" rule
  (`contract.go:81`). When `cache_read_input_tokens` exceeds `input_tokens` — e.g.
  a mostly-cached prompt whose reported input is small — the result is **negative**.
- The SDK preserves it. `UsageRecord.InputTokens` is `float64` (`contract.go:49`),
  and the package deliberately keeps JS-number semantics "including zcode's
  negative input usage" (`contract.go:11`). Nothing floors, abs's or clamps it.
- **Worked synthetic example:** the `model_usage` row above (`input_tokens = 1200`,
  `cache_read_input_tokens = 8000`) yields `InputTokens = 1200 − 8000 = −6800`,
  `OutputTokens = 48`, `CacheReadTokens = 8000`, `CacheWriteTokens = 0`.
- **Downstream:** the negative input flows into the session's `tokenUsage[0]` (the
  sum of record inputs, `usage.go:96`) and into pricing as-is (`pricing.go:24`,
  `pricing.go:197`): `totalTokens = input + output + cacheRead + cacheWrite` and
  `usd = input*rate + …` — a negative input lowers both. Contrast `opencode`, which
  uses `tokens.input` directly with no subtraction (see [opencode.md](opencode.md)).

## Aggregation & cost

- `ScanAllUsage` (`usage.go:45`) concatenates records in module order — zcode is
  `usage.go:28`, ahead of opencode (`usage.go:32`) — and merges sessions into an
  ordered map keyed by bare `sessionId` (last-write-wins). Each session's
  `tokenUsage` is `[input, output, cacheRead, cacheWrite, 3]`, where `input` is the
  (possibly negative) sum of record `InputTokens` and `3` is the status flag.
  `filePath` is tildified against `home`. Sessions with no records omit
  `tokenUsage`.
- zcode is token-billed: `EstimateTokenCost` (`pricing.go:189`) prices a record from
  model rates (`NormalizeModelId` → `LookupModelPrice`). It is not in the
  credit-billed set (only `qoder` / `qoder-cn`), so `credits` is omitted. An
  unresolvable model prices the record at `0` and counts all its tokens (negative
  input included) as unpriced.

## Quirks & edge cases

- **Negative input usage** (see above) — the defining quirk; `input − cacheRead` is
  unclamped and preserved end to end through aggregation and pricing.
- **`canDeleteSessions` is `false`.** `registry.go:27` sets no `canDelete`, so the
  reported flag is `false`; zcode is not delete-capable in this library.
- **Dedicated usage tables.** Records come from `model_usage`, tools from
  `tool_usage`, prompts from `message.semantics.kind`, skill names from `part` —
  not from `message.data.tokens` / `part` tool rows as in opencode.
- **Only completed requests.** `model_usage.status = 'completed'`; error/cancelled
  rows are excluded even though their tokens exist.
- **Every completed request emits a record** (no tokens-nil guard); a session with
  no completed request emits nothing.
- **Skill accounting exists but is best-effort.** Joined via
  `(session_id, part.callID ↔ tool_usage.tool_call_id)`; a missing/failing `part`
  table degrades to tool counts without skill names.
- **Prompt detection is tag-based:** `role == "user"` **and**
  `semantics.kind == "user_prompt"`. No text inspection.
- **One subagent filter, at the session level** (`task_type`). No message-level
  `parentID` filter (opencode has two).
- **Scanner vs usage title.** The scanner normalizes with `ToTitle`; usage keeps
  the raw title string.
- **Scanner vs usage coercion & strictness.** The scanner uses `AsString` +
  `AsEpochMs` (number or RFC3339) and defaults `createdAt` to `0`; usage uses a
  direct `.(string)` + `AsNumber` (numeric only) and **skips a session whose
  `time_created` is not numeric**.
- **`task_type` filter differs in mechanism.** The scanner reads `SELECT *` and
  filters in Go only when the column is present/non-nil; usage hard-codes
  `WHERE task_type IS NULL OR task_type = 'interactive'`. A schema lacking
  `task_type` would still list sessions in the scanner but fail the usage query
  (empty result).
- **Open-failure asymmetry.** `ScanZcode` returns an error on db open/query failure
  (the caller treats it as no sessions); `scanZcodeUsage` returns empty (no error,
  no status downgrade). Usage query failures after the session list keep sessions
  but drop records.
- **`sizeBytes` is the rollout file's real size** (`cli/rollout/model-io-<id>.jsonl`),
  not a data-length sum (opencode) and not `0`; usage sessions use `0`. The rollout
  file is never parsed.
- **Column timestamps, not JSON.** The record timestamp is `completed_at`, the tool
  timestamp is `started_at`, the prompt timestamp is the message `time_created`
  column — while the transcript prefers `message.time.created` JSON.
- **Zero timestamps are not rejected at collection** (`AsNumber` accepts `0`); the
  tally ignores non-positive timestamps for active time, but a completed request
  with `completed_at = 0` still emits a record at timestamp `0`.
- **Transcript reads only `message` + `part`.** `model_usage` / `tool_usage` /
  `semantics.kind` are usage-only.

## Source map

| Topic | Location |
|---|---|
| Registry entry | `registry.go:27` |
| Usage module registration | `usage.go:28` |
| Session scan (`SELECT *` + `task_type` filter, rollout size) | `zcode.go:13` (`ScanZcode`) |
| Transcript loader (shared with opencode) | `transcript_db.go:39` (`parseSessionDBTranscript`, `splitDBRef:14`, `parseJSONMap:25`) |
| Content → entries, tool cards, cleaning | `transcript.go` (`entriesFromContent:238`, `toolCall:344`, `roleFromString:406`) |
| Cache signature (db + `-wal`) | `transcript.go:111` (`transcriptSignature`, `fileSignature`); `transcriptFile:161` |
| Usage extraction | `usage_zcode.go:36` (`scanZcodeUsage`, `zcodeOrderOf:217`, `numberPtr:224`) |
| Negative input subtraction | `usage_zcode.go:196`; `contract.go:11`; `pricing.go:24` |
| Activity tally | `tally.go:36` (`SessionTally`, `AddEvent:44`) |
| Aggregation | `usage.go:45` (`ScanAllUsage`, `ApplyInterval:108`, `MergeInterval:126`) |
| Pricing | `pricing.go:189` (`EstimateTokenCost`, `NormalizeModelId:95`, `LookupModelPrice:117`) |
| SQLite access | `shared.go:534` (`OpenSQLite`), `shared.go:548` (`QueryMaps`) |
| Shared helpers | `shared.go` (`ToTitle:211`, `AsEpochMs:129`, `AsNumber:44`, `AsTokenCount:152`, `AsString:36`, `IsStr:75`, `FileSize:358`, `FirstOr:108`) |
| Output contract | `contract.go` (`SessionInfo:22`, `UsageRecord:44`, `Entry:126`, `ToolCall:115`) |