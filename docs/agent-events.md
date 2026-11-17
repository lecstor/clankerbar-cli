# Agent activity events (CLI emission)

The driver can stream a session's *activity* to the plane's per-run event
channel: which tools it called, the one identifying string of each call (a
command, a file path, a task id) and its todo snapshots. It sends **no tool
output and no assistant text** in this release. The contract lives in the plane
repo's `docs/proposals/agent-event-channel.md` (CLA-601); this file records what
the CLI implements (CLA-603).

## What is emitted — v1, activity tier only

| harness | source event | channel event |
| --- | --- | --- |
| claude | `assistant`/tool_use, `TodoWrite` | `todo_change` from `input.todos` |
| claude | `assistant`/tool_use, name `mcp__clankerbar__<tool>` | `clankerbar_mcp_call`, bare `<tool>` |
| claude | `assistant`/tool_use, any other tool | `tool_call` (target per tool shape) |
| opencode | terminal `tool_use` part, `todowrite` | `todo_change` from `state.input.todos` |
| opencode | terminal `tool_use` part, tool `clankerbar_<tool>` | `clankerbar_mcp_call`, bare `<tool>` |
| opencode | terminal `tool_use` part, any other tool | `tool_call` (target per tool shape) |

The normalizers live in `internal/harness/events.go`; the wire schema, redaction
and batcher in `internal/events`.

- **Target extraction.** Exec-shaped tools send their `command`; file-shaped
  tools send `file_path` / `filePath` / `path`; a small table covers the rest.
  The raw input JSON is never sent. The value is redacted, flattened to one line
  and truncated to 512 bytes (with the event's `truncated` marker).
- **clankerbar MCP calls** send the task id (or question id) as `target` and
  never the call's input. The call's result is an ordinary `tool_result` in the
  transcript tier, which is a follow-up.
- **Todos** are the full snapshot, capped at 50 items, each content redacted and
  cut to 256 bytes. Per-item todo tools (`TaskCreate` / `TaskUpdate`) are
  dropped: the tier reports the list, not its individual edits.

### Out of v1

- **codex** is explicitly out of v1: its stream carries the same shapes under a
  different event vocabulary, and the mapping is not wired. `codex` and
  `copilot` report `Capabilities.EmitsEvents` false, so the driver never builds
  a sink for them.
- **Transcript tier** (`tool_result`, `assistant_text`, the 64 KB caps, `.env*`
  read dropping and the wider redaction breadth) is the follow-up
  `CLI: transcript-tier events and redaction breadth`. The plane accepts those
  event types and counts them as dropped until then, so the wire contract is
  already stable.

## Turning it on and off

- **On** is the project's run-config dial `events.activity`, set by operator+
  under `set_run_config`. It is off by default and absent reads as off. The CLI
  applies it with the rest of the stored run config at an iteration boundary.
- **Off, machine-locally**, with `clankerbar run --no-events`,
  `clankerbar supervise --no-events` (children get `CLANKERBAR_EVENTS=off`),
  `clankerbar hook emit --no-events`, or `CLANKERBAR_EVENTS=off` (`0` / `false`
  accepted, case-insensitive). No machine-local signal can turn the channel ON.
- The plane re-checks the dial per batch: an emitter with a stale copy is
  refused `403 events_disabled` and stops for that run, saying so once.

## Interactive Claude Code sessions (hooks, CLA-605)

A session the driver did not spawn has no stream to read, so the same activity
tier is fed by Claude Code's own `PostToolUse` hook:

```json
{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "*",
        "hooks": [
          { "type": "command", "command": "clankerbar hook emit", "timeout": 10 }
        ]
      }
    ]
  }
}
```

`clankerbar hook emit` reads one hook payload on stdin and sends exactly one
event. The mapping is the same one `internal/harness` applies to a spawned
session's stream: `TodoWrite` becomes `todo_change`, an
`mcp__clankerbar__<tool>` call becomes `clankerbar_mcp_call` with the bare name,
and anything else becomes `tool_call` with the target extracted per tool shape.
Redaction and truncation are `internal/events`' own, unchanged. `TodoWrite` is
a tool inside `PostToolUse`, not a separate hook event, so one matcher covers
both; `PreToolUse` and every other hook event are ignored: the hook reads only
`PostToolUse` payloads.

**The session must be bound to a run.** The hook reads `CLANKERBAR_RUN_ID` (the
run credential `claim_task` returned — the bare uuid or the compound
`<uuid>.<secret>` form) and `CLANKERBAR_API_KEY` from its environment; if either
is absent it exits 0 without reading stdin. Hooks inherit the session process's
environment, so export both before starting `claude`. The plane origin comes
from the operator's config (`backlog_url`), never from a URL discovered in the
checkout.

**It never fails the session.** Every path exits 0 and writes nothing to stdout
— Claude Code feeds a hook's stdout back into the conversation. A malformed
payload, an unreadable config, another hook holding the sequence lock for over
**2 s**, a send that errors or returns non-200, and a send still open at its
**2 s** timeout all drop the event with at most one stderr line. The project
dial is the plane's to enforce: `events.activity` off refuses the batch
(`403 events_disabled`) and the hook treats that as a no-op. `--no-events` and
`CLANKERBAR_EVENTS=off` mute it machine-locally, exactly as for
`run`/`supervise`.

**Sequence numbers survive the process boundary.** Each invocation is a new
process, so the per-run counter is persisted at
`<XDG state home>/clankerbar/hook/run-event-seq.json` — counters only, no
content — and an exclusive lock held across the send makes concurrent hooks
(parallel tool calls) reserve *and arrive* in ascending order. The plane drops
any seq at or below its high-water mark, so a lower one arriving after a higher
would be a silent loss. Point the hook at the run the session claimed; do not
point it at a run a driver is already emitting for, or the two counters collide
and the plane drops one stream.

The installed wiring, including where `settings.json` lives and how to mute it,
is the served setup skill's
`clankerbar.com/skills/clankerbar-setup/claude-code-hooks.md`.

## Batching, and why a slow plane cannot hurt a session

- Flush every **2 s**, or when **50 events** are buffered, whichever first; the
  encoded body is capped at **256 KB** (one event that would overflow flushes the
  batch before it, never splits it).
- `Add` never blocks and never returns an error. The buffer is bounded (512
  events); past it events are dropped with one log line. The HTTP call runs on
  the emitter's own goroutine with a 5 s timeout. A `429` is retried twice with a
  short bounded wait (honouring `retry-after`, capped at 1 s); every other
  failure drops the batch (one line, then every twentieth), never retried in a
  loop and never surfaced to the session.
- `Close` attempts one final bounded flush (2 s grace) and then stops.

Sequence numbers are per **run**, from 1, and are the plane's idempotency key:
the plane drops any event whose `seq` is at or below the run's high-water mark.
A phased task runs several sessions on the same run id — and a review panel's
members run in parallel — so:

- The whole send of one run is **serialized** across the emitters sharing a
  counter (allocation, POST and commit under one per-run lock). Arrival order is
  therefore allocation order, which is what stops a later-numbered batch from
  making an earlier one look like a duplicate and silently losing its events.
- The counter is persisted in the state dir (`RUN_EVENT_SEQ`) at **allocation**,
  before the POST, so a clean restart resumes above the high-water mark and a
  crash replays no numbers either: a batch is either already covered by the
  durable mark or lost with a gap (which the plane does not mind). The one
  inversion left is between PROCESSES: a new process's batch can reach the plane
  before the previous process's final POST (which outlived its `Close` grace)
  completes, and the plane then drops the older batch as a duplicate. Its
  events are lost, though nothing is ever doubled; closing that gap needs the
  plane's dedupe to stop being a single high-water mark.

## Redaction

CLI-side, before anything leaves the machine, over the full string and **before**
truncation: `clk_`, `ghp_` / `github_pat_`, `sk-ant-`, `AKIA` and PEM private-key
blocks become `[redacted]`, and the event carries `redacted: true`. This is best
effort, not a guarantee — the tiers and the project's dials are the privacy
boundary, which is why the activity tier carries no output and the transcript
tier is a second opt-in.
