# The guillotine / dead-phase silent-death signature (CLA-386 / CLA-402)

Also called the "guillotine effect": a headless `opencode` implement session dies
silently — exit 0, no error event, zero reported usage/cost, no branch recorded —
so the external driver reads it as a completed session rather than a death.
The work is dropped with no visible failure.

This is the same failure class described upstream in the open code repo:
`anomalyco/opencode#43622` (opened 2026-08-20, assigned jlongster) and the
proposed fix `#43881`. It is **not** the `tool_count_limit` / schema-rejection
incident (`#43374` / `#43378`, closed 2026-08-20); that is a different invisible
failure documented separately at [`opencode-tool-schema-limits.md`](./opencode-tool-schema-limits.md).

## The signature (what the driver calls a dead phase)

Per the classification in `internal/loop/deadtally.go` (`CLA-402`) and the live
tally in `README.md` (§"The dead-phase rate"):

- Final step `finish` reason is `"unknown"` (`opencode`'s marker for a session
  that died without a final answer; `internal/harness/opencode.go` line 626+).
- Exit code 0, **no** `APIError` or other error event on the stream.
- Reported usage is all-zero; cost is zero (`step_finish` with zero tokens / zero cost).
- No branch (`res.Claim.Branch`) is recorded — the session produced nothing durable.
- Not explained by a cap (`!capped`), budget ceiling (`!ceiling`), or wall-clock
  cap (`!wallclock`), so it is a genuine silent death, not a controlled stop.

The same shape is described upstream at `#43622`: `packages/opencode/src/session/prompt.ts:1113`
treats an assistant `finish` of `"unknown"` as a completed turn (`!hasToolCalls` +
`parentID` check excludes only `"tool-calls"`), logs `"exiting loop"`, disposes the
instance, and exits 0 with no error — while `:1295` in the same file computes
`finished = !["tool-calls", "unknown"].includes(...)`, correctly excluding
`"unknown"`. A guard dropped during a refactor. The upstream PR (`#43881`, open, unmerged)
does **not** add `"unknown"` back to `:1113`; instead it fails empty `unknown`-finish
streams after a clean drain as a retryable `ProviderError.ResponseStreamError`
(`APIError`) — the same `:1301`-style error-surfacing shape, with the same
comment: "previously the session went idle silently" (same bug class, already
fixed once). That PR is unmerged; the change that actually shipped is
**v1.18.20**, which retries midstream network errors previously mapped to
`finish: unknown` (`opencode-agent[bot]` comment on `#43622`, 2026-08-21). So the
operator action is to update to v1.18.20+ (released), and separately track the
still-open `#43881` for the unbounded-retry case.

## Provider-level correlation (observed, partly unverified)

The guillotine / dead-phase pattern observed so far was exclusively on the
**opencode Go provider** (`opencode-go/deepseek-v4-flash` and similar, 2026-08-20).
Whether it also occurs on the **OpenRouter** provider is **not yet verified**
(the open code issue `#43622` notes the reproduction "was done against a local
fake provider, so it is not provider-specific" — the `"unknown"` finish is
client-side parsing of a stream that ended without a finish frame, not
something the provider emits). The provider's contribution, if any, is likely
dropping or truncating the stream (e.g. gateway-level), not emitting `unknown`.
So the upstream `#43622` / v1.18.20 fix is necessary but may not be sufficient
on its own; retesting against both backends after the update is the
load-bearing next step.

Evidence (all from the Go-provider path; no OpenRouter observation recorded):

- Six of sixteen headless implement sessions died this way on 2026-08-20
  (`README.md` line 478: live tally `6 dead of 23` implement/opencode over the
  full day; the 6-of-16 figure is the mid-day hand count in `README.md`
  lines 497–498 and `docs/opencode-build.md` line 68: "6 of 16 implement
  sessions on 2026-08-20").
- Five unrelated tasks, 46k to 230k context; identical logs each time
  (`#43622` reproduction: `step=1`, `"exiting loop"`, `"disposing instance"`,
  37 ms, exit 0, no stderr).
- The `looping.md` billing table (`clankerbar/docs/proposals/looping.md`,
  lines 296–297 / 306 / 334 — lives in the plane repo) describes the Go
  provider as monthly-cap (`opencode Go`) and OpenRouter/Zen as metered;
  that table says nothing about silent-death frequency. The absence of an
  observed silent death on OpenRouter is currently uncited — either add
  an actual iteration-log observation or treat it as unverified.

## The driver's response (CLA-386 / CLA-396)

The driver does not rely on exit code alone. It applies the dead-phase predicate
(`internal/loop/deadphase_test.go`; `loop.go` lines 967–996):

- A session that got past its claim (`res.Claim.TaskID != ""`) but then produced
  nothing (`deadPhase(res)` true) increments the dead tally (`t.dead++`), not
  the completed tally.
- Consecutive dead phases on the **same task** are retried up to the per-task
  budget (`perTaskDeadBound`, `CLA-386`), then the task is **parked** (`CLA-396`):
  the driver files an `OPEN` question for the operator rather than spawning a
  fifth dead session (`loop.go` `parkDeadPhase` at 1740–1785; `deadtally.go`
  `t.dead++` at 74).
- A run-level fleet dead-phase counter (`fleetDeadBound`, `CLA-396`) pauses the
  whole project when the rate indicates the provider/harness looks broken, not
  the individual tasks (`loop.go` lines 1665–1725).
- The retrospective scan (`clankerbar dead-rate`; `internal/deadscan/deadscan.go`)
  reconciles against hand counts (e.g. the 2026-08-20 mid-day count of 6 dead
  of 16 implement sessions; over the full day the scan reports 5 dead of 23,
  matching session-by-session).

The salvage (`loop.go` `salvageStrandedWork` 1566–1610, described 1532; `README.md` 674–736) runs on every abrupt ending — it commits and pushes any uncommitted work in the worktree,
recording the branch so the next session takes over instead of redoing the work.
This is independent of whether the session died silently or loudly.

Additionally, the adapter's automated session-resurrection (`CLA-406`;
`internal/harness/opencode_resume.go`) handles the same quiet-death
signature (`reason: unknown` + zero own-usage) by resuming the same session
via `opencode run --session <id>` after 25 s, probing with a coherence check,
and either continuing (mechanical task-ref match) or falling through to the
dead-phase path unchanged. It is bounded at 5 resurrections per `Invoke`
(`opencode-build.md` 69–74) and was shipped in v0.9.1. When that automated path
exhausts or fails (e.g. the transcript does not survive, or the coherence
check fails), the manual protocol below applies.

## Observed workaround (manual recovery protocol)

When the guillotine/dead-phase effect is witnessed in flight (session ends with
`reason: unknown`, exit 0, zero usage, no branch, identical to the upstream
reproduction), the current manual protocol is:

1. Confirm with the model that it is still present, still holds the session,
   and has not lost its context.
2. Confirm the session/transcript survived (the upstream `#43622` reports
   the instance is disposed on the silent exit; if the transcript survived,
   the `CLA-406` automated resumption may apply — see above; otherwise
   confirm only that the work remains recoverable from the worktree / saved
   branch).
3. Ask it to continue working on the same task.

This is a manual intervention, not an automated retry; the driver's own retry
budget (`CLA-386`) handles retries automatically, but only up to the bound.
Once the bound is reached the task is parked and raised to the operator. The
manual protocol applies to cases where the operator sees the silent death before
the bound trips, or where the retry budget needs to be overridden for a specific
session.

## Open actions

1. **Update to `opencode` v1.18.20+** (the released version that retries
   midstream network errors previously mapped to `finish: unknown` —
   `opencode-agent[bot]` on `#43622`, 2026-08-21). Try it against both the
   Go provider and the OpenRouter provider. Separately track the still-open
   `#43881` (unmerged; proposes failing empty `unknown`-finish streams after
   a clean drain as a retryable response-stream error — the `:1301` shape,
   not an `:1113` exclusion change).
2. **Retest with the Go provider specifically.** If the silent death stops on
   OpenRouter but continues on Go after the upstream fix, the provider-level
   trigger (not just the `"unknown"` finish exclusion) becomes the load-bearing
   finding for the upstream issue.
3. **Contribute provider-level observations back to `#43622`.** The upstream
   reporter (`lecstor`) reports the effect with `opencode-go/deepseek-v4-flash`
   and offers repro script and session logs on request; the Go-vs-OpenRouter
   correlation (observed only on Go so far; OpenRouter not yet verified) is
   additional evidence worth contributing there — matching the user's
   observation that the open code fix was valid but the provider also
   contributed.
4. **Document the result.** Once retested with the updated version, record
   whether the dead-phase rate (measured by `clankerbar dead-rate` and the live
   tally) drops, and whether it drops for both provider backends or only one.

## Related references

- `internal/loop/deadtally.go` — live dead-phase tally (`CLA-402`).
- `internal/loop/loop.go` — dead-phase predicate (`CLA-386`), per-task retry
  budget, fleet pause (`CLA-396`), salvage (`CLA-314`).
- `internal/harness/opencode.go` — adapter parsing (`reason: unknown` at line 626+).
- `clankerbar/docs/proposals/looping.md` (plane repo) — harness roster, billing regimes, open code survey
  (`opencode` metered / monthly-cap), decision record for the loop design.
- `README.md` — `dead-phase tally` command (line 467+), salvage description
  (line 674+), budget breaker, multi-phase harness rules.
- `docs/opencode-tool-schema-limits.md` — separate invisible failure (`#43374` / `#43378`).
- `docs/opencode-build.md` — build version tracking (`1.18.x` stable), `CLA-381`.
- `anomalyco/opencode#43622` — upstream issue (silent exit, `finish: "unknown"`, exit 0).
- `anomalyco/opencode#43881` — proposed upstream fix.
