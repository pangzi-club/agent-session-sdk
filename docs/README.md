# Agent session-store docs

Per-agent reference docs for the session stores this SDK reads: where each agent
keeps its data on disk, the record format, and how the SDK parses it into
`SessionInfo`, `Entry[]` and `UsageRecord[]`.

These docs describe behavior that already exists in the code — they are a map to
the scanners, transcript loaders and usage extractors, not a spec for new work.
When code and doc disagree, the code wins; fix the doc.

## Template

Every agent doc follows the same section order, so they read consistently and
diff cleanly against each other:

1. **At a glance** — id, display name, store dir, `canDeleteSessions`, scanner,
   transcript loader, usage module, billing model, store format.
2. **On-disk layout** — directory tree and file naming.
3. **Record format** — the fields the SDK reads, with an illustrative record.
4. **Session scanning → `SessionInfo`** — how each output field is derived.
5. **Transcript parsing → `Entry[]`** — record handling, content blocks, tool
   cards, cleaning rules, what is dropped.
6. **Usage extraction → `UsageRecord[]`** — token mapping, prompt/tool/skill
   counting, active-time model. (Omit for agents with no local usage data.)
7. **Aggregation & cost** — token-usage tuple, billing model.
8. **Quirks & edge cases** — the surprising behaviors worth remembering.
9. **Source map** — topic → file/function.

Write in English. Reference code as `file.go:function` (add a line number when
it helps). Mark any sample data as illustrative.

## Coverage

16 scanned agents, in registry order. "Usage" is whether the agent has a local
usage module (`usage.go`); gemini, cursor, kimi and kimi-code have none.

| Agent | Store dir | Transcript loader | Usage | Doc |
|---|---|---|---|---|
| `claude` | `.claude` | `parseClaudeTranscript` | yes | [claude.md](claude.md) |
| `codex` | `.codex` | `parseCodexTranscript` | yes | [codex.md](codex.md) |
| `gemini` | `.gemini` | `parseGeminiTranscript` | no | [gemini.md](gemini.md) |
| `cursor` | `.cursor` | `parseCursorTranscript` | no | [cursor.md](cursor.md) |
| `opencode` | `.local/share/opencode` | `parseSessionDBTranscript` | yes | [opencode.md](opencode.md) |
| `zcode` | `.zcode` | `parseSessionDBTranscript` | yes | [zcode.md](zcode.md) |
| `cindy` | `Library/Application Support/CindyGlobal` | `parseCindyTranscript` | yes | [cindy.md](cindy.md) |
| `workbuddy` | `.workbuddy` | `parseWorkbuddyTranscript` | yes | [workbuddy.md](workbuddy.md) |
| `pi` | `.pi` | `parsePiTranscript` | yes | [pi.md](pi.md) |
| `dsh` | `.dsh` | `parseDshTranscript` | yes | [dsh.md](dsh.md) |
| `grok` | `.grok` | `parseGenericTranscript` | yes | [grok.md](grok.md) |
| `qoder` | `.qoder` | `parseClaudeTranscript` | yes | [qoder.md](qoder.md) |
| `qoder-cn` | `.qoder-cn` | `parseClaudeTranscript` | yes | [qoder-cn.md](qoder-cn.md) |
| `kimi` | `.kimi` | `parseKimiTranscript` | no | [kimi.md](kimi.md) |
| `kimi-code` | `.kimi-code` | `parseKimiCodeTranscript` | no | [kimi-code.md](kimi-code.md) |
| `hermes` | `.hermes` | `parseHermesTranscript` | yes | [hermes.md](hermes.md) |

`qwen` is detected by consumers but never scanned, so it has no doc.

## Error model (applies to every agent)

- A store that is **absent** (agent not installed) scans as empty — never a
  failure.
- A store that exists but cannot be read (permissions, I/O errors, a corrupt
  or locked database, `ENOTDIR` paths) fails that agent's scan. The detailed
  APIs surface this: `ScanSessionsDetailed` and `ScanAllUsageDetailed` return
  `AgentScanFailure{AgentID, AgentName, Directory, Err}` next to the
  successful agents' results; the legacy `ScanSessions` / `ScanAllUsage`
  wrappers keep the old empty-on-error behavior.
- The CLI prints successful agents' JSON on stdout, lists failed agents on
  stderr (`agent scan failed: …`) and exits `1`; usage errors exit `2`.
- Scans share a process-wide file-work budget (`FileConcurrency` = 16) across
  every agent, and JSONL usage extraction streams files instead of loading
  them whole.
