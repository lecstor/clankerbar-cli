package loop

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
	"github.com/lecstor/clankerbar-cli/internal/salvage"
)

// endUntrustedDrain closes out a drain whose session output could not be read
// whole, without acting on anything that stream said (CLA-262).
//
// # Why the spend is not counted
//
// It is not that the figure is inconvenient — it is that it is not a measurement.
// The `result` event carrying a claude session's whole tokens-and-cost total is
// the LAST thing on the stream, so a stream cut short reports zero for a session
// that may have cost hundreds of dollars, and a partial sum from an adapter that
// accumulates per step is a lower bound of unknown looseness. Adding either to the
// accumulator would put a made-up number in front of the breaker and in the
// iteration's cost line.
//
// # Why a spend ceiling then stops the account
//
// Because the ceiling is a promise, and this is the driver saying it can no longer
// keep it. An operator who set `max_tokens` or `max_cost_usd` has asked to be
// protected from exactly the shape this is: sessions whose cost nothing can see.
// One unaccounted session is survivable; a night of them against an inert ceiling
// is the failure CLA-287 and CLA-258 were both about. Stopping is clean, loud, and
// resumable — the operator restarts the run once they know why.
//
// WHICH account stops follows the budget in force (CLA-480): a target whose stored
// document set the ceiling is exhausted on its own, with budgetStopped recorded so
// Run carries on with its siblings; only the local pool's ceiling (the run-wide
// promise over every project it governs) keeps the daemon-wide stop.
//
// With no spend ceiling set there is nothing to break: the wall clock does not
// depend on anything the child said, so the run carries on and the no-progress
// back-off is what bounds a target that keeps producing these.
//
// The harness NAME is the phase's own, not the run's: whether a spend ceiling is
// live is a per-harness question since CLA-367, and under per-phase harnesses
// (CLA-366) the session that went unreadable is not necessarily on d.h. Asking
// d.h would let an unreadable opencode session be waved through on the grounds
// that CLAUDE has no ceiling set.
func (d *Driver) endUntrustedDrain(t Target, ti, drainNum int, harnessName, logPath string, res harness.Result, tokens int, cost float64) (int, float64, bool, error) {
	log.Printf("iteration %d UNTRUSTED — %s (log: %s)", drainNum, res.Untrusted, logPath)
	log.Printf("iteration %d: not counting this session's parsed spend (tokens=%d cost=$%.4f — a floor, not a total), not classifying its exit (%s), and not handing back any claim it appeared to hold",
		drainNum, res.Tokens, res.CostUSD, res.ExitString())
	if !d.targetCfg(t).Budget.CountsSpendFor(harnessName) {
		return tokens, cost, false, nil
	}
	if d.exhaustUnmeasurable(ti, "the session's spend cannot be read, so that project's ceiling can no longer be honoured") {
		return tokens, cost, true, nil
	}
	log.Printf("iteration %d: stopping — a token/cost ceiling is set and this session's real spend cannot be known, so the ceiling can no longer be honoured — check the iteration log, then rerun to resume (log: %s)", drainNum, logPath)
	return tokens, cost, true, nil
}

// salvageStrandedWork commits and pushes the work a dead session left in its
// worktree, and records the branch on the task so another machine can pick it up
// (CLA-314).
//
// workdir is where the session actually ran — the resolved spawn directory, not
// rebuilt from the target — because that is where its worktree lives once
// sessions start in per-task checkouts (CLA-437). Salvaging from anywhere else
// would report "nothing to salvage" over real stranded work.
//
// # Why it runs on EVERY ending, not only on a usage limit
//
// The worktree is what is being rescued, and it looks identical whether the
// session was killed by a limit, died in a crash, or simply stopped with the work
// unfinished — while the cost of losing it is the same in all three. The limit,
// besides, is detected by parsing the stream AFTER the fact, so gating on it
// would skip exactly the sessions whose stream could not be read: the ones we
// know least about, and the likeliest to have stranded something. Running it when
// it was not needed costs nothing — a clean worktree commits nothing, pushes
// nothing and records nothing.
//
// # What it will not save (CLA-695)
//
// Untracked files alone, when the session DELIVERED — the branch it recorded for
// the task is on the origin at exactly the worktree's HEAD. There is nothing to
// rescue, and committing a generated file (a `wrangler types` dump, a report, a
// build artifact) pushes a NEW tip at the phase boundary: every review-panel
// member, the fix phase and the PR then ride a commit nobody chose, which is
// exactly what happened on the first review-panel run. Tracked edits are still
// salvaged on top of a delivered tip, and an undelivered session's untracked files
// are still salvaged in full — new source files it never added are the work
// CLA-314 exists to save. Delivery is the discriminator, and internal/salvage
// checks it against the origin rather than taking the session's word for it.
//
// # Why it is safe on the untrusted path
//
// CLA-262 forbids acting on a truncated stream's CLAIM-STATE: a settle we never
// saw may be in the bytes that never arrived, so handing the task back could post
// `ready` over work already in review. This does something different in kind.
// Recording a branch carries no status, so it cannot move a task, clear a holder
// or revert a review — it is additive, and its worst case on a task that really
// did reach review is that the branch field names the branch the work is on
// anyway. What it acts on is the local worktree, which no truncation can
// misreport. A claim we SAW settle is skipped entirely: Held() is false, and this
// is a rescue, not a tidy-up.
//
// # What a success does to the handback
//
// Claim.HasWIP is set only when the plane ACCEPTED the branch, and that is what
// makes the claim non-releasable below: the lease is left to expire so the task
// stays a takeover and the hand-off survives. If the record failed, the task has
// no branch on it, releasing to `ready` is still the better move, and the old
// path runs unchanged.
func (d *Driver) salvageStrandedWork(ctx context.Context, t Target, res *harness.Result, producedNothing bool, workdir string) {
	if d.newSalvager == nil || !res.Claim.Held() {
		return
	}
	// Detached from ctx, like the handback and unlike the delivery checks: a
	// cancelled run (Ctrl-C, SIGTERM) is precisely a session killed mid-task, so
	// this is when work goes missing, not when it can be skipped. Bounded, so a
	// wedged git or an unreachable remote cannot hold up the shutdown.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), salvageTimeout)
	defer cancel()

	out := d.newSalvager(workdir).Salvage(sctx, res.Claim.TaskID, res.Claim.Ref, recordedBranches(*res)...)
	label := claimLabel(res.Claim)
	switch out.Status {
	case salvage.Nothing:
		// Silent on the ordinary case. A session that committed its own work leaves
		// a clean tree, and a log line every iteration saying so would bury the ones
		// that matter.
		if out.Worktree != "" {
			if producedNothing {
				// CLA-386: the tree is clean because nothing was ever written, not
				// because the work landed. The line must say so plainly — "nothing to
				// salvage" reads identically either way, and the only other
				// discriminator (a `verified <ref>: origin/<branch>` line) is an
				// absence, which is the weakest possible signal. Gated on the driver's
				// own classification (not the raw deadPhase predicate): a capped or
				// untrusted session is an orderly end whose finish reason is its own
				// marker or cannot be read, and must not be handed the emphatic line.
				log.Printf("%sphase produced nothing for %s — the worktree is clean because no work was ever written to it, not because it was committed: %s",
					labelOf(t), label, out.Detail)
			} else {
				log.Printf("%snothing to salvage for %s: %s", labelOf(t), label, out.Detail)
			}
		}
		return
	case salvage.Untracked:
		// CLA-695: the session DELIVERED — the branch it recorded is on the origin
		// at exactly the worktree's HEAD — and left nothing but untracked files.
		// There is nothing to rescue and nothing to record, so this is a report
		// rather than an alarm: the paths are named because an untracked file can
		// still be work (a source file written after the last commit), and only a
		// reader can tell that from the generated output this rule exists for. The
		// claim is left exactly as the Refused and Failed cases leave it — no
		// branch recorded, so the ordinary handback rules apply unchanged.
		left := strings.Join(out.Paths, ", ")
		if left == "" {
			// A salvager that names the paths only in its prose still gets them in
			// front of the operator: a log line ending in a colon says nothing.
			left = out.Detail
		}
		log.Printf("%sdelivered %s with only untracked files left in %s — committing them would move the branch later readers review, so they are left alone: %s",
			labelOf(t), label, out.Worktree, left)
		return
	case salvage.Refused:
		log.Printf("%sSTRANDED WORK LEFT AS IS — %s: %s", labelOf(t), label, out.Detail)
		return
	case salvage.Failed:
		log.Printf("%sSALVAGE FAILED — %s: %s", labelOf(t), label, out.Detail)
		return
	}

	log.Printf("%ssalvaged %s: %s", labelOf(t), label, out.Detail)
	rec, ok := t.Releaser.(plane.Recorder)
	if !ok {
		log.Printf("%sthe branch %s is pushed but cannot be recorded on %s (no plane writes configured) — a clanker on this machine can still find it by name",
			labelOf(t), out.Branch, label)
		return
	}
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer rcancel()

	if err := rec.RecordBranch(rctx, res.Claim.TaskID, res.Claim.RunID, out.Branch); err != nil {
		if !errors.Is(err, plane.ErrNotWired) {
			log.Printf("%scould not record branch %s on %s: %v — the work IS pushed, so fetch that branch by name rather than starting again",
				labelOf(t), out.Branch, label, err)
		}
		return
	}
	// Only now. HasWIP is a statement about the TASK, not about the disk: it means
	// the next clanker will be told there is work to fetch.
	res.Claim.HasWIP = true
	res.Claim.Branch = out.Branch
	// The session is over, so no OnClaim snapshot will carry this branch to
	// CURRENT_CLAIM; update the record directly, or a supervisor handing this
	// claim back after the daemon dies would choose `release` (no branch) and
	// post `ready` over the work just rescued.
	d.noteCurrentClaim(res.Claim, t.Name)
	log.Printf("%srecorded %s as the hand-off branch for %s — the next clanker takes it over instead of starting again",
		labelOf(t), out.Branch, label)
}

// salvageTimeout bounds one rescue: a status read, a commit, and a push over the
// network. Longer than the delivery checks' minute because this one uploads.
const salvageTimeout = 3 * time.Minute
