package loop

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// claimLabel is the task's most human-readable identifier.
func claimLabel(c harness.Claim) string {
	if c.Ref != "" {
		return c.Ref
	}
	return c.TaskID
}

// releaseHeldClaim hands a task the session was still holding back to the queue, instead of leaving
// its lease to run out with nobody heartbeating it. A usage limit, a transient blip, an outright
// failure and a clean finish with the work unfinished all leave the same dead lease behind — and
// the usage-limit case is the worst of the four, because the driver then goes to sleep for as long
// as the reset takes.
func (d *Driver) releaseCarried(ctx context.Context, t Target, carried *harness.Result) *harness.Result {
	if carried == nil {
		return nil
	}
	d.releaseHeldClaim(ctx, t, *carried, false, false)
	return nil
}

// releaseCarriedEnd is releaseCarried for a phase about to loop back to a FRESH session — a
// usage-limit wait, a transient backoff. usageLimit names the cause on the lease end, which is
// how the plane spares the reclaim budget for the wait that follows.
func (d *Driver) releaseCarriedEnd(ctx context.Context, t Target, end phaseEnd, usageLimit bool) phaseEnd {
	// Only a hold that actually happened needs undoing. Every other end has ALREADY been through the
	// handback in drainPhase's seam block, and releasing again would hand the task back twice —
	// which on a usage-limit exit is the common path, not a corner: the seam releases (the phase did
	// not reach its checkpoint), and then the limit branch loops back to wait.
	if !end.checkpoint || end.claim == nil {
		return end
	}
	// Read BEFORE the call, for the same reason drainPhase reads it: a zero or settled claim releases
	// nothing, and reporting it as released would erase a live claim upstream.
	held := end.claim.Claim.Held()
	d.releaseHeldClaim(ctx, t, *end.claim, false, usageLimit)
	// The session rides the reconstruction: it is record data for the fleet iteration record, not a
	// hold to undo, and the pass it names really did spawn.
	return phaseEnd{released: held, session: end.session}
}

// countUndeclared says whether THIS call site is the one where a held-with-work claim means the
// CLA-384 failure: a phase that finished cleanly and did not hand the task over. Only the seam's
// final-phase handback passes true.
func (d *Driver) releaseHeldClaim(ctx context.Context, t Target, res harness.Result, countUndeclared, usageLimit bool) bool {
	_, queued := d.handBackClaim(ctx, t, res, countUndeclared, usageLimit)
	return queued
}

// handBackClaim is releaseHeldClaim with the two outcomes split: handed says the claim was actually
// let go (back in the ready queue, or its lease ended on pushed work), queued says it specifically
// returned to the ready queue. Only the phase seam reads handed — it must drop the drain's
// carried claim when the handback landed, but keep it for the deferred retry when it did not, and
// releaseHeldClaim's bool cannot tell those apart on the pushed-work path. usageLimit rides from the
// exit classification, because the lease end runs BEFORE handleUsageLimit names the cause.
func (d *Driver) handBackClaim(ctx context.Context, t Target, res harness.Result, countUndeclared, usageLimit bool) (handed, queued bool) {
	// A stream read only in part tells us which calls we SAW, never which the session made: the settle
	// that released the task may simply be in the bytes that never arrived.
	if res.Untrusted != "" {
		if res.Claim.Held() {
			log.Printf("%ssession appeared to end holding %s, but its output could not be read whole — leaving the lease to expire rather than releasing a task that may already be settled",
				labelOf(t), res.Claim.TaskID)
		}
		return false, false
	}
	if !res.Claim.Held() {
		// Nothing is held, so nothing is owed: the handback is complete.
		return true, false
	}
	if !res.Claim.Releasable() {
		// CLA-708: the claim has pushed work, so ReleaseDriver is forbidden — it would post `ready`
		// over the branch and throw away the takeover hand-off.
		stallReason, stalled := outputCapStallReasoning(res)
		ended, reasonSent := d.expireHeldClaim(ctx, t, res, usageLimit)
		if countUndeclared {
			d.undeclared++
		}
		reasonNote := ""
		if reasonSent {
			reasonNote = "; usage limit, reclaim budget not charged"
		}
		switch {
		case ended && stalled && countUndeclared:
			log.Printf("%sstalled: output cap hit with no output (reasoning=%d) — ended the lease on %s (pushed work kept for takeover)%s (undeclared hand-offs this run: %d)",
				labelOf(t), stallReason, claimLabel(res.Claim), reasonNote, d.undeclared)
		case ended && stalled:
			log.Printf("%sstalled: output cap hit with no output (reasoning=%d) — ended the lease on %s (pushed work kept for takeover)%s",
				labelOf(t), stallReason, claimLabel(res.Claim), reasonNote)
		case ended && countUndeclared:
			log.Printf("%ssession ended holding %s, which has pushed work — ended the lease on %s (pushed work kept for takeover)%s (undeclared hand-offs this run: %d — the phase finished but never moved the task on, so the next clanker pays to rediscover it)",
				labelOf(t), res.Claim.TaskID, claimLabel(res.Claim), reasonNote, d.undeclared)
		case ended:
			log.Printf("%ssession ended holding %s, which has pushed work — ended the lease on %s (pushed work kept for takeover)%s",
				labelOf(t), res.Claim.TaskID, claimLabel(res.Claim), reasonNote)
		case stalled && countUndeclared:
			log.Printf("%sstalled: output cap hit with no output (reasoning=%d) — session ended holding %s with pushed work; leaving the lease to expire so the takeover hand-off survives (undeclared hand-offs this run: %d)",
				labelOf(t), stallReason, claimLabel(res.Claim), d.undeclared)
		case countUndeclared:
			log.Printf("%ssession ended holding %s, which has pushed work — leaving the lease to expire so the takeover hand-off survives (undeclared hand-offs this run: %d — the phase finished but never moved the task on, so the next clanker pays to rediscover it)",
				labelOf(t), res.Claim.TaskID, d.undeclared)
		case stalled:
			log.Printf("%sstalled: output cap hit with no output (reasoning=%d) — session ended holding %s with pushed work; leaving the lease to expire so the takeover hand-off survives",
				labelOf(t), stallReason, claimLabel(res.Claim))
		default:
			log.Printf("%ssession ended holding %s, which has pushed work — leaving the lease to expire so the takeover hand-off survives",
				labelOf(t), res.Claim.TaskID)
		}
		return ended, false
	}
	if t.Releaser == nil {
		return false, false
	}
	// A stalled session is named by its cause on THIS path too, not only on the held-with-work branch
	// above: a stall that never pushed a branch is the shape that leaves nothing behind, so "handed
	// back to the queue" alone would send the next clanker to rediscover the cause — the MAK-123
	// post-mortem's complaint.
	stallReason, stalled := outputCapStallReasoning(res)
	// Detach from ctx: a cancelled run (Ctrl-C, SIGTERM) is exactly when a claim would otherwise be
	// abandoned, so the handback has to outlive the signal that prompted it.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	if err := t.Releaser.ReleaseDriver(rctx, res.Claim.TaskID, res.Claim.RunID); err != nil {
		if !errors.Is(err, plane.ErrNotWired) {
			log.Printf("%scould not hand %s back: %v — its lease will expire instead", labelOf(t), res.Claim.TaskID, err)
		}
		return false, false
	}
	d.clearCurrentClaim()
	if stalled {
		log.Printf("%sstalled: output cap hit with no output (reasoning=%d) — session ended holding %s; handed it back to the queue for another clanker",
			labelOf(t), stallReason, claimLabel(res.Claim))
		return true, true
	}
	log.Printf("%shanded %s back to the queue (the session ended still holding it)", labelOf(t), res.Claim.TaskID)
	return true, true
}

// expireHeldClaim ends a dead session's lease on a task whose pushed work must survive. When
// usageLimit, it names the provider's quota as the reason, so the plane charges the next takeover
// to its usage-limit ladder instead of the task's reclaim budget; reasonSent reports whether the
// plane ACCEPTED that reason (an older plane refuses the argument, and the lease is then ended
// without it). The call is detached from ctx and bounded exactly like the ReleaseDriver call: a
// cancelled run (Ctrl-C, SIGTERM, RESTART_NOW) is precisely when a claim would otherwise be
// abandoned, so the handback has to outlive the signal that prompted it without letting a wedged
// plane hold up shutdown.
func (d *Driver) expireHeldClaim(ctx context.Context, t Target, res harness.Result, usageLimit bool) (ended, reasonSent bool) {
	if t.Releaser == nil {
		return false, false
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	reason := ""
	if usageLimit {
		reason = plane.ReasonUsageLimit
	}
	err := t.Releaser.ExpireLease(rctx, res.Claim.TaskID, res.Claim.RunID, reason)
	if err != nil && reason != "" && errors.Is(err, plane.ErrReasonUnsupported) {
		// An older plane refuses the unknown `reason` argument outright. Retry once without it: the
		// immediate takeover matters more than the budget exemption. Never fall back to
		// ReleaseDriver - CLA-708 forbids it on pushed work.
		d.expireReasonUnsupportedOnce.Do(func() {
			log.Printf("%streating the release_task refusal as an unrecognized reason argument — ending the lease without a usage-limit reason, so this takeover will charge the task's reclaim budget", labelOf(t))
		})
		err = t.Releaser.ExpireLease(rctx, res.Claim.TaskID, res.Claim.RunID, "")
		reason = ""
	}
	switch {
	case err == nil:
		d.clearCurrentClaim()
		return true, reason != ""
	case errors.Is(err, plane.ErrNotWired):
		// Nothing to call. The old line is the honest one, and it is what the caller logs when this
		// returns false.
		return false, false
	case errors.Is(err, plane.ErrPlaneTooOld):
		d.expireUnsupportedOnce.Do(func() {
			log.Printf("%sthe backlog plane does not serve this build's task tools — it needs plane protocol %d (the same cutover as this release); dead runs' leases will expire on their own until it is promoted", labelOf(t), plane.RequiredPlaneProtocol)
		})
		return false, false
	default:
		log.Printf("%scould not end the lease on %s: %v — its lease will expire instead", labelOf(t), res.Claim.TaskID, err)
		return false, false
	}
}
