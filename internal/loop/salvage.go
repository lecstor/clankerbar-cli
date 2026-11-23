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

// endUntrustedDrain closes out a drain whose session output could not be read whole, without acting
// on anything that stream said. # Why a spend ceiling then stops the account Because the ceiling is
// a promise, and this is the driver saying it can no longer keep it.
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

// salvageStrandedWork commits and pushes the work a dead session left in its worktree, and records
// the branch on the task so another machine can pick it up. workdir is where the session actually
// ran — the resolved spawn directory, not rebuilt from the target — because that is where its
// worktree lives once sessions start in per-task checkouts.
func (d *Driver) salvageStrandedWork(ctx context.Context, t Target, res *harness.Result, producedNothing bool, workdir string) {
	if d.newSalvager == nil || !res.Claim.Held() {
		return
	}
	// Detached from ctx, like the handback and unlike the delivery checks: a cancelled run (Ctrl-C,
	// SIGTERM) is precisely a session killed mid-task, so this is when work goes missing, not when it
	// can be skipped.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), salvageTimeout)
	defer cancel()

	out := d.newSalvager(workdir).Salvage(sctx, res.Claim.TaskID, res.Claim.Ref, recordedBranches(*res)...)
	label := claimLabel(res.Claim)
	switch out.Status {
	case salvage.Nothing:
		// Silent on the ordinary case. A session that committed its own work leaves a clean tree, and a
		// log line every iteration saying so would bury the ones that matter.
		if out.Worktree != "" {
			if producedNothing {
				// CLA-386: the tree is clean because nothing was ever written, not because the work landed.
				log.Printf("%sphase produced nothing for %s — the worktree is clean because no work was ever written to it, not because it was committed: %s",
					labelOf(t), label, out.Detail)
			} else {
				log.Printf("%snothing to salvage for %s: %s", labelOf(t), label, out.Detail)
			}
		}
		return
	case salvage.Untracked:
		// CLA-695: the session DELIVERED — the branch it recorded is on the origin at exactly the
		// worktree's HEAD — and left nothing but untracked files.
		left := strings.Join(out.Paths, ", ")
		if left == "" {
			// A salvager that names the paths only in its prose still gets them in front of the operator: a
			// log line ending in a colon says nothing.
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
	// Only now. HasWIP is a statement about the TASK, not about the disk: it means the next clanker
	// will be told there is work to fetch.
	res.Claim.HasWIP = true
	res.Claim.Branch = out.Branch
	// The session is over, so no OnClaim snapshot will carry this branch to CURRENT_CLAIM; update the
	// record directly, or a supervisor handing this claim back after the daemon dies would choose
	// `release` (no branch) and post `ready` over the work just rescued.
	d.noteCurrentClaim(res.Claim, t.Name)
	log.Printf("%srecorded %s as the hand-off branch for %s — the next clanker takes it over instead of starting again",
		labelOf(t), out.Branch, label)
}

// salvageTimeout bounds one rescue: a status read, a commit, and a push over the network. Longer
// than the delivery checks' minute because this one uploads.
const salvageTimeout = 3 * time.Minute
