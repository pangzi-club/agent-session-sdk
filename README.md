# agent-session-sdk

Go library and stateless CLI for reading local coding-agent session stores:
session lists, transcripts and usage statistics (tokens, credits, USD cost).

- Module: `github.com/pangzi-club/agent-session-sdk`, package `agentsession`
- Pure Go, no cgo; single-binary CLI at `cmd/agent-session-sdk`
- MIT licensed

## Status

v0.1.0. Present: the output contract (`contract.go`, including transcript
entries and tool calls), the agent registry, shared scan helpers, the 14
session scanners, `ScanSessions`, usage extraction for the 12 agents with
local usage data, `ScanAllUsage`, `Summarize`, transcript loading
(`LoadTranscript`, `Cache`), pricing (`EstimateTokenCost`, `NormalizeModelId`,
`LookupModelPrice`, embedded model-prices.json) and the stateless CLI at
`cmd/agent-session-sdk`.

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

Session lists — one `SessionScanResult` per agent, scanned in parallel:

```go
for _, result := range agentsession.ScanSessions(home) {
    for _, session := range result.Sessions {
        fmt.Println(result.AgentID, session.ID, session.UpdatedAt)
    }
}
```

Transcripts — `LoadTranscript` takes a `SessionInfo` from the scan (it locates
the backing file through `FilePath`), so scan first, then load. `Cache` adds an
LRU keyed by session that reloads when the store's size or mtime changes:

```go
cache := agentsession.NewCache(32)
entries, err := cache.Load("claude", session) // []Entry{Role, Text, Tools, Timestamp}
```

Usage — `ScanAllUsage` returns records plus the sessions they belong to;
`Summarize` aggregates per agent; `EstimateTokenCost` prices one record
(credit-billed agents such as qoder pay credits, not model rates):

```go
scan := agentsession.ScanAllUsage(home)
for _, record := range scan.Records {
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
summaries := agentsession.Summarize(scan) // []AgentUsageSummary
```

`agentsession.PricingVersion` changes whenever the embedded price table does;
embed it in cost caches to invalidate them without a manual bump.

## CLI usage

Build with `make build-cli` (writes `./bin/agent-session-sdk`) or the
`go install` command above. All output is JSON on stdout:

```sh
agent-session-sdk <sessions|usage|summary|transcript> [flags]
```

| Subcommand | Output |
|---|---|
| `sessions` | session list per agent (`SessionScanResult[]`) |
| `usage` | usage records plus their sessions (`{records, sessions}`) |
| `summary` | per-agent totals (sessions / requests / tokens / active time) |
| `transcript` | one session's entries (`Entry[]`; needs `--agent` + `--session`) |

Flags: `--home <dir>` (default `$HOME`), `--agent <ids>`
(comma-separated filter, e.g. `--agent codex,claude`), `--pretty` (indent),
`--cost` (usage only: appends `costUsd` / `costUnpricedTokens` per record and
`pricingVersion` at the top level), `--session <id>` (transcript only).

```sh
agent-session-sdk sessions --agent codex --pretty
agent-session-sdk usage --cost | jq '.records | length'
agent-session-sdk summary --agent codex,claude
agent-session-sdk transcript --agent claude --session <sessionId> --pretty
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
go build ./...
go test ./...
go vet ./...
```

Comments, CLI help and diagnostics are written in English.

## License

[MIT](LICENSE). The embedded price table is a snapshot of
[models.dev](https://models.dev) data, also MIT licensed.
