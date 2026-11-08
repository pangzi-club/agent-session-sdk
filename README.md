# agent-session-sdk

Go library and stateless CLI for reading local coding-agent session stores:
session lists, transcripts and usage statistics (tokens, credits, USD cost).

It replaces the duplicated scan layers of [agent-view](https://github.com/pangzi-club/agent-view)
and [achat](https://github.com/pangzi-club/achat). The inventory, design decisions
and staged rollout live in [docs/extraction-plan.md](docs/extraction-plan.md).

- Module: `github.com/pangzi-club/agent-session-sdk` (private), package `agentsession`
- Pure Go, no cgo; single-binary CLI at `cmd/agent-session-sdk`
- Session and usage JSON is aligned field for field with agent-view's
  `src/ipc-contract.ts`; transcripts are an additional contract

## Status

Extraction complete, tagged v0.1.0. Present: the output contract
(`contract.go`, including transcript entries and tool calls), the agent registry,
shared scan helpers, the 14 session scanners, `ScanSessions`, usage extraction
for the 11 agents with local usage data, `ScanAllUsage`, `Summarize`, transcript
loading (`LoadTranscript`, `Cache`), pricing (`EstimateTokenCost`,
`NormalizeModelId`, `LookupModelPrice`, embedded model-prices.json) and the
stateless CLI at `cmd/agent-session-sdk`. Session and usage output match the old
agent-view scanner except the four registered deviations in the plan (§9), which
have been merged back into agent-view. Both consumers (achat, agent-view) are
migrated.

## Agents

14 scanned agents, in registry order: claude, codex, gemini, cursor, opencode,
zcode, workbuddy, pi, dsh, grok, qoder, qoder-cn, kimi, hermes. qwen is detected by
consumers but not scanned. Session deletion stays in the consumer; the library
only reports `canDeleteSessions`.

## Access

The module is private, so keep it off the public proxy and let Go use your git
credentials:

```sh
go env -w GOPRIVATE=github.com/pangzi-club/*
git config --global url."git@github.com:".insteadOf "https://github.com/"
```

Consumers pin a tag: `go get github.com/pangzi-club/agent-session-sdk@vX.Y.Z`, or
`go install github.com/pangzi-club/agent-session-sdk/cmd/agent-session-sdk@vX.Y.Z`
to build the CLI from source.

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

Real consumers: [achat](https://github.com/pangzi-club/achat) imports the
library directly, agent-view shells out to the CLI through its scan bridge.

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

Session scanning covers 14 agents; usage and summary cover the 11 agents with
local usage data (no cursor / gemini / kimi). Qoder tokens are heuristically
reconstructed and records keep credits for cost estimation.

## Development

```sh
go build ./...
go test ./...
go vet ./...
```

Comments, CLI help and diagnostics are written in English.