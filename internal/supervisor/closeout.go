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
// project's integration branch, and does the mechanical rest:
//
//  1. CLAIM the task first. The claim is atomic and first-wins, so an agent
//     close-out and this path can never both merge the same PR.
//  2. Merge the PR as a MERGE COMMIT into the CONFIGURED integration branch. The
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
//   - a judged PR whose base is missing or is not the configured integration
//     branch: an unproven merge target is never merged;
//   - CONFLICTING, failing, pending, empty or unresolvable checks. CONFLICTING is
//     the one stand-down that carries evidence: the conflicting paths, computed
//     with `git merge-tree` against freshly fetched refs, logged once per
//     head/base pair rather than on every poll;
//   - a refused claim (someone else holds it);
//   - any failure after the claim: the claim is RELEASED, so the task returns to
//     `approved` and the agent close-out can take it. A task is never left claimed
//     by a run that has stopped. The one exception is a merge that landed but could
//     not be verified: that is closed with `mergeVerified: false`, because a
//     merged PR is no longer OPEN and releasing would strand the task forever.
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
)

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
	for _, t := range targets {
		if ctx.Err() != nil || time.Now().After(budget) {
			return
		}
		d.reconcileCloseOutProject(ctx, t.cfg, t.slug, budget)
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
// timeout.
func (d *Supervisor) reconcileCloseOutProject(ctx context.Context, cfg *config.Config, slug string, budget time.Time) {
	client := d.closeOutFor(cfg, slug)
	if client == nil {
		d.closeOutSay("client:"+slug, "close-out %s: no plane endpoint could be resolved - standing down", slug)
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, closeOutCallTimeout)
	defer cancel()

	policy, err := client.ProjectPolicy(callCtx)
	if err != nil {
		d.closeOutSay("policy-err:"+slug, "close-out %s: could not read the project policy (%v) - standing down", slug, err)
		return
	}
	// The dial is the operator's turn signal: under `human` an approved-but-unmerged
	// task belongs to them, and this path has nothing to say about it. Anything that
	// is not an explicit `clanker` reads the same way — merging on a dial we could
	// not read is exactly the direction this path must not fail in.
	if !strings.EqualFold(policy.AuthorityIntegrate, "clanker") {
		d.closeOutSay("policy:"+slug, "close-out %s: authorityIntegrate is %q, not %q - approved tasks are not merged here", slug, policy.AuthorityIntegrate, "clanker")
		return
	}

	queue, err := client.ApprovedForCloseOut(callCtx)
	if err != nil {
		d.closeOutSay("queue-err:"+slug, "close-out %s: could not list the approved queue (%v) - standing down", slug, err)
		return
	}
	if len(queue.Tasks) == 0 {
		return // an empty close-out queue is not news
	}
	if queue.Total > len(queue.Tasks) {
		d.closeOutSay("queue-cap:"+slug, "close-out %s: %d approved task(s) waiting but only %d were served - the rest wait for the agent close-out", slug, queue.Total, len(queue.Tasks))
	}
	for _, task := range queue.Tasks {
		if time.Now().After(budget) || ctx.Err() != nil {
			d.closeOutSay("budget:"+slug, "close-out %s: this poll's close-out budget is spent - the remaining approved tasks wait for the next poll", slug)
			return
		}
		d.reconcileCloseOutTask(ctx, cfg, slug, client, task)
	}
}

// reconcileCloseOutTask takes one approved task as far as the deterministic path
// can, and says why it stopped otherwise. parent is the pass's cancellable context:
// a fleet stop aborts this task's judgement, claim and merge.
func (d *Supervisor) reconcileCloseOutTask(parent context.Context, cfg *config.Config, slug string, client plane.CloseOutAPI, task plane.CloseOutTask) {
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
	// The promotion branch is refused outright: a PR based on main is never
	// merged here, because `staging -> main` (or `integration -> main`) is the
	// operator's release, not a task delivery.
	res := closeOutBase(ctx, dir, cfg, slug, task.Repo)
	base := res.Base
	switch {
	case res.Source == config.PRBaseOriginDefault:
		d.closeOutSay(key+":base", "close-out: %s integrates on %q - origin's default branch, because %q is not on origin", label, base, res.Integration)
	case res.Err != nil:
		d.closeOutSay(key+":base", "close-out: %s origin could not be read to resolve its merge target (%v) - using the integration branch %q", label, res.Err, base)
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
			d.closeOutSay(key, "close-out: %s PR #%d targets %q, a promotion branch, not the integration branch %q - a release-targeted PR is the operator's promotion; leaving it", label, state.Number, state.Base, base)
		} else {
			d.closeOutSay(key, "close-out: %s PR #%d targets %q, not the integration branch %q this project integrates on - refusing to merge a PR based on another branch; leaving it", label, state.Number, state.Base, base)
		}
		return
	}

	switch state.Verdict {
	case delivery.PRReady:
		// Fail CLOSED on an unproven merge target: this path merges into one
		// configured branch, so a judged read that names no base has not shown
		// that the PR targets it. (JudgePR refuses such a read too; the guard is
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
	runID, err := client.ClaimCloseOut(ctx, task.ID)
	if err != nil {
		d.closeOutSay(key, "close-out: %s PR #%d is clean but the claim was refused (%v) - leaving it to whoever holds it", label, state.Number, err)
		return
	}

	// From here on the claim is ours, so every failure has to hand it back: a task
	// left claimed by a run that has stopped is a task nobody is working.
	//
	// checkGreen is what the plane's CI-repair ladder reads: true means "the PR's
	// checks were green, so do not count this as a failed repair". That is right for
	// a post-merge write that failed, and wrong for a MERGE that failed - there the
	// repair ladder is the only thing bounding a green-but-unmergeable PR (branch
	// protection, a base that moved), which would otherwise be claimed, refused and
	// released every poll with no escalation.
	release := func(checkGreen bool, reason string, args ...any) {
		rctx, rcancel := d.closeOutCleanupCtx()
		defer rcancel()
		if rerr := client.ReleaseCloseOut(rctx, task.ID, runID, checkGreen); rerr != nil {
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
		release(false, "could not merge PR #%d into %s (%v)", state.Number, base, err)
		return
	}
	check := d.verifyMerge(ctx, verifier, dir, base, mergeSHA)
	if check.Status != delivery.Pass {
		// The merge DID land — MergePR only returns a commit GitHub made — so the
		// claim must not be handed back: the PR is no longer OPEN, and a release
		// would put the task back on `approved` with no PR for either path to find
		// again. Record the delivery honestly as unverified instead.
		decl := plane.CloseOutDelivery{
			Commit:            mergeSHA,
			PR:                fmt.Sprintf("#%d", state.Number),
			IntegrationBranch: base,
			MergeVerified:     false,
		}
		cctx, ccancel := d.closeOutCleanupCtx()
		err := client.CompleteCloseOut(cctx, task.ID, runID, decl)
		ccancel()
		if err != nil {
			release(true, "merged PR #%d as %s but the merge could not be verified (%s) and the plane refused to close the task (%v)", state.Number, shortSHA(mergeSHA), check.Detail, err)
			return
		}
		d.closeOutSay(key, "close-out: %s PR #%d merged as %s into %s, but the merge could not be verified (%s) - task closed done with mergeVerified:false", label, state.Number, shortSHA(mergeSHA), base, check.Detail)
		return
	}
	decl := plane.CloseOutDelivery{
		Commit:            mergeSHA,
		PR:                fmt.Sprintf("#%d", state.Number),
		IntegrationBranch: base,
		MergeVerified:     true,
	}
	cctx, ccancel := d.closeOutCleanupCtx()
	err = client.CompleteCloseOut(cctx, task.ID, runID, decl)
	ccancel()
	if err != nil {
		release(true, "merged PR #%d as %s but the plane refused to close the task (%v)", state.Number, shortSHA(mergeSHA), err)
		return
	}
	d.closeOutSay(key, "close-out: %s PR #%d merged as %s into %s (merge commit is an ancestor of the remote tip) - task closed done", label, state.Number, shortSHA(mergeSHA), base)
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

// closeOutBase resolves the branch a task PR must target, with the SAME rule the
// phase driver briefs a session with (config.ResolvePRBase): the operator's
// per-repo pr_base, else the project's integration branch when origin has it, else
// origin's default branch, else the integration branch. A close-out that judged a
// PR against a branch the brief never named refused correct deliveries.
func closeOutBase(ctx context.Context, dir string, cfg *config.Config, slug, repo string) config.PRBaseResolution {
	return config.ResolvePRBase(cfg, slug, repo, func(integration string) (bool, string, error) {
		return loop.ProbeOriginPRBase(ctx, dir, integration)
	})
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
