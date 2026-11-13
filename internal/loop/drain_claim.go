package loop

import (
	"context"
	"errors"
	"log"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// The runner takes the drain's claim before the phase walk starts, so every
// phase session resumes a task the DRIVER holds rather than the first session
// claiming one of its own. A retry inside the walk must therefore re-claim
// before it respawns, because the dead or empty exit handed the claim back.

// runnerClaimAttempts bounds the offer-and-claim rounds: each round is a fresh
// offer, so three is room for two races without turning a busy queue into a
// spin.
const runnerClaimAttempts = 3

// drainClaim is what the runner's claim step decided for this drain.
type drainClaim int

const (
	// claimSessionPath means the first session claims, exactly as before: the
	// drain is not eligible, or NextOffer says this plane is not wired.
	claimSessionPath drainClaim = iota
	// claimHeld means the runner holds a claim, seeded into s.carried.
	claimHeld
	// claimNothing means there is nothing to work: no offer, every attempt lost
	// the task, or the queue failed. The drain ends without spawning.
	claimNothing
)

// runnerClaimEligible reports whether the runner should take this drain's claim
// itself. An unphased run is one session and claims as it always has, a
// custom-prompt phase owns its own wording, and a blind run cannot read the
// queue — all three keep the session-claim path.
func (s *drainSeq) runnerClaimEligible() (plane.TaskClaimer, bool) {
	if s.d.blind || len(s.phases) < 2 {
		return nil, false
	}
	for i := range s.rawPhases {
		if s.rawPhases[i].Prompt != "" {
			return nil, false
		}
	}
	claimer, ok := s.t.Releaser.(plane.TaskClaimer)
	if !ok {
		return nil, false
	}
	return claimer, true
}

// claimForDrain takes one task for the whole drain, re-reading the queue when a
// race costs the offer. It says which of the three endings applies; only
// claimHeld has a claim planted in s.carried.
func (s *drainSeq) claimForDrain(ctx context.Context) drainClaim {
	claimer, ok := s.runnerClaimEligible()
	if !ok {
		return claimSessionPath
	}
	for attempt := 0; attempt < runnerClaimAttempts; attempt++ {
		offer, err := claimer.NextOffer(ctx)
		switch {
		case errors.Is(err, plane.ErrNotWired):
			return claimSessionPath
		case err != nil:
			log.Printf("%siteration %d: the queue could not be read (%v) — ending the drain without a session",
				labelOf(s.t), s.drainNum, err)
			return claimNothing
		}
		if offer.Next == nil {
			log.Printf("%siteration %d: nothing to claim", labelOf(s.t), s.drainNum)
			return claimNothing
		}
		next := offer.Next
		next, usedReady := s.declineUnpreparable(ctx, offer)
		if next == nil {
			return claimNothing
		}
		ct, err := claimer.ClaimTask(ctx, next.ID, next.RequiresTakeover)
		if errors.Is(err, plane.ErrNotClaimable) && offer.ReadyNext != nil && !usedReady {
			// The takeover or close-out was withheld, so take what the plane
			// offers a plain claim instead — checked before the claim like Next.
			if next, _ = s.declineUnpreparable(ctx, plane.Offer{Next: offer.ReadyNext}); next == nil {
				return claimNothing
			}
			ct, err = claimer.ClaimTask(ctx, next.ID, next.RequiresTakeover)
		}
		switch {
		case err == nil:
			s.plantClaim(ct)
			return claimHeld
		case errors.Is(err, plane.ErrClaimLost):
			// Somebody else got it: the offer is stale, so re-read.
		default:
			// A transport error may hide a claim the plane DID accept. The error
			// cannot tell that from a refusal, and the lease expires on its own,
			// so the drain ends rather than retrying over a possibly-live lease.
			log.Printf("%siteration %d: claiming %s failed (%v) — ending the drain without a session; the lease, if the plane took the claim, expires on its own",
				labelOf(s.t), s.drainNum, claimLabel(harness.Claim{TaskID: next.ID, Ref: next.Ref}), err)
			return claimNothing
		}
	}
	log.Printf("%siteration %d: lost every claim attempt for this drain, so there is nothing to work",
		labelOf(s.t), s.drainNum)
	return claimNothing
}

// plantClaim seeds a held claim into the walk: the first phase resumes it, and
// the deferred handback owns releasing it. The brief is kept for the start
// phase's wiring.
func (s *drainSeq) plantClaim(ct plane.ClaimedTask) {
	res := &harness.Result{Claim: harness.Claim{
		TaskID:   ct.Task.ID,
		Ref:      ct.Task.Ref,
		RunID:    ct.RunID,
		HasWIP:   ct.Task.HasWIP,
		Category: ct.Task.Category,
	}}
	if ct.Task.HasWIP {
		// The plane always synthesises a branch name, so an UNRECORDED one is
		// not a checkpoint and must not read as one.
		res.Claim.Branch = ct.Task.Branch
	}
	s.carried = res
	s.seen = res.Claim
	s.runnerClaimed = true
	s.claimedTask = ct.Task
	s.d.noteCurrentClaim(res.Claim, s.t.Name)
	kind := "fresh"
	switch {
	case ct.Task.RequiresTakeover:
		kind = "takeover"
	case ct.Task.AwaitingMerge:
		kind = "close-out"
	}
	log.Printf("%siteration %d: runner claimed %s (%s)", labelOf(s.t), s.drainNum, claimLabel(res.Claim), kind)
}

// declineUnpreparable returns the task to claim after the pre-claim worktree
// check, or nil when the offer must be left alone. A deterministic local
// refusal is found BEFORE the claim, so it normally costs no reclaim: the task
// is simply not claimed, the drain idles, and the next offer retries once the
// tree is fixed.
//
// That is only safe while the tree can be fixed. A tree that refuses every
// check never parks and never lets the queue advance — next_task keeps offering
// the same head — so the refusal streak is bounded: at startRefusalBound the
// claim is taken anyway, the post-claim prep refuses it again, and startRefused
// parks the task for the operator. One reclaim buys the park, which is the
// ending the claimless path can never reach.
//
// When the plane offered a ReadyNext (a declined takeover or close-out), that
// task is checked and claimed instead; usedReady says the returned task is that
// fallback, so the caller does not offer it twice.
func (s *drainSeq) declineUnpreparable(ctx context.Context, offer plane.Offer) (next *plane.TaskBrief, usedReady bool) {
	next = offer.Next
	for {
		reason := s.worktreeRefusal(ctx, next)
		if reason == "" {
			return next, usedReady
		}
		label := claimLabel(harness.Claim{TaskID: next.ID, Ref: next.Ref})
		n := s.d.bumpStartRefusal(next.ID)
		if n >= startRefusalBound {
			log.Printf("%siteration %d: %s: not claimable (%s) on %d consecutive offers — claiming it anyway so the prep refuses it and parks the task",
				labelOf(s.t), s.drainNum, label, reason, n)
			return next, usedReady
		}
		log.Printf("%siteration %d: %s: not claiming it (refusal %d of %d): %s — fix the worktree and the task will be offered again; no reclaim is spent",
			labelOf(s.t), s.drainNum, label, n, startRefusalBound, reason)
		if offer.ReadyNext == nil {
			return nil, usedReady
		}
		next = offer.ReadyNext
		offer.ReadyNext = nil
		usedReady = true
	}
}

// worktreeRefusal runs the driver's pre-claim worktree check for one offered
// task and caches the origin facts its fetch produced, keyed to the task and
// checkout they describe. A check that cannot run is NOT a refusal: the claim is
// worth taking and the post-claim prep repeats the decision with the same facts.
func (s *drainSeq) worktreeRefusal(ctx context.Context, b *plane.TaskBrief) string {
	if b == nil {
		return ""
	}
	checkout, err := s.d.checkoutFor(s.t, b.Repo)
	if err != nil || checkout == "" {
		return ""
	}
	req := worktreeRequest{
		Checkout: checkout, TaskID: b.ID, Branch: b.Branch,
		Resume: b.HasWIP, CloseOut: b.AwaitingMerge,
	}
	if !b.HasWIP && !b.AwaitingMerge {
		// A create cuts from origin/<PRBase>, so resolving the base now lets the
		// pre-claim check prove a missing one; a resume never uses it.
		req.PRBase = s.d.prBaseFor(ctx, s.t, checkout, b.Repo)
	}
	doneTip := b.PhasesDone[config.ImplementPhaseName].Tip
	reason, facts, err := s.d.worktreeCheckFn()(ctx, req, doneTip)
	if err != nil {
		log.Printf("%siteration %d: %s: could not pre-check the task worktree (%v) — claiming anyway",
			labelOf(s.t), s.drainNum, claimLabel(harness.Claim{TaskID: b.ID, Ref: b.Ref}), err)
		s.clearPreClaimOrigin()
		return ""
	}
	if facts.Known {
		s.preClaimOrigin = facts
		s.preClaimTask = b.ID
		s.preClaimCheckout = checkout
	} else {
		s.clearPreClaimOrigin()
	}
	return reason
}

// clearPreClaimOrigin drops the cached pre-claim read, so nothing can resume on
// facts that belong to another task or a failed fetch.
func (s *drainSeq) clearPreClaimOrigin() {
	s.preClaimOrigin = originBranchFacts{}
	s.preClaimTask = ""
	s.preClaimCheckout = ""
}

// reclaimForRetry re-takes the claim a runner-claimed retry is about to resume,
// and re-chooses the start the respawn begins at. A plain claim first; an
// expired lease needs the takeover. When this task is gone it falls back to any
// claim at all — the dead-retry counters key on task id, so a different task
// simply earns its own budget.
func (s *drainSeq) reclaimForRetry(ctx context.Context, taskID string) bool {
	claimer, ok := s.runnerClaimEligible()
	if !ok {
		return false
	}
	if taskID == "" {
		// The dead session named no task, so a claim by id cannot be tried:
		// fall through to whatever the queue offers.
		return s.claimForDrain(ctx) == claimHeld && s.startAfterReclaim(ctx)
	}
	ct, err := claimer.ClaimTask(ctx, taskID, false)
	if errors.Is(err, plane.ErrClaimLost) {
		ct, err = claimer.ClaimTask(ctx, taskID, true)
	}
	switch {
	case err == nil:
		s.plantClaim(ct)
		return s.startAfterReclaim(ctx)
	case errors.Is(err, plane.ErrClaimLost), errors.Is(err, plane.ErrNotClaimable):
		return s.claimForDrain(ctx) == claimHeld && s.startAfterReclaim(ctx)
	default:
		log.Printf("%siteration %d: could not re-claim %s for a retry (%v) — ending the drain",
			labelOf(s.t), s.drainNum, taskID, err)
		return false
	}
}

// startAfterReclaim re-chooses the start for the claim a retry just planted: the
// retry may land a different task, or a different start on the same one, so the
// walk resumes wherever the fresh facts say.
func (s *drainSeq) startAfterReclaim(ctx context.Context) bool {
	if s.prepareStart(ctx) != startReady {
		return false
	}
	s.i = s.start
	return true
}
