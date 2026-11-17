# Agent activity events (CLI emission)

The driver can stream a session to the plane's per-run event channel in two
tiers. The **activity** tier (`events.activity`) carries which tools the session
called, the one identifying string of each call (a command, a file path, a task
id) and its todo snapshots. The **transcript** tier (a second opt-in,
`events.transcript`) adds a completed call's output and each assistant text
block, capped at 64 KB per event. The contract lives in the plane repo's
`docs/proposals/agent-event-channel.md` (CLA-601); this file records what the CLI
implements (CLA-603, transcript tier CLA-887).

## What is emitted

| harness | source event | channel event |
| --- | --- | --- |
| claude | `assistant`/tool_use, `TodoWrite` | `todo_change` from `input.todos` |
| claude | `assistant`/tool_use, name `mcp__clankerbar__<tool>` | `clankerbar_mcp_call`, bare `<tool>` |
| claude | `assistant`/tool_use, any other tool | `tool_call` (target per tool shape) |
| claude | `assistant`, block `text` | `assistant_text` (transcript) |
| claude | `user`, block `tool_result` | `tool_result` (transcript), `callId` = `tool_use_id`, `ok` = `!is_error` |
| opencode | terminal `tool_use` part, `todowrite` | `todo_change` from `state.input.todos` |
| opencode | terminal `tool_use` part, tool `clankerbar_<tool>` | `clankerbar_mcp_call`, bare `<tool>` |
| opencode | terminal `tool_use` part, any other tool | `tool_call` (target per tool shape) |
| opencode | `text` part | `assistant_text` (transcript) |
| opencode | terminal `tool_use` part, any tool | `tool_result` (transcript) from `state.output` / `state.error`, same `callId` |

The normalizers live in `internal/harness/events.go`; the wire schema, redaction
and batcher in `internal/events`.

- **Target extraction.** Exec-shaped tools send their `command`; file-shaped
  tools send `file_path` / `filePath` / `path`; a small table covers the rest.
  The raw input JSON is never sent. The value is redacted, flattened to one line
  and truncated to 512 bytes (with the event's `truncated` marker).
- **clankerbar MCP calls** send the task id (or question id) as `target` and
  never the call's input. The call's result is an ordinary `tool_result` in the
  transcript tier.
- **Todos** are the full snapshot, capped at 50 items, each content redacted and
  cut to 256 bytes. Per-item todo tools (`TaskCreate` / `TaskUpdate`) are
  dropped: the tier reports the list, not its individual edits.
- **Transcript events are a second opt-in.** With `events.transcript` off, the
  normalizers still feed the activity tier and the transcript methods do
  nothing. With it on, each `tool_result` carries `output` (head bytes at the
  64 KB cap, `truncated` when cut), `outputBytes` (the original length measured
  after redaction and before truncation, so `outputBytes > len(output)` means
  exactly "bytes were cut") and `ok`. Each `assistant_text` carries `text` and
  `textBytes` on the same rule. One event per assistant text block, in order;
  consecutive blocks are not merged.
- **`.env*` reads are withheld.** The output of a read of any `.env*` path is
  dropped entirely: the `tool_result` carries no `output`, only `redacted: true`
  (`outputBytes` is still present, as the wire requires). "Read" means the
  read-shaped tool (`Read` on claude, `read` on opencode); a shell `cat .env` is
  not visible to this rule.
- **Spilled results are rehydrated.** Claude Code and opencode sometimes replace
  an oversized tool result with a pointer to a file. When that file is readable
  under the adapter's existing guards, the transcript event carries the output
  itself, not the pointer.

### Out of v1

- **codex** is explicitly out of v1: its stream carries the same shapes under a
  different event vocabulary, and the mapping is not wired. `codex` and
  `copilot` report `Capabilities.EmitsEvents` false, so the driver never builds
  a sink for them.
- **Interactive Claude Code sessions (hooks)** emit the activity tier only. A
  `PostToolUse` hook has no assistant text to report, and the transcript tier's
  tool output is left to a spawned session for now.

## Turning it on and off

- **On** is the project's run-config dial `events.activity`, set by operator+
  under `set_run_config`. It is off by default and absent reads as off, and the
  transcript tier is a second dial beside it: `events.transcript` may only be
  true when activity is true (the plane refuses the pair otherwise, and the CLI
  reads a stored transcript-only block as off). The CLI applies both with the
  rest of the stored run config at an iteration boundary.
- **Off, machine-locally**, with `clankerbar run --no-events`,
  `clankerbar supervise --no-events` (children get `CLANKERBAR_EVENTS=off`),
  `clankerbar hook emit --no-events`, or `CLANKERBAR_EVENTS=off` (`0` / `false`
  accepted, case-insensitive). It mutes both tiers. No machine-local signal can
  turn either on.
- The plane re-checks the dials per batch: an emitter with a stale copy is
  refused `403 events_disabled` and stops for that run, saying so once. A
  transcript event sent while the transcript dial is off is counted under the
  ACK's `dropped.transcript`; the CLI treats that as a stop (below).

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

### The transcript tier's 50 MB run cap

The plane is the authority on volume: a run's transcript prefix stops at
**50 MB**, and the first event that would cross it is dropped while the plane
appends a `run_cap` `marker` to the feed. Markers are **plane-sourced** (a
`feedSeq`, no emitter `seq`) — a harness never emits one, and the CLI does not
try to.

The CLI's half of the cooperation is the ACK: every ingest response carries
`dropped: { transcript: N }`, which is non-zero when the cap stopped the tier
(or when a stale emitter sent transcript events with the dial off). On the first
non-zero count the emitter **stops the transcript tier for that run**: queued
transcript events are dropped, later ones are refused before they are buffered,
and one log line says so. Activity keeps flowing — a runaway run degrades, it
never bills unbounded.

## Redaction

CLI-side, before anything leaves the machine, over the full string and **before**
truncation: `clk_`, `ghp_` / `github_pat_`, `sk-ant-`, `AKIA` and PEM private-key
blocks become `[redacted]`, and the event carries `redacted: true`. The same pass
runs over every string field the channel carries — `target`, todo content, a
`tool_result`'s `output` and an `assistant_text`'s `text` — and it runs before
the 64 KB cut, so a secret near the cap boundary is not half-missed.

On top of that, the output of a read of any `.env*` path is dropped entirely
(`tool_result` with no `output`, `redacted: true`). Both are best effort, not a
guarantee — an unrecognised credential shape passes through — which is why the
tiers and the project's dials are the privacy boundary and the transcript tier is
a separate opt-in.

## Inbound console messages (CLI delivery)

The channel's other direction (plane task CLA-606): an operator+ can send a message from a
run's task page into the live session. The CLI polls `GET
/api/runs/<runId>/inbox?after=<seq>` every 2 s with the run's own `clk_` credential, delivers
each unacknowledged message, and then ACKs the highest seq it delivered. The plane records the
message as an `inbound_message` feed event, so the transcript shows the steering.

- **Two adapters deliver, each the only way its CLI can take input.** `claude -p
  --input-format stream-json` reads user turns from stdin for the session's lifetime, so a
  message arrives between turns: the prompt is no longer an argv argument but the first stdin
  user message, and closing stdin tells the CLI to finish the current turn and exit. opencode
  has no stdin channel, so a message is steered into the live session through the private
  `opencode serve` the adapter already runs for every non-probe session with a clankerbar MCP
  server (CLA-911): `POST /api/session/<sessionID>/prompt` with `{"text": ..., "delivery":
  "steer"}` and the server's basic auth. `steer`, not `queue`: a queued item waits for
  execution to end, which in a headless run is the moment the process exits. A session with no
  private server — a probe, or one whose config declares no clankerbar MCP server — receives
  nothing, and its messages stay queued on the plane. `codex` and `copilot` have **no
  delivery** (`Capabilities.DeliversMessages` is false): they run one-shot prompts with no
  equivalent channel. An interactive session also receives nothing in v1; its hook (CLA-605)
  only emits activity, and polling from a hook is the stated follow-up.
- **A failed or slow poll cannot fail the session.** The poller is its own goroutine with a
  per-request timeout; a plane outage is logged once per distinct failure and delivery
  resumes. A message that could not be delivered (stdin closed, or opencode's server refusing
  the prompt) stays queued on the plane. opencode's delivery target is the LIVE process: each
  run, resurrection or turn continuation starts its own private server, the target is attached
  to it for that process's lifetime, and a message arriving while no process is live waits
  rather than being posted to a stopped server.
- **Claude's turn accounting keeps the old lifecycle.** Each user message written (the
  prompt included) is one outstanding turn; each `result` on the stream retires one. When the
  last one is retired and no message is outstanding, the adapter closes stdin and the CLI
  exits — exactly when the old `-p <prompt>` form did. A message delivered mid-turn is
  therefore processed as the next turn before the session ends. An opencode steer creates no
  outstanding turn: it is handled inside the turn already running, and the run ends when the
  model stops, as it always did.
- **Exactly-once within the session.** The cursor advances only after a successful delivery —
  a stdin write, or a 2xx from opencode's prompt endpoint — and a repeated `seq` is skipped.
  Across a restart the plane may re-offer a message delivered but not ACKed (at-least-once at
  the transport); the ACK is sent immediately after each delivery to keep that window small.
- **Wiring.** `SetInboxChannel(origin, apiKey)` uses the same trusted origin and account key
  as the event sender. The run id is observed from `claim_task` mid-session (or seeded from
  the resumed claim), because the session claims inside itself.
