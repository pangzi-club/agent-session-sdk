---
name: agent-session-sdk
description: Search and inspect local coding-agent sessions, transcripts, and token usage with the agent-session-sdk CLI. Use when the user asks about their past agent conversations, a specific session, or local agent usage/cost.
---

# Agent Session SDK

Use the installed `agent-session-sdk` CLI to inspect local coding-agent data.
The CLI must be installed and available on `PATH`; this skill does not install
it. It reads the current user's local agent stores and does not modify them.

## Workflow

1. Identify the requested agent or topic. Use `--agent <id>` whenever possible
   to avoid scanning unrelated stores.
2. For a topic or title, search first:

   ```sh
   agent-session-sdk search "<query>" --agent <id> --limit 10 --human
   ```

3. Inspect a matching conversation only when needed:

   ```sh
   agent-session-sdk transcript --agent <id> --session <session-id> --index --human
   ```

4. For usage totals, prefer the compact summary:

   ```sh
   agent-session-sdk summary --agent <id> --human
   ```

   Use `usage --cost --agent <id> --human` only when per-record details are
   needed.

5. Summarize only the relevant findings. Do not paste a whole transcript unless
   the user asks for it. Be clear when a scan reports failures or finds no match.

## Output and errors

- Use `--human` for interactive inspection and readable transcripts.
- JSON is the default output. Omit `--human` when exact structured values are
  needed for further processing.
- Search reads the derived SQLite index and refreshes it by default. Use
  `--no-refresh` only when the user explicitly prefers cached results.
- Transcript output may contain private conversation text. Quote only what is
  necessary to answer the user's request.
- Exit code `1` or an `agent scan failed` diagnostic means results may be
  incomplete; do not describe them as a complete empty result.
- If the command is missing, tell the user to install it and make sure its
  install directory is on `PATH`:

  ```sh
  go install github.com/pangzi-club/agent-session-sdk/cmd/agent-session-sdk@latest
  ```

Supported agent IDs are listed by `agent-session-sdk help`.
