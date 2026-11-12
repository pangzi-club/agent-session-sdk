# Gemini CLI — session store format & parsing

How the `gemini` agent stores sessions on disk, and how this SDK turns that
store into `SessionInfo` and `Entry[]`. Gemini CLI keeps **no local usage data**,
so there is no `UsageRecord[]` extraction and no cost accounting.

This doc follows the shared section order: at a glance → on-disk layout → record
format → session scanning → transcript parsing → aggregation & cost → quirks →
source map. The "Usage extraction" section is omitted because the agent has no
usage module.

## At a glance

| Property | Value |
|---|---|
| Agent id | `gemini` |
| Display name | Gemini CLI |
| Store directory | `~/.gemini` |
| `canDeleteSessions` | `false` (the library only reports this; deletion is the consumer's job) |
| Session scanner | `ScanGemini` (`gemini.go`) |
| Transcript loader | `parseGeminiTranscript` (`transcript_gemini.go`) |
| Usage module | none — no local usage data |
| Billing | n/a (no usage records; no token tuple, no cost, no credits) |
| Store format | one `logs.json` JSON array per project hash, holding many sessions |

## On-disk layout

```
~/.gemini/
  tmp/
    <sha256(cwd)>/            # one directory per project, named by a path hash
      logs.json               # JSON array of message records for ALL sessions here
    <sha256(cwd)>/
      logs.json
```

- **One directory per project, named by a hash.** The directory name is the
  lowercase hex SHA-256 of the project's absolute path (64 characters), e.g.
  `sha256("/Users/alice/code/app")`. The path cannot be read back out of the
  name; the SDK recovers it by walking home and hashing candidate directories
  (see "Workspace reverse look-up" below).
- **One `logs.json` per project, not per session.** A single file is a JSON
  array holding records for many `sessionId`s; each record carries its own
  `sessionId`. This is the opposite of Claude/Cursor, where one file is one
  session.
- The scanner only considers `tmp/` subdirectories whose name matches
  `^[0-9a-f]{64}$` (`geminiHashRe`, `gemini.go:14`). Uppercase hex, wrong length,
  or non-directory entries are ignored.

## Record format (`logs.json`)

The file is a JSON **array** (not JSONL). Each element is one object. The fields
the SDK reads:

| Field | Type | Meaning |
|---|---|---|
| `sessionId` | string | Session the record belongs to; must be a string for the scanner to keep it |
| `message` | string | The message text (a user prompt, or a model reply when present) |
| `type` | string | `user` (default); `gemini` / `assistant` / `model` mark a model turn |
| `timestamp` | string (RFC3339) \| number | Event time; `AsEpochMs` accepts a finite number or an RFC3339 string |

`logs.json` typically records only user messages (the loader's own comment says
so), but the parser also recognizes model turns if they appear. `message` is
read as a **string**; a record whose `message` is an array or object is ignored
by the transcript loader.

### Illustrative file

Synthetic, for shape only:

```json
[
  {"sessionId":"gem-1","type":"user","message":"Create the page","timestamp":"2025-08-18T04:47:40.519Z"},
  {"sessionId":"gem-1","type":"user","message":"Continue","timestamp":"2025-08-18T04:49:02.711Z"},
  {"sessionId":"gem-2","type":"user","message":"Other session","timestamp":"2025-08-18T04:50:00.000Z"}
]
```

## Session scanning → `SessionInfo`

`ScanGemini` (`gemini.go:45`):

1. List `tmp/`, keep directories matching `geminiHashRe`. None → return empty.
2. Reverse-look-up workspaces (`resolveGeminiWorkspaces`). If **no** hash
   resolves, return empty immediately — the `logs.json` files are never read.
3. For each hash directory with a resolved workspace: `stat` then read
   `logs.json`, `json.Unmarshal` it, and require a JSON array. Any failure
   (stat / read / parse / not-an-array) skips that directory, not the scan.
4. Walk the records, aggregating per `sessionId` into an insertion-ordered map
   (`geminiAgg`):
   - `sessionId` must be a string (`AsString`); a non-string value skips the
     record.
   - **First record for a session** seeds the aggregate: `workspace` (the
     resolved path), `title = ToTitle(message)`, `createdAt = AsEpochMs(timestamp)`,
     `updatedAt = timestamp ?? 0`, `sizeBytes` = size of the **whole** `logs.json`,
     `filePath` = `logs.json`.
   - **Later records** only raise `updatedAt` to the maximum timestamp seen.
     `title`, `createdAt`, `workspace`, `sizeBytes` and `filePath` are never
     revisited.
5. Emit in insertion order: `createdAt` = first-record timestamp, falling back
   to `updatedAt` when the first record had no timestamp; `updatedAt` = max
   timestamp; `id` = `sessionId`; `filePath` = the shared `logs.json`.

### Workspace reverse look-up (`resolveGeminiWorkspaces` / `geminiWalk`, `gemini.go:149`)

The store names projects by hash, so the SDK must find which directory hashes to
each name:

- `home = filepath.Dir(agentDir)` — the parent of `~/.gemini`, i.e. `~`.
- Walk home recursively. For each subdirectory compute `Sha256Hex(full)`
  (`shared.go:174`): the lowercase hex SHA-256 of the **absolute path string**
  (e.g. `/Users/alice/code/app`). If it equals a `tmp/` directory name, record
  `hash → path` and remove that target.
- **Pruning** (`geminiSkipDirs`, `gemini.go:19`): skip dot-prefixed directories
  and a fixed list — `Library`, `Applications`, `node_modules`, `Music`,
  `Movies`, `Pictures`, `Public`, `Desktop`, `Documents`, `Downloads`. The last
  three are macOS TCC-protected; walking them would trigger a system permission
  prompt, so projects there are deliberately left unresolved (their sessions
  are skipped).
- **Depth bound**: `geminiWalk` starts at home (depth 0) and returns once
  `depth > geminiMaxDepth` (6, `gemini.go:17`), so it hashes directories down to
  seven levels below home. It also exits early once every target hash is found.
- **SHA-256, not MD5.** The `shared.go` comment pairs `Md5Hex` with `Sha256Hex`,
  but `Md5Hex` (32-char) is kimi's scheme. Gemini's 64-char names are SHA-256
  only.

### Title normalization (`ToTitle`, `shared.go:211`)

The title comes from the **first** record's `message`. `ToTitle` skips leading
blank lines, unwraps the Codex `## My request:` envelope and the
`PLEASE IMPLEMENT THIS PLAN:` preamble, treats a first line starting with `<` or
`# ` as injected (returns nil), and truncates to 120 UTF-16 code units
(117 + `...`). Unlike Claude's tiered fallback, gemini does **not** look at later
records when the first yields no usable title — the title stays nil.

## Transcript parsing → `Entry[]`

`parseGeminiTranscript` (`transcript_gemini.go:7`):

- Locate the file via `SessionInfo.FilePath` (`transcriptFile` rejects a nil /
  empty path or a `#` SQLite suffix).
- Read the **whole** file and `json.Unmarshal` it (`readJSONFileAny`). A parse
  error is returned to the caller, not swallowed.
- Iterate the array (`GetSlice`); a non-array yields no entries. Per record:
  - **Session filter**: `id = firstString(entry, "sessionId")`; skip when
    `id != "" && id != info.ID`. A record with **no** `sessionId` is kept for
    every session parsed from that file.
  - `message = firstString(entry, "message")`; skip when empty. Because
    `firstString` only returns strings, array/object messages are dropped here.
  - **Role**: default `user`; `type` of `gemini` / `assistant` / `model`
    (case-insensitive) → `assistant`.
  - `entriesFromContent(role, message, mapTimestamp(entry, false))`.
- `mapTimestamp` (`transcript.go:180`) reads `timestamp` / `time` / `createdAt` /
  `created_at` (finite number or RFC3339 string), recursing into `message` /
  `payload` / `data`; `0` when none is found.

### From content to entries (`entriesFromContent`, `transcript.go:238`)

Since `message` is a plain string, this produces a single text entry:

- **user** → `cleanUserText` (`transcript.go:217`): trim; strip a leading
  `<system-reminder>`; strip `<timestamp>…</timestamp>`; unwrap
  `<user_query>…</user_query>`; drop the result if empty or starting with `<`;
  drop known scaffolding prefixes (`base directory for this skill:`,
  `# agents.md instructions`, `please implement this plan:`).
- **assistant** → `cleanAssistantText`: trim only.

The shared block/tool handling in `entriesFromContent` (text arrays, `tool_use`
cards, dropped `thinking`/`tool_result`/`image`) is never exercised for gemini,
because the loader already requires `message` to be a string. No tool cards
appear in a gemini transcript.

## Aggregation & cost

- Gemini has **no usage module**: it is absent from `usageModules`
  (`usage.go:25`), so `ScanAllUsage` produces no `UsageRecord`s and no
  `UsageSession` for it.
- `SessionInfo.TokenUsage` stays nil (omitted from JSON). There is no token
  5-tuple, no `EstimateTokenCost` pricing and no `credits`.
- The session list still comes from `ScanSessions` (`registry.go`), which
  reports `canDeleteSessions = false` for gemini.

## Quirks & edge cases

- **Many sessions, one file.** Every session in a `logs.json` reports the same
  `sizeBytes` (the whole file) and the same `filePath`; the transcript loader
  separates them by `sessionId`.
- **Workspace is recovered by hashing, not decoding.** If the home walk cannot
  find a matching directory — pruned, too deep, or outside home — that project's
  sessions are skipped entirely. When nothing resolves, no file is even read.
- **TCC-pruned directories.** Projects under `Desktop` / `Documents` /
  `Downloads` never resolve on macOS, by design, to avoid permission prompts.
- **Title from the first record only.** `ToTitle` may return nil (injected `<` /
  `# ` text); there is no fallback to later records.
- **`createdAt` fallback.** A first record without a timestamp makes `createdAt`
  equal `updatedAt` (the max timestamp, or `0`).
- **Missing `sessionId` is shared.** A record with no `sessionId` appears in
  every session parsed from that file.
- **String-only messages.** Non-string `message` values are ignored by the
  loader; gemini transcripts are text-only.
- **SHA-256, not MD5.** Directory names are 64-char lowercase hex.

## Source map

| Topic | Location |
|---|---|
| Registry entry | `registry.go:24` |
| Session scan | `gemini.go` (`ScanGemini`, `geminiAgg`) |
| Workspace reverse look-up | `gemini.go` (`resolveGeminiWorkspaces`, `geminiWalk`, `geminiHashRe`, `geminiSkipDirs`, `geminiMaxDepth`) |
| Path hashing | `shared.go` (`Sha256Hex`; `Md5Hex` is kimi's, not gemini's) |
| Transcript loader | `transcript_gemini.go` (`parseGeminiTranscript`) |
| Content → entries, cleaning | `transcript.go` (`entriesFromContent`, `cleanUserText`, `mapTimestamp`) |
| Title normalization | `shared.go` (`ToTitle`) |
| Usage aggregation (gemini absent) | `usage.go` (`usageModules`, `ScanAllUsage`) |
| Output contract | `contract.go` (`SessionInfo`, `Entry`, `ToolCall`) |