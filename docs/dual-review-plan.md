# Plan: Dual-model review gate (ox-alpha + inkling), counterbalanced

Status: proposal / exploration - not approved for implementation
Revision: 2, 2026-08-22 - incorporates the inkling critique
(docs/inkling-critique.md). Accepted: template-as-file, diff cap,
counts-only-in-outcome with committed ledger sink, per-finding attribution
tags, explicit sequencing, status field distinguishing rate-limit/failure
from empty, named parity key, scoped-applicability wording, metrics M1-M5,
N>=30 sample bar, third-party audit caveat, parallel-pilot recommendation.
Qualified: its alternative D (see section 3D) is mechanically right that
custom prompts override builtins, but understates that making reviewer #2
meaningful forces reviewer #1 to NOT fix - which departs from the served
review-and-fix contract (Tier 2 territory), and drops handoff terminal-step
survival keyed to the literal "review" name.
Date: 2026-08-22
Origin: operator interest in comparing ox-alpha and Thinking Machines inkling
as reviewers on equal footing ("who finds what, who misses what"), using free
routes, with dispatch order counterbalanced across tasks.

## 1. Background

The fleet runs four clankerbar daemons on opencode, all currently pinned to
free ox-alpha routes (Zen gateway, OpenCode Go subscription, OpenRouter).
A second model family, thinkingmachines/inkling, is available free on
OpenRouter (`openrouter/thinkingmachines/inkling:free`, 262k ctx) and has
passed an agentic smoke test and a max-effort review calibration.

Goal: obtain TWO independent adversarial reviews of the SAME pre-fix diff on
every (pilot: ezyapp) task - one ox-alpha, one inkling - with dispatch order
alternating by task so position bias cancels out, and record both findings
lists verbatim to build a comparison dataset.

## 2. Architecture findings (all verified at v0.10.1 this session)

What config alone already gives:
- Per-phase model routing via tier buckets: `Phase.Tier` resolves through
  `HarnessConfig.Models` (`ModelForPhase`). In live use on hammer4.
- Arbitrary phase sequences with per-phase custom prompts
  (`EffectivePhases`: explicit `prompt` wins over builtin briefs).
- `harnesses` is an open map keyed by name; per-harness model/config_dir/mcp.
- Global reasoning-effort pinning via provider model options in
  ~/.config/opencode/opencode.jsonc (`reasoning_effort: "max"`).

What config does NOT give:
- Same-artifact dual review. Review phases are review-AND-FIX by design
  (builtinPhasePrompts comment: a third fix-phase was explicitly rejected);
  a second sequential reviewer sees post-fix state, which destroys the
  who-missed-what comparison.
- Per-task dynamic ordering. Config is static; parity ordering needs either
  session-side logic or driver code.
- Custom phase names silently lose review-specific semantics
  (`HandoffContinuation` keys on the literal name "review").

Session-behavior facts observed fleet-wide on 2026-08-22:
- Review gates currently run INLINE: zero subagent dispatches across every
  recent iteration log sampled. The repo guidance ("prefer inline over
  fan-out") is being followed, so there is no existing subagent to duplicate.
- Therefore the cheapest second opinion is a one-shot subprocess:
  `opencode run --model openrouter/thinkingmachines/inkling:free` fed the
  same pre-fix diff with a fixed adversarial template. No agent plumbing,
  naturally read-only, blind to the parent's conclusions.
- Outcome text is size-capped; verbatim dual findings lists may not fit -
  findings should be summarized + counted, with full lists in the deck/detail
  field or a committed ledger file.

## 3. Designs

### Tier 0 - guidance-level pilot (recommended first step)
Effort: ~30-60 min active. Reversible: one paragraph removed.
Scope: ezyapp only (pilot).
Mechanism: extend the "Reviewer dispatch discipline" section of
ezyapp/CLAUDE.md (read by both harnesses' sessions):
- After your own inline adversarial pass and BEFORE fixing anything, obtain a
  second independent review: write the pre-fix branch diff to a temp file and
  run `opencode run --model openrouter/thinkingmachines/inkling:free`
  with a fixed adversarial-review template (see appendix prompt).
- Order alternates by task-id parity for any step where order could matter;
  the two reviews are otherwise independent and blind to each other.
- Record in the outcome: counts per model, unique findings per model, overlap,
  and one-line summaries; full findings lists go in the review deck.
- If the inkling call fails or rate-limits: log that fact, proceed single-
  reviewed. Never block a task on the experimental reviewer.
Data: comparison ledger built from outcomes (fields in section 4).

Known limits of Tier 0: instruction-following variance across models; quota
caps on :free routes unknown until daemon D's first full day; findings parsing
from outcome prose is fuzzy (mitigate with fixed headings).

### Tier 1 - driver feature (only after pilot data justifies it)
Effort: ~300-500 lines incl. tests; one CLA-sized PR to clankerbar-cli.
Sketch: config key e.g. `review.gate: [{tier: "review"}, {tier: "review2"}]`
or `phases[].reviewers: [..]`; loop dispatches both against the pre-fix diff,
aggregates findings into one fix list, records attribution; parity ordering
keyed off task id. Touches config.go, loop phase handling, both harness
adapters' invocation shapes, plus conformance tests.

### Tier 1b - phase-based dual review WITHOUT driver code (from the critique)
Its alternative D, with qualifications. Mechanically: a custom-named second
phase (`review2`) carrying its own adversarial-only `prompt` runs a fresh
session over the worktree; custom prompts override builtins, so no fix
authorization is implied, and per-phase tier routing picks the model. To make
the comparison same-artifact, the FIRST review's brief must also change to
find-don't-fix (else reviewer #2 sees post-fix state) - which departs from
the served review-and-fix contract and drops the handoff terminal-step
continuation keyed to the literal name "review" (CLA-353). So: viable as a
parallel EXPERIMENT ARM on one daemon, not a drop-in replacement for the
gate; if both arms produce comparable ledgers, this arm is the more durable
foundation for Tier 1/2 because it eliminates the subprocess failure mode.
Recommended: run Tier 0 (subprocess) and Tier 1b (phase) as PARALLEL pilots
on different daemons, same ledger format, and let the data pick the survivor.

### Tier 2 - protocol coordination (the part that makes it real)
The review-gate contract partly lives in the SERVED skills (finishing.md,
review-brief.md, reviewer-agent.md are canonical at clankerbar.com). A
first-class dual gate means: canonical doc updates, multi-repo guidance sync,
migration notes for other machines/clankers. Multi-session effort. Do not
start Tier 2 without pilot data proving value.

## 4. Experiment design

Ledger fields per reviewed task - sink is a COMMITTED ledger file
(docs/review-ledger.md), never the outcome text, which carries counts and a
pointer only (`ledger_commit_sha`, `ledger_line`). Fields:
- task ref/id, date, implementing model+route
- reviewer order (parity key: last digit of the task ref number - even =
  alpha's inline pass first, odd = inkling subprocess dispatched before the
  inline pass is written up; deterministic across machines)
- ox-alpha findings: count + one-liners
- inkling findings: count + one-liners
- overlap set; unique-to-alpha; unique-to-inkling
- confirmed-real after fixing (ground truth); false positives per model
- second-opinion call status: ok / rate_limited / failed / empty_diff
  (count=0 with status=ok is a real "no findings", distinct from failure)
- per-finding attribution tags on every applied fix: [alpha] [inkling] [both]

Sequencing rule (closes the one-way-blindness gap): the reviewing session
writes its OWN findings to .review/alpha-notes BEFORE launching the inkling
subprocess; the subprocess reads only the pre-fix diff file; fixes are applied
only after both lists exist, each tagged by source.

Invocation discipline: the adversarial template ships as a FILE (heredoc),
never inline prose; diff capped at 8k lines with an explicit truncation note
in the review output; retry once after 60s on rate-limit, then log status and
proceed single-reviewed. Scope wording: "when working in this directory
(ezyapp) only".

Success criteria (revised per critique): N >= 30 tasks with both reviews
completing cleanly (status=ok), parity counterbalanced N/2 per order, ground
truth recorded for >= 80% of findings. Below that, noise dominates.

Metrics M1-M5 (adopted from the critique):
M1 overlap ratio |A n I| / min(|A|,|I|); M2 unique-real rate post-fix;
M3 false-positive rate per model; M4 severity-weighted overlap (BLOCKER..
NOTE separately); M5 review-independence index (order-vs-outcome correlation
- a mechanism-health gate: significant correlation invalidates the dataset).
Excluded as misleading: raw counts, latency, word count.

Decision rules (from the critique, adopted): inkling earns a slot iff M2
shows it catching >= 1 BLOCKER/CRITICAL-class finding alpha missed on >= 3
tasks, M3 within 1.5x of alpha's, M5 clean, call-failure rate < 15% over
N >= 30. Any of: M5 broken, M2 flat-at-zero, M3 > 2x alpha's, or > 30%
call failures -> drop or redesign. Final slot decision audited by a third
party (human or third model family), not self-certified.

## 5. Safety / rollback

- Tier 0 touches only ezyapp/CLAUDE.md (one section) - revert by deletion.
- Inkling subprocess is read-only by construction (no tools passed beyond the
  default one-shot surface; it receives diff text, returns text).
- Free-route quota burn doubles on reviews during pilot; watch D's daily cap
  experience before extending scope.
- The served protocol is untouched in Tier 0.

## 7. North star: the unified review/address loop (operator direction, 2026-08-22)

The config axis that defines everything: does a review step FIX, or only
REPORT? (`fix: true|false` per review step.) The primitive:

    round:
      1. REVIEW: one or more reviewer agents (model per tier, order per
         parity, fix: false) over the SAME state -> findings lists,
         independent and blind to each other by construction
      2. COMBINE: union with per-finding attribution ([alpha]/[inkling]/[both]);
         reviewers both flagged == high confidence; unique == verify
      3. ADDRESS: one agent consumes the combined findings, fixes, verifies,
         commits (fixer model chosen by cost/strength, independent of reviewers)
      exit: a review round reports clean at severity >= threshold,
            or rounds cap / spend cap
    then: auto-merge to staging vs hold for human (existing whose-turn dials)
    staging: reviewed by human + agents (same primitive over the integration diff)

Production mode: reviewers that fix are simply NOT separate - the address step
is the fix. Evaluation mode: exit after step 1 with fix: false. One mechanism;
"review and fix" vs "review only" is a config value, not a mode.

Why this beats single-agent assess-and-address (previous revision): with
assess+address in one agent, round N+1's reviewer never sees round N's input
state. Splitting roles keeps every reviewer on the same artifact by
construction, which is exactly the property the dual-review comparison needs -
evaluation falls out of production for free.

Answer to the builtin design's objection (config.go: the fixer "would hold
neither the implementation nor the review context"): the findings ledger is
the context - file/line-anchored, severity-rated, attribution-tagged. The
fixer needs that, not the reviewer's stream of consciousness. This is the
.review/ artifact discipline from section 3B, promoted to load-bearing.

Already exists (v0.10.1): phase-list iteration, per-phase tier->model routing,
custom per-phase prompts (a review-only brief is just prompt text forbidding
fixes), per-phase turn/wall-clock/spend caps, whose-turn dials, delivery
verification on staging PRs.

Gaps (the only real engineering):
G1. Rounds construct with structured findings handoff: repeat
    [review(s) -> combine -> address] down a tier list; early-exit when a
    review reports clean at the configured severity; hard rounds cap
    (default ~4). Reviewer list per round (count, tiers, order policy).
    Est. 300-500 lines + tests.
G2. The briefs: reviewer brief (report-only, fixed output headings, severity
    taxonomy, NO FINDINGS path) and address brief (consume ledger, fix,
    verify, attribute). Prompt text only.

Non-goals / guards: convergence is defined by severity threshold + caps, never
by reviewer agreement alone (stricter-after-laxer flaps forever); context cost
per round is bounded by ledger inheritance; the served implement/review
contract is untouched until this graduates to spec (Tier 2 coordination).

STRATEGY AXIS (operator addition): the round shape itself is configurable,
enabling strategy comparison with the same ground-truth machinery:
- Cycle:  [review(fix: true)] repeated - each reviewer addresses; the next
          reviews the fixed state. Early error-correction; smaller diffs per
          pass; but regression risk surfaces late and fix attribution blurs.
- Batch:  [review(fix: false) x N] -> combine -> address. Maximal independent
          coverage of one artifact; confidence-weighted findings (both-flagged
          ranks first); but fixes go unverified until the next round's review,
          and conflicting findings put arbitration on the fixer.
- Hybrid: batch rounds inside a cycling outer loop (the section default).
Ledger gains a strategy column; counterbalance strategies across comparable
tasks (not within one task - a task yields one strategy's dataset). Working
hypothesis the pilot can test cheaply: batch wins finding-generation
(coverage), cycle wins convergence reliability, hybrid dominates both.

Interim approximation NOW, zero driver changes: fixed-K rounds as explicit
phases - [review(tier A, fix:false brief) -> review(tier B, fix:false brief)
-> address(combined-ledger brief)] x K - rotating tiers, paying all K rounds,
fixed order. Enough to pilot the briefs and the ledger format this week.
