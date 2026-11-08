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

Extraction in progress, nothing tagged yet. Present today: the output contract
(`contract.go`, including transcript entries and tool calls), the agent registry,
shared scan helpers, the 14 session scanners, `ScanSessions`, usage extraction
for the 11 agents with local usage data, `ScanAllUsage`, `Summarize` and their
tests. Session and usage output match the old agent-view scanner except the
four registered deviations in the plan (§9). Transcript loaders, pricing and
the CLI arrive in the stages listed in the plan.

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

## Development

```sh
go build ./...
go test ./...
go vet ./...
```

Comments, CLI help and diagnostics are written in English.