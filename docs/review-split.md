# Splitting review from fix

Status: implemented in the CLI (`[implement, review, fix]`); the plane half is
already tracked as CLA-629 (per-phase harness/model, in progress) and CLA-210
(structured review record, parked), so this change files no duplicate task.

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
| `review` (non-final) | the expensive reviewer | read the diff, run the adversarial gate, **record findings, change nothing** | `update_task(outcome)` then STOP holding the claim |
| `fix` | cheap harness | apply findings by ID, re-verify scoped to them, commit, push, open the PR, hand over | `update_task(status: in_review, outcome)` |

`review` selects the read-only brief **by position**: a review with a phase after
it is the gate; a *final* `review` keeps the historical review-and-fix brief. That
keeps an existing two-phase config working. The shipped shape for new configs is
the three-phase one.

`fix` is a separate name on purpose. `reviewTerminalStep` is injected by phase
name; a fix phase called `review` would inherit the wrong contract.

**Until CLA-631 lands, run `fix` on a different provider from the review.** A fix
session that starts immediately after an Opus review can hit the same rolling
usage limit the review just consumed the tail of, and the daemon exits on it
rather than waiting — the account-limit failure a resumed phase already has. The
README's per-phase harness example (implement and fix on opencode, review on
claude) is the shape to copy; an all-`claude` three-phase config is the one most
exposed.

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

1. **The review→fix checkpoint requires a record from THIS trip.** For a
   built-in review-only phase the driver snapshots the task's outcome when the
   phase spawns, then reads it again at the seam and refuses a checkpoint unless
   the outcome changed, carries `ReviewRecordHeading` as a line of its own, and
   its **last** `## Review` block — heading line to the end — is one the
   snapshot did not already carry: an edit that leaves an earlier round's block
   as the last one recorded nothing this trip. Presence alone is not enough: a
   request_changes re-trip or a takeover returns the task with the previous
   round's block still in the outcome, and a review session that then dies
   having recorded nothing would otherwise advance to the fixer over an
   unreviewed revision. The exact line keeps a look-alike heading
   (`## Reviewer notes`) from standing in for the block. The snapshot read
   itself is retried a bounded number of times, and one that stays unread
   refuses the phase BEFORE the review spawns (CLA-663) — a full review session
   whose checkpoint the seam would refuse anyway is pure waste — with the same
   claim disposition a seam refusal leaves. Fail-closed: a session that burned
   its turns, hit its ceiling, or died on a silent EOT ends the drain, and
   because the resumed claim carries the branch (`HasWIP`) the driver does not
   release it to the queue — the lease lapses and a takeover picks the work
   up. (Pre-split, a dead review was the last phase, so it ended the drain
   with no PR — this guard preserves that property across the seam.)

   One residue of the snapshot, stated rather than hidden: a handoff respawn
   re-enters the phase and takes a FRESH baseline, so a review session that
   records its block and THEN hands off leaves its successor a baseline that
   already carries the record — the successor must record its own block (its
   handoff continuation carries the record shape, so the passing outcome holds
   two blocks; a successor that records nothing is refused and the round
   re-runs). The fresh baseline is the point: it is what keeps a handoff from
   laundering a previous trip's block through the seam.
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

### Plane half (already filed — do not duplicate)

Two existing backlog tasks own this, and this change deliberately does not
re-file them:

- **CLA-629** (in progress, `lecstor/clankerbar`) — record which harness and model
  ran each phase, durably. Its wire is an optional per-iteration
  `sessions: [{phase, harness, model}]` on the fleet report (bounds decided:
  20 per iteration, 200 per report, 100-char strings, all-or-nothing refusal),
  landing as a `phase_session` task event that survives the 30-day iteration
  prune. **CLA-630** (`lecstor/clankerbar-cli`, ready, depends on CLA-629) sends
  it from the CLI. That is the attribution half of "which model reviewed this".
- **CLA-210** (parked) — "The section 5 adversarial review has no structured
  record". That is the findings/fixes half. This change's outcome block is the
  interim record; CLA-210 is where a durable `task_review` shape belongs.

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

**Design, not built. Keep it bounded.**

The linear split is one review pass. A second pass is worth paying for only when
the first pass found something and the fix was non-trivial. Two viable bounds:

1. **A conditional second phase (recommended if it is built).** Add an optional
   `review` after `fix` with a distinct name (`verify`) and a rule: it runs only
   when the fix outcome reports `verdict: findings`. The driver gains a
   conditional-phase predicate, a second hand-off, and a second expensive
   session. Effort: medium. Risk: low, because the loop is still finite and the
   phase list is static.
2. **A round cap in the sequence.** `[implement, review, fix, verify]` where
   `verify` is read-only and hands over without fixing. Effort: small; it is one
   more brief and no new driver logic. Risk: the second review is unconditional,
   so a clean task pays for it. Bound it with `max_turns` and a tier; do not make
   it the default.

An **unbounded** review↔fix cycle is not recommended. The budget breaker fires at
phase boundaries, but a cycle whose phases each checkpoint can spend the full
budget one round at a time, and the failure ("the reviewer and fixer disagree")
is exactly the one that produces infinite plausible rounds.

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
| durable review history | none in this change | CLA-210 (parked) + CLA-629/630 for attribution | low/medium — additive, token-budgeted | when the operator unparks it |

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
