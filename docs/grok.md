# Grok Build — session store format & parsing

How the `grok` agent (Grok Build, xAI's CLI) stores sessions on disk, and how
this SDK turns that store into `SessionInfo`, `Entry[]` and `UsageRecord[]`.

Grok keeps one directory per session, holding a `summary.json` metadata file and
an `updates.jsonl` event stream. The scanner reads only `summary.json`; the
event stream contributes its size and path at scan time and is consumed later
by the usage extractor. The transcript is routed through the generic loader,
which does not understand Grok's nested event shape — see the transcript
section for the consequence. This doc follows the shared section order: at a
glance → on-disk layout → record format → session scanning → transcript parsing
→ usage extraction → aggregation & cost → quirks → source map.

## At a glance

| Property | Value |
|---|---|
| Agent id | `grok` |
| Display name | Grok Build |
| Store directory | `~/.grok` |
| `canDeleteSessions` | `false` (`registry.go:31`; the library only reports this — deletion is the consumer's job) |
| Session scanner | `ScanGrok` (`grok.go:15`) — reads `summary.json` per session dir |
| Transcript loader | `parseGenericTranscript` (`transcript_generic.go:5`), shared with `workbuddy` |
| Usage module | `scanGrokUsage` (`usage_grok.go:17`) |
| Billing | Token-metered against model rates; no credit accounting (`credits` omitted) |
| Store format | Per-session directory: `summary.json` (metadata) + `updates.jsonl` (JSON-RPC event stream) |

## On-disk layout

```
~/.grok/
  sessions/
    %2FUsers%2Falice%2Fcode%2Fapp/      # one directory per project (URL-encoded cwd)
      <uuid>/                           # one directory per session
        summary.json                    # metadata: info.id, info.cwd, session_summary, timestamps
        updates.jsonl                   # event stream: transcript filePath + usage body
      <uuid>/
        ...
    %2FUsers%2Falice%2Fcode%2Fother/
      ...
```

- **Two levels under `sessions/`.** `ListGrokSessionDirs` (`grok.go:36`)
  enumerates `sessions/<projectDir>/<sessionDir>` with `ListDirs` twice
  (`shared.go:371`, which skips dot-prefixed directories). The project directory
  name is the URL-encoded cwd; the session directory name is a uuid.
- **`summary.json` is the metadata source.** A directory without a readable,
  parseable `summary.json` is not a session and is skipped.
- **`updates.jsonl` is the body.** The scanner only stats it for `sizeBytes` and
  records it as `filePath`; the usage extractor reads it later. The generic
  transcript loader is pointed at it too, but emits nothing (see below).
- The usage scan reuses `ListGrokSessionDirs` and `ScanGrokSessionDir`, so the
  layout rules live in one place.

## Record format

### `summary.json`

A single JSON object. The fields the SDK reads:

| Field | Type | Meaning |
|---|---|---|
| `info.id` | string | Session id (falls back to the session directory name) |
| `info.cwd` | string | Workspace path (falls back to URL-decoding the project dir name) |
| `session_summary` | string | Title source, normalized by `ToTitle` |
| `created_at` | number (ms) \| string (RFC3339) | Creation time; falls back to the session dir mtime |
| `updated_at` | number (ms) \| string (RFC3339) | Update time; falls back to the session dir mtime |

### `updates.jsonl`

Each line is a JSON-RPC-style notification. The SDK reads only these paths:

| Field | Type | Meaning |
|---|---|---|
| `timestamp` | number (epoch **seconds**) | Line-top event time; fallback timestamp (×1000) |
| `params._meta.agentTimestampMs` | number (ms) \| string | Preferred event time |
| `params.update.sessionUpdate` | string | Event discriminator (see below) |
| `params.update.title` | string | On `tool_call`: the tool name |
| `params.update.usage` | object | On `turn_completed`: token counts + `modelUsage` |

`sessionUpdate` values the usage extractor handles:

| `sessionUpdate` | Treatment |
|---|---|
| `user_message_chunk` | One prompt event |
| `tool_call` | One tool event, named by `update.title` |
| `turn_completed` | Assistant event + a usage record (if `update.usage` exists) |
| anything else | Ignored (no event, no record) |

`update.usage` token fields: `inputTokens` (already excludes cache),
`outputTokens`, `cachedReadTokens` (cache reads), `cacheCreationTokens` (cache
writes), and `modelUsage` — a map of model name → `{ totalTokens, ... }`.

### Illustrative records

Synthetic, for shape only. Real events carry more fields (e.g. message content
nested under `params.update`); the SDK reads only those shown.

`summary.json`:

```json
{"info":{"id":"b1e2c3d4-uuid","cwd":"/Users/alice/code/app"},"session_summary":"Refactor the parser","created_at":1762939800000,"updated_at":1762939900000}
```

`updates.jsonl`:

```json
{"timestamp":1762939800,"params":{"update":{"sessionUpdate":"user_message_chunk"}}}
{"timestamp":1762939801,"params":{"update":{"sessionUpdate":"tool_call","title":"read_file"}}}
{"timestamp":1762939802,"params":{"_meta":{"agentTimestampMs":1762939802000},"update":{"sessionUpdate":"turn_completed","usage":{"inputTokens":1200,"outputTokens":48,"cachedReadTokens":8000,"cacheCreationTokens":0,"modelUsage":{"grok-4.6-build":{"totalTokens":9248}}}}}}
```

## Session scanning → `SessionInfo`

`ScanGrok` (`grok.go:15`) maps `ScanGrokSessionDir` over every session directory
with `MapLimit` (`shared.go:411`) at `FileConcurrency` (16); results keep input
order and any error fails the whole batch. `ScanGrokSessionDir` (`grok.go:45`):

1. Read `summary.json`; a read error or a JSON parse error → return nil (the
   directory is skipped, not fatal).
2. `id` = `info.id`, else `basename(dir)` (the uuid directory name).
3. `workspace` = `info.cwd`, else `url.PathUnescape(basename(parentDir))`. **A
   decode error is returned as an error**, which fails the batch and wipes out
   the whole Grok agent (matching the TS `decodeURIComponent` behavior).
4. `os.Stat(dir)`; a stat error is also returned as an error (fails the batch).
5. Build the record:

| `SessionInfo` field | Source |
|---|---|
| `ID` | `info.id` ?? session dir name |
| `Workspace` | `info.cwd` ?? URL-decoded project dir name |
| `Title` | `ToTitle(summary.session_summary)` |
| `CreatedAt` | `AsEpochMs(summary.created_at)` ?? `FloorMtimeMs(dirStat)` |
| `UpdatedAt` | `AsEpochMs(summary.updated_at)` ?? `FloorMtimeMs(dirStat)` |
| `SizeBytes` | `FileSize(<dir>/updates.jsonl)` (0 if missing) |
| `FilePath` | `<dir>/updates.jsonl` (always set) |

Both timestamps fall back to the **session directory's** mtime
(`FloorMtimeMs`, `shared.go:366`), not the file's. `AsEpochMs` (`shared.go:129`)
accepts a finite number (epoch ms) or an RFC3339 string.

### Title normalization (`ToTitle`, `shared.go:211`)

Skip leading blank lines; unwrap the Codex `## My request:` envelope and the
`PLEASE IMPLEMENT THIS PLAN:` preamble; treat a first line starting with `<` or
`# ` as injected (→ null); truncate to 120 UTF-16 code units (117 + `...`).
Truncation counts UTF-16 units, so an emoji counts as 2.

## Transcript parsing → `Entry[]`

`parseGenericTranscript` (`transcript_generic.go:5`) resolves `filePath` via
`transcriptFile` (`transcript.go:161`; rejects nil/empty or a `#`-suffixed
SQLite path), streams `updates.jsonl` with `scanJSONLines` (per-line cap
32 MiB), and calls `genericEntries` (`transcript.go:455`) per line.

`genericEntries` is the loose-shape loader shared with `workbuddy`. It:

1. computes `ts = mapTimestamp(obj, false)` — top-level `timestamp` / `time` /
   `createdAt` / `created_at`, recursing into `message` / `payload` / `data`;
2. finds a `role` from `obj.role` / `message.role` / `payload.role`, else from
   `obj.type` (`user`/`user_message`/…, `assistant`/`agent`/`agent_message`/…,
   `system`);
3. finds `content` from `message.content` / `obj.content` / `payload.content`,
   else a string `obj.message` / `obj.text`;
4. if both role and content are present, calls `entriesFromContent`
   (`transcript.go:238`), which renders text blocks, emits tool cards for
   `tool_use`/`tool_call`/`function_call` blocks (`toolCall`, `transcript.go:344`),
   cleans user text (`cleanUserText`, `transcript.go:217`), and drops
   `thinking`/`reasoning`/`tool_result`/`image`.

**Grok outcome: the generic loader emits no entries for the real store.** As the
usage reader shows, Grok events nest everything under `params.update` (the
discriminator is `params.update.sessionUpdate`, not a top-level `type`), and the
line exposes none of the top-level `role` / `message` / `content` / `payload` /
`text` fields `genericEntries` looks at. With no recognizable role and no
content, every line returns nil. The rich event data is consumed only by the
usage extractor, which has a dedicated reader; `LoadTranscript("grok", …)`
returns an empty transcript.

There is also a latent unit mismatch: `mapTimestamp` would read the line-top
`timestamp` (epoch **seconds**) as milliseconds, since `AsEpochMs` passes
numbers through unchanged. It is moot here because no entries are produced, but
it is why the usage path uses its own `grokEventTimestamp` instead.

## Usage extraction → `UsageRecord[]`

`scanGrokUsage` (`usage_grok.go:17`) maps `scanGrokSessionUsage` over
`ListGrokSessionDirs` at `FileConcurrency` (16). `scanGrokSessionUsage`
(`usage_grok.go:43`) first calls `ScanGrokSessionDir`; a nil session (no
`summary.json`) is skipped, while an error fails the batch. It then reads
`updates.jsonl` in full (`ReadAllLines`, read failure → empty) and walks each
line with `WalkLines`:

1. `params = obj.params`, `update = params.update`; no `update` → skip.
2. **Timestamp gate.** `grokEventTimestamp(obj, params)` (`usage_grok.go:101`):
   prefer `AsEpochMs(params._meta.agentTimestampMs)` (ms number or RFC3339
   string); else `floor(obj.timestamp × 1000)` when `obj.timestamp` is a finite
   number (seconds); else `0`. A `0` timestamp skips the line.
3. **Dispatch on `update.sessionUpdate`:**
   - `user_message_chunk` → `tally.AddEvent(ts, TallyPrompt, nil)`; stop.
   - `tool_call` → `tally.AddEvent(ts, TallyTool, update.title)`; stop. No skill
     name is tracked, so `SkillCallsByName` stays null.
   - `turn_completed` → continue to emit a record.
   - anything else → stop.
4. Feed an assistant event. `usage = update.usage`; if absent, stop (the
   assistant event still accrues to the next record).
5. **Emit a record** with `SessionID = session.ID`, `AgentID = "grok"`,
   `Model = pickGrokModel(usage)`, the resolved `timestamp`, and
   `InputTokens = AsTokenCount(usage.inputTokens)`,
   `OutputTokens = AsTokenCount(usage.outputTokens)`,
   `CacheReadTokens = AsTokenCount(usage.cachedReadTokens)`,
   `CacheWriteTokens = AsTokenCount(usage.cacheCreationTokens)`.
   `AsTokenCount` floors positive finite numbers, else 0; input is taken as-is
   (the store already excludes cache from `inputTokens`).
6. `record.ApplyInterval(tally.TakeUntil(ts))`; after the file,
   `lastRecord.MergeInterval(tally.Flush())` folds trailing events into the last
   record.

### Model selection (`pickGrokModel`, `usage_grok.go:113`)

`usage.modelUsage` is a map of model name → `{ totalTokens }`. Pick the entry
with the **largest `totalTokens`**; on ties, the **lexicographically smallest
name**. This deterministic tie-break is what keeps Go's unordered map iteration
aligned with the TS version. An empty/absent `modelUsage` yields a null model.

### Active time & the tally (`SessionTally`, `tally.go:36`)

Events must arrive in ascending time order; out-of-order events are dropped.
`ActiveMs` sums gaps between consecutive events, each capped at `ActiveGapCapMs`
(120 000 ms = 2 min). `TakeUntil(ts)` drains events with `ts <= ts`; `Flush()`
drains the rest. The interval carries `UserPrompts`, `ToolCalls`,
`ToolCallsByName` and `ActiveMs` onto the record.

## Aggregation & cost

- `ScanAllUsage` (`usage.go:45`) concatenates records in module order and merges
  sessions into an ordered map keyed by bare `sessionId` (last-write-wins). Each
  session's `tokenUsage` is the 5-tuple
  `[input, output, cacheRead, cacheWrite, 3]` (`3` = status flag; input excludes
  cache traffic). `filePath` is tildified against `home`. A module error is
  fault-isolated: Grok then contributes no sessions or records.
- Grok is token-billed: `EstimateTokenCost` (`pricing.go:189`) prices a record
  from model rates (`NormalizeModelId` → `LookupModelPrice`); `credits` is
  omitted. The build edition `grok-4.6-build` is aliased to `grok-4.6`
  (`pricing.go:39`). A model missing from the table makes the record unpriced
  (cost 0, all four token counts reported as unpriced).

## Quirks & edge cases

- **Empty transcript via the generic loader.** Grok's `params.update`-nested
  events match none of `genericEntries`' role/content paths, so
  `LoadTranscript("grok", …)` yields no entries even though usage works.
- **Two timestamp units.** The line-top `timestamp` is seconds;
  `params._meta.agentTimestampMs` is milliseconds. Usage prefers the latter and
  scales the former by 1000; the generic transcript loader would misread the
  seconds value as ms.
- **Prompts are counted per chunk.** Every `user_message_chunk` event is one
  prompt, so a user turn streamed as multiple chunks counts more than once.
- **Only `turn_completed` emits a record.** `tool_call` and `user_message_chunk`
  feed the tally but produce no record; other event kinds (e.g. agent message
  chunks) are ignored entirely.
- **A bad directory wipes the agent.** A `url.PathUnescape` failure on the
  project dir name, or an `os.Stat` failure on the session dir, returns an error
  that fails the whole batch (scan and usage alike).
- **No `summary.json` → not a session.** Unreadable or unparsable summaries are
  skipped silently; the directory contributes nothing.
- **Timestamps fall back to the directory mtime**, not the file mtime, and both
  `createdAt` and `updatedAt` share that fallback.
- **Model tie-break is lexicographic**, chosen to make unordered map iteration
  deterministic.
- **Tool name comes from `update.title`**, not a `name` field.

## Source map

| Topic | Location |
|---|---|
| Registry entry | `registry.go:31` |
| Session scan | `grok.go` (`ScanGrok`, `ListGrokSessionDirs`, `ScanGrokSessionDir`) |
| Transcript loader | `transcript_generic.go` (`parseGenericTranscript`) |
| Generic record → entries | `transcript.go` (`genericEntries`, `messageRole`, `messageContent`, `mapTimestamp`, `entriesFromContent`, `toolCall`, `cleanUserText`) |
| Usage extraction | `usage_grok.go` (`scanGrokUsage`, `scanGrokSessionUsage`, `grokEventTimestamp`, `pickGrokModel`) |
| Activity tally | `tally.go` (`SessionTally`) |
| Aggregation | `usage.go` (`ScanAllUsage`, `ApplyInterval`, `MergeInterval`) |
| Pricing | `pricing.go` (`EstimateTokenCost`, `NormalizeModelId`, `LookupModelPrice`, `modelAliases`) |
| Shared helpers | `shared.go` (`ToTitle`, `AsEpochMs`, `AsNumber`, `AsTokenCount`, `FileSize`, `FloorMtimeMs`, `ListDirs`, `MapLimit`, `ReadAllLines`, `WalkLines`) |
| Output contract | `contract.go` (`SessionInfo`, `UsageRecord`, `Entry`, `ToolCall`) |