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

Requires Go 1.26+ (pre-release; build from source):

```sh
go install github.com/lecstor/clankerbar-cli/cmd/clankerbar@latest
# or, from a checkout:
go build -o clankerbar ./cmd/clankerbar
```

## Usage

```sh
clankerbar run --harness=claude
clankerbar run --harness=claude --model=opus --max-iterations=10
clankerbar run --config ./clankerbar.json
```

Control an in-flight run with markers in the state dir (`<workdir>/.clankerbar-loop`):

```sh
touch .clankerbar-loop/STOP     # stop gracefully after the current iteration
```

## Config

Flags override the config file, which overrides defaults. Default file locations:
`./clankerbar.json`, then `~/.config/clankerbar/config.json`. (JSON today; TOML is
the likely final format.)

```json
{
  "harness": "claude",
  "model": "opus",
  "prompt": "Work the backlog.",
  "mcp_config_path": "./clankerbar-mcp.json",
  "poll_interval": "30m",
  "budget": {
    "max_tokens": 0,
    "max_cost_usd": 0,
    "max_wall_clock": "6h"
  }
}
```

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
