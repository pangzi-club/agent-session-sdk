# Qoder CN — session store format & parsing

How the `qoder-cn` agent (the Chinese edition of Qoder) stores sessions on disk,
and how this SDK turns that store into `SessionInfo`, `Entry[]` and
`UsageRecord[]`.

**Qoder CN shares Qoder's format and code almost entirely.** It uses the same
session scanner (`ScanQoder`), the same Claude transcript loader
(`parseClaudeTranscript`), and the same usage extractor body
(`scanQoderUsageAs`). Only the store directory, the agent id, the usage wrapper,
and the credit→USD price differ. Read [qoder.md](qoder.md) for the full
reference; this doc records only the deltas.

This doc follows the shared template (see [README.md](README.md)).

## At a glance

| Property | Value |
|---|---|
| Agent id | `qoder-cn` |
| Display name | Qoder CN |
| Store directory | `~/.qoder-cn` |
| `canDeleteSessions` | `false` |
| Session scanner | `ScanQoder` (`qoder.go:20`) — **same as `qoder`** |
| Transcript loader | `parseClaudeTranscript` (`transcript_claude.go:5`) — **same as `qoder` / `claude`** |
| Usage module | `scanQoderCnUsage` (`usage_qoder.go:55`) → `scanQoderUsageAs(agentDir, "qoder-cn")` |
| Billing | Credit-metered, like `qoder`; only the per-credit USD price differs |
| Store format | JSONL transcript per session; no index file — **same as `qoder`** |

## On-disk layout

Identical to [qoder.md](qoder.md#on-disk-layout), rooted at `~/.qoder-cn` instead
of `~/.qoder`:

```
~/.qoder-cn/
  projects/
    -Users-alice-code-app/
      <sessionUuid>.jsonl
      <sessionUuid>/subagents/…        # NOT scanned (non-recursive listing)
```

## Record format

Identical to [qoder.md](qoder.md#record-format-jsonl): the same JSONL fields,
content blocks, and `message.usage` keys (`context_usage_ratio`, `credits`,
`original_credits`, the always-`0` token fields, the ignored `billable`). The
same `runtime-config` line carries `contextWindow`.

## Session scanning → `SessionInfo`

Identical to [qoder.md](qoder.md#session-scanning--sessioninfo). Both registry
defs call `ScanQoder` (`registry.go:33`); the scanner takes the agent directory
as its only argument, so nothing about the scan is edition-specific.

## Transcript parsing → `Entry[]`

Identical to [qoder.md](qoder.md#transcript-parsing--entry) and
[claude.md](claude.md#transcript-parsing--entry). `LoadTranscript` dispatches
`qoder-cn` to `parseClaudeTranscript` (`transcript.go:20`), including the fixed
Chinese compaction literal `上下文已压缩`.

## Usage extraction → `UsageRecord[]`

The extractor body is shared. `scanQoderCnUsage` (`usage_qoder.go:55`) is a
one-line wrapper:

```go
func scanQoderCnUsage(agentDir string) (*usageModuleResult, error) {
	return scanQoderUsageAs(agentDir, "qoder-cn")
}
```

`scanQoderUsageAs` (`usage_qoder.go:59`) runs `ScanQoder` for the session list and
`scanQoderFileUsage` per file. The `agentID` argument (`"qoder-cn"`) is used
**only** to stamp `UsageRecord.AgentID`; every parsing step — sidechain skip,
`runtime-config` window tracking, `message.id` grouping, the
`context_usage_ratio × contextWindow` input estimate, the CJK/other character
output estimate, credit pass-through, and the tally — is exactly as documented in
[qoder.md](qoder.md#usage-extraction--usagerecord).

## Aggregation & cost

Same aggregation as `qoder` (`ScanAllUsageDetailed`, `usage.go`; module entry in
`usageModules`): the `tokenUsage` 5-tuple `[input, output, cacheRead, cacheWrite,
3]`, credits kept per-record (not summed into the tuple).

The **only** cost difference is the per-credit price in `creditPriceUSD`
(`pricing.go:48`), applied by `EstimateTokenCost` (`pricing.go:189`):

| Agent | Per-credit USD | Basis |
|---|---|---|
| `qoder` | `0.01` | global edition, ≈ $10 / 1000 credits |
| `qoder-cn` | `59.0 / 2000.0 / 7.1` ≈ `0.004155` | CN Pro ¥59 / 2000 credits at 7.1 CNY/USD |

As with `qoder`, cost = `credits × price`, `unpricedTokens` is always `0`, missing
credits cost `0`, and the reconstructed tokens never drive cost. `PricingVersion`
hashes the credit entries with **qoder-cn first**, then qoder (`pricing.go:217`).

## Quirks & edge cases

All of [qoder.md](qoder.md#quirks--edge-cases) apply unchanged (heuristic tokens,
credit billing, free requests still cost, `message.id` batching, mid-file window
switches, ratio clamping, value-only tool-input char counting, in-block system
reminders, no index, subagents excluded, timestamp asymmetry, fixed Chinese
compaction literal). The edition-specific quirk is the **different credit
price** above; a CN credit is worth roughly 0.4155 of a global credit in USD.

## Source map

| Topic | Location |
|---|---|
| Registry entry | `registry.go:33` |
| Usage module entry | `usage.go:35` |
| Usage wrapper | `usage_qoder.go:55` (`scanQoderCnUsage`) → `usage_qoder.go:59` (`scanQoderUsageAs`) |
| Session scan (shared) | `qoder.go` (`ScanQoder`, `scanQoderFile`) |
| Transcript loader (shared) | `transcript_claude.go` (`parseClaudeTranscript`); dispatch `transcript.go:20` |
| Usage body / heuristic (shared) | `usage_qoder.go` (`scanQoderFileUsage`, `flushGroup`, `qoderRequestCredits`, char/CJK helpers) |
| Credit price (CN) | `pricing.go:48` (`creditPriceUSD`), `pricing.go:189` (`EstimateTokenCost`) |
| Everything else | See [qoder.md](qoder.md#source-map) |