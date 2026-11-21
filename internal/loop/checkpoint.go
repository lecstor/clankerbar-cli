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

// checkpointEvidence reports whether the plane's record carries the phase's exit evidence, and
// returns the branch that evidence names (empty for the branch-less forms). A branch that was never
// pushed is not evidence, and a check that could not run (no git, no repository resolved) is not
// evidence either: a checkpoint is claimed only on the verified present, never on the absence of a
// negative.
func (d *Driver) checkpointEvidence(ctx context.Context, t Target, res harness.Result, workdir string, review *reviewGate) (branch, tip string, ok bool, unverified bool, why string) {
	branches := recordedBranches(res)

	// Form (a): the recorded-branch check. Remember the first failure for the diagnostic; fall through
	// to the plane-record forms when nothing here passes.
	firstB, firstWhy := "", ""
	// The first recorded branch whose check returned Unknown, kept apart from the Fail diagnostic: it
	// is the branch an unverified checkpoint carries and the detail its log names.
	unknownB, unknownWhy, unknownSeen := "", "", false
	// The branch form (a) accepted, and the origin tip the accepting check read: the tip rides the
	// checkpoint as the revision the review phase verifies and records, and the seam matches the
	// record's reviewedTip against it.
	foundBranch, foundTip, foundOK := "", "", false
	if len(branches) > 0 {
		if d.newVerifier == nil || workdir == "" {
			firstB, firstWhy = branches[0], fmt.Sprintf("branch %q recorded but could not be checked (no working tree)", branches[0])
		} else {
			v := d.newVerifier(workdir, d.cfg.AllowUncheckedPRFor(t.Name))
			vctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryCheckTimeout)
			defer cancel()
			// ANY recorded branch that passes is evidence — a session may record a draft branch early and
			// the real one later.
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
					// Fail and Unknown both mean "not evidence" for THIS branch: an unreachable branch, or one we
					// could not check.
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

	// Form (a) short-circuits for every phase that does not need the plane's record: a verified branch
	// is the strongest evidence there is, and asking the plane anyway was never the contract.
	if foundOK && !review.required {
		return foundBranch, foundTip, true, false, ""
	}

	// Forms (b) and (c): the plane's own record of the task, read ONCE here so the review-record gate
	// below can use it without a second round-trip.
	record, recordRead, recordOK, recordWhy := d.planeExitRecord(ctx, t, res)

	// Resolve the pre-gate verdict from the strongest form that held.
	switch {
	case foundOK:
		branch, tip, ok, unverified = foundBranch, foundTip, true, false
	case recordOK:
		branch, tip, ok, unverified = "", "", true, false
	case unknownSeen:
		// CLA-576: the origin could not be READ for at least one recorded branch, and no stronger form
		// held.
		if unknownWhy == "" {
			unknownWhy = "the branch check returned unknown without a detail"
		}
		branch, tip, ok, unverified, why = unknownB, "", true, true, unknownWhy
	}

	if ok {
		// The review-shaped gate: a phase whose ONLY deliverable is the review record must not checkpoint
		// on the implementer's branch alone.
		if review.required {
			if !recordRead {
				if branch == "" && !unverified {
					return branch, tip, false, false, fmt.Sprintf("the task could not be read after the review phase ended, so its %q block cannot be verified — the next phase would have nothing to apply", config.ReviewRecordHeading)
				}
				return branch, tip, false, false, "the task's review records could not be read after the review phase ended, so a durable review record cannot be verified — the next phase would risk running on a record that does not exist"
			}
			// CLA-634 decision (question 03a12578, option 1): a BRANCHLESS (settled-task) review-only
			// checkpoint cannot write the durable record — the plane accepts review writes only from the
			// task's live holder, and no run holds a settled task — so this variant keeps the legacy `##
			// Review` block gate: changed since the spawn, a heading on its own line, and a LAST block the
			// spawn-time baseline did not carry.
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

	// No form held. Compose the diagnostic from whichever legs ran: the plane's answer when it gave
	// one (prefixed by the branch leg's failure when both ran), else the legacy branch-only reasons,
	// unchanged.
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

// planeExitRecord reads the plane's record of the claimed task for the two branch-independent
// exit-evidence forms: the task having LEFT `ready` (in_review / done / parked / blocked — its
// own session settled it), or a DECLARED no-code delivery (the plane's first-class "this shipped no
// code"). Best-effort like every plane read: ok is false with an EMPTY why whenever the read cannot
// answer at all (no claimed task id, no wired reader, a plane blip), and the gate then judges on
// branch evidence alone, byte-for-byte as before this read existed.
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
		// Form (b): the task LEFT `ready`. Its session settled it on the plane, which for a no-code task
		// is the strongest statement there is.
		return st, read, true, ""
	}
	if st.DeliveryNoCode {
		// Form (c): a declared no-code delivery — the plane's own record that this task's correct
		// delivery is no code, held lease or not.
		return st, read, true, ""
	}
	return st, read, false, fmt.Sprintf("the plane's record shows no exit evidence either (status %q, no no-code delivery)", st.Status)
}

// readTaskOutcome reads the claimed task's outcome from the plane, best-effort: a failure returns
// read=false and the caller decides what to do without it.
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

// planeSeamReadAttempts bounds the retries of a plane read whose failure fails the sequence CLOSED:
// the initial attempt plus two more. Deliberately no sleep between attempts: the read's own timeout
// is the pacing, and the whole point of a bounded retry here is to avoid paying for a session (or
// discarding one already paid for), not to wait for a sick plane.
const planeSeamReadAttempts = 3

// planeTaskState reads the plane's record of one task, retrying a transient failure up to
// `attempts` times. ok is false when there is no task id, no wired reader, or every attempt failed;
// ErrNotWired is never retried, because asking an unwired client again cannot wire it.
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

// readReviewBaseline reads the task's outcome as it stands BEFORE the review phase spawns, so the
// seam can require the record to have changed. A failure still returns read=false, and the caller
// refuses to spawn the review rather than letting it run a session whose checkpoint the seam would
// reject.
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

// closeReviewRound releases a review round's sealed findings as the holding run:
// record_review({closeRound: n}). The round is the one the seam gate MATCHED, never a number
// predicted at spawn time: the plane assigns rounds, and the driver reads them back off the record
// — which is also what keeps this correct across void rounds and panel members.
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

// readTaskState reads the plane's record of one task, for the verification round's BEST-EFFORT
// reads: the fix phase's panel digest and its open-MUST digest, where a failure degrades to "judge
// on other evidence" — the fix brief's own instruction to read `reviews.openMustIds` itself.
// Best-effort like every other plane read: ok is false when there is no task id, no wired reader,
// or the read fails.
func (d *Driver) readTaskState(ctx context.Context, t Target, taskID string) (plane.TaskState, bool) {
	return d.planeTaskState(ctx, t, taskID, 1, "for the verification round")
}

// readTaskStateRetry is readTaskState for the verification round's FAIL-CLOSED reads: the round's
// spawn read, and the read the driver makes after a closed round to judge its open-MUST set. A
// failure on any of them fail-closes the sequence — the round cannot be told what to adjudicate,
// or cannot be judged — so one plane blip there discards work that has already been paid for: at
// the spawn, the fix round that just ran (the sequence ends, the claim is released and the next
// claim re-runs the phases); after a closed round, the verification round itself.
func (d *Driver) readTaskStateRetry(ctx context.Context, t Target, taskID, purpose string) (plane.TaskState, bool) {
	return d.planeTaskState(ctx, t, taskID, planeSeamReadAttempts, purpose)
}
