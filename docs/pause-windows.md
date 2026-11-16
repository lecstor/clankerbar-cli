# Scheduled pause windows

`pause_windows` is the operator's recurring "start no new work" schedule
(`internal/config/pausewindows.go`). It exists to keep paid work out of peak
pricing hours, so it applies to **every new session**, not only to the claim that
starts a task. A session already running is never interrupted — the pause lands
at the next boundary.

## The boundary rule

There are two gates:

1. **Between iterations** (`pickTarget`, `Driver.scheduledPause`): no new drain
   is started inside a window. This includes a recovery or close-out claim.
2. **At every phase boundary** (`drainSeq.checkPhaseBoundary` ->
   `holdThroughPause`): the next phase does not spawn. The driver holds the
   sequence's claim and resumes the SAME run at that phase once the window ends.

The hold keeps the claim, so the task is not re-offered as a takeover: it renews
the lease at the third-of-lease cadence (ten minutes on the plane's thirty-minute
lease), and beacons `draining` on the same cadence so a window hours long does
not make a healthy instance read as dead. It logs once on entry and once on
leaving.

A `lead` window is active too, so a sequence is not started shortly before peak
and then held at its first boundary; the lead only has to cover the time to the
first spawn, not the whole sequence. That is the point of CLA-906: a phase
sequence routinely takes one to two hours, so no short lead can cover it.

## What counts as a boundary

A session is started at exactly these places, and each is covered:

| where | covered by |
| --- | --- |
| the next configured phase | `checkPhaseBoundary` |
| a handoff respawn (same phase, fresh session) | `checkPhaseBoundary` |
| the first phase of a runner-claimed drain (including a close-out claim) | `checkPhaseBoundary` |
| a fix phase spawned after a verification round | `checkPhaseBoundary` (the rewind is another pass through the walk) |
| a sequential review-panel member | each member is a phase pass, so `checkPhaseBoundary` |
| a parallel review panel's fan-out | `checkPhaseBoundary` runs before `runPanelFanOut`, so the whole panel waits together |
| a session-claimed drain's first phase (blind, single-phase, custom prompt) | `checkPhaseBoundary`; the poll gate normally catches it first |

The supervisor's **conflict-only session** is a new paid session and is gated in
`startConflictSession`, before the claim: while a window is active the task stays
approved and the next close-out pass retries it. The **deterministic close-out**
runs no agent session (it merges with `gh`), so the schedule does not apply to
it.

## Stop and shutdown semantics

A STOP, RESTART or HALT marker, or a cancelled context, during the hold takes the
ordinary drain-stop path: the hold returns, the walk ends, and the deferred
handback releases the claim (or ends the lease on pushed work, keeping the
takeover hand-off) exactly as it does for a drain that stops at a phase seam. A
RELOAD is applied in place by the wait and the hold re-reads the schedule, so
removing or shortening the window ends the hold at the next wake.

If the plane refuses a lease renewal because the run no longer holds the task,
the hold ends rather than spawning a phase against a task that is now someone
else's; no handback is attempted for it.

## Tests

`internal/loop/pause_boundary_test.go` drives the whole drain with `d.now`:

- a window opening mid-session holds the next phase, heartbeats the held run and
  resumes at the same phase when the clock moves past the window;
- a runner-claimed drain holds (and heartbeats) before its first phase;
- STOP during the hold stops the drain and hands the claim back;
- no active window leaves the sequence byte-identical to before.

`internal/supervisor/conflict_test.go` pins the conflict-session stand-down, and
`internal/cli/doctor_pausewindows_test.go` pins the doctor line.
