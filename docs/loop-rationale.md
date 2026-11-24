# Loop Rationale

The loop (the driver daemon): polling, drains, phases, claims, budgets, worktrees, routes and waits.

This file holds the design and process reasoning retired from the code comments in
`internal/loop` when those comments were cut to the one-to-three-line rule (CLA-809). The
comments beside the code keep the invariant, trap or constraint an editor needs at
that site; the fuller rationale - why a default is the number it is, how a
failure was measured, which alternative was rejected - lives here. Task refs and
dates are provenance for the reasoning, not a changelog.

Each section is one source file. Within it, a heading names the declaration the
comments belonged to, or quotes the first line when they were a free-standing
section note.


## `internal/loop/budget.go`

### Budget enforcement (CLA-410, CLA-480)

> Budget enforcement (CLA-410, CLA-480). Two accounts are judged, and the
> distinction is the whole point of the stored run-config overlay:
>
>   - a target whose stored document SET a budget is judged by THAT document's
>     budget over its own spend, on its own clock. The local file's dials are
>     not a second, tighter bound for it — a ratified looser ceiling wins.
>   - every target the local file still governs — no stored document, or one
>     that sets no budget — is judged together by the local file's budget, over
>     their combined spend, on the run's clock: the instance-global pool the
>     README documents.
>
> A trip exhausts the account's targets for the rest of the run. Run keeps
> round-robining the others and stops only when every target is exhausted, so
> one project's ceiling can no longer take its siblings down.
>
> The ledgers live on the Driver and are charged where the loop counts a
> session's spend (countTrustedSpend and the supervised wait's probe), so every
> check — between drains, between phases, between attempts, and inside a
> paused wait — reads the same live accounts rather than a figure threaded
> down from the last boundary.

### charge

> charge books a session's spend against the target it ran for and the harness
> that ran it. It is called at the two places a session's spend enters a run —
> a counted attempt and a supervised wait's probe — so the ledger each breaker
> reads cannot drift from the memory the iteration logs and the fleet record
> report.
>
> The key is the phase's harness NAME AS THE CONFIG SPELLS IT (HarnessFor), never
> the adapter's own Name(). The two agree in production - an adapter is fetched
> from the registry by that same key - but only one of them is the currency the
> breaker spends: the per_harness blocks are keyed by config name. An adapter
> whose Name() diverged would silently disable its own per-harness ceiling rather
> than failing, so the ledger asks the config what a session IS and asks the
> adapter only behavioural questions.
>
> An UNTRUSTED phase/session stream is charged nowhere, for the reason
> endUntrustedDrain gives: a figure parsed out of a stream with a hole in it is
> not a measurement. A supervised wait's probe is the one exception — the wait
> cannot avoid spawning it, and its parsed floor is booked so the ledger does
> not under-count the sessions the wait itself adds (supervisedWait stops under
> a spend ceiling rather than poll on spend it cannot see).

### tripBudget

> tripBudget names the ceiling of b reached over this account, or "" if none
> has: the run-wide dials first, then each harness's own block against that
> harness's own spend.
>
> Any trip exhausts the account, so the order only decides which ceiling gets
> named in the log, and the global dials are named first because a config that
> sets both wants the run-wide promise reported as the one that was broken.
> Harnesses are consulted in name order so a run with two blocks reports the
> same dial every time rather than whichever the map yielded first.

### targetElapsed

> targetElapsed is how long target i has been draining. The stored budget's
> wall clock runs on THIS clock, not the run's: a project polled into a daemon
> two hours old has not thereby spent two hours of its own ratified ceiling.
> Zero until the target's first drain. stateMu is not decorative: a parallel
> panel's members run budget checks on their own goroutines while the run's next
> boundary may stamp the clock.

### notePauseHeld

> notePauseHeld books wall-clock time a scheduled pause held target ti at a
> phase boundary (CLA-906). It is idle time, not draining time, so the budget
> clocks credit it back rather than ending a sequence for waiting out a window
> the operator configured. The run-wide clock the local-file budget reads is
> shared, so it always carries the credit; the per-target clock a stored
> overlay reads is credited for the target the hold served.

### markBudgetExhausted

> markBudgetExhausted records a trip against the target(s) its account governs,
> and says so once per account. A stored budget governs its one target; the
> local file's budget is the run-wide promise over every target it still
> governs (no document, or a document that sets no budget), so they are
> exhausted together.
>
> midDrain marks the trip as the reason THIS drain ended, so Run continues with
> the siblings instead of reading the drain's stop as a run-wide stop.


## `internal/loop/checkpoint.go`

### checkpointEvidence

> checkpointEvidence reports whether the plane's record carries the phase's
> exit evidence, and returns the branch that evidence names (empty for the
> branch-less forms). Held-ness alone is NOT the evidence: since CLA-358 the
> driver's lease renewer renews a claim for a session's whole lifetime, so ANY
> session that claimed ends held, and "still holding" cannot distinguish a
> checkpoint from an empty exit (2026-08-24 fleet incident: two implement
> sessions claimed, cut a worktree, hit a silent model EOT, exited cleanly
> with real usage and no edits, and the seam advanced to review on the false
> premise).
>
> The evidence is ANY of three forms (CLA-497) — "the plane's record shows
> this phase did its job":
>
>   - (a) a branch recorded by this run whose tip really exists on the origin
>     remote — today's check, unchanged. A branch that was never pushed is not
>     evidence, and a check that could not run (no git, no repository
>     resolved) is not evidence either: a checkpoint is claimed only on the
>     verified present, never on the absence of a negative. That is the
>     deliberate difference from verifyDeliveries, which warns loudly on the
>     same failing check but does not gate anything on it.
>   - (b) the task having LEFT `ready` (in_review / done / parked / blocked):
>     its own session settled it on the plane. A no-code task can never
>     produce form (a) — there is no branch to record — and without this form
>     it is structurally incapable of checkpointing: the EZY-290 shape (a
>     one-token task-body edit) burned nine sessions in a claim -> empty-exit
>     -> release loop before this existed.
>   - (c) a DECLARED no-code delivery on the task: the same delivery.noCode
>     flag the terminal tools require to close such a task, read off the plane's
>     record. It counts whether or not the session still holds the lease.
>
> Forms (b) and (c) are read through the SAME single-task get_task read
> (planeExitRecord below), not a second implementation. The bounds
> CLA-457 set stay absolute: held-ness alone is still nothing (status
> in_progress proves only that someone holds the lease), and a session that
> claimed, recorded no branch, and left the task sitting in `ready` is STILL
> an empty exit — "nothing happened" — whatever this function would have said
> about its manners.
>
> CLA-576: an Unknown from form (a) is the ORIGIN being unreadable, not the
> branch being absent, and it is returned as its own outcome (unverified=true,
> ok=true) rather than flattened into the refusal. The phase is an UNVERIFIED
> checkpoint: the review phase spawns, carrying the recorded branch, and is
> told to verify the hand-off itself. A Fail — the origin was read and the
> branch is not there — keeps today's refusal exactly. The accepted cost is a
> narrow loosening of CLA-457: a session that recorded a branch it never
> pushed, during a remote outage, reaches a review phase, which is the
> backstop.

### The branch form (a) accepted, and the origin tip the accepting check r

> The branch form (a) accepted, and the origin tip the accepting check read
> (CLA-637): the tip rides the checkpoint as the revision the review phase
> verifies and records, and the seam matches the record's reviewedTip
> against it. Recorded rather than returned on the spot: the review-record
> gate below needs the plane read FIRST, and both the read and the gate are
> one decision.

### Form (a) short-circuits for every phase that does not need the plane's

> Form (a) short-circuits for every phase that does not need the plane's
> record: a verified branch is the strongest evidence there is, and asking
> the plane anyway was never the contract. A review-shaped phase is the one
> exception, because its checkpoint additionally needs the durable review
> record (below) and its freshness baseline (reviewGate), so it pays the
> extra reads.

### CLA-576

> CLA-576: the origin could not be READ for at least one recorded branch,
> and no stronger form held. That is an unverified checkpoint, not an empty
> exit. The unverified form is checked BEFORE the Fail-only diagnostic
> because a branch absent from a readable origin and a branch whose origin
> could not be read at all are different facts, and only the second one is a
> checkpoint. The log line this feeds is the operator's whole account of why
> the checkpoint is unverified, so an Unknown carrying no detail still gets a
> sentence: an empty parenthetical would say nothing at exactly the moment
> the log matters most. The bool, not this string, is the discriminator.

### The review-shaped gate (CLA-split; durable since CLA-637)

> The review-shaped gate (CLA-split; durable since CLA-637): a phase
> whose ONLY deliverable is the review record must not checkpoint on the
> implementer's branch alone. Its
> inherited WIP (the branch the implement phase recorded) satisfies every
> form above, so without this gate a review session that burned its turns,
> hit its ceiling, or died on a silent EOT — having recorded nothing —
> would still advance, and the next phase would be told to apply findings
> that do not exist.
>
> The record must be THIS run's record, under THIS phase's reviewer
> role (B1). Presence alone is not enough and was the shipped bug: a
> task that already went implement -> review -> fix carries the
> previous trip's `## Review` block in the outcome (and, since
> CLA-636, its durable record too), and request_changes (or a takeover)
> returns the task without clearing either. The run id is the freshness
> key the plane embedded in the record and the criterion no stale
> artefact can fake: a re-trip or a takeover is a NEW run, so only a
> record written after this phase spawned can match. The reviewed tip
> is checked against the tip the driver verified for the branch (or
> demanded non-empty when the origin could not be read), so the fix
> phase is only ever spawned on the revision that is still the tip.
>
> Fail-closed, like every other evidence form: a record that cannot be
> read — or cannot be found — is not a review that happened. The cost
> is a re-run of the review; the alternative is an unreviewed PR. An
> unread BASELINE never reaches this gate: an unreadable baseline is
> refused before the review spawns (CLA-663), so this gate judges
> only tasks both reads could see.

### CLA-634 decision (question 03a12578, option 1)

> CLA-634 decision (question 03a12578, option 1): a BRANCHLESS
> (settled-task) review-only checkpoint cannot write the durable
> record — the plane accepts review writes only from the task's
> live holder, and no run holds a settled task — so this variant
> keeps the legacy `## Review` block gate: changed since the spawn,
> a heading on its own line, and a LAST block the spawn-time
> baseline did not carry. Every branch-carrying variant still gates
> on the record.

### planeExitRecord

> planeExitRecord reads the plane's record of the claimed task for the two
> branch-independent exit-evidence forms (CLA-497): the task having LEFT
> `ready` (in_review / done / parked / blocked — its own session settled it),
> or a DECLARED no-code delivery (the plane's first-class "this shipped no
> code"). It is the SAME single-task get_task read that supplies the seam's
> exit evidence — plane.TaskStateSource — not a second implementation of the
> read.
>
> Best-effort like every plane read: ok is false with an EMPTY why whenever
> the read cannot answer at all (no claimed task id, no wired reader, a plane
> blip), and the gate then judges on branch evidence alone, byte-for-byte as
> before this read existed. A read that ANSWERS but shows no evidence returns
> the reason the gate will log — the difference between "nothing happened"
> (task still ready or merely held) and a read that never ran.
>
> The bounds are load-bearing: status "in_progress" proves only that SOMEONE
> holds the lease — held-ness alone was CLA-457's original false positive and
> stays worthless here — and a task still sitting in `ready` is still the
> empty exit, however orderly the session that said nothing about it.

### readTaskOutcome

> readTaskOutcome reads the claimed task's outcome from the plane, best-effort:
> a failure returns read=false and the caller decides what to do without it.
> purpose and consequence compose the log line, because two callers read the
> same field for different reasons — the review seam's freshness baseline, and
> the fix phase's structural grading (CLA-635) — and a shared message would
> misname one of them.

### planeSeamReadAttempts

> planeSeamReadAttempts bounds the retries of a plane read whose failure fails
> the sequence CLOSED (CLA-663 for the review baseline, CLA-686 D3 for the
> verification round's spawn read): the initial attempt plus two more. The
> failure this recovers from is a plane blip — a dropped response, a read that
> timed out — and one retry pair around a 20-second read is enough for that
> without turning an outage into a long stall. Deliberately no sleep between
> attempts: the read's own timeout is the pacing, and the whole point of a
> bounded retry here is to avoid paying for a session (or discarding one
> already paid for), not to wait for a sick plane.

### planeTaskState

> planeTaskState reads the plane's record of one task, retrying a transient
> failure up to `attempts` times. It is the single implementation behind every
> seam read of the task — the review baseline and the verification round's
> fail-closed reads share it precisely so their retry behaviour cannot drift
> apart (CLA-686 D3: the verify spawn's read claimed to mirror the baseline's
> and did not, one attempt against three).
>
> ok is false when there is no task id, no wired reader, or every attempt
> failed; ErrNotWired is never retried, because asking an unwired client again
> cannot wire it. `purpose` names the read in the log line only; the caller
> decides what an unreadable task MEANS.

### readReviewBaseline

> readReviewBaseline reads the task's outcome as it stands BEFORE the review
> phase spawns, so the seam can require the record to have changed. The task id
> comes from the carried claim (the predecessor phase's), which is exact for a
> resumed phase and is why this runs inside drainPhase, where `prev` is still
> in hand.
>
> Best-effort like every other plane read, with one difference CLA-663 added:
> the read is RETRIED up to planeSeamReadAttempts times, because a blip here
> does not degrade to "judge on other evidence" — it fails the whole sequence
> closed. A failure still returns read=false, and the caller refuses to spawn
> the review rather than letting it run a session whose checkpoint the seam
> would reject.

### closeReviewRound

> closeReviewRound releases a review round's sealed findings (CLA-637) as the
> holding run: record_review({closeRound: n}). The round is
> the one the seam gate MATCHED, never a number predicted at spawn time: the
> plane assigns rounds, and the driver reads them back off the record — which
> is also what keeps this correct across void rounds and panel members.
>
> The close is deliberately a no-op-with-an-error for a client that cannot
> close (an unwired plane, a test double): the caller turns this into a refused
> checkpoint, because a fix phase spawned against findings the plane still
> holds sealed is a session spawned to apply nothing.

### readTaskState

> readTaskState reads the plane's record of one task, for the verification
> round's BEST-EFFORT reads (CLA-638): the fix phase's panel digest and its
> open-MUST digest, where a failure degrades to "judge on other evidence" — the
> fix brief's own instruction to read `reviews.openMustIds` itself. Best-effort
> like every other plane read: ok is false when there is no task id, no wired
> reader, or the read fails. The reads that fail the sequence CLOSED instead use
> readTaskStateRetry, which shares this implementation with a retry bound.

### readTaskStateRetry

> readTaskStateRetry is readTaskState for the verification round's FAIL-CLOSED
> reads (CLA-686 D3): the round's spawn read, and the read the driver makes
> after a closed round to judge its open-MUST set. A failure on any of them
> fail-closes the sequence — the round cannot be told what to adjudicate, or
> cannot be judged — so one plane blip there discards work that has already
> been paid for: at the spawn, the fix round that just ran (the sequence ends,
> the claim is released and the next claim re-runs the phases); after a closed
> round, the verification round itself. That is exactly why the review baseline
> read these claim parity with retries, and they now share its bound
> (planeSeamReadAttempts) rather than taking a single attempt.


## `internal/loop/claims.go`

### releaseHeldClaim

> releaseHeldClaim hands a task the session was still holding back to the queue,
> instead of leaving its lease to run out with nobody heartbeating it.
>
> It runs on EVERY exit from a session, deliberately. A usage limit, a transient
> blip, an outright failure and a clean finish with the work unfinished all leave
> the same dead lease behind — and the usage-limit case is the worst of the four,
> because the driver then goes to sleep for as long as the reset takes. One real
> run slept ninety minutes on a live claim and cost the task a reclaim it had not
> earned; the budget is two before the plane parks the task for the operator.
>
> A claim with pushed work is left alone; Claim.Releasable says why.

### releaseCarried

> releaseCarried hands back a claim a phase was holding open for a successor
> that is not going to run, and returns nil so the caller cannot hand it back
> twice. A nil carried claim is a no-op, so callers need not check first.
>
> It exists because "held open across the seam" and "retry this phase with a
> fresh session" are contradictory: before phases, releaseHeldClaim ran on every
> attempt and neither the limit wait nor the transient backoff could leave a
> lease unattended. Both loop back to a session that re-claims, so both have to
> undo the hold first.

### The session rides the reconstruction (CLA-630)

> The session rides the reconstruction (CLA-630): it is record data for the
> fleet iteration record, not a hold to undo, and the pass it names really
> did spawn. The limit branch calls this BEFORE its stop paths return — a
> reset past the run's ceiling, a stop during the wait, a retry that fails
> to launch — and dropping the field there would omit a spawned session
> from the record. On the paths that loop back, the next attempt's own end
> replaces this one, so carrying it costs nothing.

### releaseHeldClaim

> countUndeclared says whether THIS call site is the one where a held-with-work
> claim means the CLA-384 failure: a phase that finished cleanly and did not hand
> the task over. Only the seam's final-phase handback passes true. Every other
> caller reaches the same branch for a reason that is not a failure to declare -
> a budget trip between phases, a crash, a transient retry, a usage-limit wait, a
> refused handoff - and counting those would make the number mean nothing, which
> is the only thing it has going for it.
>
> It returns whether the claim is ACTUALLY back in the ready queue at the end —
> that is, whether releaseHeldClaim really released it. The seam uses that to
> decide whether a bounded retry may re-claim (a claim left to the takeover path,
> or a release the plane refused, is still live and a fresh session would only be
> refused); the other callers ignore it.

### handBackClaim

> handBackClaim is releaseHeldClaim with the two outcomes split: handed says
> the claim was actually let go (back in the ready queue, or its lease ended on
> pushed work), queued says it specifically returned to the ready queue. Only
> the phase seam reads handed — it must drop the drain's carried claim when the
> handback landed, but keep it for the deferred retry when it did not, and
> releaseHeldClaim's bool cannot tell those apart on the pushed-work path.

### A stream read only in part tells us which calls we SAW, never which th

> A stream read only in part tells us which calls we SAW, never which the
> session made: the settle that released the task may simply be in the bytes
> that never arrived. Handing the task back on that reading posts `ready` over
> work already in review, which is the one outcome worse than a lease expiring
> (CLA-262). Let it expire instead — the plane's own sweep does the right thing
> with it, and keeps the takeover hand-off if there is one.

### CLA-708

> CLA-708: the claim has pushed work, so ReleaseDriver is forbidden — it
> would post `ready` over the branch and throw away the takeover hand-off.
> The plane's expireLease ends the dead run's lease NOW and leaves the
> task an immediate takeover with its branch, which is the same
> disposition the lease's own expiry sweep produces, half an hour sooner.
>
> Leaving the lease to expire remains the fallback on every failure below
> (an unwired plane, a plane older than the field, a refused write), and
> it is the ONLY behaviour for the `res.Untrusted` path above, which does
> not reach here by design.

### A stalled session is named by its cause on THIS path too (CLA-584), no

> A stalled session is named by its cause on THIS path too (CLA-584), not
> only on the held-with-work branch above: a stall that never pushed a
> branch is the shape that leaves nothing behind, so "handed back to the
> queue" alone would send the next clanker to rediscover the cause — the
> MAK-123 post-mortem's complaint. Computed before the release so the line
> that reports its outcome can carry it; the named line REPLACES the
> generic one on success, as it does above.

### expireHeldClaim

> expireHeldClaim ends a dead session's lease on a task whose pushed work must
> survive (CLA-708). It reports whether the plane accepted the handback; every
> refusal falls back to the pre-CLA-708 behaviour — leave the lease to expire —
> and says which refusal it got.
>
> The call is detached from ctx and bounded exactly like the ReleaseDriver call: a
> cancelled run (Ctrl-C, SIGTERM, RESTART_NOW) is precisely when a claim would
> otherwise be abandoned, so the handback has to outlive the signal that
> prompted it without letting a wedged plane hold up shutdown.


## `internal/loop/control.go`

### Restart and reload control (CLA-461)

> Restart and reload control (CLA-461). Three markers sit beside STOP/HALT in
> the state dir, written by `clankerbar ctl`:
>
> 	RESTART       finish the in-flight session, then re-exec at the iteration boundary
> 	RESTART_NOW   kill the in-flight session now (existing process-group kill),
> 	              release any held claim, re-exec
> 	RELOAD        re-read the config file at the iteration boundary, no exec
>
> They are honoured exactly where STOP is honoured: at the top of the run loop
> (the ITERATION boundary), inside waitOrStop (idle polls and supervised limit
> waits) and — for STOP and RESTART — at a verified PHASE SEAM (CLA-879, see
> stopAtSeamMarker). Never inside a session's lifetime for anything but
> RESTART_NOW, whose whole point is to cut that lifetime short.

### restartCheckInterval

> restartCheckInterval is how often the daemon looks for a RESTART_NOW marker
> while it has work in flight. The graceful markers need no watcher — they are
> picked up at the next boundary by construction — but --now exists to end an
> in-flight session that may have hours left, so it is polled for. Two seconds
> keeps the operator's wait short next to a session measured in minutes without
> making the state dir hot.

### watchRestartNow

> watchRestartNow polls for a RESTART_NOW marker for as long as the run lives.
> On finding one it consumes the marker FIRST (so a crash between here and the
> re-exec cannot leave a surprise-restart behind for the next boot), flags the
> restart, and cancels the run context — which is the SAME cancellation a
> Ctrl-C rides: the adapters kill the whole process group with their WaitDelay
> backstop, salvage runs per its existing paths, and any held claim is released
> by the drain's deferred handback. The daemon then exits cleanly and cli.Run
> re-execs it.

### applyReload

> applyReload acts on a consumed RELOAD marker: re-read the config file and
> swap it in. Everything the driver reads through d.cfg picks the new values up
> from the next gate/iteration on (budgets, max-iterations, phases and their
> prompts, poll intervals); what does NOT follow is everything resolved before
> the loop started — the harness adapter built at startup, the target list and
> its pollers, and the process image and environment, all of which need a
> restart (that is what the exec path is for).
>
> A config that fails to reload is NOT fatal. The daemon was running fine on
> the old one; a broken edit mid-flight must not convert a dial tweak into an
> outage, so the error is logged loudly and the current config stays.


## `internal/loop/currentclaim.go`

### The daemon's record of the claim its CURRENT child holds (CLA-708)

> The daemon's record of the claim its CURRENT child holds (CLA-708).
>
> A session that dies with the daemon process — a crash, a kill, a machine
> going to sleep — cannot hand its claim back, and nothing in the supervisor
> used to know there was one. The lease then ran out its full 30 minutes while
> the work sat idle. CURRENT_CLAIM is the missing fact: the driver writes it
> when a claim is observed, updates the branch when one is recorded, and removes
> it on every path where it lets go. A file still present when the child exits
> therefore means the child did not let go, and the supervisor hands the claim
> back on its behalf.
>
> It lives in the daemon's own state dir — the directory the supervisor already
> owns and the child is told to use — so the supervisor reads it without any
> new channel, and a normal daemon exit leaves nothing to find.
>
> The schema is deliberately small and stable: the supervisor only needs the
> ids to call the task tools, the project to pick the right MCP endpoint, and the
> branch to choose between release (none) and expireLease (pushed work). The
> claim file is NOT a source of truth about the plane: the plane refuses the
> call if the run already ended or was taken over, and that refusal is a
> logged no-op.

### noteCurrentClaim

> noteCurrentClaim folds one observed claim snapshot into the file. It is the
> daemon's only writer: the lease renewer sees every claim transition, including
> the settle, and the salvage path calls it after recording a late branch.
>
> A snapshot that is not Held (never claimed, or already settled) removes the
> file: nothing is owed to the next process. ClaimedAt is preserved across
> updates of the SAME claim, so it means "when this claim was taken", not "when
> this record was last rewritten" — the supervisor logs it when it hands a dead
> claim back.


## `internal/loop/deadtally.go`

### deadtally.go

> deadtally.go — the run's dead-phase tally (CLA-402).
>
> The dead-phase rate is the number that decides whether a fix to the opencode
> gateway worked, and until CLA-402 the only way to get it was a hand scan of
> the iteration logs (CLA-396 counted 6 dead of 16 implement sessions on
> 2026-08-20 that way, in the middle of an investigation). This file makes the
> driver count it live: every phase session that got past its claim is a phase
> run, and one that then died producing nothing is a phase dead, broken down
> by phase name and harness, and reported with its denominator where the
> operator watches — the daemon log.
>
> The predicate is the driver's own deadPhase classification, plus the
> operator's exclusion (2026-08-20 decision f518a454): a session that never
> got past its claim — a refused takeover — increments NEITHER counter, because
> such a session satisfies both conjuncts of deadPhase() while being a correct
> refusal, not a death. The one deliberate difference from the `dead` variable
> in drainPhase is the absence of the `!last` conjunct: that conjunct exists to
> veto a checkpoint the phase owes, which a LAST phase owes nobody — but a dead
> last phase is still a dead phase, and the rate is a measurement, not a seam
> decision. Counting it keeps the live tally and the retrospective scan
> (internal/deadscan) agreeing on what a dead session is.

### tallyDead

> tallyDead books one finished phase session into the run's dead-phase tally,
> and reports whether this exit was booked as a DEAD phase. The report is what
> lets a caller that books failures of its own (the review panel's member
> handler, CLA-639) avoid counting the same exit twice.
>
> It is called once per session (per Invoke), with the same classification
> inputs drainPhase computed for the seam. A session counts as a run when it
> got past its claim — claimed a task, or resumed one it was seeded with —
> because a session that never got past its claim (a refused takeover, a
> launch that observed nothing) is a correct refusal, not a phase run, and
> increments neither counter. Of the runs, one is dead when the driver's own
> dead-phase classification fires: the final step finished with reason
> "unknown" (opencode's marker for a session that died without a final
> answer), no branch was recorded, and no cap or stream failure explains the
> end — the "produced nothing" shape CLA-386 and CLA-398 were built around.

### unbookDead

> unbookDead removes a dead booking tallyDead made for an exit that is later
> classified as an infrastructure miss (CLA-703) rather than a death. It is the
> counterpart of the booking and exists because the booking has to happen when
> the exit arrives, before the review seam has decided whether the session ever
> reached the backlog. The run count is deliberately NOT decremented: the
> session did run; only its dead classification was wrong.

### tallyEmpty

> tallyEmpty books one held-but-empty implement exit into the run's dead-phase
> tally as a dead phase (CLA-457). It is called ONLY at the seam, where the
> driver has just classified the exit as empty — the 2026-08-24 fleet-incident
> shape: a clean stop still holding the task with no branch recorded
> anywhere the plane can fetch. That is the same "produced nothing" class as
> the CLA-386 quiet death, so it feeds the same rate an operator watches; it is
> booked SCENED here rather than folded into the generic deadPhase predicate
> because the classification lives at the seam (implement, non-last, would-be
> checkpoint), and only the seam has that context. The run was already counted
> by tallyDead; this adds its dead.

### panelMemberFailBound

> panelMemberFailBound is how many consecutive tasks the SAME review-panel
> member (CLA-639 M4) may record nothing on before the driver says so loudly.
>
> Deliberately a small constant of its own rather than the dead-phase ladder:
> perTaskDeadBound (4) is the budget for a phase whose death strands the task
> and is retried inside one drain, and fleetDeadBound (5) pauses a whole
> target — neither fits a member that is skipped past, on purpose, so the panel
> and the fix still run. Three consecutive tasks is where a member's failure
> stops looking like a blip: the member has a fixed harness and model, its
> prompt is the same every time, and a member that records nothing three times
> running is a member whose harness, model or brief is not producing reviews.
> The warning names the member, its harness and its model; the streak resets
> the moment that member records, and nothing is parked on it — parking a task
> because ONE reviewer of four wobbled would strand the panel's real findings.


## `internal/loop/delivery.go`

### verifyDeliveries

> verifyDeliveries checks, against local git, what the session told the plane it
> delivered — and says so out loud (CLA-253).
>
> The plane is a pipe: it stores a recorded branch and a declared merge and takes
> the clanker at its word, because it has no access to a git host and, by the
> standing decision behind this, is not being given any. The driver is already
> local and already in the tree, so it is the one place the claim can simply be
> looked at.
>
> # Warn, do not refuse
>
> A failed check is logged loudly and names what is unpushed or unmerged. It does
> NOT override the session's report, and this is a deliberate first cut rather
> than an oversight: refusing would mean the driver reverting a status a session
> chose, on the strength of a check that can be wrong about which tree it is
> looking at. Loud logging is the floor, it is cheap, and it is reversible.
> Recorded as a decision on CLA-253.
>
> "Refusing the delivery" (CLA-310) lives inside that shape: a PR check that
> fails is a Fail with the failing condition and its remedy named in the log,
> and no verified attestation behind it — the driver's verdict on the delivery
> is refusal, not a rewrite of what the session told the plane.
>
> # Fail open
>
> A check that could not run reports "could not verify" and the run carries on.
> Blocking a legitimate closure because the tree could not be found would be worse
> than the gap this replaces — and an Unknown is never allowed to read as a pass:
> the attestation below is written only for a check that actually ran.

### Resolve the claim's repo AUTHORITATIVELY from the task's own record —

> Resolve the claim's repo AUTHORITATIVELY from the task's own record —
> "the task's repo field (primary repo when unset)" is the doneWhen's
> words, and the plane's task row always carries it (CLA-437). The
> session-declared value is secondary: the new tool set has no call that
> carries `repo` alongside a branch (record_progress has no repo field),
> so it is normally empty. Mirror sessionDir's degradation exactly: an
> unwired plane falls back to the session's declaration, then to this
> target's primary repo.

### Detached and bounded PER REPORT

> Detached and bounded PER REPORT: a shared deadline would silently degrade
> every claim after the first slow one to "cannot check". Detached because a
> cancel arriving mid-check should not turn a real answer into a half one —
> the guard above is what makes cancellation prompt. The PR check's own
> polling budget sits well inside this deadline, so one slow GitHub read
> cannot starve the git checks that share the report.

### attestMerge

> attestMerge writes the driver's verdict onto the task's delivery record, so the
> answer survives the log. Only for a check that RAN: `mergeVerified` means "I ran
> the ancestor check and this is what it said", and writing it off the back of a
> check that could not run would be the same false attestation this exists to
> catch. Best-effort throughout — an unreachable or unwired plane costs the record,
> not the run, and the loud log above already carries the finding.


## `internal/loop/digests.go`

### verificationDigest

> verificationDigest is the driver's brief for a verification round (CLA-638):
> the prior open MUST ids, their findings, and the delta base, read from the
> plane's released records and PREPENDED to the verify brief the way the fix
> phase's graded list is. It exists because ids and rounds are plane state, not
> config: the driver reads them after the previous round closed and hands the
> session exactly what it must adjudicate.
>
> A record still sealed or void contributes nothing: a sealed round's findings
> are withheld by design (B2), and a void round never happened (B4). An empty
> open list is not an empty digest — the session is still told the delta base
> and that the gate currently reads as clear, so a verifier that sees a problem
> states it rather than silently passing a hand-over it cannot explain.

### settledIDs

> CLA-671: the fix phase marks the MUSTs it answers `fixed` or `refuted`
> itself, so a verifier handed only the OPEN list never sees a refutation —
> it changes no code, so the delta does not show it either — and a wrong one
> rode to in_review unexamined. Every MUST of this cycle already marked
> settled is listed for the verifier to confirm (leave it) or reject (set it
> back to open). Bounded: the round cap keeps a cycle to three rounds.

### mergedFindingsDigest

> mergedFindingsDigest is the fix phase's work list when the review ran as a
> PANEL (CLA-639, B11): every finding from every member's released record, in
> one list, with the union rule the design settled on (A1).
>
>   - Every MUST from ANY member is open and printed by its plane id. Consensus
>     never filters a MUST out — the evidence that motivated the panel (section
>     1.2) is two blockers a single reviewer missed entirely, so the union is the
>     gate.
>   - SHOULD and NIT are listed with the members that raised them, so the fixer
>     can see which are opinions repeated by two reviewers and which are one
>     reviewer's preference. They are recorded, never gating.
>
> The driver builds the list, never the fixer (B11/D3): the fixer applies it and
> has no vote on severity — a MUST it disagrees with is refuted through the
> plane, not downgraded here.
>
> expected is the panel's reviewer roles, in order, as the driver resolved them.
> A role with no record in round is named INCOMPLETE: that is the member a
> dead, capped or failed session never got to record, and the fixer must know
> the list is the union of the reviewers that RAN, not of the panel that was
> meant to. round is the panel's round; 0 means no record was matched at all.
>
> Round and status are BOTH filters (CLA-639 S1). The round is the one this
> panel actually wrote: listing every released record would relist an earlier
> cycle's findings after a verify rewind, and a member that recorded in an
> earlier round would count as having recorded THIS one. The status filter
> drops every finding the plane already shows settled — fixed, refuted or filed
> by the fix phase's own findingUpdates, or by a verifier — because handing the
> fixer a settled MUST back as open work is how a fix round spends itself
> re-fixing what already landed. An open (`""`/`open`) finding is the list.
>
> Empty only when there is nothing to grade (no member recorded and none is
> missing) — the shipped single-review path then keeps its own digest, so a
> config with no panel is byte-for-byte unchanged.
>
> failed carries, per reviewer role, the reason that member recorded nothing
> and whether the seat was a TRIAL one (CLA-639 M4, CLA-784): naming the
> missing member is not enough for the fixer to judge whether the short list is
> bad luck or a member that never ran, and a trial seat's failure must not read
> as a lost required reviewer.

### panelRoles

> panelRoles lists the reviewer roles a fix phase's prompt should treat as the
> panel: the member phases immediately before it in the CAPTURED sequence.
> Empty when the phase does not follow a panel, which is the shipped
> single-review shape.
>
> The role is POSITIONAL (`reviewerRoleFor`), never the member's log label. The
> durable record and the seam gate are keyed on the positional role, so a named
> member (`alpha`) writes `review-1`; returning the label here would make the
> merged list look for an `alpha` record and report a member that recorded as
> INCOMPLETE (CLA-639).

### branchlessVerificationDigest

> branchlessVerificationDigest is verificationDigest for a BRANCHLESS task
> (CLA-638 × CLA-670): the branchless review writes no durable record (question
> 03a12578, option 1), so there are no plane finding ids for the round to
> adjudicate. The digest therefore hands the verifier the LAST `## Review`
> block itself — the block IS this shape's round record — and says so up front,
> because the empty record set is expected here and must not read as a failed
> hand-off.

### resolvedFindingIDs

> resolvedFindingIDs lists the MUST ids on released records that a verifier has
> ALREADY marked resolved (fixed, filed or refuted). The driver remembers them
> across the round loop: an id that is open again in a later round was reopened
> after a verifier marked it resolved, which is non-convergence and parks the
> task (A6). Sealed and void records contribute nothing, exactly as in the
> digest.

### nonConvergenceReason

> nonConvergenceReason reports why the round loop must stop and park rather than
> spawn another fix (CLA-638, A6): a MUST a previous verifier marked resolved
> that the latest round has open again, or a MUST still open after two fix
> attempts (its id's round prefix is at least two behind the round that just
> closed). Empty means the loop may continue. It reads plane state plus the
> driver's in-memory resolved set, so a takeover (empty set) loses the reopened
> half but the round-prefix half still holds.

### parkVerification

> parkVerification parks a task whose verification round cannot proceed
> (CLA-638): the round cap with an open MUST (B7), the non-convergence breaker
> (A6), or a round the driver could not judge. The question CARRIES THE RECORDS
> — the open ids, the round, and where the durable records live — because the
> operator is being asked to overrule the stopping rule, and that needs the
> evidence.
>
> Non-blocking `clarification`, matching parkDeadPhase: the task is already
> parked, so a blocking question would be the wrong shape (any answer would
> un-block it to `ready`, handing the same open MUSTs straight back to a fleet
> whose cycle counter has not moved — the cap would park it again). The park's
> outcome stands alone if the question insert fails.

### verifiedBranch

> verifiedBranch returns the branch the previous phase recorded — the hand-off
> the successor's brief names (CLA-457). The seam folds the branch onto the
> carried claim when it checkpoints, so for a checkpointed phase the claim's
> branch is the one the evidence gate judged; the recorded-Branches fallback
> covers a claim that was never re-judged (a resumed-with-WIP predecessor, or
> a defensive hole). The predecessor's checkpoint is gated on this same
> evidence, so an empty return here is a misconfigured sequence announcing
> itself via a standing {{branch}} placeholder rather than a plausible runtime
> state.
>
> CLA-576: for an UNVERIFIED checkpoint — the origin could not be read — the
> returned branch is recorded but NOT verified, and the caller that builds the
> successor's brief must read Result.CheckpointUnverified and select the
> unverified brief: this function names the branch, it does not vouch for it.


## `internal/loop/done_apply.go`

### reportKindFor

> reportKindFor decides the kind from the CAPTURED phase's prompt, never the
> live one: a handoff respawn replaces ph.Prompt with the predecessor's
> successor text, and the successor still owes the same report its phase does.
> The checkpoint variant is resolved first, exactly as invocationFor does, so a
> branchless or unverified phase - which still runs a shipped brief that writes
> its own records - expects no report.


## `internal/loop/drain_claim.go`

### declineUnpreparable

> declineUnpreparable returns the task to claim after the pre-claim worktree
> check, or nil when the offer must be left alone. A deterministic local
> refusal is found BEFORE the claim, so it normally costs no reclaim: the task
> is simply not claimed, the drain idles, and the next offer retries once the
> tree is fixed.
>
> That is only safe while the tree can be fixed. A tree that refuses every
> check never parks and never lets the queue advance — next_task keeps offering
> the same head — so the refusal streak is bounded: at startRefusalBound the
> claim is taken anyway, the post-claim prep refuses it again, and startRefused
> parks the task for the operator. One reclaim buys the park, which is the
> ending the claimless path can never reach.
>
> When the plane offered a ReadyNext (a declined takeover or close-out), that
> task is checked and claimed instead; usedReady says the returned task is that
> fallback, so the caller does not offer it twice.


## `internal/loop/drain_phase.go`

### drainPhase

> drainPhase runs ONE phase to a clean finish, absorbing usage-limit pauses
> (supervised wait) and transient blips (exponential backoff) by re-running the
> SAME session — neither costs a drain count. Returns the tokens/cost consumed on
> a clean finish; stop=true if a STOP/cancel landed during a wait; err only on a
> genuine, non-retryable failure (or exhausted retries).
>
> held is non-nil only when this phase ended cleanly, is not the last, and left
> the task's claim open for its successor — see drainPhases, which either carries
> it to the next phase or hands it back.
>
> prev is the claim the PREVIOUS phase left held, and is what fills the task/run
> placeholders in this phase's prompt. nil for the first phase, which has no run
> to resume and claims one of its own.
>
> prior carries the run's start and this target's index down into the drain: the
> start is the run's clock (which a local-file budget and waitPastBudget measure
> on), and the index names the target's ledger. The ceilings themselves live on
> the Driver (see charge and budgetStop), so every wait inside the drain reads the
> same live account the between-drains breaker does.
> phaseIdx is this phase's 0-based position in the sequence, carried only so a
> phase with no name can still be NAMED in a log line — ph.Label(phaseIdx) falls
> back to "phase 2". An unphased drain passes 0 and never reaches those lines.
> ti is the target's index, for the fleet claim reflector wired into the lease
> renewer below (CLA-510).
>
> phases is the sequence as CAPTURED when the drain started — the same slice
> drainPhases iterates. It is what this phase's IDENTITY is read from (its
> built-in brief, its position in the sequence), never the live config
> (CLA-664): a RELOAD marker is applied inside waitOrStop, which runs during a
> first-phase limit wait or a transient backoff in THIS drain, so the live
> config can be a DIFFERENT sequence than the one in flight. Reading it here
> would let the two disagree — the in-flight review gated by the reloaded
> position, the "PHASE i of N" text rewritten — while drainPhases still walks
> the captured slice. The rule the capture enforces: a RELOAD takes effect at
> the next drain, never mid-sequence.
>
> panel is what the panel stage this phase follows left behind (CLA-639): the
> round its members' records landed in, zero when no panel ran or no member
> recorded. It is the fix phase's grading input (mergedFindingsDigest reads
> that round and no other); the unphased drain and a phase that does not follow
> a panel pass the zero value.

### The review seam's freshness baseline, read ONCE before the attempt lad

> The review seam's freshness baseline, read ONCE before the attempt ladder:
> the phase's outcome as the review found it. A handoff respawn re-enters
> drainPhase with the same carried claim and takes a fresh baseline, so a
> successor must still write the record its own session is responsible for.
>
> CLA-663: an unreadable baseline refuses the phase BEFORE it spawns. The
> seam cannot prove a record is this trip's from a baseline it never read —
> so the old order spawned the full review session and then threw its
> checkpoint away, and the claim (HasWIP) lapsed into a takeover that
> re-ran implement AND review. Refusing here pays the read — already
> retried by readReviewBaseline — and nothing else, and leaves the claim
> with the same disposition a seam refusal leaves it: released when it is
> releasable, left to lapse into a takeover when it carries pushed work.
>
> The reviewer role is the other half of the gate's key (CLA-637): one
> name, read from the same resolved config the brief is built from, so the
> session records under exactly the role the seam will match.

### Where this phase's sessions run, resolved ONCE per phase (CLA-437)

> Where this phase's sessions run, resolved ONCE per phase (CLA-437): the
> task repo's checkout when it resolves, the target's workdir when nothing
> does. Before the attempt ladder, so a repo that resolves to nothing fails
> the phase with repo_not_found before anything is spawned — a session that
> starts in the multi-repo parent reads a sibling's rule files and, on a
> cwd-scoped grant, hits the wall this replaces. The same directory then
> serves every retry: the task does not change between attempts.
>
> The second value is the task's repo identity (CLA-437), used for the
> pr_base override; the first is the spawn directory.

### The branch this task's repo PRs target, resolved ONCE per phase like t

> The branch this task's repo PRs target, resolved ONCE per phase like the
> spawn directory above, and for the same reason: it does not change between
> attempts. Filled into the prompt's {{prBase}} placeholder by invocationFor
> (CLA-608). Never fails here: an unresolvable probe falls back to the
> configured integration branch, which is the text the briefs carried
> before this feature existed.

### Rescue whatever the session left uncommitted, FIRST

> Rescue whatever the session left uncommitted, FIRST — before the handback,
> because a successful salvage changes what the handback should do: a task
> with a branch recorded on it is no longer safe to release (CLA-314).
>
> NOT for the review-only gate (CLA-split): its deliverable is the record
> on the PLANE, not the tree, and anything uncommitted in its worktree is
> generated output from running checks — which the brief forbids it to
> commit. Salvaging it would (a) commit the unattributable change the
> review's own record forbids, and (b) push a new tip AFTER the reviewed
> tip it just recorded, so the fix phase's tip comparison would fail and
> it would park a healthy task for a mismatch the DRIVER created. A
> review that genuinely dies leaving source edits loses them, which is
> exactly what "review only, change nothing" means.
>
> A tool-loop kill is salvaged like any other end, but not with the
> emphatic "produced nothing" line: a clean tree there may well be work
> the session committed and pushed before it started looping.
>
> A run the plane says is gone is not salvaged at all: the task is
> someone else's now, and the plane told this run not to commit or push.

### CLA-457

> CLA-457: held-ness alone must never advance the seam. Since CLA-358 the
> driver's lease renewer renews a claim for a session's whole lifetime, so
> ANY session that claimed ends held — "still holding" is a constant, not
> a checkpoint signal. A non-final phase reaches its checkpoint ONLY when
> the plane's record carries the phase's exit evidence: a branch recorded
> by this run whose tip really exists on the origin remote (the implement
> phase in the shipped sequence). The evidence is computed even on a
> handoff: an HONORED handoff continues the same phase regardless (its
> successor has nothing to falsely resume), but a REFUSED handoff must be
> re-judged as the bare exit it really is, and this is the only place the
> verification can run.

### A handoff holds the seam OPEN even without evidence

> A handoff holds the seam OPEN even without evidence: its successor
> continues THIS phase's job, so it is not the false-premise advance the
> gate exists to stop. The evidence gate governs only the bare seam
> advance to the NEXT configured phase (handoff == ""); a refused handoff
> is downgraded to that bare judgement in drainPhases.
>
> CLA-638: a verification round that left an OPEN MUST holds the seam
> even though it is the last phase, because the driver spawns another
> fix against it. A clean round does not hold.

### CLA-703 backstop

> CLA-703 backstop: the primary fix is the adapter's MCP preflight, but
> a session can still end with no clankerbar call and no record (an
> older build, a session that acted before its tools were offered, a
> harness whose preflight does not exist). Re-run it ONCE with the same
> brief before the panel records the member as failed. The claim was
> deliberately kept above, so a resumed phase retries the same run; the
> spend is already counted, so the retry is visible to the budget.

### Stopping here ends the whole run, so say WHAT was judged non-retryable

> Stopping here ends the whole run, so say WHAT was judged non-retryable.
> Without it the operator gets "exited 1 (non-retryable)" and no way to
> tell a genuine failure from a blip the classifier merely does not know
> yet — which is the one thing they need in order to report the gap. The
> text is the harness's own diagnostic scope, never the raw stream, so
> the agent's narration is not quoted back at them (CLA-258).


## `internal/loop/drain_phase_prep.go`

### CLA-697 review finding 1.review-3.1

> CLA-697 review finding 1.review-3.1: this claim is the ONE the
> whole panel is working under. Releasing it here would expire a
> lease the other members' in-flight sessions are still appending
> their records under — their durable writes would be
> unauthorized and the panel would lose findings it had already
> earned. Only the JOIN may dispose of it, and it does: the
> panel's deferred handback runs once, in drainPhases, after every
> member has ended.

### CLA-635

> CLA-635: the FIX phase's brief carries the driver's structural grading of
> the review record (B5). Read once per phase, before the attempt ladder —
> the record cannot change while this phase runs except by this phase
> itself, and every attempt must see the same grade. A read that fails
> leaves the digest empty and the brief falls back to the raw block, which
> the fix phase reads with get_task anyway.
>
> Like requireReviewRecord above, the signal is the CAPTURED phase, not
> ph.Prompt and not the live config: a handoff respawn replaces ph.Prompt
> with the successor's continuation wholesale, and the successor of a
> mid-fix handoff needs the same graded list its predecessor got — its
> continuation (fixTerminalRecord) already holds it to the grade, so the
> grade itself must be the one thing the respawn does not drop. The resolved
> prompt is the builtin fix brief exactly when the operator left the phase's
> own Prompt empty; a custom fix prompt owns its wording and gets no digest,
> the same convention the variant swap in invocationFor follows.

### CLA-639

> CLA-639: when the fix follows a review PANEL, the driver's list is the
> merged one (B11) — every member's released record for the panel's round,
> MUSTs open from any member, SHOULD/NIT annotated with who raised them,
> and the members that never recorded named as incomplete. It REPLACES the
> legacy block's grading: the panel writes no block, so the legacy digest
> above has nothing to grade, and the fixer must not be handed one
> reviewer's list as though it were the panel's. The read is the same
> get_task; a failed read leaves the legacy digest above in place, exactly
> as before the panel existed.
>
> panel.round is the round the panel's records landed in (CLA-639 S1), and
> it is the ONLY round the merged list may read: with round 0 the digest
> would relist an earlier cycle's findings after a verify rewind and count
> a member that recorded in an earlier round as having recorded this one.

### CLA-671

> CLA-671: on a fix round after a verification round, the newest record
> holds only the verifier's NEW findings, so a MUST an earlier round left
> open is not in the block the grading above reads; the fixer used to leave
> it untouched and the breaker parked the task as non-convergence. Prepend
> the plane's own open list. Best-effort: an unreadable task state leaves
> the brief's instruction to read reviews.openMustIds itself.

### CLA-638

> CLA-638: the VERIFICATION round's brief carries the prior open MUST ids and
> their findings, read from the plane's released records before the session
> spawns. Fail-closed: a round that cannot be told what it must adjudicate is
> refused before it is paid for, the same disposition CLA-663 gives the
> review baseline — which includes its RETRY (CLA-686 D3): one plane blip
> here would otherwise discard the fix round that just ran and hand the
> sequence back to the queue, so this read shares the baseline's bound
> instead of taking a single attempt. The task id comes from the carried
> claim; the phase is resumed, so it is exact.


## `internal/loop/drain_phases.go`

### drainPhases

> drainPhases runs ONE drain as its configured sequence of phase sessions —
> each a separate harness process, so the context resets between them. With no
> phases configured that sequence is one session and this is exactly the old
> behaviour; see config.Phase for why the split exists and config.EffectivePhases
> for why "unphased" is expressed as a single phase rather than a second path.
>
> Two things have to be true across a seam, and they are the whole design:
>
>  1. The claim is NOT handed back. The next phase resumes the same run with
>     heartbeat(runId) rather than claiming, so the lease has to survive the gap.
>     A handback here would put the task in the queue while we are still on it.
>  2. If the next phase will never run, the handback skipped at the seam happens
>     here instead — otherwise a budget stop leaves the task leased to nobody.

### rawPhases is the same sequence with every EXPANSION collapsed

> rawPhases is the same sequence with every EXPANSION collapsed: index-
> aligned 1:1 with phases, but each review-panel member maps back to the one
> `review` phase the operator wrote (CLA-639), so a member is recognised as
> running the resolved built-in brief its configured phase asked for rather
> than a prompt of its own. The handoff branch below reads it to tell a
> built-in-brief phase from an operator's custom prompt, and it MUST be this
> captured copy (CLA-664): a live read would let a RELOAD applied mid-drain
> reshape the continuation of the sequence in flight — a 3 -> 2 reload drops
> the in-flight fix successor's terminal step, and a custom-prompt phase
> later covered by a built-in gains one.

### CLA-639

> CLA-639: whether this position is one reviewer of a review panel. The
> members are consecutive review-shaped phases, so "before the next
> configured phase" is the same test the shipped sequence already uses.
> Captured BEFORE the handoff branch replaces ph.Prompt with a
> continuation: a panel member never honours a handoff, but the
> continuation's own review position must still be read from the phase
> identity, not from a prompt that is no longer a brief.

### CLA-703

> CLA-703: a LONE review-only phase (no panel) that never reached the
> backlog is an infrastructure miss, not a dead phase and not a review
> verdict. It gets its own drained ending: the claim was handed back (or
> left to lapse) by drainPhase, the tally books the miss separately, and
> the LOG names it so the operator does not read a review as having been
> attempted when the tools were absent.
>
> CLA-738: the name is a log line only. The iteration Outcome stays the
> four-word enum the plane accepts — prose there makes marshalReport
> refuse the whole report, presence refresh included.

### CLA-784

> CLA-784: an optional (trial) seat's invocation error is that seat's
> result, not the drain's — the same rule the parallel join applies with
> its `!m.optional` guard. A member that recorded and THEN hit a harness
> error has already delivered its record; ending the drain on it would
> throw away the required members' round and the fix, the exact harm the
> trial-seat contract exists to prevent. A REQUIRED member's error still
> ends the drain exactly as before.

### CLA-947 review 1.review-1.6

> CLA-947 review 1.review-1.6: a pricing window opened between the
> boundary check and the spawn, so the spawn was refused and nothing was
> spent. That is a SCHEDULING condition, not the evidence-gate empty
> shape above: no retry (there is nothing to retry — the window is
> still open), no tally, no park. The walk ends here; the ordinary
> no-checkpoint handback releases the claim and the next drain's
> boundary holds the spawn until the window ends.

### A held-but-empty exit is a FAILED phase (2026-08-24), not a checkpoint

> A held-but-empty exit is a FAILED phase (2026-08-24), not a checkpoint
> and not an ordinary "ended without holding the task" end: the claim was
> held to the end, it just carried no durable, origin-reachable branch
> evidence. It gets ONE bounded retry, and only when the claim was ACTUALLY
> released back to the queue — a fresh claiming session can then pick the
> task straight back up. A recorded-but-unreachable branch was left to the
> takeover path by the handback (never released), and retrying would seed a
> session against a live lease nothing here renews, so such an exit ends the
> drain. This also covers a handoff the guards REFUSED above (chain cap,
> max-iterations): the handoff exemption ends the moment the successor is
> not going to run, so the exit must be re-judged as the bare exit it really
> is. Deliberately OUTSIDE the dead-phase ladder: the dead shape
> retries-then-parks a task that can kill four sessions, while the empty
> shape returns to the queue for the next drain — the tally measures it
> without tripping the park or fleet machinery, which is how a fleet-wide
> silent EOT shows up in the rate the operator watches.
>
> A runner-claimed retry re-claims first: the empty exit handed the
> task back, so nothing is held to resume.

### A non-final phase that did not reach its checkpoint leaves the next on

> A non-final phase that did not reach its checkpoint leaves the next one
> nothing to resume: the session settled the task itself (worked past its
> brief and finished it), its stream could not be trusted to say either way,
> or the adapter does not observe claims at all. The sequence is over, and
> none of those is a failure — a task that reached in_review in one session
> is a task that got done.

### CLA-396

> CLA-396: a successful IMPLEMENT phase resets the FLEET-wide dead-phase
> counter. Only the implement phase runs the harness that can exhibit the
> zero-usage death — the review phase runs on the claude harness, which
> cannot — so a claude success must not vouch for the opencode gateway:
> letting any success reset the counter would hide a fleet fault from the
> detector that exists to find it. Reaching here means this phase did not
> die and did not fail, so for the implement phase it is the "successful
> implement" the reset is keyed to.

### drainWithRetries

> drainWithRetries runs a single unphased drain — the whole task in one session.
> It is what a config with no `phases` gets, and it is kept as its own entry
> point because that is the shape every existing test drives.
>
> ti is the target's index, taken explicitly rather than from prior: a caller
> that passes spend{start: ...} without a ti must not have its spend silently
> booked to target 0 and judged by target 0's stored budget.
>
> The phase comes from EffectivePhases, not an inline literal, so the unphased
> path carries the same resolution every other path does — the top-level or
> default turn cap (CLA-343), which is precisely what the inline zero-value
> version failed to do.

### onSessionLaunch

> onSessionLaunch, when non-nil, is called immediately before the adapter is
> invoked — the first moment this phase's session launch has actually BEGUN —
> on every attempt the ladder makes. A parallel panel member uses it to
> release the NEXT member's start: signalling when its worktree was ready let
> a later member's own setup and launch overtake an earlier member's, so the
> scheduler's list-order guarantee (CLA-697) did not extend to the adapters
> (CLA-738 review 3.2). Idempotent on the panel side, so a retry's repeat
> call is harmless. Nil for every other caller.


## `internal/loop/drain_seq.go`

### panelIncomplete

> CLA-639: the panel's members that wrote no record this round, and the round
> the panel's released records landed in. A dead, capped or otherwise
> record-less member does not stop the panel: it is listed here, the round
> still closes, and the fix phase is told the panel was incomplete. The
> round number is the one the seam MATCHED on a member that did record, so a
> later member's failure can still release the records already written.

### panelRecorded

> CLA-639 M4: whether any REQUIRED member of this panel recorded, and why
> each member that did NOT failed. The first decides whether a failed LAST
> member hands the round to the fix phase (there is something to apply) or
> ends the drain; the second rides the fix brief, so "the panel was
> incomplete" names the members AND what happened to them.
>
> CLA-784: an OPTIONAL (trial) seat's record deliberately does NOT set this.
> A round where only optional seats recorded is no review at all — there is
> no required member's findings for a fix to work — and is treated exactly
> as a panel where nobody recorded.

### deadRetries

> Consecutive dead phases this TASK has produced (CLA-386). The retry budget
> is per task, not per run: a single silent death is plausibly transient, a
> second on the same task is not, and the same task reached fresh in a later
> run earns its retry again. deadTask names which task the counter counts for,
> because the retry is a fresh claiming session (the dead claim was handed
> back): if next_task hands it a DIFFERENT task, that task has died once, not
> twice, and must not be parked for a second death it did not have (review
> finding). A dead phase always holds a claim, so deadTask is set whenever the
> classification fires.

### finalOutcome

> finalOutcome is the CLAIM's disposition at the drain boundary: one of
> the fleet.Outcome* constants, or "" when nothing was observed (no
> claim state at all, or claimDisposition's nil return). The deferred
> switch below is what makes the RECORDED outcome one of the four — it
> maps "" to released and the drain's own verdicts (parked/dead) over
> the disposition — so the variable itself is not always safe to hand
> on (CLA-738). Never append prose to it: marshalReport refuses the
> WHOLE report on a word it does not know, so free text here loses the
> iteration record AND the presence refresh riding the same report.
> Free-text notes belong in the log.

### CLA-639

> CLA-639: the LOG says the panel was incomplete, by member, whether the
> drain then reached the fix phase or stopped before it. The reviewers
> that DID record are what the fix phase receives; this names the ones
> that did not, so "the panel" never reads as the full panel when it was
> not.
>
> CLA-738: the note is a log line and NOTHING more. It must never be
> folded into the iteration Outcome, which is one of the four
> fleet.Outcome* words: the pre-CLA-738 code appended it to the
> disposition, and marshalReport then refused the whole report on the
> free text — dropping the iteration record and the presence refresh for
> exactly the drains the operator most needs to see. The per-member
> failures already ride each session's `cause`.


## `internal/loop/drain_seq_judge.go`

### CLA-386

> CLA-386: a dead phase is a FAILED phase, not a completed one, and it is
> not "ended without holding the task" either — it never finished the job
> it was spawned to do. The claim was released back in drainPhase (a retry
> re-claims, exactly like a transient one), so the only question left is
> retry vs park. Consecutive dead phases on the SAME task: retry up to the
> per-task budget, then park — a task that can kill four full sessions
> reaches the operator rather than a fifth (CLA-396, raised from two by
> the 2026-08-20 operator decision).
>
> Deliberately BEFORE the error break below: the dead classification
> names a phase that produced nothing whatever its exit code, so a dead
> phase is retried-then-parked, never a run-failing error — the non-zero
> exit that would otherwise stop the daemon is itself part of the silent
> death, not a verdict.
>
> A retry cannot re-seed a resumed phase after the dead handback: invocationFor
> only substitutes the task/run placeholders and sets ResumeClaim when the
> predecessor claim is present, which the handback cleared. Retrying a dead
> RESUMED phase would spawn a session whose brief still carries the literal
> {{taskId}}/{{runId}} and whose handback, salvage and delivery checks are
> all switched off. So the dead retry is scoped to the phase the walk
> STARTED at (`i == start`). A session-claimed start re-claims in its own
> session; a runner-claimed start is re-claimed by the runner before the
> retry. A resumed phase's dead signature still vetoes its checkpoint below
> (the false-premise guard applies to every phase); it just is not retried or
> parked here — the drain ends and the task returns to the queue, where a
> fresh claiming session retries it with a valid seed (review finding). The
> operator decision's "retry the implement phase N times, then park" is
> exactly `i == start` in the shipped [implement, review] sequence. A
> resume-only failure is the exception (see below): it KEEPS the claim, so
> `prev` re-seeds it and it may retry at any non-last position.
>
> CLA-396: a dead phase feeds BOTH counters, but the per-task park is
> checked and taken FIRST — before the fleet counter's trip is even
> consulted. The fleet counter persists on the Driver across drains and
> only resets on a successful implement phase, so it can carry a residual
> count left over from an earlier, unrelated task's death into THIS
> task's own run of deaths. Checking fleet first would let that leftover
> tip the fleet bound one death before this task reaches its own —
> pre-empting a park this task legitimately earned with a fleet-wide
> pause that blames the provider for what is, this time, actually the
> task. Per-task takes priority: a task that has itself earned a park is
> parked, full stop; only once that is ruled out does a fleet-wide run
> of deaths across DIFFERENT tasks get to trip the fleet pause. A session
> that never got past its claim is excluded from both by construction:
> `end.dead` requires `res.Claim.Held()`, and a refused claim (lease_live,
> a lost race) observes no task id, so its death counts toward neither
> counter. This is the discriminator CLA-402's retrospective scan must
> agree with.

### CLA-877

> CLA-877: a resume-only failure KEPT its claim on purpose (see
> settleWithoutCheckpoint), so the retry resumes that same run without a
> re-claim. Its retry is safe in ANY phase position, not only the phase
> the walk started at: the kept claim is still `prev`, so invocationFor
> seeds the retry's brief exactly as it seeded the failed attempt's. Only
> the start phase carries the prep facts (s.prep), and those it keeps —
> minus the stale tip, dropped below.

### CLA-900

> CLA-900: a TOOL-CAP kill is still a death, so it still counts - a run
> of them across tasks must keep the fleet bound, or a fleet-wide stuck
> call (a hung MCP server) would spin sessions forever: the per-task
> counter resets whenever a retry lands a different task. What changes is
> the QUESTION: fleetTrip words a cap kill by what was observed instead
> of blaming the provider (see trips.go).

### A runner-claimed start must re-claim before the respawn when the

> A runner-claimed start must re-claim before the respawn when the
> dead phase handed the task back. The reclaim can land a DIFFERENT
> task when this one is gone, so the counters below follow what is
> actually held now, not what died. A report-resume failure handed
> nothing back: its claim is still held, and re-claiming it would be
> a plane round trip against a live lease (and could only be refused).

### The handoff was REFUSED, so its exemption from the evidence gate ends

> The handoff was REFUSED, so its exemption from the evidence gate ends
> here: the exit is re-judged as the bare exit it really is (CLA-457).
> With no durable, origin-reachable branch evidence that is a FAILED
> empty phase — logged, tallied, never handed to the review brief.
>
> CLA-637: the review stage is over after all. drainPhase deliberately
> left the matched review round SEALED when it detected the handoff —
> the successor was going to close it at its own seam — but the refused
> respawn is the last session that continuation was ever going to get,
> so the round must be released HERE, before the checkpoint advances: a
> fix phase spawned against a sealed round can neither read nor answer
> (finding_sealed) the findings it was told to apply. A close that
> fails refuses the checkpoint, exactly as the seam's own close does.

### CLA-638

> CLA-638: the verification round is the stopping rule's gate. It is the
> LAST configured phase, so the generic `!last` branches above never
> applied to it; what follows is decided HERE from the durable records
> the round just released:
>
>   - no open MUST → the gate passed. The verifier carried the terminal
>     step (push check, PR, in_review) itself; the sequence is done.
>   - open MUST(s) → another fix is spawned, unless the round cap or the
>     non-convergence breaker parks the task first. Never one more round
>     on the driver's own initiative.

### The seam could not judge the round

> The seam could not judge the round: no durable record this run
> wrote, a round that would not close, or a task that could not
> be read back. That is the same fail-closed shape CLA-637 gives
> a refused review checkpoint — the drain ends without the
> hand-over and the claim is released/lapses, so a takeover can
> retry it. It is not the stopping rule parking a disagreement.

### The local fallback counts the same ROUNDS the plane does (CLA-686

> The local fallback counts the same ROUNDS the plane does (CLA-686
> D2). A review cycle is one review round plus the verification
> rounds that follow it, and the plane's `counted` includes the
> review round — so comparing verification rounds ALONE against the
> cap allowed a cycle one round longer than the counted path, and
> the park then read "Round 4 of the 3-round cap". `+1` is the
> review round that opened this cycle.

### CLA-879

> CLA-879: the rewind spawns the fix phase on the same verified
> checkpoint, so it is a phase seam too: a STOP or RESTART dropped
> during the verification round must act here, not after the fix
> round it would otherwise start. The start table may resume at
> verify instead of fix (open MUSTs not at the tip), so the log
> names "a later phase" rather than promising the fix.

### CLA-639

> CLA-639: the merged panel list belongs to the FIRST fix after
> the panel, before any verification round has run. A verify
> rewind is a later fix round: its grading is the verifier's own
> record (plus the plane's open-MUST list), and reprinting the
> panel's list would replace the verifier's findings with a
> stale round. Clear the panel round so drainPhase falls back to
> the shipped single-review path unchanged.

### CLA-662

> CLA-662: the transient arm's wait, in the same place and for the same
> reason as the limit wait below — after the sequence's handback, with NO
> lease held. The rung is the target's streak of consecutive transient
> ends, so a provider that keeps blipping the resumed phase is slowed
> between drains instead of being re-drained the instant the last sequence
> died (and, with implement and the resumed phase on different harnesses,
> the only momentum the first-phase backoff could ever have was this).
> The wait is bounded by retry_cap like every backoff(n), and STOP /
> RESTART / RELOAD are honoured through waitOrStop exactly as they are
> during the limit wait; the drain returns when it ends and Run polls again.
>
> Deliberately NOT fed into sidelineTarget, which the task that added this
> asked us to consider: a sideline says "this target's harness is broken,
> bench it" — its counters and log line are built for a drain that FAILED
> to run a session — and this end returns err == nil. Run's success branch
> would immediately clear the failure counter it read while leaving the
> 15m+ skipUntil in place, so the log would report "the harness succeeded
> again — rejoining the rotation" against a target still benched by a
> sit-out nothing would then account for. Doing it properly means
> redefining what a successful drain proves about the harness, well beyond
> this bound's need: the ladder's growth, capped by retry_cap, plus
> max_iterations is the brake the un-waited re-drain was missing.

### CLA-631

> CLA-631: the sequence ended on a rolling usage limit and the run is owed
> its wait — after the handback above, deliberately, so the wait is taken
> with NO lease held: the claim is back in the queue or, with pushed work,
> left to lapse into a takeover exactly as the old stop path left it. The
> wait itself is the first-phase path's own machinery, reused: the same
> budget breaker inside supervisedWait, the same STOP/RESTART/RELOAD reads
> in waitOrStop, the same probes on the harness that hit the limit, and the
> same stated-reset handling — the difference is only that there is no
> session left to re-run when it lifts, so the drain returns and Run polls
> again. waitPastBudget is consulted first, exactly as on the first-phase
> path: a reset beyond the remaining ceiling is not worth sleeping through,
> so this account stops draining and says when to start a fresh one.


## `internal/loop/drain_seq_panel.go`

### CLA-639 M5

> CLA-639 M5: a BRANCHLESS task cannot run a panel AT ALL, so its panel
> is one reviewer. This shape's review record is the legacy `## Review`
> block written to the task's outcome — the plane accepts a durable
> record only from the task's live holder, and a settled task has none
> (question 03a12578, option 1) — so a second member could record nothing
> a durable round could hold, and the block it would append is the FIRST
> member's own record.
>
> CLA-784: that ONE reviewer is the first REQUIRED member, never blindly
> the first configured one. An optional (trial) seat is skipped here: its
> failure is that seat's own result, and if it consumed the single review
> slot a branchless task would be left with no required review at all
> (review 1.review-2.1 / 1.review-4.1). Validate refuses an all-optional
> panel, so a required seat always exists; the members after the chosen
> one are skipped, with one log line.
>
> The test is the claim CARRIED INTO this phase (`carried`), not
> `branchlessCheckpoint`: that flag is set at the seam from the claim the
> PREVIOUS phase was entered with, so it is still false at the FIRST
> panel member — exactly the position where the trial seat sits in the
> shape this finding names. `carried` is what `drainPhase` itself reads
> for `branchlessPanel` (`prev.Claim.Branch == ""`), so the two agree.

### CLA-697

> CLA-697: fan the panel's contiguous members out concurrently, each in
> its own detached worktree. A handoff respawn continues ONE phase (never
> a panel run), and a branchless checkpoint runs one reviewer by design —
> both take the shipped sequential path below.
>
> The join owns everything the sequential member handler would have done
> one member at a time: the incomplete list, the round close, the run-level
> wait a limited member owes, and the decision not to spawn a fix when
> nobody recorded. With cap = 1 (or a single member) this block does
> nothing at all and the old path runs byte for byte.

### CLA-947 review 1.review-1.2

> CLA-947 review 1.review-1.2: the boundary check above saw only the
> FIRST member's phase. A later member whose own route list is inside
> a "wait" pricing window would reach invocationFor and be refused
> there — booked as a reviewer that recorded nothing, charging its
> failure streak and the dead rate for a scheduled peak. Hold the WHOLE
> panel until every member can start, over every member's list.

### The member claims are the same run the panel resumed; keep

> The member claims are the same run the panel resumed; keep
> the CARRIED claim (with its verified branch and tip) rather
> than adopting one member's copy. A member that recorded
> nothing can end holding a claim that never had the tip
> folded onto it, and adopting that one would hand the fix
> phase an unverified-looking checkpoint.

### The run-level endings, in the order the sequential handler

> The run-level endings, in the order the sequential handler
> checks them: a close that failed is fail-closed and stops the
> drain; a member's budget stop ends the drain and exhausts that
> target (other projects continue); a limit or transient end
> hands the drain its wait (the claim is left to lapse into a
> takeover, never released); no record at all means no fix.

### CLA-639

> CLA-639: the panel's members are skippable where a lone review phase is
> not. A member that recorded nothing — dead, capped before its record
> write, empty under the evidence gate, or turned away by a non-retryable
> error — is RECORDED AS FAILED and the panel moves on, instead of ending
> the drain with three reviewers still unrun. That is the panel's version
> of the doneWhen's "recorded or been recorded as failed/capped".
>
> Deliberately BEFORE the dead/fleet arms: those exist for a phase whose
> death leaves the task going nowhere, but a panel member's death is one
> reviewer lost, not the task lost. Counting it toward the fleet pause
> would let a flaky reviewer bench the whole target.
>
> The round is NOT closed while another member follows: closing it
> would release the earlier members' findings to that member, which is
> exactly the panel independence the seal exists to protect. It is closed
> when the panel is over, whichever way the last member ends.

### CLA-639 M4

> CLA-639 M4: whether the NEXT member, or the fix phase after the
> panel, could still resume this run. The member's own claim is the
> live one when it observed any; a member that observed NOTHING (a
> launch failure) leaves the predecessor's carried claim in place, and
> that is still the run the panel is working. A claim the session
> settled itself, or one already handed back, is not live: nothing may
> be resumed from it, and only the live holder may close a round.

### A member ends the PANEL's stage when it recorded (its record passed

> A member ends the PANEL's stage when it recorded (its record passed
> the seam gate) or when no next member can run — the last member, a
> claim the failed member took with it, or a harness sequence-end.
> Only a failed member with a live claim and a member after it is
> skipped past.
>
> A member whose own round close failed DID record: the seam matched
> a record and the close is a later, separate step. Counting that as
> "recorded nothing" would name a reviewer INCOMPLETE and charge its
> failure streak for the plane's failure to release the round.

### A member that writes its durable record and only THEN ends the

> A member that writes its durable record and only THEN ends the
> sequence (a usage limit or a transient failure) returns before the
> seam evaluates the record gate. Ask the plane the same question the
> seam asks, so the member is counted as recorded instead of unnamed
> INCOMPLETE and charged a failure for writing exactly what it owed
> (CLA-639 fix pass 4). Only the seam's own matcher inputs qualify: a
> verified tip (match it exactly) or the unverified mark (demand a
> tip). A carried claim with neither — a branch with no tip and no
> unverified mark, the shape the checkpoint-fact copy cannot fill
> when the claim is not held — would make the matcher accept any
> record by run and role, looser than the seam ever is, so it is left
> to the failure arm (CLA-639 fix pass 5).

### CLA-639 M4

> CLA-639 M4: the member is a failure the operator has to be able
> to see, so it is counted in the run's per-harness dead-phase
> tally (CLA-402) — the rate that exists for exactly this shape —
> unless the seam's own classification already booked this exit
> (a dead session, or a held-but-empty one). And a member that
> keeps failing task after task is a fault in that member rather
> than bad luck: the streak is counted across tasks and said
> loudly once it reaches panelMemberFailBound. Deliberately NOT a
> park: one reviewer of several failing is not a reason to strand
> the panel's real findings.
>
> CLA-703: a no_mcp miss belongs to NEITHER counter. It is not a
> dead phase (no work was attempted), and it is not the member's
> fault — charging its streak would make an MCP outage look like a
> broken reviewer. It gets its own tally cell and its own log line.
>
> CLA-947: a member held out by a pricing window belongs to neither
> counter either — it never ran, so there is no harness, model or
> brief to blame.

### Earlier members DID record, so the panel hands over rather

> Earlier members DID record, so the panel hands over rather
> than ending: the phase after it (the fix) applies the merged
> list, which names this member as INCOMPLETE, and resumes the
> same run. Checked BEFORE the sequence-end arm on purpose — a
> member turned away by a usage limit is exactly the case the
> bar names ("a usage-limited or dead member does not stop the
> round"), and the sequence is not ending here: if the phase
> that follows hits the same limit it says so itself, with the
> wait that belongs to ITS end.

### CLA-784

> CLA-784: a panel where ONLY optional seats recorded has no review
> to apply — an optional seat's record is that seat's result, not a
> review the fix was waiting for. Treat it exactly as a panel where
> nobody recorded: leave the round sealed (no close) and spawn no
> fix. Reached only at the panel's LAST member; an earlier optional
> seat's record is kept while a required member may still record.

### This member recorded

> This member recorded. A recorded mid-panel member whose session was
> then turned away by a run-level limit or transient failure continues
> the panel exactly as a failed one does: its deliverable is in the
> round, and the wait belongs to the end that actually stops the
> sequence.
>
> The LAST member is the panel's close, so its sequence-end must do
> what the failed last member's arm does: release the round while the
> holder is still live (a sealed round would be voided by the
> takeover, losing every member's record), then hand over to the fix
> that applies the merged list. A close that fails is fail-closed —
> the drain ends with the wait and no fix runs against a sealed
> round.

### Close only when the seam did not already close the round for

> Close only when the seam did not already close the round for
> this member. A session the adapter ALSO classifies as a limit
> or transient can exit cleanly (or be capped with WIP), so the
> seam runs first, matches the record and closes the round; the
> sequence-end is then read by the arm below. A second close is
> the plane's no_open_round, and failing closed on it would skip
> the fix against a round that is already released.


## `internal/loop/drain_seq_phase.go`

### The respawn continues this phase's job under its knobs

> The respawn continues this phase's job under its knobs — turn cap
> and tier still apply. The session's own prompt is framed by
> config.HandoffPreamble, not handed over bare: the successor needs
> the same "resume, don't claim" contract a phase resume gets from
> its brief, and a session-authored prompt cannot be trusted to
> carry that forward on its own. promptBytes is the session's own
> prompt alone — what the size cap was measured against — not the
> preamble-inflated total, so the log line stays comparable to it.
>
> config.HandoffContinuation (CLA-353) is appended after it for the
> same reason: the phase's OWN brief — not only the preamble's
> contract — is otherwise dropped on a handoff, since ph.Prompt is
> replaced wholesale rather than framing the built-in text. For the
> review phase that would silently drop the PR-then-hand-off
> terminal step the moment a session hands off mid-review.
>
> Gated on the phase actually RUNNING the built-in brief (review
> finding): HandoffContinuation matches on name alone, so an
> operator who reuses the name "review" for a phase carrying their
> own custom Prompt would otherwise have the shipped terminal step
> force-injected into a handoff respawn of a brief that never
> established that contract in the first place — clashing with
> whatever terminal step their own prompt names, if any.
> rawPhases (the configured sequence captured at drain start, not
> EffectivePhases' output) is index-aligned with phases: a panel
> member maps back to the `review` phase it expanded from, and every
> other entry is its own configured phase. Never re-read live: a
> RELOAD applied mid-drain must not reshape the in-flight sequence's
> continuation (CLA-664) — the same rule the mode and the count follow.

### CLA-497

> CLA-497: a handoff during a NO-CODE review must not append the
> branch-shaped reviewTerminalStep, which tells the successor to
> open a PR for a task that has no branch and no code.
> branchlessCheckpoint records the checkpoint that opened this
> phase (captured at the seam from the carried claim), so the
> continuation matches the brief this phase's first session ran.
>
> The split adds the POSITION-dependent review mode: a handoff
> during a review-only phase must carry the record-then-hold
> ending, never the commit/push/PR/in_review terminal step. A
> fix phase carries that terminal step by name.

### CLA-637

> CLA-637: the checkpoint that OPENED this phase decides which
> review-only variant the successor continues. A branchless
> checkpoint's successor gets the no-tip shape; an UNVERIFIED
> one has no driver-known tip to hand over either, so its
> successor gets the variant that tells the reviewer to
> establish the tip itself — the verified shape's {{reviewedTip}}
> could not be filled and would leak as a literal.

### The breaker at a phase boundary

> The breaker at a phase boundary — the third place a session ends and
> the loop decides whether to spawn another, alongside between-drains and
> between-attempts. It is not decoration: one measured task spent 92% of a
> whole run's 75M ceiling inside a single session, which a between-drains
> check cannot see coming. A handoff respawn gets the same check, BEFORE
> the respawn: a session cannot spend its way past the ceiling by handing
> off to itself.
>
> The breaker is the TARGET's account (CLA-480): a stored overlay's
> budget over its project's spend, or the local budget over the projects
> it still governs. charge() books every counted phase, so the ledger
> already carries the phases before this boundary.

### CLA-906

> CLA-906: a window holds the next spawn, never a running session. The
> claim is kept and its lease renewed while it waits. Nothing is spent
> during the hold, so the breaker's pre-hold verdict stands: the wait is
> NOT charged to max_wall_clock (see docs/pause-windows.md).
>
> CLA-947: the same hold, one question narrower. `pause_windows` stops every
> route; a route schedule stops the routes it names, and only becomes a hold
> here when EVERY route this phase may start on is inside a window and its
> schedule says wait. Checked second because it is the more specific
> setting: when both apply the operator gets the daemon-wide line first.

### takeBoundaryHolds

> takeBoundaryHolds runs the boundary holds in order and folds the two endings
> that are not "resume" into the walk: a stop, and a lost run.
>
> Shared with the parallel panel's own fan-out hold (CLA-947 review
> 1.review-1.2), so a whole-panel wait answers STOP / RESTART / HALT and a lost
> claim exactly as the per-phase boundary does.

### CLA-466

> CLA-466: announce the phase change NOW rather than at the next poll, so
> the console sees which phase is running without waiting out an idle
> interval. taskRef is empty until a claim has been observed (the first
> phase claims inside drainPhase); from then on it rides every beacon.
> CLA-510: a first-phase claim no longer waits for the next phase
> boundary — the renewer's claim reflector (startLeaseRenewal) revises
> the in-flight ref and beacons the moment the session's claim lands.

### What, if anything, is still owed a handback

> What, if anything, is still owed a handback. Three cases, and getting any
> of them wrong is a task either stranded on a dead lease or posted back to
> the queue over work already in review:
>
>   - the phase handed the claim back itself → nothing is owed;
>   - the phase observed claim state → that is now the authoritative view,
>     including Settled and HasWIP, which releaseHeldClaim reads;
>   - the phase observed nothing (it never launched) → KEEP the predecessor's,
>     or a phase 2 that failed to start would silently drop the handback for
>     the task phase 1 is still holding.
>
> A phase that went off-brief and claimed a DIFFERENT task is its own case.
> noteClaimed replaces the observed claim wholesale, so the seam releases
> that task and reports `released` — which would drop the predecessor's
> still-live claim and hand back a task the sequence was never meant to
> touch. Keep what we had: the task phase 1 is holding is the one owed a
> handback, whatever its successor went and did.

### CLA-497

> CLA-497: whether the checkpoint that opened THIS phase was evidenced
> by the plane's record (no verified branch). Read from the PRE-switch
> claim - the one the seam advanced with: after the switch, carried is
> this phase's OWN claim, whose branch says nothing about the checkpoint
> that opened the phase, and the handoff continuation must match the
> brief this phase's FIRST session ran, not what its last one recorded.

### CLA-639 M4

> CLA-639 M4: a PANEL MEMBER that produced no record leaves the run
> resumable — the next member, or the fix phase, picks up the SAME run
> from the SAME verified checkpoint. Its own result is what carries the
> claim forward (the switch above), and a session that was turned away or
> died before reading the task reports no branch: without this, the next
> member's brief would be handed an empty branch and read as the
> BRANCHLESS variant, whose record is the legacy block the panel cannot
> use. So the checkpoint's resumption facts ride forward from the claim
> that opened this member, onto a claim that is still live and short of
> them. Only a member: every other phase's claim is its own.

### A run-level stop wins over a dead phase

> A run-level stop wins over a dead phase. drainPhase returns stop=true
> for a budget trip and credit exhaustion (lim.Stop) — and on those
> paths end.dead can be set too (a credit-starved session is exactly the
> shape that dies on reason "unknown" with no branch). Retrying a dead
> phase against a stopped run burns another session into the same limit
> and, on the second, parks a task for what was actually a run-wide
> outage instead of stopping the daemon. A run stop is final; the dead
> branch only decides retry vs park when the run is continuing. (A
> resumed phase ending on a live lease is NOT one of those paths — see
> the sequence-end break below, deliberately before the dead arm,
> because that end leaves the run continuing.)

### CLA-631/CLA-651/CLA-662

> CLA-631/CLA-651/CLA-662: a RESUMED phase ended the sequence without
> completing it — a rolling usage limit (CLA-631) or a transient failure
> (CLA-651) — and the RUN must not end with it. The sequence stops
> exactly like the pstop above — the claim is not handed back to the
> queue, so the lease lapses into a takeover with its branch recorded —
> but the run carries on: the wait the signal owes is taken below,
> after the sequence's deferred handback, holding no lease. Broken here
> rather than flowing into the checks below because those read a session
> that merely got turned away: the dead/fleet counters would tick on a
> blip's exit shape (or a limit's "unknown" finish reason), and none of
> that machinery applies to a sequence end the run is ordered to survive.
>
> The transient arm ALSO extends its target's streak here (CLA-662):
> nothing else bounds a provider that keeps blipping the resumed phase
> while the implementing harness stays healthy — the drain is over, so
> drainPhase's retry ladder was never entered, and the re-drain would
> otherwise land the very next instant. A rolling limit extends no
> streak: its wait is a reset, not a rung, and the next drain is
> scheduled after it anyway.
> CLA-639: a PANEL member's sequence-end is judged by the panel handler
> below first — it records the member as failed, closes the round that the
> earlier members wrote into, and only then lets the run-level wait take
> over. Every other resumed phase keeps the shipped break here.

### CLA-662

> CLA-662: a resumed phase that ended any OTHER way than the transient
> sequence-end above — completed, settled, died — breaks its target's
> streak: the next blip on that target+harness is not consecutive with
> the last, and starts the ladder over. The other sequence-end arm, a
> rolling limit, also breaks out above and so neither extends nor
> clears: the reset the run waits out is its own bound. Reset on the
> seam BETWEEN the arms, so "consecutive" means exactly what it says:
> the arm above extends, this one clears.


## `internal/loop/drain_start.go`

### prepareTaskDir

> prepareTaskDir prepares the worktree the drain runs in and returns the choice
> the tree leaves standing: a tree holding unpushed commits cannot resume the
> recorded tip, so the start drops to implement. ok=false means the claim is no
> longer usable — handed back, or parked by startRefused.
>
> A close-out still gets a prepared tree: the close-out brief directs a red
> check's repair onto the branch in this worktree, so there is no close-out
> shape that provably needs no tree.

### markPhaseDone

> markPhaseDone records the phase marker a later claim's start choice reads: an
> evidenced, non-handoff checkpoint of implement, fix or review. A review marker
> says only that the review phase finished — never that the review was clean;
> only phasesDone.implement is read today. Verify is excluded by name because its
> checkpoint means open MUSTs remain, so the seam it holds is for another fix
> round, not a finished phase.


## `internal/loop/fleet.go`

### fleet.go

> fleet.go — the driver's half of activity reporting (CLA-466): presence
> beacons riding the backlog-summary poll, state-change beacons at phase
> boundaries, and one iteration record per drain at the boundary the loop just
> crossed.
>
> Everything here leans on the same design rule the reporter itself serves:
> telemetry is never control flow. Every helper is a nil-safe no-op when the
> target carries no Reporter (not wired, or a test), and nothing in the loop
> branches on whether a report happened.

### iterState

> iterState is one target's mid-drain presence: what `iteration {n, taskRef,
> phase}` the Fleet page should show while this drain runs.
>
> Accesses are guarded by Driver.iterMu: the loop writes at phase boundaries
> (beginIteration / endIteration), and since CLA-510 the lease renewer's
> goroutine ALSO revises the ref mid-session (reflectClaim), so these are no
> longer touched by the loop alone.

### fleetIdentity

> fleetIdentity composes what every beacon for target ti carries: who it is,
> where it runs, what build, which config. The fingerprint is the target's
> EFFECTIVE config — the CLA-410 overlay when one is in force, else the base
> file, the same rule the drain reads through d.targetCfg — because that is
> what the drain runs on. Read fresh per report, so a RELOAD moves it without a
> restart: at once for a target with no overlay in force, and for an overlaid
> one when the refetch at the next poll boundary lands (d.invalidateRunConfigs).

### A target INDEX, not a captured Target

> A target INDEX, not a captured Target: the poll beacons a target before
> checkRunConfigVersion rebuilds its overlay, so a copy would fingerprint
> the document being replaced. d.cfg is read under controlMu because a
> RELOAD swaps it on another goroutine (a panel member parked in
> waitResponsive); the pointer taken there is immutable — a reload installs
> a new config rather than editing this one.

### fleetState

> fleetState derives the presence State for target ti from what the loop already
> tracks — no new bookkeeping beyond the mid-drain marker set in drainPhases and
> the hold marker set in holdThroughPause:
>
>   - held at a phase boundary by a pause window -> draining with the schedule
>     reason, the window's end, and the held task's ref and phase (CLA-908).
>     This outranks the iteration marker because the walk's iteration stays open
>     across the hold while no session is running.
>   - mid-drain                -> iteration {n, taskRef, phase}
>   - console-, fleet-, startup- or budget-paused -> draining naming that cause:
>     it is the one the operator has to act on, and it outranks the schedule.
>   - schedule-paused            -> draining naming the window and its end
>   - otherwise                -> idle
>
> A backed-off target (skipUntil still in the future) reports idle on purpose:
> the back-off is minutes-long and self-clearing, and flapping the state to
> draining for every quiet spell would blur the one signal draining exists for —
> "something is holding spawns closed until an operator acts".

### scheduleState

> scheduleState is the scheduled pause's presence: draining, why, until when,
> and — when a phase sequence is held — what is waiting behind it.
>
> The window is re-read rather than trusted from the flag: schedPaused is
> daemon bookkeeping updated at the poll gate and at a boundary, so it can
> still be set for one idle interval after the window ended (and after a STOP
> landed during a hold). A stale flag reports plain draining, never a reason
> with no resume instant.

### beaconLimited

> beaconLimited tells the plane this daemon is parked on a usage limit
> (CLA-728), for EVERY target it serves. The wait blocks the whole loop, so no
> other project's queue is being serviced either — beaconing only the target
> whose harness hit the limit would leave the other projects' presence rows
> silent, and each of those reads as a dead instance for exactly the reason
> this state exists to correct.
>
> It rides the ordinary beacon path, so the local BEACON file in the state dir
> carries the same state: `supervise` status and the console agree by
> construction rather than by two writers that could drift. resetAt is the
> harness's stated instant, or the zero time when it stated none (or when the
> wait has already measured that instant false).

### beaconSessionTargets

> beaconSessionTargets is the in-session presence lap (CLA-771): the poll's own
> per-target beacon, for the window in which the poll does not run. The running
> target reports what fleetState derives for it (iteration with the current
> claim's ref and phase); every other target the daemon serves reports its own
> state, which is idle - or draining while a pause holds its spawns - because no
> session is running there.
>
> The other targets are not decoration. Each project has its own Fleet reporter
> and its own project-scoped presence row, so beaconing only the running target
> would leave every sibling project's row to go stale for the whole session, and
> that project's console would show the daemon as a dead instance (the same
> symptom CLA-771 exists to fix, one project over). beaconLimited beacons every
> target for the same reason.
>
> running and runningTarget are handed in rather than indexed out of d.targets
> so a hand-built driver - the unit tests - still beacons the target it was
> given; the lap then adds any other target, skipping that index.
>
> Only the running target writes the local BEACON file: it takes no target, and
> its last writer wins. The running target's row is the informative one - the
> iteration, or the task and phase a schedule is holding (CLA-908) - so the
> siblings' beacons are plane-only rather than letting whichever project is
> last in the list decide what `supervise` prints about this daemon.

### pollBeaconState

> pollBeaconState folds one target's presence into the daemon-wide state the
> poll pass writes to the local BEACON. The file takes no target, so with
> several projects one of them has to speak for the daemon, and target order
> must not decide which (CLA-908): a scheduled pause states the resume instant
> the console's banner shows, so an idle or console-paused sibling polled later
> must not overwrite it. A schedule state that names a held task outranks one
> that does not; every other draining cause explains a pause the operator
> already sees in the console and renders no local line, so the first stands.

### LocalBeacon

> LocalBeacon is the JSON shape of LocalBeaconName. The supervisor's roll reads
> it with this same type — one contract, not two mirrored structs — so a field
> or tag renamed here cannot leave the reader behind.
>
> ResetAt is the `limited` state's payload (CLA-728): the instant the harness
> stated, present only while the wait still believes it — exactly the field the
> plane POST carries, so the local file keeps mirroring the next report rather
> than saying `limited` with no moment while the console shows a countdown. A
> pointer rather than a time.Time so `omitempty` can actually omit it (the
> encoder never treats a time.Time's zero value as empty).
>
> PauseReason/PauseUntil are the same contract for a scheduled pause (CLA-908):
> the reason and the window's end, so `supervise` prints what the console's
> banner shows without a plane read. PauseZone is the schedule's IANA zone —
> local only, so the listing can say "14:00 AEST" as the operator configured it
> rather than the machine's zone. TaskRef/Phase mirror the plane POST's pair
> (the running iteration's, or the task and phase a hold is waiting behind the
> window).

### writeLocalBeacon

> writeLocalBeacon refreshes the local beacon file. Fail-soft exactly like the
> plane beacon: telemetry is never control flow, so a write failure is logged
> (once per streak) and dropped — the daemon's own life never depends on it.
> The write is remove-then-create: a concurrent reader (the roll) sees either
> the absent name or complete bytes, never a torn file, and a symlink planted
> at the name is removed, not followed.

### A concurrent beacon won the remove-then-create race

> A concurrent beacon won the remove-then-create race — the claim
> reflector (CLA-510) beacons from the lease-renewer goroutine, so
> two beacons can write this file at once. The winner's file is a
> complete, fresh beacon from the same daemon, which is exactly
> what this write was going to produce: not a failure, and not a
> reason to tell the operator the roll cannot verify.

### reflectClaim

> reflectClaim returns the claim reflector for target ti (CLA-510): it keeps
> the mid-drain presence ref in step with the CURRENT claim the session holds
> — set when a claim is actually observed, cleared when the session settles it
> or holds nothing — and beacons immediately on a change, so the fleet card
> fills in within seconds of a first-phase claim instead of at the next phase
> boundary. It is a REVISION of the in-flight state, never a second iteration
> record: the one record per drain posted by endIteration is unchanged.
>
> It rides the SAME observation seam as lease renewal — the renewer calls it
> from apply for every snapshot its OnClaim hook receives — so there is no
> second watcher to keep in agreement, and a claim that was REFUSED never
> reaches it: a refused claim records no ids, so the adapter fires nothing
> (harness.Invocation.OnClaim). Only a ref CHANGE beacons; a session that
> churns claims cannot turn this into a stream.
>
> Runs on the renewer goroutine, which is exactly why iterMu exists: the loop
> writes d.iter at phase boundaries while this revises it mid-session.
>
> The renewer never reflects after stop() returns: leaseRenewer.apply drops
> snapshots once done is closed, and the write site re-checks done under
> iterMu below, so a snapshot that slipped past the apply guard while done was
> still open cannot land on a later phase's row even if something between the
> two checks were ever made blocking. No revision can land on a cleared row or
> a later phase's row: close(done) precedes the loop's own row writes, and the
> write site re-checks done under the same mutex those writes serialize on.
> The beacon that follows a write is a report send, governed by the reporter's
> own non-blocking contract, not driver state.

### endIteration

> endIteration clears the mid-drain marker and posts ONE iteration record with
> the outcome mapped from what the loop already knows, riding the same report as
> the back-to-normal presence state.
>
> Outcome precedence, decided once here so all four words mean one thing:
>
>   - parked  the dead-phase budget parked the task (parkDeadPhase ran)
>   - dead    the drain ended on a death: a dead phase that exhausted its
>     retry/park paths, a fleet trip, or a returned error (zero-spend ladder,
>     unclassified ladder, repo_not_found...)
>   - else the CLAIM's disposition at the boundary: pushed work left for a
>     successor takeover -> checkpoint; anything else (handed back to ready, or
>     settled by the session itself) -> released
>   - nothing observed at all -> released, best-effort (the iteration ran;
>     exactly one record posts per iteration, always)
>
> Free text is NEVER an outcome (CLA-738): marshalReport refuses the whole
> report on a word it does not know, so a note about the drain belongs in the
> log. A caller that appends prose to the disposition loses the record AND the
> presence refresh riding the same report.
>
> That invariant is enforced here as a last line of defence, not left to
> convention: an outcome outside the four is posted as released with one loud
> line naming the caller's mistake. The alternative is what CLA-738's incident
> did — the reporter refuses the WHOLE report (every iteration plus the presence
> refresh) and then logs only the first failure of the streak, so one prose
> outcome would silently starve the plane for the rest of the session. One
> degraded iteration is the cheaper failure, and the loud line is what gets the
> convention fixed.

### sessionRunID

> sessionRunID is the run the session worked the task under, or "" when it never
> held one (CLA-691). The value is the run's UUID, never the credential the
> session holds.
>
> The split is load-bearing, not cosmetic: the id `claim_task` returns — and
> seeds into a resumed phase's brief — is the COMPOUND credential
> `<run uuid>.<secret>`, and the secret half is what AUTHORISES a write as that
> run (the plane refuses a bare uuid for a credential-bound run). Telemetry must
> not carry a bearer token: this report is stored on the plane and served back to
> every key in the project, while the plane keeps the plaintext secret out of
> every durable record (CLA-509). The uuid is also the only form the per-round
> join can use, since the plane's own records hold bare uuids. `runUUID` is the
> existing CLA-509 helper for exactly this split, so both sides parse the
> credential the same way; the plane splits again on arrival, because a reporter
> is never trusted.
>
> A session that CLAIMED the task observed its own run id, and that is the
> value to record. A later phase's session never calls claim_task — the driver
> resumes the run the predecessor left held — so it has no observation of its
> own, and the id comes from the claim carried INTO the phase. That is the same
> claim the driver substitutes into the session's brief as {{runId}}, so the
> record and the session's own writes agree by construction rather than by two
> lookups that could drift.

### sessionCost

> sessionCost is the harness-reported dollar figure for one session's Result, or
> nil when there is no figure to carry.
>
> Two questions have to be answered yes, and they are different ones. The
> adapter has to be one that reports money at all — Capabilities.ReportsCost,
> false for codex and copilot today — or there is no figure by construction. And THIS
> session has to have actually reported one: Result.CostReported, because a
> session whose stream ended before its harness's accounting reported tokens and
> no dollars, and a zero taken from that silence would read on the console as
> "$0 spent" for a session that cost real money. Absence is "unknown", which is
> what nil means, and it is why the wire field is nullable rather than a zero.
>
> A harness that DID report a zero (a free local model, a cached-only turn)
> keeps its zero: the pointer survives marshalling.

### sessionSpend

> sessionSpend is what one PHASE PASS spent, summed across the attempt ladder's
> sessions (CLA-691).
>
> The fleet record publishes one session entry per pass, and a pass that
> retried is several paid processes: the loop counts every attempt toward the
> iteration's total (its own comment: "each attempt is a distinct session")
> while the entry keeps the LAST attempt's identity. Summing the money here is
> what keeps the per-session figures adding up to the iteration figure — the
> property the operator's cost analysis reads them for — instead of dropping
> every retried attempt's spend on the floor.
>
> Cost is summed only while EVERY attempt that ran produced a figure; one
> attempt whose dollars were never reported leaves the pass's cost UNKNOWN
> rather than partial, because a partial figure presented as the session's cost
> understates it. A pass that reported no usage at all has no figure either,
> however many attempts carried one — see cost(). The token buckets have no such
> problem: they sum exactly.

### cost

> cost returns the pass's dollar figure as a pointer, or nil when it is unknown.
>
> A figure only counts as one when the pass actually REPORTED usage (CLA-737).
> `CostReported` and `UsageReported` answer different questions, and an adapter
> that reports money can still set the first with no usage report at all:
> opencode's export path does exactly that for a session killed before its first
> answered request (a provider.quota error is the common one), where the run's
> export is all zeroes but still a document with a cost. Its zero is silence,
> not "$0 spent", so the pass has no figure to publish — and the wire must not
> carry one, because the plane refuses the WHOLE report with
> "session.costUsd requires session.usageReported" and every other iteration in
> that batch, presence refresh included, is lost with it.

### recordPassSpend

> recordPassSpend folds one attempt's Result into the phase pass's running spend
> and publishes it on the pass's session entry (CLA-691).
>
> It is called from the SAME places the loop adds this attempt's Tokens/CostUSD
> to the iteration's accumulators, and ONLY from there: the loop skips counting an
> unreadable stream and (on teardown) a cancelled run, and the per-session figures
> have to skip those too, or the sessions would add up to more than the iteration
> they belong to. One call site, one rule.


## `internal/loop/handoff.go`

### Session-initiated handoff (CLA-352)

> Session-initiated handoff (CLA-352): a session ends its FINAL message with a
> marker line followed by the prompt for its successor, and the driver respawns
> a fresh session on that prompt — same task, same run-continuity rules as a
> phase resume — instead of moving to the next backlog iteration. The phase
> split (v0.5.0) is this idea at two fixed, driver-chosen points; this
> generalises it to points the session chooses.
>
> The prompt is parsed out of the final message, NOT a file: the state dir is
> deliberately outside every session workdir (CLA-259) precisely so sessions
> cannot write control markers, and the result stream is a channel the driver
> already consumes.
>
> A session-authored respawn prompt is self-directed prompt injection, so it is
> honoured only inside three non-negotiable guards, enforced in drainPhases:
> a respawn counts as an iteration against max_iterations and the token budget
> (checked BEFORE the respawn), an over-size prompt is refused with a logged
> fallback, and a chain of consecutive handoffs is capped.

### handoffChainCap

> handoffChainCap bounds CONSECUTIVE handoff respawns within one phase, so a
> session cannot chain itself indefinitely: at the cap the emitted prompt is
> refused and the sequence falls back to the standard brief — the next
> configured phase, or the drain ending and the task returning to the queue,
> whose next pickup runs on the configured prompt either way.
>
> The chain resets at a phase boundary (drainPhases resets `chain` to 0
> whenever a phase ends without an accepted handoff), so a multi-phase
> sequence can chain up to this cap PER PHASE, not once for the whole task.
> max_iterations and the token budget still bound the total across every
> phase, so this is never the only ceiling — just the one that stops a single
> phase's session from respawning itself forever.

### parseHandoff

> parseHandoff scans a session's final message for the handoff block: a line
> that is exactly config.HandoffMarker (surrounding whitespace ignored),
> followed by the successor's prompt.
>
> found reports that a marker LINE is present at all; refusal, when non-empty,
> says why the block cannot be honoured anyway (no prompt, or over the size
> cap). The two are separate so a refusal can be logged as what it is — a
> handoff the session asked for and the driver declined — rather than
> disappearing into "no marker".
>
> The LAST marker line wins, because the block is defined as ending the
> message: a session that quotes the marker while discussing it and then emits
> a real block must get the real one, and everything above the last marker is
> by definition not the successor's prompt. A marker mentioned inline in prose
> does not match — it is not a line of its own.
>
> A marker line must be UNINDENTED, and must not sit inside a fenced code
> block: a genuine emission is neither. Indentation catches a quotation
> inside a list item; the fence tracking catches the other natural way a
> session quotes the mechanism (or explains why it decided against a
> handoff) — a ``` block, which is itself unindented and would otherwise
> match. Both are real shapes a session explaining itself produces, and
> neither may be mistaken for a real handoff block.

### detectHandoff

> detectHandoff reads a finished session's result for a handoff it is safe to
> act on, returning the successor prompt or "".
>
> Each condition is load-bearing, and they are checked here rather than at the
> seam so every refusal is logged as itself:
>
>   - an untrusted stream is not read for a handoff at all, for the same reason
>     it is not read for claim state (CLA-262): the block may be cut mid-prompt,
>     and half a session-authored prompt is worse than none. The untrusted path
>     already logs loudly, so nothing is added here.
>
>   - only a CLEAN exit hands off. A turn-capped or failed session's final
>     message is wherever it happened to stop, not a deliberate ending.
>
>   - the session must still HOLD its task: the successor resumes the same run,
>     so a settled or never-claimed task leaves nothing to resume. The two ways
>     that happens are logged as themselves (CLA-421), because they are
>     different situations with different consequences.
>
>     A SETTLED task means the session moved it off in_progress itself -
>     done, in_review, parked, blocked (settlesTask: any status but
>     in_progress) - and there is genuinely no run to resume. The
>     refusal is final and correct.
>
>     NO OBSERVED CLAIM means the adapter never saw this session claim anything,
>     so the driver holds no task/run ids - and without them the resume
>     preamble cannot be seeded: handing the successor a brief carrying literal
>     placeholders is worse than refusing. A marker from an unobserved claim is
>     therefore refused: the successor runs a SESSION-authored prompt, and the
>     guards against self-directed injection were sized for sessions whose claim
>     state was observed. The claim cannot be recovered from the plane either:
>     get_task serves presence only, and a run id is a bearer credential no read
>     returns (CLA-889). The lease is left to expire, and the plane's expiry
>     sweep is the path that gets the work picked up with its context intact: it
>     keeps a recorded branch attached as the takeover hand-off.


## `internal/loop/heartbeat.go`

### heartbeatGiveUp

> heartbeatGiveUp is how many consecutive failed renewals the renewer
> tolerates before standing down. One failure is a blip (the plane restarting,
> a network blip); three in a row across a third-of-lease cadence means the
> plane is unreachable for this session's remaining lifetime, and retrying
> every tick would only fill the log. The lease may then expire mid-session -
> which is the pre-CLA-358 behaviour, not a worse one.

### heartbeatInterval

> heartbeatInterval reports how often a live claim's lease should be renewed:
> a third of the lease, the cadence the served clankerbar protocol itself
> prescribes ("heartbeat every ~10 minutes - a third of the 30-minute lease").
> Beating faster costs a round-trip per tick and buys nothing; beating slower
> spends the margin the protocol built in. A lease under three ticks wide
> degrades to one millisecond rather than zero, so a degenerate value still
> renews (and a test can pin the arithmetic without waiting).

### leaseRenewer

> leaseRenewer renews the lease of whatever backlog task the spawned session
> is holding, for exactly as long as the session's Invoke runs (CLA-358).
>
> The run id to renew is learned one of two ways: seeded from the invocation's
> ResumeClaim (a resumed phase continues a live run it never claimed), or
> observed live as the session's own claim_task lands - both adapters that
> track claims fire Invocation.OnClaim the moment the stream carries the ids,
> so an opencode session that claims twenty minutes in is renewed from that
> moment, which is the exact gap that used to kill its lease (the CLA-331
> measurement this task exists for).
>
> The lifecycle is the point. The renewer lives from just before Invoke to
> just after it returns, so renewal PAUSES whenever no child process is
> running - between attempts, across supervised waits, after exit - and never
> beats on a lease the driver is about to release or hand to the next phase.
> Within the window, the adapter's own resurrection machinery (opencode
> CLA-406) is spanned deliberately: those processes continue the SAME session
> on the SAME claim, and the claim's lease does not pause just because the
> child between them died.

### claimFile

> claimFile, when non-nil, is called synchronously from observe with every
> observed claim snapshot to maintain CURRENT_CLAIM (CLA-708) - the record
> a supervisor uses to hand a dead child's claim back. It must run before
> observe returns so a branch is never left queued behind a parked
> Heartbeat. Nil in the heartbeat unit tests, which construct the renewer
> directly.

### presence

> presence, when non-nil, sends one lap of presence beacons on every
> renewal tick (CLA-771), exactly what the poll's per-target lap sends: the
> running target's live state and every other served target's own. The
> renewal tick is the only cadence that still fires while a session runs -
> the backlog poll that otherwise carries presence does not run mid-drain -
> so without this a session longer than the plane's
> INSTANCE_STALE_AFTER_MS (15 minutes) reads on the console as a dead
> instance. It is called BEFORE the beat decision, so a session that holds
> no claim yet (a first-phase session claims minutes in) is still visibly
> alive, and the tick itself keeps firing after the BEAT stands down (see
> run), so presence never depends on the heartbeat's health: the fleet
> report is a different transport from the MCP heartbeat, and a plane that
> refuses renewals may still accept presence. Nil for a target with no
> fleet reporter, and nil in the heartbeat unit tests, which construct the
> renewer directly.
>
> Its signature carries no error on purpose: reporting is fail-soft by
> contract, so a failed report is the Reporter's to log and drop, and there
> is nothing a caller could do with the failure anyway.

### startLeaseRenewal

> startLeaseRenewal begins renewing the spawned session's claim lease and
> wires the observation callback into inv. It returns nil - and touches
> nothing - when there is nothing to renew with: no Releaser on the target,
> or one that cannot heartbeat. A probe invocation is skipped too: a probe
> never claims, so there is nothing to renew and the callback would be dead
> weight on a liveness check.
>
> ti is the target's index, so the fleet claim reflector can revise the right
> presence row (CLA-510). When the target carries a fleet reporter, the same
> OnClaim seam that feeds renewal also feeds that reflector, and every renewal
> tick also sends a presence beacon (CLA-771).
>
> The returned renewer's stop() must be called when Invoke returns.
>
> onLost is called once if the plane refuses a renewal because the run no longer
> holds its task; it is how a caller ends the session at once (see run).

### observe

> observe receives one claim snapshot from an adapter's parser. Latest wins:
> if the renewer has not drained the previous snapshot yet, it is dropped in
> favour of this one - only the newest claim state can be true.
>
> CURRENT_CLAIM (CLA-708) is persisted HERE, synchronously, before this
> callback returns, rather than from the renewer goroutine in apply. The
> difference is a lost takeover: `record_progress(branch: ...)` succeeds, then the
> daemon dies while the renewer is still parked inside a Heartbeat call with
> the branch snapshot queued, and the supervisor reads the old no-branch record
> and sends `release` over pushed work. A local state-dir write is the one
> thing this callback cannot defer; the queueing below stays non-blocking.
>
> Invocation.OnClaim asks for a non-blocking implementation, and this is one
> except at that write and one bound: if FOUR snapshots pile up undrained -
> which takes the renewer stuck inside one Heartbeat call while four claim
> transitions stream past - the fifth waits for that call's
> heartbeatBeatTimeout rather than drop the newest state. Dropping the newest
> is the one loss this channel cannot tolerate; the wait is bounded by stop()
> regardless.

### apply

> apply folds one observed claim snapshot into the renewer's running state,
> logging the transitions that matter. The newest snapshot is the truth: a
> settle keeps the run id and flips Settled, so it arrives as a snapshot whose
> RunID is unchanged - which is exactly why the pause below keys off Held.
>
> The fleet reflector (CLA-510) runs here, on the renewer goroutine, before the
> renewal state folds in. The done-guard at the top bounds what can reach the
> reflector after stop(): stop()'s wait is bounded, so the goroutine can
> outlive it wedged in a Heartbeat call, and a snapshot queued before the stop
> and picked up after it must not revise the fleet row - the driver may already
> be into the next phase. The guard is a check-then-act; the reflector's own
> write site re-checks done under iterMu (see Driver.reflectClaim), so the
> no-reflect-after-stop invariant holds by construction at the write, not only
> by interleaving luck.
>
> CURRENT_CLAIM is NOT written here: observe persists it synchronously so a
> branch cannot be lost behind a parked Heartbeat (CLA-708, review 1.1).

### run

> run is the renewer's loop: beat every interval for as long as the current
> claim is HELD (Claim.Held: a task held and not yet settled by the session
> itself), follow the claim as the session settles one task and claims the
> next, and exit on cancellation or stop().
>
> The BEAT stands down - permanently, for this renewer - on a not-wired plane,
> on plane.ErrRunLost (the run no longer holds the task, so the caller ends the
> session), or on heartbeatGiveUp consecutive failures. The TICK does not: it keeps firing
> for the rest of the Invoke so the presence beacon still goes out every
> interval (CLA-771). Two transports, two fates: the MCP heartbeat may be
> refusing (an older plane with no tool, a broken MCP transport) while the
> fleet-report POST is perfectly healthy, and a session that goes dark on the
> console for the rest of its life is exactly the symptom CLA-771 exists to
> fix. Standing the whole goroutine down here would bring it back.
>
> Held is the gate, not RunID != "": a settle keeps the run id on the tracked
> claim (the driver reads it after exit) and flips Settled instead, so a
> settle arrives as a snapshot whose RunID is unchanged. Keying the pause off
> RunID would make it unreachable and keep beating a lease the plane already
> released.

### A claim transition queued by observe() wins over the tick,

> A claim transition queued by observe() wins over the tick,
> every time. Without draining here, select's random pick can
> hand the tick to a loop whose `current` is still the pre-settle
> claim, and beat ONCE MORE on a lease the session already
> settled - the flake that made the pause test fail on CI. The
> settle is newer truth than the cadence the ticker carries.

### CLA-771

> CLA-771: the presence beacon rides THIS tick - the one cadence
> that still fires while a session runs. It goes out before the
> beat decision so a session that has not claimed yet (a
> first-phase session claims minutes into its stream) still shows
> its instance alive; a held claim's tick then beats as before.
> It also goes out BEFORE the stand-down guards below, so a beat
> that is disabled (or a claim that is settled) never takes the
> presence cadence with it.
> The beacon is fail-soft by contract (fleet.Reporter.Send drops a
> failed report and returns), so it cannot stop or delay renewal.
> The done-guard is the reflector's rule: a tick that loses the
> race with stop() must not revise a row the next phase is about
> to own.


## `internal/loop/invocation.go`

### invocationFor

> invocationFor builds the invocation for one PHASE: the target's invocation,
> carrying this phase's prompt and turn cap, with the previous phase's claim
> substituted into the prompt's placeholders.
>
> spawnDir is where this phase's sessions run, resolved once per phase by
> sessionDir — the task repo's checkout when one is known, else the target's
> workdir exactly as before. It is resolved by the CALLER rather than here so a
> retry ladder re-spawns into the SAME directory it started in (the task has not
> changed between attempts), and so a resolution failure fails the phase before
> anything is spawned.
>
> Unlike the directory, the session env is resolved per CALL, not once per
> phase: every attempt re-runs its fromCommand declarations fresh, so a token
> rotated mid-ladder still reaches the next attempt (CLA-462).
>
> The substitution is what lets a later phase RESUME a run rather than claim a
> task. A session cannot know its own run id — it never called claim_task — so
> the driver, which watched the earlier phase's stream go by, is the only thing
> that can tell it. That is also why the ids come from the observed claim and
> not from config: they are per-task facts, discovered at runtime.
>
> A placeholder with no claim to fill it is LEFT STANDING rather than blanked.
> A literal {{runId}} in the log is a misconfigured sequence announcing itself;
> an empty string is a session quietly deciding to claim fresh work instead.
>
> prBase is the branch this task's repo PRs target, resolved per phase by
> drainPhase (CLA-608), and fills {{prBase}} in EVERY phase: the other three
> placeholders are per-run facts only a claim can carry, while the implement
> brief — phase 1, claim-free at spawn time — is itself one of the carriers, in
> its resumed-work rule. Empty leaves the placeholder standing, like the rest.
>
> runner is the runner-claimed briefing, nil for the session-claim path: it
> swaps the shipped briefs for the ones that assume the runner already holds the
> claim, and names the branch a fresh claim has not recorded yet.
>
> baseSHA is the runner's prep merge-base when this spawn has no runner briefing
> to carry it (a parallel panel member), so the member's brief still diffs from
> the commit the runner computed rather than the origin/<prBase> fallback. A
> runner briefing's own baseSHA wins when there is one: the prep fact is the
> same value, and the briefing is what the context block reads.
>
> phases is the sequence as CAPTURED when the drain started, and supplies every
> positional/attribution fact here — the "PHASE i of N" total and the previous
> phase's harness/model. A RELOAD applied inside a wait swaps the live config
> under an in-flight sequence (CLA-664), and the sequence must keep its own
> shape and attribution until the next drain.

### The per-session runaway ceiling, resolved once per drain from the budg

> The per-session runaway ceiling, resolved once per drain from the budget:
> the operator's own dial, else 2x max_tokens, else the documented floor.
> 0 never reaches here — SessionTokenCeiling has no disabled state, because
> the whole point of CLA-343 is that nothing was able to stop the 285.9M
> session.
> Asked PER HARNESS since CLA-367: a token ceiling that lives in this
> harness's own per_harness block derives the detector exactly as the global
> dial does, so moving a ceiling into a block does not loosen it. And the
> harness asked about is THIS PHASE's (CLA-366), not the run's — the ceiling
> is compiled into the invocation that is about to spawn, so keying it on d.h
> would hand an opencode phase the ceiling derived from claude's block.

### The phase's tier, or the run-wide model when it names none

> The phase's tier, or the run-wide model when it names none. Reported when a
> tier was named and resolved to nothing, because that is a typo in the
> operator's map and the session otherwise runs on the default with nothing
> said — the quiet failure, since a tier is set precisely when the default is
> not what was wanted.
>
> "resolves to no model" rather than "the map does not define it": ModelForTier
> also says not-ok for a bucket that IS in the map and is mapped to blank or
> whitespace, and telling that operator their map lacks a bucket they can see
> sitting in it would send them looking in the wrong place — the more so
> because the whitespace is the thing they cannot see. Label(), not Name, so a
> phase carrying a custom prompt and no name is still identified.
>
> Resolved against the tier map of the harness THIS PHASE runs on, not the
> run's: a bucket name is policy and travels, the alias inside it is a
> provider's and does not. An opencode phase whose harness block names no
> models therefore runs on opencode's own configured model — which is where
> opencode's model lives — rather than on a claude alias its CLI would die on.

### A pinned Phase.Model would otherwise make the rule INERT

> A pinned Phase.Model would otherwise make the rule INERT:
> ModelForPhase resolves Model before Tier, so setting the tier
> alone leaves the session on the pinned alias while the log
> claims an escalation (CLA-379's whole point is to raise the
> review to the escalated tier's alias). The rule fires on a
> phase, not the operator's per-phase wish, so the tier wins and
> the pinned alias is dropped.

### CLA-782

> CLA-782: the ordered route list this phase may fall back through, with
> this run's marks applied. ModelForPhase (and therefore inv.Model, the spawn
> line and the brief placeholder) keeps naming the FIRST routable route, so a
> single-route config is unchanged; the full list is what the adapter switches
> between when a provider denies the free route mid-session.
>
> CLA-947: the same selection also drops a route inside one of its pricing
> windows. When that leaves NOTHING and the schedule says wait, the spawn is
> refused: the phase boundary holds until the window ends, and starting on
> the blocked route here would spend exactly what the schedule exists to
> avoid.

### The {{branch}} placeholder is filled from the recorded, VERIFIED hand

> The {{branch}} placeholder is filled from the recorded, VERIFIED hand-off
> (CLA-457): the checkpoint the driver ran against this claim already
> confirmed the branch is on the origin remote, so the review brief can
> name it instead of asserting an unverified phase-1 success. verifiedBranch
> reads the accepted reports first (the plane record of what THIS run
> recorded), falling back to the claim's own branch for a resumed-with-WIP
> predecessor.
>
> CLA-497: a checkpoint evidenced by the PLANE'S RECORD - the task left
> `ready` or declared a no-code delivery - names no branch, and the
> branch-shaped builtin brief would tell its successor the hand-off
> FAILED ("an empty branch field ... is a FAILED hand-off to report"),
> the exact opposite of the checkpoint the driver just recorded. Swap in
> the no-code brief for the BUILT-IN prompt only; an operator's custom
> prompt owns its own wording. The empty claim branch is the
> discriminator because drainPhase holds "claim.Branch is the verified
> branch, or empty when the evidence was branch-less" by construction.
>
> CLA-576: an UNVERIFIED checkpoint carries its recorded branch but no
> verification - the origin could not be read - so the branch-shaped
> builtin brief's "a branch the driver verified to exist on the origin
> remote" is the one claim that must not reach the successor. It gets
> the unverified variant instead, again for the built-in prompt only.
>
> The split adds a THIRD mode: a non-final review runs the review-only
> brief, whose variants must stay review-only (record, never fix or
> hand over). The mode is selected by the resolved prompt text, not by a
> flag, so a custom prompt is never rewritten into a shipped one.

### CLA-639 M5

> CLA-639 M5: a panel MEMBER writes the durable record and nothing else
> on the task, so its brief is the panel variant — same core, no outcome
> write. A branchless checkpoint has no panel variant and must not be
> given one: its record IS the legacy block (no live holder accepts a
> durable one), so the first member keeps the shipped no-code brief and
> the driver skips the members after it (see drainPhases).

### Every placeholder is filled in ONE pass (CLA-split review finding)

> Every placeholder is filled in ONE pass (CLA-split review finding). Three
> sequential replacers ran over each other's OUTPUT, so a claim-derived value
> that happened to contain another placeholder — a branch named
> `clanker/{{model}}`, which is a legal git ref — would be corrupted by the
> later pass. One replacer cannot rescan its own output, so a substituted
> value is final whatever it contains.
>
> The attribution/position values are config- and spawn-derived facts only the
> driver has: a session cannot reliably report its own model, and it cannot
> see the previous phase's at all. prevModel is the PREVIOUS phase's resolved
> model from the static config, which is exact for the shipped shapes: tier
> escalation applies only to a review phase, and the shipped consumer of
> {{prevModel}} is a review brief naming the IMPLEMENT phase.
> The positional/attribution values are facts of the sequence that was
> captured at drain start (CLA-664), not of the live config: a RELOAD landing
> mid-drain must not rewrite the "PHASE i of N" text of the sequence already
> in flight, nor attribute the previous phase to a shape it never ran.

### CLA-637

> CLA-637: the durable review record's identity. The role is the
> driver's assignment for THIS phase, and the tip is the origin commit
> the driver verified at the checkpoint that opened the phase — the two
> facts a session cannot see for itself. Filled for every phase rather
> than keyed on the prompt text: a handoff respawn's prompt is the
> continuation, not one of the briefs, and replacing an absent
> placeholder is a no-op either way. The tip is only filled when the
> checkpoint has one: the unverified and branchless variants carry no
> {{reviewedTip}} at all, and the verified variant is only ever chosen
> for a checkpoint whose verified branch check read a tip.

### invocationOn

> invocationOn builds the invocation for one target ON A NAMED HARNESS: the
> project's workdir and MCP config over that harness's own fields.
>
> The harness-shaped fields (config dir, MCP config, settings, model) come from
> config.SessionFor, which fills the run-wide values in for the top-level
> harness only — so a single-harness run resolves to exactly what it always did,
> and a phase on another harness gets that harness's block or nothing, never the
> top level's claude-shaped paths.
>
> The MCP config is the one field with two claimants, because it carries two
> facts at once: which PROJECT (its /mcp/<slug>) and which SCHEMA (the
> harness's). The project's per-harness file answers both and wins; the
> project's own file is the top-level harness's, so it only applies there; and
> the harness block's path is the single-project fallback. config.Validate
> refuses the combination that would silently resolve to the wrong project.
>
> The session env is composed HERE, at spawn time, fresh every call (CLA-462):
> the four-level env overlay with every fromCommand executed now, so a rotated
> token reaches this session without a daemon restart. An error — a failing,
> timed-out or empty-output command, an unreadable @path file — REFUSES the
> spawn: the caller reports it and no session starts, because a session without
> its declared env is the incident, not a degraded mode.

### The agent event channel (CLA-603)

> The agent event channel (CLA-603). The loop's top-level probe is a one-word
> liveness check, not a session doing work, so it never emits. A recovery
> probe inside an already-started session is different: it inherits this
> factory (opencode's resurrection sets Probe on a copy) and its tool calls
> are part of that session's activity, so they do emit. The destination is
> the project's run config dial, and the machine can only have opted out.

### sessionDir

> sessionDir resolves where a PHASE's sessions run: the checkout of the task
> they will work, not the multi-repo parent (CLA-437, agent-rule-scoping pieces
> 2 and 3). Which task that is depends on the phase. A RESUMED phase (prev
> carries a held claim) knows its exact task, so its repo is read straight off
> the plane with get_task. A FRESH phase has claimed nothing yet — the claim
> happens inside the session — so the driver peeks at next_task, the same
> queue-head read the session is about to make, and starts in that task's repo.
> The peek cannot race a session out of work (it holds nothing), and if another
> clanker does claim the peeked task first, the spawned session claims elsewhere
> and still works: the permission policy covers every declared repo regardless
> of cwd, which is what makes a best-effort cwd safe rather than load-bearing.
>
> Resolution then runs config.ResolveCheckout over what the target declared:
> explicit repos entries, then the workdir-is-the-checkout convention, then
> <workdir>/<name>. An identity that matches nothing fails the iteration with
> repo_not_found — starting in the parent instead is exactly the wrong-repo-
> reads and cwd-scoped-grant shape this exists to kill — while an empty result
> (nothing configured anywhere) keeps the legacy workdir behaviour byte for byte.
>
> Lookup failures are degradation, not failure: a plane blip costs the spawn its
> per-task directory (it falls through to the primary-repo chain), never the
> iteration — the session about to spawn needs the plane too, and it can resolve
> nothing either way.
>
> The same read yields the task's repo IDENTITY, returned alongside so the
> caller can key the pr_base override by it (CLA-608) without a second plane
> read; "" is the same best-effort empty the checkout resolution already
> tolerates, and means only "no override can match".

### checkoutFor

> checkoutFor resolves a repo identity to the local checkout this target runs
> the project in. It is sessionDir's resolution without the task read, for the
> pre-claim worktree check, which knows the identity from the offer already.
>
> With the per-project layout declared, the session must START inside the
> project root its permission scope is bounded to. ResolveCheckout prefers an
> explicit `repos` path, so a stale override pointing at a developer checkout
> would otherwise run the session outside the project and hand that path to the
> policy (CLA-678 review round 2, finding 3). The materialized config already
> rewrites the overrides; this is the driver-side refusal that holds even for a
> config that adopted the layout by hand.

### extraDirsFor

> extraDirsFor lists the directories a session's permission policy must cover
> BESIDES where it runs: every checkout the project declares, whatever directory
> the session starts in (agent-rule-scoping piece 2). This is the piece that
> makes a best-effort spawn cwd safe — a two-repo project whose session started
> in one repo can still reach the other.
>
> One implicit entry joins the declared ones: the conventional worktree area
> beside the spawn checkout (<checkout>-wt as its sibling, this fleet's
> documented layout). Task worktrees are separate git roots OUTSIDE every
> declared repo, and a session that can create one but not edit in it is worse
> than one never started. It is added only when the directory already exists —
> no speculation, no widening beyond what is on disk.
>
> With nothing declared and no worktree area found this returns nil, which leaves
> each adapter's policy exactly as it shipped before CLA-437.

### With the per-project layout declared, every grant must stay INSIDE the

> With the per-project layout declared, every grant must stay INSIDE the
> project root. The scope below is additive: declared checkouts are added
> before it, so a stale `repos` override pointing at a developer checkout
> would be granted outright even though the session is scoped to the
> project — the escape the layout exists to close (CLA-678 review round 2,
> finding 3). sessionDir refuses such a checkout as a spawn dir; this also
> keeps it out of the reach list, alongside the conventional worktree area.

### The PROJECT ROOT and its children (CLA-678), when the config declares

> The PROJECT ROOT and its children (CLA-678), when the config declares the
> per-project layout (`project_root`): a session starts in
> <project root>/<repo name>, and the rest of its project — the other repos,
> and the worktrees directory every task worktree lives in — sits beside it
> under the same root. Granting the children is what makes cross-repo work
> inside a project possible at all, and the project root itself is granted
> so `external_directory` — the gate that decides a path-taking tool's reach
> — is carved out for the same subtree rather than for one checkout.
>
> The flag is what keeps this off a developer's multi-repo parent: without
> it a session starting in one of `~/dev`'s ~150 trees would be handed every
> sibling it happens to have there. Nothing is granted on a guess.

### projectScope

> projectScope reports the PROJECT ROOT a session's reach is bounded to: the
> workdir, when the session starts strictly INSIDE it — the per-project layout,
> where the session runs in <project root>/<repo name> and the project's other
> repos and its worktrees sit beside that checkout. "" means there is no scope
> to widen, which is every legacy shape (the workdir is the checkout itself, or
> the session starts nowhere near it).


## `internal/loop/limitstop.go`

### LimitStopName

> LimitStopName is the marker a daemon leaves in its state dir when it ends on
> a Stop-class usage limit (budget or credits exhausted, no reset to wait for).
> First line is the reason, the route that stopped and the provider's message.
> The supervisor reads it to leave the child stopped instead of respawning it
> into the same wall; deleting the file is the operator's resume. See
> docs/limit-stop.md.

### limitStopMessage

> limitStopMessage is the provider's own message for the marker. It reads the
> adapter's diagnostic scope - stderr plus a terminal error event, never the
> agent's narration - and prefers the terminal error event's message, so a
> JSON envelope does not become the reason. Nothing an agent wrote can reach
> this: the adapter's Diagnostic is the boundary that keeps it out, the same
> boundary DetectLimit reads.

### terminalErrorMessage

> terminalErrorMessage picks the provider's own message out of the last JSON
> error event in an adapter's diagnostic text. opencode nests the provider's
> response under error.message (2.x) or error.data.message (the older APIError
> shape); an adapter that reports a whole event with the error as a plain
> string is read too. Anything else yields "" and the caller falls back to the
> diagnostic text.


## `internal/loop/loop.go`

### Package loop is the driver

> Package loop is the driver: a long-running daemon that gates each iteration on
> a cheap backlog read, spends a fresh harness session only when there is
> claimable work, and — crucially — stays alive and keeps polling when the queue
> is empty, so it reacts when questions are answered, items are promoted, or new
> work is filed. On a usage limit it pauses and polls for an early reset; on a
> transient blip it backs off and retries. All durable state lives in the backlog
> (over MCP), so a session killed mid-task is reclaimed by the next iteration.

### errZeroSpendLoop

> errZeroSpendLoop marks the one give-up the budget breaker could not have
> reached: a run of attempts that each ended before the harness reported any
> usage, so nothing fed the token or cost ceiling and the retry ladder would have
> gone on indefinitely (CLA-288).
>
> It is a distinct, wrapped sentinel rather than a plain string because the three
> ways a run can end early are three different things for an operator to do next:
> a budget stop means retune the ceiling, a wall-clock stop means the window was
> too short, and this one means the harness is not running - none of which is
> legible from "the run stopped".

### errUnclassifiedRetryLoop

> errUnclassifiedRetryLoop marks the other give-up no ceiling would have reached:
> a ladder of retries taken on a failure the adapter could not NAME, only judge
> by heuristic (harness.Adapter.IsUnclassifiedTransient).
>
> It is the sibling of errZeroSpendLoop and exists for the mirror-image reason.
> That one bounds attempts that spend NOTHING, so the budget breaker never sees
> them. This one bounds attempts that spend NORMALLY on a failure that will
> happen again: a deterministic post-usage crash satisfies the heuristic every
> time, and none of the ordinary dials stop it — max_retries and budget are off
> by default, and max_zero_spend_attempts is reset by the very usage the
> heuristic requires. Left unbounded it re-spawns a full paid session on the
> same task at the backoff ceiling, all night (CLA-381).
>
> Also distinct because the operator's next move is distinct: this one means the
> adapter has no pattern for what went wrong, so the fix is to read the
> diagnostic in the error and — if it is a real blip — teach the adapter's
> transient patterns about it.

### unclassifiedRetryBound

> unclassifiedRetryBound is how many retries one phase's ladder may take on
> heuristically-judged failures before it gives up.
>
> Deliberately a constant and not a config dial. The dials an operator sets are
> about how much they are willing to spend on failures the tool UNDERSTANDS;
> this bounds guesses, and the honest number of guesses is small at any budget.
> Two leaves room for a genuine one-off blip the patterns have not caught up
> with — the case the heuristic exists for — while a deterministic failure costs
> three paid sessions instead of a night's worth.

### perTaskDeadBound

> perTaskDeadBound is how many consecutive dead phases on the SAME task park it
> (CLA-396, raised from 2 by the 2026-08-20 operator decision). A task really
> can be cursed — a brief that reliably kills every session sent at it — but at
> 2 the budget fired on ordinary bad luck: at the observed 37.5% per-session
> death rate, two consecutive deaths is a 14% roll, and CLA-390 was parked on
> exactly that and had to be un-parked by hand. Four consecutive deaths is a
> ~2% roll — a signal rather than noise — while a task that dies once per drain
> and succeeds between (the retry re-claims the same task within the drain)
> never accumulates past one here, because the counter resets when a phase of
> the task advances (see the reset below).

### fleetDeadBound

> fleetDeadBound is how many consecutive dead phases ACROSS tasks pause the
> loop for that target and raise one project-level question (CLA-396). One
> more than the per-task bound, on purpose: a single cursed task parks at 4
> (its own counter fires first, and the drain breaks before any fifth death),
> so the fleet counter can only reach 5 across DIFFERENT tasks — the shape
> only a fleet-wide fault produces. A task that dies once looks identical to
> a healthy task hit by a provider outage, and only a run of deaths spanning
> tasks distinguishes the two; 5 consecutive deaths at the observed 37.5% rate
> is a ~0.7% roll, and it stays conservative after CLA-406 removes the
> retryable class from the numerator.

### startupFailBound

> startupFailBound is how many consecutive harness-startup failures on one
> target pause it and raise one project-level question (CLA-562). Mirrors
> fleetDeadBound on purpose: the dead-phase escalation (CLA-396) never fires
> for a session that dies at startup - that counter only sees sessions that
> started - so the startup-failure sideline path (CLA-507) was unbounded by
> design: 50 consecutive failures over 4 days on a bad reasoningEffort value,
> sidelining 2h and retrying, with nothing filed and nothing paused. Five is
> the same conservative signal as the fleet bound, and triage may move it.

### MCPConfigPaths

> MCPConfigPaths is this project's MCP config PER HARNESS, for a sequence
> whose phases are not all on one harness (CLA-366). MCPConfigPath above is
> the top-level harness's file; a phase on another harness needs the same
> project in that harness's schema, which is a different file. Empty on every
> single-harness run, and config.Validate refuses the combination that would
> need an entry here and lacks one.

### Repos

> Repos is this project's repo -> local-checkout map (CLA-437, agent-rule-
> scoping pieces 2 and 3), and PrimaryRepo the fallback for a task that
> carries no repo. Between them they decide where each session STARTS: the
> task's own repo resolved to a checkout, never blindly the workdir — so the
> session reads its repo's rule files instead of a sibling's, and a grant
> scoped per-cwd cannot wall off the tree the task needs. Resolution order,
> the loud repo_not_found failure and the nothing-configured legacy path are
> config.ResolveCheckout's; this field only carries what the operator
> declared. Both empty = the feature is off and every spawn behaves exactly
> as before.

### ProjectRoot

> ProjectRoot says this target's workdir is a FLEET PROJECT ROOT (CLA-678):
> a directory holding this project's clones and its worktrees directory,
> with each session's checkout inside it. It is the config's own
> `project_root` flag, carried here because the session, not the config, is
> what the permission policy is built for. See extraDirsFor for what it
> widens.

### Cfg

> Cfg is this target's EFFECTIVE config — the driver's base config with the
> project's stored run-config document overlaid — or nil when no document is
> in force, in which case every read falls through to d.cfg unchanged. It
> carries only policy the plane may say (the pipeline — prompt, phases,
> review_panel — plus harness/model/tiers/budget/escalation/backstops);
> wiring, env and machine paths stay local, so the overlay can never
> repoint a session at somebody else's dialect.
> Rebuilt between iterations when the poll's runConfigVersion moves — never
> mid-session.

### now

> now is the clock the scheduled-pause check reads (at the poll gate and at a
> phase boundary), the route-window filter (routableRoutes) and route-mark
> expiry, so a pricing window and a mark can both be driven from a fixed
> clock in tests. Nil falls back to time.Now, so a hand-built test Driver
> works. Three readers deliberately stay on the WALL clock: the back-off
> ladder in progress.go, loadRouteMarks (a persisted mark is an absolute
> instant written by a previous process, so it is loaded against real time)
> and routeRetryLabel (it renders an operator's clock). schedPaused is
> whether a window is active — kept so the enter and exit are logged once,
> not per poll.

### panelLaunch

> panelLaunch, when non-nil, is called with the launching member's reviewer
> role at a PARALLEL panel's launch boundary: the first moment that member's
> session launch has actually begun, immediately before the panel releases
> its successor. It is a test seam, and it exists because the launch ORDER
> cannot be observed soundly from inside a fake adapter: the adapter is
> entered one call after the boundary, so a goroutine descheduled in between
> lets a correctly ordered panel look out of order, which is exactly the
> assertion that flaked on CI (CLA-755 review 1.review-5.1). Recording here,
> before the release, is the order the panel guarantees.
>
> Nil in every production build — nothing but a test sets it — so it costs
> one nil check and changes nothing the panel does.

### panelRelease

> panelRelease, when non-nil, is called with the member's index each time a
> PARALLEL panel member releases its successor, before that successor is
> unblocked. panelLaunch pins the launch ORDER; this pins the release POINT,
> which is the guarantee itself: a member must release from its launch
> boundary (phaseRunOpts.onSessionLaunch), not from its worktree being
> ready. At member k's launch exactly members 0..k-1 have been released, so
> a panel that released at worktree-ready — where release_k always precedes
> launch_k — reports k+1 releases at that launch. Deterministic, no
> scheduling window (CLA-757).
>
> It is wired at the scheduler's started wrapper rather than at the panel's
> call site, so a regression that calls started() earlier still reports the
> release where it really happened. Nil in every production build, like
> panelLaunch: a test seam, no behaviour change.

### undeclared

> undeclared counts the sessions this run that ended still holding a task
> whose work they had already pushed — the phase did its job and then did not
> hand the task over (CLA-384). Nothing is lost: the branch is recorded and a
> takeover resumes. What is lost is the money, twice, because the next clanker
> pays to rediscover where the last one got to. Measured 2026-08-19 on v0.8.1,
> this was 3 of 4 review phases in one evening and $31.30 of that run's spend,
> and the only way to notice was diffing task statuses against the log by hand.
> So it is counted and the count is said out loud, per occurrence: a run that
> is silently repeating its own work should be visible without forensics.

### quietTokens

> Per-target no-progress state. `claimable > 0` is a claim that work is
> available, not that it can be DONE: a task can be claimable and still
> unworkable — gated on an unanswered question, needing a toolchain the
> harness may not run, blocked on something no session can resolve. The gate
> cannot tell, so it spawns, the session correctly declines, and the operator
> pays for the same report every cycle. One real run spent ten iterations
> re-writing the same two questions. These back a repeatedly fruitless target
> off instead.
>
> The accumulator is denominated in TOKENS, not sessions (CLA-343): the old
> count of 3 fruitless sessions cost ~78M tokens before it first fired (~26M
> per measured iteration), and one huge fruitless drain could repeat that
> spend without ever climbing the count. Tokens since last progress make the
> first sit-out land at the same ~80M while letting a single runaway drain
> trip it on its own.

### harnessFails

> Per-target harness-failure state (CLA-507). A drain that returns an ERROR
> failed while trying to run THIS target's session - a malformed opencode.json
> in its workdir, a missing checkout, a harness binary that will not start.
> That failure belongs to the project, not to the run: ending the run took
> every healthy sibling down with it on 2026-08-26 (three of four fleet
> daemons died on one project's bad config). So the target sits out on the
> same ladder the no-progress breaker shapes, escalating while it keeps
> failing and rejoining once it succeeds again, while the others drain on.
>
> The quiet breaker cannot stand in for this: its accumulator is denominated
> in TOKENS, and a session killed by a bad config reports ~0 of them, so a
> target failing this way would never climb it. harnessFails counts
> consecutive failed drains since the last success (reset only by success,
> so a target that keeps failing escalates: an idle stretch does NOT clear
> the sit-out (judgeProgress's idle reset preserves skipUntil while
> harnessFails > 0), so the ladder holds until the back-off elapses or the
> harness succeeds again); harnessErrs keeps each failure's text for the
> log line and for the run-wide report when nothing is left to drive.

### resumedTransients

> The resumed-phase transient streak (CLA-662), keyed by the target that
> failed. A resumed phase's transient failure ends its sequence, and every
> counter the dead/fleet machinery keeps is deliberately skipped on that
> path — so without this the run re-drains AT ONCE, and when the failing
> phase runs on a different harness from the one the queue drains on
> (implement on opencode, review on claude — the README's own example),
> the first-phase transient arm never sees the failure at all. The streak
> is the rung of the backoff the run owes after the sequence's handback,
> before polling again. Per target AND per phase harness: the failure is
> the provider's, so a claude streak must climb while opencode stays
> healthy, and an opencode success must not vouch for claude. Reset the
> moment a resumed phase on the same target+harness ends any other way
> than the transient sequence-end — only a run of consecutive transient
> ends is what the growing wait is there to bound.

### routeMarks

> routeMarks is this instance's sticky memory of model routes that hit a usage
> limit (CLA-782): route alias -> the instant the mark expires. A run moved
> off its free route stays on the paid one for later PHASES until the mark
> lifts, so the fleet does not flip between routes at every phase boundary;
> it goes back to free only when the free route's reset has passed.
>
> Persisted in the state dir (ROUTE_MARKS) so a restart keeps them; per
> instance, not per host (operator decision 10164e9e): a sibling clanker
> discovers the same limit on its own next request and switches mid-session.
> Guarded by routeMarksMu because the review panel runs its members
> concurrently and each may record a switch.

### fleetDead

> The fleet dead-phase counter (CLA-396). Per-target because a multi-target
> driver runs several projects, and "the fleet" is one project's sessions:
> a run of dead phases on project A must not pause project B. The per-task
> budget in drainPhases cannot see a provider failing for every task — each
> individual task dies only once, so no per-task counter ever trips — and
> these exist to catch exactly that: consecutive dead phases across
> DIFFERENT tasks mean the harness or provider is broken, not the tasks.
> The counter resets ONLY on a successful implement phase (see the reset in
> drainPhases), because review phases run on the claude harness and cannot
> exhibit the zero-usage death: a claude success vouching for the opencode
> gateway would hide the very fault the detector exists to find.

### startupFails

> Per-target harness-startup-failure state (CLA-562). A startup failure is
> a drain that returned an ERROR having reported no usage (tokens == 0):
> no session ever ran - a bad reasoningEffort value exiting 1 at spawn, a
> missing checkout, a harness binary that will not start - exit before any
> usage report. A dead phase (CLA-396) is the opposite: the session ran and
> produced nothing. The two counters are deliberately distinct and one
> failure never ticks both: a dead phase returns err == nil (handled inside
> drainPhases, never reaching the sideline path), and a startup failure
> returns err != nil with tokens == 0 (never satisfying end.dead, which
> requires a held claim). startupFails counts consecutive startup failures
> since the last drain that spawned anything (reset on any err == nil, and
> on any err with tokens > 0, which proves a session ran); the backoff /
> sideline (harnessFails) stays for the first startupFailBound-1 attempts
> and only the Nth escalates to a pause-and-raise, mirroring fleetTrip.

### limitBeaconInterval

> limitBeaconInterval is how often a paused wait re-beacons `limited`
> (CLA-728). The plane calls an instance dead once its last beacon is
> INSTANCE_STALE_AFTER_MS (15 minutes) old, and a limit wait can be hours,
> so the wait speaks at half that window — 5 minutes, since half is 7.5 and
> the task takes the shorter of the two. A field for the same reason as
> waitGrace: no test should sleep five minutes to watch a beacon repeat.

### probePRBase

> probePRBase reads the origin remote of the checkout in dir to resolve a
> repo's PR base when no pr_base override names one (CLA-608): whether
> refs/heads/<integration> exists on origin, and origin's default branch
> (its symbolic HEAD target). A field so tests can substitute one;
> production always runs git. Errors are NOT failures — the caller falls
> back to the configured integration_branch, exactly the text the briefs
> carried before this feature — so a probe that cannot run degrades to
> today's behaviour, never blocks a spawn.

### newAdapter

> newAdapter resolves a harness NAME to its adapter, for a sequence whose
> phases are not all on one harness (CLA-366). Production is harness.Get; a
> field so tests can hand out their own fakes per name.
>
> It is only consulted for a phase naming some OTHER harness — a phase on the
> run's own harness gets d.h, the adapter the caller constructed the Driver
> with. That is what keeps every existing test (which passes a fake and never
> names a harness on a phase) driving its fake rather than reaching for a real
> binary through the registry.

### stateMu

> stateMu serialises the run-scoped ledgers and maps below. They are written
> by whatever goroutine a session's result is processed on: with a review
> panel's members running concurrently (CLA-697) that is more than one at a
> time, and a Go map written from two goroutines is a data race, not a lost
> update. Every read-modify-write of tgtSpend, startRefusals, deadTally and
> panelFailStreak holds it; nothing that calls out to the plane or a harness
> may hold it.

### controlMu

> controlMu serialises control-marker CONSUMPTION and the RELOAD action
> (CLA-697 review finding 1.review-1.2). A parallel panel puts N members
> inside waitOrStop's loop at once, and reading a marker then removing it is
> not atomic: two members could both find RELOAD before either removed it
> and both would run applyReload, swapping d.cfg and invalidating the
> run-config overlays from two goroutines. Holding it across
> consume-then-act means the second consumer finds the marker already gone —
> exactly what a single-threaded loop would see.

### panelFailStreak

> panelFailStreak is how many consecutive TASKS each review-panel member has
> recorded nothing on (CLA-639 M4), keyed by the member's log label (the
> positional `review-N` when unnamed). Like deadTally it is the run's
> account: one flaky member is a blip, but the same member failing task
> after task is a fault in that member's harness, model or brief, and the
> warning it earns must not depend on the drain boundary. Cleared for a
> member the moment that member records. See panelMemberFailed /
> panelMemberRecorded.

### rcVersions

> Per-target run-config state (CLA-410). rcVersions holds the version each
> target's effective config was built from, -1 until the first fetch; the
> poll's runConfigVersion differing from it is the re-read signal, honoured
> exactly where STOP/RELOAD are: the ITERATION boundary (the poll only
> happens there), never mid-session. rcAttempt/rcAttemptVer rate-limit a
> FAILED refetch of one version to one try per backoff window (a new version
> always fetches), so a blip at ratify time retries without spamming.
> rcFails counts consecutive transient failures for rcAttemptVer, so the
> window widens exponentially (CLA-481) instead of fixed 30s spam; a
> permanent latch (ErrNoConfig/ErrNotWired) or any success resets it.

### resumedTransientKey

> resumedTransientKey names the target a resumed phase's transient failure is
> charged to (CLA-662): the project (its index in targets) and the harness the
> failed phase runs on. The harness is part of the key because the failure is
> the provider's, not the project's — the per-phase shape (implement on
> opencode, review on claude) drains a healthy opencode while claude blips,
> and a project-keyed streak would let an opencode success reset it.

### adapterFor

> adapterFor resolves the harness that runs one phase.
>
> The run's own harness resolves to d.h without consulting the registry, which
> is both cheaper and the thing that keeps a Driver built with a substituted
> adapter honest: a phase that names nothing, or names the configured harness,
> runs on exactly the adapter the caller handed over.
>
> An unresolvable name is an error rather than a fallback to d.h. config.Validate
> has already refused an unregistered harness, so reaching here means the
> registry and the config disagree, and quietly running the phase on the other
> harness would be the wrong half of a mixed sequence — the review phase spawning
> on the implement harness, with nothing in the log to say so.


## `internal/loop/maps_session.go`

### mapssession.go

> maps_session.go — the review phase's maps-session method (CLA-869).
>
> The method is the runner's, not the model's: the driver maps the change into
> items (CLA-866/CLA-868), sends them in chunks, and the SAME opencode session
> writes notes per item before a final findings turn turns those notes into the
> review record (CLA-864). The brief is unchanged — the maps section is appended
> to it — so the single-pass review is untouched and only a phase that names
> the method takes this path.

### mapsSessionRequested

> mapsSessionRequested reports whether this attempt is a review phase that asked
> for the maps-session method.
>
> The identity comes from the CAPTURED prompt: a handoff respawn replaces
> ph.Prompt with the predecessor's continuation, and that successor is not the
> phase that maps a diff — it continues work the predecessor already read.

### A blank Turn.Model means "inherit the session's current model", which

> A blank Turn.Model means "inherit the session's current model", which after
> the notes turns is the notes model. Name the findings model whenever that
> inheritance would land somewhere else — including when the phase names no
> model and only the notes stage did. There is no per-turn way back to a
> harness default inside a live session, so that one combination runs the
> findings turn on the notes model; the README says so.


## `internal/loop/panel_parallel.go`

### panelparallel.go

> panel_parallel.go — running a review panel's members concurrently (CLA-697).
>
> The panel (CLA-639) expands one review phase into N review-shaped phases.
> Until CLA-697 the phase loop ran them one after another, which made a round
> cost the SUM of its members' sessions. The members are independent by design
> (D5): the same diff, no fixes, and no member sees another's findings until the
> driver closes the round. Nothing about the review needs the ordering, so the
> fan-out is driver-level: this file spawns the contiguous run of panel-member
> phases at once, joins, and lets the ordinary phase loop continue at the fix.
>
> Two things make concurrency safe that are NOT this file's invention:
>
>   - the plane's heartbeat is an idempotent lease renewal — one UPDATE of
>     last_heartbeat_at and `heartbeat_count + 1`, with no version predicate
>     (plane: apps/web/src/repositories/runs.ts heartbeat), so N members
>     renewing ONE run id cannot corrupt each other; and
>   - a review record is keyed by (round, reviewerRole), and each member has a
>     distinct positional role, so concurrent appends land as distinct rows in
>     the same round (plane: apps/web/src/repositories/task-reviews.ts append).
>     The round number is derived from the rows already present (max + 1 when
>     none is sealed), which is deterministic for concurrent first writers; the
>     duplicate check is per role, which is exactly what makes two DIFFERENT
>     roles safe and a repeated role a retry.
>
> What IS this file's job is keeping the members from sharing a checkout. Each
> member gets a DETACHED worktree at the reviewed tip, created here and removed
> when the member ends. The worktree is throwaway by construction: a review-only
> session changes nothing, salvage is already disabled for a review-shaped
> phase, and a dirty member tree is logged and DISCARDED — never committed,
> never handed to the fix phase.
>
> With `review_panel_concurrency: 1` none of this runs: the phase loop's shipped
> sequential path executes, byte for byte, worktree and all.

### runPanelMembers

> runPanelMembers runs n work functions with at most `concurrency` in flight.
> A slot is acquired BEFORE the member's goroutine is spawned, and the loop
> waits for that member to call started() before spawning the next, so no
> member is spawned until its predecessor's work has begun: the panel's
> sessions begin in the config's list order. A finished member frees its slot
> and the next member in list order starts immediately — a backfill queue, not a
> wave barrier, so a short reviewer never waits for a long one in its batch.
>
> run MUST call started exactly once, as soon as its work has begun. A panel
> member's "work" is its SESSION LAUNCH, so it passes started down to the
> launch boundary (phaseRunOpts.onSessionLaunch) rather than calling it when its
> checkout exists (CLA-738 review 3.2); a member that never launches — setup
> failed, budget refused — calls it itself before returning. The test calls it
> at the top of its callback.
>
> onRelease, when non-nil, is called with the member's index at the moment that
> member releases its successor — from inside the started wrapper, before the
> successor is unblocked, on every path that ends the wait including the
> safety-net defer. It exists so the release POINT can be observed where a
> release actually is, not inferred from the panel's intentions: a regression
> that calls started() from somewhere earlier (worktree-ready) is reported
> there, which a probe wrapped around the panel's own call site could not see
> (CLA-757). Nil in production: it is a test seam, like panelLaunch.

### releaseMember

> releaseMember is the WHOLE of a member's release: it reports the release to
> the observer (onRelease, test-only), and only then unblocks the scheduler that
> spawns the successor. The order is the guarantee the release-point probe rests
> on (CLA-757): when member k+1's session launches, exactly members 0..k have
> been released. Keeping both steps in one function makes that order structural
> rather than two adjacent statements a later edit could silently swap: a swap
> would leave the panel fixture green, because the successor needs a
> `git worktree add` subprocess before it can reach its own launch and would win
> that race every time, turning the count observed at a launch into a race.
> TestReleaseFiresBeforeTheSuccessorIsUnblocked pins it without a schedule.

### runPanelInParallel

> runPanelInParallel runs phases[start:end] concurrently, each in its own
> detached worktree at the reviewed tip. ok=false means the fan-out could not be
> set up at all (no live claim, no repo, no resolvable tip) and the caller must
> run the shipped sequential path instead — a worktree failure that would only
> make one member fail does not refuse the panel.
>
> baseSHA is the start prep's merge-base, threaded into every member so its
> brief diffs from the same commit a single reviewer's would.

### The next member starts only once THIS member's session launch

> The next member starts only once THIS member's session launch
> has actually begun (CLA-738 review 3.2). Signalling when the
> worktree was ready woke the scheduler first: a later member
> whose setup happened to finish sooner could reach its adapter
> ahead of this one, so the config list order was not the
> adapter/start order the panel promises.
>
> The release is also the only point a test can see the launch
> order exactly, so d.panelLaunch fires here, BEFORE the release:
> the recorded sequence cannot then be reordered by the
> scheduling gap between the release and the adapter call, which
> is what made an Invoke-entry assertion race (CLA-755
> 1.review-5.1).

### CLA-784

> CLA-784: a CANCEL is the run's, not the member's, so it stops the run
> whoever the seat is. A budget stop is account-scoped (CLA-480): it
> ends this drain and exhausts the target, and the run continues with
> its siblings. A member's OWN Stop — credit exhaustion — is converted
> to a recorded failure in drainPhase before it reaches here, so an
> optional seat cannot end the run through this flag.

### createPanelWorktree

> createPanelWorktree makes the member's own DETACHED checkout at tip. The
> directory is unique and does not pre-exist: git itself refuses an existing
> path, so the driver cannot accidentally adopt a tree somebody else owns. It
> lives in the fleet's worktrees directory when that exists (the project-root
> layout), else beside the checkout's conventional `-wt` area, else the OS temp
> directory — each still inside a directory the session is allowed to use.


## `internal/loop/pause_boundary.go`

### holdThroughPause

> holdThroughPause holds a phase sequence at its next boundary while a scheduled
> pause window is active (CLA-906).
>
> A session already running is never interrupted: this runs only before the
> NEXT session spawns. The drain's claim is kept for the whole wait — the next
> phase resumes the same run — and its lease is renewed at the usual
> third-of-lease cadence, so the task is not re-offered as a takeover. Presence
> goes out on the same cadence whether or not the lease can still be renewed,
> to the held target and every sibling project the daemon serves, so a window
> hours long does not make the instance read dead.
>
> It reports how it ended, so the caller can take the ordinary stop path or the
> drain-level run-lost ending. A nil claim (a session-claimed drain that has not
> spawned yet) still waits: there is no lease to renew, but a window must not
> start a session either.

### The poll that normally carries presence does not run mid-drain, and th

> The poll that normally carries presence does not run mid-drain, and the
> phase's own renewer stopped when its session ended: without this a window
> hours long would make a healthy instance — and every sibling project this
> daemon serves — read as dead. The held target's row is a `draining` state
> carrying the schedule reason, the window's end and the held ref and phase,
> which is what the console's pause banner reads.


## `internal/loop/pausewindows.go`

### scheduledPause

> scheduledPause reports whether an operator-configured pause window is active
> (config.PauseWindows), logging the entry and the exit once each so a window
> hours long does not repeat the line on every poll.
>
> It is read BETWEEN iterations and at every phase boundary (CLA-906). A session
> already running is never interrupted; a window holds the NEXT spawn — the next
> phase, a handoff respawn, or a drain's first session — and holdThroughPause is
> what keeps the claim alive while that waits.

### pauseState

> pauseState is scheduledPause's read without its transition logging: whether a
> window is in force now, the instant it ends, and its schedule for a log line.
> A nil config or schedule is inactive.
>
> d.cfg is read under controlMu because a RELOAD swaps it on another goroutine (a
> panel member parked in waitResponsive), and since CLA-908 this is reachable
> from the lease renewer's presence lap as well as the loop. The returned
> schedule is immutable: a reload installs a new config rather than editing this
> one, so reading it after the unlock is safe.


## `internal/loop/phase_attempt.go`

### The breaker, from inside the drain

> The breaker, from inside the drain. Every path that loops back here has
> just waited — a supervised wait on a usage limit, an exponential backoff
> on a transient blip — and each of those waits used to be unbounded by
> anything but the wall clock, which is only one of three dials and the one
> an operator is least likely to have set. A drain that re-spawns a paid
> session on a loop is the expensive failure this stops, whatever put it
> there (CLA-258).
>
> The account is the TARGET's, not the run's (CLA-480): a stored overlay's
> budget is judged on its own project's spend, and a local-file budget on
> the projects it still governs. charge() books every counted attempt, so
> the ledger already carries this drain's spend without threading it down.

### The breaker's blind spot, bounded

> The breaker's blind spot, bounded. The check above can only stop spend it
> was told about; an attempt that died before its harness reported usage
> added nothing to either accumulator, so a ladder of those attempts leaves
> a token or cost ceiling permanently unreachable and only max_wall_clock -
> the dial an operator is least likely to have set as a spend bound - can
> end the run. Reached only on an attempt re-spawn (the first pass has
> counted nothing), so a single silent attempt that ends the phase some
> other way never trips it.

### Each attempt streams live to the terminal and to its own logfile

> Each attempt streams live to the terminal and to its own logfile. The name
> carries the drain number and attempt counter as well as the timestamp: two
> attempts in the same second (a sub-second backoff) would otherwise share a
> name, and a name already taken is REFUSED rather than truncated.
>
> The random tail is not decoration. This log carries the session's whole
> output — prompts, tool arguments, tool output, occasionally a token — and
> the rest of the name is derivable to within a second by the session that
> produced it. Unguessable names mean a session that CAN write to the state
> dir (an operator who pointed state_dir back inside the workdir) still
> cannot pre-plant a decoy at the name we are about to use. statedir.Create
> refuses an existing path either way; this stops it turning into a denial
> of the log itself.

### blocked

> CLA-947: every route is inside a pricing window. The phase boundary is
> where that normally holds the spawn (checkPhaseBoundary); reaching it
> here means the window opened between the boundary check and this spawn,
> so end the phase without spending. windowHeld marks it as a scheduling
> condition rather than a failed phase (review 1.review-1.6): the claim
> goes back through the ordinary no-checkpoint path and the next drain's
> boundary holds the spawn until the window ends.

### The session env refused to resolve (a failing or empty-output

> The session env refused to resolve (a failing or empty-output
> fromCommand, an unreadable @path file — the error names the
> variable). REFUSING THE SPAWN is the fail-closed contract: no
> session starts without its declared env (CLA-462). This is not a
> retryable failure — nothing was spawned and the cause is config,
> not a blip — so it fails the phase like a bad spawnDir does.

### echo

> The console is where the adapter streams the session: the file, and
> NOT the terminal unless the operator asked for the echo (CLA-701).
>
> The stream is the whole harness transcript — prompts, tool arguments,
> tool output — and once it also went to the daemon's stderr it left only
> the last few minutes of one session in the operator's scrollback, with
> none of the daemon's own lines beside it. Those lines (spawn, exit,
> classification, why the run ended) are the record of what the driver
> DECIDED, and nothing re-reads a terminal; the file keeps the stream in
> full, so echoing it only duplicates it over the top of the useful part.
>
> `echo_sessions: true` restores the old live echo for an operator
> watching one session. What stays live either way is the daemon's own
> logging (which also lands in daemon.log). Nothing harness-side is
> echoed separately: claude's stderr is part of its stream and lands in
> the iteration log with the rest of it, and a session worth watching
> live is why the opt-in exists.

### The spawn line stamps what the session was actually spawned WITH

> The spawn line stamps what the session was actually spawned WITH:
> phase and the resolved model id inv.Model carries (ModelForPhase's
> output, never the tier alias). A token/cost line in an old log is then
> attributable to the settings of THAT day rather than inferred from
> whatever the config file says now (CLA-345). An empty model= is itself
> evidence: no explicit model was configured at spawn time, and the
> harness ran its own default.

### CLA-358

> CLA-358: while THIS child runs, renew whatever task-lease it holds.
> Started before Invoke so a resumed phase's seeded claim renews from
> the first tick; stopped the moment Invoke returns, which is what makes
> renewal pause whenever no child process is running - between attempts,
> across supervised waits, after exit. Inside the window the adapter may
> run its own resurrection machinery (opencode CLA-406): those processes
> continue the SAME session on the SAME claim, so renewal spans them.
>
> The child runs on its own cancellable context so the renewer can end it
> the moment the plane says the run no longer holds the task. The renewer
> itself stays on the parent context: it still has its log line to write,
> and a cancelled parent is the operator stopping the whole run, not a lost
> claim.

### The orderly-cap classifications, computed here (before the salvage) so

> The orderly-cap classifications, computed here (before the salvage) so
> the salvage can tell whether a clean tree means "produced nothing" — a
> question the caps answer: a capped session is an orderly end with its
> own marker, not a silent death, and an untrusted stream's finish reason
> cannot be read at all. Purely adapter reads of `res`, so moving them up
> changes nothing else (review finding).

### CLA-739

> CLA-739: the adapter killed this session for making the same failing
> tool call over and over. That is a DEAD session - no delivery, whatever
> it pushed before it started looping - and it joins the dead signature
> everywhere below: the seam veto, the dead-phase ladder, the tally, the
> panel's failed-member path and the fleet record. Not a cap: a cap is an
> orderly end of a session doing its job, and this one had stopped.

### CLA-900

> CLA-900: a kill that landed with a tool call OPEN is the call's own cap
> ending a session that was waiting inside one call (a CI wait, a long test
> run), not the idle clock finding a provider that stopped answering. It is
> still a dead phase - no delivery came of it - and it still counts toward
> the fleet bound, so a fleet-wide stuck call cannot spin sessions forever;
> what changes is how the fleet question words it (openCallKill, trips.go).

### Hand back anything the session was still holding, BEFORE deciding what

> Hand back anything the session was still holding, BEFORE deciding what to
> do next — every branch below either waits, retries or returns, and all of
> them leave the lease unattended. Above the ierr check too: on every
> adapter, an Invoke whose run failed with something other than an exit
> status returns whatever the stream had announced by then, fully parsed,
> alongside the error (CLA-299) — claude's drain path parses its stream as
> it arrives; codex and opencode run their parsers to completion before
> classifying the failure — so a claim observed there is real and must not
> be dropped just because the process died untidily. A launch failure
> yields a zero Result, which releases nothing.
>
> UNLESS this phase reached its checkpoint with another phase still to run.
> Then the sequence is not over: the next phase resumes THIS run with
> heartbeat(runId) instead of claiming, so handing the lease back here
> would post the task to the queue while we are still working it, and the
> next phase would find someone else's task or none at all. drainPhases
> owns the handback in that case and performs it the moment the sequence
> ends for any other reason.
>
> Only an ORDERLY end holds it open, and each conjunct is load-bearing: a
> retry deliberately re-claims a half-done task with a fresh session
> (which is why an ordinary non-zero exit still releases), and an untrusted
> stream must not be read for claim state at all, because the settle we
> never saw may be in the bytes that never arrived (CLA-262).
>
> A turn-capped end CAN be orderly — but only if something durable came out
> of it. The salvage above is not a guarantee: it commits nothing on a
> clean worktree, and refuses outright on a diverged remote or a tree in
> the middle of a merge. A phase 1 that burned its turns reading and
> planning without writing therefore leaves no branch at all, and calling
> that a checkpoint spawns a session told "an earlier session has already
> implemented, committed and pushed" and pointed at a branch that is not
> there — a paid session that can only fail, or worse, move the task to
> review over no work.
>
> HasWIP is exactly that signal: it is set by the only two things that make
> a checkpoint real — the phase recording its own branch, or the salvage
> recording one for it and the PLANE accepting the record.
> A session the adapter ended on its wall-clock cap is the third member of
> the same family: an orderly cut-off mid-thought, whose survivability rests
> on the salvage exactly as a turn cap's does — so it earns a checkpoint on
> the same terms, HasWIP and all (CLA-368). Classified by the PHASE's adapter
> (`a`), never the run's: "did this session end on its own wall clock" is a
> question only the harness that RAN it can answer, and under per-phase
> harnesses (CLA-366) that harness is not necessarily d.h.
> CLA-700: the exit status, and the ONE exception it does not decide. A
> stream that shows the session took a provider error and then RECOVERED
> from it — retried and carried on — ends on something other than the
> error, and opencode 2.x exits non-zero anyway (pinned by the
> conformance suite). Judging that exit by its status is what stranded two
> delivered phases on 2026-10-19: the driver read the status as "failed
> attempt" and either retried (a resumed phase ending the sequence, its
> lease left to lapse into a takeover) or stopped, with the pushed work
> already on the origin remote and the task held mid-sequence.
>
> So a recovered exit is asked the DELIVERY question instead, through the
> same evidence gate an orderly checkpoint uses, read once below. It is
> deliberately narrow: it needs the adapter's own marker (a stream whose
> events include a recovered error and no terminal one), so a plain exit 1
> from a harness with no such marker — or from a session that really did
> stop on an error — keeps every shipped classification. A recovered exit
> is eligible for a checkpoint on the evidence alone; when the evidence
> says it delivered nothing, it is the existing held-but-empty shape
> (retry once, then hand the task back), never a run-ending stop over an
> exit status the harness itself explains.
>
> CLA-739: a tool-loop kill is none of these. The process usually reads
> exit -1 from the kill, but the session can exit on its own in the
> instant before the kill lands, and a zero exit must not turn a looping
> session into a checkpoint - nor can its stream's recovered provider
> errors (the incident's own log carries one).

### CLA-386

> CLA-386: a session whose final step finished with reason "unknown" and
> left no branch recorded produced NOTHING — a dead phase, not a completed
> one. It must not earn a checkpoint even on a zero exit, because a
> checkpoint is what hands the next phase a brief that claims "an earlier
> session has already implemented, committed and pushed". Two real
> implement phases died exactly this way and the seam advanced to review
> on the false premise; the signal has to veto the checkpoint.
>
> A capped session is excluded even when its stream carries "unknown": the
> wall-clock kill can land mid-turn, but a cap is an orderly end with its
> own marker — retrying it would re-spend against the same cap and parking
> it would read a doing-its-job backstop as a silent death.
> CLA-877: a report-resume failure is a resume-only failure and rides the same
> retry ladder, but under a different claim disposition: the session's own
> run is not what failed, so the claim is KEPT and the retry resumes it
> rather than settling, expiring the lease and re-claiming.

### CLA-402

> CLA-402: book this session into the run's dead-phase tally, per phase
> label and harness, whatever the seam decided about retrying it. The
> tally's dead classification deliberately drops the `!last` conjunct
> that guards the checkpoint veto — a dead LAST phase is still a dead
> phase, and the rate is a measurement, not a seam decision (see
> deadtally.go). The report rides the end (CLA-639): the panel's member
> handler books a failed member into the same tally, and must not count
> an exit this call already booked.

### CLA-630

> CLA-630: the session this pass actually spawned, in exactly the words
> the spawn line above used — the phase's label, the phase's resolved
> harness, and the resolved model (empty when the harness default
> applied; never guessed). Set on every attempt, so the LAST attempt's
> values are the ones that ride: the entry describes the PASS, and a
> retry within the ladder cannot change what the pass ran — only a
> reload landing mid-ladder can.
>
> CLA-691: its spend is filled in later, at the point the loop counts the
> attempt's spend toward the iteration (recordPassSpend), so the two can
> never disagree about which attempts were counted. The pass sums every
> counted attempt, because the loop counts each one as its own paid
> session: an entry reporting only the surviving attempt's money would
> not add up to the iteration total.

### CLA-638

> CLA-638: a verification round does not chain via a handoff. Its record
> is the deliverable, and the DRIVER decides what follows (another fix, a
> park, or nothing) from the open MUSTs the record releases; a successor
> prompt here would have nothing to continue that this round has not
> already written, and honoring it would defer the close/evaluation the
> driver owes the round.


## `internal/loop/phase_end.go`

### failed

> failed maps a member's role to the reason it recorded nothing, so the fix
> brief names the incomplete members AND what happened to them, not only
> that the list is short. The entry also carries whether the seat was a
> TRIAL seat (CLA-784), so the fix brief can mark it `(optional)` exactly as
> the daemon log does — a fixer must not read a lost trial seat as a lost
> required reviewer.

### recorded

> recorded reports that a REQUIRED member has already recorded in this
> panel (CLA-784). The last panel member's seam closes the round; an
> OPTIONAL seat may only trigger that close when a required member has
> actually recorded, because a round where only trial seats recorded is no
> review at all and must stay sealed — the same path as a panel where none
> recorded.

### phaseEnd

> phaseEnd is how a phase reports what it left behind, so drainPhases can decide
> whether to run the next one and what is still owed a handback.
>
> The three fields are deliberately independent. A phase can end holding a claim
> without it being a checkpoint (it was the last one), and can observe claim
> state without owing anything (it settled the task, or already handed it back).

### dead

> dead reports the CLA-386 dead-phase signature: the session's final step
> finished with reason "unknown" and no branch was recorded on the task, so
> the phase produced nothing. It is a FAILED phase, not a completed one —
> the seam must not hand it on to the next phase's brief. The claim has
> already been released (a retry re-claims) EXCEPT for a report-resume failure,
> which kept it (reportResumeFailed); drainPhases owns the retry-once-then-
> park decision and its per-task budget.

### producedNothing

> producedNothing is the WIDER dead classification (the tally's, CLA-402):
> the same signature WITHOUT the !last conjunct end.dead carries, because a
> dead LAST phase is still a death even though no retry/park machinery can
> apply to it. Only the fleet record reads this field (CLA-466) — the
> iteration record must say `dead` when the drain ended on exactly such a
> final-phase death — so it deliberately does not feed the counters or the
> seam, whose !last gating is pinned by its own tests.

### deadBooked

> deadBooked reports that the run's dead-phase tally (CLA-402) already
> counted THIS exit as a dead phase: either tallyDead's own predicate fired
> or the seam's held-but-empty branch booked it (tallyEmpty). The review
> panel's member handler books a failed member into the same tally (CLA-639
> M4), and this is how it avoids counting one exit twice — see
> notePanelMemberFailure's caller.

### empty

> empty reports a held-but-empty exit — the 2026-08-24 fleet-incident shape:
> the phase ended cleanly still holding the task with NO durable,
> origin-reachable branch evidence on the plane record. It is a FAILED phase,
> not a checkpoint: the seam must not hand it to the review brief. The claim
> has already been released (a fresh session re-claims) or left to expire (a
> recorded-but-unreachable branch keeps its takeover hand-off); drainPhases
> owns the one-bounded retry. Deliberately distinct from dead, which keys on
> the "unknown" finish reason — the incident's sessions finished with reason
> "stop", so the dead signature alone did not catch them. Set on a HANDOFF
> too, so a refused handoff is re-judged as the bare exit it really is.

### windowHeld

> windowHeld reports that the spawn was REFUSED by a pricing window that
> opened between the boundary check and the spawn (CLA-947), so no session
> ran and nothing was spent. It is a scheduling condition, not a failed
> phase: the walk ends here, the claim goes back through the ordinary
> no-checkpoint handback, and the next drain's boundary holds the spawn
> until the window ends. Deliberately NOT empty, which is the evidence-gate
> FAILED shape (the 2026-08-24 incident): a schedule must never be counted
> as a harness failure.

### branch

> branch is the branch name when this phase checkpointed — the
> plane-record evidence the checkpoint is gated on (CLA-457), carried here
> so the successor's brief can name it instead of asserting an unverified
> phase-1 success. Folded onto the carried claim by the seam, which marks
> it UNVERIFIED (CLA-576) when the origin could not be read; the brief
> selection reads that mark, so the branch reaches the successor named but
> never presented as verified.

### handoff

> handoff is the successor prompt the session emitted in its final message's
> handoff block, already past the parse-time guards (marker present, prompt
> non-empty and under the size cap, clean exit, trusted stream, task still
> held — see detectHandoff). Non-empty implies checkpoint: the lease is
> being held for the successor. drainPhases owns the remaining guards — the
> chain cap, max-iterations, the budget breaker — and either respawns on it
> or falls back to the standard path (CLA-352).

### noMCP

> noMCP reports that this pass ended with NO review record and NO successful
> clankerbar MCP call at all (CLA-703). That is an infrastructure miss, not
> a reviewer's verdict: the tools the brief tells it to use were not there.
> The panel handler names it distinctly and keeps it off the member's
> failure streak; drainPhases books it apart from dead/capped so analytics
> can exclude it. It is set only on the FINAL attempt — the one bounded
> retry the same pass already took is not reported as the member's outcome.

### toolLoop

> toolLoop is the cause when the adapter killed this pass's session for
> repeating a failing tool call (CLA-739), "" otherwise. It is also
> `idle: <IdleKilled>` when the adapter killed the session for silence. Both
> are driver kills: the pass is also dead (when !last and held), and this
> carries the NAME of the death, so the panel handler records the member as
> failed with the cause rather than with "died producing nothing".

### reportResumeFailed

> reportResumeFailed is the cause when the pass's ONLY failure was the
> adapter's bounded re-emission of a refused done report: it stalled on a
> silent provider, or errored before producing a reply (CLA-877). The
> session's own run ended as it did — usually cleanly, with its work pushed.
> For the phase the walk started at, the retry then runs inside the SAME
> claim: settling without a checkpoint must not hand the claim back, and
> drainPhases must not end the lease and re-claim it. Empty for every other
> end.

### reviewCloseFailed

> reviewCloseFailed reports that this pass's review seam gate passed far
> enough to find a SEALED round, but the driver's own close of that round
> failed. The record exists (the reviewer did record), so the panel must not
> name the member INCOMPLETE or charge its failure streak; the drain must
> end without spawning the fix, because the fix would run against findings
> the plane still holds sealed. It is the close-failure half of
> reviewRecorded, kept separate because the evidence gate sets both to the
> same value everywhere else.

### reviewRound

> reviewRound is the review round the seam gate MATCHED this pass (CLA-637),
> zero when the phase is not review-shaped or no record matched. The round is
> normally closed inside drainPhase the moment the stage is over — but a
> DETECTED handoff defers that close to the successor's seam, and the
> remaining handoff guards (chain cap, max-iterations) live in drainPhases:
> when they REFUSE the respawn, the stage is over after all, and drainPhases
> closes this round before advancing — otherwise the fix phase would spawn
> against findings the plane still holds sealed, which it can neither read
> nor answer (finding_sealed).

### sequenceEnd

> sequenceEnd, when non-nil, reports that this RESUMED phase ended the
> sequence without completing it, and the RUN must not end with it. Two
> arms produce it: a ROLLING usage limit (CLA-631) and a TRANSIENT failure
> (CLA-651). In both cases the sequence ends exactly as the old stop did —
> the claim is not handed back to the queue, so the lease lapses into a
> takeover with its branch recorded — but drainPhases returns to Run, which
> carries on polling. Both arms owe a wait, taken after the sequence's
> deferred handback with no lease held: a set limitWait is the reset the
> run waits out (CLA-631); a nil one is a transient blip, and the run waits
> the target's growing backoff rung instead (CLA-662) — because with the
> resumed phase on a DIFFERENT harness from the one that claims the queue
> (implement opencode, review claude), there is no first-phase failure for
> the in-drain ladder to see. Nil on every other path, including a hard
> stop (lim.Stop), which still stops the run in drainPhase.

### deadPhase

> deadPhase reports the CLA-386 dead-phase signature: the session's final step
> finished with reason "unknown" — opencode's marker for a session that died
> without producing a final answer — AND no branch is recorded on the task, so
> nothing durable came out of it. The two conjuncts together are what "produced
> nothing" means: the reason names the death, the missing branch rules out the
> one thing that would have made the phase survivable anyway. A session that
> pushed work and THEN died on an unknown reason has still produced something,
> and its phase keeps its checkpoint.
>
> It reads the reason off the adapter's Raw map rather than asking the adapter:
> the key is stable across the harnesses that observe it, and no Adapter method
> needs to grow for a signal only opencode emits today.


## `internal/loop/phase_exit.go`

### Count this attempt's spend before giving up on it

> Count this attempt's spend before giving up on it. A non-exit run
> failure is either a launch failure (nothing emitted, nothing parsed —
> adding zeroes changes nothing) or a session that ran, announced its
> spend and died in Wait (CLA-299): the Result was parsed precisely so
> the budget would not lose what the attempt actually cost. That holds
> only while the stream was read whole, which is what the guard below
> makes binding: a failure whose stream ALSO came back untrusted
> carries a floor, not a total, and gets the same refusal as the
> untrusted branch below and endUntrustedDrain (CLA-262) - counted
> nowhere, so no breaker ever acts on a figure that cannot be known.

### CLA-703

> CLA-703: an MCP preflight failure is infrastructure, not a verdict
> on the phase. The session never reached its first turn, so the
> claim it was seeded with is untouched and the same run can be
> retried. One retry — the same bound the no-record backstop uses —
> then the run fails loudly with the reason rather than producing a
> silent empty phase.

### held is nil here by construction

> held is nil here by construction — an untrusted stream never holds a
> phase open — so the sequence ends and nothing is carried forward.
>
> CLA-691: no spend figures were recorded on this pass's session entry
> either (recordPassSpend runs only where the loop counts an attempt),
> so the record says "unknown" instead of publishing a FLOOR from a
> stream with a hole in it as if it were a report.

### Count THIS attempt's spend toward the budget breaker regardless of how

> Count THIS attempt's spend toward the budget breaker regardless of how it
> ends — usage-limit, transient, stop, or clean. A failed/retried attempt
> still burned tokens, and a "leave headroom" breaker must err toward seeing
> real spend, not under-counting it. Each attempt is a distinct session, so
> summing per attempt (and returning the accumulator, not the final res)
> counts every session exactly once.

### CLA-784

> CLA-784: an OPTIONAL panel seat is a trial reviewer. A hard
> stop on its account is recorded against that seat — never the
> run. Today's stop path would end the daemon for a reviewer we
> were only evaluating, throwing away the required members' round
> with it. The optional member's failure rides the usual
> per-member bookkeeping: named INCOMPLETE with this reason, and
> the panel moves on.

### A RESUMED phase must not wait one out (CLA-631)

> A RESUMED phase must not wait one out (CLA-631). The seam's handback
> has already ended the claim: `release` puts a no-work claim straight
> back in the queue, and `expireLease` ends a pushed-work claim's lease
> immediately, leaving the task an immediate takeover — so a supervised
> wait of hours would only run against a task this sequence no longer
> holds, with a runId in its prompt that no longer means anything. End
> the sequence instead. The RUN, though, must not end with it — exiting
> the daemon left a whole fleet idle until an operator noticed — so the
> wait is handed to drainPhases, which owes it only once the sequence's
> handback has run and no lease is held.

### Waiting out a reset that lands past the wall-clock ceiling buys

> Waiting out a reset that lands past the wall-clock ceiling buys
> nothing: the budget check runs between drains, so the loop would
> sleep through the window, run one session on the freshly reset quota
> and stop on the very next check — having spent the night waiting for
> headroom it then declines to use. Stop now and say when the quota
> returns, so the operator can start a fresh run against it.

### The wait's own probe spend lands in the SAME account as the sessions'

> The wait's own probe spend lands in the SAME account as the sessions'
> (charge, inside the wait), so it reaches the breaker at the top of this
> loop, the between-iterations sweep, and the iteration's cost line —
> rather than a separate figure nothing is measured against (CLA-287).
> Probed on the harness that hit the limit, in that harness's own
> session shape: a usage limit is a property of one provider's account,
> so probing the run's default harness would answer a question nobody
> asked, and spend on it.

### Below the limit branch, so a usage-limit attempt is counted NEITHER wa

> Below the limit branch, so a usage-limit attempt is counted NEITHER way.
> A session the subscription cap turned away often reports nothing at all
> - under claude the notice can arrive on stderr with no stream, and under
> codex as free text - so counting it as silence would end the run on the
> ordinary overnight quota wait, and blame a harness that is starting
> perfectly well. It is not a reset either: the cap is a known cause with
> its own breakers (the budget check inside supervisedWait, waitPastBudget,
> and the probes' own spend), and clearing the count on it would let a
> limit between silences hide the loop this bounds.
>
> Everything from here down is an attempt that RAN and ended on its own
> terms, which is where "did it tell us anything?" is the right question -
> a different question from what it told us, since a report of zero is
> still a report (CLA-288). Counted here, enforced at the top of the loop,
> so the bound fires on the decision to spawn again rather than on this
> attempt's exit.

### CLA-739

> CLA-739: killed for repeating a failing tool call. Ahead of the exit
> status, because the session can exit 0 in the instant before the kill
> lands. Never retried here as a transient (a fresh session would loop
> the same way, and each call re-sends the whole context), and never a
> run failure (the kill did its job). The dead classification above has
> already decided what follows: drainPhases' dead-phase ladder, or the
> panel's failed-member path.

### A clean exit is the shape the CLA-398 quiet death wears, so the

> A clean exit is the shape the CLA-398 quiet death wears, so the
> marker gets named here rather than inferred downstream: a final
> step_finish with reason "unknown" and all-zero usage used to read
> as "iteration done (tokens=0 cost=$0.00)" — indistinguishable from
> a cheap clean run, which is exactly what made these deaths
> invisible. The adapter's own marker is the only thing that can
> tell them apart.

### CLA-700

> CLA-700: a session that recovered from its provider errors is not judged
> by its exit status — the stream shows the harness took the error, retried
> and carried the session on, and 2.x exits non-zero regardless (pinned by
> the conformance suite). Everything this exit could have meant was settled
> above by the DELIVERY question: either the phase checkpoints (its work is
> verified on the origin remote, or its review record appended) and the
> sequence advances, or it delivered nothing and ends the drain as the
> held-but-empty shape. Falling through to the classifiers here would undo
> both — the transient ladder retries a session whose work is already
> pushed, and the non-retryable stop ends the run over an exit status that
> names no failure.
>
> Deliberately HERE, after the untrusted and spend-accounting arms and
> after DetectLimit: a usage limit or budget stop is still honoured (its
> scan reads stderr and a TERMINAL error event, so a recovered blip cannot
> trip it while a genuine account stop still can), and the cap branches
> below are unreachable from here by construction — a capped exit is
> `orderlyExit`, so it never sets recoveredExit.

### A session the ADAPTER killed for crossing its per-session token ceilin

> A session the ADAPTER killed for crossing its per-session token ceiling
> is the same shape as a turn cap: an orderly end with the salvage's
> problem to handle, never a failure and never a retry. Retrying would
> re-spend against the same runaway ceiling; failing would stop the run
> over a kill that did its job (CLA-343).
>
> The cost is reported as unmeasured, never as a number: the result event
> — the only carrier of total_cost_usd — is deliberately not parsed on a
> killed stream, so res.CostUSD is 0 and printing it would under-report
> the drain's spend as exactly nothing. The tokens are the honest figure.

### A session the ADAPTER ended for outliving its wall-clock cap is the sa

> A session the ADAPTER ended for outliving its wall-clock cap is the same
> shape again: the phase ends, and neither a retry nor a failure is right -
> a retry would spend the same hours over again and reach the same deadline,
> and failing would stop the run over a cap doing its job (CLA-368).
>
> Spend IS reported here, unlike the token-ceiling branch: opencode's usage
> arrives per step_finish and is summed all the way to the kill, so the
> figures are the honest cost of the session up to the moment it ended.
>
> Since CLA-365 opencode has BOTH HonoursSessionWallClock and
> TracksClaims (Capabilities.TracksClaims: true), so a capped opencode
> session holding its claim IS salvaged - the res.Claim.Held() arm above
> is the one that fires today. The else branch (nothing salvaged) applies
> only if an adapter enforcing this cap ever lacks claim observation
> again; it is kept defensive, not descriptive of the present. The claim-held
> wording was earned by CLA-365, not reserved for a future adapter.

### A RESUMED phase does not retry, for the same reason it does not wait

> A RESUMED phase does not retry, for the same reason it does not wait
> out a usage limit — and this path is the more dangerous of the two,
> because MaxRetries defaults to 0, meaning never give up. Seeding the
> resumed claim made the seam's handback live for phase 2, so a blip
> here hands the task back mid-sequence at once: `release` for a claim
> with nothing pushed, `expireLease` for pushed work, which ends that
> claim's lease immediately and leaves the task an immediate takeover.
> A retry would then be handed the very same heartbeat(runId) brief
> against a run that no longer holds anything. End the sequence. The
> RUN, though, must not end with it (CLA-651) — exiting the daemon on
> one provider blip left the fleet idle until an operator noticed, the
> same failure CLA-631 fixed for limits — so the sequence-end signal
> goes to drainPhases, which owes it a wait (CLA-662): there is no
> reset for a blip, so the run waits the target's growing backoff rung
> after the handback and then polls again. The rung matters precisely
> when this phase's harness is NOT the one the queue drains on: an
> opencode implement start can never see the claude blip, so without
> the run-level wait the same implement-and-lose loop would repeat with
> nothing bounding it.
> CLA-784: like a usage limit, an OPTIONAL seat's transient end is
> its own result — never the sequence's, resumed or not.

### A retry the adapter could not NAME is bounded on its own, because

> A retry the adapter could not NAME is bounded on its own, because
> nothing else bounds it: the dials are off by default and the
> zero-spend counter is reset by the usage this heuristic requires. The
> diagnostic goes in the error because it is the whole remedy - the
> adapter has no pattern for this text, so an operator who can see it
> can say whether one should be added (CLA-381).


## `internal/loop/phase_run.go`

### mcpRetried

> CLA-703: the ONE retry a phase gets for an infrastructure miss. TWO arms
> spend this single budget, deliberately: an adapter preflight failure
> (ErrMCPNotConnected, any phase) and a review-only session that made no
> successful clankerbar MCP call and wrote no review record (the no_mcp
> backstop). They are not independent one-retry lanes — a preflight failure
> followed by a no-record end closes the round INCOMPLETE with no second
> backstop. Deliberately a separate counter from `retries`: this is not the
> transient/limit ladder, and its bound is one regardless of max_retries.

### noUsage

> Consecutive attempts that ended without the harness reporting any usage -
> the spend the budget breaker above can never see (CLA-288).
>
> Deliberately scoped to THIS phase's attempt ladder, which is where the
> unbounded loop lives: every re-spawn of an attempt comes back through the
> top of this loop, and it is the ladder - not the phase sequence - that has
> no other bound under `max_retries: 0`. A phased run therefore allows the
> bound per phase, and a handoff respawn (which needs a clean exit and a held
> claim, so a silently dying attempt cannot produce one) starts a fresh count
> under drainPhases' own chain cap.

### unclassified

> Retries this ladder has taken on a failure the adapter judged retryable
> WITHOUT recognising it (harness.Adapter.IsUnclassifiedTransient) - see
> errUnclassifiedRetryLoop. Scoped per phase like noUsage, and for the same
> reason: the ladder is what has no other bound.
>
> Never reset. A classified retry in between does not make the next guess any
> better informed, and resetting would let a failure that alternates between a
> recognised blip and an unrecognised one loop forever - which is the exact
> shape of a provider degrading: real 5xx blips interleaved with stream drops
> nothing has a pattern for.


## `internal/loop/phase_seam.go`

### The verification seam runs even on the LAST phase

> The verification seam runs even on the LAST phase. Unlike every
> other terminal phase its evidence is not a checkpoint (nothing
> follows to resume) but the durable record plus the open-MUST set:
> that set is what decides whether ANOTHER fix is spawned, whether
> the sequence is done, or whether the round cap parks the task
> (CLA-638, B12).

### CLA-638 × CLA-670

> CLA-638 × CLA-670: the checkpoint rested on the legacy `## Review`
> block (the branchless arm of the gate), so there is no durable
> round to release and no plane open-MUST set to judge — the block
> is this shape's record. Read the task back anyway, for the status
> the hand-over log needs: a branchless verifier that recorded its
> pass but left the task in_progress must be named loudly, exactly
> as the id-based shape names it. A failed read ends the sequence
> fail-closed, like every other unreadable seam — so it retries
> like them too (CLA-686 D3).

### CLA-637

> CLA-637: the review phase's durable record is SEALED — the plane
> withholds its findings from every reader until the driver closes
> the round. The close is the driver's, made when the review stage
> is OVER: not while the session hands off to a successor reviewer
> (the successor continues this round), and not while the next
> configured phase is another review-shaped one (a panel member,
> whose independence depends on not reading the earlier member's
> findings). A close that fails REFUSES the checkpoint, fail-closed:
> the alternative is a fix phase spawned against findings the plane
> still has sealed, which can only park the task.
>
> CLA-639: for a panel, the stage is the whole panel. Every member
> but the last leaves the round sealed here; the last member's close
> is what releases them all to the fix phase, and it happens in the
> SAME condition below because nextReviewShaped is false only for
> the last member (the panel's last phase, when no fix follows).
> CLA-784: an OPTIONAL seat's record alone must not release the
> round. If no required member has recorded, this trial seat's
> record is kept sealed — the panel is treated as one where nobody
> recorded, so no fix runs against a round that is no review.

### A held-but-empty exit (2026-08-24)

> A held-but-empty exit (2026-08-24): a would-be checkpoint refused by the
> evidence gate — not by any of the older vetoes (dead, untrusted, not
> checkpointable) — so this is a FAILED phase, not a completed one. It is
> carried on the result in BOTH branches on purpose: for a bare exit it is
> the classification of record, and for a handoff it is what lets
> drainPhases downgrade the exit when the handoff is refused. An honored
> handoff never reaches that downgrade — the phase continues first.
>
> CLA-639: a panel member is not special-cased here. A member whose
> review record did not pass the gate is a FAILED member, and the panel
> handler in drainPhases is what records it as failed and keeps the round
> going; the flag is shared with the shipped single-review path, which
> still refuses to advance on it.

### CLA-703

> CLA-703: did this review-only session ever reach the backlog? A clean
> end with no successful clankerbar call and no review record is the
> missing-MCP shape — the tools the brief tells it to use were not
> offered, so "recorded nothing" is not a reviewer's verdict.
>
> The dead signature is EXCLUDED: a silent death is already classified
> as dead and retried by the dead-phase ladder, and relabelling it no_mcp
> here would hide a real death behind an infrastructure excuse. A
> non-zero exit is excluded for the same reason — the transient/
> non-retryable classification owns it. Caps and untrusted streams are
> excluded because neither can be read as "the tools were missing".
>
> A runner-claimed phase is excluded too: its session is TOLD to write
> nothing, so no successful clankerbar call is its correct shape. Its
> missing record is a failed report, never a missing MCP server.

### The invariant "the carried claim's branch is the VERIFIED one"

> The invariant "the carried claim's branch is the VERIFIED one"
> is held by construction, whatever form the evidence took
> (CLA-497): the branch form folds the verified branch on, and
> the plane-record forms clear whatever the session recorded -
> a recorded branch that FAILED origin verification must not
> ride into the successor's brief, which names {{branch}} as
> verified on the origin remote (CLA-457), and the no-code
> brief is selected exactly on an empty claim branch.
>
> CLA-576: an UNVERIFIED checkpoint is the one exception, and it
> is marked rather than hidden. Its branch rides the claim (the
> review phase needs the name to verify it) with
> CheckpointUnverified set, so the successor's brief is the
> unverified variant - never the verified wording, which this
> checkpoint cannot support.

### CLA-637

> CLA-637: the origin tip the verified branch check read. The
> review brief hands it to the reviewer as the revision to
> check out and record, and the seam matches the record's
> reviewedTip against it. Empty exactly when no tip could be
> read (branchless or unverified), which is when no tip can be
> demanded of the reviewer either.

### CLA-685

> CLA-685: a HANDOFF is the one checkpoint this session's own
> exit did not earn — it holds only because the successor
> continues THIS phase, which the claim carried in as `prev`
> opened. So when the exit evidenced NOTHING (evidenceOK false)
> the verdict just folded above is its REFUSAL's, not a
> checkpoint's: the record gate zeroes the unverified mark even
> though the branch leg had established it. The successor then
> reads carried.CheckpointUnverified as false, is handed the
> VERIFIED continuation - "The driver verified the origin
> remote's tip ... as {{reviewedTip}}" - for a tip this driver
> never read, and the placeholder stays literal, since it is
> filled only when the tip is non-empty.
>
> The exit's verdict about the checkpoint is empty exactly when
> it read no tip of its own: an evidenced branch leg always
> brings a tip (the verified form), and a non-empty tip implies
> unverified=false — the verified continuation, whose
> {{reviewedTip}} that tip then fills. So the opening
> checkpoint's mark is the only true one there, and it rides
> forward, as the panel-member block in drainPhases rides a
> claim's resumption facts onto the claim it hands on.
>
> The tip is carried only for the branch it was read for — it
> is `prev`'s, and a claim that now names another one must not
> be handed a tip that never belonged to it. The BRANCH stays as
> folded: it is the live truth about where the work is. An exit
> that WAS evidenced keeps its own verdict untouched, so the
> verified path is unaffected.

### The cause is the CHECK not completing, named by evidenceWhy

> The cause is the CHECK not completing, named by evidenceWhy
> (the Unknown check's own detail) — never "the origin remote
> could not be read" asserted as fact: delivery.Unknown also
> covers a check that never reached the remote (a branch no
> local repository has, an ambiguous repo, no git on PATH), and
> the log must not contradict the detail it prints.

### released records only a handback that could actually have happened

> `released` records only a handback that could actually have happened:
> a zero Result from a failed launch releases nothing, and reporting it
> as released would erase a predecessor's claim that is still live.
>
> CLA-639 M4: a PANEL MEMBER that ended without a record does NOT hand
> its claim back here. Everything the panel handler must do about a
> failed member needs the claim to still be live: closing the round (only
> the LIVE holder may, and a failed close would leave the earlier
> members' findings sealed behind a lease that is already gone) and
> handing the next member — or the fix phase — the same run to resume.
> A member that genuinely lost the claim is not special-cased: Held() is
> false, so there is nothing to release either way. The handler owns the
> handback for this shape, and takes it AFTER the close (see
> drainPhases).
> CLA-877: a report-resume failure is the one dead end whose claim is KEPT.
> The session's own run succeeded — its commits are pushed — and only the
> reply that re-emits the done report went silent, so the phase retry
> resumes this very run. Handing the claim back here would expire the lease
> on pushed work and cost a reclaim before the same phase could run again.

### Book it into the dead-phase tally (CLA-457)

> Book it into the dead-phase tally (CLA-457): this is the
> "produced nothing" class the rate exists to measure, and a
> fleet-wide silent EOT is exactly the incident in question.
>
> Unless the session's exit was already booked as the CLA-402
> SILENT death: a LAST phase can wear both shapes at once (its
> finish reason is "unknown" AND its evidence gate failed), and
> one session may not read as two dead ones out of one run —
> CLA-639 M4 needs this booking to be exactly one per failed
> panel member, and the operator's rate needs it to be honest.
>
> And unless this is a panel member whose review close failed
> (CLA-639 fix pass 3): a member that WROTE its record and whose
> round the plane then refused to release produced a review — the
> empty shape here would report it as a session that died
> producing nothing, which is the opposite of what happened. The
> skip is PANEL-ONLY: the plain single review keeps the untaxed
> CLA-457 booking it has had since the base commit, so an
> operator's dead-phase rate reads the same with no panel
> (CLA-639 fix pass 4).

### last && a clean exit is the shape CLA-384 is about

> last && a clean exit is the shape CLA-384 is about: the phase ran to
> completion, pushed, and simply did not declare. A non-zero exit reached
> the same place by crashing, which is a different failure with its own
> reporting, and an earlier phase reaching here did not have declaring as
> its job at all.
>
> The return tells whether the claim is actually back in the ready queue
> — the thing a bounded retry can re-claim. A release the plane refused
> (or that never ran) leaves the lease live, so a retry session would
> only be refused and burned.


## `internal/loop/phase_shape.go`

### Whether this phase is a review-SHAPED gate (CLA-637)

> Whether this phase is a review-SHAPED gate (CLA-637): its deliverable is
> the durable review record on the task, and the seam below refuses it a
> checkpoint without one. Read off the CAPTURED sequence, never the live
> config (CLA-664) and never ph.Prompt (a handoff respawn replaces that
> wholesale). The predicate is config.ReviewShaped, so every review-only
> variant is covered — an operator's own custom prompt owns its own contract
> and is not one. nextReviewShaped is the same question about the NEXT
> configured phase: the driver closes a round only when the review stage is
> over, so a later panel member (CLA-639) never sees an earlier member's
> findings.

### CLA-639

> CLA-639: whether this pass is one reviewer of a review panel, and whether
> it is the LAST one. The last member is the panel's seam: it runs the same
> record gate as the shipped single review, and it must run it even when the
> sequence ends there (a panel with no fix phase after it) — the round close
> and the merged list are owed to whoever reads the records next.
>
> A panel member is a review-shaped phase the operator did not write one at
> a time: EffectivePhases expanded one `review` into N. The distinction
> matters because the members are skippable — a dead or capped member is
> recorded as failed and the round still closes — while a lone review phase
> is the sequence's only record and keeps the shipped fail-closed path.

### CLA-784

> CLA-784: an OPTIONAL member is a trial seat. Its end is recorded against
> that seat and nothing more: it may not end the sequence, hand the panel's
> shared claim back, or stop the run. The member handler below reads this
> flag instead of the member's end shape, so every end the task names
> (transient, usage limit, hard stop, death, tool-loop kill, setup error) is
> covered by one branch rather than a list that can rot.

### A BRANCHLESS checkpoint runs exactly ONE reviewer

> A BRANCHLESS checkpoint runs exactly ONE reviewer: the record this shape
> can write is the legacy `## Review` block, not a durable one, so
> drainPhases skips the members after the first. That first member is the
> panel's last member in every sense the seam cares about — its record is
> the panel's only record and the "no member recorded → no fix" rule must
> apply to it — so the position test alone (`no review-shaped phase follows`)
> is not enough here.


## `internal/loop/prbase.go`

### prBaseFor

> prBaseFor resolves the branch this task's repo PRs target, substituted into
> the phase briefs as {{prBase}}. The rule itself is config.ResolvePRBase, shared
> with the deterministic close-out so both judge a repo's merge target the same.
>
> The probe runs afresh per phase rather than caching: a phase is spawned at most
> a few times per task, and a cache keyed on remote state would outlive the branch
> creations it is supposed to observe.

### ProbeOriginPRBase

> ProbeOriginPRBase asks the origin remote of dir two questions in ONE network
> round-trip: whether refs/heads/<integration> exists there, and what its
> symbolic HEAD points at — the answers config.ResolvePRBase's resolution order
> needs. Exported so the deterministic close-out can resolve the same merge
> target the phase briefs were given (internal/supervisor/closeout.go).
>
> `git ls-remote --symref origin HEAD refs/heads/<integration>` prints the
> symref line ("ref: refs/heads/main	HEAD") plus each matching ref, so one call
> answers both. Reading origin is deliberate and the local origin/HEAD symref
> is NOT an alternative: it is unset in some of this fleet's clones (the
> makespdf-zed-extension and clankerbar-cli checkouts the task that filed this
> recorded), and where it is set it can be stale. A remote whose HEAD is
> detached prints no symref line: the default is then unknown and prBaseFor
> falls back.
>
> The read is scoped to the account owning the remote (delivery.ScopeRemoteEnv,
> CLA-458) exactly as the delivery verifier's own ls-remote is: without it an
> HTTPS github origin that names no account is read as whichever gh account is
> ACTIVE, which on the operator's machine cannot see the private lecstor repos,
> and every probe would silently degrade to the integration-branch fallback —
> the feature would never fire where it was filed from. Any scoping failure
> degrades to running unscoped, the same fail-open shape delivery keeps; the
> probe has no correctness cliff on top of it because its own failure mode is
> prBaseFor's fallback.

### runOriginLsRemote

> runOriginLsRemote is ProbeOriginPRBase's one git exec, split out so the probe
> reads as its two answers. The environment discipline is delivery.Verifier's
> gitRun: an unattended run has no terminal, and a git that decides to ask for
> a password would hang the phase rather than answer. WaitDelay reaps the
> helpers ls-remote spawns — they inherit the pipes behind these buffers, and a
> killed git can otherwise leave the call sitting past its deadline.

### parseOriginRefs

> parseOriginRefs reads runOriginLsRemote's output: the "ref: refs/heads/X
> HEAD" symref line when the remote's HEAD is symbolic, and any line naming
> refs/heads/<integration>. A detached remote HEAD prints no symref line, so
> the default comes back empty and prBaseFor falls back. Exact-field matching,
> never substring: "refs/heads/staging" must not be read as a match for an
> integration branch named "ging".


## `internal/loop/progress.go`

### quietBackoff

> quietBackoff is how long a target sits out once its fruitless spend crosses
> the quiet-token ladder: 15m, 30m, 1h, then 2h. It never reaches "never" — the
> blocker is usually an operator answering a question, and a target that can
> never come back would need a restart to notice.
>
> rung counts how many quietTokenThresholds the accumulator has crossed, offset
> by quietThreshold so the FIRST crossing lands on the same 15m the old
> session-count ladder started at.

### quietThreshold

> quietThreshold is how many quietTokenThresholds of fruitless spend it takes
> to back a target off — kept as the ladder's base rung so quietBackoff's shape
> is unchanged from the session-count version.
>
> The old "three, not one or two" rationale survives in token terms: a single
> fruitless drain is ordinary, and so is a second — a genuinely large task can
> span several sessions before anything reaches a reviewer, and backing off
> then would punish exactly the deep work this loop exists to do. ~80M of
> fruitless spend is a different claim, and against the run this was written
> for — which repeated the same no-op ten times at ~26M each — the threshold
> still saves most of them.

### quietTokenThreshold

> quietTokenThreshold is the token-denominated replacement for the old
> session-count trigger (CLA-343). Calibration: the old threshold of 3
> fruitless SESSIONS fired at ~78M tokens (~26M per measured iteration — the
> audit's own figure); 80M lands within one drain of that (the third drain at
> 26M each totals 78M, just under, and the fourth crosses). The drift is
> deliberate and lenient, and it buys the property the session count could
> never give: ONE huge fruitless drain trips the first 15-minute sit-out on its
> own — the 285.9M runaway would have earned it at under a third of its spend
> instead of running three sessions past it.

### judgeProgress

> judgeProgress reads this poll as the verdict on the target's last drain, and
> backs the target off once its fruitless drains have spent enough tokens
> achieving nothing.
>
> Progress is `Settled()` rising — work reaching a reviewer or finishing. A drain
> that only recorded why it could not proceed bumps `version` but settles
> nothing, which is exactly the run this exists to stop repeating. A fruitless
> drain charges its tokens (Driver.spent) to the accumulator; progress clears
> the accumulator.
>
> An ANSWERED QUESTION is the one kind of progress `Settled()` cannot see, and it
> is the very thing the back-off's own message asks the operator to go and do
> (CLA-248). Answering a BLOCKING question takes its task `blocked -> ready` on
> the plane, and a non-blocking one never moved its task at all; either way
> neither `InReview` nor `Done` shifts, so a falling `OpenQuestions` is the only
> trace of it a poll can read. Both signals are watched, and both baselines move
> on every poll: the verdict is always about the change since the last look.

### IDLE IS NOT FRUITLESS

> IDLE IS NOT FRUITLESS. `quiet` counts consecutive DRAINS that settled
> nothing, and a poll with nothing claimable is not a drain — the gate below
> will not spend a session, so this target has not failed at anything. Without
> this the count is immortal: it survives the blocker being parked (`parked`
> is not in Settled(), so nothing else clears it), an arbitrarily long idle
> stretch, and a complete turnover of the queue. A task filed a week later
> then inherits a back-off it did nothing to earn and serves the escalated
> 2h wait on its FIRST fruitless drain — defeating quietThreshold, which
> exists precisely so a large task spanning sessions is not punished.
>
> ONLY WITH NO VERDICT OUTSTANDING, and that condition is load-bearing rather
> than defensive. `claimable == 0` does not only mean "the queue is empty" — it
> also means "everything ready is claimed right now", and the poll immediately
> after a drain is exactly when that is most likely, because a session that
> ends still holding a task it pushed work on is deliberately NOT handed back
> (see releaseHeldClaim). Forgetting the count there would not merely decline
> to charge the drain, it would CANCEL the verdict on one that already ran — so
> a target whose every session ends holding its task would alternate spawn /
> forget forever and never back off at all. Requiring the verdict to be settled
> first costs one extra poll of an idle stretch and nothing else: the drain is
> judged, and the NEXT poll — still idle — does the forgetting.
>
> Clearing skipUntil too, and not only the count: an in-flight wait is moot
> while there is nothing to spawn for, and leaving it set would make the next
> task filed sit out the tail of a wait somebody else's blocker earned.
>
> RESIDUAL, stated rather than implied. A target whose only claimable task is
> held ELSEWHERE for more than one poll — another driver on the same project,
> an agent working it by hand — reads as idle for that stretch, so it forgets
> and lifts. Such a target backs off at the base 15m every quietThreshold
> fruitless drains instead of escalating toward 2h: the breaker still bounds
> the spend, it just stops tightening. That is accepted rather than narrowed
> away, because the two obvious narrowings are both worse. Requiring
> `in_progress == 0` reinstates the immortal count for any project holding
> abandoned WIP (CLA-274), which is precisely the population most likely to
> have some. Requiring N consecutive idle polls does not discriminate at all:
> a task held elsewhere is held for a lease, which is many poll intervals.
> Nothing in a summary count separates "quiet because there is no work" from
> "quiet because somebody else has it" — only whether WE are still spending
> sessions does, and the guard above is the part of that signal worth having.
>
> This cannot spin. A drain needs a poll where the target IS spawnable, so the
> reset never applies to the poll that authorises a session; the count climbs
> from zero the ordinary way, and reaching the threshold again takes
> quietThreshold fresh fruitless drains.

### An idle stretch clears the no-progress (quiet) back-off - with no work

> An idle stretch clears the no-progress (quiet) back-off - with no work
> there is nothing to judge, and the blocker may have resolved itself.
> A CLA-507 harness sit-out is different: the harness itself is broken,
> and an empty queue does not fix it. Clearing skipUntil here would cut
> the laddered sit-out short the moment the queue momentarily empties
> (a sibling daemon consuming the work), so it is preserved until it
> elapses or the harness succeeds again.

### No drain outstanding

> No drain outstanding. Progress can still arrive from elsewhere — the
> operator merging a PR, another machine's loop — and it means whatever was
> stuck may now be unstuck, so let the target back in immediately.
>
> Note what the merge case actually rides on, because it is not obvious and
> nothing here can pin it: the plane approves a task back to `ready` BEFORE
> its PR is merged and it is marked `done`. The first write DROPS `Settled()`
> by one and lowers the baseline with it here, so the later `done` reads as a
> rise. It is that two-step, not the merge, that this branch sees, and only
> because a poll lands between the two writes - which an idle poll of a minute
> against a human-paced approve-then-merge ordinarily does. `in_review -> done`
> with no poll in between is invisible, exactly like `blocked -> ready`.

### The strike is earned whatever else moved

> The strike is earned whatever else moved: a session that answered its own
> question has not thereby delivered anything, and the drain is being judged on
> what it SETTLED. But an answer that landed while the drain was running is the
> operator doing the very thing the back-off message asked for, and this poll is
> the only one that can ever see it: the baseline advances just below, so a fall
> consumed here is gone rather than deferred. Take the strike, skip the sit-out.
> The target gets one immediate retry against the answer; if that settles
> nothing too, the retry's spend is charged like any other, so the rung it
> reaches depends on how big it was (a whole-threshold retry escalates at once,
> a smaller one repeats the band) — the retry is never an exemption.

### Bands, matching the old session-count ladder at the ~26M-per-drain

> Bands, matching the old session-count ladder at the ~26M-per-drain
> calibration: 3 fruitless drains (~78M) earned 15m, 4 (~104M) earned 30m,
> 5 (~130M) earned 1h, 6+ (~156M) earned the 2h cap. In whole thresholds:
> acc in [80M,160M) → rung 3 → 15m; [160M,240M) → 30m; [240M,320M) → 1h;
> 320M+ → 2h. One huge fruitless drain (≥80M) trips the FIRST band on its
> own — the property the session count could never give (CLA-343).

### sidelineTarget

> sidelineTarget marks target i failed after drainPhases returned an error: the
> failure happened while running THIS project's session, so this project sits out
> while the others drain on. The sit-out uses the quiet ladder's shape: 15m,
> 30m, 1h, then a 2h cap, keyed on consecutive failures rather than fruitless
> tokens, and it self-heals: the operator fixes the config (or the checkout), the
> next attempt after the sit-out succeeds, and Run resets the count.
>
> The log line is the operator's whole diagnosis for the exact incident that
> motivated this (2026-08-26): which project died, in which workdir, the
> harness's own error text - and the reassurance that the fleet carried on.

### allSidelined

> allSidelined reports whether every target is CURRENTLY sitting out a harness
> failure: nothing left to drive, so ending the run beats idle-polling forever.
> A target counts only while its failure back-off is live - skipUntil zeroed by
> an elapsed wait means it is drivable again, whatever its history, and a target
> that has never failed since its last success never counts.
>
> A target paused for an operator answer (fleet or startup trip, CLA-396/562)
> never counts as sidelined: the pause is meant to wait for the answer, not to
> exit. Without this a fleet of one that trips its startup bound would exit on
> the same iteration it paused, losing the wait it just asked for.

### failureDetail

> failureDetail renders a harness diagnostic for a one-line stop message: the
> TAIL of the text, non-printables stripped, whitespace collapsed, bounded.
>
> The tail rather than the head, deliberately. The scope starts with stderr,
> which on a real run leads with startup noise that is identical on every
> session; the thing that killed this one is the last thing said. Truncating
> from the front would reliably show the operator the one part that is never
> the answer.
>
> Non-printables are stripped because this changes what the text is FOR. Up to
> now a diagnostic was only ever fed to regexp.MatchString, where a control byte
> is inert; here it is rendered to a terminal, where ESC is executed. The
> scoping rules inherited from CLA-258 answer "what may a classifier READ", which
> is a different question from "what may be PRINTED", and stderr reaches this
> verbatim from the harness. Stripping is cheap and the answer does not depend
> on trusting the upstream CLI's output.
>
> Empty in, empty out — and the caller appends it, so a harness with nothing to
> say leaves the existing message exactly as it was rather than trailing a
> colon and a blank.

### Counted and sliced in RUNES, not bytes

> Counted and sliced in RUNES, not bytes: the harness's own text carries "·"
> in every usage-limit notice and "→" in rendered tool markers, so a byte
> slice would routinely cut a rune in half and emit U+FFFD at the seam.
>
> "..." rather than U+2026: this string exists to be pasted into a bug
> report, and a single-character ellipsis is exactly what arrives as
> mojibake when terminal output is copied out.

### droppedNote

> droppedNote says how much of a session's output the retained window did not
> keep, when it kept less than all of it.
>
> It rides on the message that ENDS a run, because that is the message whose
> weight depends on it: "exited 1 (non-retryable): <text>" reads as "this was the
> failure", when what the classifier actually saw was the last couple of MiB. An
> operator deciding whether the classifier merely does not know this failure yet
> needs to know which of those they are looking at. Empty when nothing was
> dropped, which is the ordinary case.


## `internal/loop/review_records.go`

### reviewGate

> reviewGate is the review-shaped phase's seam contract (CLA-637): whether a
> durable review record is required, and the outcome as it stood BEFORE the
> phase spawned. The record requirement makes the review this-trip rather than
> last-trip the way B1 prescribes: the seam demands a record keyed on the run
> that wrote it, and a request_changes re-trip (or a takeover) runs under a
> fresh run id, so a record from a previous trip can never satisfy it. The
> baseline is kept for the pre-spawn read's tri-state and the refusal's
> diagnostic.
>
> read is the tri-state the freshness check needs: false means the baseline
> could not be read (no plane reader, a failed read, no task id on the carried
> claim). Since CLA-663 that is refused BEFORE the phase spawns — a full
> review session whose checkpoint the seam would refuse anyway is pure waste,
> and the claim would lapse into a takeover that re-runs implement and review.
> baseline is meaningful only when read is true.
>
> Since CLA-637 the gate itself is the DURABLE review record (B1), keyed on
> the run that wrote it: role names the record this phase's session writes
> (both sides read the same name out of the brief), and round is written back
> by checkpointEvidence with the round the matched record came from — the round
> the driver then closes to release the findings to the fix phase. Zero means
> no record matched.

### reviewerRoleFor

> reviewerRoleFor is the reviewer role of the review-shaped phase at idx
> (CLA-637): `review-N`, counting the review-shaped phases up to and including
> it, so two review phases in one sequence (a panel, CLA-639) write distinct
> records and the plane's one-row-per-role-per-round rule is not tripped. The
> role is a name both sides read: the brief hands it to the session and the
> seam gate keys the record on it. Empty for a phase that is not review-shaped.

### matchingReviewRecord

> matchingReviewRecord finds the durable review record a review-shaped phase's
> seam gate demands (CLA-637, B1): a record THIS RUN wrote under this reviewer
> role. The run id is the freshness key — a request_changes re-trip or a
> takeover runs under a fresh run id, so the previous trip's record can never
> satisfy the gate — and records voided by a lease expiry or takeover (B4)
> never match either. The tip requirement follows the checkpoint the driver
> made:
>
>   - verified: tip is the origin tip the driver read for the branch, and the
>     record must name exactly it — the revision reviewed must still BE the
>     tip the fix phase will find.
>   - unverified (CLA-576): the driver has no tip, so the reviewer established
>     one itself; the record must name one, because the fix phase compares it
>     with the worktree.
>
> A BRANCHLESS checkpoint does not reach this matcher at all: the plane accepts
> review writes only from the task's live holder, and a settled task has none,
> so that variant keeps the legacy `## Review` block gate (the CLA-634 decision
> on question 03a12578, option 1).
>
> The run ids are compared by runUUID: the claim hands the driver the run
> CREDENTIAL, while the plane stores and serves a record's run as the bare id.
>
> The newest match wins, so a retried attempt's record supersedes an earlier
> one in the same run.

### panelMemberRecordMatch

> panelMemberRecordMatch asks the plane whether a panel member's durable record
> is already in the round, using the SAME evidence the review seam uses:
> matchingReviewRecord keyed by this run and the member's positional role, at
> the checkpoint's reviewed tip.
>
> The seam asks that question after a session ends. A member that writes its
> record and only THEN ends the sequence — a usage limit or a transient failure
> — returns from drainPhase before the seam runs, so the panel handler must ask
> it here or the member is recorded as a failure while its record sits in the
> round (CLA-639 fix pass 4). Best-effort: an unreadable task or an unwired
> reader leaves the member classified as failed, exactly as before this check.

### runUUID

> runUUID is a run id's identity: the part before the first dot. Since CLA-509
> claim_task returns a run as the credential `<uuid>.<secret>`, but the plane
> keys everything it stores (review records, claimedByRun) on the bare uuid,
> so two ids name the same run exactly when their uuids match. A bare id (a
> run claimed before credentials, or one read back from the plane) is its own
> uuid. This is the plane's own split (parseRunCredential).

### reviewRecordLine

> reviewRecordLine reports whether outcome carries the review heading as its
> own line. The BRANCHLESS review-only checkpoint gates on it (CLA-634
> decision, question 03a12578, option 1): a settled task cannot write the
> durable record — the plane accepts review writes only from the live holder —
> so that variant keeps the legacy block gate. The exact line keeps a
> look-alike heading (`## Reviewer notes`, or prose that merely mentions the
> words) from standing in for the block. Trailing whitespace is ignored — the
> plane stores text as sent, and a CRLF paste or a stray trailing space is
> still the heading — while leading whitespace is not: a heading the session
> indented into a code block is not the block the fix phase will find.

### lastReviewBlock

> lastReviewBlock returns the outcome's text from its LAST `## Review` heading
> line to the end — the block a fix session is to read and apply. A task that
> went through a request_changes re-trip, or a takeover, can carry several
> blocks: both the review and the fix APPEND, so round 1's block stays in the
> outcome above round 2's, and "the `## Review` block" is ambiguous unless it
> means the last one (which is why every fix brief says so). The heading line
> itself is included: a block whose text is only its heading would otherwise
> decode to "", which is a substring of everything. It recognises the heading
> exactly as reviewRecordLine does, so the two agree on what a block is.
>
> The BRANCHLESS review-only checkpoint's stale-round guard reads it: a
> session that makes some other statusless outcome edit while leaving an
> earlier round's block as the last one recorded nothing this trip. Empty when
> the outcome carries no heading line at all; the gate checks reviewRecordLine
> first, so an empty return there is not a block, not an old one.

### reviewFindingsDigest

> reviewFindingsDigest is the driver's graded list of a review record's
> findings, prepended to the fix phase's prompt (CLA-635). It exists because
> B5's structural downgrade is the DRIVER's to apply: a `must` recorded without
> an evidence kind or payload is listed here as a SHOULD, so the fixer is held
> to the same grade the record will be read with later. The severities are the
> ones the fix brief's obligation speaks about; the raw block stays in the
> outcome for the record.
>
> Empty when the record carries no parseable findings — the fix brief then
> works from the raw block alone, exactly as before this existed.


## `internal/loop/review_run.go`

### reviewrun.go

> review_run.go — `clankerbar review run` (CLA-870): review ONE local
> base..head outside any task, claim or plane, and emit the findings as JSON.
>
> It is a driver, not a second review: the brief is the review phase's own
> substance without a task (config.ReviewRunBrief), the maps are built by the
> phase's planner (planMapsSession), and the report is the phase's done report
> read by the same ParseDone/ValidateReview pair. What it does NOT do is any
> part of the task lifecycle — no claim, no MCP config, no plane record — so the
> eval harness scores the review the driver really runs, not a reimplementation
> of it.

### RunReview

> RunReview reviews opts.Base..opts.Head in a temporary detached worktree at
> Head, removes that worktree whatever happened, and returns the parsed report.
>
> The worktree is the isolation contract: the reviewed repository's own checkout
> is only ever read from (rev-parse, worktree add/remove), so a dirty tree, a
> branch in the middle of something, or a review that edits files cannot disturb
> it. When the run produced a report but cleanup failed, the report is returned
> WITH the error so a caller can still write the JSON; the command exits non-zero
> either way.

### reviewRunInvocation

> reviewRunInvocation composes the session: the taskless brief, the maps
> section when the method asks for it, and the harness/session fields the
> phase driver would resolve from config. It deliberately leaves MCPConfigPath
> empty: this command has no plane identity to hand a session, and a session
> that tried a clankerbar call would be talking to the operator's backlog in a
> review that must not touch it.


## `internal/loop/route_fallback.go`

### routefallback.go

> route_fallback.go — the driver's half of CLA-782's model route fallback.
>
> The opencode adapter switches routes INSIDE one session: a usage limit on the
> free route resumes the same session on the paid route and reports the switch on
> Result.Raw (see harness.RouteFallback). This file is what makes the switch
> STICKY across the run AND across restarts (marks persist in the state dir, ROUTE_MARKS): it records a mark per denied route, keeps later phases
> off a marked route until the mark lifts, and writes the switch to both logs
> with the route, the reason and when the denied route is worth trying again.
>
> The mark's length is the provider's stated reset when it gave one, else the
> same default interval the supervised wait probes at (config `poll_interval`,
> 30 minutes). Nothing here waits: a marked route is simply not offered at the
> next phase's spawn, so the clanker keeps working on the route that worked.

### routableRoutes

> routableRoutes applies this instance's marks AND the config's route pricing
> windows to one phase's configured route list, at the driver's clock.
>
> Unmarked, unwindowed routes are kept in configured order. A route inside one
> of its schedule's windows is unavailable exactly as a marked one is; the
> window is a filter on SELECTION, never a gate on the driver, so a route with
> no schedule (or an empty one) is always available.
>
> When every route carries a usage MARK — a phase that starts while the whole
> chain is still in its reset window — the route whose mark lifts FIRST is used,
> alone: it is the soonest the free route can be tested again, and a phase must
> still start rather than wait on a mark. A marked route is never offered as a
> fallback to an unmarked one, and when_all_blocked "run" never overrides a mark:
> only the window filter degrades.
>
> A WINDOW, unlike a mark, is never overridden by one: a mark only orders the
> candidates ("test the soonest-resetting route first"), while a window is the
> price the operator said not to pay. So the all-marked fallback is window
> filtered too — a marked route inside its window under "wait" is dropped, the
> marked route whose mark lifts first among those left open is used, and when
> every marked route is windowed the phase holds until the earliest window ends.
>
> When no route is marked but every one is inside a window, the schedule's
> when_all_blocked decides, through config.RouteChoiceAt: "run" offers them
> again, "wait" returns none plus the instant the earliest window ends so the
> caller can hold the spawn.

### noteRouteFallbacks

> noteRouteFallbacks records the marks an adapter reported and writes each
> SWITCH to the daemon log and the iteration log. It runs on every attempt,
> before any classification: a switch is a fact about the session that happened
> whichever way the attempt then ended, and the next attempt (a transient retry)
> must already start on the route that worked.
>
> A denied LAST route is marked but not logged here: there was no switch, and the
> existing limit branch already says what happened to the session.

### routeRoutesFor

> routeRoutesFor is the one place the driver turns a phase into the routes its
> session will offer the adapter, marks and pricing windows applied. It keeps
> ModelForPhase's answer authoritative when the config names no model at all
> (the harness default), and otherwise starts on the first ROUTABLE route.
>
> A choice with no routes and a WaitUntil is not "no model": it means every
> route is inside a window under a "wait" schedule, and invocationFor refuses
> the spawn rather than running on a route the operator paused.

### probeRouteFor

> probeRouteFor is the route a supervised-wait probe should test for a phase
> with a route list (review finding 1.review-1.3): the route its next session
> would start on, marks and pricing windows applied. A wait is entered only once
> every route is spent, so that is the route whose mark lifts first - not the
> harness block's first route, which may be a route this run has already moved
> off. It returns "" for a phase with one route or none, so a single-route probe
> keeps the model it always had, and also when every route is windowed (there is
> no preferred route to aim at; probeInvocation refuses that probe instead).

### probeInvocation

> probeInvocation is the supervised-wait probe's Invocation: the phase harness's
> probe invocation, aimed at probeRouteFor's route when the phase has a route
> list.
>
> A phase whose every route is inside a "wait" pricing window has NO route a
> probe may start on, so it refuses the probe rather than letting the phase's
> model through: the probe is a paid session, and paying the peak price to learn
> whether a limit lifted is exactly what the schedule exists to prevent. The
> wait logs the refusal and retries on its own interval, spending nothing.


## `internal/loop/route_pricing.go`

### routepricing.go

> route_pricing.go — the loop's half of CLA-947: per-route pricing windows.
>
> `pause_windows` is the operator's daemon-wide "start no new work" schedule.
> A route schedule answers a narrower question — "do not pay THIS provider's
> peak rate" — and is therefore a filter on route SELECTION (routableRoutes),
> not a gate on the driver. Only when every route in a phase's list is inside a
> window does it become a gate, and then the schedule's when_all_blocked decides
> whether to hold the spawn or accept the peak price.
>
> The filter is a pure function of (route, now): the boundary hold below and
> invocationFor both read it through a clock the driver can pin in tests, and
> both read the LIVE config, so a RELOAD that removes, shortens or adds a
> schedule reaches the hold at its next wake rather than at the next drain.

### routeWindowBlocked

> routeWindowBlocked is invocationFor's refusal when every route a phase may
> start on is inside a pricing window and its schedule says wait.
>
> It is not a failure: the phase boundary holds the spawn until the earliest
> window ends (holdThroughRouteWindows). Reaching here means the window opened
> in the instant between the boundary check and the spawn, so the ladder ends
> the phase without spending rather than starting on a route the operator
> paused.

### holdThroughRouteWindows

> holdThroughRouteWindows holds a phase sequence at its next boundary while
> every route that phase may start on is inside one of its pricing windows and
> the schedule says wait.
>
> It is the same hold as a scheduled pause (CLA-906): the claim is kept and its
> lease renewed, presence goes out on the same cadence, and STOP / RESTART /
> HALT / RELOAD stay responsive. A session already running is never interrupted
> — this runs only before the NEXT session spawns.
>
> The config is resolved at every wake, from the same source invocationFor
> reads (liveCfg), so a RELOAD that deletes a schedule ends the hold at once and
> one that adds a window puts the hold back: a captured config would leave the
> hold parked to a window the operator has removed while the next spawn was
> already being refused by the new one.

### holdThroughPanelRouteWindows

> holdThroughPanelRouteWindows is the same hold for a PARALLEL review panel's
> fan-out, over every member's route list rather than the first member's.
>
> checkPhaseBoundary runs once per fan-out and can only see the phase at s.i, so
> a later member whose own list is windowed would otherwise reach invocationFor
> and be refused there — booked as a reviewer that recorded nothing, with the
> failure streak and the dead rate charged for a scheduled peak. The whole panel
> is held instead, until the last blocked member's window opens; the wait is
> shared, so a member that could start sooner does not start alone.


## `internal/loop/run.go`

### Run

> Run drives the daemon until STOP/HALT, a ceiling (max-iterations / budget), or
> context cancellation. An empty queue is NOT a stop condition — it idles and
> keeps polling. Returns nil on a graceful stop; an error only on an unexpected,
> non-retryable failure.
>
> Two things hold on every way out (CLA-491): a STOP marker this run never
> consumed is removed rather than left where the next start would read it as a
> fresh stop request, and the last log line names why the run ended.

### Belt and braces on the way out (CLA-491)

> Belt and braces on the way out (CLA-491). Every shutdown path that does not
> pass a STOP read (a non-retryable phase failure, a budget or zero-spend
> trip, max-iterations, HALT, a cancelled context) leaves a
> pending STOP marker where it fell, and the next start used to read it as
> fresh and stop itself seconds into its first cycle: the 2026-08-25 fleet
> soft-stop left one behind clanker1, and the upgraded replacement DOA'd on
> it. Registered here rather than at each return so a future shutdown path
> cannot forget it. (The closure, not `defer d.sweepPendingStop(runErr)`:
> arguments evaluate at registration, and runErr is not assigned yet.)

### Every harness this run will SPAWN, not just the configured one

> Every harness this run will SPAWN, not just the configured one: a mixed
> sequence's banner naming one of them is a log that disagrees with the
> sessions underneath it from the first line (CLA-366). SpawnedHarnesses
> rather than PhaseHarnesses, so a run-wide harness every phase overrides is
> not announced as being driven when nothing will ever run on it - the banner
> is the operator's first line of output, and it should describe the run.

### A STOP marker already sitting here PREDATES this process

> A STOP marker already sitting here PREDATES this process: markers are only
> ever read and consumed by a running daemon (the cycle top below, and
> waitOrStop), so one found at start-up was left behind by a previous run
> that exited without consuming it (CLA-491). Its intent died with that run;
> acting on it here would stop this daemon before it did anything: exactly
> what the upgraded clanker1 did seconds after its start banner. Consume it
> with a loud note instead. A stop dropped AFTER this point is fresh and
> still lands on every read below.

### The budget breaker's between-iterations pass (CLA-480)

> The budget breaker's between-iterations pass (CLA-480): every target's
> own account is judged against the budget in force for it, and a target
> that reaches a ceiling stops draining while its siblings carry on. The
> run ends only when every target is exhausted. Run here, before the poll
> gate, so an already-elapsed wall-clock ceiling still stops the loop
> before it spends or even polls.

### Gate on cheap control-plane reads

> Gate on cheap control-plane reads: only spend a session when some target
> has work to spawn for — ready work, or abandoned work the plane would
> offer for takeover (Summary.Spawnable, CLA-274). When idle, keep polling
> (and logging) so the loop reacts to answered questions / promotions /
> newly filed work. With several targets (CLA-142) every queue is polled
> each cycle, and the drain goes to the next spawnable one after the last
> drained (round-robin) — a busy queue can't starve a quiet one.

### The drain's spend is already in the target's ledger

> The drain's spend is already in the target's ledger: charge() books it
> as each session's result is counted, which is what lets the mid-drain
> checks and this between-iterations sweep read the same live account.
> Handoff respawns are charged there too (CLA-352): unlike phase sessions,
> which the operator priced in when they configured `phases`, each respawn
> is a session the SESSION chose to add, so it consumes an iteration of the
> operator's ceiling rather than extending the run for free.
>
> Judge the budget now, at the same between-iterations moment the old
> run-wide breaker read (CLA-480). A target that has reached the budget in
> force for it is exhausted for the rest of the run; the run stops only
> when every target is.

### Classify before acting (CLA-507)

> Classify before acting (CLA-507). The run-wide causes are the ones
> the poll loop already hard-stops on - a bad key or a missing project
> selector is shared by every target, so no target can make progress
> and sidelining would just defer the same exit. They never originate
> in a drain today; the guard keeps that boundary explicit rather than
> incidental. Context cancellation arrives as stop=true with err=nil,
> so it needs no classification here.

### CLA-562

> CLA-562: the startup-failure escalation rides on top of the
> sideline, mirroring the fleet trip. A startup failure is an ERROR
> with no usage reported (no session ever ran); a dead phase is
> err == nil, so one failure never ticks both counters. The sideline
> stays for the first startupFailBound-1 attempts; only the Nth
> escalates to a pause-and-raise. An error WITH usage proves a
> session ran, so it breaks startup consecutiveness without tripping.

### CLA-564

> CLA-564: every task boundary is a stop boundary, not just idle waits.
> The cycle top reads these before the next poll, and this repeats the
> read after the drain has fully unwound so the stop lands here —
> before the next poll's side effects and the next spawn — rather than
> depending on a single read a future fast path could bypass. A
> file-exists read, never a wait, so the wired "loop straight back"
> path below stays fast. HALT stops without being consumed (the
> operator deletes it to resume); STOP is consumed on stop so a
> restart is not stillborn.
>
> A cancelled context outranks both markers here exactly as at the
> cycle top: falling through lets the top report the cancel, and the
> exit sweep clears any pending STOP (CLA-491).

### Blind mode has no counts to gate on, so it idles between sessions rath

> Blind mode has no counts to gate on, so it idles between sessions rather
> than spinning. Note this is a PER-TASK pause now: the default prompt asks
> for one task, so a blind run alternates task / idle / task, where the old
> drain default spent one idle on the whole queue. An operator tuning
> `idle_poll_interval` on a blind run is setting the gap between tasks.
> In wired mode, loop straight back — the next cheap poll decides whether
> more work appeared.

### sweepPendingStop

> sweepPendingStop is Run's exit sweep (CLA-491): it removes a STOP marker the
> run never consumed, and logs the reason the run ended.
>
> A pending marker at exit means the operator dropped a soft stop against a LIVE
> daemon that then exited through a path with no STOP read (a non-retryable
> phase failure, a budget or zero-spend trip inside the drain, HALT beside it,
> a signal). The marker's intent is satisfied by ANY exit: the daemon is down
> either way, but leaving it would make the next start read a ghost and stop
> itself before doing anything. runErr carries how this run ended: an error
> return names it in the closing line (and so does every graceful path's own
> log line above), so the daemon log always says why.

### A 400 projectrequired means the poll hit the LEGACY slug-less summary

> A 400 project_required means the poll hit the LEGACY slug-less summary
> route with an ACCOUNT-scoped key, which that route cannot bind to a
> project. The remedy is a project selector, NOT abandoning the account
> key (decision 2026-07-29): give the loop a slug to name in the path —
> a `projects` list in the config, or an .mcp.json whose clankerbar URL
> is /mcp/<slug> — or, for CI-style setups only, a project-scoped key.
> Still a hard stop: the mismatch won't self-heal, and idle-retrying or
> blind-draining would just burn doomed sessions (CLA-133/CLA-142).

### The stored execution-config boundary (CLA-410)

> The stored execution-config boundary (CLA-410): a moved
> runConfigVersion — including the first poll, which is
> "start of run" — refetches and re-overlays BEFORE the
> spawn gate reads anything, so the drain that follows this
> poll runs on the current document. Mid-session is
> unreachable by construction: polls happen only here.

### staleclaimable is reported beside claimable, not folded into it

> stale_claimable is reported beside claimable, not folded into it: an
> operator watching the console has to be able to tell "spawning
> because there is fresh work" from "spawning to recover an abandoned
> branch", and before CLA-274 those were indistinguishable — the
> second did not happen at all, and now that it does it must not look
> like the first.

### Console pause (CLA-76 plane / CLA-130 driver)

> Console pause (CLA-76 plane / CLA-130 driver): the operator can pause
> a run from the web console, PER PROJECT. Honour it BEFORE the spawn
> gate so a paused project never gets a new session even when it has
> work to spawn for — including a recovery session for abandoned work,
> which a pause outranks like any other. Other projects keep draining.
> Distinct from
> STOP (which exits the instance) and from an empty queue. A pause
> landing mid-drain is only seen here, between iterations, so it never
> kills an in-flight session (exactly like STOP).

### A poll taken while the loop has stopped spawning cannot tell "our

> A poll taken while the loop has stopped spawning cannot tell "our
> last drain settled nothing" from "nothing happened because we
> stopped", so the verdict waits until the loop is live again. A
> budget-exhausted target is one of those stops (CLA-480): judging
> its poll would charge the drain that reached the ceiling to the
> quiet accumulator and log a back-off for a project that will
> never spawn again this run.

### CLA-466

> CLA-466: the presence beacon rides THIS poll — no new cadence. It
> fires AFTER the switch, whose default arm writes the pause flags
> fleetState reads (CLA-483); every poll beacons bar the two hard-stop
> arms, which get their `stopping` from fleetShutdown. A session blocks
> the poll, so the in-session renewal tick carries the same lap (CLA-771).
> The plane gets one report per target; the local file is written once
> after the loop (see localState above).

### Blind mode has no counts to route on - rotate across targets so every

> Blind mode has no counts to route on - rotate across targets so every
> queue still gets sessions. A target sitting out a harness failure
> (CLA-507) is skipped here exactly as the wired gate skips it; the only
> way skipUntil can be set in blind mode is that failure path, since
> judgeProgress never runs without polls. Walking a full lap without
> finding a drivable target means nothing is left, and that exit is the
> honest one. The lap reads from the PRE-pass cursor: mutating d.cursor
> per step would make later steps orbit the mutated value instead of
> walking the ring.

### The restart/reload markers (CLA-461), honoured exactly where STOP is

> The restart/reload markers (CLA-461), honoured exactly where STOP is: the
> ITERATION boundary and, for STOP and RESTART, a verified phase seam
> (CLA-879). Reaching this point means the previous drain has fully unwound
> and handed its claim back, so a graceful restart holds no claim across the
> boundary by construction (resume is the plane's job anyway). A RESTART_NOW
> consumed here found no session in flight and acts as a plain restart; when
> one IS in flight the watcher in watchRestartNow has already cancelled the
> drain and this block is never reached.


## `internal/loop/runconfig.go`

### Run-config consumption in the driver (CLA-410)

> Run-config consumption in the driver (CLA-410): the plane's stored execution
> config, fetched per project and applied AT THE ITERATION BOUNDARY.
>
> The signal is free: the backlog-summary poll this loop already makes between
> iterations carries `runConfigVersion` (CLA-408). When it moves for a target —
> or on the very first poll, which is how "at start of run" is satisfied — the
> target's effective config is rebuilt here, before the spawn gate reads it.
> A session already in flight finishes on the config it started with: the poll
> never happens mid-session, so "never applies mid-session" is structural,
> not a promise to keep.
>
> The failure posture copies applyReload's (CLA-461): a fetch that fails is
> retried at a poll-bounded rate; a document that arrives but fails local
> validation is refused loudly and the previous config stays. Neither converts
> an operator's ratify click into a daemon outage.

### liveCfg

> liveCfg is targetCfg re-read NOW: the same answer invocationFor gets, taken
> fresh at the moment of asking rather than held from the drain's start. A
> boundary hold calls it at every wake (CLA-947 review 1.review-1.1) because
> d.cfg is swapped by a RELOAD on another goroutine — it is read under controlMu
> the way pauseState reads it — so the hold decides from the config the next
> spawn will use, not from a schedule the operator has since removed.

### overlayBudgetAt

> overlayBudgetAt reports whether target i is judged by a stored budget of its
> own. A stored document that sets no budget leaves the target in the local
> pool: the README's exception is the document's BUDGET, not its mere existence,
> and applying a model-only document must not quietly split the instance-wide
> pool into one-per-project ceilings. Set at applyRunConfig time from
> doc.Budget != nil.

### Permanent nothing-stored (CLA-481)

> Permanent nothing-stored (CLA-481): version 0 IS the plane's "never set"
> answer, so when nothing is in force (unfetched or already at none) the
> answer is known without a fetch. Latch silently — staying at none is not
> news — instead of fetching every boundary only to learn ErrNoConfig.
> A 0 arriving while a stored overlay is in force (rcVersions > 0) still
> fetches below, so the drop to local is confirmed and logged.

### Rate-limit RETRIES after a failed fetch of the SAME version to one per

> Rate-limit RETRIES after a failed fetch of the SAME version to one per
> backoff window, so a blip at ratify time retries without spamming the log.
> A DIFFERENT version always fetches: the operator moved the document again,
> and no throttle may delay that past the boundary it arrived on. The window
> widens with consecutive failures (CLA-481), so a permanently unreachable
> plane backs off instead of logging every 30s forever.

### Permanent

> Permanent: nothing to fetch, ever, for this version. ErrNotWired
> is the slug-less / project-scoped-key shape (rcNotWired) — the
> same "local file rules" posture as nothing stored, silent
> thereafter, never retried. Say it once per transition, not every
> poll — dropping from a stored doc to none is real news, staying
> at none is not.

### invalidateRunConfigs

> invalidateRunConfigs marks every wired target's stored-config state as
> unfetched again, so the next poll refetches and re-overlays on the CURRENT
> base. Called from applyReload: a RELOAD swaps d.cfg, and a target's existing
> overlay was built from the OLD base — its phases, prompts and any dial the
> stored document does not set are the pre-reload file's, and must not outlive
> it. The refetch lands on the poll immediately after the RELOAD marker was
> consumed, which is still an iteration boundary — never mid-session. The
> existing overlay is NOT dropped here: if that refetch fails, the previous
> config stays in force (the same posture as any failed fetch), and it
> self-heals on a later boundary.


## `internal/loop/runner_brief.go`

### runnerPrompt

> runnerPrompt swaps a variant-resolved shipped brief for its runner-claimed
> counterpart. The runner owns the claim, the worktree and the lease, so the
> shipped text that tells a session to do those things would be wrong.
>
> Everything else is left alone: a custom prompt owns its own wording, a panel
> member does its own job, and the no-code and unverified variants still work in
> a resumed run (runner versions of those are a follow-up).


## `internal/loop/salvage.go`

### endUntrustedDrain

> endUntrustedDrain closes out a drain whose session output could not be read
> whole, without acting on anything that stream said (CLA-262).
>
> # Why the spend is not counted
>
> It is not that the figure is inconvenient — it is that it is not a measurement.
> The `result` event carrying a claude session's whole tokens-and-cost total is
> the LAST thing on the stream, so a stream cut short reports zero for a session
> that may have cost hundreds of dollars, and a partial sum from an adapter that
> accumulates per step is a lower bound of unknown looseness. Adding either to the
> accumulator would put a made-up number in front of the breaker and in the
> iteration's cost line.
>
> # Why a spend ceiling then stops the account
>
> Because the ceiling is a promise, and this is the driver saying it can no longer
> keep it. An operator who set `max_tokens` or `max_cost_usd` has asked to be
> protected from exactly the shape this is: sessions whose cost nothing can see.
> One unaccounted session is survivable; a night of them against an inert ceiling
> is the failure CLA-287 and CLA-258 were both about. Stopping is clean, loud, and
> resumable — the operator restarts the run once they know why.
>
> WHICH account stops follows the budget in force (CLA-480): a target whose stored
> document set the ceiling is exhausted on its own, with budgetStopped recorded so
> Run carries on with its siblings; only the local pool's ceiling (the run-wide
> promise over every project it governs) keeps the daemon-wide stop.
>
> With no spend ceiling set there is nothing to break: the wall clock does not
> depend on anything the child said, so the run carries on and the no-progress
> back-off is what bounds a target that keeps producing these.
>
> The harness NAME is the phase's own, not the run's: whether a spend ceiling is
> live is a per-harness question since CLA-367, and under per-phase harnesses
> (CLA-366) the session that went unreadable is not necessarily on d.h. Asking
> d.h would let an unreadable opencode session be waved through on the grounds
> that CLAUDE has no ceiling set.

### salvageStrandedWork

> salvageStrandedWork commits and pushes the work a dead session left in its
> worktree, and records the branch on the task so another machine can pick it up
> (CLA-314).
>
> workdir is where the session actually ran — the resolved spawn directory, not
> rebuilt from the target — because that is where its worktree lives once
> sessions start in per-task checkouts (CLA-437). Salvaging from anywhere else
> would report "nothing to salvage" over real stranded work.
>
> # Why it runs on EVERY ending, not only on a usage limit
>
> The worktree is what is being rescued, and it looks identical whether the
> session was killed by a limit, died in a crash, or simply stopped with the work
> unfinished — while the cost of losing it is the same in all three. The limit,
> besides, is detected by parsing the stream AFTER the fact, so gating on it
> would skip exactly the sessions whose stream could not be read: the ones we
> know least about, and the likeliest to have stranded something. Running it when
> it was not needed costs nothing — a clean worktree commits nothing, pushes
> nothing and records nothing.
>
> # What it will not save (CLA-695)
>
> Untracked files alone, when the session DELIVERED — the branch it recorded for
> the task is on the origin at exactly the worktree's HEAD. There is nothing to
> rescue, and committing a generated file (a `wrangler types` dump, a report, a
> build artifact) pushes a NEW tip at the phase boundary: every review-panel
> member, the fix phase and the PR then ride a commit nobody chose, which is
> exactly what happened on the first review-panel run. Tracked edits are still
> salvaged on top of a delivered tip, and an undelivered session's untracked files
> are still salvaged in full — new source files it never added are the work
> CLA-314 exists to save. Delivery is the discriminator, and internal/salvage
> checks it against the origin rather than taking the session's word for it.
>
> # Why it is safe on the untrusted path
>
> CLA-262 forbids acting on a truncated stream's CLAIM-STATE: a settle we never
> saw may be in the bytes that never arrived, so handing the task back could post
> `ready` over work already in review. This does something different in kind.
> Recording a branch carries no status, so it cannot move a task, clear a holder
> or revert a review — it is additive, and its worst case on a task that really
> did reach review is that the branch field names the branch the work is on
> anyway. What it acts on is the local worktree, which no truncation can
> misreport. A claim we SAW settle is skipped entirely: Held() is false, and this
> is a rescue, not a tidy-up.
>
> # What a success does to the handback
>
> Claim.HasWIP is set only when the plane ACCEPTED the branch, and that is what
> makes the claim non-releasable below: the lease is left to expire so the task
> stays a takeover and the hand-off survives. If the record failed, the task has
> no branch on it, releasing to `ready` is still the better move, and the old
> path runs unchanged.

### CLA-386

> CLA-386: the tree is clean because nothing was ever written, not
> because the work landed. The line must say so plainly — "nothing to
> salvage" reads identically either way, and the only other
> discriminator (a `verified <ref>: origin/<branch>` line) is an
> absence, which is the weakest possible signal. Gated on the driver's
> own classification (not the raw deadPhase predicate): a capped or
> untrusted session is an orderly end whose finish reason is its own
> marker or cannot be read, and must not be handed the emphatic line.

### CLA-695

> CLA-695: the session DELIVERED — the branch it recorded is on the origin
> at exactly the worktree's HEAD — and left nothing but untracked files.
> There is nothing to rescue and nothing to record, so this is a report
> rather than an alarm: the paths are named because an untracked file can
> still be work (a source file written after the last commit), and only a
> reader can tell that from the generated output this rule exists for. The
> claim is left exactly as the Refused and Failed cases leave it — no
> branch recorded, so the ordinary handback rules apply unchanged.


## `internal/loop/seam_markers.go`

### stopAtSeamMarker

> STOP and RESTART act at a VERIFIED PHASE SEAM as well as between iterations
> (CLA-879). Releasing a checkpointed claim at the seam lets the next claim
> resume at the following phase from `phasesDone`, so a stop or roll waits one
> phase session instead of a whole task sequence.
>
> A seam is only safe where the resume mechanism actually exists: a
> runner-claimed drain whose checkpoint is verified. PROJECT PAUSE is
> deliberately not read here — it gates new claims and lets the in-flight task
> finish every phase (decision 6019ea26-1464-43a3-a152-3271510b8b42). RELOAD,
> RESTART_NOW and HALT keep their existing boundaries.


## `internal/loop/task_worktree.go`

### taskworktree.go

> task_worktree.go — the runner prepares the task's worktree before the session
> starts (runner-owned mechanics A4, docs/runner-owned-mechanics.md).
>
> The session is handed a branch that is already checked out at a deterministic
> path, based on the PR base, and — when resuming — already merged with the
> current base where git can merge it. Everything mechanical that used to be a
> prompt instruction and a full-context turn happens here instead.
>
> Two rules the code below keeps:
>   - never discard work. A reused worktree that is detached, on another
>     branch, or holds tracked changes or commits origin lacks is REFUSED with
>     a reason. Untracked files are not committed work: when HEAD is at or
>     behind a ref origin has, they are moved aside to a logged directory
>     OUTSIDE the tree (nothing deleted) and the tree is reused. Only a
>     fast-forward is applied to a clean, matching tree.
>   - a conflict is not a failure. The merge is aborted, the conflicting paths
>     are returned, and the session starts clean at the pre-merge tip with the
>     conflict to resolve itself.
>
> Nothing here pushes or opens a PR; that is the close-out's job (B7).

### adoptBranchWorktree

> adoptBranchWorktree takes over the tree the task's branch is already checked
> out in, or refuses it by name. A tree under the task-worktree roots is this
> task's own; one elsewhere is adopted only when it holds nothing origin does
> not, because another cut of the same branch would otherwise be refused here
> and burn the task's claim. Getting here means this run holds the task's claim,
> so the previous holder's lease has ended, and nothing in the tree is moved
> until the check below proved it holds nothing origin lacks.

### taskWorktreeUnsafeToAdopt

> taskWorktreeUnsafeToAdopt names what a tree outside the task-worktree roots
> holds that origin does not, or "" when adopting it would discard nothing: a
> tree at or behind origin (or the PR base, for a branch never pushed) carries
> no unpushed work, and this run holds the task's claim. Untracked files are
> cleared by reuse, so they are unsafe only when HEAD could hold work origin
> lacks.

### reuseTaskWorktree

> reuseTaskWorktree takes over a worktree that is already registered for this
> repo. It only adopts one that is demonstrably the task's own tree; anything
> that could hold committed work is refused by name, because the alternative is
> discarding work. Untracked files cannot hold committed work once HEAD is
> proven to be at or behind origin: they are moved aside, never deleted, and
> the tree is reused.

### finishTaskWorktreePrep

> finishTaskWorktreePrep records the tip HEAD afterwards, plus where origin's
> branch is: a reused tree ahead of origin keeps its unpushed commits, so its
> Tip is not "the tip on origin" and the caller must be able to tell.
>
> BaseSHA is the commit a later review's `git diff` starts from. It is computed
> here, after any prep merge, because that is the only place the runner already
> has the prepared tree: a review that re-derived it could not know the tip the
> prep merged to. A missing merge-base (unrelated histories) leaves it empty and
> the briefs fall back to origin/<PRBase>, which computes the same thing itself.


## `internal/loop/task_worktree_claim.go`

### claimWorktreeRefusal

> claimWorktreeRefusal is the pre-claim, READ-ONLY answer for one task: the
> deterministic reason the task's existing worktree cannot be prepared for a
> claim, or "" when taking the claim is worth it. It moves, fetches-to-reset and
> prunes nothing, so a refusal here happens BEFORE the claim and costs no
> reclaim — the park that two refused claims produce is what this exists to
> prevent.
>
> Only what can be proven is refused. Everything judged against origin is
> decided after one fetch; a fresh tree at a PR base the request does not carry
> is deferred to the post-claim prep, which resolves that base itself. A check
> that cannot run is not a refusal: the prep repeats the same decision with the
> same facts, and a transient failure must not starve the task.
>
> The origin facts it fetched are returned so the post-claim prep can reuse them
> instead of paying a second round-trip.


## `internal/loop/trips.go`

### deadReason

> deadReason renders the finish reason a dead phase was classified on, for a log
> line. The classification fired on FinishReasonUnknown, so a nil claim — a
> session that observed no task — still has a reason to name.
>
> The adapter's OWN terminal_reason marker wins when present: ZeroUsageReason
> (CLA-398) names the quiet-death signature more precisely than the bare
> "unknown" it rides on, so the operator's log says which flavour of death this
> was instead of the driver re-inferring it from the zero usage downstream.

### outputCapStallReasoning

> outputCapStallReasoning reports whether this session ended on the CLA-584
> output-cap stall, and the stalled final step's own reasoning-token count when
> it did. The question is asked of the adapter's own terminal_reason marker —
> the same channel deadReason reads — never re-derived from the finish reason,
> so a harness that cannot produce the stall simply never answers true.
>
> It exists so the hand-back can say WHY a session stopped holding a task it
> never moved on: the MAK-123 post-mortem found only "the phase finished but
> never moved the task on", with nothing in the daemon log saying the model had
> spent its whole output budget thinking (reasoning=32000) and emitted nothing.

### openCallKill

> openCallKill reports that this session's kill landed while a tool call was
> open. In practice that is the TOOL CAP: the guard pauses the idle clock while a
> call is open precisely so the cap, not the idle clock, ends it, and an adapter
> reports the open call alongside whatever ended it. Only the adapter can see that
> window, so the predicate reads its verdict rather than re-deriving one from the
> cause string (CLA-900).

### fleetTrip

> fleetTrip pauses a target whose fleet dead-phase counter has reached
> fleetDeadBound (CLA-396): that many consecutive dead phases across tasks mean
> the harness or provider is broken right now, not the tasks, and every further
> session would pay the same death rate for nothing. The target stops spawning
> until the operator answers the raised project-level question (the open-question
> count falls back to what it was at the raise — see the poll gate in Run).
>
> A death that reached the bound while a call was open had a different
> observation: the last session was waiting inside one open tool call, not silent
> on a provider. The pause is the same, but the question and the log line name
> what was seen instead of asserting the provider broke (CLA-900).
>
> The question is PROJECT-level — ask_question with no taskId — because a fleet
> trip is not one task's triage: pinning it to whichever task happened to be in
> flight would make the operator answer about a bystander. It is filed as a
> non-blocking `decision` — there is no task to block (the loop's pause is the
> enforcement), and the operator's ruling on a fleet incident is a project-level
> judgment worth standing in the decision log, unlike the park's one-task
> clarification.
>
> "Exactly one" is guaranteed by fleetRaised: while the pause is in force the
> flag is set, so a later trip in the same episode does not re-raise. The flag
> clears with the pause, so a NEW episode — the operator answered, the loop
> resumed, and the fleet died again — raises a fresh question.
>
> If the question cannot be filed, the pause is NOT set: a pause the operator
> can never be told about and can never resume is a permanent stall, which is
> worse than the fall-through (per-task parking still bounds each task). The
> dead-phase counter stays at the bound, so the next dead phase tries again —
> loudly.

### startupTrip

> startupTrip pauses a target whose harness-startup counter has reached
> startupFailBound (CLA-562): that many consecutive spawns failing before any
> usage report means the harness or provider is broken right now, not the
> tasks, and every further spawn would pay the same sideline-and-retry for
> nothing. Same shape as fleetTrip: the target stops spawning until the
> operator answers the raised project-level question (the open-question count
> falls back to what it was at the raise - see the poll gate in Run).
>
> The question is PROJECT-level - ask_question with no taskId - because a
> startup trip is not one task's triage: pinning it to whichever drain happened
> to be in flight would make the operator answer about a bystander. It is filed
> as a non-blocking `decision` - there is no task to block (the loop's pause is
> the enforcement), and the ruling is project-level judgment for the decision
> log.
>
> The body carries the harness exit signature (the sideline cause), as the
> sideline log line does: "provider looks broken" without the error string
> sends the operator grep-hunting through state-dir logs.
>
> "Exactly one" is guaranteed by startupRaised, cleared with the pause so a new
> episode raises fresh. If the question cannot be filed, the pause is NOT set
> and the counter stays at the bound, so the next startup failure retries the
> raise - loudly - while the sideline backoff keeps bounding each attempt.

### parkDeadPhase

> parkDeadPhase parks the task after four consecutive dead phases, filing an
> OPEN question so the park reaches the operator instead of vanishing into the
> Done tab (CLA-395). The question is a non-blocking `clarification` — the task
> is already parked, so there is nothing left to block, and a `decision` would
> read as project-wide standing law rather than one task's triage. Its body and
> options are modelled on the plane's own escalateExhausted shape: what
> happened, the dead-phase signature, how many sessions it cost, where the
> iteration logs are, and options covering retry / re-scope / leave parked /
> drop (CLA-386, per the 2026-08-20 operator decision: retry the implement
> phase four times, then park; the budget is per task).
>
> The phase is dead — its sessions are gone — so nobody is left to declare the
> failure; the driver must. The claim was released before this ran (the retry
> re-claimed, then died again), so the park is a plain status write signed by
> the dead phase's own run.

### How the sessions died

> How the sessions died. A CLA-739 tool-loop kill is dead whatever it
> pushed, so "no branch recorded" may be false for it and the finish reason
> says nothing: the cause names what happened instead. The cause is the
> LAST attempt's, which is the one the operator is being asked about.
> CLA-877: a report-resume failure delivered its work too — only the reply
> that re-emits the done report kept failing — so it is named for what it
> is rather than as a session that produced nothing.

### parkStartRefusal

> parkStartRefusal parks a task whose worktree the runner refused on two
> consecutive claims, and reports whether the task IS parked: only a Park that
> took makes the claim safe to let go. The caller logs the outcome, because only
> it knows the refusal count and the cause. Unlike a dead phase the claim is LIVE
> here, so the park signs it with its own run and then lets it go: a dirty or
> diverged tree refuses every claim identically, so another claim buys nothing and
> only a human can clear it. The question is a non-blocking `clarification` for
> the same reason parkDeadPhase's is — the task is already parked, so there is
> nothing to block.


## `internal/loop/waits.go`

### sleepStall

> sleepStall reports how much real time a wait lost to system suspension, and
> whether that is worth saying out loud.
>
> The threshold is absolute rather than proportional on purpose: five minutes of
> unexplained wall clock is worth one log line whether it interrupted a one-minute
> idle poll or a thirty-minute limit wait, and a proportional rule would either
> spam the short waits or stay silent through the long ones. Small overshoots are
> ordinary scheduling and say nothing.

### waitPastBudget

> waitPastBudget reports whether waiting for resetAt would cost more time than
> the run has left. An unknown reset (zero) is waited out as before, because the
> supervised wait polls for an EARLY lift and the limit may clear long before any
> stated time; with no ceiling set there is nothing to be past.
>
> remaining comes from the breaker's own clock — MONOTONIC elapsed, which does
> not advance while the machine is suspended. The wait is measured on the wall
> clock, because a quota reset is a wall-clock event. Those are the right two
> clocks for this question and the mixture is deliberate.
>
> The earlier form compared the reset against a wall-clock DEADLINE while the
> breaker counted monotonic, so the two diverged by exactly the time the machine
> spent asleep — the very thing this file's sleep detection exists to report. An
> eight-hour run that lost five hours to a closed lid would stop with seven hours
> of budget unspent, and tell the operator it had reached a ceiling it had not.

### probeWait

> probeWait returns how long the paused loop sleeps before its next probe.
>
> A known reset still ahead of us is worth waiting for precisely: sleep until
> just past it (reset + grace) instead of to the next tick of the poll grid,
> so a cap that lifts on time costs one grace period of idleness rather than
> up to a whole interval (CLA-305: 22 minutes dead on 2026-08-10, work queued,
> nothing wrong). The cap at the interval stays: waitOrStop is what keeps a
> paused loop responsive to a STOP marker, and an aligned multi-hour sleep
> would make the stop switch feel dead - so the loop wakes on min(reset+grace,
> the grid) and simply laps again when it wakes short of the reset. An unknown
> or already-past reset falls back to the blind grid: codex states no reset at
> all, and after CLA-258 claude parses one only from typed sources, so an
> unknown reset is normal rather than exceptional.

### routeWakeHint

> routeWakeHint is how long a still-limited wait should sleep when a route the
> walk left blocked clears sooner than the poll interval: the soonest instant any
> of the phase's routes is outside BOTH its usage mark and its pricing window.
>
> The deadline after a wake used to be the interval measured from the last
> attempt, always. When the walk ended with every remaining route window-blocked,
> that overran the window's end by up to a full interval: on 2026-11-24 a daemon
> waiting on Synthetic stopped its 12:23-13:59 wakes on two routes that were
> inside the deepseek-peak window, then slept through 14:00 and probed again at
> 14:29, leaving a usable route idle for 29 minutes. The walk knew the window's
> end (the refusal carries it); the wait now uses it.
>
> A route's two blockers are not alternatives: the route is probeable only when
> BOTH are clear, so the per-route answer is the later of the two, and the wake
> is the earliest of those across the list. The same grace a stated-reset wake
> takes is added, for the same reason: a probe that lands a hair before the edge
> pays a whole interval for its timing. A route already clear returns no hint —
> there is no future instant to wake at — but that is NOT evidence the walk
> probed it: a mark can expire while a later route's probe is still running, and
> the walk then stops at tried[route] without re-probing it. supervisedWait
> covers that case through routeClearNow (CLA-988). When every route is marked
> with the fallback interval (no stated reset), the hint is at or past the
> interval and the caller keeps the interval unchanged.

### routeClearAt

> routeClearAt is the single per-route answer the wake hint and the clear-now
> check both read: the later of the route's usage mark and a withholding pricing
> window, or a time not after now when it is already usable. A "run" schedule
> never withholds its route (it offers it back at peak), so only its mark can
> hold it.

### routeClearNow

> routeClearNow reports whether one of the phase's routes is usable RIGHT NOW.
> It exists for a wake that ran out of routes exactly as a route cleared: the
> walk marked route A at its probe, A's mark expired while route B's probe was
> still running, and the walk then stopped at "already tried" for A without
> re-probing it. The hint only answers with a FUTURE instant, so without this
> check the wait fell back to the whole interval with A sitting idle (CLA-988).
> One grace out is the same precision a stated-reset wake takes; if the
> provider's reset was approximate and the probe lands early, it simply marks
> the route again.

### supervisedWait

> supervisedWait pauses on a usage limit, then polls for an early reset rather
> than sleeping blindly to the stated reset. Returns the probe spend it incurred,
> and stop=true if the loop should stop (STOP marker, context cancel, or a budget
> ceiling reached during the wait).
>
> sofar carries the run's start and the target index: the run's clock (which a
> local-file budget runs on) and whose ledger the probes are charged to. The
> breaker is consulted HERE and not only by the caller, because this loop does not
> return to the caller while the limit persists: it waits, probes, and waits again
> for as long as it takes. Without this, the one shape the task names — a drain
> stuck in a supervised wait — was the one shape no ceiling could end, and it is
> the shape codex always produces, since it states no reset for waitPastBudget to
> measure (CLA-258).
>
> The probes are themselves paid sessions, so each one is charged to the target's
> ledger by charge() as it lands, and returned to the caller for the iteration's
> accumulator. A breaker that can only be tripped by spend it can SEE was blind to
> exactly the spend this loop generates: a week-long cap polled every 30 minutes is
> ~336 unaccounted sessions, and this is the one loop with no other way out
> (CLA-287).
>
> While it waits it speaks: a `limited` presence beacon on entry and again at
> limitBeaconInterval while the pause lasts (CLA-728). A wait can be hours, and
> the plane calls an instance dead once its last beacon is stale, so silence
> here is what made a healthy daemon look crashed. Beacons cost nothing, and
> they ride their own ticker rather than the probe loop, so the two cadences are
> independent in both directions: a beacon never spends (probes are paid
> sessions, CLA-287), and a probe in flight — a harness session with no ceiling
> of its own — never silences the beacon.

### CLA-728

> CLA-728: the wait can outlast the plane's liveness window many times over,
> and a healthy daemon that stops talking reads on the console as dead -
> which is what sent the operator looking for a crash on 2026-10-20. Say
> `limited` on entry and keep saying it at limitBeaconInterval; the next
> ordinary beacon (`iteration` or `idle`) is what replaces it on resume, so
> there is no exit call.

### resetMu

> What the beacon carries is the harness's stated reset only while the wait
> still believes it. The fallback below measures a stated reset false, and
> from that moment the console must read "probing for a reset" rather than a
> countdown to an instant this loop already knows did not lift the limit.
> Mutex-guarded because the re-beacon goroutine reads it while this loop
> writes it.

### The re-beacon runs on its OWN goroutine, not as another wake in the pr

> The re-beacon runs on its OWN goroutine, not as another wake in the probe
> loop below. A probe is a harness session with no ceiling of its own — the
> session wall-clock cap exempts probes (CLA-368) — so a cadence driven by
> the same loop goes silent for the whole probe, and a probe longer than the
> plane's liveness window puts the instance back to `dead`: the exact
> failure this state exists to fix. The goroutine is joined before the wait
> returns, so the boundary's next ordinary beacon is still the last word.

### Probe time

> Probe time.
>
> A stated reset is the provider's claim, not a guarantee, so crossing it
> mid-wait does NOT resume the run the way a blind-grid wake used to: it
> falls back to interval polling, and the very next probe is what confirms
> or refutes the claim. Only a reset that was ALREADY behind us at entry
> resumes on the stated time alone - the legacy contract pinned by
> TestDrainWithRetries_StatedResetPassed, where the caller sent us here
> knowing the reset had passed and one more interval of idleness is the
> price of not re-probing a phase that just told us it was capped.

### The successor deadline is measured from HERE, the end of the attempt,

> The successor deadline is measured from HERE, the end of the attempt,
> and only here. Anchoring it at the wake instead would let a probe that
> outlives the poll interval consume the whole wait: the next paid
> session would start the instant this one returned, which is more
> probes than the pre-CLA-728 loop ever spawned (it slept the interval
> AFTER each probe) and, for a harness whose probe reports no spend, a
> cadence the CLA-287 breaker cannot see. The attempt counts as finished
> before its verdict is read, so an error path that retries is spaced
> exactly like a successful one.

### A probe whose own output could not be read is not a blip that clears

> A probe whose own output could not be read is not a blip that clears
> itself: it reports no verdict AND no spend, so a wait that keeps polling
> on it re-spawns a paid session every interval against a ceiling that can
> never see the cost — the one shape this loop's breaker exists to end
> (CLA-262, and the same reasoning as endUntrustedDrain). With no spend
> ceiling there is nothing to protect, so it waits on as before.

### probeWake

> probeWake runs one wake's probes (CLA-984). A phase with a route list walks
> it: a route that probes limited is marked exactly as a session limit marks it
> (the probe's stated reset, else routeMarkInterval), and the next unmarked
> routable route is probed in the same wake. Without the walk, a route with no
> stated reset was re-marked by nothing: its 30-minute fallback mark expired,
> it came back to first place, and every probe after that tested it alone, so
> a daemon sat "still limited" for hours while a later route was open
> (headless, 2026-11-23).
>
> The walk ends at the first unlimited probe (resume), at a budget ceiling
> (stop), or when the next route
> probeInvocation would aim at is already marked, already tried this wake, or
> window-blocked; at most one probe per route per wake. It also reports WHICH
> still-limited ending it reached — the walk exhausted, or a probe/spawn error —
> because only the first can leave a route clear NOW and earn the one-grace
> successor deadline; a probe that failed marks nothing, so waking at grace
> after one would re-spawn it in a tight loop (CLA-988). Otherwise the wait
> schedules its successor deadline and every probe is still charged to the
> ledger as it lands (CLA-287). The cost is at most one probe per route per
> wake: routes are few and a probe is minimal.
>
> A wake's first probe may land on a marked route: when every route is marked,
> probeInvocation aims at the one whose mark lifts first, as before. A
> plain-string model is a route list of one, so its probe marks it; a phase
> with no model at all probes and marks nothing, exactly as before. The budget
> ceiling is re-read before every walk probe after the first, since each is a
> paid session; STOP and HALT are read only between wakes, as a wake's probes
> are few and short.

### waitOrStop

> waitOrStop waits up to dur, but stays responsive to a STOP marker (consuming it)
> and to context cancellation. Reports whether the loop should stop.
>
> CLA-461: the restart/reload markers get the same responsiveness, so a control
> request is never stuck behind a multi-hour idle poll or a supervised limit
> wait. RESTART / RESTART_NOW return true (stop) with restartRequested set, so
> the caller's caller re-execs; RELOAD re-reads the config in place and lets the
> CURRENT wait run out on its existing schedule — ending a supervised limit
> wait early would turn a dial edit into an immediate paid probe against the
> very limit being waited out. A changed poll_interval applies from the NEXT
> pause, which recomputes it.

### waitOrStopHalt

> waitOrStopHalt is waitOrStop with one addition: a HALT marker ends the wait
> too (CLA-728).
>
> HALT is deliberately NOT consumed here — it is the operator's note, and the
> next run's start-up read reports it again without clearing it either. Ending
> the wait early therefore does not change where the run ends, only WHEN: the
> same halt, at most a poll interval sooner, which is what "STOP/HALT still end
> the wait promptly" asks for a pause that can last hours.
>
> It is a separate entry point rather than a widening of waitOrStop because the
> idle polls are not this wait: widening those is a change to every wait in the
> loop, and this task is about the limit pause.

### Round(0) strips the monotonic reading, so wallStart measures REAL elap

> Round(0) strips the monotonic reading, so wallStart measures REAL elapsed
> time. Everything else here runs on the monotonic clock, which does not
> advance while the machine is suspended — so comparing the two is the only
> way a slept-through wait becomes visible. Without it, a laptop that idle
> sleeps turns a 30-minute pause into a multi-hour silence that looks exactly
> like a hung process.

### consumeWaitControls

> consumeWaitControls reads and consumes the control markers waitOrStop honours:
> STOP, the restart family, and RELOAD (which is APPLIED here, in place, as
> control.go describes). It reports whether the wait must stop, which is true
> for STOP and either restart marker.
>
> haltEndsWait asks for HALT to end the wait as well (waitOrStopHalt, the
> supervised limit pause). HALT is READ but never consumed: it is the operator's
> note, so it is still there for the next run's start-up read.
>
> The caller MUST hold d.controlMu: the read-then-remove of a marker is not
> atomic, and a parallel panel's members can be in this wait at the same time
> (CLA-697 review finding 1.review-1.2). The lock is what makes a marker a
> single-consumer switch no matter how many waiters are parked on it.

### readMarker

> readMarker reports whether a control marker file exists, and its first line.
>
> A marker is the operator's switch, so it is read through the state dir handle:
> a symlink at the name is refused rather than followed, and the read is capped.
> Following one would let whoever planted it choose a file for us to open and a
> line of it to echo into the daemon's log.

### randomTail

> randomTail is the unguessable part of an iteration log's name. crypto/rand,
> not math/rand: the whole point is that the session whose transcript this is
> cannot predict it, and a seeded PRNG in a daemon it can watch start is not
> unpredictable. rand.Read never fails on any supported platform (it panics
> internally rather than returning an error), so there is no fallback branch to
> get wrong.

