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
lease), and beacons presence on the same cadence — to the held target and every
sibling project the daemon serves — so a window hours long does not make a
healthy instance read as dead. It logs once on entry and once on leaving. The
hold is not charged to `max_wall_clock`: no session runs while it waits, so it
is idle time rather than draining time, and the breaker's pre-hold verdict
stands when the sequence resumes.

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

## What is NOT a boundary

- A phase's own attempt ladder — a transient retry, a usage-limit probe — is a
  continuation of the session unit already in flight, not a new phase.
  Interrupting it mid-phase would leave the phase's deliverable unfinished, so
  the window applies at the next phase boundary instead.
- The conflict session's model-route ladder (CLA-890) is the same unit: a fresh
  session on the next route after a quota refusal, inside one short conflict
  attempt. It is covered by the start gate above, not between routes.
- The deterministic close-out (above) spawns no session at all.

## What the beacon says

A window-paused daemon reports `draining` with `pauseReason: "schedule"` and
`pauseUntil` (the window's end, which `PauseWindows.ActiveAt` already returns),
so the console's banner (CLA-907) can name the window and its resume time instead
of showing the same bare `draining` any pause used to send. A sequence held at a
boundary (above) also names the task and the phase waiting:
`state.taskRef`/`state.phase` on the same draining beacon, which is the pair the
console's hold reads.

The other causes name themselves too (`console`, `fleet`, `startup`, `budget`)
and carry no resume time — they end when the operator acts, not at a moment. A
beacon carries ONE reason: when several causes apply at once the console, fleet,
startup and budget pauses all outrank the schedule, because the schedule clears
itself while the others wait on a human.

The local `BEACON` file in the state dir carries the same payload (plus the
schedule's IANA zone, which is local rendering only — the plane gets an instant
with an explicit offset), so `supervise`'s fleet listing and the console cannot
disagree:

    clanker1: running (pid 4242, version 0.25.6) - paused by schedule until 14:00 AEST (holding CLA-1 at the review phase)

## Stop and shutdown semantics

A STOP, RESTART or HALT marker, or a cancelled context, during the hold takes the
ordinary drain-stop path: the hold returns, the walk ends, and the deferred
handback releases the claim (or ends the lease on pushed work, keeping the
takeover hand-off) exactly as it does for a drain that stops at a phase seam. A
RELOAD is applied in place by the wait and the hold re-reads the schedule, so
removing or shortening the window ends the hold at the next wake.

If the plane refuses a lease renewal because the run no longer holds the task,
the hold ends the DRAIN rather than the run: the claim is dropped with no
handback (it is someone else's task now), no further phase spawns, and the run
continues to its next poll — the same ending as a lost run mid-phase. The pause
flag is cleared for that next poll, because the window may still be active when
the daemon returns to polling.

## Tests

`internal/loop/pause_boundary_test.go` drives the whole drain with `d.now`:

- a window opening mid-session holds the next phase, heartbeats the held run and
  resumes at the same phase when the clock moves past the window;
- a runner-claimed drain holds (and heartbeats) before its first phase;
- a parallel review panel of three members is held before its fan-out — no
  member starts while the window is active — and all of them run once it ends;
- STOP during the hold stops the drain and hands the claim back; a cancelled
  context does the same;
- a lost run during the hold ends the drain without stopping the run, with no
  handback and no next phase;
- the hold's renewal failures are each pinned: `ErrNotWired` stands the beat
  down and keeps holding, three generic errors log the give-up and keep holding,
  `ErrRunLost` takes the drain-level ending;
- a hold is not charged to a short `max_wall_clock`: the sequence still resumes;
- no active window leaves the sequence byte-identical to before.

`internal/supervisor/conflict_test.go` pins the conflict-session stand-down, and
`internal/cli/doctor_pausewindows_test.go` pins the doctor line.
