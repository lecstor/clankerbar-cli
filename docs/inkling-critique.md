1. ATTACK ON TIER 0 PILOT

Plan ref: docs/dual-review-plan.md §3 (Tier 0) and §5 (safety/rollback).

Flaws the spec admits but underweights:
- Instruction-following variance. `opencode run` takes `--model` but there's no enforcement that the subprocess only receives the diff + template — a session that pastes its full context, the branch name (`clanker/<id>`), or its own findings leaks identity and collapses the "blind" claim (§3: "naturally read-only, blind to parent's conclusions" — only true if the parent behaves, which is not enforced). Fix: template must be a heredoc file, not inline prose, with an explicit "DO NOT include anything beyond this file" guard.
- Diff-size handling. No cap specified. A large diff exceeds the 262k ctx of `openrouter/thinkingmachines/inkling:free` silently (truncation) or hits the gateway's request-size floor. The spec says nothing about splitting, summarizing, or refusing. Needed: a line cap (e.g. 8k lines of diff) with graceful degradation ("diff truncated at line N; review first 8k only").
- Outcome size caps (§4, §5). "Counts + one-liners" is the mitigation, but the spec names no exact keys. If `outcome` fields are fixed-size (README implies no custom schema on outcome), adding `inkling_findings_count`, `inkling_findings_summary`, `alpha_findings_count`, `alpha_findings_summary`, `overlap`, `unique_inkling`, `unique_alpha`, `review_order_parity`, `inkling_call_status` risks truncation of the very overlap computation. The ledger file (`docs/review-ledger.md`) is the safe sink — the spec should mandate: outcome gets COUNTS ONLY; full verbatim lists go straight to the committed ledger, never through outcome.
- Blindness is one-way only (§5: "inkling pass ... blind to the parent's conclusions"). Nothing stops the parent (ox-alpha session) from reading the inkling output before fixing — the subprocess returns to stdout which the parent reads. The "independent" claim requires the parent to fix BEFORE reading the inkling result, and the spec does not sequence that. The plan says "after your own inline adversarial pass and BEFORE fixing anything, obtain a second review" — but then the parent still holds both lists when it fixes. That's fine structurally; the flaw is that nothing records which findings the parent accepted from which source. Without attribution tags (`[alpha]`, `[inkling]`, `[both]`) on each applied fix, the post-fix ledger cannot reconstruct overlap.
- Quota burn (§4, §5). "Watch D's daily cap experience" is not a mechanism. No throttle, no retry-backoff, no rate-limit parser. The `opencode-tool-schema-rejection` doc (§"What the CLI does") shows how a 400 with `isRetryable:false` should be handled: classify as transient (even if arguably not), log, continue. Tier 0 needs the same: a 60s retry once, then log `inkling_rate_limited` and proceed single-reviewed. Not optional.
- The "no findings" option is required (§2 task list) but the spec does not include it in the outcome schema. If inkling returns nothing, `count=0` is ambiguous (did it fail? was the diff empty? did it miss?). The outcome must distinguish `count=0, status=ok` from `count=0, status=rate_limited`.
- Position bias cancellation (§3: "order alternates by task-id parity") assumes task ids have an even/odd property usable across sessions. The spec does not define the parity key (`task.id % 2`? the claim's numeric suffix?). If two separate harnesses process the same task on different machines, parity must be deterministic from a shared seed (the task id itself) — which it is, but needs to be named explicitly.
- Default is "blinded" (§5 open questions). The template must not reveal the implementing model; but `opencode run --model ...` leaks the reviewer's identity (inkling) and the diff often names branches (`clanker/x`) that include the task id. That's acceptable — the blind is author-identity, not reviewer-identity. But the spec should say so.

Quiet assumptions not listed:
- That `opencode run` exits non-zero on failure. It may not; the adapter's `opencodeTransientRe` behavior must be reused.
- That the reviewer's output is parseable prose. Without fixed headings, overlap computation requires NLP heuristics. The spec must enforce headings.
- That the pilot scope (`ezyapp` only) is enforceable by a session reading `ezyapp/CLAUDE.md`. Nothing stops a session from applying the procedure to other repos unless the section is scoped explicitly (e.g. "When working in this directory only").

2. ADVERSARIAL-REVIEW TEMPLATE (subprocess prompt, <250 words)

File reference: docs/dual-review-plan.md appendix prompt (not yet present; this is it). Intended invocation: `opencode run --model openrouter/thinkingmachines/inkling:free --prompt-file /tmp/inkling-adversarial.md --input-file /tmp/pre-fix.diff` with NO additional tools beyond read-only file access.

---

You are an independent adversarial reviewer (inkling). Receive ONE file: pre-fix git diff. You DO NOT know the author, harness, or any other reviewer's output. Read nothing else. Severity: BLOCKER (breaks prod/data), CRITICAL (likely-to-ship defect), WARNING (fix pre-merge, not catastrophic), NOTE (nit/style). Zero findings? Write exactly "NO FINDINGS" in FINDINGS and justify briefly.

Output format — copy the headings exactly, preserve order, no extra headings:

--- REVIEW START ---
DIFF SUMMARY: <one line: file count, line delta, language/domain>
REVIEWER: inkling / openrouter/thinkingmachines/inkling:free
BLIND: author identity hidden; no other reviewer input seen

FINDINGS:
[BLOCKER] <filename>:<line-range> — <one sentence description> — <why it matters>
[CRITICAL] ...
[WARNING] ...
[NOTE] ...
(If zero findings: [NONE] — NO FINDINGS — <one sentence justification>)

SEVERITY SUMMARY:
BLOCKER: N  CRITICAL: N  WARNING: N  NOTE: N  TOTAL: N

OVERLAP NOTE (optional, one line): if any finding echoes a known anti-pattern (e.g. missing input validation, race condition), name it briefly — do NOT guess whether a previous reviewer mentioned it.

CONFIDENCE: <HIGH / MEDIUM / LOW> — brief justification (<20 words).

--- REVIEW END ---

Constraints: total response <600 words; no markdown outside headings; do not propose fixes (only identify); do not ask clarifying questions; do not mention model names other than in the REVIEWER line.

3. BETTER ALTERNATIVES (no driver changes required for any)

A. Double-subprocess (pure Tier 0 extension). Instead of one inkling pass, launch TWO independent `opencode run` calls with DIFFERENT fixed prompts (e.g. "security-focused adversarial" vs "logic/behavior-focused adversarial") against the SAME diff file. Both use `openrouter/thinkingmachines/inkling:free`; one can use a second free route (e.g. `groq` or `together` free) if available. Cost: 2x quota; gain: measures intra-model variance (how consistent is inkling with itself) which is a baseline before comparing to ox-alpha. No driver change; only the guidance section of `ezyapp/CLAUDE.md` grows.

B. Sequential-blind with artifact isolation. The parent writes the diff to `workdir/.review/diff.pre` before ANY review (ox-alpha inline included). After its own inline review and BEFORE fixing, it runs the inkling subprocess reading ONLY `.review/diff.pre`, writes output to `.review/inkling.out`. The fix phase then reads both `.review/ox-alpha.self-notes` (written before subprocess) and `.review/inkling.out`, applies changes, and records per-finding attribution (`alpha` / `inkling` / `both`). This makes overlap reconstruction exact rather than heuristic. Still no driver change; it's just file discipline in the session's worktree.

C. Ledger-only, no outcome fields. Drop all review metrics from the session's outcome text; commit a `docs/review-ledger.md` entry per task with fixed headings (`task`, `date`, `reviewer_order`, `alpha_findings`, `inkling_findings`, `overlap`, `unique_alpha`, `unique_inkling`, `confirmed_real`, `false_positives_alpha`, `false_positives_inkling`, `inkling_status`). The outcome only records `ledger_commit_sha=` and `ledger_line=` pointers. This sidesteps the outcome-size cap entirely (§4) and makes the dataset durable independent of session memory.

D. Use the EXISTING phase mechanism differently (§2, architecture findings). The spec claims "same-artifact dual review via phases" is impossible because review phases fix what they find. That is the built-in `review` brief (`builtinPhasePrompts`), not a structural limit. A custom phase with `name: "review2"` and `prompt:` set to an adversarial-only prompt (no fix instruction) runs as a fresh session with no fix authorization — the session reads the same worktree, sees the same pre-fix diff (if the previous phase never committed fixes), and produces only findings. The spec says `HandoffContinuation` keys on the literal name `"review"` (§2) — using `"review2"` avoids that trap. The phase mechanism ALREADY supports per-phase `tier` routing (§2: `EffectivePhases`, `Phase.Tier` → `HarnessConfig.Models`). So a two-phase config `[{name:"review", tier:"strong"}, {name:"review2", tier:"strong", prompt:"adversarial-only"}]` achieves the dual review WITHIN the driver, no subprocess needed. The spec's claim that phases cannot do it depends on using the built-in `review` name; renaming solves it. This is a stronger path than Tier 0 but counts as a "no driver code change" alternative (only a config + brief change).

Note on D: it contradicts the spec's "what config does NOT give" line only if you assume `review2` inherits fix semantics. It does not — custom prompts override builtins. I include D specifically because the spec is slightly wrong here, and a senior engineer should say so.

4. METRICS (up to 5, separating quality from verbosity and luck)

Ref: docs/dual-review-plan.md §4 (ledger fields). Ground truth arrives after fixes land (`confirmed-real` in the ledger); until then, no absolute accuracy metric exists. So metrics must be relative and structural.

M1 — OVERLAP RATIO: `|alpha_findings ∩ inkling_findings| / min(|A|, |I|)`. High overlap means reviewers agree; very low overlap with equal counts suggests either one misses a class or one over-generates false positives. Separates from verbosity (counts cancel in ratio) and from single-reviewer luck (requires both lists).

M2 — UNIQUE-REAL RATE (post-fix, deferred metric): after fix lands, `confirmed_real_unique_to_alpha / |unique_alpha|` and same for inkling. This is the only absolute-accuracy metric; it must be tracked but cannot be computed same-day. Separates quality from verbosity (a verbose reviewer with low unique-real rate is worse than a terse one with high rate).

M3 — FALSE-POSITIVE RATE (post-fix, same delay): `(1 - unique_real / total_findings)` per model. Combined with M2, it distinguishes a reviewer who finds a lot (many unique real) from one who finds nothing useful (many false positives, few unique real). Critical for deciding whether inkling earns a slot.

M4 — SEVERITY-WEIGHTED OVERLAP: overlap computed at each severity tier separately (`BLOCKER`, `CRITICAL`, `WARNING`, `NOTE`). A reviewer that catches all BLOCKERs but misses NOTES is higher-quality than one that catches many NOTES and misses a BLOCKER. This prevents verbosity gaming (NOTE inflation) from masking structural misses.

M5 — REVIEW-INDEPENDENCE INDEX: `1 - (correlation between review order and which model found the unique finding)`. With counterbalanced parity (§3: "dispatch order alternating by task-id parity"), over N≥10 tasks, a significant correlation means position bias dominates reviewer identity — the mechanism is broken regardless of which model "wins". This is a mechanism-health metric, not a reviewer-quality metric, and it is the only one that validates the experiment design itself.

Excluded (would mislead): raw finding count (verbosity), time-to-output (confounded by quota/rate-limit), word count (verbosity proxy), agreement on NO FINDINGS (low-information; useful only as negative signal).

5. VERDICT

Would I trust this mechanism's data enough to decide whether inkling earns a permanent reviewer slot? Not alone — and the spec almost admits it. The mechanism produces a comparison dataset, not a ground-truth dataset. Ground truth (`confirmed-real`) arrives only after fixes land (§4), which requires the implementing phase to complete, push, and have the fix verified independently — that is a lag of at least one full task cycle, often more (if the fix is multi-phase or disputed). The decision about inkling's slot therefore MUST be deferred until both M2 (unique-real rate) and M3 (false-positive rate) can be computed over a sufficient sample.

Sample size: the spec proposes `>= 10 tasks with both reviews completing` (§4, success criteria). That is the MINIMUM for M5 (independence index) to have statistical meaning, but it is INSUFFICIENT for M2/M3: with 10 tasks, a single false-positive swing changes rates by 10%. I would want N ≥ 30 completed tasks with both reviews finishing cleanly (`inkling_status=ok`, not rate-limited or failed), with parity counterbalanced (N/2 each order), and with ground truth recorded for ≥ 80% of findings. At N=30, a 20% absolute difference in false-positive rate (e.g. 15% vs 35%) is distinguishable; below N=30, noise dominates.

What would change my mind (against the mechanism):
- M5 shows significant order-correlation (p < 0.1 on Fisher's exact) — the counterbalancing failed, so all other metrics are confounded.
- M2 unique-real rate for inkling is not measurably different from zero (no findings it caught that alpha missed) across N=30 — the second reviewer adds cost without value.
- M3 false-positive rate for inkling exceeds alpha's by >2x with M2 flat — it is noisy, not useful.
- More than 30% of inkling calls fail (`status` ≠ `ok`) — the free-route quota is too unstable to rely on (§4: "watch D's daily cap experience" is not a mechanism; a 30% failure rate means the mechanism produces biased samples — the tasks that complete are the ones that didn't hit the cap, which may correlate with diff size or time-of-day).

What would change my mind (for the mechanism):
- M2 shows inkling catching ≥ 1 BLOCKER- or CRITICAL-class finding that alpha missed, on ≥ 3 separate tasks (not a one-off).
- M3 false-positive rate is within 1.5x of alpha's (not wildly worse, per §4 success criteria).
- M5 shows no significant order-correlation.
- Failure rate is < 15% over the full sample.

Additional limitation: I am one of the two models being compared (§1). My assessment of inkling's potential bias favors concrete structural flaws over flattering generalities — that's the job here — but the mechanism's data should be audited by a THIRD reviewer (a human or a different model family) before it becomes the basis of a permanent slot decision. The mechanism is good at comparing two automated reviewers; it is not self-validating.

Also: alternative D (two-phase `review2` with custom adversarial prompt) should be tried in parallel with the pilot, not after. It uses the existing driver (no code change beyond config/prompt) and produces the exact same dataset format, but eliminates the subprocess failure mode entirely. If the pilot succeeds for the subprocess approach but the phase-based approach also works, the phase approach is the more durable Tier 1. The spec treats them as sequential tiers; they should be parallel pilots.
