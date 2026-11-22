package supervisor

// The deterministic close-out (CLA-758).
//
// An approved-awaiting-merge task is normally integrated by a full agent session:
// `next_task` offers it ahead of fresh work, and the next clanker to finish its
// current task runs an implement-phase session to merge the PR, re-run the gates
// and close it. The spend is small; the LATENCY is the problem — a close-out waits
// for some clanker to finish, and a review panel runs 30-60 minutes.
//
// Almost none of that needs a model. For every supervised project that lets a
// clanker integrate, this pass takes each approved-awaiting-merge task whose open
// PR is MERGEABLE, carries a check rollup that ran and passed, and is based on the
// integration branch the phase briefs resolve for that repo, and does the
// mechanical rest:
//
//  1. CLAIM the task first. The claim is atomic and first-wins, so an agent
//     close-out and this path can never both merge the same PR.
//  2. Merge the PR as a MERGE COMMIT into that RESOLVED integration branch. The
//     request names that branch and the judged head revision explicitly: a PR's
//     own base can be retargeted by anyone with write access, and the merge must
//     not follow it (a main-based merge is the operator's promotion, never a task
//     delivery). Never a squash or a rebase: the batch integration flow depends on
//     the history a merge commit keeps.
//  3. Verify the merge commit is an ancestor of the base on ORIGIN — the same
//     ancestor test the delivery gate records as `mergeVerified`, run on the
//     commit GitHub made, and read from a throwaway clone rather than the checkout
//     the fleet's sessions are sharing.
//  4. Close the task done with that delivery, in the claim's run.
//
// Everything else stands down and leaves the task exactly where it was, for the
// agent close-out:
//
//   - the project's dial says the operator integrates (`authorityIntegrate: human`);
//   - no open PR, or more than one, for the task's recorded branch;
//   - a PR based on `main`/`master` — the staging→main promotion is the operator's;
//   - a judged PR whose base is missing or is not the resolved integration
//     branch: an unproven merge target is never merged;
//   - CONFLICTING, failing, pending, empty or unresolvable checks. CONFLICTING is
//     the one stand-down that carries evidence: the conflicting paths, computed
//     with `git merge-tree` against freshly fetched refs, logged once per
//     head/base pair rather than on every poll;
//   - a refused claim (someone else holds it);
//   - any failure after the claim: the claim is RELEASED, so the task returns to
//     `approved` and the agent close-out can take it. A task is never left claimed
//     by a run that has stopped. The one exception is a merge that LANDED but could
//     not be finished: its close is retried on transient failures, the delivery is
//     recorded if it still fails, and the claim is left to its lease — because a
//     merged PR is no longer OPEN, so releasing would strand the task forever.
//
// One log line per outcome, and each unchanged stand-down is said once rather than
// on every poll: the supervisor's own log is a status surface, and a line repeated
// every 15 seconds is one an operator learns to skip.

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/loop"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

const (
	// closeOutTaskTimeout bounds ONE task's whole close-out: the PR judgement's
	// own poll budget, the claim, a merge, a fetch and the ancestry check. A pass
	// that runs long is a merge that is hanging, and releasing is the honest end.
	closeOutTaskTimeout = 90 * time.Second

	// closeOutCallTimeout bounds one plane call that is not the PR judgement (the
	// policy read, the queue, the claim, the release, the close). The release
	// gets its own budget because it runs after a failure, when the thing that
	// failed may be the very thing that would have answered this one.
	closeOutCallTimeout = 20 * time.Second

	// closeOutPassBudget bounds a whole reconcile's close-out work, so a queue of
	// stuck merges cannot hold the reconcile loop (which also spawns, stops and
	// reports) for longer than the fleet can tolerate: two minutes is well over a
	// dozen clean close-outs, and everything past it waits for the next poll.
	closeOutPassBudget = 2 * time.Minute

	// closeOutCompleteAttempts bounds the retries of the final close after a
	// merge landed. It is the one write whose failure must not fall back to a
	// release: the PR is gone from the open list, so a release strands the task.
	// Four attempts spend 1.75s of backoff inside the single cleanup timeout.
	closeOutCompleteAttempts = 4
)

// closeOutCompleteBackoff is the wait before the second close attempt; it doubles
// each retry. A var so a test can shrink it rather than wait the real bound out,
// the same discipline closeOutBaseProbeTimeout keeps.
var closeOutCompleteBackoff = 250 * time.Millisecond

// closeOutBaseProbeTimeout bounds the close-out's own origin probe. The driver
// gives its probe 30s (loop.prBaseProbeTimeout) because a phase spawn has nothing
// else to do; a close-out shares its 90s task budget with the PR judgement, the
// claim and the merge check, so one unreachable origin must not be able to spend a
// third of it and get the task cancelled mid-flight. A few seconds is ample for the
// one ls-remote, and a probe that cannot answer in that time is exactly the
// fallback ResolvePRBase already handles. A var so a test can shrink it rather
// than wait the real bound out, the same discipline loop.prBaseProbeTimeout keeps.
var closeOutBaseProbeTimeout = 5 * time.Second

// closeOutOriginProbe is the origin read closeOutBases resolves through; a var so
// a test can count or stall the probes a pass makes without a wrapper on PATH.
var closeOutOriginProbe = loop.ProbeOriginPRBase

// reconcileCloseOuts is the pass body. The supervisor runs it on its own goroutine
// (startCloseOutPass) and a test drives it directly.
func (d *Supervisor) reconcileCloseOuts() {
	d.runCloseOutPass(d.closeOutTargets())
}

// closeOutTarget is one project a pass has to work, snapshotted from the fleet
// before the pass leaves the reconcile goroutine. The pass itself runs on its own
// goroutine, so it must not read instance state that reconcile is concurrently
// changing: the config pointer and the slug are all it needs, and both are fixed
// for the life of one pass.
type closeOutTarget struct {
	cfg  *config.Config
	slug string
}

// closeOutTargets lists the projects the running fleet drives. A stopped, removed
// or refused instance is not driving its project: the operator's stop is about that
// project's autonomous work, integration included, so its approved queue waits for
// the agent path (or for the next instance that does run it).
func (d *Supervisor) closeOutTargets() []closeOutTarget {
	var out []closeOutTarget
	seen := map[string]bool{}
	for _, inst := range d.instances {
		if inst.removed || inst.refused || inst.desired != RosterDesiredRunning {
			continue
		}
		inst.mu.Lock()
		cfg := inst.cfg
		inst.mu.Unlock()
		if cfg == nil {
			continue
		}
		for _, slug := range closeOutProjects(cfg) {
			if seen[slug] {
				continue
			}
			seen[slug] = true
			out = append(out, closeOutTarget{cfg: cfg, slug: slug})
		}
	}
	return out
}

// startCloseOutPass runs one pass OFF the reconcile goroutine.
//
// reconcile is the supervisor's only control path: it reaps exited children,
// respawns due ones, arms restarts and publishes status, and it all happens in the
// select loop between polls. A close-out judgement can poll a `gh` read for up to
// 90 seconds, so running the pass inline held that path for the whole pass on every
// tick. One pass at a time: the fleet's next poll is worked by the running pass's
// next visit, and the queue is small enough that serialising it costs nothing.
func (d *Supervisor) startCloseOutPass() {
	if !d.closeOutBusy.CompareAndSwap(false, true) {
		return
	}
	targets := d.closeOutTargets()
	d.closeOutWG.Add(1)
	go func() {
		defer d.closeOutWG.Done()
		defer d.closeOutBusy.Store(false)
		d.runCloseOutPass(targets)
	}()
}

// waitCloseOut is the bounded wait a fleet stop gives an in-flight pass: its work
// is cancelled already, but a claim it had taken still has to be handed back before
// the process exits (see closeOutCleanupCtx).
func (d *Supervisor) waitCloseOut() {
	done := make(chan struct{})
	go func() {
		d.closeOutWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeOutCallTimeout + 5*time.Second):
		log.Print("close-out: a pass was still running at shutdown - a claim it had taken may stay held until its lease expires")
	}
}

// runCloseOutPass works one snapshot's projects within the pass budget. It never
// returns an error: every outcome is a log line, because a close-out that could not
// run must not stop the fleet from being reconciled - the approved task is still
// waiting for the agent path.
func (d *Supervisor) runCloseOutPass(targets []closeOutTarget) {
	ctx := d.baseCtx()
	budget := time.Now().Add(closeOutPassBudget)
	// One base cache per pass: every task of the same repo resolves the same
	// branch, so probing origin per task is the same answer at N times the cost.
	bases := newCloseOutBases()
	// approved is the union of the queues this pass read, and whole records
	// whether it read them all: a task missing from a queue that was never read
	// is not a task that left the queue, so a partial pass must not sweep.
	approved := map[string]bool{}
	whole := true
	for _, t := range targets {
		if ctx.Err() != nil || time.Now().After(budget) {
			whole = false
			break
		}
		if !d.reconcileCloseOutProject(ctx, t.cfg, t.slug, budget, bases, approved) {
			whole = false
		}
	}
	if whole {
		d.pruneCloseOutState(approved)
	}
}

// closeOutProjects lists the project slugs one instance's config drives: its
// roster projects when it names any, else the slug its MCP endpoint carries (the
// single-project shape).
func closeOutProjects(cfg *config.Config) []string {
	var out []string
	for _, p := range cfg.Projects {
		if slug := strings.TrimSpace(p.Slug); slug != "" {
			out = append(out, slug)
		}
	}
	if len(out) == 0 {
		if slug := strings.TrimSpace(cfg.Slug()); slug != "" {
			out = append(out, slug)
		}
	}
	return out
}

// reconcileCloseOutProject stands the pass up or down for one project, then works
// its approved queue. ctx is the pass's own (the supervisor lifetime): a fleet stop
// cancels it, and every call below therefore aborts rather than finishing out its
// timeout. bases carries the pass's memoised PR bases.
//
// It reports whether the pass learned this project's approved queue, which is what
// lets the end-of-pass sweep prune safely: an unread queue is not an empty one. An
// operator-integrated project reports TRUE with nothing added - the close-out never
// creates state for it, so there is nothing to keep for it either.
func (d *Supervisor) reconcileCloseOutProject(ctx context.Context, cfg *config.Config, slug string, budget time.Time, bases *closeOutBases, approved map[string]bool) bool {
	client := d.closeOutFor(cfg, slug)
	if client == nil {
		d.closeOutSay("client:"+slug, "close-out %s: no plane endpoint could be resolved - standing down", slug)
		return false
	}
	callCtx, cancel := context.WithTimeout(ctx, closeOutCallTimeout)
	defer cancel()

	policy, err := client.ProjectPolicy(callCtx)
	if err != nil {
		d.closeOutSay("policy-err:"+slug, "close-out %s: could not read the project policy (%v) - standing down", slug, err)
		return false
	}
	// The dial is the operator's turn signal: under `human` an approved-but-unmerged
	// task belongs to them, and this path has nothing to say about it. Anything that
	// is not an explicit `clanker` reads the same way — merging on a dial we could
	// not read is exactly the direction this path must not fail in.
	if !strings.EqualFold(policy.AuthorityIntegrate, "clanker") {
		d.closeOutSay("policy:"+slug, "close-out %s: authorityIntegrate is %q, not %q - approved tasks are not merged here", slug, policy.AuthorityIntegrate, "clanker")
		return true
	}

	queue, err := client.ApprovedForCloseOut(callCtx)
	if err != nil {
		d.closeOutSay("queue-err:"+slug, "close-out %s: could not list the approved queue (%v) - standing down", slug, err)
		return false
	}
	// The whole served page is this pass's knowledge of the queue, including the
	// tasks it will not reach before the budget: they are still approved.
	for _, task := range queue.Tasks {
		approved[task.ID] = true
	}
	if len(queue.Tasks) == 0 {
		return true // an empty close-out queue is not news
	}
	if queue.Total > len(queue.Tasks) {
		d.closeOutSay("queue-cap:"+slug, "close-out %s: %d approved task(s) waiting but only %d were served - the rest wait for the agent close-out", slug, queue.Total, len(queue.Tasks))
	}
	for _, task := range queue.Tasks {
		if time.Now().After(budget) || ctx.Err() != nil {
			d.closeOutSay("budget:"+slug, "close-out %s: this poll's close-out budget is spent - the remaining approved tasks wait for the next poll", slug)
			return false
		}
		d.reconcileCloseOutTask(ctx, cfg, slug, client, task, bases)
	}
	return true
}

// reconcileCloseOutTask takes one approved task as far as the deterministic path
// can, and says why it stopped otherwise. parent is the pass's cancellable context:
// a fleet stop aborts this task's judgement, claim and merge.
func (d *Supervisor) reconcileCloseOutTask(parent context.Context, cfg *config.Config, slug string, client plane.CloseOutAPI, task plane.CloseOutTask, bases *closeOutBases) {
	label := strings.TrimSpace(task.Ref)
	if label == "" {
		label = task.ID
	}
	key := "task:" + task.ID

	if task.Branch == "" {
		d.closeOutSay(key, "close-out: %s is approved but records no branch, so there is no PR to find - leaving it to the agent close-out", label)
		return
	}
	if task.Repo == "" {
		d.closeOutSay(key, "close-out: %s is approved but names no repo, so its checkout cannot be located - leaving it to the agent close-out", label)
		return
	}
	dir, err := closeOutRepoDir(cfg, d.o.WorkdirRoot, slug, task.Repo)
	if err != nil {
		d.closeOutSay(key, "close-out: %s cannot be closed out from here: %v - leaving it to the agent close-out", label, err)
		return
	}

	ctx, cancel := context.WithTimeout(parent, closeOutTaskTimeout)
	defer cancel()
	verifier := d.closeOutVerifier(dir)

	prs, err := verifier.OpenPRs(ctx, dir, task.Repo, task.Branch)
	if err != nil {
		d.closeOutSay(key, "close-out: %s could not be asked about its pull request (%v) - leaving it to the agent close-out", label, err)
		return
	}
	switch len(prs) {
	case 0:
		d.closeOutSay(key, "close-out: %s has no OPEN pull request whose head is %s - leaving it to the agent close-out", label, task.Branch)
		return
	case 1:
	default:
		d.closeOutSay(key, "close-out: %s has %d open pull requests whose head is %s (%s) - refusing to guess which one carries the delivery", label, len(prs), task.Branch, prBases(prs))
		return
	}
	pr := prs[0]

	// The base is the merge target, resolved with the SAME rule the phase driver
	// briefs a session with (config.ResolvePRBase): the operator's per-repo
	// pr_base, else the project's integration branch when origin has it, else
	// origin's default branch, else the integration branch. Resolving it from
	// config alone made the close-out refuse the very PRs the briefs asked for.
	// The pass memoises the answer per repo (closeOutBases), so a queue of tasks
	// in one repo probes origin once; the line is per repo for the same reason.
	// The promotion branch is refused outright: a PR based on main is never
	// merged here, because `staging -> main` (or `integration -> main`) is the
	// operator's release, not a task delivery.
	res := bases.resolve(ctx, dir, cfg, slug, task.Repo)
	base := res.Base
	switch {
	case res.Source == config.PRBaseOriginDefault:
		d.closeOutSay("base:"+slug+":"+task.Repo, "close-out: %s integrates on %q - origin's default branch, because %q is not on origin", task.Repo, base, res.Integration)
	case res.Err != nil:
		d.closeOutSay("base:"+slug+":"+task.Repo, "close-out: %s origin could not be read to resolve its merge target (%v) - using the integration branch %q", task.Repo, res.Err, base)
	}
	if isPromotionBranch(base) {
		d.closeOutSay(key, "close-out: %s integrates into %s, which is a release branch - promotion is the operator's, so approved tasks here are not merged by the fleet", label, base)
		return
	}

	// The judgement is the delivery gate's own (internal/delivery), so this path
	// can never call a PR greener than the gate that judges an agent's delivery.
	state := verifier.JudgePR(ctx, dir, task.Repo, pr.Number)

	// The base is checked on the JUDGED read, not the listing above: the two are
	// separate calls, and a PR retargeted between them must not be merged into the
	// target the older read named.
	if state.Base != "" && state.Base != base {
		if isPromotionBranch(state.Base) {
			d.closeOutSay(key, "close-out: %s PR #%d targets %q, a promotion branch, not this project's merge target %q - a release-targeted PR is the operator's promotion; leaving it", label, state.Number, state.Base, base)
		} else {
			d.closeOutSay(key, "close-out: %s PR #%d targets %q, not this project's merge target %q - refusing to merge a PR based on another branch; leaving it", label, state.Number, state.Base, base)
		}
		return
	}

	switch state.Verdict {
	case delivery.PRReady:
		// Fail CLOSED on an unproven merge target: this path merges into one
		// resolved branch, so a judged read that names no base has not shown that
		// the PR targets it. (JudgePR refuses such a read too; the guard is
		// repeated here because this is the only place a merge is authorised.)
		if state.Base == "" {
			d.closeOutSay(key, "close-out: %s PR #%d is ready but was judged with no base branch, so its merge target is not proven to be %q - leaving it to the agent close-out", label, state.Number, base)
			return
		}
	case delivery.PRConflict:
		paths, known := d.logCloseOutConflict(ctx, verifier, dir, label, key, task, state, base)
		// A conflict needs judgement, so the deterministic path cannot finish it —
		// but the fleet can still handle it without waiting for whichever clanker
		// finishes its current task: a short conflict-only session (CLA-759) on
		// the fleet's implement model. The session's brief is the narrow one (merge
		// the base, only the conflicting hunks, the repo gate, push); it claims
		// nothing, because this path takes the claim first. An unproven base is
		// the one conflict the session cannot be briefed for: it must merge a
		// named branch, and guessing which one is the wrong direction.
		if state.Base == "" {
			return
		}
		if known && len(paths) == 0 {
			// The evidence came back with NO conflicting paths: the two tips merge
			// cleanly, so GitHub's CONFLICTING read is stale (its mergeable state lags
			// a push). Starting a session here would be a SECOND paid attempt at a
			// conflict that no longer exists — the shape a mechanical resolution
			// leaves behind, since it pushes a new head and the task stays approved.
			// Leave it to the deterministic close-out, which merges it as soon as the
			// stale read catches up.
			d.closeOutSay(key+":conflict-stale", "close-out: %s PR #%d still reads CONFLICTING with %s, but the fetched tips merge cleanly - the conflict state is stale, so no conflict session was started; leaving it to the deterministic close-out", label, state.Number, base)
			return
		}
		d.startConflictSession(ctx, cfg, slug, client, task, state.Number, base, paths, dir)
		return
	default:
		d.closeOutSay(key, "close-out: %s PR #%d is not close-out ready - %s", label, state.Number, state.Check.Detail)
		return
	}

	// Claim BEFORE merging: the claim is the lock that keeps an agent close-out and
	// this path from both integrating the same PR.
	claim, err := client.ClaimCloseOut(ctx, task.ID)
	if err != nil {
		d.closeOutSay(key, "close-out: %s PR #%d is clean but the claim was refused (%v) - leaving it to whoever holds it", label, state.Number, err)
		return
	}
	runID := claim.RunID

	// From here on the claim is ours, so every failure up to the merge has to
	// hand it back: a task left claimed by a run that has stopped is a task nobody
	// is working. AFTER a merge lands there is deliberately no release path (see
	// settleMergedCloseOut): the PR is no longer OPEN, so releasing would strand
	// the task on `approved` with nothing to find.
	//
	// The release attests checkGreen false because the MERGE is what failed: a
	// green-but-unmergeable PR (branch protection, a base that moved) is bounded
	// only by the plane's CI-repair ladder, and attesting green would leave it
	// claimed, refused and released every poll with no escalation.
	release := func(reason string, args ...any) {
		rctx, rcancel := d.closeOutCleanupCtx()
		defer rcancel()
		if rerr := client.ReleaseCloseOut(rctx, task.ID, runID, false); rerr != nil {
			log.Printf("close-out: %s claim could NOT be released (%v) - the task stays claimed until its lease expires", label, rerr)
			d.closeOutSay(key, "close-out: %s %s - and the claim could not be released; its lease must expire before an agent can take it", label, fmt.Sprintf(reason, args...))
			return
		}
		d.closeOutSay(key, "close-out: %s %s - claim released, left approved for the agent close-out", label, fmt.Sprintf(reason, args...))
	}

	// The merge names the base and the judged head itself. The PR's own base can be
	// retargeted at any instant, and the pull-request merge endpoint would follow
	// it; this request cannot be redirected, and the head pin refuses a push that
	// landed after the judgement.
	mergeSHA, err := verifier.MergePR(ctx, dir, task.Repo, state.Number, base, state.HeadOID)
	if err != nil {
		release("could not merge PR #%d into %s (%v)", state.Number, base, err)
		return
	}
	check := d.verifyMerge(ctx, verifier, dir, base, mergeSHA)
	// The merge is ON THE REMOTE now: MergePR only returns a commit GitHub made,
	// so the PR is no longer OPEN and there is deliberately no release path from
	// here. settleMergedCloseOut retries the close and, if it still cannot
	// finish, records the delivery and leaves the claim to its lease - because a
	// release would put the task back on `approved` with no PR for either path to
	// find again.
	d.settleMergedCloseOut(client, label, key, task.ID, runID, state.Number, base, mergeSHA, check)
}

// settleMergedCloseOut finishes a task whose PR this pass has already merged.
//
// It has NO release path, by construction: the merge landed, so the task must
// never return to `approved` — there is no open PR left for the agent close-out
// to find. A close that fails transiently (a 502, a timeout) is retried within
// one cleanup budget, and a close that still cannot be finished leaves the claim
// to its lease with the delivery recorded, so the successor can close the task
// from the merge SHA and PR rather than rediscovering a merge that already
// happened.
func (d *Supervisor) settleMergedCloseOut(client plane.CloseOutAPI, label, key, taskID, runID string, pr int, base, mergeSHA string, check delivery.Check) {
	verified := check.Status == delivery.Pass
	decl := plane.CloseOutDelivery{
		Commit:            mergeSHA,
		PR:                fmt.Sprintf("#%d", pr),
		IntegrationBranch: base,
		MergeVerified:     verified,
	}
	if err := d.completeCloseOut(client, label, taskID, runID, decl); err != nil {
		d.recordMergedDelivery(client, label, key, taskID, runID, decl, err)
		return
	}
	if !verified {
		d.closeOutSay(key, "close-out: %s PR #%d merged as %s into %s, but the merge could not be verified (%s) - task closed done with mergeVerified:false", label, pr, shortSHA(mergeSHA), base, check.Detail)
		return
	}
	d.closeOutSay(key, "close-out: %s PR #%d merged as %s into %s (merge commit is an ancestor of the remote tip) - task closed done", label, pr, shortSHA(mergeSHA), base)
}

// completeCloseOut closes the task done, retrying a TRANSIENT plane failure
// (transport or 5xx) within one cleanup budget. A tool-level refusal is the
// plane's answer and is returned at once: retrying it would only repeat it.
func (d *Supervisor) completeCloseOut(client plane.CloseOutAPI, label, taskID, runID string, decl plane.CloseOutDelivery) error {
	ctx, cancel := d.closeOutCleanupCtx()
	defer cancel()
	wait := closeOutCompleteBackoff
	var err error
	for attempt := 1; attempt <= closeOutCompleteAttempts; attempt++ {
		if err = client.CompleteCloseOut(ctx, taskID, runID, decl); err == nil {
			return nil
		}
		if !plane.Transient(err) || attempt == closeOutCompleteAttempts {
			return err
		}
		log.Printf("close-out: %s PR %s merged as %s, but closing the task failed transiently (attempt %d of %d: %v) - retrying",
			label, decl.PR, decl.Commit, attempt, closeOutCompleteAttempts, err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait *= 2
	}
	return err
}

// recordMergedDelivery is the recovery write for a merge that landed but could
// not be closed. The claim is LEFT HELD on purpose: releasing it would return a
// merged task to approved with no open PR, and the lease expiry is the safe
// hand-back - it offers the task as a takeover, and this record tells the
// successor the merge SHA and PR are already merged.
func (d *Supervisor) recordMergedDelivery(client plane.CloseOutAPI, label, key, taskID, runID string, decl plane.CloseOutDelivery, closeErr error) {
	ctx, cancel := d.closeOutCleanupCtx()
	recErr := client.RecordCloseOutDelivery(ctx, taskID, runID, decl)
	cancel()
	if recErr == nil {
		d.closeOutSay(key, "close-out: %s PR %s merged as %s into %s, but closing the task failed (%v) - delivery RECORDED (mergeVerified:%t) so a successor can close it; the claim is left held rather than returning a merged PR to approved",
			label, decl.PR, decl.Commit, decl.IntegrationBranch, closeErr, decl.MergeVerified)
		return
	}
	d.closeOutSay(key, "close-out: %s PR %s merged as %s into %s, but closing the task failed (%v) AND the delivery could not be recorded (%v) - the claim is left held until its lease expires; a successor must close the task from that merge commit and PR",
		label, decl.PR, decl.Commit, decl.IntegrationBranch, closeErr, recErr)
}

// verifyMerge settles that the merge commit is an ancestor of base on origin. Two
// properties matter beyond the check itself:
//
//   - it reads in a THROWAWAY clone (delivery.IsolatedClone), never in the fleet's
//     shared checkout, whose refs and ref locks belong to the sessions working
//     there;
//   - it retries within the verifier's poll window, because a merge GitHub has just
//     performed is not always visible to the first read. Everything that still
//     cannot be settled stays Unknown, never a pass.
func (d *Supervisor) verifyMerge(ctx context.Context, verifier *delivery.Verifier, repo, base, sha string) delivery.Check {
	iso, cleanup, err := verifier.IsolatedClone(ctx, repo, "origin")
	if err != nil {
		return delivery.Check{Kind: delivery.CommitMerged, Status: delivery.Unknown, Detail: fmt.Sprintf(
			"no throwaway checkout could be made to verify the merge in: %v", err)}
	}
	defer cleanup()

	// Retry at a fraction of the window, so a small test window still gets
	// several attempts and the production one settles within a couple of seconds.
	window := verifier.PollWindow()
	interval := window / 5
	if interval > 2*time.Second {
		interval = 2 * time.Second
	}
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	deadline := time.Now().Add(window)
	for {
		check := verifier.MergedOnRemote(ctx, iso, "origin", base, sha)
		if check.Status == delivery.Pass || ctx.Err() != nil || !time.Now().Before(deadline) {
			return check
		}
		select {
		case <-ctx.Done():
			return check
		case <-time.After(interval):
		}
	}
}

// closeOutCleanupCtx is the bounded, cancellation-INDEPENDENT context for the
// writes that must still happen when a fleet stop lands mid-close-out: releasing a
// claim this path took, and recording a delivery whose merge already landed.
// Everything BEFORE that point runs on the pass's cancellable context, so a stop
// aborts a judgement, a claim or a merge instead of waiting out its 90-second
// budget.
func (d *Supervisor) closeOutCleanupCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(d.baseCtx()), closeOutCallTimeout)
}

// conflictEvidence is what one conflicting task's head/base pair is known to
// conflict on: the pair the paths were computed against, the paths themselves, and
// whether they were actually computed. An empty KNOWN result means the fetched tips
// merge cleanly — a different fact from "the paths could not be read", and the one
// that says a CONFLICTING read is stale. Cached so the conflict session started in
// the same pass is briefed with them, and so a later pass for the same unchanged
// pair neither re-logs the evidence nor has to recompute it.
type conflictEvidence struct {
	pair  string
	paths []string
	known bool
}

// logCloseOutConflict records the evidence for a conflicting PR — the task, the
// PR, the base and EVERY conflicting path — and returns those paths for the
// conflict-only session that follows, with whether they could be computed at all
// (FALSE when a tip, a clone, a fetch or the merge itself failed; TRUE when the
// paths are the truth for this pair, including the empty case where the two tips
// merge cleanly).
//
// One line per head/base PAIR, not one per poll. The pair is the judged head's oid
// plus the base tip read fresh from the remote (`ls-remote`, not a fetch), so a push
// that changes the head — or a base that moves under it — earns a new line, while an
// unchanged conflict is silent and its cached paths are returned instead. That is
// what keeps this a greppable evidence trail as well as the session's input. The
// paths themselves are computed in a throwaway clone, so the evidence never touches
// the checkout the fleet's sessions are using.
func (d *Supervisor) logCloseOutConflict(ctx context.Context, verifier *delivery.Verifier, dir, label, key string, task plane.CloseOutTask, state delivery.PRState, base string) ([]string, bool) {
	baseTip, err := verifier.RemoteTip(ctx, dir, "origin", base)
	if err != nil {
		d.closeOutSay(key+":conflict-tip", "close-out: %s PR #%d is CONFLICTING with %s, but its base tip could not be read (%v)", label, state.Number, base, err)
		return nil, false
	}
	pair := state.HeadOID + "@" + baseTip
	if d.closeOutConflicts == nil {
		d.closeOutConflicts = map[string]conflictEvidence{}
	}
	if prev, ok := d.closeOutConflicts[task.ID]; ok && prev.pair == pair {
		return prev.paths, prev.known // this exact head/base pair has already been read
	}

	head := strings.TrimSpace(state.Head)
	if head == "" {
		head = task.Branch
	}
	// Naming the paths needs the two tips as OBJECTS, so it fetches. That fetch
	// happens in a throwaway clone, never in the fleet's shared checkout: the
	// forced refspec would move origin/<base> under the sessions reading it, and
	// the fetch's ref locks are ones those sessions also take.
	iso, cleanup, err := verifier.IsolatedClone(ctx, dir, "origin")
	if err != nil {
		d.closeOutSay(key+":conflict-iso", "close-out: %s PR #%d is CONFLICTING with %s, but a throwaway checkout to name the paths could not be made (%v)", label, state.Number, base, err)
		return nil, false
	}
	defer cleanup()
	dir = iso
	if err := verifier.FetchRef(ctx, dir, "origin", base); err != nil {
		d.closeOutSay(key+":conflict-fetch", "close-out: %s PR #%d is CONFLICTING with %s, but the base could not be fetched to name the paths (%v)", label, state.Number, base, err)
		return nil, false
	}
	if err := verifier.FetchRef(ctx, dir, "origin", head); err != nil {
		d.closeOutSay(key+":conflict-fetch", "close-out: %s PR #%d is CONFLICTING with %s, but its head could not be fetched to name the paths (%v)", label, state.Number, base, err)
		return nil, false
	}
	paths, err := verifier.ConflictPaths(ctx, dir, "origin/"+base, "origin/"+head)
	if err != nil {
		d.closeOutSay(key+":conflict-paths", "close-out: %s PR #%d is CONFLICTING with %s (head %s@%s, base %s@%s), but its conflicting paths could not be computed (%v)", label, state.Number, base, head, shortSHA(state.HeadOID), base, shortSHA(baseTip), err)
		return nil, false
	}
	d.closeOutConflicts[task.ID] = conflictEvidence{pair: pair, paths: paths, known: true}
	named := strings.Join(paths, ", ")
	if named == "" {
		named = "(none reported - the fetched tips merged cleanly, so the PR's state is stale)"
	}
	d.closeOutSay(key, "close-out: %s PR #%d is CONFLICTING with %s (head %s@%s, base %s@%s) - conflicting paths: %s",
		label, state.Number, base, head, shortSHA(state.HeadOID), base, shortSHA(baseTip), named)
	return paths, true
}

// closeOutSay logs one close-out line, unless the line for that key is unchanged —
// the same "say it once per change" discipline the fleet status listing keeps, so an
// idempotent poll stays quiet.
func (d *Supervisor) closeOutSay(key, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if d.closeOutSaid == nil {
		d.closeOutSaid = map[string]string{}
	}
	if d.closeOutSaid[key] == line {
		return
	}
	d.closeOutSaid[key] = line
	log.Print(line)
}

// pruneCloseOutState drops the per-task entries of the say maps, the conflict
// evidence cache and the conflict attempt budget for tasks that are no longer
// approved, so a supervisor that runs for weeks does not accumulate one entry per
// task it has ever seen.
//
// approved is the union of the approved queues this pass read. A task gets ONE
// pass of grace (closeOutSeenTasks holds the previous pass's set), and the task a
// conflict session is still working is kept outright: both are tasks a claim has
// taken out of the approved queue while their state is still live, and dropping a
// conflict budget there would reset the paid-retry bound (CLA-759). The keys
// scoped to a project, repo or the fleet are not task state and stay.
func (d *Supervisor) pruneCloseOutState(approved map[string]bool) {
	keep := make(map[string]bool, len(approved)+len(d.closeOutSeenTasks)+1)
	for id := range approved {
		keep[id] = true
	}
	for id := range d.closeOutSeenTasks {
		keep[id] = true
	}
	d.conflictMu.Lock()
	if d.conflictActive != "" {
		keep[d.conflictActive] = true
	}
	for id := range d.conflictTried {
		if !keep[id] {
			delete(d.conflictTried, id)
		}
	}
	for key := range d.conflictSaid {
		if id, ok := conflictTaskKey(key); ok && !keep[id] {
			delete(d.conflictSaid, key)
		}
	}
	d.conflictMu.Unlock()
	for id := range d.closeOutConflicts {
		if !keep[id] {
			delete(d.closeOutConflicts, id)
		}
	}
	for key := range d.closeOutSaid {
		if id, ok := closeOutTaskKey(key); ok && !keep[id] {
			delete(d.closeOutSaid, key)
		}
	}
	d.closeOutSeenTasks = approved
}

// closeOutTaskKey is the task ID a closeOutSaid key names: every per-task key
// starts "task:<id>", optionally followed by ":<suffix>" for a more specific
// line about the same task.
func closeOutTaskKey(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, "task:")
	if !ok {
		return "", false
	}
	id, _, _ := strings.Cut(rest, ":")
	return id, id != ""
}

// conflictTaskKey is the task ID a conflictSaid key names. The conflict path's
// per-task keys are "<prefix>:<id>"; its slug, harness and fleet keys are not
// task-scoped, so they are never pruned here.
func conflictTaskKey(key string) (string, bool) {
	for _, prefix := range []string{"paused:", "routespaused:", "busy:", "worktree:", "exhausted:", "slot:", "claimed:"} {
		if id, ok := strings.CutPrefix(key, prefix); ok && id != "" {
			return id, true
		}
	}
	return "", false
}

// closeOutFor builds the close-out client for one project's endpoint, mirroring
// planeFor: the project's own endpoint when the config names one, the backlog
// endpoint otherwise, then the roster origin's derived MCP path (which is what lets
// the supervisor's tests point the whole fleet at one server).
func (d *Supervisor) closeOutFor(cfg *config.Config, project string) plane.CloseOutAPI {
	endpoint := ""
	if cfg != nil {
		if project != "" {
			for _, p := range cfg.Projects {
				if strings.EqualFold(p.Slug, project) {
					endpoint = cfg.ProjectEndpoint(p)
					break
				}
			}
		}
		if endpoint == "" {
			endpoint = cfg.BacklogEndpoint()
		}
	}
	if endpoint == "" {
		endpoint = mcpEndpointFromRoster(d.o.RosterURL, project)
	}
	if endpoint == "" || d.o.APIKey == "" {
		return nil
	}
	return plane.NewCloseOutAPI(endpoint, d.o.APIKey)
}

// closeOutVerifier builds the local verifier one close-out judges a repo with. The
// hook exists for the same reason PlaneFor does: the PR judgement's poll window is
// 30 seconds in production, which no test can wait out, and its gh binary is the
// package's own test seam.
func (d *Supervisor) closeOutVerifier(dir string) *delivery.Verifier {
	if d.o.CloseOutVerifier != nil {
		return d.o.CloseOutVerifier(dir)
	}
	return delivery.New(dir, "")
}

// closeOutRepoDir resolves the checkout the close-out works a task's repo in. Two
// sources, in the order the children themselves use:
//
//  1. the fleet's DERIVED clone at <root>/<project slug>/<repo name>, when a machine
//     workdir root is stated (the layout the workdir derivation materializes);
//  2. the operator's own declared checkout, resolved exactly as a session's spawn cwd
//     is (`repos` entry, primary, then the workdir conventions).
//
// Whichever answers is VERIFIED to be a checkout of that repo before it is used:
// fetching into, and merging the PR of, whatever happens to sit at a name would be a
// correctness failure invisible in the log — the repo the PR belongs to is decided by
// the task's own repo identity, not by the directory layout.
func closeOutRepoDir(cfg *config.Config, root, slug, repo string) (string, error) {
	var tried []string
	if strings.TrimSpace(root) != "" {
		if projectDir, err := ProjectRoot(root, slug); err == nil {
			dir := filepath.Join(projectDir, basenameOfRepo(repo))
			if err := checkCloseOutCheckout(dir, repo); err == nil {
				return dir, nil
			}
			tried = append(tried, dir)
		}
	}
	workdir := closeOutProjectWorkdir(cfg, slug)
	if dir, err := config.ResolveCheckout(cfg.ReposFor(slug), cfg.PrimaryRepoFor(slug), workdir, repo); err == nil {
		if err := checkCloseOutCheckout(dir, repo); err == nil {
			return dir, nil
		}
		tried = append(tried, dir)
	}
	if len(tried) == 0 {
		return "", fmt.Errorf("no checkout of %s could be located (no machine workdir root and no configured checkout)", repo)
	}
	return "", fmt.Errorf("no checkout of %s: %s", repo, strings.Join(tried, ", "))
}

// closeOutProjectWorkdir is the workdir a project's sessions run in, for the
// configured-checkout fallback: its own when the project names one, else the
// config's.
func closeOutProjectWorkdir(cfg *config.Config, slug string) string {
	for _, p := range cfg.Projects {
		if strings.EqualFold(p.Slug, slug) && strings.TrimSpace(p.WorkDir) != "" {
			return p.WorkDir
		}
	}
	return cfg.WorkDir
}

// checkCloseOutCheckout verifies dir is a real directory (not a symlink) and a
// checkout of repo, by its attributable origin remote. See checkoutOriginRemote for
// what "attributable" means and delivery.MatchesRepoSlug for the comparison.
func checkCloseOutCheckout(dir, repo string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("no checkout at %s (%v)", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory the fleet owns", dir)
	}
	remote, err := checkoutOriginRemote(dir)
	if err != nil {
		return fmt.Errorf("%s is not a checkout (%v)", dir, err)
	}
	if !delivery.MatchesRepoSlug(remote, repo) {
		return fmt.Errorf("%s is a checkout of %s, not %s", dir, remote, repo)
	}
	return nil
}

// closeOutBases memoises each repo's resolved PR base for the lifetime of ONE
// pass. The probe is three process spawns and a network round-trip, and the
// supervisor starts a pass on every roster poll, so an approved queue of N tasks
// in one repo must not pay it N times - the answer is the same for every task of
// that repo. A failed probe is memoised too: the pass has already learned it
// cannot read origin, and retrying it per task would spend the same timeout N
// times over. A pass works its queue on one goroutine, so the map needs no lock.
// The zero value is not usable; build one with newCloseOutBases.
type closeOutBases struct {
	seen map[string]config.PRBaseResolution
}

func newCloseOutBases() *closeOutBases {
	return &closeOutBases{seen: map[string]config.PRBaseResolution{}}
}

// resolve is the branch a task PR must target, with the SAME rule the phase
// driver briefs a session with (config.ResolvePRBase): the operator's per-repo
// pr_base, else the project's integration branch when origin has it, else origin's
// default branch, else the integration branch. A close-out that judged a PR
// against a branch the brief never named refused correct deliveries.
//
// The probe gets the close-out's own short bound, never the driver's 30s one: it
// runs inside a task budget shared with the PR judgement, the claim and the merge
// check. Only the first task of a repo pays it; the rest read the memo.
func (c *closeOutBases) resolve(ctx context.Context, dir string, cfg *config.Config, slug, repo string) config.PRBaseResolution {
	integration := cfg.IntegrationBranchFor(slug)
	key := slug + "\x00" + repo + "\x00" + integration
	if res, ok := c.seen[key]; ok {
		return res
	}
	res := config.ResolvePRBase(cfg, slug, repo, func(integration string) (bool, string, error) {
		pctx, cancel := context.WithTimeout(ctx, closeOutBaseProbeTimeout)
		defer cancel()
		return closeOutOriginProbe(pctx, dir, integration)
	})
	c.seen[key] = res
	return res
}

// isPromotionBranch reports whether a branch is a release line the fleet must never
// merge into: the staging→main promotion is the operator's act.
func isPromotionBranch(branch string) bool {
	switch strings.ToLower(strings.TrimSpace(branch)) {
	case "main", "master":
		return true
	}
	return false
}

// prBases renders the open PRs one branch has, with the base each one targets, for
// the "refusing to guess" line — the base is what tells the operator whether the
// duplicates are two deliveries or one retargeted PR.
func prBases(prs []delivery.PRRef) string {
	parts := make([]string, 0, len(prs))
	for _, pr := range prs {
		base := pr.Base
		if base == "" {
			base = "no base"
		}
		parts = append(parts, fmt.Sprintf("#%d -> %s", pr.Number, base))
	}
	return strings.Join(parts, ", ")
}

// shortSHA abbreviates a commit for a log line.
func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
