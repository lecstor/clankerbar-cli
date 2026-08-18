# clankerbar

Drive a coding agent through your [clankerbar](https://clankerbar.com) backlog,
unattended. `clankerbar` respawns fresh harness sessions that work the backlog one
task at a time, and **survives usage limits** — pausing on the cap and resuming
(including on an early reset) instead of dying until you come back.

It's a local, open-source client of the clankerbar control plane: the hosted plane
holds the state (your backlog, over MCP), this runs on your machine and drives your
coding agent. Because it holds your credentials and can edit your code and run
shell commands, the source is open so you can audit exactly what it does before you
trust it with an overnight run.

> **Status: pre-1.0, released.** The loop runs and is what drives clankerbar's own
> backlog; the harness adapters and the limit/budget machinery are still being
> hardened. It is a supported surface rather than an internal tool - see
> [Versioning](#versioning) for what the `v0.x` line does and does not promise.

## How it works

Each iteration is a *fresh* harness session told to work **one** task, landing a PR
at `in_review`. That is the `prompt` knob, and the wording is exact: the served
protocol reads "work the next backlog item" as one task and "work the backlog" as
*drain the whole ready queue in this session*. One task per iteration is the default
because it is what keeps a session's context bounded no matter how long the queue
is, and because the operator's controls — console pause, `HALT`, `max_iterations`,
the budget breaker — are consulted between iterations, so a shorter iteration is a
shorter wait for them to bite. Set `prompt` to the drain phrase if you want the old
behaviour, and expect both properties to go with it.

When a session ends, the driver spawns a new one — so context never rots across a
long run, and a session killed mid-task is fine: the driver hands the task straight back to the
queue (see [Resilience](#resilience)) and the next iteration picks it up. The
backlog is the durable state; the loop is thin.

### Phases: splitting one task across two sessions

A session's context grows monotonically and is re-read on every turn, so a long
task gets expensive at the *end*: one measured task spent 66.2M tokens over 370
turns, and its last four deciles were 53% of that purely because each of those
turns re-read a quarter of a million tokens. The served protocol already tells a
session to shed its context at a safe checkpoint, and says in the same breath
that the rule is a no-op if the harness cannot discard context. Inside a `claude
-p` run there is no tool and no slash command **the model** can use to discard
its own context, so for the agent that rule is dead letter.

The driver does have options, and they were evaluated rather than assumed away
(claude 2.1.226): `--autocompact <auto|tokens>` compacts at a *threshold*, and
`--continue` / `--resume` / `--fork-session` restore a session rather than
shedding one. A phase boundary was chosen over compaction because it lands where
everything load-bearing is already durable somewhere else — code pushed, bar and
standing decisions on the plane — so the next phase re-reads them from source,
where a summary that garbled the bar would not announce itself. It is also
harness-agnostic, where `--autocompact` is Claude-only. The two compose, and
trying autocompact inside a long phase costs nothing.

`phases` splits one task across several sessions, so the context resets at that
checkpoint:

```json
"phases": [{ "name": "implement" }, { "name": "review" }]
```

Phase 1 claims the task, implements it, self-verifies, commits, pushes and
records the branch — then stops. Phase 2 starts on a **fresh context**, resumes
the *same* run (the driver substitutes the task and run ids into its brief, so it
calls `heartbeat` instead of claiming), re-reads the bar and the standing
decisions from the plane, runs the adversarial review, fixes what it finds and
hands the task to `in_review`. The claim is held across the seam, so the task is
never posted back to the queue mid-sequence.

The split works because each phase's prompt is a **scope** instruction — "your
job this session is implementation only, then stop" — rather than a request that
the model manage its own context. Naming a phase takes its built-in brief; set
`prompt` on it to write your own, and `max_turns` to cap it as a backstop for a
session that works past its brief (whatever it leaves uncommitted is then caught
by the salvage). `prompt` and `phases` are mutually exclusive.

**It is opt-in, and worth understanding before switching it on.** A task that
reaches its checkpoint and stops is only half finished until the next session
runs, so this trades a driver that cannot lose a task for one that can leave it
half-done if the sequence is interrupted. The budget breaker fires at the phase
boundary too. Two other consequences worth knowing:

- **`max_iterations` counts task sequences, not sessions.** A two-phase config
  spawns roughly twice the sessions for the same limit.
- **A multi-phase config requires a harness that observes the session's task
  claim**, which today means `claude` or `opencode`. The handback across a seam,
  the salvage and the delivery check all depend on it, so a config naming `codex`
  alongside two or more phases is refused at validation rather than quietly
  stopping after phase 1 on every task. A single phase hands off to nobody, so it
  is allowed anywhere.
- **`max_turns` is a `claude`-only backstop.** `opencode` can now run phases, but
  `Invocation.MaxTurns` never reaches its CLI and it has no mid-stream token
  ceiling either, so a phased `opencode` config has no per-session cap at all —
  only the budget breaker at the phase boundary and `max_wall_clock`. Set those
  deliberately rather than assuming the `max_turns` you wrote is doing anything.
- **A sequence that ends on the `implement` brief is refused**, on any harness:
  that brief tells its session to stop at the checkpoint and leave `in_review`
  alone, so with no phase after it every task would stop half-finished, forever,
  with nothing in the logs reading as an error.

The saving is **projected** at 20-28% for a two-way cut — modelled off one real
task's decile curve, not measured from a phased run, and stated that way
deliberately until a phased run has been measured. Splitting thinner earns less
each time while every extra boundary still pays a session's full startup cost.

### Session-initiated handoff: a boundary the session chooses

Phases cut at two fixed, driver-chosen points. A session can also choose its own
cut: ending its **final message** with a marker line followed by a prompt makes
the driver respawn a fresh session on that prompt, prefixed by driver-authored
framing that carries the same "resume this run, do not claim" contract a phase
resume gets from its brief — same task, same run-continuity rules as a phase
resume — instead of moving on. The built-in
briefs teach the mechanism and its trigger: hand off at a genuine *pivot*
(exploration finished and implementation about to start; one sub-goal landed and
the next beginning), writing the successor's prompt the way an operator's
continuation prompt reads — decisions made, state verified, exact next steps,
nothing about the journey. Most tasks need zero handoffs.

A session-authored respawn prompt is self-directed prompt injection, so it is
honoured only inside three guards: **each respawn consumes an iteration** of
`max_iterations` (the one exception to "counts task sequences") and the budget
breaker runs before the respawn ever spawns; an **over-size prompt** (over 4KB)
is refused with a logged fallback to the normal path, never truncated; and a
**chain of consecutive handoffs is capped** at 3 per phase, after which the
sequence falls back to the standard brief; `max_iterations` and the budget
still bound the total across every phase. Every respawn is named in the daemon
log and tagged in its iteration log's filename (`-h1`, `-h2`, ...), so the
chain reads the way phases do.

### The independence the split also buys

Context economy is why phases were built, but it is not the only thing they are
worth. Before phases, one session implemented a task, dispatched its own reviewer
and applied the fixes: **the thing being reviewed wrote the reviewer's brief.** An
author naming what to attack frames away from its own weak spots without meaning
to — measured on one task, a hand-written brief named three things to attack, all
three came back fine, and the real defect the review found was something the brief
never pointed at.

A phase boundary fixes that structurally rather than by asking anyone to be
fair-minded: **the driver is the caller, so the implementing session cannot write
the reviewing session's prompt.** Phase N+1's brief is built from your config plus
exactly two values off the held claim — the task id and the run id — and phase N's
session output is not an input to it at all. A test pins that absence rather than
the wording of the shipped brief, because the wording is the part that is allowed
to change.

The second phase also puts the review behind the checkpoint, which is where a
session's context peaks and so where a usage limit or a crash is most likely to
land. Before the split, a death there destroyed the findings and the half-applied
fixes together; now it costs the review, never the implementation.

### Per-phase models: a tier map you own

`model` pins one model alias for the whole run. `models` and a phase's `tier` let
different phases run on different models:

```json
"models": { "strong": "opus", "standard": "sonnet", "cheap": "haiku" },
"phases": [
  { "name": "implement", "tier": "strong" },
  { "name": "review",    "tier": "strong" }
]
```

**The buckets are yours, and the indirection is the point.** clankerbar drives
three harnesses and cannot know which models you have, nor which of them is
cheaper — that needs a price table, which goes stale silently and in the direction
that costs money. So it never learns what models exist; it learns only that you
bucketed them, and a phase names a **bucket**, never an alias. (`"tier": "opus"`
does not pin opus. It looks for a bucket called `opus`.)

**Everything falls back, and nothing here can stop a run.** A phase with no
`tier` runs on `model`; a `tier` naming a bucket you never defined runs on `model`
and says so in the iteration log; `model` itself empty means the harness picks, as
it always has. A config written before any of this existed behaves exactly as it
did. A mistyped bucket costs a log line at 3am, not a stopped drain.

**Which way to tier is a question of blast radius, not difficulty.** A phase that
produces a durable artifact — code, a recorded decision, a bar someone will be
measured against — is worth the strong bucket, because a mis-recorded decision is
read for months by people who cannot see it was wrong. A phase that only
compresses a large output into a small answer can take a cheap one, because the
error is visible immediately to whoever asked the closed question. Nothing is
tiered by default: an untouched config runs every phase on the run's model, and
these two shipped phases both produce durable artifacts.

### Per-phase harnesses: implement on one, review on another

A phase can name the **harness** it runs on, not only the model:

```json
"harness": "claude",
"harnesses": {
  "opencode": {
    "config_dir": "~/.config/opencode",
    "mcp_config_path": "~/.config/clankerbar/opencode-mcp.json"
  }
},
"phases": [
  { "name": "implement", "harness": "opencode" },
  { "name": "review" }
]
```

That runs the implementation on a cheap provider-agnostic backend and keeps the
adversarial review on the harness with the subagent machinery. Nothing in the
seam resists it: each phase is already a fresh session seeded from the observed
claim, so what crosses the boundary is a task id and a run id, and both live on
the plane rather than in a session.

**The harness-shaped fields stop being run-wide, and that is the whole of the
configuration.** `config_dir`, `mcp_config_path`, `settings_path`, `model` and
`models` at the top level describe your `harness` and no other, because each is a
dialect: `config_dir` is `CLAUDE_CONFIG_DIR` for one adapter and
`OPENCODE_CONFIG_DIR` for another, `mcp_config_path` is Claude's `.mcp.json` for
one and an opencode-schema config for another — and opencode does not ignore the
difference, it **refuses to start** on `mcpServers`. So a phase on another
harness inherits none of them and reads its own `harnesses.<name>` block instead.
A phase naming a harness with no block is refused at validation, rather than
spawning a session with no clankerbar tools that looks in the log like a model
that declined the work.

**Tiers are resolved per harness.** A phase's `tier` names a bucket in the tier
map of *the harness that phase runs on*, so one `"tier": "strong"` can mean opus
here and something else there — the bucket name is your policy and travels, the
alias inside it is a provider's and does not. A harness whose block names no
models runs on that harness's own configured default, which is where opencode's
model lives; a claude alias is never handed to another harness's `--model`.

**The first phase claims, so the rule about claim tracking is about it.** A
multi-phase sequence needs the *claiming* phase's harness to observe the
session's task claim (`claude` or `opencode`); later phases are handed that claim
and never observe one, so they can run anywhere. Validation says which phase and
which harness when it refuses.

**Multi-project runs need one MCP file per project per harness.** That file
carries two facts at once — which project (its `/mcp/<slug>`) and which schema
(the harness's) — so a `projects` entry declares them under `mcp_config_paths`:

```json
"projects": [
  { "slug": "clankerbar", "workdir": "~/dev",
    "mcp_config_paths": { "opencode": "~/.config/clankerbar/opencode-clankerbar.json" } }
]
```

Omitting one is refused rather than resolved: the single top-level
`harnesses.<name>.mcp_config_path` names one project, so falling back to it would
poll one queue while sessions worked another. `doctor` reports each
project-and-harness pair separately, along with each harness's binary, config dir
and permission policy.

On a usage limit the loop doesn't die — it pauses and polls for the reset (catching
Anthropic's semi-random early resets), then continues.

## Install

**Download a prebuilt binary.** Each release ships cross-platform binaries on the
[releases page][releases] - macOS and Linux, amd64 and arm64, as
`clankerbar_<version>_<os>_<arch>.tar.gz`. Extract it and put `clankerbar` on your
`PATH`. No Go toolchain required.

```sh
# macOS arm64; swap the os/arch for yours.
VERSION=0.1.0 OSARCH=darwin_arm64
curl -fsSLO "https://github.com/lecstor/clankerbar-cli/releases/download/v${VERSION}/clankerbar_${VERSION}_${OSARCH}.tar.gz"
curl -fsSLO "https://github.com/lecstor/clankerbar-cli/releases/download/v${VERSION}/checksums.txt"
shasum -a 256 --ignore-missing -c checksums.txt   # Linux: sha256sum --ignore-missing -c checksums.txt
tar -xzf "clankerbar_${VERSION}_${OSARCH}.tar.gz"
./clankerbar version
```

**Then verify where it came from, not just that it arrived intact.** The checksum
above catches a corrupt or truncated download, and that is all it catches: it is
served from the same release page as the archive, so anyone able to replace one
could replace both. Every release is also signed with [build provenance][slsa] -
a Sigstore attestation held by GitHub, not by the release, recording that these
exact bytes came out of this repo's `release.yml` at a specific commit:

```sh
gh attestation verify "clankerbar_${VERSION}_${OSARCH}.tar.gz" \
  --repo lecstor/clankerbar-cli \
  --signer-workflow lecstor/clankerbar-cli/.github/workflows/release.yml
```

`--signer-workflow` is the half that makes the sentence above true: `--repo`
alone accepts an attestation signed by *any* workflow in this repo, while naming
the workflow pins it to the one that publishes releases. That is worth the extra
line - this binary holds your credentials and runs shell commands on your
machine, and provenance is the part that would notice if the archive were not
ours. Needs the [GitHub CLI][gh-cli] and a `gh auth login`; run
`gh attestation verify --help` for the offline and JSON-output variants.

[slsa]: https://slsa.dev/spec/v1.0/provenance
[gh-cli]: https://cli.github.com

Downloading in a browser instead of with `curl` gets the archive quarantined by
macOS, and the binaries are not notarized, so the first run is refused with
"cannot be opened because the developer cannot be verified". Clear it with
`xattr -d com.apple.quarantine clankerbar`, or use the `curl` route above, which
never sets the attribute.

**Or build from source** (requires Go 1.26+):

```sh
go install github.com/lecstor/clankerbar-cli/cmd/clankerbar@latest
# or, from a checkout:
go build -o clankerbar ./cmd/clankerbar
```

A source build reports the version it was stamped with - `go install` leaves it at
the `0.0.0-dev` default, so `clankerbar version` naming a real `vX.Y.Z` means you
are on a release archive.

[releases]: https://github.com/lecstor/clankerbar-cli/releases

## Versioning

Releases are semver tags (`vX.Y.Z`) and the tool is pre-1.0, so expect the `v0.x`
line while the surface settles. Concretely, while pre-1.0:

- **A breaking change can land on a minor bump** (`v0.1` -> `v0.2`). That is what
  the `v0.x` line means; pin a version if you are automating against it.
- **A breaking change is never silent.** Permitted is not the same as unannounced:
  breaking commits carry a `!` (`feat(cli)!: ...`) and the release config hoists
  them into a **Breaking changes** heading at the top of the notes, rather than
  leaving them as one line among thirty.
- **Releases are cut when there is something to ship**, not on a schedule. The
  commitment is that a tag produces working, downloadable binaries.

Those promises harden at v1.0.

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

> **If you built from a checkout before v0.1.0**, this changed under you. A single
> dash now introduces *short* flags, so the Go-stdlib spelling `-harness claude` no
> longer works - use `--harness claude`. Anything invoking `clankerbar` with
> single-dash long options (a cron wrapper, a launchd plist, a shell alias) needs
> updating. The old spelling is rejected with a message naming the double-dash form
> rather than being quietly reinterpreted. v0.1.0 is the first release, so it ships
> the GNU-style spelling from the start.

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
WARN  state_dir    /Users/you/.local/state/clankerbar/loop/dev-9f70ef211d1e0549 has a leftover STOP marker
                -> delete it, or the loop stops immediately: rm /Users/you/.local/state/clankerbar/loop/dev-9f70ef211d1e0549/STOP
WARN  workdir[acme] /Users/you/dev has no agent-instructions file (AGENTS.md / CLAUDE.md)
                -> add one here naming each repo below and where its protocol lives — a session started in a multi-repo parent loads nothing from the repos under it
PASS  permissions  /Users/you/.config/clankerbar/headless.json parses
WARN  toolchains   no grant for: go (/Users/you/dev/acme-cli)
                -> allow the verbs each one needs in /Users/you/.config/clankerbar/headless.json (e.g. Bash(go build:*), Bash(go vet:*), Bash(go test:*)) — a headless session fails closed, so an ungranted tool is refused with no prompt and its task ships unverified
PASS  budget       no ceiling configured — the loop stops on a STOP/HALT marker or signal; a dry backlog idle-polls rather than exiting
```

The checks: **config** (discovered, parses, validates — plus the resolved harness,
workdir and derived backlog URLs), **harness** (binary on PATH and runnable, with
its version), **config_dir** (resolves, exists, looks initialised for the chosen
harness), **backlog** (creds present and the summary read succeeds — distinguishing
no creds, a rejected key, a `project_required` key/route mismatch, and an
unreachable endpoint — plus whether the queue is gated on *your* open questions,
or paused from the console), **state_dir** (the driver's own directory: writable,
no leftover `HALT`/`STOP`, and not sitting inside a configured workdir - a state
dir under one a session is spawned in is writable by that session, which hands it
the loop's own `STOP`/`HALT` switch; under a workdir nothing runs in *yet* it is
the same trap for the next `projects[]` entry that inherits it), **workdir** (per
project: it resolves, an `.mcp.json`
reaches it, and it carries an agent-instructions file), **permissions**
(harness-specific policy sanity), **toolchains** (the build tools the project's
repos need are actually granted), **power** (whether the machine will stay awake
long enough to do the work), and **budget** (ceilings parse and are sane).

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
- **A sleeping machine does not pause a run, it freezes one.** Timers use the
  monotonic clock, which does not advance while the machine is suspended, so a
  wait stops mid-flight and the loop goes silent exactly like a hang. One run lost
  5h31m of a 10h window this way, waking only in 45-second Power Nap bursts.

### Sleep, on laptops

`clankerbar run` now holds a no-idle-sleep assertion for its own lifetime (macOS;
it dies with the process, so nothing outlives the run). Two caveats it cannot fix,
both of which have cost a real run its night:

- **A closed lid sleeps regardless.** No assertion overrides clamshell sleep.
- **Start the run on AC.** Plugging in later does *not* wake a sleeping Mac, so a
  machine that idle-slept on battery stays down even once power arrives — its
  never-idle-sleep-on-AC setting never gets a chance to apply.

When a wait does get frozen, the loop now says so on the way out, rather than
leaving an unexplained gap in the log:

```
wait of 30m0s took 2h28m11s of wall clock — the machine was suspended for ~1h58m11s;
timers are frozen while it sleeps, so an unattended run stalls silently
```

### When the queue lies about having work

`claimable > 0` means work is *available*, not that it can be *done*. A task gated
on an unanswered question is claimable and unworkable: the loop spawns, the session
correctly declines to pre-empt your decision, and you pay for that report every
cycle. One run did it ten times.

So after **three consecutive sessions that settle nothing** — nothing reaching
`in_review` or `done` — the loop backs that project off for 15 minutes, then 30,
then an hour, capped at two. Other projects keep draining.

So does the queue simply going quiet. A poll that shows nothing to spawn for at all
means the project is *idle*, not fruitless — there is nothing for the loop to
spawn, so it has not failed at anything — and the count is forgotten along with
any wait still running. Without that, parking the blocker would leave the count
standing (`parked` is not progress a backlog can see), and the next task you file
would serve out a two-hour wait it did nothing to earn.

Note "nothing to spawn for" rather than "nothing claimable": abandoned work counts
as something to spawn for. A project can hold a branch whose session died, and the
sweep that hands it back runs only when an agent asks the plane for its next task
— so if the loop declined to spawn while the ready queue was empty, nothing would
ever ask, and the branch would sit there for good. The gate is `claimable +
stale_claimable`, both of which the queue line reports.

**What bounds a recovery that never finishes is the plane, not this back-off.**
Taking a branch over renews its lease, so the task stops being offerable until the
lease lapses again — and the poll in between reads as idle and forgets the strike.
The count therefore does *not* climb across successive takeovers of one branch, and
the local back-off will usually never reach its threshold. That is deliberate: the
plane counts hand-offs per task, and once a branch has defeated its allowance it is
**parked and raised to you as a question** instead of being offered again, which
drops it out of `stale_claimable` for good. So the spend is bounded per branch by
the plane, at a session or so per lease period, rather than by three strikes here.
The local back-off still bites the case it was built for — a target that keeps
being offered the *same* unfinishable work without the lease resetting.

Three rather than one, because a genuinely large task can span several sessions
before anything reaches a reviewer, and backing off then would throttle exactly
the deep work the loop exists to do.

**Answering the question ends the wait, and so does any other progress**: you
merging a PR, another machine's loop settling something. Both are seen on the next
poll, so "immediately" means within one `idle_poll_interval` (60s by default), not
sooner. The answer is the one worth spelling out, because it is what the back-off
message asks you to go and do: an answer moves neither `in_review` nor `done`, so
a falling open-question count is the only trace of it the loop can see, and it
watches both signals for that reason. Answer while a session happens to be running
and the loop gives that project one immediate retry rather than the next rung of
the ladder. That session still settled nothing, and its strike stands.

WARN vs FAIL is the "would this still make progress?" line: no creds and an
unreachable plane WARN, because the loop drains blind and still gets work done; a
rejected key and a key/route mismatch FAIL, because they never self-heal and every
session the loop spawns carries the same broken credential.

Control an in-flight run with markers in the state dir, which lives OUTSIDE the
workdir - `$XDG_STATE_HOME/clankerbar/loop/<workdir-slug>`, i.e.
`~/.local/state/clankerbar/loop/<workdir-slug>` by default. `doctor` prints the
resolved path:

```sh
touch ~/.local/state/clankerbar/loop/dev-9f70ef211d1e0549/STOP   # stop gracefully (responsive even mid-wait)
```

An explicit `state_dir` wins, but pointing it back inside a workdir gives every
session spawned there the ability to write these markers. `doctor` WARNs whenever
the resolved state dir sits inside a configured workdir - including the default,
if your workdir happens to contain `~/.local/state`.

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

### Checking what a session says it delivered

The control plane holds the backlog and takes a clanker at its word. When a session
records a branch (`update_task(branch: …)`) or declares a delivery merged
(`delivery.commit` + `integrationBranch`), nothing on the plane can look at a
repository to see whether either is true. One task read `done` for four days while
about 900 lines of its work sat unpushed on one laptop, and its own PR merged a
stale snapshot.

The driver is already local and already in the git tree, so it checks both claims
after each session — no new credentials, no plane change:

- **A recorded branch is really on the remote**, and the local tip is an ancestor
  of the remote tip. A local branch *ahead* of its remote is the failure above.
- **A declared delivery really landed**: `commit` is an ancestor of the remote tip
  of `integrationBranch` — the same ancestor check the plane asks the clanker to
  attest to, run rather than trusted. The integration branch is read from the
  remote, never from a local `main` that is routinely tens of commits stale. Only
  a *closure* is checked this way: a hand-off to `in_review` carries the delivery
  it is proposing, and at that moment nothing has merged yet. And only a commit
  **id** counts — a revision expression like `main` or `HEAD` is trivially its own
  ancestor, so it is reported as unverifiable rather than checked.

```
DELIVERY UNVERIFIED — CLA-134: branch "clanker/x" is ahead of origin/clanker/x by
12 commits — local tip 4a91c0de, remote 0b3f21aa; that work is UNPUSHED and
exists only in /Users/you/dev/acme
```

**It warns; it does not refuse.** A failed check is logged loudly and names what is
unpushed or unmerged, but the session's own report stands — the driver does not
revert a status a session chose on the strength of a check that could be looking at
the wrong tree. When the merge check *ran*, its verdict is written back to the task
as `delivery.mergeVerified`, true or false, so the answer outlives the log.

**It fails open.** No git, no remote, a workdir that is not a repository, a remote
tip that cannot be fetched — all report *could not verify* and the run carries on.
Blocking a legitimate closure because the tree could not be found would be worse
than the gap this closes. A check that could not run never writes an attestation:
not knowing is not the same as knowing it is fine.

**Finding the tree.** The driver spawns sessions in a workdir, but the work happens
in a per-task worktree it did not create — and the workdir is routinely a
multi-repo parent (`~/dev`) that is not a repository at all. Linked worktrees share
their repository's refs, so the specific directory does not matter: the driver
looks for the *repository* whose refs carry the branch, searching the workdir and
its first two directory levels (enough to reach both `~/dev/<repo>` and
`~/dev/<repo>-wt/<task>`). Repositories are never descended into and bare ones are
skipped, so the search costs a bounded handful of `git rev-parse` calls. If *two*
repositories under the workdir carry the same branch — a review clone sitting
beside the session's own tree — that is reported as unverifiable too: checking the
wrong tree is worse than not checking.

This covers **unattended runs only**, by design: an interactive session bypasses
the driver entirely.

### Rescuing work a killed session left behind

A usage limit kills a session mid-turn. There is no graceful shutdown, so the
protocol's *commit, push, record the branch before you let go* never runs — there
is no *before*. The uncommitted work survives in the worktree and nowhere else:
invisible to the plane, invisible to another machine, invisible to the next
session. The lease then expires with no branch recorded, the task correctly
returns to `ready`, and the next clanker starts from nothing — redoing hours of
work sitting intact on the same disk. One such task cost 112.7M tokens, a third
of a day's spend, for work that was done twice.

So when a session ends still holding a task, the driver looks for the worktree it
was working in and, if anything is uncommitted, **commits it, pushes it, and
records the branch on the task** — turning a redo into a takeover:

```
salvaged CLA-314: committed the uncommitted work in /Users/you/dev/acme-wt/cc561415
as 4a91c0de and pushed it to origin/clanker/cc561415-a-session-killed-mid-task
recorded clanker/cc561415-a-session-killed-mid-task as the hand-off branch for
CLA-314 — the next clanker takes it over instead of starting again
```

**On every abrupt ending, not only a usage limit.** A crash, a killed process and
a limit are different to the supervisor and identical to the worktree, and the
limit is only detected by parsing a stream that may not have arrived whole.
Running it when it was not needed costs nothing: a clean worktree commits
nothing, pushes nothing, and records nothing — an empty hand-off would send the
next clanker to fetch a branch with nothing on it, which is worse than recording
none at all.

**It commits work it cannot vouch for, and says so.** The driver cannot tell a
half-applied refactor from good work, and the failure it exists to prevent is
caused by dropping work, never by keeping it. So the commit lands with a subject
that reads `WIP salvage: … (unreviewed, may not build)` and a body saying in as
many words that nothing here was reviewed, built or tested. It skips hooks and
commit signing on purpose: a lint hook would reject exactly the half-finished
tree this exists to save, and a signing passphrase has nobody to ask at 3am.
**One exception**: a worktree holding an unresolved conflict is left exactly as
it is, because committing it would record a state nobody chose — it would
fabricate a resolution out of the conflict markers and push a file that reads as
source with `<<<<<<<` in it. That covers both a git operation still in flight (a
merge, rebase, cherry-pick or revert) and a conflict nothing is holding open: a
`git stash pop`, `git apply --3way` or `git checkout -m` leaves unmerged entries
with no state file at all, so the unmerged index is checked as well as the
operation. Either way it is logged as `STRANDED WORK LEFT AS IS` with the path.

**It cannot touch a tree that is not this task's.** The worktree is identified by
its checked-out branch carrying `clanker/<first 8 characters of the task id>` —
the *task-id* half, not the title-derived slug, so a task retitled mid-flight is
still matched. A detached HEAD is never matched, `main` and `staging` can never
match, and two trees answering to the same task is reported rather than guessed
at. Nothing is ever forced: no `push --force`, no checkout, no branch creation,
no worktree removal. A push the remote rejects leaves the commit safely on this
machine and records **no** branch — a recorded branch is a promise that another
machine can fetch it.

**Recording a branch is not settling a claim.** The record carries no status, so
it cannot move a task, clear a holder, or post `ready` over work already in
review — which is why it is still safe on a session whose output could not be
read whole, where handing the claim back is forbidden. Once a branch *is*
recorded, the claim is deliberately not handed back either: the lease expires and
the task stays a takeover, so the next clanker is told there is work to fetch.

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
  "models": { "strong": "opus", "standard": "sonnet", "cheap": "haiku" },
  "prompt": "Work the next backlog item.",
  "max_turns": 400,
  "mcp_config_path": "./.mcp.json",
  "config_dir": "~/.claude",
  "idle_poll_interval": "60s",
  "poll_interval": "30m",
  "max_retries": 0,
  "retry_cap": "5m",
  "max_zero_spend_attempts": 3,
  "budget": {
    "max_tokens": 0,
    "max_cost_usd": 0,
    "max_wall_clock": "6h",
    "max_session_tokens": 0
  }
}
```

`max_turns` bounds every session's turns when its phase sets none of its own:
the phase's `max_turns` wins, then this, then a built-in default of 400
(CLA-343 — an unphased run used to have no cap anywhere in the chain, and one
session ran 1093 turns / 285.9M tokens with nothing able to stop it). "0" no
longer means uncapped; it means "defer upward". Claude only: the other
harnesses take no turn flag, so for them the cap is inert.

`budget.max_session_tokens` is the per-session runaway detector: when a
session's own cumulative usage crosses it, the harness kills the session
mid-stream and the salvage commits whatever it left (CLA-343). Unlike the
run-wide breaker, which only checks BETWEEN sessions, this one can stop a
single huge session in flight — the 285.9M runaway was 3.8x its run's whole
`max_tokens` ceiling. 0 defers: to 2x `budget.max_tokens` when that is set,
else to a 150M floor (~2.3x the largest legitimate session measured, CLA-309).
It is deliberately always on — it is a detector, not a budget. Claude only
today: the other harnesses' streams are parsed but never killed mid-session,
so for them the dial is inert until an adapter grows the kill.

`mcp_config_path` defaults to `<workdir>/.mcp.json` when that file exists — Claude's
headless mode does not auto-discover it, so **under `harness: "claude"`** the default
is what gives spawned sessions their clankerbar tools. It always gives the driver a
project slug to poll with, whichever harness is configured.

**The other two harnesses do not read that file, and one of them chokes on it.**
The default is applied regardless of harness, so this matters the moment you switch:

| harness | what it does with `mcp_config_path` |
| --- | --- |
| `claude` | passed as `--mcp-config` (with `--strict-mcp-config`) and read as Claude's `.mcp.json`. |
| `codex` | **ignored.** codex has no per-run MCP flag; its servers come from `[mcp_servers]` in `config.toml` under `CODEX_HOME` (which `config_dir` pins). |
| `opencode` | passed as `OPENCODE_CONFIG`, and **must be an opencode config** - servers under `mcp`, not Claude's `mcpServers`. Pointed at a Claude-shaped `.mcp.json`, opencode refuses to start and every session dies at spawn. |

Under `opencode`, set `mcp_config_path` **explicitly** to an opencode config. Leaving
it out does not opt out: an empty value re-runs the `<workdir>/.mcp.json` discovery,
so a workdir that carries a Claude `.mcp.json` hands it over no matter what. Run
`clankerbar doctor` - it prints the caveat instead of a verdict where the file is not
read, and FAILs the workdir check when `opencode` is pointed at a Claude-shaped one.

Point it at *your* workdir, not at a checkout of this repo. The `.mcp.json` at the
root here is the maintainers' own agent wiring and names the `clankerbar` project
slug; running the loop from inside this checkout would have it poll a queue you
cannot read, which it refuses rather than drains.

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

**One thing is bounded even under `max_retries: 0`: attempts that report no spend
at all.** A budget ceiling can only stop spend it is told about, and the figures it
reads arrive in the harness's own usage events. A session killed before any of
them arrives - the very thing a transient retry exists for - contributes *zero*,
so with a token- or cost-only budget the ladder is: attempt, zero spend, back off
at `retry_cap`, attempt, forever, with only `max_wall_clock` able to end it. So
`max_zero_spend_attempts` (default **3**) caps **consecutive** attempts within one
retry ladder that ended without the harness reporting any usage, and the run stops
with a distinct error naming the zero-spend loop - not a budget stop, not a
wall-clock stop, because what it means is that sessions are dying early rather
than working. An attempt that *does* report resets the count, **including one that
honestly reports zero**, so an ordinary retry ladder is untouched and a session
that legitimately did nothing is never counted against you. A **usage-limit pause
is counted neither way**: a session the cap turned away often reports nothing at
all, and waiting out a quota overnight must not read as sessions failing to start.
The ladder is per phase, so a phased config allows the bound in each. Raise it if a
harness of yours dies silently more often than that.

**What gets read as a failure.** A session's output is the whole event stream, and
the events quote the backlog verbatim — the task the session claimed is sitting in
the same bytes. So both classifications read only what the *harness* said: its
stderr, its own non-event output, its typed error events, and Claude's
`terminal_reason`. A task whose body happens to say "hit your", "usage limit" or
"api error: 500" is narration, and narration is never a cap and never a blip.

**Handing the task back.** A claim carries a 30-minute lease that the session
heartbeats. When a session ends mid-task, that lease is left ticking with nobody
renewing it — and clankerbar charges the task a *reclaim* to sweep it up, of which
there are only two before the task is parked for a human. So the driver watches
the session's stream for what it claimed, and hands the task straight back
(`update_task(status: ready)`) on **every** exit: usage limit, transient blip,
outright failure, or a clean finish with the work unfinished. The usage-limit case
is why this matters most — the pause that follows can be hours, and the release is
ordered before it.

One case is deliberately left to expire: a task whose session **pushed a branch**.
clankerbar computes its takeover hint only for an in-progress task with a dead
lease, so handing that one back would discard the very flag telling the next
clanker there is work to pick up. There it takes the reclaim and keeps the
hand-off. Releasing is best-effort throughout — if the plane refuses or is
unreachable, the loop logs it and carries on, since an expiring lease is merely
the behaviour it already had.

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

   **Prefer `max_cost_usd` - under `claude` or `opencode`.** It comes straight
   from the harness's own reported cost, so it measures the thing you actually
   care about. `max_wall_clock` counts the hours a run spends *waiting out a usage
   limit*, in which almost nothing is billed - one run elapsed 10h23m against an
   8h ceiling with 5h31m of that asleep. Keep it as an outer bound on how late a
   run may finish, not as your spend ceiling. `doctor` warns if it is your only
   dial.

   **Under `codex`, `max_cost_usd` is inert - set `max_tokens` beside it.**
   `codex exec --json` reports tokens, not money, so the adapter never populates
   a cost figure and no code path can reach that ceiling: it is not approximate,
   it is absent. A codex run with cost as its *only* dial therefore has no
   effective ceiling at all, whatever the config says. Give it `max_tokens` (or
   `max_wall_clock`) alongside; `doctor` WARNs on the cost-only case rather than
   reporting a budget that cannot fire.

   **`max_tokens` counts about ten times more than it used to.** Token totals now
   include cache reads and writes, which `input_tokens` excludes and which
   dominate a long agentic session — one run reported 140,387 tokens against
   $147.98 of real spend, about $1.05 per thousand, which is no model's price.
   The old number was the wrong one, but a `max_tokens` you tuned against it will
   now trip roughly ten times sooner. Re-tune it, or move to `max_cost_usd`.

   `max_wall_clock` is measured on the **monotonic** clock, so time the machine
   spends suspended does not count against it. That is also the clock the
   limit-reset check uses, so the two cannot disagree about how much of a run is
   left.

   When a limit's stated reset falls beyond `max_wall_clock`, the loop stops
   right then and tells you when the quota returns, rather than sleeping
   through the window to run one session against the fresh quota and stop on
   the next check.

   Every ceiling is also checked **inside** an iteration, before each re-run of a
   session that paused on a usage limit or backed off on a transient blip. Those
   waits can repeat for hours, and a run that reached its ceiling during one stops
   there instead of spending another session first.

   **The polls during a pause count too.** Waiting out a usage limit means probing
   for an early reset every `poll_interval`, and a probe is a real harness session
   — cheap (a one-character prompt, with the harness locked down to no tools or
   read-only), but not free, and a week-long cap polled every 30 minutes is a few
   hundred of them. Their tokens and cost go into the same running total the
   breaker reads, so a pause whose probes alone cross a ceiling ends on that
   ceiling rather than polling on.

   **A session whose spend cannot be measured stops the run, if you set a spend
   ceiling.** Every figure the breaker reads is parsed out of the harness's event
   stream, and one enormous line — a `tool_result` carrying a large file read —
   can end that stream early. The session's own total arrives in its *last* event,
   so a stream cut short reports near-zero for a session that may have cost
   hundreds of dollars. The loop refuses to count that figure, says so loudly, and
   then stops if `max_tokens` or `max_cost_usd` is set, because the ceiling you
   asked for can no longer be honoured. Under `max_wall_clock` alone, or with no
   ceiling at all, it carries on: the clock does not depend on anything the child
   said. The same session's task is left to its lease rather than handed back —
   the settle that released it may be in the bytes that never arrived.

## Harnesses

| Harness | Status |
|---|---|
| Claude Code (`claude`) | primary |
| Codex (`codex`) | adapter present; parsing being hardened |
| OpenCode (`opencode`) | adapter present; provider-agnostic (model comes from opencode's own config) |

New harnesses are a small adapter (`internal/harness`). Contributions welcome.

**Permissions are set per harness, and the loop fails closed under all of them.**
`claude` gets its allowlist from the settings file (`--settings`), `codex` from
`config.toml`, and `opencode` from `OPENCODE_PERMISSION` — a JSON policy baked
into the spawned process env. The opencode policy is a **path-scoped
PermissionConfig**: the run's workdir subtree (where the session's per-task
worktrees live, e.g. `~/dev/clankerbar-cli-wt/<task>`) is allowed for
`read`/`edit` (the workdir root's own listing included), `external_directory`
— opencode's gate for any path-taking tool on a path outside the project
boundary — allows the same subtree while **denying every other path** (so
`bash cp ~/.ssh/...` stays blocked, exactly as before), and `bash` stays
allowed tool-level (its permission patterns match parsed commands, not paths).
Everything else — reads or edits outside the workdir, `glob`/`grep`/`lsp`/`task`/`skill`,
and the network tools (`webfetch`, `websearch`) in every shape — is denied by
a `*` catch-all rather than asked, and denied tools are hidden from the
session's catalog entirely. Two exceptions keep the session alive inside that
catch-all: `*_*` allows the MCP tools (tool names are `<server>_<tool>` in
opencode — `clankerbar_get_backlog_summary`, `context7_query-docs`,
`chrome-devtools_click` — and the plane itself is reached over MCP), and a
probe/read-only run additionally denies `edit` and `bash` for zero writes. The
policy is pinned by unit tests in `opencode_test.go`, including the JSON key
order the catch-all depends on and a replica of opencode's rule evaluator over
a table of effective decisions.

**The workdir should be a multi-repo parent, not a git checkout.** The
read/edit rules are emitted to match the patterns opencode asks for a session
whose project is *not* inside a git repo (worktree `/`; the multi-repo-parent
case, `~/dev`). Point the workdir at a git checkout and opencode asks with
checkout-relative patterns the rules cannot express, locking the session's
structured Read/Edit tools out with no error — the exact failure this change
removes. A per-task worktree always lives *under* a parent workdir, so the
parent is the correct setting.

## License

MIT (see [LICENSE](./LICENSE)).
