package loop

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// checkpointEvidence reports whether the plane's record carries the phase's
// exit evidence, and returns the branch that evidence names (empty for the
// branch-less forms). Held-ness alone is NOT the evidence: since CLA-358 the
// driver's lease renewer renews a claim for a session's whole lifetime, so ANY
// session that claimed ends held, and "still holding" cannot distinguish a
// checkpoint from an empty exit (2026-08-24 fleet incident: two implement
// sessions claimed, cut a worktree, hit a silent model EOT, exited cleanly
// with real usage and no edits, and the seam advanced to review on the false
// premise).
//
// The evidence is ANY of three forms (CLA-497) — "the plane's record shows
// this phase did its job":
//
//   - (a) a branch recorded by this run whose tip really exists on the origin
//     remote — today's check, unchanged. A branch that was never pushed is not
//     evidence, and a check that could not run (no git, no repository
//     resolved) is not evidence either: a checkpoint is claimed only on the
//     verified present, never on the absence of a negative. That is the
//     deliberate difference from verifyDeliveries, which warns loudly on the
//     same failing check but does not gate anything on it.
//   - (b) the task having LEFT `ready` (in_review / done / parked / blocked):
//     its own session settled it on the plane. A no-code task can never
//     produce form (a) — there is no branch to record — and without this form
//     it is structurally incapable of checkpointing: the EZY-290 shape (a
//     one-token task-body edit) burned nine sessions in a claim -> empty-exit
//     -> release loop before this existed.
//   - (c) a DECLARED no-code delivery on the task: the same delivery.noCode
//     flag the terminal tools require to close such a task, read off the plane's
//     record. It counts whether or not the session still holds the lease.
//
// Forms (b) and (c) are read through the SAME single-task get_task read
// (planeExitRecord below), not a second implementation. The bounds
// CLA-457 set stay absolute: held-ness alone is still nothing (status
// in_progress proves only that someone holds the lease), and a session that
// claimed, recorded no branch, and left the task sitting in `ready` is STILL
// an empty exit — "nothing happened" — whatever this function would have said
// about its manners.
//
// CLA-576: an Unknown from form (a) is the ORIGIN being unreadable, not the
// branch being absent, and it is returned as its own outcome (unverified=true,
// ok=true) rather than flattened into the refusal. The phase is an UNVERIFIED
// checkpoint: the review phase spawns, carrying the recorded branch, and is
// told to verify the hand-off itself. A Fail — the origin was read and the
// branch is not there — keeps today's refusal exactly. The accepted cost is a
// narrow loosening of CLA-457: a session that recorded a branch it never
// pushed, during a remote outage, reaches a review phase, which is the
// backstop.
func (d *Driver) checkpointEvidence(ctx context.Context, t Target, res harness.Result, workdir string, review *reviewGate) (branch, tip string, ok bool, unverified bool, why string) {
	branches := recordedBranches(res)

	// Form (a): the recorded-branch check. Remember the first failure for the
	// diagnostic; fall through to the plane-record forms when nothing here
	// passes.
	firstB, firstWhy := "", ""
	// The first recorded branch whose check returned Unknown, kept apart from
	// the Fail diagnostic: it is the branch an unverified checkpoint carries
	// and the detail its log names (CLA-576). The bool is the discriminator, not
	// the detail string: an Unknown check with an empty Detail is still an
	// unreadable origin.
	unknownB, unknownWhy, unknownSeen := "", "", false
	// The branch form (a) accepted, and the origin tip the accepting check read
	// (CLA-637): the tip rides the checkpoint as the revision the review phase
	// verifies and records, and the seam matches the record's reviewedTip
	// against it. Recorded rather than returned on the spot: the review-record
	// gate below needs the plane read FIRST, and both the read and the gate are
	// one decision.
	foundBranch, foundTip, foundOK := "", "", false
	if len(branches) > 0 {
		if d.newVerifier == nil || workdir == "" {
			firstB, firstWhy = branches[0], fmt.Sprintf("branch %q recorded but could not be checked (no working tree)", branches[0])
		} else {
			v := d.newVerifier(workdir, d.cfg.AllowUncheckedPRFor(t.Name))
			vctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryCheckTimeout)
			defer cancel()
			// ANY recorded branch that passes is evidence — a session may record a draft
			// branch early and the real one later. Remember the first failure for the
			// diagnostic, keep going, and only give up when no recorded branch could be
			// verified.
		branchScan:
			for _, b := range branches {
				out := v.Verify(vctx, delivery.Claim{Label: claimLabel(res.Claim), Branch: b})
				for _, c := range out.Checks {
					if c.Kind != delivery.BranchPushed {
						continue
					}
					if c.Status == delivery.Pass {
						foundBranch, foundTip, foundOK = b, c.Tip, true
						break branchScan
					}
					// Fail and Unknown both mean "not evidence" for THIS branch: an
					// unreachable branch, or one we could not check. Keep scanning; a
					// later recorded branch may be the one that is really there.
					if c.Status == delivery.Unknown && !unknownSeen {
						unknownB, unknownWhy, unknownSeen = b, c.Detail, true
					}
					if firstWhy == "" {
						firstB, firstWhy = b, c.Detail
					}
					break
				}
			}
		}
	}

	// Form (a) short-circuits for every phase that does not need the plane's
	// record: a verified branch is the strongest evidence there is, and asking
	// the plane anyway was never the contract. A review-shaped phase is the one
	// exception, because its checkpoint additionally needs the durable review
	// record (below) and its freshness baseline (reviewGate), so it pays the
	// extra reads.
	if foundOK && !review.required {
		return foundBranch, foundTip, true, false, ""
	}

	// Forms (b) and (c): the plane's own record of the task, read ONCE here so the
	// review-record gate below can use it without a second round-trip.
	record, recordRead, recordOK, recordWhy := d.planeExitRecord(ctx, t, res)

	// Resolve the pre-gate verdict from the strongest form that held. Form (a)
	// outranks the plane's record: a verified branch is the implementer's own
	// committed work, where a task that left `ready` may have been settled by
	// another channel.
	switch {
	case foundOK:
		branch, tip, ok, unverified = foundBranch, foundTip, true, false
	case recordOK:
		branch, tip, ok, unverified = "", "", true, false
	case unknownSeen:
		// CLA-576: the origin could not be READ for at least one recorded branch,
		// and no stronger form held. That is an unverified checkpoint, not an empty
		// exit. The unverified form is checked BEFORE the Fail-only diagnostic
		// because a branch absent from a readable origin and a branch whose origin
		// could not be read at all are different facts, and only the second one is a
		// checkpoint. The log line this feeds is the operator's whole account of why
		// the checkpoint is unverified, so an Unknown carrying no detail still gets a
		// sentence: an empty parenthetical would say nothing at exactly the moment
		// the log matters most. The bool, not this string, is the discriminator.
		if unknownWhy == "" {
			unknownWhy = "the branch check returned unknown without a detail"
		}
		branch, tip, ok, unverified, why = unknownB, "", true, true, unknownWhy
	}

	if ok {
		// The review-shaped gate (CLA-split; durable since CLA-637): a phase
		// whose ONLY deliverable is the review record must not checkpoint on the
		// implementer's branch alone. Its
		// inherited WIP (the branch the implement phase recorded) satisfies every
		// form above, so without this gate a review session that burned its turns,
		// hit its ceiling, or died on a silent EOT — having recorded nothing —
		// would still advance, and the next phase would be told to apply findings
		// that do not exist.
		//
		// The record must be THIS run's record, under THIS phase's reviewer
		// role (B1). Presence alone is not enough and was the shipped bug: a
		// task that already went implement -> review -> fix carries the
		// previous trip's `## Review` block in the outcome (and, since
		// CLA-636, its durable record too), and request_changes (or a takeover)
		// returns the task without clearing either. The run id is the freshness
		// key the plane embedded in the record and the criterion no stale
		// artefact can fake: a re-trip or a takeover is a NEW run, so only a
		// record written after this phase spawned can match. The reviewed tip
		// is checked against the tip the driver verified for the branch (or
		// demanded non-empty when the origin could not be read), so the fix
		// phase is only ever spawned on the revision that is still the tip.
		//
		// Fail-closed, like every other evidence form: a record that cannot be
		// read — or cannot be found — is not a review that happened. The cost
		// is a re-run of the review; the alternative is an unreviewed PR. An
		// unread BASELINE never reaches this gate: an unreadable baseline is
		// refused before the review spawns (CLA-663), so this gate judges
		// only tasks both reads could see.
		if review.required {
			if !recordRead {
				if branch == "" && !unverified {
					return branch, tip, false, false, fmt.Sprintf("the task could not be read after the review phase ended, so its %q block cannot be verified — the next phase would have nothing to apply", config.ReviewRecordHeading)
				}
				return branch, tip, false, false, "the task's review records could not be read after the review phase ended, so a durable review record cannot be verified — the next phase would risk running on a record that does not exist"
			}
			// CLA-634 decision (question 03a12578, option 1): a BRANCHLESS
			// (settled-task) review-only checkpoint cannot write the durable
			// record — the plane accepts review writes only from the task's
			// live holder, and no run holds a settled task — so this variant
			// keeps the legacy `## Review` block gate: changed since the spawn,
			// a heading on its own line, and a LAST block the spawn-time
			// baseline did not carry. Every branch-carrying variant still gates
			// on the record.
			if branch == "" && !unverified {
				switch {
				case record.Outcome == review.baseline:
					return branch, tip, false, false, fmt.Sprintf("the task's outcome is unchanged since the review phase spawned — the review recorded no new %q block, so the next phase would have nothing to apply", config.ReviewRecordHeading)
				case !reviewRecordLine(record.Outcome):
					return branch, tip, false, false, fmt.Sprintf("the review recorded no %q block in the task's outcome — the next phase would have nothing to apply", config.ReviewRecordHeading)
				case strings.Contains(review.baseline, lastReviewBlock(record.Outcome)):
					return branch, tip, false, false, fmt.Sprintf("the task's LAST %q block already appears in the outcome the review phase found — the review wrote no NEW block this trip, so the next phase would apply an earlier round's record", config.ReviewRecordHeading)
				}
				return branch, tip, true, false, ""
			}
			rec, matched := matchingReviewRecord(record.Reviews, review.role, res.Claim.RunID, tip, unverified)
			if !matched {
				expect := fmt.Sprintf("for reviewer role %q written by this run (%s)", review.role, runUUID(res.Claim.RunID))
				switch {
				case tip != "":
					expect += " with reviewed tip " + tip
				case unverified:
					expect += " that names a reviewed tip (the driver could not verify the origin remote, so the record must name the revision itself)"
				}
				detail := ""
				if record.Outcome == review.baseline {
					detail = " — the task's outcome is also unchanged since the phase spawned"
				}
				return branch, tip, false, false, "the task carries no durable review record " + expect + ", so the next phase would have nothing to apply" + detail
			}
			review.round = rec.Round
			review.state = rec.State
		}
		return branch, tip, true, unverified, why
	}

	// No form held. Compose the diagnostic from whichever legs ran: the plane's
	// answer when it gave one (prefixed by the branch leg's failure when both
	// ran), else the legacy branch-only reasons, unchanged.
	switch {
	case firstWhy != "" && recordWhy != "":
		return firstB, "", false, false, firstWhy + "; " + recordWhy
	case recordWhy != "":
		return "", "", false, false, recordWhy
	case firstWhy != "":
		return firstB, "", false, false, firstWhy
	case len(branches) > 0:
		return "", "", false, false, "none of the recorded branches could be verified on the origin remote"
	default:
		return "", "", false, false, "no branch recorded on the task"
	}
}

// planeExitRecord reads the plane's record of the claimed task for the two
// branch-independent exit-evidence forms (CLA-497): the task having LEFT
// `ready` (in_review / done / parked / blocked — its own session settled it),
// or a DECLARED no-code delivery (the plane's first-class "this shipped no
// code"). It is the SAME single-task get_task read that supplies the seam's
// exit evidence — plane.TaskStateSource — not a second implementation of the
// read.
//
// Best-effort like every plane read: ok is false with an EMPTY why whenever
// the read cannot answer at all (no claimed task id, no wired reader, a plane
// blip), and the gate then judges on branch evidence alone, byte-for-byte as
// before this read existed. A read that ANSWERS but shows no evidence returns
// the reason the gate will log — the difference between "nothing happened"
// (task still ready or merely held) and a read that never ran.
//
// The bounds are load-bearing: status "in_progress" proves only that SOMEONE
// holds the lease — held-ness alone was CLA-457's original false positive and
// stays worthless here — and a task still sitting in `ready` is still the
// empty exit, however orderly the session that said nothing about it.
func (d *Driver) planeExitRecord(ctx context.Context, t Target, res harness.Result) (st plane.TaskState, read bool, ok bool, why string) {
	taskID := res.Claim.TaskID
	src, wired := t.Releaser.(plane.TaskStateSource)
	if taskID == "" || !wired {
		return plane.TaskState{}, false, false, ""
	}
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	st, err := src.TaskState(sctx, taskID)
	switch {
	case err == nil:
		read = true
	case errors.Is(err, plane.ErrNotWired):
		return plane.TaskState{}, false, false, ""
	default:
		log.Printf("%scould not read the plane's record of %s for phase exit evidence (%v) — judging the exit on branch evidence alone", labelOf(t), taskID, err)
		return plane.TaskState{}, false, false, ""
	}
	switch st.Status {
	case "in_review", "done", "parked", "blocked":
		// Form (b): the task LEFT `ready`. Its session settled it on the plane,
		// which for a no-code task is the strongest statement there is.
		return st, read, true, ""
	}
	if st.DeliveryNoCode {
		// Form (c): a declared no-code delivery — the plane's own record that
		// this task's correct delivery is no code, held lease or not.
		return st, read, true, ""
	}
	return st, read, false, fmt.Sprintf("the plane's record shows no exit evidence either (status %q, no no-code delivery)", st.Status)
}

// readTaskOutcome reads the claimed task's outcome from the plane, best-effort:
// a failure returns read=false and the caller decides what to do without it.
// purpose and consequence compose the log line, because two callers read the
// same field for different reasons — the review seam's freshness baseline, and
// the fix phase's structural grading (CLA-635) — and a shared message would
// misname one of them.
func (d *Driver) readTaskOutcome(ctx context.Context, t Target, prev *harness.Result, purpose, consequence string) (string, bool) {
	if prev == nil || prev.Claim.TaskID == "" {
		return "", false
	}
	src, wired := t.Releaser.(plane.TaskStateSource)
	if !wired {
		return "", false
	}
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	st, err := src.TaskState(sctx, prev.Claim.TaskID)
	if err != nil {
		if !errors.Is(err, plane.ErrNotWired) {
			log.Printf("%scould not read the plane's record of %s %s (%v) — %s", labelOf(t), prev.Claim.TaskID, purpose, err, consequence)
		}
		return "", false
	}
	return st.Outcome, true
}

// planeSeamReadAttempts bounds the retries of a plane read whose failure fails
// the sequence CLOSED (CLA-663 for the review baseline, CLA-686 D3 for the
// verification round's spawn read): the initial attempt plus two more. The
// failure this recovers from is a plane blip — a dropped response, a read that
// timed out — and one retry pair around a 20-second read is enough for that
// without turning an outage into a long stall. Deliberately no sleep between
// attempts: the read's own timeout is the pacing, and the whole point of a
// bounded retry here is to avoid paying for a session (or discarding one
// already paid for), not to wait for a sick plane.
const planeSeamReadAttempts = 3

// planeTaskState reads the plane's record of one task, retrying a transient
// failure up to `attempts` times. It is the single implementation behind every
// seam read of the task — the review baseline and the verification round's
// fail-closed reads share it precisely so their retry behaviour cannot drift
// apart (CLA-686 D3: the verify spawn's read claimed to mirror the baseline's
// and did not, one attempt against three).
//
// ok is false when there is no task id, no wired reader, or every attempt
// failed; ErrNotWired is never retried, because asking an unwired client again
// cannot wire it. `purpose` names the read in the log line only; the caller
// decides what an unreadable task MEANS.
func (d *Driver) planeTaskState(ctx context.Context, t Target, taskID string, attempts int, purpose string) (plane.TaskState, bool) {
	if taskID == "" {
		return plane.TaskState{}, false
	}
	src, wired := t.Releaser.(plane.TaskStateSource)
	if !wired {
		return plane.TaskState{}, false
	}
	lastErr := ""
	for attempt := 1; attempt <= attempts; attempt++ {
		if ctx.Err() != nil {
			return plane.TaskState{}, false
		}
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		st, err := src.TaskState(sctx, taskID)
		cancel()
		switch {
		case err == nil:
			return st, true
		case errors.Is(err, plane.ErrNotWired):
			// No wiring to retry against; the caller refuses at its own guard.
			return plane.TaskState{}, false
		default:
			lastErr = err.Error()
		}
	}
	if attempts > 1 {
		log.Printf("%scould not read the plane's record of %s %s, after %d attempts (%s)", labelOf(t), taskID, purpose, attempts, lastErr)
	} else {
		log.Printf("%scould not read the plane's record of %s %s (%s)", labelOf(t), taskID, purpose, lastErr)
	}
	return plane.TaskState{}, false
}

// readReviewBaseline reads the task's outcome as it stands BEFORE the review
// phase spawns, so the seam can require the record to have changed. The task id
// comes from the carried claim (the predecessor phase's), which is exact for a
// resumed phase and is why this runs inside drainPhase, where `prev` is still
// in hand.
//
// Best-effort like every other plane read, with one difference CLA-663 added:
// the read is RETRIED up to planeSeamReadAttempts times, because a blip here
// does not degrade to "judge on other evidence" — it fails the whole sequence
// closed. A failure still returns read=false, and the caller refuses to spawn
// the review rather than letting it run a session whose checkpoint the seam
// would reject.
func (d *Driver) readReviewBaseline(ctx context.Context, t Target, prev *harness.Result) (string, bool) {
	if prev == nil {
		return "", false
	}
	st, ok := d.planeTaskState(ctx, t, prev.Claim.TaskID, planeSeamReadAttempts, "before the review phase spawned")
	if !ok {
		return "", false
	}
	return st.Outcome, true
}

// closeReviewRound releases a review round's sealed findings (CLA-637) as the
// holding run: record_review({closeRound: n}). The round is
// the one the seam gate MATCHED, never a number predicted at spawn time: the
// plane assigns rounds, and the driver reads them back off the record — which
// is also what keeps this correct across void rounds and panel members.
//
// The close is deliberately a no-op-with-an-error for a client that cannot
// close (an unwired plane, a test double): the caller turns this into a refused
// checkpoint, because a fix phase spawned against findings the plane still
// holds sealed is a session spawned to apply nothing.
func (d *Driver) closeReviewRound(ctx context.Context, t Target, res harness.Result, round int) error {
	closer, ok := t.Releaser.(plane.ReviewRoundCloser)
	if !ok {
		return errors.New("the plane client does not support closing review rounds")
	}
	if res.Claim.TaskID == "" || res.Claim.RunID == "" {
		return fmt.Errorf("no held claim to close review round %d with", round)
	}
	return closer.CloseReviewRound(ctx, res.Claim.TaskID, res.Claim.RunID, round)
}

// readTaskState reads the plane's record of one task, for the verification
// round's BEST-EFFORT reads (CLA-638): the fix phase's panel digest and its
// open-MUST digest, where a failure degrades to "judge on other evidence" — the
// fix brief's own instruction to read `reviews.openMustIds` itself. Best-effort
// like every other plane read: ok is false when there is no task id, no wired
// reader, or the read fails. The reads that fail the sequence CLOSED instead use
// readTaskStateRetry, which shares this implementation with a retry bound.
func (d *Driver) readTaskState(ctx context.Context, t Target, taskID string) (plane.TaskState, bool) {
	return d.planeTaskState(ctx, t, taskID, 1, "for the verification round")
}

// readTaskStateRetry is readTaskState for the verification round's FAIL-CLOSED
// reads (CLA-686 D3): the round's spawn read, and the read the driver makes
// after a closed round to judge its open-MUST set. A failure on any of them
// fail-closes the sequence — the round cannot be told what to adjudicate, or
// cannot be judged — so one plane blip there discards work that has already
// been paid for: at the spawn, the fix round that just ran (the sequence ends,
// the claim is released and the next claim re-runs the phases); after a closed
// round, the verification round itself. That is exactly why the review baseline
// read these claim parity with retries, and they now share its bound
// (planeSeamReadAttempts) rather than taking a single attempt.
func (d *Driver) readTaskStateRetry(ctx context.Context, t Target, taskID, purpose string) (plane.TaskState, bool) {
	return d.planeTaskState(ctx, t, taskID, planeSeamReadAttempts, purpose)
}
