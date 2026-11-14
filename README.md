# agent-session-sdk

Go library and stateless CLI for reading local coding-agent session stores:
session lists, transcripts and usage statistics (tokens, credits, USD cost).

- Module: `github.com/pangzi-club/agent-session-sdk`, package `agentsession`
- Pure Go, no cgo; single-binary CLI at `cmd/agent-session-sdk`
- MIT licensed

## Status

v0.3.9. Present: the output contract (`contract.go`, including transcript
entries and tool calls), the agent registry, shared scan helpers, the 16
session scanners, `ScanSessions` / `ScanSessionsDetailed` (per-agent failure
reporting), usage extraction for the 12 agents with
local usage data, `ScanAllUsage` / `ScanAllUsageDetailed`, `Summarize`,
transcript loading
(`LoadTranscript`, `Cache`), pricing (`EstimateTokenCost`, `NormalizeModelId`,
`LookupModelPrice`, embedded model-prices.json), the incremental SQLite session
index and the CLI at `cmd/agent-session-sdk`.

## Agents

16 scanned agents, in registry order: claude, codex, gemini, cursor, opencode,
zcode, cindy, workbuddy, pi, dsh, grok, qoder, qoder-cn, kimi, kimi-code, hermes. qwen is detected by
consumers but not scanned. Session deletion stays in the consumer; the library
only reports `canDeleteSessions`.

## Install

```sh
go get github.com/pangzi-club/agent-session-sdk@v0.1.0
go install github.com/pangzi-club/agent-session-sdk/cmd/agent-session-sdk@v0.1.0
```

## Library usage

```go
import agentsession "github.com/pangzi-club/agent-session-sdk"
```

Session lists — one `SessionScanResult` per agent, scanned in parallel. A
failing agent contributes no sessions; `ScanSessionsDetailed` additionally
returns per-agent failures so an unreadable store is distinguishable from an
empty one (a store that is simply absent never counts as a failure):

```go
report := agentsession.ScanSessionsDetailed(home)
for _, failure := range report.Failures {
    log.Printf("agent scan failed: %v", failure) // AgentID, Directory, Err
}
for _, result := range report.Results {
    for _, session := range result.Sessions {
        fmt.Println(result.AgentID, session.ID, session.UpdatedAt)
    }
}
```

`ScanSessions` remains as the original convenience API when failures can be
ignored.

Session index — a derived SQLite index at
`<home>/.agent-session-sdk/home.sqlite` skips unchanged agent stores during
refresh, then serves lists, exact lookups and substring search (including CJK)
directly from SQLite:

```go
index, err := agentsession.OpenSessionIndex(home)
if err != nil { return err }
defer index.Close()
if _, err := index.Refresh(); err != nil { return err }

results, err := index.List(agentsession.SessionIndexQuery{Search: "登录"})
session, err := index.Get("claude", "session-id")
```

Set `AGENT_SESSION_INDEX_DB` to use another SQLite path. The index is a cache:
schema and scanner versions invalidate it automatically, and source stores stay
authoritative.

Transcripts — `LoadTranscript` takes a `SessionInfo` from the scan (it locates
the backing file through `FilePath`), so scan first, then load. `Cache` adds an
LRU keyed by session that reloads when the store's size or mtime changes:

```go
cache := agentsession.NewCache(32)
entries, err := cache.Load("claude", session) // []Entry{Role, Text, Tools, Timestamp}
```

Usage — `ScanAllUsageDetailed` returns records plus the sessions they belong
to and, like the session scan, per-agent failures (`ScanAllUsage` is the
convenience wrapper that drops them); `Summarize` aggregates per agent;
`EstimateTokenCost` prices one record
(credit-billed agents such as qoder pay credits, not model rates):

```go
report := agentsession.ScanAllUsageDetailed(home)
for _, record := range report.Scan.Records {
    usd, unpriced := agentsession.EstimateTokenCost(agentsession.TokenCostInput{
        AgentID:          record.AgentID,
        Model:            record.Model,
        InputTokens:      record.InputTokens,
        OutputTokens:     record.OutputTokens,
        CacheReadTokens:  record.CacheReadTokens,
        CacheWriteTokens: record.CacheWriteTokens,
        Credits:          record.Credits,
    })
    fmt.Println(record.SessionID, usd, unpriced)
}
summaries := agentsession.Summarize(report.Scan) // []AgentUsageSummary
```

`agentsession.PricingVersion` changes whenever the embedded price table does;
embed it in cost caches to invalidate them without a manual bump.

## CLI usage

Build with `make build-cli` (writes `./bin/agent-session-sdk`) or the
`go install` command above. All output is JSON on stdout:

```sh
agent-session-sdk <sessions|usage|summary|transcript|search> [flags]
```

| Subcommand | Output |
|---|---|
| `sessions` | session list per agent (`SessionScanResult[]`) |
| `usage` | usage records plus their sessions (`{records, sessions}`) |
| `summary` | per-agent totals (sessions / requests / tokens / active time) |
| `transcript` | one session's entries (`Entry[]`; needs `--agent` + `--session`) |
| `search` | indexed sessions matching title/workspace/id (`SessionScanResult[]`) |

Flags: `--home <dir>` (default `$HOME`), `--agent <ids>`
(comma-separated filter, e.g. `--agent codex,claude`), `--pretty` (indent),
`--cost` (usage only: appends `costUsd` / `costUnpricedTokens` per record and
`pricingVersion` at the top level), `--session <id>` (transcript only).
Additional indexed-read flags: `--index` (sessions/transcript), `--no-refresh`,
`--limit <n>` and `--offset <n>` (indexed sessions/search).

Exit codes: `0` on success, `1` when output could not be produced or at least
one selected agent store failed to scan, `2` for invalid usage. Failed agents
are reported on stderr (`agent scan failed: …`) while successful agents' JSON
still goes to stdout, so a partial failure never masquerades as "no data".

```sh
agent-session-sdk sessions --agent codex --pretty
agent-session-sdk usage --cost | jq '.records | length'
agent-session-sdk summary --agent codex,claude
agent-session-sdk transcript --agent claude --session <sessionId> --pretty
agent-session-sdk sessions --index --agent codex --pretty
agent-session-sdk search 登录 --agent claude --limit 20
```

Session scanning covers 16 agents; usage and summary cover the 12 agents with
local usage data (no cursor / gemini / kimi / kimi-code). Qoder tokens are heuristically
reconstructed and records keep credits for cost estimation.

## Price table

`model-prices.json` is an embedded snapshot of [models.dev](https://models.dev)
model rates, curated per provider. Refreshes only touch that JSON file;
`PricingVersion` (a hash of the table) changes with it.

## Development

```sh
make check   # build + vet + test + race
```

Comments, CLI help and diagnostics are written in English.

## License

[MIT](LICENSE). The embedded price table is a snapshot of
[models.dev](https://models.dev) data, also MIT licensed.
