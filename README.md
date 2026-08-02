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
clankerbar run --config ./clankerbar.json     # or: -c ./clankerbar.json
```

Flags are **GNU-style**: `--long` options, `-x` shorts. `--config` (`-c`) and
`--help` (`-h`) are the only short aliases; everything else is long-form only.
`clankerbar run --help` and `clankerbar doctor --help` list them. A short flag's
value is separate (`-c ./x.json`) or `=`-joined (`-c=./x.json`); the inline
`-c./x.json` form is rejected, so a typo like `-cofnig` cannot quietly become
`--config=ofnig`.

> **Breaking (pre-release).** A single dash now introduces *short* flags, so the
> Go-stdlib spelling `-harness claude` no longer works - use `--harness claude`.
> Anything invoking `clankerbar` with single-dash long options (a cron wrapper,
> a launchd plist, a shell alias) needs updating. The old spelling is rejected
> with a message naming the double-dash form rather than being quietly
> reinterpreted.

### Preflight: `clankerbar doctor`

Most of what goes wrong in an unattended run only shows up as *degraded
behaviour*, hours in — a rejected key silently drops the loop into blind mode, a
missing binary kills the first session, an unreachable plane looks exactly like an
empty queue. `doctor` turns all of that into one cheap answer before you start:

```sh
clankerbar doctor
clankerbar doctor --config ./clankerbar.json --harness codex   # or: -c ./clankerbar.json
clankerbar doctor && clankerbar run --harness=claude   # gate a cron wrapper
```

It prints one `PASS` / `WARN` / `FAIL` line per check, with a one-line remedy
under anything that isn't a PASS, and **exits non-zero if any check FAILs** — so
it composes with `&&` as above.

```
PASS  config       loaded ./clankerbar.json
                   harness: claude
                   workdir: /Users/you/dev
                   backlog: https://clankerbar.com/api/projects/acme/backlog-summary
PASS  harness      /usr/local/bin/claude (2.1.0)
WARN  config_dir   not set — the session inherits the ambient environment
                -> set config_dir (or --config-dir) so a cron/launchd run loads the same skills, plugins and auth as your terminal
WARN  backlog      https://clankerbar.com/api/projects/acme/backlog-summary — 0 claimable, 2 open question(s) — nothing to claim; the loop will idle without spawning
                -> answer the open question(s) at clankerbar.com, or expect an idle run
WARN  state_dir    /Users/you/dev/.clankerbar-loop has a leftover STOP marker
                -> delete it, or the loop stops immediately: rm /Users/you/dev/.clankerbar-loop/STOP
WARN  workdir[acme] /Users/you/dev has no agent-instructions file (AGENTS.md / CLAUDE.md)
                -> add one here naming each repo below and where its protocol lives — a session started in a multi-repo parent loads nothing from the repos under it
PASS  permissions  /Users/you/.config/clankerbar/headless.json parses
WARN  toolchains   no grant for: go (/Users/you/dev/acme-cli)
                -> allow the verbs each one needs in /Users/you/.config/clankerbar/headless.json (e.g. Bash(go build:*), Bash(go vet:*), Bash(go test:*)) — a headless session fails closed, so an ungranted tool is refused with no prompt and its task ships unverified
PASS  budget       no ceiling configured — the loop runs until the backlog is dry or it is stopped
```

The checks: **config** (discovered, parses, validates — plus the resolved harness,
workdir and derived backlog URLs), **harness** (binary on PATH and runnable, with
its version), **config_dir** (resolves, exists, looks initialised for the chosen
harness), **backlog** (creds present and the summary read succeeds — distinguishing
no creds, a rejected key, a `project_required` key/route mismatch, and an
unreachable endpoint — plus whether the queue is gated on *your* open questions,
or paused from the console), **state_dir** (the driver's own directory: writable,
no leftover `HALT`/`STOP`), **workdir** (per project: it resolves, an `.mcp.json`
reaches it, and it carries an agent-instructions file), **permissions**
(harness-specific policy sanity), **toolchains** (the build tools the project's
repos need are actually granted), and **budget** (ceilings parse and are sane).

A multi-project config gets **one backlog check and one workdir check per
project** — one queue can be wired wrong while the others are fine. A project
entry that omits `workdir` or `mcp_config_path` inherits the top-level one, and
doctor resolves it **exactly the way the loop does**, so it never reports on a
directory your sessions will not use.

The `toolchains` audit reads every settings file Claude merges — the `--settings`
policy, the config dir's `settings.json`/`settings.local.json`, and each session
workdir's `.claude/settings.json`/`.claude/settings.local.json`. A *narrow* deny
(`Bash(go run:*)` alongside an allowed `Bash(go test:*)`) is reported as a hole in
an otherwise-granted tool, not as a blocked one — only a bare `Bash(go:*)` deny
means the toolchain is unusable.

Three of those exist because of failure modes that cost a real overnight run
whole iterations:

- **The queue said there was work and there wasn't.** A ready task gated on an
  unanswered question still counts as claimable, so the loop spawns, the session
  correctly declines to pre-empt your decision, and you pay for that report ten
  times. Preflight now says so before the window opens.
- **A session started in a multi-repo parent reads nothing.** A harness loads its
  instruction file, skills and project settings from the session's cwd *and
  upward* — never from the repos below it. Spawn in `~/dev` and every session
  begins by rediscovering the layout, with none of your conventions.
- **An ungranted toolchain does not stop a run, it un-verifies one.** A headless
  session fails closed, so `go test` that was never allowed is refused with no
  prompt reaching you — and the task ships written, pushed and never compiled.

WARN vs FAIL is the "would this still make progress?" line: no creds and an
unreachable plane WARN, because the loop drains blind and still gets work done; a
rejected key and a key/route mismatch FAIL, because they never self-heal and every
session the loop spawns carries the same broken credential.

Control an in-flight run with markers in the state dir (`<workdir>/.clankerbar-loop`):

```sh
touch .clankerbar-loop/STOP     # stop gracefully (responsive even mid-wait)
```

You can also **pause a run remotely from the clankerbar web console** (Settings →
Pause) — per project. A paused project stops getting new sessions (other projects
keep draining) and the loop idle-polls until you Resume — without exiting, and
without killing an in-flight session (a pause is honoured between iterations, like
`STOP`). Remote pause rides on the driver's cheap backlog read, so it needs a
**wired poller**: a `CLANKERBAR_API_KEY` and a resolvable plane URL. Your
**account key** (mint at `clankerbar.com/account/api-keys`) is the normal choice —
the driver polls the project-in-path summary route whenever it knows the slug, which
it derives from your `.mcp.json`'s `/mcp/<slug>` URL or from the `projects` config
(below). A *project-scoped* key works too (the CI-style setup). With no creds or no
resolvable endpoint the loop drains blind and can't see the flag — the local
`STOP`/`HALT` markers remain the fallback there. Two misconfigurations hard-stop
(non-zero exit) instead of blind-draining doomed sessions: a revoked/wrong key
(`401/403`), and an account key with **no derivable project slug** (`400
project_required` from the legacy slug-less route) — give the loop a slug via
`projects` or an `/mcp/<slug>` `.mcp.json`.

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

`mcp_config_path` defaults to `<workdir>/.mcp.json` when that file exists — Claude's
headless mode does not auto-discover it, so the default is what gives spawned
sessions their clankerbar tools (and gives the driver a project slug to poll with).

### Multi-project: one instance, many queues

One loop instance can drive **several projects** with a single **account-scoped**
key — you never need an instance per project, or per-project keys. Declare the
projects; the driver polls each queue and round-robins sessions across whichever
have claimable work, each session spawning in its own project's workdir (whose
`.mcp.json` names `/mcp/<slug>`, selecting that project's tools):

```json
{
  "harness": "claude",
  "projects": [
    { "slug": "clankerbar", "workdir": "~/dev" },
    { "slug": "ezyapp", "workdir": "~/work/ezyapp" }
  ]
}
```

Per entry: `slug` (required, the `<slug>` in `/mcp/<slug>`), `workdir`, and
optionally `mcp_config_path` (defaults to `<workdir>/.mcp.json`). Budgets,
`max_iterations`, and the `STOP`/`HALT` markers stay **instance-global** — one
operator, one spend pool; the console pause is per project. With no `projects`
list, the top-level fields drive a single project exactly as before.

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
