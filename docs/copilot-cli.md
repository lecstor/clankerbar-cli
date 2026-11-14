# Driving GitHub Copilot CLI headless

The `copilot` adapter drives GitHub Copilot CLI's non-interactive mode. Everything
here was verified against **Copilot CLI 1.0.112** on Linux; it is the reference for
the flags and event shapes `internal/harness/copilot.go` depends on, and for what an
operator has to configure.

## The session

```
copilot --output-format json --no-ask-user --no-auto-update --no-color \
        --disable-builtin-mcps [--model <alias>] \
        [--additional-mcp-config @<mcp_config_path>] [--add-dir <dir>]... \
        --allow-tool read --allow-tool write --allow-tool shell \
        --allow-tool clankerbar \
        -p "<prompt>"
```

- `-p/--prompt` is one non-interactive session; the CLI exits when it is done.
- `--output-format json` makes stdout a JSONL event stream. `assistant.message`
  carries the final answer (`data.content`; the last one is `phase: "final_answer"`),
  `tool.execution_*` carries tool calls, and `session.usage_checkpoint` carries the
  session's usage.
- The prompt is the VALUE of `-p`, never a positional argument, so a prompt that
  starts with `-` cannot be read as a flag.

## Permissions are fail-closed by default

In `-p` mode a tool request that would prompt is **denied** (there is nobody to
ask). `--allow-tool` / `--deny-tool` open the parts of the working set a session
needs; `--deny-tool` wins over every allow, including `--allow-all`.

The session environment pins `COPILOT_ALLOW_ALL=false` and
`COPILOT_ASSISTED_APPROVAL=false` AFTER any declared `env`, so an ambient (or
config-declared) approval shortcut cannot reopen the posture: `COPILOT_ALLOW_ALL=true`
approves every tool and trusts the folder, and the assisted judge is probabilistic
and explicitly not a security boundary. A bare server name is a valid allow rule
(`--allow-tool clankerbar` granted all of /mcp's tools in a verified session).

The adapter grants `read`, `write`, `shell` and the `clankerbar` MCP server for a
working session. The file boundary is the CLI's own path verification (the working
directory, the system temp dir, and each `--add-dir`), and it applies to shell
commands as well as file tools: verified on 1.0.112, a `view` of `/etc/hostname` AND
a `cat /etc/hostname` were both refused under `--allow-tool read --allow-tool shell`,
"Permission denied because no interactive user response was available", while an
in-workdir write and shell command succeeded. An out-of-boundary path cannot prompt
in `-p` mode, so it fails closed. A **probe** is read-only: it denies
`write` and `shell` outright (read-only shell commands would otherwise run without
asking); when `mcp_config_path` is set its only grant is
`clankerbar(get_backlog_summary)`, and without one it has no tool grants and asks
for the bare `.` prompt.

`--disable-builtin-mcps` drops the built-in GitHub MCP server. Its tools are not in
the allow policy, so in `-p` mode they would be offered to the model and then
denied; a session's MCP servers are the operator's `mcp_config_path` and nothing
else.

## MCP servers

`--additional-mcp-config @<file>` registers a file for one session, in the same
`mcpServers` schema Claude Code uses:

```json
{
  "mcpServers": {
    "clankerbar": {
      "type": "http",
      "url": "https://clankerbar.com/mcp/clankerbar",
      "headers": { "Authorization": "Bearer ${CLANKERBAR_API_KEY}" }
    }
  }
}
```

This is why the adapter reports `mcp_config_path` as Claude's schema: the object is
the one it reads. `${VAR}` expansion inside the file works (verified: the server
connected with 37 tools and a real `get_backlog_summary` call succeeded).

The server must be named **`clankerbar`**. The work-session allow rule
(`--allow-tool clankerbar`), the probe's single grant and the claim observer's name
mapping all key on that name — the same convention the claude and opencode adapters
use. A file whose server is named something else still connects, but its tools are
not in the appended policy, so in `-p` mode the session is denied them (fail
closed).

A workspace `.mcp.json` is auto-discovered **only once the folder is trusted**, and
`--additional-mcp-config` is what makes the file reach the session regardless. The
adapter never trusts a folder: trusting one also approves that directory's skills,
plugins, MCP servers and hooks (including hooks that run shell commands).

## Claim observation

The adapter feeds Copilot's tool requests (`assistant.message.toolRequests` and
`tool.execution_start`) and their results (`tool.execution_complete`) to the same
shared clankerbar observer claude and opencode use, mapping an MCP call
(`mcpServerName: "clankerbar"`, `mcpToolName: "claim_task"`) onto the canonical
`mcp__clankerbar__claim_task` name. So `Result.Claim`, `Reports`, `Settled` and
`ClankerbarCalls` are populated and a phased config can run on this harness. The
request/result plumbing was verified live (a real `get_backlog_summary` call was
observed); the claim payload itself is the plane's own JSON, which the same
observer already parses for the other two adapters. The claim/settle paths are
pinned only by synthetic event shapes and the shared observer: no captured or
live `claim_task` round-trip exists yet, so the first real phased run's claim is
the remaining check on this wiring.

## Usage and billing

Copilot moved to usage-based AI-credit billing on 2026-06-01: a monthly allowance of
AI credits, then per-token overage. It is **metered**, not a rolling-window
subscription, so there is no 5-hour/weekly window to wait out and the adapter never
enters the supervised wait.

`session.usage_checkpoint.data.accountingSnapshot` is the session's cumulative
report. `tokenDetails` is already disjoint — `input` excludes `cache_read` and
`cache_write` — so the adapter copies the four buckets into `Result.Usage` and sums
them into `Result.Tokens`. `modelMetrics.*.usage.inputTokens` is **inclusive** of the
cache buckets; that path is only used as a fallback when `tokenDetails` is absent,
and subtracts them. The CLI's `reasoningTokens` is folded into output; it is kept in
`Raw["reasoning_tokens"]` for diagnostics rather than invented as a fifth bucket.

`totalNanoAiu` is AI credits × 10⁹ (10⁹ nano-AIU = 1 credit). It is **not money**:
a credit's dollar value depends on the plan and on whether the call is inside the
included allowance. `Result.CostUSD` therefore stays zero and
`Capabilities.ReportsCost` is false — `budget.max_cost_usd` is inert under this
harness, and `budget.max_tokens` (fed by the checkpoint) is the breaker.

## Limits

`DetectLimit` recognises the CLI's documented CREDIT/ALLOWANCE exhaustion
statements (`copilot help billing`, `copilot help limits`):

- plan allowance exhausted — `Plan: 100% used, resets <date>`, "out of AI credits",
  "quota exceeded";
- the session AI-credit cap — "Session limit reached".

Both are `Limit.Stop` (`budget_exhausted`): the run stops cleanly and writes the
`LIMIT_STOP` marker (`docs/limit-stop.md`) for the operator to resume after
topping up.

The CLI's session/weekly/model **rate limits** are NOT a stop. They are short
rolling windows with a stated reset — the same kind of provider blip that arrives
as a bare 429 — so they fall through to `IsTransient` and are retried. Stopping a
whole run on "You've hit the rate limit for this model" would end it on a wait it
could have taken.

The scope is the CLI's own text — stderr, non-JSON stdout lines, and error events —
never `assistant.message` content or a tool result. That is what stops a backlog
body quoting "Plan: 100% used" from tripping a stop. The CLI's 75%-used quota notice
is a `session.warning` and is deliberately out of scope: it is not a limit.

## What the adapter does not provide

- **No turn cap.** `Invocation.MaxTurns` has no Copilot CLI equivalent. The phase
  backstop is `max_session_wall_clock`, which the adapter enforces itself.
- **No idle watchdog or repeated-tool-failure guard.** `max_session_idle` and
  `max_repeated_tool_failures` are inert for this harness.
- **No multi-turn sequences.** `Invocation.Turns` is refused; the CLI can resume a
  session (`--resume` / `--session-id`) but the adapter does not use it yet.
- **No headless account usage.** `/usage` is TUI-only; `ReadUsage` returns
  `ErrUsageUnsupported`. `--usage-output-file` writes the same session figures the
  JSONL checkpoint carries.

## Reading the output

`internal/harness/testdata/copilot/mcp-smoke.jsonl` is a captured run (fixture for
the parser tests), and `model-unavailable.jsonl` is a captured real failure. To
capture another, run the command above with a scratch `COPILOT_HOME` and keep the
stdout.

The quickest live check that the wiring is intact is the opt-in smoke, which runs
ONE real read-only session through the adapter (a paid call, ~0.2 AI credits):

```sh
CLANKERBAR_COPILOT_LIVE=1 CLANKERBAR_COPILOT_MCP=$PWD/.mcp.json \
  go test ./internal/harness -run TestCopilotLiveSmoke -v
```

It asserts that `--additional-mcp-config` loaded the server, that the
`${CLANKERBAR_API_KEY}` header expanded (a real `get_backlog_summary` result), that
the call was observed, that usage was reported, and that the read-only posture
wrote nothing. The probe records every server's status in
`Result.Raw["mcp_servers"]`.
