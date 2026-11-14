# Cindy — session store format & parsing

How Cindy stores local sessions, and how this SDK reads them without crossing
its account boundary.

## At a glance

| Property | Value |
|---|---|
| Agent id | `cindy` |
| Display name | Cindy |
| Store directory | `~/Library/Application Support/CindyGlobal` |
| Session scanner | `ScanCindy` (`cindy.go`) |
| Transcript loader | `parseCindyTranscript` (`transcript.go`) |
| Usage module | `scanCindyUsage` (`usage_cindy.go`) |
| Store format | SQLite session metadata + engine-specific JSONL transcripts |

Cindy supports three engines. `sessions.agent_kind` identifies `pi`, `codex` or
`cc` (Claude Code), and `sdk_session_id` supplies a path for legacy Pi rows or
an engine-native session ID for newer rows. The owner path is a namespace for
bot files, memories and dialogue workspaces, not the transcript directory.

## On-disk layout

```
~/Library/Application Support/CindyGlobal/
  cindy-<dataOwnerId>.db                    # active profile's session metadata
  owners/<ownerKey>/                        # account-scoped workspace namespace
  pi-agent-home/sessions/*.jsonl            # legacy Pi transcript and usage
  codex-home/sessions/YYYY/MM/DD/*.jsonl    # embedded Codex rollouts
  claude-home/projects/<key>/<id>.jsonl     # legacy dev Claude fallback

$CLAUDE_CONFIG_DIR/projects/<key>/<id>.jsonl # explicit Claude config, if set
~/.claude/projects/<key>/<id>.jsonl          # current shared Claude store
```

`ownerKey` is the first 20 lowercase hex characters of
`SHA-256(dataOwnerId)`. `cindyOwnerDirName` implements this mapping.
`activeCindyProfile` selects the top-level profile with the newest database/WAL
activity and requires its owner directory. This avoids merging accounts after
an account switch.

## Session scanning

The scanner opens the selected database read-only, reads `sessions` ordered by
`updated_at DESC`, and skips rows with non-null `hidden_at` or status
`deleted` / `hidden`.

| Field | Source |
|---|---|
| `ID` | Cindy's `sessions.id`, never the engine-native ID |
| `Workspace` | `sessions.working_dir` |
| `Title` | `ToTitle(sessions.title)` |
| `CreatedAt` / `UpdatedAt` | `sessions.created_at` / `updated_at` as epoch ms |
| `FilePath` | Resolved engine transcript path |
| `SizeBytes` | Transcript file size, or `0` when missing |
| Internal transcript format | Derived from `agent_kind`; persisted in the derived session index, not emitted as JSON |

Absolute Pi paths are retained, including older rows whose `agent_kind` is
`cc`. Relative Codex IDs are matched against the embedded rollout filename.
Claude paths use the workspace and native session ID: its project key replaces
non-alphanumeric characters with `-` and is limited to 200 bytes. The SDK checks
`CLAUDE_CONFIG_DIR`, `~/.claude`, then Cindy's legacy `claude-home`; a direct
miss falls back to a filename lookup to account for moved sessions.

## Transcript and usage

The engine recorded during scanning is authoritative; the parser does not infer
format from a filename. The session index stores this metadata and its scanner
version invalidates older derived rows.

| Engine | Transcript | Usage |
|---|---|---|
| Pi / legacy absolute path | `parsePiTranscript` | `scanPiSessionUsage`: direct `message.usage` token fields |
| Codex | `parseCodexTranscript` | `scanCodexUsageRecords`: deltas of cumulative `token_count` totals |
| Claude Code | `parseClaudeTranscript` | `scanClaudeUsageRecords`: per-turn `message.usage` token fields |

The shared engine implementations preserve their native model, prompt, tool and
active-time semantics. Output records replace only `SessionID` and `AgentID`
with Cindy's profile ID and `cindy`. A missing transcript contributes session
metadata with no usage records; an unknown engine returns an explicit error
rather than silently applying Pi semantics.

See [pi.md](pi.md), [codex.md](codex.md) and [claude.md](claude.md) for each
engine's event mapping and aggregation rules.

## Quirks

- Multiple `cindy-*.db` files can coexist after account switches. The SDK uses
  database/WAL activity to identify the recently active profile; it does not
  merge profiles.
- A missing transcript still emits the metadata row with zero size.
- A Claude workspace move can change the project key, so direct-path lookup is
  not the only discovery mechanism.
- The SQLite file may be open while Cindy is running. The SDK opens it
  read-only with a short busy timeout.

## Source map

| Topic | Location |
|---|---|
| Profile and session discovery | `cindy.go` (`ScanCindy`, `cindyTranscript`) |
| Claude path candidates | `cindy.go` (`cindyClaudeTranscript`) |
| Engine-specific transcript dispatch | `transcript.go` (`parseCindyTranscript`) |
| Engine-specific usage dispatch | `usage_cindy.go` (`scanCindySessionUsage`) |
| Shared usage implementations | `usage_pi.go`, `usage_codex.go`, `usage_claude.go` |
| Indexed format persistence | `index.go` (`querySessions`, `replaceAgentSessions`) |
