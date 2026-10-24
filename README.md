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

### Phases: splitting one task across several sessions

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
checkpoint. The shipped shape is three phases:

```json
"phases": [
  { "name": "implement" },
  { "name": "review", "tier": "strong" },
  { "name": "fix", "tier": "cheap" }
]
```

Phase 1 claims the task, implements it in a worktree cut from the repo's
integration branch, self-verifies, commits, pushes and records the branch — then
stops. When phase 1 instead finds an existing branch or worktree already
carrying the task's work (a recovered claim, a prior session's WIP), its first
step is to merge the integration branch into it - a merge, never a rebase,
since rewriting pushed history breaks tip comparison - and re-validate the task
against the merged tip before touching what it found: superseded means record
the decision and park the task with an outcome saying so, or salvage only what
still adds value - never verify-and-push stale work.

The **`review`** phase starts on a **fresh context**, resumes the *same* run (the
driver substitutes the task and run ids into its brief, so it calls `heartbeat`
instead of claiming), re-reads the bar and the standing decisions from the
plane, works in that same worktree — never on the integration branch itself —
reads the diff and runs the adversarial review gate. It does **not** re-run the
repo's gates: the implement phase self-verified at this tip and the fix and
verify phases run them again over what they change, so a command here is only a
targeted probe of a suspected defect, and the worktree is left clean. It is
**review only**: it
does not edit, commit, push, open a PR, or move the task. Its deliverable is the
review record on the task: a sealed per-reviewer durable record written through
`record_review` — round, reviewer role, the phase's resolved
harness/model, the reviewed tip, scope, a `verdict`, and every finding with its
severity grade and `raisedByModel` — plus, for one release, the legacy block
in the outcome (`record_progress(outcome: ...)`, no status move) carrying the
reviewed tip, both sides' harness/model, the `verdict`, and every finding by ID
with the required fix. Findings are graded
— `must`, `should`, `nit` — and only a `must` gates. A `must` must carry its
evidence, one of: a quoted `doneWhen` clause the change violates; a repo gate
that fails or would fail (with the command and the output, or why it would
fail); a concrete failure scenario in the change or its direct regression
surface; a security exposure (credential exposure, an authorization bypass,
injection, a widened permission — with the path); a contradicted standing
decision (by id) or a hard rule in the repo's agent instructions (file + rule);
an irreversible migration, deletion or publish step missing the guard the repo
requires; or a new or changed guard, gate or state transition with no test that
pins it (its file:line and the missing test). Never a `must`, however confident
the reviewer: style, naming, a design they would prefer, a performance concern
without a measurement, documentation wording, or missing tests the bar does not
require — and where a repo hard rule concerns style, prose or documentation
wording, that never-`must` list wins. The block is
**appended below** whatever the outcome already carries — a statusless outcome
replaces the field wholesale — and the driver refuses to advance to `fix` unless
the plane serves a durable record **this run** wrote under this phase's
reviewer role whose reviewed tip equals the branch tip the driver verified on
origin: an earlier trip's record (a `request_changes` rework, a takeover) never
satisfies it. Before a review session is spawned at all, the driver reads the
task's baseline and retries the read; if it stays unread the sequence ends
without the review — a session whose record the seam could not judge against
the task it found is pure waste (CLA-663). Before spawning `fix` the driver
**closes the round**, which is what releases the sealed findings to the fixer.
The claim is held across the seam, so the task is never posted back to the queue
mid-sequence. One variant is the deliberate exception (question 03a12578,
option 1): a **branchless** (settled-task) checkpoint keeps the legacy block
gate above, because the plane accepts review writes only from the task's LIVE
holder and no run holds a task its own session settled — its brief prescribes
the `## Review` block alone, no round exists to close, and the fix phase that
follows reads that block alone too. A `verify` phase on such a task follows the
same shape: it adjudicates the block, appends its own, and hands the task over
when it judges no MUST open — the driver neither loops nor counts rounds there
(the id-based machinery needs plane finding ids this shape does not have), and
a MUST the verifier leaves open is **parked** for the operator instead.

The **`fix`** phase is the last one and runs on the cheap harness — unless a
`verify` phase follows it, in which case the PR and hand-over belong to the
verifier (see below). It resumes the
same run, reads the released durable record back with `get_task` — and, for one
release, the **last** `## Review` block beside it, because a re-trip can carry
several — checks the record's reviewed tip still matches the worktree's HEAD,
and applies every finding by its plane ID
(or records why not). The driver has already read the record and listed the
graded findings in the fix brief, applying the structural check the fixer must
not second-guess: a `must` recorded without an evidence kind or payload is
listed as a `should`. Every listed `must` is fixed, or refuted with evidence
recorded in its outcome — a `must` is never downgraded, and one raised by a
single reviewer is never dropped because nobody else raised it. Cheap `should`s
are fixed; `nit`s and the rest are recorded as not done with a reason (a
follow-up task only for a `should` worth doing). The fixer re-verifies scoped to
those fixes, commits and pushes them, opens a PR targeting the repo's PR base if
none exists yet for the branch, and hands the task to `in_review`. Its outcome
keeps everything above that **last** block — earlier rounds included — and
carries the review forward with a `### Fixes` list and both sides' attribution,
so the task holds the whole round: who reviewed, what was found, what was done
about it. See
[`docs/releases.md`](./docs/releases.md) for what happens to that PR after
`in_review` — merged by a clanker only once a human approves it, promoted to a
release only by the operator.

**The `verify` phase closes the loop (optional, and interim).** Add a fourth
phase to re-review the fixes instead of trusting the fixer's own account:

```json
"phases": [
  { "name": "implement" },
  { "name": "review", "tier": "strong" },
  { "name": "fix", "tier": "cheap" },
  { "name": "verify" }
]
```

With a `verify` phase configured, `fix` stops after committing and pushing: it
does **not** open the PR or move the task to `in_review`. The verifier then
reviews the **fix delta** — the reviewed tip the previous record names up to
`HEAD`, plus the regression surface those fixes touch, not the whole diff again
— and adjudicates every prior open MUST **by its plane finding id**
(`<round>.<reviewer role>.<n>`): resolved (`fixed`), resolved by dispatch
(`filed`), refutation accepted (`refuted`), or left open with a reason. It may
raise new MUSTs only inside the delta. The fixer marks the MUSTs it answers itself, so the driver
also hands the verifier every MUST of the current review cycle already marked
`fixed`, `refuted` or `filed`, with the fixer's evidence: a refutation changes
no code and would not show in the delta. The verifier confirms each marking or
sets it back to open, which keeps the gate shut. A fix round after a
verification round is handed every MUST still open on any released record, not
only the newest record's findings.

The stopping rule is a bar, not a feeling: the gate is **no open MUST**. A
verification round that ends with none carries the terminal step — push check,
PR, and a hand-over. That is ONE call: `submit_for_review(…, closeRound: …)`
carries the round close in the same atomic write that moves the task to
`in_review`, so the order that used to be load-bearing is now the plane's own
guard rather than two client calls. The plane refuses a sealed holder's outcome
write, so no outcome is sent on that call. The fix phase has already written
the outcome with the
`**Tests**` section the `in_review` guard reads. A round with any open MUST
releases its round the same way but ends **without** the PR and without moving
the task; the driver spawns another `fix`, then verifies again. SHOULD and NIT
findings are recorded and never gate: they ride to `in_review` in the outcome.

Two bounds make the loop terminate:

- **Round cap 3 per review cycle.** The plane counts non-void rounds, so a void
  round (lease expiry or takeover) does not count and a human verdict starts a
  new cycle; a takeover or reclaim does not reset the count. At the cap, any
  open MUST **parks** the task with a question carrying the records — never one
  more round. Only SHOULD/NIT may ride to `in_review`.
- **Non-convergence breaker.** A MUST id still open after two fix attempts, or
  one a verifier marked resolved that a later verifier reopens, parks the task
  with a question instead of spending another round on the disagreement.

The verifier is configurable like any phase (`harness`, `tier`, `model`). Left
unset, it inherits the **fix** phase's harness and its model/tier pair — the
cheap, non-reviewer side of the split — because verification rounds are scoped
and cheap and a reviewer's Claude subscription limit is shared with the
operator's interactive use. In the shipped example above that means the verifier
runs on opencode, not on the expensive review model.

This loop is **interim**. It lives in the CLI's `drainPhases` because it ships
before the state-machine reshape, which moves phase sequencing and the
`agent_review → implementing` transition onto the plane (B10); the round and
verification logic here is deliberately small so that reshape can replace it
rather than untangle it.

**A final `review` keeps the old combined brief.** The read-only mode is selected
by position: a `review` with a phase after it is the gate, while a sequence
ending on `review` still runs the historical review-and-fix brief, so an
existing `[implement, review]` config keeps working exactly as it did. An
existing config with a **custom-prompt phase after `review`** changes meaning in
this release: that `review` used to run the combined review-and-fix brief and
open the PR itself — it is now the read-only gate, and the custom phase after it
is expected to apply the findings. New configs should use the three-phase shape
— the saving is precisely the work that moves off the expensive model — or the
four-phase shape when the fixes need an independent verification round. `fix`
must follow a `review` and is either the last phase or is followed directly by
`verify`; `verify` must be last and must directly follow `fix`. Validation
refuses a sequence that violates any of those rules, including a verification
loop with no fix between its rounds (it could not make progress). The full
design, including the plane half that would store
review history durably, is in
[`docs/review-split.md`](./docs/review-split.md).

A usage limit that lands on a **resumed** phase is the one exception to the
driver waiting out caps inside the session that hit one: a wait of hours would
outlive the 30-minute lease the resumed phase is working, re-spawning it
against a run the plane has swept. So there the **sequence** ends instead —
the lease is left to lapse into a takeover with its branch recorded — but the
**run** does not: it waits the limit out holding no lease, probing exactly as
it would have inside the phase, and carries on polling when the cap lifts.
Only a hard stop (`lim.Stop`, credit exhausted) — or a reset landing past the
run's remaining `budget.max_wall_clock` — ends the daemon there.

A transient server/network blip on a resumed phase ends its sequence for the
same lease reason — the backoff ladder would walk past the 30-minute lease —
and the run stays up there too. There is no reset to wait out, but the
sequence still owes a wait: the run waits a backoff that grows with the
consecutive blips on that target and harness, and starts over the moment a
resumed phase there completes. That reset-per-harness is what makes the bound
real when the phases run on different providers — review on claude can keep
blipping while implement on opencode stays healthy, and nothing else in the
run would keep it from re-draining the instant the last sequence ended.

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

- **`max_iterations` counts task sequences, not sessions.** A three-phase config
  spawns roughly three sessions for the same limit — and the middle one is the
  expensive review.
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

The seam also survives a blind spot in how claims are observed (CLA-451). The
driver learns a session's claim by parsing clankerbar tool calls out of its
stream; a session that claimed through another channel — a raw API call with the
same key, a future harness quirk, a renamed tool — ends its implement phase
holding a task the driver never saw it take, and used to end the sequence there:
no review phase, no lease renewal, and pushed work stranded behind an expiring
lease until a takeover recovered it at a reclaim's cost. Now, before declaring a
checkpointable phase empty, the driver asks the plane about the one task it can
name — the task it dispatched: if that task is held with a branch recorded and
that branch verifies on the origin remote (the same evidence gate every
checkpoint passes), the driver treats it as the checkpoint and resumes the run
by heartbeat in the next phase, exactly as after an observed claim. The guards
are unchanged: an unreadable stream is never filled in from the plane, a session
that died producing nothing is never promoted by a plane answer, an observed
claim always wins over a peeked one, and a failed peek keeps today's ending.

The saving is **still unmeasured**: a phased run has now happened, but it is not
comparable evidence - its implement phase reached the checkpoint over an
implementation that already existed, so its totals capture a checkpoint plus a
review rather than a from-scratch split, and quoting them as a saving would
overstate the effect by a wide margin. A like-for-like measurement still does
not exist, and it costs a full task run to get. Splitting thinner earns less
each time while every extra boundary still pays a session's full startup cost.

### The review panel: several reviewers, one revision, one merge

One reviewer can return a confident all-clear on a diff that ships unreviewed
work: the review/fix split was itself reviewed by two models on the same diff,
and each found four issues the other did not. The panel is the answer — N
independent review phases on the **same reviewed tip**, each writing its own
sealed record, merged by the driver before the fix phase runs.

**The panel is opt-in, and clankerbar ships no panel, no preset and no model
names.** The CLI cannot know which harnesses, providers or models an operator
has, so it never picks one. With no `review_panel`, a config's `review` phase
runs exactly as it did before the panel existed: one reviewer, on the
harness/model the config already gives it, with the historical brief and the
legacy fix path. Setting the key is what fans the review phase out:

```json
"review_panel": [
  { "harness": "opencode", "model": "opencode-go/gpt-6-luna" },
  { "harness": "opencode", "model": "opencode-go/gpt-6-luna" },
  { "harness": "opencode", "model": "opencode-go/gpt-6-luna" },
  { "harness": "opencode", "model": "opencode-go/glm-5.3-flash" }
]
```

That is **one operator's fleet as an example, not a default** — the members are
whatever your own harnesses and models are. Each entry is one reviewer and one
review phase; the same model appearing more than once is deliberate, because
each run is independent. The panel applies to a **built-in** `review` phase (no
prompt of its own) that is **not the last phase** — the read-only gate. A final
review runs the historical combined brief, and a review phase with its own
prompt owns its own contract; `review_panel` alongside either is refused rather
than silently ignored. An empty array (`"review_panel": []`) is refused too:
removing the key is how you ask for a single reviewer.

Members run **in parallel by default**: the whole panel at once, each in its own
**detached worktree at the reviewed tip**. The driver creates that checkout (a
`git worktree add --detach <tip>`), starts the member in it, and removes it when
the member ends; a dirty member tree is logged and discarded, never salvaged,
because a review-only session's deliverable is its record and committing its
probe artifacts would move the tip past the revision every member recorded.
`review_panel_concurrency` caps how many run at once: `1` is the historical
sequential behaviour (members then share the task's own worktree), `N` runs at
most `N`, and an absent key or `0` means the whole panel. It is **machine-owned**
(local `config.json` only, never taken from the plane's stored document): how
many seats run at once changes how fast a machine reaches the result, not what
the result is. Members start in
`review_panel` order: with a cap smaller than the panel, the first `N` entries
run first and each freed slot goes to the next entry, so put the slowest
reviewer at the top of the list — its long session then overlaps the others
instead of extending the round's tail. The fix phase starts only after the join
has every member's record — or its INCOMPLETE outcome — and then works in the
task's own worktree as before.

Each member's reviewer role is **positional** — `review-1` .. `review-N`, counted
over the review-shaped phases — and that is the role the durable record is keyed
on. `name` is only the member's log label (it defaults to the same positional
string); naming a member `alpha` changes what the logs call it, never which
record it writes. A member may set a `tier`, a `model`, or both — the model
alias wins on expansion — so a tier resolves through a harness's tier map.

**Independence is a seal, not a promise.** The plane holds a round's records
sealed: written, but with their findings withheld from every `get_task` until
the round closes, and the driver does not close the round between members. So
member 2 reads the metadata of member 1's record (that it exists, its counts)
and never its findings — the same record shape the console will show later. A
panel member's brief is the **durable record alone**: it writes no outcome
block, because member 1's block would land in the task and be the first thing
member 2 read, and because the plane refuses this run's outcome writes anyway
once its record seals the round. The panel's findings reach the outcome through
the fix phase — the merged list it is handed, and the outcome it writes. A
later member's prompt is built by the driver and contains no earlier member's
findings.

The round closes once **every member has recorded or been recorded as failed**:
a usage-limited, capped or dead member is recorded as failed — with its reason,
booked in the per-harness dead-phase tally — and the panel **moves on to the
next member** rather than ending. After the last member the round closes (by
the live holder, before anything hands the claim back) and the fix runs on the
merged list, which names the incomplete member. A panel whose round cannot be
closed ends without a fix rather than spawning one against sealed findings
(fail closed); otherwise, only a panel in which NO member recorded ends without
a fix. A member that loses the claim ends the panel exactly as before. A panel
that fails **one reviewer** therefore never strands the findings the others
recorded, and the same member recording nothing on successive tasks is said
loudly, with its harness and model. A **branchless** task cannot run a panel at
all — its record is the legacy `## Review` block, and the plane takes no durable
record from a settled task — so it runs one reviewer, with the brief it would
have had anyway. That one reviewer is the first **required** seat: an optional
trial seat is skipped there (a trial seat's failure would otherwise consume the
single review slot and leave the task with no required review at all). It is
the panel's last in every sense that matters: if it records nothing, no fix
runs.

**A member can be a trial seat.** Writing `"optional": true` on a
`review_panel` entry marks it as a reviewer being evaluated, not one the panel
relies on. An optional member runs and records like any other — its record,
findings and failure all count toward that member's own assessment — but its
failure is that member's result and nothing more. Whatever its session ends in
(a transient blip, a usage limit, a credit stop, a death, a tool-loop kill or a
setup error), it does **not** end the sequence, does not hand the panel's shared
claim back, and does not stop the run. The required members keep reviewing, the
round still closes, and the fix runs on what they found; the INCOMPLETE line
names the optional seat `(optional)` — in the daemon log and in the fix brief
alike — so the two are not confused. A member with no `optional` key is
**required**, exactly as before. A panel must have at least
one required member (`Validate` refuses an all-optional panel), and a round in
which **only** optional seats recorded is no review at all: the round is not
closed and no fix spawns, exactly as when nobody recorded.

**The driver merges; the fixer never grades reviews.** Every MUST from ANY
member of the panel's round is open — consensus never filters one out, because a
blocker one reviewer found and another missed is exactly the case the panel
exists for. Findings the plane already shows settled (`fixed`, `refuted`,
`filed`) are not relisted as open work, and the list reads only the round the
panel itself wrote, so a verify rewind does not resurrect an earlier cycle's
findings. SHOULD and NIT findings are listed with the members that raised them;
they are recorded, never gating. The merged list names any member that recorded
nothing and why, so the fixer knows it is seeing the union of the reviewers that
ran, not the panel that was configured. Refuting a MUST is done by the fixer
with evidence, never by dropping a finding another reviewer would have kept.

The panel is **interim** and local config, like the phase sequencing it
multiplies (B10). The state-machine reshape moves review sequencing and the
`agent_review -> implementing` transition onto the plane, where panel membership
becomes plane phase state; until then the CLI owns the fan-out. Members resume
the **same run** and all heartbeat its one run id: that is safe because a
heartbeat is an idempotent lease renewal (one `last_heartbeat_at` /
`heartbeat_count` UPDATE, no version check), and each member's durable record is
keyed by its **positional role**, so concurrent appends land as distinct rows in
the one round the plane derives (`max(existing round) + 1` when nothing is
sealed — deterministic for simultaneous first writers). Membership therefore
needs no per-(task, phase) leases; what it needs is what ships here: a private
checkout per member and a join that closes the round before the fix. The cap is
also the budget breaker's granularity: the members share one run-wide ceiling,
so each member publishes the spend it has counted to a shared panel ledger the
moment its attempt ladder counts it, and every member's pre-spawn budget check
measures the panel's **aggregate** — a member that starts after a sibling has
already paid (the cap staggers them) sees that money against the ceiling. What
no check can see is spend still in flight, so the residual overshoot stays
bounded by the sessions running alongside it, at most `cap - 1`. `clankerbar
doctor` prints the resolved panel, with each member's harness and model, so an
overnight run's review spend is visible before it happens.

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
the values the driver read off the held claim — the task id, the run id and the
verified branch — and the phases' own resolved harness/model/position. Phase N's
session output is not an input to it at all. (Its *findings* are, deliberately:
the review record is the one thing that must cross the review→fix seam, and it
crosses as a task record the plane stored, not as text the reviewing session
smuggled into the fixer's prompt.) A test pins that absence rather than the
wording of the shipped brief, because the wording is the part that is allowed to
change.

The review phase also puts the judgement behind the checkpoint, which is where a
session's context peaks and so where a usage limit or a crash is most likely to
land. Before the split, a death there destroyed the findings and the half-applied
fixes together; now the findings are on the task before the expensive session
ends, and a death costs the fix session, not the review.

### Per-phase models: a tier map you own, or a model you name

`model` pins one model alias for the whole run. A phase can name its model one
of two ways, and the stored run-config document carries both:

- `tier` names a bucket in `models`, so one policy word can resolve to a
  different alias on each harness;
- `model` names an alias directly, for a step whose model you do not want to
  express through a bucket.

A phase sets **one or the other, never both**, and `Validate` refuses both so it
is never ambiguous which one wins (a review-panel member may set both, and the
alias wins there). Resolution is `model`, then `tier`, then the harness default.

```json
"models": { "strong": "opus", "standard": "sonnet", "cheap": "haiku" },
"phases": [
  { "name": "implement", "tier": "standard" },
  { "name": "review",    "model": "opencode-go/gpt-6-luna" },
  { "name": "fix",       "tier": "cheap" }
]
```

**One caveat that is not about models.** A `fix` phase that shares the review's
Claude account can hit the same rolling usage limit the Opus review just consumed
the tail of, because the fix session starts immediately after it. That no longer
exits the daemon — the sequence ends and the run waits the reset out — but the
task lapses into a takeover that pays `implement` again, plus the wait. Keep the
three-phase shape, but run `fix` on a different provider — see
[Per-phase harnesses](#per-phase-harnesses-implement-on-one-review-on-another).

**The buckets are yours, and the indirection is the point — when you want it.**
clankerbar drives three harnesses and cannot know which models you have, nor
which of them is cheaper — that needs a price table, which goes stale silently and
in the direction that costs money. So it never learns what models exist; with
`tier` it learns only that you bucketed them, and the phase names a **bucket**,
never an alias. (`"tier": "opus"` does not pin opus. It looks for a bucket called
`opus`.) `model` is the way to name an alias directly when you do not want that
indirection — it is a plain alias, so a typo is a model the provider refuses
rather than a bucket that silently resolves to the harness default.

**Everything falls back, and nothing here can stop a run.** A phase with neither
`model` nor `tier` runs on the run-wide `model`; a `tier` naming a bucket you never
defined runs on that model and says so in the iteration log; the run-wide `model`
itself empty means the harness picks, as it always has. A config written before any of this existed behaves exactly as it
did. A mistyped bucket costs a log line at 3am, not a stopped drain.

**Route lists: a free route first, its paid twin after it.** A model field may
also name an **ordered list of routes** instead of one alias. The session starts
on the first; if the provider denies it with a usage limit — a free tier that has
run dry — the opencode adapter resumes the **same session** on the next route, so
the transcript and the work in progress survive the switch:

```json
"harnesses": {
  "opencode": {
    "model": ["opencode/deepseek-v4.1-flash-free", "opencode-go/deepseek-v4.1-flash"]
  }
}
```

The array form is accepted anywhere a model alias is: a harness block's `model`,
each value of its `models` tier map, a phase's `model`, and a review-panel
member's `model`. The top-level `model` and `models` stay plain strings — that is
the plane's stored schema — so on a single-harness config put the list in that
harness's block.

The switch is **sticky for the run, not per request**. The driver marks the
denied route until the reset the provider stated, or 30 minutes when it stated
none, and later phases start on the route that worked; the run returns to the
free route at a phase boundary once the mark lifts, never mid-session. A
transient 429 is not a switch — it stays on the retry ladder. When every route is
denied the session is treated exactly as a limit always was: a rolling window is
waited out and a hard budget stop ends the run. A phase that starts with every
route still marked uses the one whose mark lifts first. A list of one behaves
exactly like the plain string it replaced, including the config identity the
fleet reports.

**Which way to tier is a question of blast radius, not difficulty.** A phase that
produces a durable artifact — code, a recorded decision, a bar someone will be
measured against — is worth the strong bucket, because a mis-recorded decision is
read for months by people who cannot see it was wrong. A phase that only
compresses a large output into a small answer can take a cheap one, because the
error is visible immediately to whoever asked the closed question. Nothing is
tiered by default: an untouched config runs every phase on the run's model. The
review split is the concrete case the rule is for: the review is judgement over a
diff and a bar, so it takes the strong bucket; the fix applies findings that are
already written down and re-runs the gates, so it can take the cheap one. Putting
the review's model on the fix's typing was the $3.92-per-session mistake this
split removes.

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
  { "name": "review", "harness": "claude" },
  { "name": "fix", "harness": "opencode" }
]
```

That runs the implementation and the fixes on a cheap provider-agnostic backend
and keeps the adversarial review on the harness with the subagent machinery.
Nothing in the seam resists it: each phase is already a fresh session seeded from
the observed claim, so what crosses the boundary is a task id and a run id, and
both live on the plane rather than in a session.

**Every key in `harnesses` must name a registered harness** — `claude`, `codex`,
`opencode`. A block keyed by anything else is refused at startup rather than kept
as dead config: nothing ever looks that key up, so its `model`, `models` and
paths would silently stop applying. What that catches is the leftover of a
rename — a `harnesses.opencode2` block from the retired 2.x preview harness
(CLA-675) — and the refusal names the rename to make.

**The review can be a panel.** Setting `review_panel` fans the one built-in,
non-final review phase out into one review phase per member; see [the review
panel](#the-review-panel-several-reviewers-one-revision-one-merge) for the
config, independence and the merge. It is opt-in — with no `review_panel` the
review phase runs once, as before — and members name their own `harness`/`model`,
so a panel can span harnesses too. The members run concurrently by default, each
in its own detached worktree; `review_panel_concurrency: 1` restores the
sequential sequence.

**Do not run `fix` on the review's Claude account.** A fix session that starts
right after an Opus review can hit the same rolling usage limit the review just
consumed the tail of. That no longer takes the daemon down — the sequence ends
and the run waits the reset out holding no lease — but the task lapses into a
takeover that pays `implement` again on top of the wait. Pair the review with a
different provider for `fix` (opencode in the example above); an all-Claude
three-phase config is the shape most exposed to this, and the example here is
deliberately not one.

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

**Every phase's harness has to observe the session's task claim** (`claude` or
`opencode`), not only the first one's. The first phase has to *observe* a claim
or there is nothing to hand on; a later phase is *handed* one - but seeding it is
the adapter's job, and an adapter that does not track claims does not seed
either. A non-tracking phase mid-sequence would end every drain early with a log
line that reads like an ordinary finish, and one at the end would leave the
driver handing the task back over its own work. Validation names the phase and
the harness when it refuses.

**A block that declares nothing is refused like a missing one**, since an empty
one spawns exactly the session the rule exists to prevent. And `--harness` /
`--model` are refused outright on a config that selects harnesses per phase: both
flags assert the run has one harness, and `--harness` would silently re-point
which harness the run-wide fields belong to. Edit the `harnesses` block instead.

**A single-project run is slug-checked too.** With no `projects` block the poll's
slug comes from the top-level `mcp_config_path` while each harness's sessions
work whatever *its* file names, so two files naming different projects are
refused the same way a mismatched `projects` entry is - the poll would gate on
one project's counts while sessions claimed and worked another's.

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
clankerbar                          # the fleet supervisor: reconcile this machine
                                    # against the plane's account roster
clankerbar run --harness=claude
clankerbar run --harness=claude --model=opus --max-iterations=10
clankerbar run --config ./clankerbar.json     # or: -c ./clankerbar.json
clankerbar ctl restart -c ./clankerbar.json   # tell the RUNNING daemon to re-exec
clankerbar ctl reload  -c ./clankerbar.json   # re-read the config file, no exec
clankerbar propose-config                     # import this file into the plane (PENDING)
clankerbar upload shot.png                    # print asset:<id> (the bytes never leave the shell)
clankerbar deck deck.html --task CLA-581      # upload a review deck, print its review URL
```

### The fleet supervisor: bare `clankerbar`

`clankerbar` with **no subcommand** (equivalently `clankerbar supervise`) is the
fleet supervisor: it reconciles this machine against your **account-scoped
roster** on the plane, polled every 15 seconds from
`<backlog_url>/api/daemon-roster`. The account key (`CLANKERBAR_API_KEY`) is
required — a supervisor spans projects, so a project key will not do.

Every poll — and immediately after any child state change — the supervisor also
reports its own view back to `<backlog_url>/api/daemon-roster/status`, so the
console can tell a HALTed instance from a crashed one and a stopped machine from
a sleeping one. The body carries the host, the supervisor's version and start
time, and one row per instance: its state (`running`, `halted`, `backing_off`,
`exited`, `stopped`), the reason, since when, the child's version, its last exit
(code and time) and any scheduled retry. Like the daemon's fleet beacons this is
fail-soft telemetry — a failed POST is logged once per outage and never changes
what the supervisor does.

Each roster entry declares one instance:

- **`desiredState: running`** — a supervised child is kept up, spawned as
  `clankerbar run -c <generated config>` in the child's own state dir. A child
  that exits for any reason other than a stop it was told about is restarted,
  with backoff: the delay doubles from 2s to a 60s cap per consecutive
  unexpected exit, and a run that stayed up at least 2 minutes is treated as
  healthy and resets the ladder.
- **`desiredState: stopped`** — the supervisor writes the instance's STOP
  marker into its state dir (the same marker a hand-written `touch STOP`
  writes) and the daemon drains at its iteration boundary. Flipping desired
  state in the console lands on the machine within one poll.
- **`placement: remote`** — not implemented (Decision 7); the entry is ignored
  with a log line. An entry deleted in the console has its child stopped and is
  dropped.

**Stopping the supervisor stops the fleet.** SIGINT/SIGTERM to the supervisor
writes a STOP marker into every child's own state dir, so each daemon drains at
its iteration boundary (an in-flight session finishes) rather than being killed
mid-session, and the supervisor waits for all of them before exiting. A child
that was told to stop is not restarted; that covers the fleet-wide stop and the
per-instance case — a `HALT` marker (the one marker the daemon does NOT
consume) left in a child's state dir stops that instance for good; delete the
marker and restart the supervisor to resume it. A hand-written `STOP`, by
contrast, is consumed by the daemon when it honours it, so the supervisor
cannot see it and reads the exit as unexpected: **to stop ONE instance while
the supervisor runs, touch `HALT` in its state dir, not `STOP`.**

**A dead child's claim is handed back immediately.** A session that dies with
the daemon process — a crash, a `kill`, the machine sleeping — would otherwise
leave its lease ticking for the full 30 minutes with nobody working the task.
While a daemon holds a claim it keeps `CURRENT_CLAIM` in its state dir (task id,
run id, project, branch once one is recorded, and when the claim was taken) and
removes it on every path where it lets go. When a child exits with the file
still there, the supervisor makes the call the dead daemon could not, before any
respawn: `release` when no branch was recorded, `expireLease` when one was, so
pushed work comes back as an immediate takeover with its branch instead of
waiting out the lease. The same happens inside the daemon when a phase is
abandoned holding pushed work. A plane that refuses because the run already
ended or was taken over (`not_holding`, `run_superseded`) is logged and ignored.

**A clean approved task is integrated without a session.** An
approved-awaiting-merge task used to wait for a fresh agent session to merge its
PR — which waits for some clanker to finish the task it is on, and a task with a
review panel runs 30-60 minutes. Every step except resolving a conflict is
mechanical, so the supervisor does them: for each supervised project whose
policy says a clanker integrates (`authorityIntegrate: clanker`), it finds the
open PR for the task's recorded branch, judges it with the same delivery check
the driver applies to an agent's claim (mergeable, and a check rollup that ran
and passed), **claims the task first** so an agent close-out and this path can
never both merge it, merges the PR as a merge commit, verifies that merge commit
is an ancestor of the base on origin, and closes the task done with that
delivery. It does nothing under `authorityIntegrate: human`, for a PR based on
`main` (the promotion is yours), for a failing, unchecked or unresolvable PR, or
when the claim is refused. A conflict is the one stand-down that hands work
somewhere instead of just leaving the task alone: the conflicting paths are
logged once per head/base pair, and a conflict-only session is started for it.
Anything after a
successful claim that cannot be finished releases it, so the task goes back to
`approved` rather than being left held — with one exception: a merge that LANDED
but whose ancestry could not be verified is closed `done` with `mergeVerified:
false`, because the PR is no longer open and a release would strand the task.
The merge itself names the configured base and the judged head revision, so a
PR retargeted after the judgement cannot be merged into `main`; its verification
and conflict reads run in a throwaway clone, never in the checkout the fleet's
sessions share.

**A conflict gets a short dedicated session, not the next clanker's full one.**
A CONFLICTING PR used to wait for whichever clanker finished its current task — a
full implement-phase session, with the whole drain brief, to resolve a merge
conflict. The supervisor now handles it: it takes the claim itself (so an agent
close-out and this path cannot both work the task — a refused claim means no
session), then starts **one** short session on the fleet's implement harness and
model, configurable as the `conflict` step:

```json
"conflict": { "harness": "opencode", "model": "commandcode/deepseek/deepseek-v4.1-flash", "max_turns": 80 }
```

Every field is optional and defaults to the implement step's own, so an untouched
config runs the conflict session on the model that implemented the branch. The
brief is narrow: create a worktree of the branch, merge the PR's base into it (a
merge, **never** a rebase — the branch is already pushed), resolve **only** the
conflicting hunks, run the repo-wide gate (`go test ./...` when the checkout has
a `go.mod`, `pnpm test` when it has a `package.json`, else the repo's own
documented gate), and push. It then hands the task back by the nature of the
resolution: **mechanical** (renumbering, keeping both sides of a list,
regenerating a pinned figure — no change to what the code does) releases the
claim and leaves the task `approved` for the close-out above to merge once CI is
green; **behavioural** or uncertain moves it back to `in_review` with a note
naming what changed, because that code has not been reviewed; a **failure** (a
gate that will not go green, a hunk that cannot be resolved without guessing)
releases the claim (attesting `checkGreen: false`, since the merge never
happened — that is what makes the plane's CI-repair ladder bound a
green-but-unmergeable PR), leaves the task `approved` and is logged with its
reason.

At most one conflict session runs at a time, and the bound is a **shared lease,
not a per-process flag**: the supervisor takes an exclusive lock on
`conflict-session.lock` under its state root for the session's lifetime, so every
supervisor using that state root contends for one slot — two processes on a
machine, or two instances in one process. The kernel drops the lock when the
holder exits, so a session that crashed or was rolled never strands the slot, and
a second conflicting task waits for the next poll. The plane claim remains the
guard for the SAME task (a refused claim means someone else has it). Machines
whose state roots are separate each hold their own slot, so a fleet spanning
machines should give them a shared state root if the one-session-at-a-time bound
has to hold across all of them; a cross-machine slot for *different* tasks needs a
lease in the plane, which does not exist yet.

A CONFLICTING read whose fetched tips merge cleanly starts no session at all:
GitHub's mergeable state lags a push, so that is the evidence that the conflict is
already resolved (exactly the shape a mechanical resolution leaves behind), and a
session there would be a second paid attempt at a conflict that no longer exists.
The task is left to the deterministic close-out, which merges it as soon as the
stale read catches up. Daemon-started sessions never run on Claude: a
`conflict` step resolving to `claude` stands down with a log line and leaves the
task to the agent close-out.

**The base config is the machine layer.** The supervisor reads your local
config from the one discovered location, `~/.config/clankerbar/config.json`
(loaded exactly as `clankerbar run` loads it, or bare defaults when no file
exists). When the file exists it is re-read on every reconcile. An edit that
changes a running child's effective config is written into that child's
generated config and queued as a graceful `RESTART`, so it reaches the child at
its next iteration boundary — no supervisor restart and no hand-written `STOP`.
An edit that changes nothing writes nothing (an unchanged config serializes to
the same bytes, so there is no restart loop), and an edit the permission-policy
check refuses is logged and never written: the running child stays on its
current file. Everything the plane may not set — `env` and `@file` secrets,
`settings_path`, `config_dir`, `mcp_config_path`, `backlog_url` and the rest —
comes from this file.

**The roster names the projects; the base config wires them.** A roster entry
carries the project(s) the instance drives. For each one the supervisor starts
from the base config's matching `projects[]` entry — its `workdir`, `repos`
map, `mcp_config_path` / `mcp_config_paths`, `pr_base` and every other
per-project key — and takes only the project's slug and primary repo from the
roster. A roster project with no matching base entry runs on the roster's slug
and repo alone, with the top-level fallbacks; the roster's primary repo (the
plane's `project.primary_repo`) is what the workdir derivation derives from.
This is how an instance running e.g. implement-on-opencode / review-on-claude
keeps one MCP file per project per harness (see "Multi-project runs need one
MCP file per project per harness" above) instead of losing its wiring.

**One-directional overrides.** A roster entry may carry exactly one class of
override — `harness` and its per-harness block (`model` / `models`) — and
nothing else: a machine-layer key (`env`, `settings_path`, `workdir`,
`mcp_config_path`) or any other run-config key is **refused loudly** and named
in the log, never silently dropped — a silent drop is how a daemon ends up
looking configured and not being.

**Workdir derivation (phase 2a) — the fleet's own clones.** Set
`CLANKERBAR_WORKDIR_ROOT` to the machine's one root and the supervisor lays out
one directory per project, and clones into it:

```
<root>/
  <project slug>/                     the project root — a session's scope
    <repo name>/                      one clone per repo the project names
    worktrees/                        every worktree the fleet creates here
```

Each repo the project names — the keys of its `repos` map (whose VALUES are
optional local-path overrides, not identities) plus its `primary_repo` — is
derived as `<root>/<project slug>/<repo name>`. A bare `repos` key that names
the same checkout as one of the project's full `owner/name` identities is that
identity spelled short and is materialized under the full name, which is the
only spelling a clone URL can come from; two DIFFERENT full identities that
share a name (`acme/api` and `other/api`) cannot both live at
`<project root>/api`, and the derivation refuses the instance naming both rather
than cloning one over the other. A repo that is **not there yet
is cloned** from the plane's repo identity, so nobody has to check anything out
for the fleet first; the clone reads the run's git credential (the same
`gh auth token --user <owner>` scoping the delivery verifier uses, and the token
travels in the child's environment, never on a command line). The project's
`worktrees/` directory is created too: it is the fleet's own area, not the
operator's.

The clone's transport comes from `clone_url_base` (default
`https://github.com/`). With the default, `origin` is
`https://github.com/<owner>/<name>.git` and the one clone invocation resolves the
owner's token through `gh auth token`. With an ssh prefix such as
`git@github-personal:`, `origin` is
`git@github-personal:<owner>/<name>.git`, the clone uses that host alias and the
key, and **no `gh auth token` is spawned** — so a session's fetch and push do not
depend on a token riding in its environment. The key is a prefix, so it ends
where the owner begins (`/` for a URL or path, `:` for an scp-style ssh alias);
[Config](#config) has the full shape and the refusal for anything else.

Derivation fails closed, on the conditions learned from the CLA-441
wrong-workdir failure. A directory that **exists** is never replaced, adopted or
cloned over: if it is not a checkout of the expected repo — a plain directory, a
file, a symlink, a checkout of a different repo, a repo whose origin cannot be
attributed — that daemon is **refused**, the path tried is reported, and the
supervisor's own working directory is never used as a fallback. A **symlink** at
the project root, a repo clone or the `worktrees/` directory is refused too: it
would put the project's clones, its worktrees and the sessions' scope outside the
project root the layout promises, so those are always real directories. A failed
clone leaves nothing behind (it lands in a temporary sibling and is renamed into
place, and only onto a path nothing else created in the meantime). With the
variable unset, derivation is off and children run on the base config's own
workdirs, exactly as before.

**These clones are the fleet's.** The fleet fetches in them, resets its own
default checkout, creates and deletes branches and worktrees in them, and
clones one in when it is missing. They are not a place to keep work that is not
pushed. The derivation writes `project_root: true` into the generated config,
which is what widens each session's permission scope to the whole project root
(see "What each harness receives" below) and what `doctor`'s `project root`
check verifies: every configured repo resolves INSIDE the project root, no path
in the layout is a symlink out of it, the clone's origin names the repo it sits
under, and the worktree directory exists and is inside the root.

**Working in the fleet's clones, or not.** The choice is yours:

- **Work in them** and let the fleet manage them (the default the layout is
  built for). Nothing to keep in sync: your checkout IS the fleet's.
- **Keep your own clones** elsewhere (`~/dev/...`, wherever you like) and pull
  the branches the fleet pushes. Costs disk for a second copy of every repo,
  and you must not expect the fleet to touch your tree — it never reads or
  writes outside `<root>`.
- **Middle path: a worktree of a fleet clone.** `git worktree add` from
  `<root>/<project slug>/<repo name>` gives you your own working tree sharing
  the fleet clone's objects, with these caveats:
  - git refuses to check out a branch that is checked out in another worktree,
    in **either** direction: you cannot check out a branch a fleet worktree
    holds, and the fleet cannot create a worktree for a branch you hold. Use
    your own branch names, or `--detach`.
  - refs and objects are shared, so a commit of yours is visible to the fleet
    clone immediately, and a fleet `git worktree prune` / `gc` runs over your
    worktree's metadata (safe while your worktree directory exists).
  - if the fleet ever re-clones the repo, worktrees hanging off the old clone
    break.
  - the fleet's own default checkout (the repo directory itself) must be left
    alone — that is where it reads, fetches and branches.

**Materialized configs (phase 2b).** Every child runs on a config the
supervisor *generates*, not a hand-maintained file: the child's effective
config — the operator's machine layer plus the roster's projects and permitted
override, with the machine conventions resolved (the derived workdir, the
resolved `settings_path` / `config_dir` / `mcp_config_path` and the default
`backlog_url`; the credential itself stays in `CLANKERBAR_API_KEY` in the
environment, which the child inherits) — is written to `materialized.json` in
the child's own state dir, and the child is started with `run -c` pointing at
it. The generated file is a **cache**: written when the child is spawned and
rewritten at reconcile when an edit changes it (see "The base config is the
machine layer"), never hand-edited, and safe to delete while a child runs — the
next spawn regenerates it. It is written to disk rather than
held in memory so it doubles as the offline last-known-good: with the plane
unreachable the supervisor reconciles against the cached roster and starts
children from their materialized configs, rather than starting nothing (a cold
start with no cache has nothing to supervise and says so). The instance
identity and the state dir are pinned inside it, so a daemon beacons and stops
exactly as it did when started by hand.

**Permission policy gate (phase 2c).** The supervisor never starts a daemon
without its permission policy: `settings_path` is the fail-closed headless
allow/deny policy, deliberately read-only to agents — a plane that could set
it could hand a daemon a permissive policy — and the file it names must exist
when the daemon starts. An instance whose policy file is absent is **refused**
— no child is spawned, and the refusal names the daemon and the policy path
tried. The gate is checked at every reconcile and again at every (re)spawn, so
a policy file deleted while the supervisor runs stops the next respawn, not the
running child, and a restored file is picked up by the next backoff retry. A
config that names no `settings_path` is unaffected — it runs on the ambient
allowlist, which `doctor` already warns about.

**The log is a status surface.** On every change of the fleet the supervisor
lists the version it itself runs and, per instance, the state and the version
of the child serving it — so version skew between children (and between a
child and the supervisor) is observable on the machine, not only on the fleet
page. Those lines, and its refuse/spawn/exit decisions, are also written to
`<supervisor state dir>/supervise.log` (see [Visibility](#visibility)).
`clankerbar supervise roll` rolls the children onto a newly installed
binary one at a time, verifying each one's next beacon reports it before the
next is touched; `clankerbar supervise replace` drains the fleet and execs the
running supervisor into a newly installed build. Neither fetches or installs
anything — you install the new build at the fleet's launch path first.

The config dir can hold more JSON than the base config — MCP configs
(`opencode-mcp.json`), headless permission policies (`headless-drain.json`),
opencode's own config (`opencode.json`), leftovers from hand-started daemons
(`clanker1.json`, ...). The supervisor reads none of them: its machine layer is
`~/.config/clankerbar/config.json` alone, and which instances run comes from
the roster, not from files in the directory. (`clankerbar run -c <file>` still
runs any config you point it at by hand.)

Two operational notes, both inherited from how `run` already behaves:

- **The supervisor resolves each child's config from the supervisor's own
  working directory.** A config with a RELATIVE `workdir` resolves against
  wherever the supervisor was started; the operator's configs use absolute
  paths, and so should the base config.
- **The supervisor's children run with the supervisor's environment** — the
  account key, the harness binaries on PATH, everything. A child's `RESTART`
  marker re-execs the child in place (same pid, same launch path), so a
  rolling restart of one instance works exactly as it did when started by
  hand.

Flags are **GNU-style**: `--long` options, `-x` shorts. `--config` (`-c`) and
`--help` (`-h`) are the only short aliases; everything else is long-form only.
`clankerbar run --help`, `clankerbar doctor --help` and
`clankerbar propose-config --help` list them. A short flag's
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
PASS  config       loaded /Users/you/.config/clankerbar/config.json
                   harness: claude
                   workdir: /Users/you/dev
                   api key origin: https://clankerbar.com
                   backlog: https://clankerbar.com/api/projects/acme/backlog-summary
PASS  harness      /usr/local/bin/claude (2.1.0)
WARN  config_dir   not set — the session inherits the ambient environment
                -> set config_dir (or --config-dir) so a cron/launchd run loads the same skills, plugins and auth as your terminal
WARN  backlog      https://clankerbar.com/api/projects/acme/backlog-summary — 0 claimable, 2 open question(s) — nothing to claim; the loop will idle without spawning
                -> answer the open question(s) at clankerbar.com, or expect an idle run
WARN  state_dir    /Users/you/.local/state/clankerbar/loop/dev-9f70ef211d1e0549 has a leftover STOP marker
                   STOP — the loop stops on its first tick
                 -> delete it, or the loop acts on it immediately on start: rm /Users/you/.local/state/clankerbar/loop/dev-9f70ef211d1e0549/STOP
WARN  workdir[acme] /Users/you/dev has no agent-instructions file (AGENTS.md / CLAUDE.md)
                -> add one here naming each repo below and where its protocol lives — a session started in a multi-repo parent loads nothing from the repos under it
PASS  permissions  /Users/you/.config/clankerbar/headless.json parses
WARN  toolchains   no grant for: go (/Users/you/dev/acme-cli)
                -> allow the verbs each one needs in /Users/you/.config/clankerbar/headless.json (e.g. Bash(go build:*), Bash(go vet:*), Bash(go test:*)) — a headless session fails closed, so an ungranted tool is refused with no prompt and its task ships unverified
PASS  budget       no ceiling configured — the loop stops on a STOP/HALT marker or signal; a dry backlog idle-polls rather than exiting
```

The checks: **config** (discovered, parses, validates — plus the resolved harness,
workdir, api key origin and derived backlog URLs), **harness** (binary on PATH
and runnable, with its version), **config_dir** (resolves, exists, looks initialised for the chosen
harness), **backlog** (creds present and the summary read succeeds — distinguishing
no creds, a rejected key, a `project_required` key/route mismatch, and an
unreachable endpoint — plus whether the queue is gated on *your* open questions,
or paused from the console), **state_dir** (the driver's own directory: writable,
no leftover control markers (`HALT`/`STOP` and the `ctl` restart/reload family),
and not sitting inside a configured workdir - a state
dir under one a session is spawned in is writable by that session, which hands it
the loop's own `STOP`/`HALT` switch; under a workdir nothing runs in *yet* it is
the same trap for the next `projects[]` entry that inherits it), **workdir** (per
project: it resolves, an `.mcp.json`
reaches it, and it carries an agent-instructions file), **permissions**
(harness-specific policy sanity), **opencode_ambient_config** (an opencode
config the driver never named that a session merges anyway: one whose
`clankerbar` server points at another project, or one carrying keys that shape
what a session may do), **toolchains** (the build tools the project's
repos need are actually granted), **power** (whether the machine will stay awake
long enough to do the work), **deploy_lag** (how far the deployed build lags the
project's integration branch), **pr_gate** (gh is on PATH *and* a real
`gh pr view --json` parses — decorated output is a FAIL, an unreachable GitHub a
WARN, and "no PR to read" says the probe was not exercised), and **budget**
(ceilings parse and are sane).

A multi-project config gets **one backlog check and one workdir check per
project** — one queue can be wired wrong while the others are fine. A project
entry that omits `workdir` or `mcp_config_path` inherits the top-level one, and
doctor resolves it **exactly the way the loop does**, so it never reports on a
directory your sessions will not use.

`deploy_lag` reads the plane's `/health` (`health_url`, top level or per
project) to learn WHICH commit is deployed, and compares it against the REMOTE
tip of `integration_branch` (default `staging`). A gap is warned about only once
its oldest undeployed commit is older than an hour - one fresh commit is normal
deploy cadence, 21 overnight is not - and the count and age are named either
way. A deployment AHEAD of the branch (a hotfix, or work not yet merged) passes:
it is newer than the shared line, not a stale deploy. The sentinel stamps
`unknown` and `<sha>-dirty` each get their own warning naming what they mean,
and anything doctor cannot establish (endpoint down; the commit held by no local
clone even after fetching the branch into them) is an honest WARN, never a green
line. Every git call it makes is bounded by its own deadline, so an unreachable
remote ends as that WARN rather than a hung preflight, and the fetch hunt is
capped in ATTEMPTS so a dead remote cannot turn it into one round trip per
clone. Unconfigured it prints one quiet PASS naming `health_url`, so the feature
is discoverable without nagging anyone who opted out; in a mixed multi-project
config the unmonitored projects still get their own quiet PASS rather than
disappearing from doctor's output.

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

### The dead-phase rate: `clankerbar dead-rate`

The opencode implement phase has been observed dying silently — a session whose
final step finishes with reason `unknown` and no branch recorded, so it produced
nothing. That rate is the number that decides whether a fix to the harness or
provider worked, and the driver now tallies it **live**: each phase session that
got past its claim counts as a phase run, and one that then died producing
nothing counts as dead, broken down by phase name and harness, and reported
after every drain with its denominator:

```
dead-phase tally: implement/opencode: 6 dead of 23 (26.1%); review/claude: 0 dead of 4 (0.0%)
```

A session that never got past its claim — a correctly refused takeover — counts
toward **neither** counter, because it satisfies both conjuncts of the dead
signature while being a correct refusal, not a death.

The same predicate, applied to the existing iteration logs, is available as a
subcommand — the retrospective view over the logs the driver has already
written, per day, per phase and per harness:

```sh
clankerbar dead-rate
clankerbar dead-rate --error tool_count_limit   # logs whose APIError events match
```

A session that ended on the **output-cap stall** (see the harness section: final
step reason `length`, zero output) is reported in its own `stalled` column,
never in `dead`. The two are different ends — a stall is a live, resumable
session, a dead one is not — and folding them together would hide which fix
moved which number. The `rate` column stays dead/run. The column counts that
shape in the logs, whatever harness wrote them; the resume-with-a-steer below is
the opencode adapter's response to it, not a condition of the count.

The scan is verified against known-positive controls. `--error tool_count_limit`
finds exactly the three 2026-08-19 logs that carry it as an APIError event, not
the later logs where the same string appears as task-body text an agent merely
read. Against the recorded 2026-08-20 hand count (6 dead of 16 implement
sessions, taken mid-day) it reconciles session by session: three of the six are
dead; the CLA-386 takeover held WIP inherited through its own claim result, so
it ran but did not die; the two CLA-390 sessions never got past their claim and
count toward neither counter; and two further deaths fall after the mid-day
snapshot. Over the full day the scan reports 5 dead of 23.

### Uploads: `clankerbar upload` and `clankerbar deck`

Two agent-facing subcommands put a file into a project's asset store and hand
back a reference to paste, so a screenshot, a video or a review deck is
described by its **path** rather than typed into a tool argument as base64. The
bytes are sent by the CLI itself, straight from the file on disk: what leaves
the shell is the file's path, content type, size and sha256 — never its
contents.

```sh
IMG=$(clankerbar upload shot.png)                 # stdout is exactly asset:<id>
clankerbar upload clip.mp4 --task CLA-581         # attribute the file to a task
clankerbar upload shot.bin --type image/png       # when the extension doesn't say
URL=$(clankerbar deck deck.html --task CLA-581)   # stdout is exactly the review URL
```

`upload` prints **exactly** `asset:<id>` on stdout — `IMG=$(clankerbar upload
shot.png)` is the whole point — and `deck` prints exactly the review URL;
everything a human wants to read goes to stderr. `deck` uploads the HTML as a
`text/html` asset and binds it to the task, so the task's review URL points at
the rendered deck.

The plane owns the rules: the content-type allowlist and the size caps are
checked on the declaration, and its refusal text is what you see, not a
paraphrase. The commands exit non-zero with a one-line stderr reason when the
file is unreadable, the type cannot be told and no `--type` was given, the plane
refuses the declaration, or the upload fails — the last names the HTTP status
and the plane's own error code, so a 401/403 is never mistaken for a hash or
size mismatch.

Auth is the same `CLANKERBAR_API_KEY` the daemon uses, and the project comes
from the daemon config or `--project <slug>`; the key is never printed,
including in errors. A **headless** session fails closed, so grant both
commands in its policy (`headless.json`) or they are refused with no prompt:
`Bash(clankerbar upload:*)` and `Bash(clankerbar deck:*)`.

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
clankerbar ctl restart    # finish the session, then re-exec at the iteration boundary
clankerbar ctl restart --now   # kill the in-flight session, release its claim, re-exec now
clankerbar ctl reload     # re-read the config file at the next boundary, no exec
```

`ctl` writes a marker into the same state dir (`RESTART` / `RESTART_NOW` /
`RELOAD`, read exactly where `STOP` is read), resolved from `-c <config>` —
plus `--workdir`, if the daemon was launched with that override — the same way
`run` resolves them. A **graceful restart** never cuts a session short:
the daemon finishes what it is doing, holds no claim across the iteration
boundary (the plane keeps all durable state, so resume is by construction),
then re-execs itself with the **same argv and environment**, through its launch
path - so restarting after installing a new build runs the new build. **Reload**
re-reads the config file at the next iteration boundary without replacing
anything: budget, ceiling, prompt and poll-interval edits apply to the next
iteration; harness, env and binary changes need a restart.

On a **supervised** child there is nothing to reach for: the file its `-c`
names is the supervisor's generated `materialized.json`, not the base config
you edit, and the supervisor itself rewrites that file and queues the `RESTART`
when your base-config edit lands (see "The base config is the machine layer").

One limit worth knowing before you reach for restart: **re-exec preserves the
daemon's CURRENT environment.** It cannot conjure env the daemon was never
launched with - a tokenless daemon restarts tokenless (that class of problem is
env ownership, not restart). And a leftover control marker causes a surprise
restart or reload on the next start - `doctor` warns about all of them, and
`ctl`'s own output says what to do if no loop is actually running against the
config you pointed it at.

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
`STOP`/`HALT` markers (and `ctl restart`/`reload`) remain the fallback there. Two misconfigurations hard-stop
(non-zero exit) instead of blind-draining doomed sessions: a revoked/wrong key
(`401/403`), and an account key with **no derivable project slug** (`400
project_required` from the legacy slug-less route) — give the loop a slug via
`projects` or an `/mcp/<slug>` `.mcp.json`.

### Visibility

The driver logs milestones to your terminal as it goes (timestamped): spawning,
queue state each poll, done + tokens/cost, usage-limit pauses, transient retries.

Those lines — the daemon's own record of what it *decided* — are also written to
**`<state-dir>/daemon.log`**, rotated at 8 MiB and keeping 3 files
(`daemon.log`, `daemon.log.1`, `daemon.log.2`), so a daemon that runs for weeks
does not leave one unbounded file and a monitoring agent does not need your
terminal. The state dir is the one `doctor`'s `state_dir` line prints
(`~/.local/state/clankerbar/loop/<workdir-slug>` by default); `doctor` also names
the log paths.

A session's own stream is **not** echoed there by default. It is the whole
harness transcript — prompts, tool arguments, tool output — and once it also
went to the daemon's stderr it buried the daemon's lines under thousands of rows
of scrollback, which is exactly the record that says why a run ended. Each
attempt is kept in full in its own `<state-dir>/iteration-<ts>.log`, and the
daemon's spawn and end lines name that file, so the two can be joined. The
Claude harness still renders the agent's progress — assistant text and `→ Tool`
markers — into that file.

To watch one session live again, set **`"echo_sessions": true`**: the stream is
mirrored to the daemon's terminal *as well as* written to the iteration log.
No harness gets a separate live channel of its own: claude's stderr is part of
its stream and lands in the log with the rest of it.

The fleet supervisor keeps its own lines the same way, in
**`<supervisor state dir>/supervise.log`** (`~/.local/state/clankerbar/loop/roster/supervise.log`
by default, size-rotated on the same terms) — refuse-to-start reasons, spawn
failures, exits and backoff, and the per-change fleet status listing. That is the
long-lived `clankerbar supervise` process; the one-shot `supervise roll` and
`supervise replace` write their own lines to stderr only, and exit. Children's
stderr still streams to the supervisor's own terminal, as before.

### Fleet visibility

The daemon also reports what it is doing to the plane, so the console's Fleet
page shows every instance — working *and* idle, where the run-based panel only
sees a daemon that currently holds a task. Two things are reported, both riding
cadences the loop already runs — presence on each backlog poll and on each
in-session lease-renewal tick, iteration history at each drain boundary — with
one exception: a run parked on a usage limit re-beacons on a ticker of its own,
because the pause is the one time that poll and session cadences both stop:

> **Release order**: a plane older than CLA-727 refuses an unknown
> `state.kind` for the WHOLE report (400), so `limited` has to be promoted on
> the plane — accepting `state.kind: "limited"` and `state.resetAt` — before a
> CLI that sends it reaches the fleet. Against an older plane every beacon is
> rejected, not just the `limited` ones, and the instance goes dark in every
> state.

- **Presence** rides each backlog poll: who the instance is (`instance_name`;
  when unset it resolves to the machine's hostname plus the config file's
  basename, e.g. `Jasons-MBP/clanker1`, so several daemons started from
  different configs on one host stay distinct; set `instance_name` to override),
  the host and binary version, a
  fingerprint of the config in force, and the state — `idle`,
  `iteration` {n, taskRef, phase} while a session runs, `draining` while a pause
  holds spawns, `limited` {resetAt} while the run is parked on a harness usage
  limit (with the stated reset instant when the harness stated one), `stopping`
  as the final beacon when the run ends. State changes
  beacon at phase boundaries too, so the page does not wait out a poll interval
  to learn which phase is running; a session in progress re-beacons the same
  `iteration` state on every lease renewal (a third of the plane's 30-minute
  lease, so ten minutes apart at most), because the poll does not run while a
  session does and a session longer than the plane's 15-minute liveness window
  would otherwise read as a dead instance — and that tick keeps firing, without
  the lease beat, after renewal stands down on a plane that refuses it, since
  the fleet report is a different transport and a healthy instance must not go
  dark mid-session; every other project the daemon serves is re-beaconed its own
  `idle`/`draining` state on the same tick, for the same reason one project over;
  and a limit wait re-beacons `limited`
  every five minutes — well inside the plane's liveness window — so hours spent
  waiting out a cap do not read on the console as a dead instance. The plane
  stores exactly what you report; it infers nothing.
- **Iteration history**: exactly one record per drain, posted at the boundary it
  just crossed — the task, the phases attempted, how it ended (`checkpoint`
  work left for a takeover / `released` back to the queue or settled /
  `parked` for the operator / `dead`), duration, tokens, and the drain's
  harness-reported dollar spend (`costUsd`; zero means no harness reported a
  cost). The record also
  names every session the drain spawned, in spawn order, with the label of the
  phase it ran, the harness that ran it, the model it was spawned with (empty
  when the harness default applied), and what that session spent: the token
  usage as a normalised breakdown — `input`, `output`, `reasoning`,
  `cacheRead`, `cacheWrite`, five disjoint buckets summed across the session's
  own steps, so the figure can be repriced later — plus the harness-reported
  dollar figure, when there is one. The breakdown is what the operator's cost
  analysis reads; the dollar figure is labelled as harness-reported because on a
  subscription it is the API-rate equivalent, not what was paid, and it is
  omitted for a harness that reports no cost at all (codex today), and for a
  session that reported no usage at all — the plane refuses a report carrying a
  session's cost without its usage report, so the figure rides only when the
  usage report does.
  The plane keeps a session's usage on the task, as a durable `phase_session`
  record (`list_phase_sessions` reads it back), alongside the run that session
  worked under — the run's UUID, never the credential the session holds, which
  carries a bearer secret and is stripped off on the way out — so the cost of a
  round of review can be attributed to the round it belongs to.

Everything is strictly fail-soft telemetry: a failed report is logged once and
dropped, never retried (v1), and can never block, delay, or fail the loop, a
claim, or a phase. Reports go to `/api/projects/<slug>/fleet/report` on the same
trusted origin as every other credentialed call and are authenticated by your
project API key - the same key the sessions use, never anything extra.

`clankerbar doctor` checks the wiring in its PASS/WARN table (`fleet`): whether
a report endpoint could be derived and is reachable, with the key accepted -
verified without writing anything, so a cron'd doctor cannot fight a live
daemon's presence row.

### Checking what a session says it delivered

The control plane holds the backlog and takes a clanker at its word. When a session
records a branch (`record_progress(branch: …)`) or declares a delivery merged
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

#### The PR a delivery names

When a session's delivery names a pull request, the driver also checks it through
the `gh` CLI - the one part of the check that reaches GitHub rather than your
local tree. Two conditions, both required:

- **The PR is MERGEABLE.** A `CONFLICTING` PR is refused: a conflicted branch
  gets no `pull_request` event at all (GitHub cannot compute the merge ref), so
  it reports zero checks rather than a failing one, and quiet CI proves nothing.
- **The check rollup actually ran and passed.** An **empty** rollup is a
  refusal, not a pass - "no checks found" is the exact signature of the
  conflicted PR above, so silence never reads as green. Skipped/neutral checks
  (path-filtered jobs) pass but are named in the log.

GitHub computes mergeability lazily, so an UNKNOWN first read is polled within
a bounded window (30s) and then **refused**, never assumed. Failing or
unfinished checks refuse by name. If `gh` is not installed the check degrades
to an explicit could-not-verify - never a pass - and `clankerbar doctor`'s
`pr_gate` line warns about it up front.

The gate parses `gh`'s `--json` output, so it owns that child's environment:
`GH_FORCE_TTY` is **removed** (from the inherited environment too — gh treats
ANY value as "force terminal-style output", and the `GH_FORCE_TTY=0` this gate
shipped itself turned colour ON) and `NO_COLOR=1` / `CLICOLOR=0` are set. When
the output still does not parse, the delivery is **refused**: gh ran and its
answer is unintelligible, which is a defect in the gate's own contract, not an
absence of evidence. That is deliberately *not* the same as "could not verify":
a missing `gh` or an unreachable GitHub stays an explicit could-not-verify,
while a parse failure refuses. Until CLA-776 the difference was invisible —
every real delivery logged "could not verify … carrying on" for two months.
A remote whose host is an SSH config alias (`git@github-personal:…`) is resolved
through `ssh -G` and checked when the alias lands on github.com, rather than
being skipped as "not a github.com repository".

**Repos with no CI at all:** refusing every delivery there would wedge the
driver shut, so a per-project opt-out exists - set `"allow_unchecked_pr": true`
(top level, or on a `projects[]` entry) and an empty rollup is logged as a
WARNING instead of a refusal. The mergeability condition is never relaxed: a
conflicted PR refuses either way. `doctor` prints the resolved state per
project, so a loose project is visible before it fires.

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
records the branch on the task** — turning a redo into a takeover (with the one
exception below, for a tip the session already delivered):

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

**Untracked files alone, at a tip the session already delivered, are left alone.**
The driver asks the origin rather than the session: when the branch the session
recorded is on the remote at **exactly** the worktree's HEAD, the work is pushed
and there is nothing left to rescue, so the untracked files are left uncommitted
and logged. What is in them is not known here — a `wrangler types` dump and a
source file written after the last commit are the same shape in a status line —
and only a reader can tell which of them is disposable. Committing them would push
a **new** tip at the phase boundary, and every reader downstream — each
review-panel member, the fix phase, the PR — would then review the salvage commit
instead of the tip the session handed over, which is precisely what one implement
phase cost before this rule existed.

Delivery is the whole discriminator, and it is checked, not assumed: an edit to a
**tracked** file is salvaged on top of a delivered tip like any other uncommitted
work, and an undelivered session's untracked files — the new module it never
staged — are salvaged in full, which is what the rescue above is for. If the origin
cannot be read the answer is "not delivered" and the work is saved exactly as
before: the failure mode of this check is the old behaviour, never a quiet loss.

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

Flags override the config file, which overrides defaults. Exactly one file is
discovered on its own: `~/.config/clankerbar/config.json`. Any other file has to
be named - `clankerbar run -c ./clankerbar.json` - and an unnamed
`clankerbar.json` sitting in the directory you launch from is **refused, not
read**; [where the key may go](#where-the-account-scoped-key-is-allowed-to-go)
says why. (JSON today; TOML is the likely final format.)

Under the fleet supervisor, that same directory's machine-layer role is just
the one discovered file: `config.json` is the **base config** each supervised
child's own config is generated from (see [The fleet
supervisor](#the-fleet-supervisor-bare-clankerbar)). Which instances run - and
under what identity - comes from the plane's account-scoped roster, not from
files in the directory: a per-daemon file like `clanker2.json` is not an
instance the supervisor reads.

```json
{
  "harness": "claude",
  "backlog_url": "https://clankerbar.com",
  "model": "opus",
  "models": { "strong": "opus", "standard": "sonnet", "cheap": "haiku" },
  "prompt": "Work the next backlog item.",
  "max_turns": 400,
  "max_session_wall_clock": 0,
  "mcp_config_path": "./.mcp.json",
  "config_dir": "~/.claude",
  "instance_name": "",
  "env": {},
  "idle_poll_interval": "60s",
  "poll_interval": "30m",
  "max_retries": 0,
  "retry_cap": "5m",
  "max_zero_spend_attempts": 3,
  "max_repeated_tool_failures": 5,
  "echo_sessions": false,
  "budget": {
    "max_tokens": 0,
    "max_cost_usd": 0,
    "max_wall_clock": "6h",
    "max_session_tokens": 0,
    "per_harness": {}
  }
}
```

`echo_sessions` (default **false**) restores the old live echo of a session's
stream to the daemon's terminal. Off, the stream goes only to its iteration log,
which is what keeps the daemon's own lines readable — [Visibility](#visibility)
has the reasoning and where both logs live.

`clone_url_base` (default `https://github.com/`) is the URL **prefix** the
supervisor's workdir derivation clones a missing repo from:
`<clone_url_base><owner>/<name>.git`. Set it to an ssh host alias such as
`git@github-personal:` to give derived fleet clones an ssh `origin`, so a
session's fetch and push use the key rather than a GitHub token in its
environment; any other mirror prefix — `https://git.example.com/mirror/`,
`file:///srv/mirrors/` — works too. It is read only by the supervisor: a plain
`run` has no clone to make. The value has to end where the owner begins, so a
URL or path prefix ends with `/` and an scp-style ssh alias ends with `:`.
Anything else is refused (`doctor` and `supervise` both name the key) rather than
concatenated into a URL nobody can clone. An ssh base spawns no
`gh auth token` at all while cloning; the HTTPS github.com default keeps
resolving the owner's token exactly as before. Existing clones are never
re-cloned, so a change affects only repos cloned after it is set.

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

`max_session_wall_clock` is the same backstop measured in TIME, for a harness
whose CLI takes no turn flag: when a session outlives it the adapter ends the
process and the driver treats that exactly like a hit turn cap — the session
ends, nothing is retried, nothing fails, and the run carries on (CLA-368). A
phase's own `max_wall_clock` wins over it, and **0 is off, which is the
default**: unlike the turn cap it ships no built-in number, because a duration
that catches a runaway on one model/provider is a routine session on another. It
is measured per SESSION, so it is not `budget.max_wall_clock`, which is the
run-wide ceiling and counts the hours a run spends *waiting out* a usage limit.
Since CLA-406 the cap bounds an opencode session's WHOLE life including any
quiet-death resurrections: the budget is computed once at spawn and spent down
across the original run plus up to 5 resume rounds, not refreshed per round.
The inter-round overhead (25s backoff plus a 30s probe box per round) is
charged against it like any other time, so a fully-capped session does not
materially outlive the dial; a resurrection round is not started unless the
remaining slice covers that overhead plus a minute, and a continuation is not
started on a remaining slice under a minute.

**opencode only** today — it is the harness with no turn flag, so this is its
backstop; under claude or codex the dial is inert and `doctor` says so. Two
consequences of that pairing are worth knowing before you set it. First, the
salvage applies here: since CLA-365 opencode observes the session's task claim,
so when a capped session ends, whatever it left uncommitted is committed and
pushed by the salvage, the same as for a turn-capped `claude` session - unless
the salvage refuses (an interrupted merge, rebase, cherry-pick or revert;
unresolved conflicts; an ambiguous worktree match) and leaves the work
untouched, or the push is rejected and the work stays committed locally only.
Both of those are named in the log. If instead no worktree at or below the
run's workdir has a branch starting `clanker/<first 8 characters of the task
id>`, the salvage finds nothing to act on and stays silent.
Second, `phases` runs on opencode on that same claim-tracking basis (a
multi-phase sequence is refused over a harness that observes no claim, which
is codex today), so the per-phase `max_wall_clock` is reachable in a phased
config too - though such a run still has no per-session turn or token cap (see
`max_turns` and `budget.max_session_tokens` above), so set this dial
deliberately.

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
| `opencode` | passed as `OPENCODE_CONFIG`, plus a rewritten copy (every server out of Code Mode, the permission policy added) as `OPENCODE_CONFIG_CONTENT`. It **must be a strict-JSON opencode config** - servers under `mcp` (1.x dialect, rewritten) or `mcp.servers` (2.x), not Claude's `mcpServers`. opencode 2.x skips a Claude-shaped file's `mcpServers` key, so every session would run with no clankerbar tools. |

Under `opencode`, set `mcp_config_path` **explicitly** to an opencode config. Leaving
it out does not opt out: an empty value re-runs the `<workdir>/.mcp.json` discovery,
so a workdir that carries a Claude `.mcp.json` hands it over no matter what. Run
`clankerbar doctor` - it prints the caveat instead of a verdict where the file is not
read, and FAILs the workdir check when `opencode` is pointed at a Claude-shaped one.

#### A discovered `.mcp.json` may not choose what runs

A file that arrives through the default above was FOUND, not named - and a checkout
can write it as easily as you did (sessions run with edit permission in that
directory, and the damage would land on the next unattended start). Since it is
handed to the harness whole, a config nobody pointed at is held to three rules:

1. **An MCP entry carrying a `command` - a locally spawned server - is refused.**
   It starts at session init, before any permission rule applies, running as you
   with the session's whole environment (`CLANKERBAR_API_KEY` included). Entries
   you meant go under `"allow_local_mcp_servers": ["docs"]` - top level, or on a
   `projects[]` entry (which replaces the top-level list for that project). The
   other way out is naming the file: an explicit `mcp_config_path` adopts it
   wholesale.
2. **Keys that decide what a session IS are refused outright.** Under opencode the
   file is the session's ENTIRE config, so a discovered one may not set
   `permission` (it would replace the fail-closed policy the driver pins),
   `plugin`, or `agent`. No allowlist reaches these; name the file if you mean them.
3. **A discovered file that cannot be read is refused**, not passed through.

`allow_local_mcp_servers` gets the same visibility as every other loose state:
`doctor`'s `mcp_servers` check reports the list beside the entries it admits. A
file you NAMED with `mcp_config_path` is exempt from all three rules - naming it
is the statement that you vetted it - and its local servers are still disclosed
by doctor's WARN as they always were.

Point it at *your* workdir, not at a checkout of this repo. The `.mcp.json` at the
root here is the maintainers' own agent wiring and names the `clankerbar` project
slug; running the loop from inside this checkout would have it poll a queue you
cannot read, which it refuses rather than drains.

#### `env`: the daemon owns each session's environment

`env` declares extra environment for spawned sessions, so load-bearing variables
do not depend on how the binary happened to be launched - with `GH_TOKEN`
declared here, the wrapper script that used to export it stops being
load-bearing and starting `clankerbar run` directly is safe (CLA-462). A value
is either a literal string or a command whose stdout becomes the value:

```json
{
  "env": {
    "GH_TOKEN": { "fromCommand": "gh auth token -u your-account" },
    "CLAUDE_CODE_OAUTH_TOKEN": "@~/.secrets/claude-oauth"
  }
}
```

The map overlays at four levels, most specific wins per key, in the same order
the MCP config uses: top level, then `harnesses.<name>.env`, then
`projects[].env`, then that project's `env_per_harness.<name>`. In multi-project
mode declare each project's token on its own entry, so no session ever carries
another project's credential.

Three properties carry the security and reliability weight:

- **Command-derived values are resolved fresh at every spawn.** A rotated token
  reaches the next session with no daemon restart. A command that fails, times
  out (10s), or prints nothing REFUSES that spawn with a log line naming the
  variable - never a silent partial env, because a session missing its declared
  environment is the incident, not a degraded mode.
- **`@path` secrets stay owner-only, enforced.** A file any other local account
  can read refuses the spawn, and so does a file that has been emptied - an
  invisible token rot must fail at spawn with the variable named, not inside
  git later.
- **Values come from the operator's own config only.** Nothing in a workdir or
  checkout feeds this map; the credential-origin rule for the account key is
  unchanged.

Variable names must be valid names (`[A-Za-z_][A-Za-z0-9_]*`) and commands must
be non-empty; both are refused at load. `doctor` runs every declared command and
re-checks every `@path` file, so a source that has stopped working is a FAIL in
the preflight instead of a refusal at 3am, and it WARNs when a project declares
repos but no `GH_TOKEN` source anywhere.

#### `env_allowlist`: what a session may inherit

By default a spawned session inherits the daemon's **whole environment** — every
variable in the shell (or launchd unit) that started the supervisor, including
tokens for services the session has no business with. `env_allowlist` makes that
inheritance a whitelist: with it set, every inherited variable NOT named there
is dropped from the harness process.

```json
{
  "env_allowlist": ["HOME", "PATH", "CLANKERBAR_API_KEY", "SSH_AUTH_SOCK"],
  "env": { "GH_TOKEN": { "fromCommand": "gh auth token -u your-account" } }
}
```

- **The `env` block still applies on top.** The allowlist filters INHERITANCE
  only; a value declared in `env` (or `@path`, or `fromCommand`) is an
  instruction for the session and is always present. That is how `GH_TOKEN`
  and `CLAUDE_CODE_OAUTH_TOKEN` arrive, and a `fromCommand` may read a variable
  that is not on the allowlist because it runs in the daemon, before the filter.
- **The daemon keeps its own environment.** The filter is applied at the last
  hop, where the harness process is spawned. The daemon needs its environment to
  resolve `fromCommand` values and to read git remotes; the session — the
  model-driven process — is what the list governs. This bounds what is HANDED
  OVER, not what a determined same-user session can reach: on a platform where
  one same-user process can read another's environment (Linux
  `/proc/<pid>/environ`), a session that goes looking by PID can still recover
  the dropped variables from the daemon's own environment.
- **Four levels, whole-list replacement.** Like `env` it is declared at the top
  level, per harness, per project, and per project-per-harness
  (`env_allowlist_per_harness.<name>`), but the most specific level that
  declares one WINS ENTIRELY rather than merging. An allowlist is a complete
  answer to "what may this session see"; unioning levels would make a narrower
  level unable to remove a variable a broader one admitted. A
  `env_allowlist_per_harness` key must name a registered harness: an unknown one
  is refused at load rather than left inert, because a key nothing consults
  would silently leave that harness on full inheritance.
- **Omit the key for today's behaviour.** An absent key means full inheritance,
  and a config that says nothing about it is byte-identical to before. An empty
  `[]` is refused by validation and `clankerbar doctor` as a half-edit — the
  only way to say "everything" is to omit the key.

`doctor` reports which shape is in force, prints the variable names that will
reach a session (never values), and **FAILs** when the list omits one of the
variables the fleet needs:

- `HOME` — git and ssh read their configuration from it;
- `PATH` — the harness and every command a session runs;
- `CLANKERBAR_API_KEY` — the MCP config carries it as
  `Bearer {env:CLANKERBAR_API_KEY}`; without it the session cannot reach the
  plane;
- `CLANKERBAR_WORKDIR_ROOT` — required while the workdir derivation is on
  (`project_root` on a generated config, or the variable in a base config's
  environment), because it names the fleet layout every brief works in.

Missing one of those does not fail the spawn; it fails later and elsewhere
(`repo_not_found`, an unauthenticated plane), which is exactly the failure
`doctor` exists to move forward.

### Where the account-scoped key is allowed to go

`CLANKERBAR_API_KEY` is an **account-scoped bearer token** — one key covers every
project you belong to, not just the one being drained. So *where* it may be sent is
pinned, not inferred, and the pin is your own `backlog_url`:

- **`backlog_url` is the one trusted origin.** It is the only origin the driver
  ever sends the key to. It defaults to `https://clankerbar.com`; to point at a
  self-hosted plane, name it there instead (e.g. `"backlog_url":
  "https://plane.example.com"`). It is **no longer derived from `.mcp.json`** — a
  checkout's `.mcp.json` contributes at most a project slug, and never redirects
  the key.
- **Set it in a config file the sessions cannot rewrite.** `backlog_url` is read
  from `~/.config/clankerbar/config.json` - the one file discovered
  automatically - or from a file you name with `--config`. A `clankerbar.json`
  sitting unnamed in the directory you launch from is **refused, not read**, and
  the loop stops rather than falling back to your home config: that directory is,
  by default, the checkout the spawned sessions edit, so a session could rewrite
  the field that decides where tomorrow's key goes. Naming it
  (`-c ./clankerbar.json`) does lift the refusal - but it lifts only the
  *implicit adoption* and guards the file no further, so keep `backlog_url`
  itself in `~/.config/clankerbar/config.json`.
- The scheme floor is TLS: `https` anywhere, or `http` only to loopback (a plane
  under local development).

Fixating on the origin is what makes the file-for-credential leak closeable. The
MCP config is handed to the harness whole, and the harness's session carries the
key in its environment, so a checkout's `.mcp.json` could name any host it liked;
that is the exfil it refuses rather than silently polls.

`clankerbar run` logs where the key will go, once, at startup (with the usual
`clankerbar: <time>` log prefix in front of it):

```
clankerbar: 12:54:24 sending the API key to https://clankerbar.com
```

and `clankerbar doctor` prints the same origin as a preflight line:

```
api key origin: https://clankerbar.com
```

That line is the thing to check in an overnight log: if it names an origin you
did not choose, stop the run before it drains anything.

**Refusals, each with its remedy.** A config or MCP file that would move the
account key off that origin is refused at startup and flagged by `doctor` — never
silently dropped, because dropping the file would spawn a session with no
clankerbar tools and read as "the backlog was empty":

| Refusal | When | Remedy |
| --- | --- | --- |
| **Foreign origin** | an MCP server that carries the key points at an origin other than `backlog_url` | set `backlog_url` to that origin if you mean it, or fix the file — a checkout's `.mcp.json` is not trusted to redirect an account-scoped key |
| **Cleartext non-loopback** | `backlog_url` (or a server URL) is plain `http` to a host that is not loopback | use `https` — the key would cross the wire in cleartext |
| **Local server handed the key** | a server entry that carries the key (`clankerbar`, or any entry whose headers/env reference the variable) declares no `url` — a local/stdio server | give the key only to a URL-based (HTTP) server on `backlog_url` — a spawned process may send it anywhere |
| **Unparseable MCP config** | an MCP config that exists (at `mcp_config_path`, or a per-harness/per-project equivalent) cannot be read or parsed, so its destination cannot be checked | make the file readable and valid JSON so the check can see where it points |

Each of those errors is written to carry its own remedy, so they read as policy
rather than as a bug.

### Multi-repo projects: `repos` and `primary_repo`

A task can name the repo it belongs to, and a session should start in THAT
checkout rather than somewhere unconsidered. Two keys configure it - top level,
or per project in multi-project mode, where a project's own non-empty values
replace the top-level ones:

```jsonc
{
  "repos": {
    "lecstor/clankerbar-cli": "~/dev/clankerbar-cli",
    "clankerbar": "../clankerbar"
  },
  "primary_repo": "lecstor/clankerbar-cli"
}
```

**`repos`** maps a repo identity to its local checkout. The key is what a task's
`repo` field carries: the full `owner/name`, or just the bare name. The value is
a directory path, relative to the workdir or starting with `~`. It holds paths,
never credentials.

**`primary_repo`** names where a session starts when its task carries NO repo:
a `repos` key, or any identity resolvable by the same steps below. With exactly
one repo declared and no primary named, that one is primary implicitly. Left
empty with two or more repos declared, a repo-less task fails the iteration
instead of guessing.

Resolution of an identity to a checkout, first match wins:

1. a `repos` entry keyed by the full identity (`owner/name`);
2. a `repos` entry keyed by the bare name;
3. the workdir itself, when its basename IS that bare name;
4. `<workdir>/<bare name>`, when such a directory exists;
5. nothing: the iteration fails loudly with `repo_not_found` naming the
   identity, rather than starting the session in the workdir anyway.

With NOTHING configured at all the feature is off and the legacy workdir
behaviour runs unchanged, so existing single-repo configs need none of this.
Every resolved path is verified to exist as a directory before use; doctor
reports each entry's resolution so a broken path is seen before it fails a run.

**What each harness receives.** Declaring repos also widens every session's
reach past its spawn checkout, so a session started in repo A can still work on
repo B:

- **`claude`**: `--add-dir` on the session argv, followed by one argv ELEMENT
  per extra checkout (the flag is variadic over separate elements). The
  settings file's deny rules still govern what it may actually do there.
- **`opencode`**: an `external_directory` allow per declared checkout in the
  pinned `permission` policy, matched absolutely regardless of cwd, plus a
  read/edit allow in the instance-relative `../<checkout>/**` form.
- **`codex`**: nothing on argv. Codex's sandbox has no per-invocation flag for
  extra writable roots; declare them yourself under
  `[sandbox_workspace_write] writable_roots` in your `config.toml`. The driver
  does not rewrite a sandbox you own.

The conventional task-worktree area beside a checkout (`<checkout>-wt` as its
sibling) joins the grant list automatically whenever that directory exists, so
a session can create and edit the worktree it is told to work in.

Under the per-project layout (`project_root`, see "Workdir derivation" above)
the reach is the whole PROJECT instead: the project root and every directory
inside it — each repo of the project, and `worktrees/` — are granted, whether
or not they are declared, and a repo the fleet clones in later is in scope with
no config edit. Everything outside the project root stays closed: a session in
`<root>/<project slug>/<repo name>` cannot read a sibling project under the
same root, nor anything above it. Because the grant is the project root rather
than a declaration, it is asserted by the config flag and never inferred from
the directory layout — a developer's multi-repo parent (`~/dev` and its ~150
trees) is never widened just because a session runs inside it.

### PR bases: `pr_base`

The phase briefs tell a session which branch its PR targets, and which branch a
resumed task branch must be synced with before its work is trusted. That branch
is a per-REPO fact, not per project: the makesPDF plugin repos merge into
`main`, lecstor/makespdf and lecstor/clankerbar merge into `integration`, and
this repo's PRs target `staging`.

`pr_base` maps a repo identity (full `owner/name`, or the bare name) to the
branch that repo's PRs target - top level, or per project in multi-project
mode, where a project's own non-empty map replaces the top-level one:

```jsonc
{
  "pr_base": {
    "lecstor/makespdf": "integration",
    "lecstor/clankerbar": "integration"
  }
}
```

Without an entry, the driver resolves the branch per repo at phase-spawn time:
the project's `integration_branch`, when that branch exists on the origin
remote, else origin's default branch (both read with a scoped
`git ls-remote --symref origin HEAD`, so a private github remote is read as
the account that owns it). A probe that cannot run - offline, no origin, no
git - falls back to `integration_branch`, which is what the briefs said before
`pr_base` existed, and a fallback never blocks a spawn.

`integration_branch` keeps its own meaning unchanged: the branch whose tip
`doctor`'s deploy_lag check judges the deployed build against. A repo can
deploy from one branch while its PRs target another, which is exactly why the
two knobs are separate.

### Execution config from the plane

A project's execution dials - the **pipeline** (`prompt`, `phases`,
`review_panel`), harness, model, tier buckets, budget, review-tier escalation
rules, and the two backstops (`max_turns`, `max_session_wall_clock`, as integer
seconds) - can live **on the plane** instead of in this file. The daemon reads
each project's stored document (per project, so two queues can run two postures)
and overlays it over this file; until a project has one, this file rules byte
for byte. `doctor` reports which is in force per project, warns when a stored
dial cannot fire on your machine (a turn cap under opencode, say), and **fails**
when a stored step names a harness this machine cannot spawn (no
`harnesses.<h>` block) - shape-validates on the plane, machine-fit is doctor's.

The pipeline is **one section**: a stored document carrying any of the three
pipeline keys replaces all three, so a stored `phases` clears a local `prompt`
and `review_panel` rather than running beside them. A document that carries none
of them leaves the local pipeline alone. A phase may name a `model` directly,
exactly as in the local file (a configured phase sets `model` or `tier`, not
both).

`review_panel_concurrency` is **machine-owned**, not part of the section and
never taken from the stored document: the phase sequence, every phase's harness
and model, and the panel's composition are policy, so the run's result is the
same on every machine, while how many panel seats run at once is how fast that
machine gets there. The local file's value stays in force whatever the document
carries.

A panel member's `optional` flag travels with the member when a document is
**applied**: a stored trial seat stays a trial seat, never silently promoted to
a required reviewer. It cannot yet be **proposed**: the plane's stored-run-config
schema refuses `review_panel[].optional` as an unknown panel key, so
`propose-config` refuses a document whose panel carries a trial seat — rather
than emitting a key the plane would reject, or dropping it and promoting the
seat on every machine applying the pipeline. Keep a trial seat local until the
plane's stored schema learns the key and this build's `$schema_version` moves in
step; the two sides must land together (the phase-level `model` key needed the
same lockstep).

Edits apply at the **next iteration boundary**, never mid-session: the daemon
watches the version on the backlog-summary poll it already makes, and on a bump
re-reads and re-overlays before spawning again. A document that fails local
validation keeps the previous config and says so loudly. Flags still outrank the
stored document: `--model` on the command line wins over a ratified value.

Import today's file as the starting point with:

```sh
clankerbar propose-config            # --slug <project> when projects[] names several
```

That records a PENDING proposal; nothing changes until you ratify it in the
console (Settings -> Run configuration). The pipeline travels with it — `prompt`,
`phases`, `review_panel`, and a phase's `model` — along with the
model/tier/budget/escalation dials. `review_panel_concurrency` deliberately does
not travel: it names this machine's parallelism, not the pipeline's result.
Wiring, env and paths stay local too: the plane custodies policy, never machine
fit or credentials.

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
`max_iterations`, and the control markers (`STOP`/`HALT` plus the
`ctl restart`/`reload` family) stay **instance-global** — one
operator, one spend pool; the console pause is per project. With no `projects`
list, the top-level fields drive a single project exactly as before.

### Resilience

A fresh session can die on a server blip (API 5xx / overloaded) or a network
hiccup — nothing to do with the task. These are **retried with exponential
backoff** (30s → 60s → … capped at `retry_cap`), re-running the same iteration; a
fresh session reclaims any half-done task, so a retry costs minutes, not work.
Detection is anchored (Claude's `API Error:` prefix, connection-error strings), so
a task log that merely *mentions* an HTTP 500 isn't mistaken for a dead session,
and under Claude a `400` still stops. `max_retries: 0` (the default) means **never give up** —
keep retrying at the ceiling until the API recovers, right for a daemon; set a
positive number to bound it. A usage-limit pause and a transient retry both re-run
the same iteration and neither advances the iteration count. `STOP` stays
responsive during any wait — and so do the restart/reload markers (`ctl
restart`, `--now`, `reload`).

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

**A session that keeps making the same failing tool call is killed.** When the
same tool returns the same error on `max_repeated_tool_failures` (default **5**)
**consecutive** tool calls, the harness kills the session mid-stream (CLA-739).
On 2026-10-20 nine review-panel sessions each called one clankerbar tool with an
argument the plane refuses, 65 to 316 times per session, and every retry re-sent
the whole context: ~273M tokens in ~45 minutes, until the provider's 5-hour window
ran out. The inputs are not compared (the incident varied an argument while the
error stayed identical); any other tool call in between, or a success, breaks the
streak. A killed session is classified **dead, no delivery**: the daemon log
names the tool, the count and the error (`repeated failing tool call:
clankerbar_update_task x5: invalid_release: ...`), the same text rides the
session's `cause` on the fleet report, an implement phase goes down the
dead-phase retry ladder, and a review-panel member is recorded as a failed member
with that cause. It is never retried as a transient failure and never stops the
run. `0` turns the check off; unset is the default; a negative is refused.
Enforced by the `claude` and `opencode` adapters, which see tool results as they
stream; inert under `codex`.

**And one more thing is bounded: retries taken on a failure the harness could not
name.** `opencode`'s error surface changes between versions, so beyond the anchored
patterns it also *guesses*: an exit 1 from a session that had already reported
usage is retried, on the reasoning that a session which authenticated and did paid
work hit something mid-flight rather than a bad config. That guess is what carries
a stream drop from a version the patterns haven't caught up with — but a
**deterministic** fault satisfies it just as well, and every dial above is blind to
that one. `max_retries` and `budget` are off unless you set them, and
`max_zero_spend_attempts` cannot fire here at all, because the reported usage the
guess requires is exactly what resets its counter. So a guessed retry is capped at
**2 per phase**, after which the run stops with its own distinct error quoting the
diagnostic nothing recognised. Retries the harness *did* recognise are not counted
against it — a provider flapping 5xx is still yours to bound with `max_retries`.
The cap is deliberately not configurable: the dials you set are for failures the
tool understands, and the honest number of guesses is small at any budget. If you
hit it, the error text is the thing to report — it is a failure shape the adapter
should learn a pattern for.

**The quiet-death signature.** A session that dies with a final step whose
reason is `unknown` and whose own usage is all-zero is the quiet-death
signature. On opencode 2.x the final step is read from the session export,
because the stream no longer reports it. The cause was a 1.x client defect, not
the provider - see
[`docs/opencode-build.md`](./docs/opencode-build.md). The marker itself is
described in
[`docs/opencode-tool-schema-limits.md`](./docs/opencode-tool-schema-limits.md),
which is a separate failure with a separate cause.

Since v0.10.1 the adapter does not accept the signature as terminal: it backs off,
resumes the SAME session in place (`opencode run -s <id>`), and asks the
agent to prove it is intact by naming its task ref. A mechanical match continues
the session where it left off (bounded at 5 resurrections per session, one probe
per death); a failed probe falls through to the dead-phase path unchanged.

**The output-cap stall (CLA-584).** A session whose FINAL step carries
reason `length` with zero output tokens (read from the session export on 2.x) is a different end: the model spent the
whole step's output budget thinking and emitted nothing. It is not the quiet
death (that is reason `unknown` with all-zero usage, and its stream was dropped)
— opencode ended this turn normally and the session is alive, so the adapter
resumes it exactly ONCE with a steer: do not retry the approach, and upload file
contents by path (`clankerbar upload` / `clankerbar deck`, which the steer names)
rather than transcribing them into a tool argument. A second
consecutive stall ends the session, and the daemon log names it
(`stalled: output cap hit with no output (reasoning=<n>)`) instead of reporting
a bare "never moved the task on".

**What gets read as a failure.** A session's output is the whole event stream, and
the events quote the backlog verbatim — the task the session claimed is sitting in
the same bytes. So both classifications read only what the *harness* said: its
stderr, its own non-event output, its typed error events, and Claude's
`terminal_reason`. A task whose body happens to say "hit your", "usage limit" or
"api error: 500" is narration, and narration is never a cap and never a blip.

Scoping to the harness is necessary and not sufficient, which is why the patterns
are anchored on JSON fields and word pairs rather than single words: a harness's
stderr still carries text it did not write — a provider SDK's error prose, a Node
stack trace — and the MCP SDK names its own classes `StdioClientTransport` and
`SSEClientTransport`, so a bare `transport` would read a dead MCP server (a config
fault that must stop the run) as a network blip to retry.

**Handing the task back.** A claim carries a 30-minute lease that the session
heartbeats. When a session ends mid-task, that lease is left ticking with nobody
renewing it — and clankerbar charges the task a *reclaim* to sweep it up, of which
there are only two before the task is parked for a human. So the driver watches
the session's stream for what it claimed, and hands the task straight back
(`release_task`, the driver-marked variant) on **every** exit: usage limit, transient blip,
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

   **A mixed-harness run needs one block per harness - `budget.per_harness`.**

   ```json
   "per_harness": {
     "claude": { "max_tokens": 75000000 },
     "opencode": { "max_cost_usd": 2 }
   }
   ```

   The dials above are one ceiling over every session a run spends, and no single
   number means the same thing on two backends: 75M tokens is a sane week of
   Claude and roughly $2 on a DeepSeek-class backend, so the same `max_tokens`
   that paces a week ends a metered drain after a task or two; and a dollar figure
   is meaningless for a session billed to a subscription, which reports a price
   nobody is charged. So each harness can carry its own block, keyed by harness
   name, counted over **only that harness's own sessions** and measured in the
   unit that harness understands - tokens for `claude`, dollars for a metered
   backend. **Any block that trips stops the whole run**: these are circuit
   breakers, not per-harness quotas.

   A token ceiling in a block derives the per-session runaway detector
   (`max_session_tokens`) exactly as the run-wide `max_tokens` does, so moving a
   ceiling into a block does not quietly loosen it.

   The dials above keep working exactly as they always have, over the run's whole
   spend, and a config that sets no `per_harness` block behaves as before. Set
   both and both apply, whichever is reached first. Wall clock is deliberately not
   available per harness: it measures the run, not a harness. The same goes for
   the CLA-262 stop above - a session whose spend cannot be measured stops the run
   when *that session's harness* is under a spend ceiling, its own block's or the
   run-wide one.

   `doctor` reads these the way it reads every other ceiling: what can actually
   fire. A block naming a harness this config does not run, or a `max_cost_usd`
   on a harness that never reports cost, is annotated `INERT`, and a config whose
   only ceilings are unreachable is told it has none. A block keyed by a name no
   adapter answers to is refused at load, since nothing could ever be charged to
   it.

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
   for an early reset, and a probe is a real harness session - cheap (a
   one-character prompt, with the harness locked down to no tools or read-only),
   but not free, and a week-long cap polled every 30 minutes is a few hundred of
   them. Their tokens and cost go into the same running total the breaker reads,
   so a pause whose probes alone cross a ceiling ends on that ceiling rather than
   polling on.

   **A pause holding a stated reset aims at it.** When the limit carries a reset
   time, the first probe lands just past that time instead of on the next tick of
   the `poll_interval` grid (still capped at `poll_interval`, so a STOP marker
   never goes stale), and the paused log line says so. A stated reset is a claim,
   not a guarantee: once the pause crosses it, it falls back to probing every
   `poll_interval` until a probe actually confirms the cap has lifted - it never
   resumes on the stated time alone after having slept through it.

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
| OpenCode (`opencode`) | opencode **2.x** only (1.x and the `opencode2` harness name are retired); provider-agnostic (model comes from opencode's own config) |

New harnesses are a small adapter (`internal/harness`). Contributions welcome.

**Permissions are set per harness, and the loop fails closed under all of them.**
`claude` gets its allowlist from the settings file (`--settings`), `codex` from
`config.toml`, and `opencode` from a `permission` block the adapter writes into
the `OPENCODE_CONFIG_CONTENT` layer it pins on every spawn. opencode 2.x ignores
the `OPENCODE_PERMISSION` variable 1.x read, and with no policy it runs every
tool, so the content layer is set whether or not an mcp config is named.

The opencode policy is a **path-scoped PermissionConfig**. 2.x makes read/edit
asks relative to the session's instance directory - the task's repo checkout the
driver spawns it in, not the project `workdir` - so `read` and
`edit` allow `**` and deny `../**`; `external_directory` - opencode's gate for a
path-taking file tool whose target is outside the instance directory - allows
that checkout absolutely and **denies every other path**. The same policy is
pinned on every built-in agent and the run passes `--agent build`, because on
2.x an agent-level permission block or a `default_agent` in any config layer -
including a project opencode.json the session could write into its own
checkout - otherwise overrides the top-level policy. `shell` is
allowed tool-level (its permission patterns match parsed commands, not paths).
Everything else - `lsp`, `subagent`, `skill`, `question`, the network tools
(`webfetch`, `websearch`) and `execute` - is denied by a `*` catch-all or by
name, and denied tools are hidden from the session's catalog entirely. `grep`,
`glob` and `list` are allowed (CLA-390): their asks carry the search pattern,
not a path, so their boundary is the `external_directory` gate. `*_*` allows the
MCP tools (`<server>_<tool>`: `clankerbar_get_backlog_summary`,
`context7_query-docs`), and a probe/read-only run additionally denies `edit` and
`shell` for zero writes. The policy is pinned by unit tests in
`opencode_test.go` and `instancedir_test.go` (including the JSON key order the
catch-all depends on and a replica of opencode's rule evaluator), and against
the real binary by the conformance suite.

**opencode 2.x does not path-gate the shell tool.** Only the shell tool's own
`workdir` argument hits `external_directory`; the paths inside a command do not,
so `touch /abs/outside` runs. The operator accepted this gap on 2026-10-18
(CLA-675), because an unattended session can already run arbitrary commands
inside the workdir; `doctor` states it as a known, accepted gap.

**MCP servers are taken out of Code Mode.** By default 2.x exposes an MCP
server's tools only inside its `execute` tool, where no claim can be observed.
The adapter sets `codemode: false` on every server in the content layer
(rewriting 1.x `mcp.<name>` entries to `mcp.servers.<name>`, the only dialect
where the key is honoured) and denies `execute`. 2.x registers the MCP
*resource* tools in Code Mode only, so a session cannot read MCP resources; the
policy's `mcp:*` read allow is kept for a build that exposes them again. 2.x
also connects MCP servers after the first model request, so the adapter prefixes
working prompts with one sentence telling the model to call a not-yet-available
MCP tool again. A result above 51,200 bytes is truncated by 2.x to a pointer at
a saved file; the adapter reads a truncated `claim_task` result back from the
session's own tool-output directory, so a large claim is still tracked.
Switching off a server the global opencode config declares takes a full entry
(`type` plus `command`/`url`) with the off switch; a partial
`{"enabled": false}` leaves it running, and `doctor` WARNs on one.

**Verifying the policy against a live session.** To watch opencode decide, read
the permission lines in `~/.local/share/opencode/log/opencode.log`, or run the
conformance suite (`docs/harness-conformance.md`), which drives the real binary
against the adapter's policy with a fake provider.

**Each session's instance directory is pinned to the directory it is spawned in**
(the task's repo checkout, or the project workdir when no repo resolves). `cmd.Dir` sets
the child's real cwd and nothing else; the inherited `PWD` still named wherever
the daemon was started, and opencode honours `$PWD`. So every session every
daemon spawned ran in the daemon's start directory whatever the project's
`workdir` said, which is the same defect as the wrong project's MCP server
(below), the wrong ask shape, and the wrong repo's `AGENTS.md`. Every adapter now
sets `PWD` to the directory it is about to run in and drops `OLDPWD`.

**And the project's MCP block is pinned past every layer opencode merges after
it.** opencode 2.x's config order is: its global config dir
(`$XDG_CONFIG_HOME/opencode`, replaced by `config_dir` via
`OPENCODE_CONFIG_DIR`), then `OPENCODE_CONFIG` (the file the driver names), then
the project-level `opencode.json` and `.opencode/opencode.json` discovered from
the instance directory, then `OPENCODE_CONFIG_CONTENT`. The project layer lands
*after* the driver's file, and on 1.x a committed repo-level `opencode.json` and
a global block were both observed redirecting the `clankerbar` server at
another project. The content layer is merged last, so a drain session reaches
the backlog it polls. A file that still names another project is reported by
`doctor`'s `opencode_ambient_config` check and logged once at startup - a
warning, not a refusal, since spawned sessions are pinned past it and the file
is often a checked-in artifact of somebody else's repo, but every *interactive*
session in that tree still gets the wrong backlog. The same scan reports a
second kind of finding that the content pin does *not* mitigate: a discovered
config carrying `permission`, `plugin`, an `agent` with tool or permission
authority, or an `mcp` server that starts a local process. A *discovered*
`mcp_config_path` is refused for those keys and a named one carrying
`permission` is a doctor WARN; a file opencode discovers by itself is merged
into every session started in that tree. `permission` is the sharpest of them:
the merge is per key and in insertion order, so a file declaring
`permission.read` moves that key ahead of the pinned policy's `*` catch-all and
the catch-all matches last, denying every read.

## Docs

- [`docs/releases.md`](./docs/releases.md) - the release/publish pipeline: how a change gets from a task PR to a published binary.
- [`docs/large-tool-results.md`](./docs/large-tool-results.md) - how the driver reads a tool_result that Claude Code persisted to a file instead of inlining.
- [`docs/opencode-tool-schema-limits.md`](./docs/opencode-tool-schema-limits.md) - the opencode-go request-time tool-schema rejection family (`unsupported_tool_schema` / `tool_count_limit`): the incident, the real root cause behind the misleading error code, and what the CLI does about a recurrence.
- [`docs/opencode-build.md`](./docs/opencode-build.md) - which opencode build the fleet runs (2.x), how to tell which one actually ran, how the adapter drives it (permissions, Code Mode, usage via the session export, resume, typed errors), the accepted shell path gap, and what an operator configures.
- [`docs/harness-conformance.md`](./docs/harness-conformance.md) - the hermetic conformance suite that execs the real opencode binary against a fake provider and MCP server.

## License

MIT (see [LICENSE](./LICENSE)).
