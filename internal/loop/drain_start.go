package loop

// The start of a runner-claimed drain: gather the facts the start-phase table
// reads, choose the phase to begin the walk at, and prepare the worktree every
// phase of the drain runs in. See start_phase.go for the table itself.

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// startRefusalBound is how many consecutive worktree refusals of one task reach
// the operator: a dirty or diverged tree refuses every claim, so the second one
// is not going to clear itself.
const startRefusalBound = 2

// startPrep is how preparing the start ended.
type startPrep int

const (
	startReady startPrep = iota // s.start, s.carried and s.taskDir are set
	startAbort                  // hand the claim back and end the drain
)

// prepareStart reads what the plane and origin know about the claimed task,
// picks the phase to start at, and prepares the worktree the drain runs in.
// startAbort means the claim can be neither honoured nor resumed: the caller
// hands it back (or, when the refusal was deterministic, has parked it).
func (s *drainSeq) prepareStart(ctx context.Context) startPrep {
	// A reclaim may land a different task, so nothing from the previous start —
	// its worktree above all — may survive into this one.
	s.taskDir, s.prep, s.closeOut, s.start = "", worktreePrep{}, false, 0
	task := s.claimedTask
	claim := s.carried.Claim
	st, read := s.d.readTaskStateRetry(ctx, s.t, claim.TaskID, "to choose the start phase")
	if !read {
		// Neither the recorded work nor a no-code delivery can be placed without
		// the plane's record, so every start would be a guess.
		log.Printf("%siteration %d: %s: the task could not be read, so neither its recorded work nor its delivery can be placed — handing it back",
			labelOf(s.t), s.drainNum, claimLabel(claim))
		return startAbort
	}
	checkout, _, repo, err := s.d.sessionDir(ctx, s.t, s.carried)
	if err != nil {
		log.Printf("%siteration %d: %s: the task's checkout could not be resolved (%v) — handing the claim back",
			labelOf(s.t), s.drainNum, claimLabel(claim), err)
		return startAbort
	}
	prBase := s.d.prBaseFor(ctx, s.t, checkout, repo)
	doneTip := task.PhasesDone[config.ImplementPhaseName].Tip
	// A task with no recorded branch has nothing to ask origin about, and the
	// fetch would only cost a round-trip before the branch exists.
	of := originBranchFacts{Known: true}
	if task.HasWIP {
		of = s.d.originBranchFn()(ctx, checkout, task.Branch, doneTip)
	}
	f := startFacts{
		CloseOut: task.AwaitingMerge, HasWIP: task.HasWIP, Branch: task.Branch,
		NoCodeDelivered: st.DeliveryNoCode, OriginKnown: of.Known, OnOrigin: of.OnOrigin, Tip: of.Tip,
		ImplementDoneTip: doneTip, ImplementDoneIsAncestor: of.DoneIsAncestor,
		Reviews: st.Reviews, OpenMustIDs: openMustIDs(st.Reviews), OpenMustAtTip: openMustAtTip(st.Reviews, of.Tip),
	}
	choice := chooseStartPhase(s.phases, f)
	if choice.Abort {
		log.Printf("%siteration %d: %s: %s", labelOf(s.t), s.drainNum, claimLabel(claim), choice.Why)
		return startAbort
	}
	log.Printf("%siteration %d: %s starts at the %s phase: %s",
		labelOf(s.t), s.drainNum, claimLabel(claim), s.phases[choice.Index].Label(choice.Index), choice.Why)
	s.closeOut = choice.CloseOut
	if !choice.Branchless {
		var ok bool
		if choice, ok = s.prepareTaskDir(ctx, checkout, prBase, claim, task, of, choice); !ok {
			return startAbort
		}
	}
	s.start = choice.Index
	if choice.Index > 0 {
		// The chosen checkpoint carries no branch for a branchless start, which
		// is how the walk reads "the plane's record is the evidence".
		s.carried.Claim.Branch = choice.Checkpoint.Branch
		s.carried.CheckpointTip = choice.Checkpoint.Tip
		s.carried.CheckpointUnverified = false
	}
	s.d.clearStartRefusal(claim.TaskID)
	return startReady
}

// prepareTaskDir prepares the worktree the drain runs in and returns the choice
// the tree leaves standing: a tree holding unpushed commits cannot resume the
// recorded tip, so the start drops to implement. ok=false means the claim is no
// longer usable — handed back, or parked by startRefused.
func (s *drainSeq) prepareTaskDir(ctx context.Context, checkout, prBase string, claim harness.Claim, task plane.TaskBrief, of originBranchFacts, choice startChoice) (startChoice, bool) {
	req := worktreeRequest{
		Checkout: checkout, TaskID: claim.TaskID, Branch: task.Branch, PRBase: prBase,
		Resume: of.OnOrigin, CloseOut: choice.CloseOut, NoMerge: choice.Index != 0,
		// The origin read already fetched, but only a WIP task runs one.
		Fetched: of.Known && task.HasWIP,
	}
	prep, err := s.d.prepareWorktreeFn()(ctx, req)
	if err != nil {
		s.startRefused(ctx, claim, err)
		return choice, false
	}
	if choice.Index != 0 && prep.Ahead > 0 {
		// The session would see the local tip, not the checkpoint's, so the phase
		// the choice picked cannot be handed the work it expects. Implement takes
		// the tree as it is, PR base merged into it.
		log.Printf("%siteration %d: %s: the task worktree holds %d unpushed commit(s), so its tip is not the recorded checkpoint — starting at implement",
			labelOf(s.t), s.drainNum, claimLabel(claim), prep.Ahead)
		req.NoMerge = false
		if prep, err = s.d.prepareWorktreeFn()(ctx, req); err != nil {
			s.startRefused(ctx, claim, err)
			return choice, false
		}
		choice = startChoice{Why: "the worktree holds unpushed commits: implement"}
	}
	if choice.Index != 0 && prep.OriginTip != "" && prep.OriginTip != choice.Checkpoint.Tip {
		// Origin moved between the fact read and the prep, so the checkpoint the
		// choice was made on is no longer the tip: let the next claim re-derive
		// the start rather than resume work nobody reviewed.
		log.Printf("%siteration %d: %s: origin moved to %s after the start was chosen at %s — handing the claim back so the next claim re-derives it",
			labelOf(s.t), s.drainNum, claimLabel(claim), prep.OriginTip, choice.Checkpoint.Tip)
		return choice, false
	}
	if len(prep.Conflicts) > 0 {
		log.Printf("%siteration %d: %s: the PR base did not merge cleanly; the session is briefed with the conflicting paths: %s",
			labelOf(s.t), s.drainNum, claimLabel(claim), strings.Join(prep.Conflicts, ", "))
	}
	s.taskDir = prep.Dir
	s.prep = prep
	return choice, true
}

// startRefused handles a failed worktree prep. A fetch failure is transient, so
// it only ever hands the claim back; a deterministic refusal counts, and the
// second consecutive one tries to park the task. Whether the park took decides
// what the claim needs: a parked task is let go, anything else is handed back.
func (s *drainSeq) startRefused(ctx context.Context, claim harness.Claim, err error) {
	if !errors.Is(err, errWorktreeFetch) {
		if n := s.d.bumpStartRefusal(claim.TaskID); n >= startRefusalBound {
			if s.d.parkStartRefusal(ctx, s.t, s.carried, err.Error()) {
				s.carried = nil
				s.d.clearStartRefusal(claim.TaskID)
				log.Printf("%siteration %d: %s: the task worktree could not be prepared (%v), %d refusals in a row — parked for the operator",
					labelOf(s.t), s.drainNum, claimLabel(claim), err, n)
				return
			}
			log.Printf("%siteration %d: %s: the task worktree could not be prepared (%v), %d refusals in a row, and it could not be parked — handing the claim back; the next refusal retries the park",
				labelOf(s.t), s.drainNum, claimLabel(claim), err, n)
			return
		}
	}
	log.Printf("%siteration %d: %s: the task worktree could not be prepared (%v) — handing the claim back",
		labelOf(s.t), s.drainNum, claimLabel(claim), err)
}

// bumpStartRefusal records one consecutive worktree refusal for taskID and
// reports the new count.
func (d *Driver) bumpStartRefusal(taskID string) int {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if d.startRefusals == nil {
		d.startRefusals = make(map[string]int)
	}
	d.startRefusals[taskID]++
	return d.startRefusals[taskID]
}

// clearStartRefusal forgets a task's refusal streak: a claim that got its
// worktree prepared is the evidence the streak is over.
func (d *Driver) clearStartRefusal(taskID string) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	delete(d.startRefusals, taskID)
}

// markPhaseDone records the phase marker a later claim's start choice reads: an
// evidenced, non-handoff checkpoint of implement, fix or review. A review marker
// says only that the review phase finished — never that the review was clean;
// only phasesDone.implement is read today. Verify is excluded by name because its
// checkpoint means open MUSTs remain, so the seam it holds is for another fix
// round, not a finished phase.
func (s *drainSeq) markPhaseDone(ctx context.Context, st *seqStep) {
	if !s.runnerClaimed || !st.end.checkpoint || st.end.handoff != "" {
		return
	}
	res := st.end.claim
	if res == nil || !res.Claim.Held() || res.CheckpointTip == "" || res.CheckpointUnverified {
		return
	}
	switch st.ph.Name {
	case config.ImplementPhaseName, config.FixPhaseName:
	case config.ReviewPhaseName:
		// A panel writes one round across several members: only the member that
		// ends the review is the phase that finished it.
		if st.panelMember && !st.terminal {
			return
		}
	default:
		return
	}
	m, ok := s.t.Releaser.(plane.PhaseMarker)
	if !ok {
		return
	}
	if err := m.RecordPhaseDone(ctx, res.Claim.TaskID, res.Claim.RunID, st.ph.Name, res.CheckpointTip); err != nil {
		// A missing marker costs the next claim a re-derived start, never this
		// drain: the work itself is already on the plane and origin.
		log.Printf("%siteration %d: could not mark the %s phase done on %s (%v) — the next claim re-derives its start without it",
			labelOf(s.t), s.drainNum, st.ph.Name, res.Claim.TaskID, err)
	}
}
