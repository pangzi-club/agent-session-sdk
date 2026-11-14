# OpenCode — session store format & parsing

How the `opencode` agent stores sessions on disk, and how this SDK turns that
store into `SessionInfo`, `Entry[]` and `UsageRecord[]`.

OpenCode keeps its sessions in a SQLite database (`opencode.db`). A legacy
per-session JSON store still exists on older installs; the scanner reads both and
dedupes by id. The transcript loader is shared with `zcode`
(`parseSessionDBTranscript`, `transcript_db.go:39`) — see [zcode.md](zcode.md) for
the same loader applied to a different schema. Shared helpers
(`entriesFromContent`, `toolCall`, `SessionTally`, `ScanAllUsage`) are described in
[claude.md](claude.md) and only summarized here.

## At a glance

| Property | Value |
|---|---|
| Agent id | `opencode` |
| Display name | OpenCode |
| Store directory | `~/.local/share/opencode` (XDG data dir) |
| `canDeleteSessions` | `false` (`registry.go:26` sets no `canDelete`; the library only reports it) |
| Session scanner | `ScanOpenCode` (`opencode.go:16`) |
| Transcript loader | `parseSessionDBTranscript` (`transcript_db.go:39`), shared with `zcode` |
| Usage module | `scanOpenCodeUsage` (`usage_opencode.go:31`) |
| Billing | Token-metered against model rates; no credit accounting (`credits` omitted) |
| Store format | SQLite `opencode.db` (`session` / `message` / `part`), plus legacy `storage/session/**/*.json` |

## On-disk layout

```
~/.local/share/opencode/
  opencode.db                     # SQLite store: session, message, part tables
  opencode.db-wal                 # write-ahead log sidecar (watched by the cache)
  storage/
    session/
      <projectID>/
        <id>.json                 # legacy: one JSON object of session metadata
        <id>.json
```

- **Two coexisting stores.** `ScanOpenCode` reads the legacy JSON store first,
  then the database, and merges them into an insertion-ordered map keyed by
  session id. **The database wins**: a db row overwrites a legacy entry with the
  same id (`opencode.go:24-33`). A legacy-only session survives only if the db has
  no row for that id.
- **The database is the transcript source.** `message` and `part` rows hold the
  conversation; the legacy JSON files hold metadata only and are never parsed for
  a transcript (see Quirks).
- **`FilePath` carries a `#` marker.** A db session's `FilePath` is
  `<dbPath>#<sessionId>` (e.g. `~/.local/share/opencode/opencode.db#ses_abc`). The
  `#` is what tells the shared loader to split a SQLite reference rather than open
  a JSONL file. A legacy session's `FilePath` is the plain `.json` path with no
  `#`.
- The legacy scan is recursive (`ListFilesE(..., ".json", true)`), descending into
  every `<projectID>` subdirectory; dot-prefixed files are skipped. A missing
  storage tree is an empty store; any other listing or read error fails the
  OpenCode scan (a legacy file that vanished after enumeration is skipped).

## Record format (SQLite + JSON)

The database has three tables the SDK reads. `QueryMaps` (`shared.go:548`)
returns each row as a column→value map, normalizing `INTEGER`→`float64`,
`TEXT`/`BLOB`→`string`, `NULL`→`nil`. The `data` columns are `TEXT` holding one
JSON object each; the SDK parses them with `parseJSONMap` / `json.Unmarshal`.

### `session` table

| Column | Used for |
|---|---|
| `id` | session id (string) |
| `parent_id` | non-empty string ⇒ subagent session, skipped |
| `directory` | workspace (absolute project path) |
| `title` | session title (normalized by `ToTitle`) |
| `time_created` | epoch ms → `createdAt` |
| `time_updated` | epoch ms → `updatedAt` |

### `message` table

Columns: `id`, `session_id`, `time_created` (epoch ms), `data` (JSON). The
`message.data` object fields the SDK reads:

| Field | Where | Meaning |
|---|---|---|
| `role` | transcript + usage | `user` / `assistant` (via `roleFromString`) |
| `time.created` | transcript + usage | epoch ms; overrides the `time_created` column for the entry timestamp |
| `parentID` | usage | non-empty string ⇒ subagent message, not a main-session prompt |
| `modelID` | usage | model id for the assistant turn |
| `tokens` | usage | `{ input, output, reasoning, cache: { read, write } }` |

`tokens.reasoning` is **ignored** (never mapped to a record field).

### `part` table

Columns: `id`, `message_id`, `session_id`, `time_created` (epoch ms), `data`
(JSON). One message has many parts; each part is one transcript row. The
`part.data` object fields the SDK reads:

| Field | Meaning |
|---|---|
| `type` | `text`, `tool`, `reasoning`, `step-start`, `step-finish`, `file`, `timeline` (missing/empty treated as `text`) |
| `text` | the text body (for `text` parts) |
| `synthetic` | boolean `true` ⇒ injected text, dropped |
| `metadata.visibility` | `"model-only"` ⇒ dropped |
| `tool` | tool name (for `tool` parts) |
| `state.input` | tool arguments object |
| `state.status` | tool status; anything but `""`/`completed` is appended to the card meta |

### Illustrative rows (synthetic)

`message.data` for an assistant turn:

```json
{"role":"assistant","modelID":"claude-sonnet-4","parentID":"","time":{"created":1762940002000},"tokens":{"input":1200,"output":48,"reasoning":0,"cache":{"read":8000,"write":0}}}
```

`part.data` for a text part and a tool part:

```json
{"type":"text","text":"Reading the file."}
{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":"/Users/alice/code/app/main.go"}}}
```

Legacy `storage/session/<projectID>/<id>.json`:

```json
{"id":"ses_abc","directory":"/Users/alice/code/app","title":"Fix the bug","time":{"created":1762939000000,"updated":1762940500000}}
```

## Session scanning → `SessionInfo`

`ScanOpenCode` (`opencode.go:16`) runs the two scanners and merges by id.

### Legacy storage (`scanOpenCodeLegacyStorage`, `opencode.go:84`)

List `storage/session/**/*.json` recursively. Per file: read and parse the JSON
object (unreadable / unparseable / non-object files are skipped).

- `id` = `id`, else the file name minus `.json`.
- `workspace` = `directory`; **skip the session if empty/missing**.
- `createdAt` = `AsEpochMs(time.created)` ?? `0`.
- `updatedAt` = `AsEpochMs(time.updated)` ?? `createdAt`.
- `title` = `ToTitle(title)`.
- `sizeBytes` = the JSON file's size on disk.
- `filePath` = the `.json` path (no `#`).
- **A failed `os.Stat` propagates as an error** (`opencode.go:111-116`), which
  fails the whole `opencode` agent — a deliberate match to the TS behavior, not a
  per-file skip.

### Database (`scanOpenCodeDatabase`, `opencode.go:37`)

Open `opencode.db` read-only. **A missing db returns empty** (`nil, nil`) so
the legacy results stand; a db that exists but cannot be opened or queried is
an error that fails the `opencode` scan. The query computes size from the
data columns:

```sql
SELECT s.id, s.parent_id, s.directory, s.title, s.time_created, s.time_updated,
  (SELECT COALESCE(SUM(LENGTH(p.data)), 0) FROM part p WHERE p.session_id = s.id) +
  (SELECT COALESCE(SUM(LENGTH(m.data)), 0) FROM message m WHERE m.session_id = s.id) AS size_bytes
FROM session s
```

Per row:

- Skip if `parent_id` is a non-empty string (subagent session).
- `id` = `AsString(id)`, `workspace` = `AsString(directory)`; skip if either is
  empty/missing.
- `createdAt` = `AsEpochMs(time_created)` ?? `0`.
- `updatedAt` = `AsEpochMs(time_updated)` ?? `createdAt`.
- `title` = `ToTitle(title)`.
- `sizeBytes` = `size_bytes` (the summed `LENGTH(data)`), else `0`.
- `filePath` = `<dbPath>#<id>`.

Timestamps come from the db columns, not file mtime. `AsEpochMs` accepts a finite
number or an RFC3339 string; the columns are numeric epoch ms.

## Transcript parsing → `Entry[]`

`parseSessionDBTranscript` (`transcript_db.go:39`) is shared with `zcode`.

1. **Split the reference.** `splitDBRef` (`transcript_db.go:14`) splits `FilePath`
   on the first `#` into `dbPath` and `sessionID`; both halves must be non-empty
   or the loader errors with `session %q has no database reference`. (This is the
   mirror image of `transcriptFile`, `transcript.go:161`, which *rejects* a `#`
   path for the JSONL loaders.)
2. **Open read-only** via `OpenSQLite` (`shared.go:534`, `mode=ro`,
   `busy_timeout=1000`). A missing/unopenable db is an error here.
3. **Join messages to parts:**

```sql
SELECT m.data AS message_data, m.time_created AS message_time, p.data AS part_data
FROM message m
JOIN part p ON p.message_id = m.id
WHERE m.session_id = ?
ORDER BY m.time_created, m.id, p.time_created, p.id
```

The inner join means a message with no parts yields no rows. Rows are ordered by
message time, message id, then part time, part id.

Per row, parse `message_data` and `part_data` as JSON objects; **skip the row if
either fails to parse**. Then:

- `role` = `roleFromString(message.role)` (`user`/`human`/`input` → user,
  `assistant`/`agent`/`model`/… → assistant, `system`/`developer` → system, else
  empty).
- `timestamp` = `AsEpochMs(message_time)` (the `m.time_created` column),
  **overridden by `message.time.created`** when that parses. `0` when neither is
  present.

The part `type` drives the entry:

| `part.type` | Treatment |
|---|---|
| `text` or missing/empty | Emit a text entry (see filters below) |
| `tool` | Emit one assistant tool entry |
| `reasoning` | Emit an assistant thinking entry (bounded `part.text`) |
| `step-start`, `step-finish`, `file`, `timeline` | Dropped (internal steps / attachments) |
| anything else | Dropped (no matching case) |

**Text parts.** Read `part.text`; skip when it is empty, when `part.synthetic` is
boolean `true`, or when `part.metadata.visibility == "model-only"`. If `role` is
empty, default it to `assistant`. The string is passed to
`entriesFromContent(role, text, timestamp)` (`transcript.go:238`), which cleans it
(`cleanUserText` for user role, `cleanAssistantText` otherwise) and emits a single
text entry — or none if cleaning empties it. Because each part is its own row,
**multiple text parts of one message become separate entries** (they are not
joined the way content-block arrays are in the Claude loader); they share the
message timestamp.

**Tool parts.** Read `part.tool` (skip if empty). Build the card with
`toolCall(name, part.state.input)` (`transcript.go:344`): kind `file`/`command`/
`search`/`other`, a clipped title from the input, meta = the tool name. Then, if
`part.state.status` is non-empty and not `completed`, append `" | " + status` to
`Meta` (e.g. `bash | running`). `callID` becomes `ID`, `state.input` /
`state.output` are kept as the bounded `Input` / `Output` payloads, and `Failed`
is set when the status is `error`. The entry is **always `RoleAssistant`** with
exactly one `ToolCall`, regardless of the message role.

**Reasoning parts.** Read `part.text` (skip if empty) and emit an assistant
thinking entry bounded to `MaxThinkingChars`.

**Timing (`Entry.DurationMs`).** Reasoning and tool entries carry their part-level
`time.end - time.start` span; text entries carry the message's
`time.completed - time.created` generation span. `0` when the store keeps no
timing. Consumers can show a thinking step's duration and sum a turn's wall-clock
work from these.

### Cache invalidation (`transcriptSignature`, `transcript.go:111`)

For a `#` path the cache signature is `fileSignature(dbPath) + "+" +
fileSignature(dbPath + "-wal")` — the main db file's `size:mtimeMs` plus its
write-ahead log sidecar. Appends still sitting in the WAL therefore invalidate the
cached transcript. (The `-shm` sidecar is not watched.) A legacy `.json` path with
no `#` is signed as a plain file.

## Usage extraction → `UsageRecord[]`

`scanOpenCodeUsage` (`usage_opencode.go:31`) opens `opencode.db`; **a missing
db yields an empty result** (OpenCode not installed ⇒ no data, no module
failure), while a db that exists but cannot be opened or queried — including
the session / message / part queries — is an error reported per agent.
`scanOpenCodeDb` (`usage_opencode.go`) builds a session list and
three event streams, then walks them per session in time order.

### Session list

```sql
SELECT id, parent_id, directory, title, time_created, time_updated FROM session
```

Same subagent filter (`parent_id` non-empty string ⇒ skip) and workspace-required
rule as the scanner. `id`/`directory` go through `sqlToString` (coerces a numeric
value to its string form; `nil`→`""`). `createdAt`/`updatedAt` use `AsNumber`
directly (numeric only). `title` = `ToTitle(title)`, `sizeBytes` = `0`,
`filePath` = `<dbPath>#<id>`. Only these ids are kept in `sessionIDs`; events for
any other session id are dropped.

### Events

Three kinds are merged per session (`opencodeEntry`): **prompt**, **tool**,
**record**.

- **Messages** — `SELECT session_id, data FROM message`. Parse `data`; take
  `ts = time.created` (numeric); **skip when `ts == 0`**.
  - `role == "user"`: skip if `parentID` is a non-empty string (subagent message);
    otherwise add a **prompt** event. There is no text inspection — every
    timestamped non-subagent user message counts as a prompt.
  - `role == "assistant"`: add a **record** event carrying the parsed object.
- **Parts** — `SELECT session_id, time_created, data FROM part`. Parse `data`;
  keep only `type == "tool"` with a non-nil `tool`. Take `ts` from the
  **`time_created` column** (not the JSON); skip when missing or `0`. Add a
  **tool** event with the tool name. **No skill name is attached** — OpenCode
  never populates `SkillCallsByName`.

### Ordering and the tally

Per session, events are stably sorted by `ts`, ties broken by `opencodeOrderOf`
(record = 1, prompt/tool = 0) so **activity at the same instant settles before the
record point**. Then a `SessionTally` (`tally.go:36`) is walked:

- **record**: `AddEvent(ts, TallyAssistant)` first, *then* read `tokens`. If
  `tokens` is missing, `continue` — the assistant event still counts toward active
  time, but no record is emitted (its activity lands on the next record, or the
  last one via `Flush`). Otherwise emit a `UsageRecord`:

  | Record field | Source |
  |---|---|
  | `Model` | `message.modelID` |
  | `Timestamp` | `ts` (`time.created`) |
  | `InputTokens` | `AsTokenCount(tokens.input)` |
  | `OutputTokens` | `AsTokenCount(tokens.output)` |
  | `CacheReadTokens` | `AsTokenCount(tokens.cache.read)` |
  | `CacheWriteTokens` | `AsTokenCount(tokens.cache.write)` |

  `tokens.reasoning` is ignored. Input is the non-cached input already (no
  subtraction, unlike `zcode`). `AsTokenCount` floors positive finite numbers,
  else `0`. Then `record.ApplyInterval(tally.TakeUntil(ts))`.
- **prompt**: `AddEvent(ts, TallyPrompt)`.
- **tool**: `AddEvent(ts, TallyTool, toolName)`.

After the walk, the last record absorbs the tail via
`MergeInterval(tally.Flush())`. **A session with events but no record emits
nothing** (`sessionRecords` is appended only when non-empty).

`SessionTally` drops out-of-order events and caps each inter-event gap at
`ActiveGapCapMs` (120 000 ms) so idle time never inflates `ActiveMs`; see
[claude.md](claude.md) for the full model.

## Aggregation & cost

- `ScanAllUsageDetailed` (`usage.go`) concatenates records in module order and
  merges sessions into an ordered map keyed by bare `sessionId`
  (last-write-wins; the later module in aggregation order wins a shared id).
  Each session's `tokenUsage` is
  `[input, output, cacheRead, cacheWrite, 3]` — input excludes cache traffic, `3`
  is the status flag. `filePath` is tildified against `home`. Sessions with no
  records omit `tokenUsage`. Failed modules are reported as `AgentScanFailure`
  entries.
- OpenCode is token-billed: `EstimateTokenCost` (`pricing.go:189`) prices a record
  from model rates (`NormalizeModelId` → `LookupModelPrice`). It is not in the
  credit-billed set, so `credits` is omitted. An unresolvable model prices the
  record at `0` and counts all its tokens as unpriced.

## Quirks & edge cases

- **Database wins the dedup.** Legacy JSON is read first; a db row with the same
  id overwrites it. Ordering keeps the legacy insertion position for shared ids.
- **Legacy-only sessions have no loadable transcript.** Their `FilePath` is a
  plain `.json` path with no `#`, so `splitDBRef` fails and
  `parseSessionDBTranscript` returns an error. They appear in the session list but
  cannot be opened as a transcript (the loader only reads the db).
- **`sizeBytes` is a data-length sum, not a file size.** For db sessions it is
  `SUM(LENGTH(part.data)) + SUM(LENGTH(message.data))`. SQLite `LENGTH` on a `TEXT`
  value counts characters, so this is a character-count proxy, not bytes on disk.
  Legacy sessions use the real file size; usage sessions use `0`.
- **Two subagent filters.** The `session.parent_id` column drops subagent
  sessions; the `message.parentID` JSON field drops subagent user messages from
  the prompt count. They are different fields at different levels.
- **Tool entries are always assistant**, even when the parent message role is
  user or system; text entries keep the message role (defaulting to assistant when
  the role is unrecognized).
- **No skill accounting.** Tool events carry only a name; `SkillCallsByName` is
  always nil for OpenCode.
- **Timestamp override.** The entry timestamp prefers `message.time.created` over
  the `time_created` column used for ordering.
- **Assistant-without-tokens still tallies.** The assistant event is added before
  the `tokens` nil-check, so it contributes active time even when no record is
  emitted.
- **A legacy `stat` failure fails the whole agent** (error propagates), while a db
  open/query failure degrades silently to empty.
- **Usage vs scanner coercion.** The usage path uses `sqlToString` (numbers →
  strings) and `AsNumber` (numeric timestamps only); the scanner uses `AsString`
  (strings only) and `AsEpochMs` (numbers or RFC3339). For the numeric db columns
  these agree.

## Source map

| Topic | Location |
|---|---|
| Registry entry | `registry.go:26` |
| Usage module registration | `usage.go:32` |
| Session scan (legacy + db dedup) | `opencode.go` (`ScanOpenCode`, `scanOpenCodeDatabase`, `scanOpenCodeLegacyStorage`) |
| Transcript loader (shared with zcode) | `transcript_db.go:39` (`parseSessionDBTranscript`, `splitDBRef`, `parseJSONMap`) |
| Content → entries, tool cards, cleaning | `transcript.go` (`entriesFromContent`, `toolCall`, `cleanUserText`) |
| Cache signature (db + `-wal`) | `transcript.go:111` (`transcriptSignature`, `fileSignature`); `transcriptFile:161` |
| Usage extraction | `usage_opencode.go` (`scanOpenCodeUsage`, `scanOpenCodeDb`, `opencodeOrderOf`, `sqlToString`) |
| Activity tally | `tally.go` (`SessionTally`) |
| Aggregation & failure reporting | `usage.go` (`ScanAllUsageDetailed`, `ApplyInterval`, `MergeInterval`), `registry.go` (`AgentScanFailure`) |
| Pricing | `pricing.go` (`EstimateTokenCost`, `NormalizeModelId`, `LookupModelPrice`) |
| SQLite access | `shared.go:534` (`OpenSQLite`), `shared.go:548` (`QueryMaps`) |
| Shared helpers | `shared.go` (`ToTitle`, `AsEpochMs`, `AsNumber`, `AsTokenCount`, `FileSize`, `ListFiles`) |
| Output contract | `contract.go` (`SessionInfo`, `UsageRecord`, `Entry`, `ToolCall`) |