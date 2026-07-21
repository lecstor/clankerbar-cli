# clankerbar

Drive a coding agent through your [clankerbar](https://clankerbar.com) backlog,
unattended. `clankerbar` respawns fresh harness sessions that drain the backlog
and **survives usage limits** — pausing on the cap and resuming (including on an
early reset) instead of dying until you come back.

It's a local, open-source client of the clankerbar control plane: the hosted plane
holds the state (your backlog, over MCP), this runs on your machine and drives your
coding agent. Because it holds your credentials and can edit your code and run
shell commands, the source is open so you can audit exactly what it does before you
trust it with an overnight run.

> **Status: work in progress.** The skeleton runs; the harness adapters and the
> limit/budget machinery are being hardened. Not yet released.

## How it works

Each iteration is a *fresh* harness session told to work the backlog (which it does
by dispatching tasks to subagents and landing PRs at `in_review`). When a session
ends, the driver spawns a new one — so context never rots across a long run, and a
session killed mid-task is fine: clankerbar reclaims and continues the task on the
next iteration. The backlog is the durable state; the loop is thin.

On a usage limit the loop doesn't die — it pauses and polls for the reset (catching
Anthropic's semi-random early resets), then continues.

## Install

**Download a prebuilt binary.** Each release ships cross-platform binaries
(macOS and Linux, amd64 + arm64) on the [releases page][releases] — grab the
archive for your OS/arch, extract, and put `clankerbar` on your `PATH`. No Go
toolchain required. Releases are semver tags (`vX.Y.Z`); the tool is pre-1.0, so
expect the `v0.x` line while the surface settles. Verify a download against the
`checksums.txt` published alongside it.

**Or build from source** (requires Go 1.26+):

```sh
go install github.com/lecstor/clankerbar-cli/cmd/clankerbar@latest
# or, from a checkout:
go build -o clankerbar ./cmd/clankerbar
```

[releases]: https://github.com/lecstor/clankerbar-cli/releases

## Usage

```sh
clankerbar run --harness=claude
clankerbar run --harness=claude --model=opus --max-iterations=10
clankerbar run --config ./clankerbar.json
```

Control an in-flight run with markers in the state dir (`<workdir>/.clankerbar-loop`):

```sh
touch .clankerbar-loop/STOP     # stop gracefully (responsive even mid-wait)
```

You can also **pause a run remotely from the clankerbar web console** (Settings →
Pause). A paused loop stops spawning new sessions and idle-polls until you Resume —
without exiting, and without killing an in-flight session (a pause is honoured
between iterations, like `STOP`). Remote pause rides on the driver's cheap backlog
read, so it needs a **wired poller**: a *project-scoped* `CLANKERBAR_API_KEY` (mint
one at `clankerbar.com/projects/<slug>/api-keys`) and a resolvable plane URL. With
only an account key or no endpoint the loop drains blind and can't see the flag —
the local `STOP`/`HALT` markers remain the fallback there.

### Visibility

The driver logs milestones to your terminal as it goes (timestamped): spawning,
queue state each poll, done + tokens/cost, usage-limit pauses, transient retries.
The Claude harness runs with `--output-format stream-json`, so the agent's own
progress — assistant text and `→ Tool` markers — streams live too, and each
attempt is captured to its own `<state-dir>/iteration-<ts>.log`.

### Skills, plugins, and auth

A headless session loads **project** skills from the working directory
(`.claude/skills/`) and **user/plugin** skills + auth from the config dir. Set
`config_dir` (→ `CLAUDE_CONFIG_DIR` / `CODEX_HOME`) so an unattended run loads the
*same* skills, plugins, and login as your interactive session — a bare cron
environment would otherwise have none of them.

## Config

Flags override the config file, which overrides defaults. Default file locations:
`./clankerbar.json`, then `~/.config/clankerbar/config.json`. (JSON today; TOML is
the likely final format.)

```json
{
  "harness": "claude",
  "model": "opus",
  "prompt": "Work the backlog.",
  "mcp_config_path": "./.mcp.json",
  "config_dir": "~/.claude",
  "idle_poll_interval": "60s",
  "poll_interval": "30m",
  "max_retries": 0,
  "retry_cap": "5m",
  "budget": {
    "max_tokens": 0,
    "max_cost_usd": 0,
    "max_wall_clock": "6h"
  }
}
```

### Resilience

A fresh session can die on a server blip (API 5xx / overloaded) or a network
hiccup — nothing to do with the task. These are **retried with exponential
backoff** (30s → 60s → … capped at `retry_cap`), re-running the same iteration; a
fresh session reclaims any half-done task, so a retry costs minutes, not work.
Detection is anchored (Claude's `API Error:` prefix, connection-error strings), so
a task log that merely *mentions* an HTTP 500 isn't mistaken for a dead session,
and a `400` still stops. `max_retries: 0` (the default) means **never give up** —
keep retrying at the ceiling until the API recovers, right for a daemon; set a
positive number to bound it. A usage-limit pause and a transient retry both re-run
the same iteration and neither advances the iteration count. `STOP` stays
responsive during any wait.

### Not getting locked out

No coding-agent harness lets a headless caller read its remaining subscription
quota, so `clankerbar` cannot stop at a precise "80% of the window". Two reliable
alternatives, in order of preference:

1. **Separate credentials** — run the loop under a *different* account / API key
   than your interactive agent use, so it physically cannot spend your daytime
   quota. This is the only can't-be-wrong option.
2. **Budget circuit breaker** — set `budget.max_tokens` / `max_cost_usd` /
   `max_wall_clock`; the loop self-accounts and stops early. Blunt but simple —
   tune it by watching a couple of runs.

## Harnesses

| Harness | Status |
|---|---|
| Claude Code (`claude`) | primary |
| Codex (`codex`) | adapter present; parsing being hardened |

New harnesses are a small adapter (`internal/harness`). Contributions welcome.

## License

MIT (see [LICENSE](./LICENSE)).
