# Cindy — session store format & parsing

How Cindy stores local sessions, and how this SDK reads them without crossing
its account boundary.

## At a glance

| Property | Value |
|---|---|
| Agent id | `cindy` |
| Display name | Cindy |
| Store directory | `~/Library/Application Support/CindyGlobal` (macOS), `%APPDATA%\CindyGlobal` (Windows), `~/.config/CindyGlobal` (Linux) |
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
~/Library/Application Support/CindyGlobal/    # macOS; %APPDATA%\CindyGlobal on
                                                # Windows, ~/.config/CindyGlobal on Linux
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
an account switch. Only `cindy-*.db` files are profile candidates, so a backup
renamed `cindy-<id>.db.bak.<timestamp>` no longer matches.
Legacy rows store POSIX transcript paths (`/Users/...`). On Windows those fail
`filepath.IsAbs` yet are still paths, so a leading `/` counts as absolute when
distinguishing a stored transcript path from an engine-native session id.

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
| Missing file (any engine) | `parseCindyDBTranscript` (`transcript_cindy_db.go`): the messages mirror | none (no engine usage data on this machine) |

The shared engine implementations preserve their native model, prompt, tool and
active-time semantics. Output records replace only `SessionID` and `AgentID`
with Cindy's profile ID and `cindy`. A missing transcript contributes session
metadata with no usage records; an unknown engine returns an explicit error
rather than silently applying Pi semantics.

See [pi.md](pi.md), [codex.md](codex.md) and [claude.md](claude.md) for each
engine's event mapping and aggregation rules.

## Remote sessions and the messages mirror

Cindy mirrors every visible turn of every session in the profile database's
`messages` table — including sessions whose engine ran on another device
(remote agents) and legacy rows whose stored transcript path belongs to a
different home. When the resolved engine transcript is missing locally, the
scanner falls back to that mirror:

- `FilePath` becomes `<profile-db>#<session-id>`, the same database-reference
  convention the OpenCode/Zcode loaders use;
- the internal transcript format is `cindy-db`, handled by
  `parseCindyDBTranscript` (`transcript_cindy_db.go`);
- `SizeBytes` is the summed `content` + `agent_meta` length of the session's
  non-rewound messages.

The mirror parser reads rows ordered by `created_at`:

| `role` | Output |
|---|---|
| `user` | text entry; a `{"text": …}` JSON envelope is unwrapped, images/files ignored |
| `assistant` | text entry (plain text) |
| `thinking` | thinking entry with `text` and `durationMs` |
| `tool_use` | tool card from `{toolUseId, toolName, input}` |
| `tool_result` | matched back onto its tool card by `tool_use_id`; `<tool_use_error>` marks failure |
| `error` | system notice with the engine error message |
| `system`, `agent_switch`, `ask_user`, `progress` | internal bookkeeping, skipped |

Rows with a non-null `rewind_at` are excluded. Usage scanning stays with the
engine transcripts: the mirror carries no token counts, so a mirror-backed
session contributes metadata with no usage records (the usage module already
skips sessions whose transcript is unreadable).

The session index persists `transcript_format`, and bumping
`sessionIndexScannerVersion` rebuilds derived rows so pre-mirror indexes pick
up the new references.

## Quirks

- Multiple `cindy-*.db` files can coexist after account switches. The SDK uses
  database/WAL activity to identify the recently active profile; it does not
  merge profiles. Backup files (`cindy-<id>.db.bak.*`) are ignored.
- The registry's store directory is platform-specific (`cindyStoreDirectory`):
  Electron resolves `appData` to `AppData/Roaming` on Windows and `.config` on
  Linux, so the Cindy store is not always under the macOS path.
- A missing transcript still emits the metadata row with zero size — or the
  messages-mirror reference when the mirror has content for it.
- A Claude workspace move can change the project key, so direct-path lookup is
  not the only discovery mechanism.
- The SQLite file may be open while Cindy is running. The SDK opens it
  read-only with a short busy timeout.

## Source map

| Topic | Location |
|---|---|
| Profile and session discovery | `cindy.go` (`ScanCindy`, `cindyTranscript`, `cindyMessageSizes`) |
| Messages-mirror fallback | `cindy.go` (`cindyMessageSizes`), `transcript_cindy_db.go` (`parseCindyDBTranscript`) |
| Claude path candidates | `cindy.go` (`cindyClaudeTranscript`) |
| Engine-specific transcript dispatch | `transcript.go` (`parseCindyTranscript`) |
| Engine-specific usage dispatch | `usage_cindy.go` (`scanCindySessionUsage`) |
| Shared usage implementations | `usage_pi.go`, `usage_codex.go`, `usage_claude.go` |
| Indexed format persistence | `index.go` (`querySessions`, `replaceAgentSessions`) |
