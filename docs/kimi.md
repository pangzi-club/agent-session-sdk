# Kimi — session store format & parsing

How the `kimi` agent stores sessions on disk, and how this SDK turns that store
into `SessionInfo` and `Entry[]`. Kimi has **no local usage data**: it is absent
from `usageModules` (`usage.go:25`), so it never produces `UsageRecord[]`.

This doc follows the shared section order: at a glance → on-disk layout → record
format → session scanning → transcript parsing → aggregation & cost → quirks →
source map. The usage-extraction section is omitted (no usage module). Shared
helpers (`entriesFromContent`, `toolCall`, `ToTitle`) are documented in
[claude.md](claude.md); only kimi-specific behavior is re-derived here.

## At a glance

| Property | Value |
|---|---|
| Agent id | `kimi` |
| Display name | Kimi |
| Store directory | `~/.kimi` |
| `canDeleteSessions` | `false` (registry sets no `canDelete`; the library only reports it) |
| Session scanner | `ScanKimi` (`kimi.go:15`) |
| Transcript loader | `parseKimiTranscript` (`transcript_kimi.go:10`) |
| Usage module | none — no local usage data |
| Billing | n/a — no usage records are emitted, so nothing is priced |
| Store format | one directory per session holding `wire.jsonl` / `context.jsonl` + `metadata.json`; a top-level `kimi.json` registers workspaces |

## On-disk layout

```
~/.kimi/
  kimi.json                               # work_dirs registry (md5 reverse-lookup source)
  sessions/
    5d41402abc4b2a76b9719d911017c592/     # md5(cwd), one directory per workspace
      7f3c1e2a-9b40-4c1d-8e2f-1a2b3c4d5e6f/   # one directory per session (uuid)
        wire.jsonl                        # turn-event stream (preferred transcript)
        context.jsonl                     # message transcript (fallback)
        metadata.json                     # official title + wire_mtime
      ...
    ...
```

- **One directory per workspace, named `md5(cwd)`.** The name is the lowercase
  MD5 hex of the workspace's absolute path (`Md5Hex`, `shared.go:169`). The SDK
  does **not** decode the hash back to a path (unlike Claude's `DecodePathDirName`);
  it reverse-looks-up the name in a table built from `kimi.json`.
- **One directory per session.** The directory base name (a uuid) is the session
  id. A session may have a `wire.jsonl`, a `context.jsonl`, or both; if neither
  exists the session is skipped.
- **`kimi.json` is mandatory for scanning.** It supplies the `work_dirs` list
  used to resolve workspace paths. A project directory whose name is not a known
  hash is skipped entirely; a missing or unparseable `kimi.json` yields an empty
  map, so **every** session is skipped.
- Dot-prefixed directories are skipped at both levels (`ListDirs`, `shared.go:371`).

> Note: `Sha256Hex` (`shared.go:174`) is the *gemini* reverse-lookup hash, not
> kimi's. Kimi uses `Md5Hex` only.

## Record format

Kimi reads four files. All JSONL lines are parsed independently; blank lines and
lines that fail to parse are silently skipped.

### `kimi.json` (workspace registry)

| Field | Type | Meaning |
|---|---|---|
| `work_dirs` | array | workspace entries |
| `work_dirs[].path` | string | absolute workspace path; `Md5Hex(path)` becomes the lookup key |

`loadKimiWorkspaces` (`kimi.go:50`) builds `md5hex(path) → path`. Only entries
with a string `path` are kept.

### `metadata.json` (optional, per session)

| Field | Type | Meaning |
|---|---|---|
| `title` | string | official session title, normalized by `ToTitle` |
| `wire_mtime` | number | wire update time, **epoch seconds** → `SecToMs` |

### `wire.jsonl` (turn-event stream)

Each line is one JSON object. The SDK reads:

| Field | Type | Meaning |
|---|---|---|
| `timestamp` | number | event time, **epoch seconds** (fractional allowed) → `SecToMs` |
| `message` | object | `{ type, payload }`; a line without it is skipped |
| `message.type` | string | `TurnBegin`, `TextPart`, `TextChunk`, `AssistantText`, `Text`, … |
| `message.payload` | object | event payload; a line without it is skipped |
| `message.payload.user_input` | string \| array | user content on a `TurnBegin` event |
| `message.payload.role` | string | role for non-`TurnBegin` events |
| `message.payload.content` | any | content (preferred) |
| `message.payload.text` | any | content fallback when `content` is absent |

A top-level handshake line such as `{"type":"metadata","protocol_version":"1.2"}`
has no `message` object and is skipped.

### `context.jsonl` (message transcript, fallback)

Loosely shaped message records (`{ role, content, timestamp? }`) parsed by the
shared `genericEntries` (`transcript.go:455`). Used when there is no wire file, or
when the wire file yields no entries.

### Illustrative wire line

Synthetic, for shape only:

```json
{"timestamp":1770264596.95,"message":{"type":"TurnBegin","payload":{"user_input":[{"type":"text","text":"Refactor the login module"}]}}}
```

`1770264596.95` seconds → `1770264596950` ms via `SecToMs` (which rounds).

### Content blocks

`user_input` / `content` values flow through the shared `entriesFromContent`
(`transcript.go:238`): a string becomes one text entry; an array of blocks joins
consecutive text blocks with `\n`, emits a tool card per `tool_use`, and drops
`thinking` / `reasoning` / `tool_result` / `image`. See [claude.md](claude.md) for
the full block table and tool-card rules.

## Session scanning → `SessionInfo`

`ScanKimi` (`kimi.go:15`) loads the workspace map, lists `sessions/`, and scans
each project directory in parallel (`MapLimit`, `FileConcurrency` = 16). Per
project directory:

1. `workspace = workspaces[base(projectDir)]`. If the hash is unknown, the whole
   directory is skipped (`return nil, nil`).
2. List its session subdirectories and scan each via `scanKimiSession`
   (`kimi.go:70`), again bounded by `FileConcurrency`.

`scanKimiSession` derives each field:

| Field | Derivation |
|---|---|
| `id` | base name of the session directory (the uuid) |
| `workspace` | the path resolved from the md5 lookup |
| `title` | tiered: `metadata.title` → wire `TurnBegin.user_input` → context first `user` content (each via `ToTitle`) |
| `createdAt` | first wire line's `SecToMs(timestamp)` ?? `baseMtime` |
| `updatedAt` | `metadata.wire_mtime` (`SecToMs`) ?? `baseMtime` |
| `sizeBytes` | `wire.size + context.size` (whichever exist) |
| `filePath` | `wire.jsonl` if present, else `context.jsonl` |

Details:

- **Existence gate.** `stat` both `wire.jsonl` and `context.jsonl`; if neither
  exists, return nil (skip the session).
- **`baseMtime`** is `FloorMtimeMs` of the wire file when present, otherwise the
  context file (`kimi.go:134`). It is the fallback for both timestamps.
- **Head reads.** Title/creation extraction uses `ReadHeadLines(file, 64*1024)`
  (`shared.go:435`), which keeps the first 64 KiB, then `ParseLines`
  (`shared.go:447`) takes the first line satisfying a predicate.
  - `createdAt`: first line whose `timestamp` is a positive number (`SecToMs`).
  - wire title (only if `metadata.title` gave nothing): first `message.type ==
    "TurnBegin"` line, `payload.user_input` → `ContentToText` → `ToTitle`.
  - context title (only if still nil and a context file exists): first
    `role == "user"` line, `content` → `ContentToText` → `ToTitle`.
- **`ContentToText`** (`shared.go:310`) joins an array of `{text}` blocks with a
  **space** (not `\n`); an empty result is nil.
- **`ToTitle`** (`shared.go:211`) drops injected first lines (starting `<` or
  `# `) and truncates to 120 UTF-16 units (117 + `...`), so a blank or injected
  metadata title falls through to the wire/context candidates.

## Transcript parsing → `Entry[]`

`parseKimiTranscript` (`transcript_kimi.go:10`) resolves the file via
`transcriptFile` (`transcript.go:161`), which rejects a nil/empty `FilePath` or
one containing `#` (a SQLite reference). Then:

- If the file name ends with `context.jsonl`, go straight to `parseKimiContext`.
- Otherwise stream `wire.jsonl` with `scanJSONLines` (per-line cap
  `ScanMaxTranscriptLine`, 32 MiB). Per line:
  - `message = obj.message`; skip if absent.
  - `ts = mapTimestamp(obj, true)` — **seconds mode**: `SecToMs` over
    `timestamp` / `time` / `createdAt` / `created_at`, recursing into `message` /
    `payload` / `data` (`transcript.go:180`). `0` when none is found.
  - `payload = message.payload`; skip if absent.
  - **`TurnBegin`** → `entriesFromContent(user, payload.user_input, ts)`. The role
    is always `user`, regardless of any `payload.role`.
  - **otherwise** → `role = roleFromString(payload.role)`; if empty and
    `message.type` is one of `TextPart` / `TextChunk` / `AssistantText` / `Text`,
    role becomes `assistant`; if still empty, skip. Content is `payload.content`,
    falling back to `payload.text`; emit `entriesFromContent(role, content, ts)`.
- **Wire→context fallback.** If the wire scan errored **or** produced at least
  one entry, return immediately. Only when it produced **zero entries with no
  error** does the loader try the sibling `context.jsonl` via `parseKimiContext`,
  returning its entries on success (else the empty wire result).

`parseKimiContext` (`transcript_kimi.go:61`) streams `context.jsonl` and runs
`genericEntries` per line. Note the **timestamp asymmetry**: `genericEntries`
calls `mapTimestamp(obj, false)` → `AsEpochMs` (a number passes through as
milliseconds, or an RFC3339 string is parsed), *not* `SecToMs`. A context file
storing epoch seconds would therefore be misread as milliseconds.

Text cleaning (`cleanUserText` / `cleanAssistantText`) and tool cards (`toolCall`)
are the shared helpers; see [claude.md](claude.md).

## Aggregation & cost

Kimi is **not** a usage module: it does not appear in `usageModules`
(`usage.go:25`), so `ScanAllUsage` never sees it. It contributes only to the
session list (`ScanSessions`), never `UsageRecord[]`, `UsageSession[]`,
`tokenUsage`, or cost. There is no local usage data in `wire.jsonl` /
`context.jsonl` that the SDK reads. (The price table does list `kimi-*` models,
but with no usage records there is nothing to price.)

## Quirks & edge cases

- **MD5 reverse-lookup, not path decoding.** Workspace paths come from
  `kimi.json`'s `work_dirs`, keyed by `Md5Hex(path)`. A missing/unparseable
  `kimi.json` means an empty map and **zero sessions**; unknown-hash directories
  are skipped. `Sha256Hex` is gemini's, not kimi's.
- **Timestamps are epoch seconds.** The scanner (`wire_mtime`, wire `timestamp`)
  and the wire transcript (`mapTimestamp(..., true)`) use `SecToMs`, which rounds
  to whole milliseconds. The context fallback uses `AsEpochMs` (ms/ISO) instead —
  an inconsistency to remember when a session has only a context file.
- **Title tiering.** `metadata.title` → wire `TurnBegin.user_input` → context
  first `user` content. `ContentToText` joins blocks with a space; `ToTitle`
  discards injected (`<` / `# `) first lines, so a title can fall through.
- **Wire preferred, context is a true fallback.** The transcript only reads
  `context.jsonl` when the wire file is the chosen `FilePath`, or when the wire
  scan yields zero entries without error. A wire scan error short-circuits the
  fallback.
- **`TurnBegin` is always a user turn**, independent of `payload.role`.
- **Head reads are bounded to 64 KiB** but `ReadHeadLines` loads the file then
  truncates (not a streaming scan). A wire/context file that exists but fails to
  read returns an error that propagates through `MapLimit`, failing the whole
  `ScanKimi` call — `ScanSessions` then reports no kimi sessions
  (`shared.go:434` comment).
- **`sizeBytes` sums wire + context**, and `filePath` points at wire when present
  (so the transcript cache watches the wire file's size/mtime).
- **No usage data**, no cost, no `tokenUsage`.

## Source map

| Topic | Location |
|---|---|
| Registry entry | `registry.go:34` |
| Session scan | `kimi.go` (`ScanKimi`, `loadKimiWorkspaces`, `scanKimiSession`) |
| Transcript loader | `transcript_kimi.go` (`parseKimiTranscript`, `parseKimiContext`) |
| Generic context entries | `transcript.go` (`genericEntries`, `messageRole`, `messageContent`) |
| Content → entries, tool cards, cleaning | `transcript.go` (`entriesFromContent`, `toolCall`, `cleanUserText`) |
| Timestamp mapping | `transcript.go` (`mapTimestamp`); `shared.go` (`SecToMs`, `AsEpochMs`) |
| Hashing / title / head reads | `shared.go` (`Md5Hex`, `ToTitle`, `ContentToText`, `ReadHeadLines`, `ParseLines`, `FloorMtimeMs`, `ListDirs`, `MapLimit`) |
| Usage aggregation (kimi absent) | `usage.go` (`usageModules`, `ScanAllUsage`) |
| Output contract | `contract.go` (`SessionInfo`, `Entry`, `ToolCall`) |