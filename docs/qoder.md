# Qoder — session store format & parsing

How the `qoder` agent stores sessions on disk, and how this SDK turns that
store into `SessionInfo`, `Entry[]` and `UsageRecord[]`.

Qoder writes Claude Code's JSONL transcript shape, so it shares the Claude
transcript loader. It differs from Claude in two important ways: tokens are
**heuristically reconstructed** (the on-disk token fields are always `0`), and
billing is **credit-based**, not model-rate-based. The Chinese edition
(`qoder-cn`) shares the same scanner and format; see [qoder-cn.md](qoder-cn.md)
for the few things that differ.

This doc follows the shared template (see [README.md](README.md)): at a glance →
on-disk layout → record format → session scanning → transcript parsing → usage
extraction → aggregation & cost → quirks → source map.

## At a glance

| Property | Value |
|---|---|
| Agent id | `qoder` |
| Display name | Qoder |
| Store directory | `~/.qoder` |
| `canDeleteSessions` | `false` (the library only reports this; deletion is the consumer's job) |
| Session scanner | `ScanQoder` (`qoder.go:20`), shared with `qoder-cn` |
| Transcript loader | `parseClaudeTranscript` (`transcript_claude.go:5`), shared with `claude` / `qoder-cn` |
| Usage module | `scanQoderUsage` (`usage_qoder.go:51`) → `scanQoderUsageAs(agentDir, "qoder")` |
| Billing | Credit-metered: records carry `credits`, priced in credits (not model rates). Tokens are heuristic reconstructions. |
| Store format | JSONL transcript per session; no index file |

## On-disk layout

```
~/.qoder/
  projects/
    -Users-alice-code-app/            # one directory per project (encoded cwd)
      <sessionUuid>.jsonl             # one transcript per session
      <sessionUuid>/                  # optional per-session directory
        subagents/                    # subagent transcripts — NOT scanned
          <...>.jsonl
      <sessionUuid>.jsonl
    -Users-alice-code-other/
      ...
```

- **One directory per project.** The directory name is the project's absolute
  path with `/` replaced by `-`, the same encoding Claude uses. The SDK reverses
  it with `DecodePathDirName` (`shared.go:334`) only as a fallback; the in-file
  workspace fields are preferred (see scanning below).
- **One JSONL file per session.** The file name minus `.jsonl` is the session id
  (a UUID).
- **No index.** Unlike Claude there is no `sessions-index.json`; every session
  file is streamed.
- **Subagents are skipped naturally.** Subagent transcripts live under
  `<sessionUuid>/subagents/`. `ListFiles(projectDir, ".jsonl", false)`
  (`shared.go:387`) is non-recursive, so that subtree is never enumerated. The
  usage path (`os.ReadDir`, `usage_qoder.go:71`) is likewise top-level only.
- Dot-prefixed project directories are skipped (`ListDirs`); dot-prefixed files
  are skipped (`ListFiles`).

## Record format (JSONL)

Each line is one JSON object. Blank lines and lines that fail to parse are
silently skipped. The fields the SDK reads:

| Field | Type | Meaning |
|---|---|---|
| `type` | string | `user`, `assistant`, `system`, `runtime-config` (others ignored) |
| `timestamp` | string (RFC3339) \| number | Event time. Scan/transcript accept both; usage accepts strings only |
| `sessionId` | string | Session the line belongs to (required for a usage record) |
| `directories` | string[] | Workspace-directories line: first non-empty entry is the workspace |
| `cwd` | string | Project working directory (workspace fallback) |
| `contextWindow` | number | On a `runtime-config` line: the context window for token reconstruction |
| `isSidechain` | bool | `true` for subagent lines interleaved in the main transcript — skipped in usage |
| `isMeta` | bool | `true` for injected meta user lines — skipped in transcript |
| `message` | object | `{ id, role, content, model, usage }` |
| `message.id` | string | Assistant message id; lines sharing one id are merged into a single usage request |
| `message.content` | string \| array | Text, or an array of content blocks |
| `message.model` | string | Model id for the assistant turn |
| `message.usage` | object | Per-request usage (see below) |
| `subtype` | string | On system lines: `compact_boundary` marks a compaction |
| `isCompactSummary` | bool | Alternate compaction marker |

The workspace-directories line carries `directories` but no timestamp; the
following `user` / `assistant` lines carry `cwd` and an ISO timestamp. The SDK
does not gate workspace detection on `type` — `qoderRecordWorkspace`
(`qoder.go:102`) reads `directories`/`cwd` from any line.

### Content blocks

`message.content` is either a plain string or an array of typed blocks, identical
to Claude:

| Block `type` | Shape | Transcript treatment | Usage char counting |
|---|---|---|---|
| `text` | `{ type, text }` | Rendered as text | `text` counted |
| `tool_use` | `{ type, name, input, id }` | Rendered as a tool card | string values of `input` counted |
| `tool_result` | `{ type, tool_use_id, content }` | Dropped | not counted |
| `thinking` / `redacted_thinking` | `{ type, thinking }` | Dropped | `thinking` counted |
| `image` | `{ type, source }` | Dropped | not counted |

A plain-string `content` on a **user** line is an echoed skill/command body, not
a user prompt (see `QoderUserPromptText`); on an **assistant** line it is counted
as reply text for the output-token estimate.

### `message.usage`

Qoder's four token fields are always `0`; the SDK reconstructs tokens from
`context_usage_ratio` and reply characters, and reads credits directly.

| Key | Maps to |
|---|---|
| `context_usage_ratio` | Drives `InputTokens` = `round(ratio × contextWindow)` (ratio clamped to `[0,1]`) |
| `input_tokens` | `InputTokens` **fallback** when no valid ratio (always `0` for Qoder) |
| `output_tokens` | `OutputTokens` **fallback** when no valid ratio (always `0` for Qoder) |
| `cache_read_input_tokens` | `CacheReadTokens` via `AsTokenCount` (always `0` for Qoder) |
| `cache_creation_input_tokens` | `CacheWriteTokens` via `AsTokenCount` (always `0` for Qoder) |
| `credits` | `Credits` (preferred) |
| `original_credits` | `Credits` fallback when `credits` is absent/non-positive |
| `billable` | **Not read.** Free requests (`billable === false`) keep their credits too |

`OutputTokens` is not taken from `usage` at all in the normal path — it is
estimated from the merged reply characters (see usage extraction).

### Illustrative line

Synthetic, for shape only:

```json
{"type":"assistant","timestamp":"2026-11-12T09:30:02.000Z","sessionId":"abc-123","message":{"id":"msg-1","role":"assistant","model":"qmodel_38max","content":[{"type":"text","text":"已完成检查。"}],"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"context_usage_ratio":0.05,"credits":9.216,"original_credits":9.216,"billable":true}}}
```

A preceding `runtime-config` line sets the window:

```json
{"type":"runtime-config","model":"qmodel_38max","contextWindow":200000,"timestamp":1790000000000}
```

## Session scanning → `SessionInfo`

`ScanQoder` (`qoder.go:20`) lists `projects/` (`ListDirs`, skipping dot
directories) and, per project directory, lists `*.jsonl` non-recursively
(`ListFiles`). Files are scanned up to `FileConcurrency` (16) at once via
`MapLimit` → `scanQoderFile` (`qoder.go:39`). There is no index fast path.

Per file:

1. **Stat.** `os.Stat(file)`; if it fails (the file vanished after enumeration)
   the file is skipped, not fatal.
2. **Metadata pass** — one streaming `ScanLines` pass (bounded by `ScanMaxBytes`,
   32 MiB) that remembers the first hit for each of two fields and stops once
   both are found:
   - `createdAt` = `AsEpochMs(timestamp)` from the first line with a parseable
     timestamp (number or RFC3339 string). The workspace-directories line has no
     timestamp, so this usually comes from the first user/assistant line.
   - `workspace` = `qoderRecordWorkspace(obj)`: the first non-empty string in
     `directories`, else `cwd`.
   - If no line ever yields a timestamp (`createdAt == nil`), the file is **not**
     a session and is skipped (empty file, no read permission, etc.).
3. **Workspace fallback.** If no in-file workspace was found, decode the project
   directory name with `DecodePathDirName`. If the workspace is still nil or
   empty, skip the file.
4. **Title pass** — a separate `ScanLines` pass for the first `type == "user"`
   line whose `QoderUserPromptText(message)` yields a usable `ToTitle`. Lines
   that produce no prompt text (tool_result echoes, plain-string skill bodies)
   or an injected/empty title are skipped, so the scan keeps looking for the
   first real prompt.
5. **Emit `SessionInfo`:**
   - `id` = file name minus `.jsonl`.
   - `workspace` = the resolved workspace.
   - `title` = the title pass result (`*string`, may be nil).
   - `createdAt` = the metadata timestamp (ms).
   - `updatedAt` = `FloorMtimeMs(info)` (file mtime, ms).
   - `sizeBytes` = file size.
   - `filePath` = the file path.

### Prompt text & title (`QoderUserPromptText`, `qoder.go:119`)

Shared by the session-title path and the usage-side prompt count:

- `message.content` **must be an array**. A plain string is an echoed
  skill/command body (`Base directory for this skill: …`) and returns nil.
- Walk the blocks; consider only `type == "text"` blocks with a string `text`
  (`tool_result` blocks are excluded).
- Trim leading whitespace, then `stripLeadingSystemReminder` (`qoder.go:150`):
  Qoder prepends `<system-reminder>…</system-reminder>` to the user's prompt
  **inside the same text block**, so it is stripped before the injected-text
  check. An unterminated reminder (no closing tag) is left in place and then
  rejected as injected.
- Accept the text only if it is non-empty and not `IsInjectedText` (does not
  start with `<` or `# `). A pasted-image placeholder like `[Image: …]` still
  counts as user-authored.

`ToTitle` (`shared.go:211`) then unwraps the Codex `## My request:` envelope and
the `PLEASE IMPLEMENT THIS PLAN:` preamble, re-checks for injection, and
truncates to 120 UTF-16 code units (117 + `...`).

## Transcript parsing → `Entry[]`

Qoder uses the **same loader as Claude**: `LoadTranscript` dispatches `qoder`
and `qoder-cn` to `parseClaudeTranscript` (`transcript.go:20`). See
[claude.md](claude.md#transcript-parsing--entry) for the full parser
(`entriesFromContent`, `cleanUserText`, `toolCall`, dropped blocks, tool-card
kinds/titles). Only the Qoder-specific notes are repeated here:

- **Qoder-only record types produce no entries.** The workspace-directories line
  and `runtime-config` lines are neither `user`, `assistant` nor `system`, so the
  loader ignores them.
- **System reminders are stripped in the transcript too.** `cleanUserText`
  (`transcript.go:217`) calls the same `stripLeadingSystemReminder`, so a user
  text block that begins with `<system-reminder>…</system-reminder>` renders as
  the real prompt that follows it.
- **No `message.id` merging.** The transcript processes each JSONL line
  independently. A batched assistant turn (separate thinking / tool_use / text
  lines sharing one `message.id`) yields separate entries per line — thinking is
  dropped, tools and text are emitted. Only the **usage** path merges those lines
  into one request.
- **Compaction marker is the fixed Chinese literal** `上下文已压缩` ("context
  compressed"), emitted for `subtype == "compact_boundary"` or `isCompactSummary`.
- **Subagent files are not loaded.** They are never scanned as sessions, so
  `LoadTranscript` is not called on them. The loader itself does not filter
  `isSidechain` lines interleaved in a main transcript (the usage path does).

## Usage extraction → `UsageRecord[]`

`scanQoderUsage` (`usage_qoder.go:51`) is a thin wrapper over
`scanQoderUsageAs(agentDir, "qoder")` (`usage_qoder.go:59`). It reuses `ScanQoder`
for the session list, then for each project directory reads every top-level
`*.jsonl` in full (`scanQoderProjectUsage` → `scanQoderFileUsage`,
`usage_qoder.go:85`). `ReadAllLines` reads the whole file (no byte cap; failures
yield empty). The `agentID` argument only stamps `UsageRecord.AgentID`; the
parsing is otherwise identical for both editions.

### Per-line walk (`scanQoderFileUsage`)

State: a `SessionTally`, a running `contextWindow` (default `200000`), and the
current assistant `group` (`*qoderAssistantGroup`). In order, per line:

1. **Skip sidechains.** `isSidechain == true` is ignored entirely.
2. **`runtime-config`.** If `contextWindow` is a finite number `> 0`, update the
   running window; then stop (this line needs no timestamp). The window is the
   latest non-empty config seen so far in the file.
3. **Timestamp gate.** `AsTimestamp(timestamp)` parses *strings only* (RFC3339).
   A line with no parseable timestamp is skipped. (Same asymmetry as Claude: the
   transcript path also accepts numbers.)
4. **User line.** Flush the current group; if `QoderUserPromptText(message)` is
   non-nil, feed a prompt event to the tally; then stop. `tool_result` echoes and
   plain-string skill bodies do not count as prompts.
5. **Non-assistant lines** are ignored.
6. **Assistant line.** Feed an assistant event, then group:
   - `rawID = message.id`; `hasID` = non-empty string.
   - Start a **new group** (flushing the old one) when there is no group, the
     line has no id, the current group has no id, or the ids differ. The new
     group snapshots the current `contextWindow`.
   - Last-write-wins onto the group: `sessionID` (from `sessionId`, non-empty),
     `model` (from `message.model`).
   - **Count reply characters** into the group (see below): a plain-string
     `content`, or each block of an array `content`. For each `tool_use` block
     with a name, also feed a tool event (name + `SkillOfToolUse(block)`).
   - If `message.usage` exists, set the group's `usage`, `timestamp` (this line's
     ts) and `contextWindow` (current). The **last** usage-bearing line in the
     group wins.
   - If the line has **no id**, flush the group immediately (old transcripts
     cannot merge multi-line turns; each line is its own request).
7. After the walk, flush the final group; if any records exist,
   `lastRecord.MergeInterval(tally.Flush())` folds trailing events into the last
   record.

### Grouping by `message.id`

Qoder flushes one assistant turn as several JSONL lines (thinking, then a
tool_use batch, then text + usage) that share one `message.id`. These merge into
a **single request**: characters accumulate across all lines, while
`usage` / `timestamp` / `sessionID` / `model` / `contextWindow` are taken from the
last line that carries them. A new id, any user line (including a `tool_result`
echo), or an id-less line closes the group. A group emits a record **only** if it
has `usage`, `sessionID` and `timestamp`; otherwise it is dropped.

### Token reconstruction (the heuristic)

On flush (`flushGroup`, `usage_qoder.go:92`), tokens are reconstructed rather than
read, because Qoder's `input_tokens` / `output_tokens` are always `0`:

- **If `context_usage_ratio` is a finite number `>= 0`:**
  - Clamp the ratio to `1` if it exceeds `1`.
  - `InputTokens = round(ratio × contextWindow)`, where `contextWindow` is the
    group's window (latest `runtime-config.contextWindow > 0`, default
    `200000` = `qoderDefaultContextWindow`).
  - `OutputTokens = round(cjkChars × 1.5 + otherChars × 0.22)`
    (`qoderCjkTokensPerChar` / `qoderOtherTokensPerChar`), over the merged reply
    characters.
- **Else (no valid ratio):** fall back to the raw fields —
  `InputTokens = AsTokenCount(usage.input_tokens)`,
  `OutputTokens = AsTokenCount(usage.output_tokens)` (both `0` for Qoder).
- **Cache** is always read directly: `CacheReadTokens =
  AsTokenCount(usage.cache_read_input_tokens)`, `CacheWriteTokens =
  AsTokenCount(usage.cache_creation_input_tokens)` (`0` for Qoder).

`AsTokenCount` (`shared.go:152`) floors positive finite numbers, else `0`.

### Reply-character counting

Characters are counted per rune (code point), matching the TS `codePoint`
iteration:

- `qoderAddBlockChars` (`usage_qoder.go:223`): `text` → `block.text`; `thinking`
  → `block.thinking`; `tool_use` → `qoderAddValueChars(block.input)`. Other block
  kinds contribute nothing.
- `qoderAddValueChars` (`usage_qoder.go:241`) recurses through `tool_use.input`,
  counting only **string values** (through arrays and objects). It deliberately
  does not serialize JSON — key order and escaping differ between TS and Go and
  would break parity. Object keys, numbers and booleans are not counted.
- A plain-string assistant `content` is counted whole (`qoderAddTextChars`).
- `qoderAddTextChars` (`usage_qoder.go:256`) classifies each rune as CJK or other
  via `qoderIsCjkRune` (`usage_qoder.go:268`). CJK ranges:

  | Range | Block |
  |---|---|
  | `2E80–2FDF` | CJK radicals / Kangxi radicals |
  | `3000–303F` | CJK symbols and punctuation |
  | `3040–30FF` | Hiragana / Katakana |
  | `3400–4DBF` | CJK unified ideographs extension A |
  | `4E00–9FFF` | CJK unified ideographs |
  | `AC00–D7AF` | Hangul syllables |
  | `F900–FAFF` | CJK compatibility ideographs |
  | `FF00–FFEF` | Halfwidth / fullwidth forms |
  | `20000–2FA1F` | CJK extensions B–F / compatibility supplement |

  CJK characters weigh `1.5` tokens each; everything else `0.22`.

### Credits (`qoderRequestCredits`, `usage_qoder.go:214`)

Return the first positive finite number among `usage.credits`, then
`usage.original_credits`; else `0`. `record.Credits` is set only when this is
`> 0`. **`billable` is never checked** — free requests (`billable === false`)
retain their credits, because the value represents the request's API cost. The
credit→USD conversion happens in the pricing layer (below), not here.

### Prompt, tool, skill & active-time tally

Identical machinery to Claude (`SessionTally`, `tally.go:36`):

- **Prompts** come from `QoderUserPromptText` (array text blocks, reminder
  stripped, not injected) — not Claude's `claudeIsUserPrompt`.
- **Tools**: each named `tool_use` block feeds a tool event; `SkillOfToolUse`
  (`shared.go:82`) adds a skill event only for a tool named exactly `Skill` with
  a non-empty `input.skill`.
- **Active time**: events must arrive in ascending time order (out-of-order
  events are dropped); `ActiveMs` sums consecutive gaps, each capped at
  `ActiveGapCapMs` (120 000 ms = 2 min).
- `record.ApplyInterval(tally.TakeUntil(record.timestamp))` folds the increment
  accrued up to the record's timestamp; `MergeInterval(tally.Flush())` folds the
  tail into the last record. Because the record timestamp is the **last**
  usage-bearing line of the group, activity up to that point lands on the record.

## Aggregation & cost

- `ScanAllUsage` (`usage.go:45`) concatenates records in module order and merges
  sessions into an ordered map keyed by bare `sessionId` (last-write-wins). Each
  session's `tokenUsage` is the 5-tuple
  `[input, output, cacheRead, cacheWrite, 3]` (input excludes cache traffic; `3`
  is the status flag). `filePath` is tildified against `home`. **Credits are not
  summed into this tuple** — they live only on individual `UsageRecord`s.
- **Qoder is credit-billed.** `EstimateTokenCost` (`pricing.go:189`) routes
  `qoder` / `qoder-cn` through `creditPriceUSD` (`pricing.go:48`) instead of model
  rates: `usd = credits × per-credit price`, and `unpricedTokens` is always `0`.
  A record with no credits (older transcripts) costs `0` and counts nothing as
  unpriced. The reconstructed tokens never drive Qoder cost.
  - `qoder` per-credit price = `0.01` USD (global edition, ≈ $10 / 1000 credits).
  - These are subscription-tier averages — an estimate, not a bill.
- `PricingVersion` (`pricing.go:215`) hashes the model table **plus** the credit
  prices (qoder-cn first, then qoder), so a credit-price change invalidates
  caches.

## Quirks & edge cases

- **Tokens are reconstructed, not read.** The on-disk `input_tokens` /
  `output_tokens` / cache fields are always `0`; input comes from
  `context_usage_ratio × contextWindow`, output from a weighted character count.
- **Credit-billed, not token-billed.** Cost = `credits × per-credit price`; model
  rates are never consulted for Qoder.
- **Free requests still cost.** `billable` is ignored; `billable === false`
  records keep their credits.
- **`message.id` batching.** Several assistant lines merge into one request;
  without an id each line is its own request. The transcript does **not** merge
  them.
- **Window switches mid-file.** The latest `runtime-config.contextWindow > 0`
  wins; default `200000`. A group uses the window at its last usage-bearing line.
- **Ratio clamped to 1.** `context_usage_ratio > 1` is capped; a negative or
  non-numeric ratio falls back to the (zero) token fields.
- **Tool-input chars counted as values, not JSON.** Only string values of
  `tool_use.input` are counted, recursively, to keep TS/Go parity.
- **System reminder lives inside the prompt text block.** Stripped in both the
  title/prompt path (`QoderUserPromptText`) and the transcript (`cleanUserText`).
- **No index, no `/memory/` filter.** Every session file is streamed; workspace
  comes from `directories` → `cwd` → decoded directory name.
- **Subagents excluded.** Nested `<uuid>/subagents/` files are never enumerated;
  interleaved `isSidechain` lines are skipped in usage (but not in the transcript).
- **Timestamp asymmetry.** Scan/transcript accept numeric or string timestamps;
  usage accepts strings only.
- **Compaction marker is a fixed Chinese literal** (`上下文已压缩`), not localized.

## Source map

| Topic | Location |
|---|---|
| Registry entry | `registry.go:32` |
| Usage module entry | `usage.go:34` |
| Session scan | `qoder.go` (`ScanQoder`, `scanQoderFile`, `qoderRecordWorkspace`) |
| Prompt text / reminder strip | `qoder.go` (`QoderUserPromptText`, `stripLeadingSystemReminder`) |
| Transcript loader (shared) | `transcript_claude.go` (`parseClaudeTranscript`); dispatch `transcript.go:20` |
| Content → entries, tool cards, cleaning | `transcript.go` (`entriesFromContent`, `toolCall`, `cleanUserText`) |
| Usage extraction | `usage_qoder.go` (`scanQoderUsage`, `scanQoderUsageAs`, `scanQoderProjectUsage`, `scanQoderFileUsage`) |
| Token reconstruction / chars / CJK | `usage_qoder.go` (`flushGroup`, `qoderAddBlockChars`, `qoderAddValueChars`, `qoderAddTextChars`, `qoderIsCjkRune`) |
| Credits | `usage_qoder.go` (`qoderRequestCredits`) |
| Activity tally | `tally.go` (`SessionTally`) |
| Aggregation | `usage.go` (`ScanAllUsage`, `ApplyInterval`, `MergeInterval`) |
| Pricing (credit-billed) | `pricing.go` (`EstimateTokenCost`, `creditPriceUSD`, `PricingVersion`) |
| Shared helpers | `shared.go` (`ToTitle`, `DecodePathDirName`, `SkillOfToolUse`, `AsEpochMs`, `AsTimestamp`, `AsTokenCount`, `ListDirs`, `ListFiles`, `ScanLines`, `ReadAllLines`) |
| Output contract | `contract.go` (`SessionInfo`, `UsageRecord`, `Entry`, `ToolCall`) |