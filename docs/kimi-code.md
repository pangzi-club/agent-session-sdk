# Kimi Code — desktop session store format & parsing

How Kimi Code Desktop stores sessions on disk, and how this SDK turns that
store into `SessionInfo` and `Entry[]`. Kimi Code Desktop is intentionally
separate from the legacy Kimi CLI (`kimi` / `~/.kimi`).

## Layout

```
~/.kimi-code/
  session_index.jsonl                     # sessionId -> sessionDir/workDir
  sessions/
    wd_<workspace-slug>_<hash>/
      session_<uuid>/
        state.json
        agents/
          main/wire.jsonl
```

`state.json` supplies the session id, workspace (`cwd`), title, creation time
and update time. `session_index.jsonl` is a workspace fallback for an
incomplete state file. A session can contain multiple agents; non-`main`
agents are represented as `<sessionId>:<agentName>`.

## Transcript parsing

`parseKimiCodeTranscript` reads `agent.message.appended` records. The desktop
protocol nests the message at `message.message`; times are epoch milliseconds.

- `role: user` content becomes user text.
- Assistant `content` becomes thinking and text entries; the `think` block
  type is retained.
- Assistant `toolCalls` become one entry per call, preserving its id, bounded
  input and semantic tool kind.
- The following `role: tool` records are matched back to calls by
  `toolCallId`, attaching bounded output and an error flag when available.
- `turn.ended.durationMs` is retained on the turn's final entry.
