# Splitting review from fix

Status: implemented in the CLI (`[implement, review, fix]`, plus an optional
`verify` round for the fix delta — CLA-638); the plane half is
tracked as CLA-629 (per-phase harness/model, done) and the durable review record
(CLA-636 plane, CLA-637 CLI — both landed), so this change files no duplicate
task. CLA-210 (the parked structured-record task) was archived by CLA-636.
The `review` phase can now fan out to several independent reviewers (CLA-639,
parallel since CLA-697); see the README's
[review panel](../README.md#the-review-panel-several-reviewers-one-revision-one-merge)
section.

## Why

On 2026-10-17 the review phase ran on `claude` / tier `strong`
(`claude-opus-5-5`). Against the makesPDF backlog five review sessions in about
forty minutes produced two completed sessions at 3.63M tokens / $3.92 and 2.09M
tokens / $2.73, and three capped mid-run whose spend is **unrecorded** rather
than zero. Implement phases in the same window ran 1.4M–13.8M tokens at
$0.02–$0.17.

The token volume is normal for an agentic session. The model is the cost. A
review phase was doing review **and** fix **and** gates **and** PR **and**
`in_review`: 64–104 tool calls plus a subagent. That is a second implementation
session, run on the most expensive model available.

## Why this reverses an earlier decision, deliberately

CLA-321 built the two-session split (implement, then review-**and**-fix) and
measured the three-session shape at **25–80% more expensive** — every session in
that experiment ran on the same model, so the third session bought a fresh context
for a full implementation-price. That is still true. The new evidence is about
*which* model pays: on 2026-10-17 the review ran on `claude-opus-5-5` while the
implement ran on `opencode-go/deepseek-v4.1-flash`, and the review's typing (64–104
tool calls plus a subagent) cost $2.73–$3.92 per session. Moving that typing to the
cheap harness changes the multiplier the earlier experiment measured. This is a
cost-informed reversal, not a rediscovery; if the fix phase is put on the same
expensive model, the earlier finding applies unchanged and the third session is
not worth buying.

## The phase list

```
implement  →  review            →  fix
             (review-only)          (last: PR + in_review)
```

| phase | actor | job | ends by |
|---|---|---|---|
| `implement` | cheap harness | claim, build, self-verify, commit, push, record branch | STOP holding the claim |
| `review` (non-final) | the expensive reviewer | read the diff, run the adversarial gate (the record, not the repo's gates — see the brief's gate-scope rule), **record findings, change nothing** | `update_task(outcome)` then STOP holding the claim |
| `fix` | cheap harness | apply findings by ID, re-verify scoped to them, commit, push, open the PR, hand over | `update_task(status: in_review, outcome)` |

`review` selects the read-only brief **by position**: a review with a phase after
it is the gate; a *final* `review` keeps the historical review-and-fix brief. That
keeps an existing two-phase config working. The shipped shape for new configs is
the three-phase one.

`fix` is a separate name on purpose. `reviewTerminalStep` is injected by phase
name; a fix phase called `review` would inherit the wrong contract.

**Run `fix` on a different provider from the review.** A fix session that starts
immediately after an Opus review can hit the same rolling usage limit the review
just consumed the tail of. CLA-631 changed what that costs, not whether to avoid
it: the sequence ends and the run waits the reset out instead of the daemon
exiting, but the task still lapses into a takeover that pays `implement` again,
plus the wait. The README's per-phase harness example (implement and fix on
opencode, review on claude) is the shape to copy; an all-`claude` three-phase
config is the one most exposed.

## Where the review artifact lives

**Today (CLI only, no plane change): the task's `outcome`.**

- The review phase calls `update_task(taskId, runId, outcome: ...)` with no status
  change — the plane's documented way to write or correct an outcome without a
  fake transition.
- The record is a fixed, compact shape (the heading is a shared constant,
  `config.ReviewRecordHeading`, because the review writes it, the fix reads it,
  and the driver's seam gate refuses a checkpoint unless the outcome changed
  since the review spawned and carries the heading):

  ```markdown
  ## Review
  - reviewed tip: <sha>
  - implemented by: harness <h>, model <m>
  - reviewed by: harness <h>, model <m>
  - verdict: pass | findings
  ### Findings
  - R1 [must] (<kind>): <evidence> — <file:line> — <what is wrong>. Required fix: <smallest change>.
  - R2 [should] <file:line> — <what is wrong>. Required fix: <smallest change>.
  ```

  Findings are graded `must` / `should` / `nit`, and only a `must` gates. A
  `must` must cite evidence of one of seven kinds: a quoted `doneWhen` clause it
  violates; a repo gate that fails or would fail (command + output or reason); a
  concrete failure scenario in the change or its regression surface; a security
  exposure with the path; a contradicted standing decision (id) or repo hard
  rule (file + rule); an unguarded irreversible operation; or a new/changed
  guard, gate or state transition with no test pinning it (file:line + the
  missing test). Style, naming, preferred designs, unmeasured performance,
  documentation wording and missing tests outside that last kind are never
  `must`s. The driver parses the block back and lists the findings it will hold
  the fixer to; a recorded `must` with no evidence kind or payload is listed as
  a `should` (the structural downgrade), and the fixer may not re-grade — a
  `must` is fixed or refuted with evidence, and a single reviewer's `must` is
  never dropped.

  The heading carries no round number: a hard-coded "round 1" would be false on
  a later run of the same task (a request-changes re-work), and the driver has no
  round counter to fill in. Per-round numbering is the plane's to store
  (CLA-210).

- The fix phase reads the outcome's **last** `## Review` block with `get_task`
  (a re-trip can leave several; an earlier one is an earlier round), verifies
  `reviewed tip == git rev-parse HEAD`, applies findings by ID, and carries that
  block forward — with everything that sat above it — into its own terminal
  outcome with a `### Fixes` list. The final outcome on the task is therefore
  the whole round: who reviewed, what was found, what was done.
- No file in the worktree: a file is invisible to the plane, dies with the
  worktree, and cannot be read from another machine.

Attribution (`{{harness}}`, `{{model}}`, `{{prevHarness}}`, `{{prevModel}}`) is
injected by the driver, because a session cannot reliably name its own model and
cannot see its predecessor's.

**Limit of the CLI-only record:** `outcome` is one text field, and a statusless
`update_task(outcome: ...)` replaces it wholesale. A second round would overwrite
the first unless the second session copies it forward, so both shipped briefs say
to append: the review copies the existing outcome unchanged and adds its block
below it, and the fix keeps everything above the **last** review block while
adding `### Fixes`. The linear sequence has no second review, but
request_changes and takeovers ARE second rounds — they return the task with
the previous round's block still in the outcome — so the driver snapshots the
outcome when the review phase spawns and its seam refuses a checkpoint whose
outcome has not changed since. Each round records its own block. It is still
not the long-term shape.

### Two seam guards the split needs

The review-only phase inherits phase 1's recorded branch (`HasWIP`), so the
existing evidence gate — which was written for a phase whose deliverable IS the
branch — would pass it with nothing recorded at all. Two guards close that:

1. **The review→fix checkpoint requires a durable record from THIS run.** Since
   CLA-636/637 the review phase's deliverable is a sealed per-reviewer record
   written through `record_review` (or `update_task(review: ...)` on a plane
   older than protocol 40), and the seam gate is that record:
   the driver reads the task's `reviews` block at the seam and refuses a
   checkpoint unless it carries a record this run wrote, under this phase's
   reviewer role, whose `reviewedTip` equals the branch tip the driver verified
   on origin (a non-empty tip when the origin could not be read). A
   request_changes re-trip or a takeover runs under a NEW run id, so the
   previous trip's record — however complete — can never satisfy the gate, and
   a voided round (lease expiry or takeover) never matches either. The task's
   baseline read at spawn is retried a bounded number of times, and one that
   stays unread refuses the phase BEFORE the review spawns (CLA-663) — a full
   review session whose checkpoint could not be judged against the task it
   found is pure waste — with the same claim disposition a seam refusal leaves.
   Fail-closed: a session that burned its turns, hit its ceiling, or died on a
   silent EOT ends the drain, and because the resumed claim carries the branch
   (`HasWIP`) the driver does not release it to the queue — the lease lapses
   and a takeover picks the work up. (Pre-split, a dead review was the last
   phase, so it ended the drain with no PR — this guard preserves that property
   across the seam.)

   The driver then CLOSES the round before spawning `fix`, which is what
   releases the sealed findings to the fixer; a close that fails refuses the
   checkpoint. The legacy `## Review` outcome block is still written for one
   release — older planes and readers expect it — but it is not what the gate
   reads: the outcome snapshot exists only for the pre-spawn read's tri-state
   and the refusal's diagnostic.

   One variant keeps the pre-636 gate, by the operator's decision on question
   03a12578 (option 1): a **branchless** checkpoint — a task its own session
   settled, so no run holds it — cannot write the durable record at all (the
   plane accepts review writes only from the task's live holder), and the seam
   gates it on the legacy block instead: the outcome changed since the spawn,
   a `## Review` heading on its own line, and a LAST block the spawn-time
   baseline did not carry. Its brief prescribes the block alone, and there is
   no round to close; the fix brief that follows reads that block alone too —
   with no durable record there is no plane finding id to apply by and no
   `findingUpdates` call to make. Every branch-carrying variant gates on the
   record. A `verify` phase on a branchless task inherits the same shape: it
   adjudicates the block, appends its own, and the driver neither counts rounds
   nor reads plane ids for it — there are none — so a MUST the verifier leaves
   open is a park (its brief says so), the same disposition the round cap gives
   the id-based shape.

   One handoff residue, stated rather than hidden: a handoff respawn re-enters
   the phase under the SAME run and role, so a review session that writes its
   record and THEN hands off leaves a record the successor's seam already
   accepts — the phase's deliverable is written, and a successor that added
   nothing is a successor with nothing left to add. A successor on a phase that
   never recorded still fails the gate as any recording-less session does.
2. **The salvage is skipped for the review-only phase.** Its deliverable is the
   plane record, not the tree. Anything uncommitted in its worktree is generated
   output from running checks; committing and pushing it would (a) commit the
   unattributable change the review's own record forbids and (b) move the tip
   past the `reviewed tip` it just recorded, so the fixer would park a healthy
   task on a mismatch the driver created.

The third guard is in the briefs: a handoff continuation carries the record shape
and the record requirement, not only the terminal step, so a respawned successor
can still write a record the gate will accept.

The fix phase has **no** unverified variant, deliberately: its brief asserts no
driver verification at all. It reads the review's record, which names the
hand-off's real state, and its `reviewed tip == HEAD` comparison fails closed if
the branch is missing — a third brief would only restate what the record already
says.

### Plane half (filed and landed — do not duplicate)

The backlog tasks that own this, so this change does not re-file them:

- **CLA-629** (done, `lecstor/clankerbar`) — record which harness and model
  ran each phase, durably. Its wire is an optional per-iteration
  `sessions: [{phase, harness, model}]` on the fleet report (bounds decided:
  20 per iteration, 200 per report, 100-char strings, all-or-nothing refusal),
  landing as a `phase_session` task event that survives the 30-day iteration
  prune. **CLA-630** (`lecstor/clankerbar-cli`, ready, depends on CLA-629) sends
  it from the CLI. That is the attribution half of "which model reviewed this".
- **CLA-636** (done, `lecstor/clankerbar`) — the durable per-reviewer
  `task_review` store written through `record_review` / `update_task(review: ...)`, sealed until
  the round closes. **CLA-637** (this work, `lecstor/clankerbar-cli`) makes the
  review phase write it, gates the seam on it and closes the round before `fix`.
  CLA-210 was archived pointing at CLA-636 and the `agent_review` state.

If the operator wants the full multi-round history, unpark CLA-210 and give it
this shape:

| column | meaning |
|---|---|
| `id`, `project_id`, `task_id` | identity |
| `run_id` | the run that wrote it (soft link, like `delivery_attested_by_run`) |
| `round` | 1-based round per task |
| `revision_id` | the task revision reviewed |
| `branch`, `reviewed_tip` | what was reviewed |
| `implementer_harness`, `implementer_model` | from CLA-629's `phase_session` events |
| `reviewer_harness`, `reviewer_model` | the same, for the review phase |
| `verdict` | `pass` / `findings` |
| `findings` | markdown, verbatim |
| `fix_commit`, `fixed_by_run` | which fixes landed, attributed |
| `created_at` | receipt time |

Write path: **extend `update_task` with an optional `review` object**, not a new
MCP tool. The tool surface is token-budget-sensitive
(`server/mcp/tool-surface.test.ts`, `docs/token-budget.md`); `update_task` is
already the write path for everything that changes a task, and its receipt is
compact. Read path: a compact `reviews` block on `get_task` only when reviews
exist (newest first, bounded), with full text in `export_project` / the task page.

**Ordering that matters:** CLA-629 must deploy before CLA-630 ships (CLA-492's
precedent: a CLI ahead of the plane's parser had its fleet reports rejected). The
review split does not depend on either; it ships with the outcome block.

## Review → fix → review

**Implemented as an optional `verify` phase (CLA-638).** Configure
`[implement, review, fix, verify]` and the driver repeats the `fix` →
`verify` pair until a verification round closes with **no open MUST**, or the
round cap parks the task. With a verifier configured, `fix` no longer opens the
PR: a clean verification round carries the terminal step (push check, PR,
`in_review`), and a round with an open MUST ends holding so another fix can run.

The verifier is review-shaped (it writes a durable record like the review gate)
and is scoped to the **fix delta** — the reviewed tip the previous record names
through `HEAD`, plus the regression surface the fixes touch. Unlike the review
gate, the verifier releases its OWN round in its terminal call: the plane
refuses a sealed holder's outcome write, and the close must ride the same
`update_task` call as the `in_review` move, because after the status move the
run is no longer the task's live holder and cannot close. The driver accepts
either state — sealed (it closes, as it does for the review gate) or already
released — and never closes twice. Its brief lists the prior open MUST ids
(read from the plane's released
records), and it adjudicates each one **by its plane id**
(`<round>.<reviewer role>.<n>`): resolved (`fixed`), resolved by dispatch
(`filed`), refutation accepted (`refuted`), or left open with a reason. New
MUSTs are allowed only inside the delta and get new ids.

On a **branchless** task (the CLA-670 shape above) the verifier follows the
review's block-only path instead: no durable record, no plane finding ids, no
round to close — it adjudicates the block and appends its own, and the driver's
cap and breaker do not apply (there is nothing for them to count). The no-code
brief's terminal carries that shape's disposition: the `in_review` move when it
judges no MUST open, and a park for the operator when it leaves one open.

The bounds are driver-enforced, because a conditional phase alone is not a
bound:

- **No open MUST** is the gate. SHOULD and NIT are recorded and never gate;
  they ride to `in_review` in the outcome.
- **Round cap 3 per review cycle.** The plane counts non-void rounds; a human
  verdict starts a new cycle, a takeover or reclaim does not, and a void round
  does not count. At the cap an open MUST parks the task with a question
  carrying the records — never one more round. The driver also counts, itself,
  the rounds it closed, for the case where a plane serves no count at all (an
  older plane, a degraded read): the count is the opening review round plus
  every verification round this drain ran, so it never exceeds the plane's
  count, and a degraded read cannot buy the cycle a fourth round (CLA-686 D2).
- **Non-convergence breaker.** A MUST still open after two fix attempts, or one
  a verifier marked resolved that a later verifier reopens, parks the task with
  a question rather than spending another round on the disagreement.

An **unbounded** review↔fix cycle remains not recommended: the budget breaker
fires at phase boundaries, but a cycle whose phases each checkpoint can spend
the full budget one round at a time, and the failure ("the reviewer and fixer
disagree") is exactly the one that produces infinite plausible rounds. The
round cap and breaker above are the answer, not a bigger `max_iterations`.

This loop is **interim**: it lives in `drainPhases` with the rest of the phase
logic, and the state-machine reshape (task-state-machine build phase 4, B10)
moves the transitions onto the plane. The CLI README documents the shipped
shape; this file keeps the design rationale.

## The new-task alternative

*The review session files a new task on the back of the existing branch; a worker
fixes it.*

It has a real attraction: the queue already durably holds tasks, branches,
priorities and retries, and no driver-level repeat machinery is needed. But it
**does not remove the need for a bound**, and it costs task identity:

- **The branch has one owner.** The fix task would claim the same branch as a
  *takeover*, while the original task is still in `in_progress` or must be parked
  to release it. Two rows now describe one unit of work. The close-out flow asks
  "which task owns this branch/PR?" and the plane has no parent-completes-child
  rule for a normal (non-container) task.
- **The findings still need a plane write.** Agents cannot post comments. The new
  task's `detail` would carry them, which is one more full task body on a surface
  the token budget already watches.
- **The queue is not a bound.** A fresh task per round progresses every time, so
  the reclaim / no-progress ladders never engage. You would need a round counter
  on the branch (or a new plane rule) to stop review → fix → review → fix. That is
  the same bound, moved to the plane where it is harder to see.
- **It is the right shape for the HUMAN review loop.** `request_changes` already
  moves a task out of `in_review` with the verdict in context. That is the queue
  acting as the cycle, and it should stay that way. The alternative confuses the
  pre-PR agentic gate with the post-PR human gate.

**Answer:** the intuition is half right. The queue *is* a cycle, and it is
the correct one for a human request-changes round. It does **not** remove the need
for a driver-level bound on the agentic review↔fix loop, and for pre-PR findings
the linear fix phase is strictly simpler: one task, one branch, one PR, one
close-out.

## How much work is each option?

| option | CLI effort | plane effort | risk | when |
|---|---|---|---|---|
| linear `[implement, review, fix]` | **done**: 2 briefs + variants, positional selection, attribution/position placeholders, validation, tests | none for the first round; the review record is an `outcome` block | low — no new loop control | now |
| bounded second review | one more brief + a conditional-phase predicate in `drainPhases` | none | medium — conditional spawning is new control flow | after a live run measures the split |
| new task on the branch | no CLI change | task-linking + round counter + close-out semantics | high — two tasks per unit of work | not recommended pre-PR |
| durable review history | **done** (CLA-637): the record is a sealed per-reviewer row written via `record_review` (protocol 40+) or `update_task(review: ...)` on an older plane; the seam gates on it and the driver closes the round | CLA-636 (done): the append-only `task_review` store; CLA-629/630 carry the attribution | low — additive, bounded, token-budgeted | shipped alongside this doc's split |

## Answers, explicitly

**How much work is review → fix → review compared with the linear split?**
The linear split is one new brief and no new control flow. A bounded second
review is a further brief plus a conditional-phase predicate in `drainPhases`
(medium effort, and the only new control flow). An unbounded cycle is not a
control-flow change at all but a policy choice, and the expensive one. Build the
linear split, measure a live run, and add the second review only if the fix phase
is landing findings in a way the first review should re-check.

**Does "the review files a new task on the existing branch" remove the need for a
driver-level cycle?** No — not for the pre-PR agentic gate. It moves the bound to
the plane rather than removing it (a fresh task per round defeats the reclaim and
no-progress ladders), it forces two task rows per unit of work and a takeover on a
branch whose original task is still open, and it puts the findings on a new task's
`detail` because agents cannot comment. The queue *is* the right cycle for a
**human** `request_changes` verdict — that path already works and should stay —
but using it for an agent reviewer's findings makes a human the router between two
agents. For a bounded first round the linear fix phase is strictly simpler: one
task, one branch, one PR, one close-out.

## What is still unmeasured

The split's saving is a hypothesis until a like-for-like run: one task, same
model buckets, three phases, with the review session's spend separated from the
fix session's. The iteration log already separates sessions; the fleet report
does not yet separate models. Measure before adding a conditional second review.
