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

// releaseHeldClaim hands a task the session was still holding back to the queue,
// instead of leaving its lease to run out with nobody heartbeating it.
//
// It runs on EVERY exit from a session, deliberately. A usage limit, a transient
// blip, an outright failure and a clean finish with the work unfinished all leave
// the same dead lease behind — and the usage-limit case is the worst of the four,
// because the driver then goes to sleep for as long as the reset takes. One real
// run slept ninety minutes on a live claim and cost the task a reclaim it had not
// earned; the budget is two before the plane parks the task for the operator.
//
// A claim with pushed work is left alone; Claim.Releasable says why.
// releaseCarried hands back a claim a phase was holding open for a successor
// that is not going to run, and returns nil so the caller cannot hand it back
// twice. A nil carried claim is a no-op, so callers need not check first.
//
// It exists because "held open across the seam" and "retry this phase with a
// fresh session" are contradictory: before phases, releaseHeldClaim ran on every
// attempt and neither the limit wait nor the transient backoff could leave a
// lease unattended. Both loop back to a session that re-claims, so both have to
// undo the hold first.
func (d *Driver) releaseCarried(ctx context.Context, t Target, carried *harness.Result) *harness.Result {
	if carried == nil {
		return nil
	}
	d.releaseHeldClaim(ctx, t, *carried, false)
	return nil
}

// releaseCarriedEnd is releaseCarried for a phase about to loop back to a FRESH
// session — a usage-limit wait, a transient backoff. It hands back whatever the
// attempt was holding open and returns an end that owes nothing further, so a
// seam's hold cannot survive into a retry that is going to re-claim anyway.
func (d *Driver) releaseCarriedEnd(ctx context.Context, t Target, end phaseEnd) phaseEnd {
	// Only a hold that actually happened needs undoing. Every other end has
	// ALREADY been through the handback in drainPhase's seam block, and releasing
	// again would hand the task back twice — which on a usage-limit exit is the
	// common path, not a corner: the seam releases (the phase did not reach its
	// checkpoint), and then the limit branch loops back to wait.
	if !end.checkpoint || end.claim == nil {
		return end
	}
	// Read BEFORE the call, for the same reason drainPhase reads it: a zero or
	// settled claim releases nothing, and reporting it as released would erase a
	// live claim upstream.
	held := end.claim.Claim.Held()
	d.releaseCarried(ctx, t, end.claim)
	// The session rides the reconstruction (CLA-630): it is record data for the
	// fleet iteration record, not a hold to undo, and the pass it names really
	// did spawn. The limit branch calls this BEFORE its stop paths return — a
	// reset past the run's ceiling, a stop during the wait, a retry that fails
	// to launch — and dropping the field there would omit a spawned session
	// from the record. On the paths that loop back, the next attempt's own end
	// replaces this one, so carrying it costs nothing.
	return phaseEnd{released: held, session: end.session}
}

// countUndeclared says whether THIS call site is the one where a held-with-work
// claim means the CLA-384 failure: a phase that finished cleanly and did not hand
// the task over. Only the seam's final-phase handback passes true. Every other
// caller reaches the same branch for a reason that is not a failure to declare -
// a budget trip between phases, a crash, a transient retry, a usage-limit wait, a
// refused handoff - and counting those would make the number mean nothing, which
// is the only thing it has going for it.
//
// It returns whether the claim is ACTUALLY back in the ready queue at the end —
// that is, whether releaseHeldClaim really released it. The seam uses that to
// decide whether a bounded retry may re-claim (a claim left to the takeover path,
// or a release the plane refused, is still live and a fresh session would only be
// refused); the other callers ignore it.
func (d *Driver) releaseHeldClaim(ctx context.Context, t Target, res harness.Result, countUndeclared bool) bool {
	_, queued := d.handBackClaim(ctx, t, res, countUndeclared)
	return queued
}

// handBackClaim is releaseHeldClaim with the two outcomes split: handed says
// the claim was actually let go (back in the ready queue, or its lease ended on
// pushed work), queued says it specifically returned to the ready queue. Only
// the phase seam reads handed — it must drop the drain's carried claim when the
// handback landed, but keep it for the deferred retry when it did not, and
// releaseHeldClaim's bool cannot tell those apart on the pushed-work path.
func (d *Driver) handBackClaim(ctx context.Context, t Target, res harness.Result, countUndeclared bool) (handed, queued bool) {
	// A stream read only in part tells us which calls we SAW, never which the
	// session made: the settle that released the task may simply be in the bytes
	// that never arrived. Handing the task back on that reading posts `ready` over
	// work already in review, which is the one outcome worse than a lease expiring
	// (CLA-262). Let it expire instead — the plane's own sweep does the right thing
	// with it, and keeps the takeover hand-off if there is one.
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
		// CLA-708: the claim has pushed work, so Release is forbidden — it would
		// post `ready` over the branch and throw away the takeover hand-off. The
		// plane's expireLease ends the dead run's lease NOW and leaves the task an
		// immediate takeover with its branch, which is the same disposition the
		// lease's own expiry sweep produces, half an hour sooner.
		//
		// Leaving the lease to expire remains the fallback on every failure below
		// (an unwired plane, a plane older than the field, a refused write), and
		// it is the ONLY behaviour for the `res.Untrusted` path above, which does
		// not reach here by design.
		stallReason, stalled := outputCapStallReasoning(res)
		ended := d.expireHeldClaim(ctx, t, res)
		if countUndeclared {
			d.undeclared++
		}
		switch {
		case ended && stalled && countUndeclared:
			log.Printf("%sstalled: output cap hit with no output (reasoning=%d) — ended the lease on %s (pushed work kept for takeover) (undeclared hand-offs this run: %d)",
				labelOf(t), stallReason, claimLabel(res.Claim), d.undeclared)
		case ended && stalled:
			log.Printf("%sstalled: output cap hit with no output (reasoning=%d) — ended the lease on %s (pushed work kept for takeover)",
				labelOf(t), stallReason, claimLabel(res.Claim))
		case ended && countUndeclared:
			log.Printf("%ssession ended holding %s, which has pushed work — ended the lease on %s (pushed work kept for takeover) (undeclared hand-offs this run: %d — the phase finished but never moved the task on, so the next clanker pays to rediscover it)",
				labelOf(t), res.Claim.TaskID, claimLabel(res.Claim), d.undeclared)
		case ended:
			log.Printf("%ssession ended holding %s, which has pushed work — ended the lease on %s (pushed work kept for takeover)",
				labelOf(t), res.Claim.TaskID, claimLabel(res.Claim))
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
	// A stalled session is named by its cause on THIS path too (CLA-584), not
	// only on the held-with-work branch above: a stall that never pushed a
	// branch is the shape that leaves nothing behind, so "handed back to the
	// queue" alone would send the next clanker to rediscover the cause — the
	// MAK-123 post-mortem's complaint. Computed before the release so the line
	// that reports its outcome can carry it; the named line REPLACES the
	// generic one on success, as it does above.
	stallReason, stalled := outputCapStallReasoning(res)
	// Detach from ctx: a cancelled run (Ctrl-C, SIGTERM) is exactly when a claim
	// would otherwise be abandoned, so the handback has to outlive the signal that
	// prompted it. Bounded, so a wedged plane cannot hold up the shutdown.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	if err := t.Releaser.Release(rctx, res.Claim.TaskID, res.Claim.RunID); err != nil {
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

// expireHeldClaim ends a dead session's lease on a task whose pushed work must
// survive (CLA-708). It reports whether the plane accepted the handback; every
// refusal falls back to the pre-CLA-708 behaviour — leave the lease to expire —
// and says which refusal it got.
//
// The call is detached from ctx and bounded exactly like the Release call: a
// cancelled run (Ctrl-C, SIGTERM, RESTART_NOW) is precisely when a claim would
// otherwise be abandoned, so the handback has to outlive the signal that
// prompted it without letting a wedged plane hold up shutdown.
func (d *Driver) expireHeldClaim(ctx context.Context, t Target, res harness.Result) bool {
	if t.Releaser == nil {
		return false
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	err := t.Releaser.ExpireLease(rctx, res.Claim.TaskID, res.Claim.RunID)
	switch {
	case err == nil:
		d.clearCurrentClaim()
		return true
	case errors.Is(err, plane.ErrNotWired):
		// Nothing to call. The old line is the honest one, and it is what the
		// caller logs when this returns false.
		return false
	case errors.Is(err, plane.ErrPlaneTooOld):
		d.expireUnsupportedOnce.Do(func() {
			log.Printf("%sthe backlog plane does not serve this build's task tools — it needs plane protocol %d (the same cutover as this release); dead runs' leases will expire on their own until it is promoted", labelOf(t), plane.RequiredPlaneProtocol)
		})
		return false
	default:
		log.Printf("%scould not end the lease on %s: %v — its lease will expire instead", labelOf(t), res.Claim.TaskID, err)
		return false
	}
}
