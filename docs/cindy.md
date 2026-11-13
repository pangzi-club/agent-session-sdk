# Cindy — session store format & parsing

How Cindy stores local sessions, and how this SDK reads them without crossing
its account boundary.

Cindy is a split store. Session metadata lives in one profile SQLite database;
visible turns, tool events and provider usage live in the Pi JSONL file named by
that row's `sdk_session_id`. The owner path is not the transcript directory: it
is a namespace for bot files, memories and dialogue workspaces.

## At a glance

| Property | Value |
|---|---|
| Agent id | `cindy` |
| Display name | Cindy |
| Store directory | `~/Library/Application Support/CindyGlobal` |
| Session scanner | `ScanCindy` (`cindy.go:18`) |
| Transcript loader | `parsePiTranscript` (`transcript_cursor.go:27`) |
| Usage module | `scanCindyUsage` (`usage_cindy.go:9`) |
| Store format | SQLite session metadata + Pi-format JSONL transcripts |

## On-disk layout

```
~/Library/Application Support/CindyGlobal/
  cindy-<dataOwnerId>.db                    # active profile's session metadata
  owners/<ownerKey>/                        # account-scoped workspace namespace
  pi-agent-home/sessions/*.jsonl            # transcript and usage bodies
```

`ownerKey` is the first 20 lowercase hex characters of
`SHA-256(dataOwnerId)`. For example, data owner `abc` hashes to a directory
named `ba7816bf8f01cfea414140de5dae2223b00361a3`, not to `abc`. This mapping is
implemented by `cindyOwnerDirName` (`cindy.go:106`).

`activeCindyProfile` (`cindy.go:66`) scans top-level `cindy-*.db` files, picks
the one with the newest database/WAL activity, strips its `cindy-` prefix and
`.db` suffix to recover `dataOwnerId`, and accepts it only when the matching
owner directory exists. Selecting one profile prevents sessions from another
signed-in account from leaking into the Cindy agent.

## Session scanning

The scanner opens the selected database read-only, reads `sessions` ordered by
`updated_at DESC`, and skips rows with non-null `hidden_at` or status
`deleted` / `hidden` (`isCindyHidden`, `cindy.go:111`).

| Field | Source |
|---|---|
| `ID` | `sessions.id` |
| `Workspace` | `sessions.working_dir` (raw path) |
| `Title` | `ToTitle(sessions.title)` |
| `CreatedAt` / `UpdatedAt` | `sessions.created_at` / `updated_at` as epoch ms |
| `FilePath` | raw `sessions.sdk_session_id` path, when present |
| `SizeBytes` | size of the Pi JSONL file, or `0` when missing |

The scanner deliberately keeps Cindy's `sessions.id`, not Pi's internal session
ID, so transcripts and usage aggregates remain keyed by the Cindy session.

## Transcript and usage

Cindy's transcript file uses Pi's JSONL event shape. `LoadTranscript` routes
`cindy` to `parsePiTranscript`, which emits user/assistant text, reasoning and
tool calls from `message` events. `scanCindyUsage` reuses the shared Pi session
extractor through `scanPiSessionUsage` (`usage_pi.go:44`), replacing only the
record's `AgentID` with `cindy`.

Usage mapping therefore follows Pi: `message.usage.input`, `.output`,
`.cacheRead` and `.cacheWrite` map directly to the record fields. Prompts, tool
calls and active time are inferred with the same session tally used by Pi.

## Quirks

- `owners/<ownerKey>` normally contains empty per-dialogue directories; it is
  the account namespace, not the message store.
- Multiple `cindy-*.db` files can coexist after account switches. The SDK uses
  mtime to identify the recently active profile; it does not merge profiles.
- A missing or no-longer-present Pi JSONL still emits the metadata row, with the
  stored transcript path and zero size.
- The SQLite file may be open while Cindy is running. Like the other SQLite
  agents, this SDK opens it read-only with a short busy timeout.
