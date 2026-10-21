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
//  2. Merge the PR as a MERGE COMMIT. Never a squash or a rebase: the batch
//     integration flow depends on the history a merge commit keeps.
//  3. Verify the merge commit is an ancestor of the base on ORIGIN — the same
//     ancestor test the delivery gate records as `mergeVerified`, run on the
//     commit GitHub made.
//  4. Close the task done with that delivery, in the claim's run.
//
// Everything else stands down and leaves the task exactly where it was, for the
// agent close-out:
//
//   - the project's dial says the operator integrates (`authorityIntegrate: human`);
//   - no open PR, or more than one, for the task's recorded branch;
//   - a PR based on `main`/`master` — the staging→main promotion is the operator's;
//   - CONFLICTING, failing, pending, empty or unresolvable checks. CONFLICTING is
//     the one stand-down that carries evidence: the conflicting paths, computed
//     with `git merge-tree` against freshly fetched refs, logged once per
//     head/base pair rather than on every poll;
//   - a refused claim (someone else holds it);
//   - any failure after the claim: the claim is RELEASED, so the task returns to
//     `approved` and the agent close-out can take it. A task is never left claimed
//     by a run that has stopped.
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
	// reports) for longer than the fleet can tolerate. Work it does not reach is
	// left for the next poll.
	closeOutPassBudget = 4 * time.Minute
)

// reconcileCloseOuts is the pass, called from reconcile once the fleet's configs are
// current. It never returns an error: every outcome is a log line, because a
// close-out that could not run must not stop the fleet from being reconciled — the
// approved task is still waiting for the agent path.
func (d *Supervisor) reconcileCloseOuts() {
	if strings.TrimSpace(d.o.WorkdirRoot) == "" {
		d.closeOutSay("no-root", "close-out: no workdir_root is configured, so there is no checkout to verify a merge against - approved tasks stay with the agent close-out")
		return
	}
	budget := time.Now().Add(closeOutPassBudget)
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
			d.reconcileCloseOutProject(cfg, slug, budget)
		}
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
// its approved queue.
func (d *Supervisor) reconcileCloseOutProject(cfg *config.Config, slug string, budget time.Time) {
	client := d.closeOutFor(cfg, slug)
	if client == nil {
		d.closeOutSay("client:"+slug, "close-out %s: no plane endpoint could be resolved - standing down", slug)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.baseCtx()), closeOutCallTimeout)
	defer cancel()

	policy, err := client.ProjectPolicy(ctx)
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

	queue, err := client.ApprovedForCloseOut(ctx)
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
		if time.Now().After(budget) || d.baseCtx().Err() != nil {
			d.closeOutSay("budget:"+slug, "close-out %s: this poll's close-out budget is spent - the remaining approved tasks wait for the next poll", slug)
			return
		}
		d.reconcileCloseOutTask(cfg, slug, client, task)
	}
}

// reconcileCloseOutTask takes one approved task as far as the deterministic path
// can, and says why it stopped otherwise.
func (d *Supervisor) reconcileCloseOutTask(cfg *config.Config, slug string, client plane.CloseOutAPI, task plane.CloseOutTask) {
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
	dir, err := closeOutRepoDir(d.o.WorkdirRoot, slug, task.Repo)
	if err != nil {
		d.closeOutSay(key, "close-out: %s cannot be closed out from here: %v - leaving it to the agent close-out", label, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.baseCtx()), closeOutTaskTimeout)
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
		d.closeOutSay(key, "close-out: %s has %d open pull requests whose head is %s (%s) - refusing to guess which one carries the delivery", label, len(prs), task.Branch, prNumbers(prs))
		return
	}
	pr := prs[0]

	// The base is the merge target, and it is read from the project's own config
	// (the operator's per-repo PR base, else the project's integration branch).
	// The promotion branch is refused outright: a PR based on main is never
	// merged here, because `staging -> main` (or `integration -> main`) is the
	// operator's release, not a task delivery.
	base := closeOutBase(cfg, slug, task.Repo)
	if base == "" {
		d.closeOutSay(key, "close-out: %s has no integration branch configured for %s - leaving it to the agent close-out", label, task.Repo)
		return
	}
	if isPromotionBranch(base) {
		d.closeOutSay(key, "close-out: %s integrates into %s, which is a release branch - promotion is the operator's, so approved tasks here are not merged by the fleet", label, base)
		return
	}
	if pr.Base != base {
		d.closeOutSay(key, "close-out: %s PR #%d targets %q, not the integration branch %q - a release-targeted PR is the operator's promotion; leaving it", label, pr.Number, pr.Base, base)
		return
	}

	// The judgement is the delivery gate's own (internal/delivery), so this path
	// can never call a PR greener than the gate that judges an agent's delivery.
	state := verifier.JudgePR(ctx, dir, task.Repo, pr.Number)
	switch state.Verdict {
	case delivery.PRReady:
	case delivery.PRConflict:
		d.logCloseOutConflict(ctx, verifier, dir, label, key, task, state, base)
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
	release := func(reason string, args ...any) {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(d.baseCtx()), closeOutCallTimeout)
		defer rcancel()
		if rerr := client.ReleaseCloseOut(rctx, task.ID, runID); rerr != nil {
			log.Printf("close-out: %s claim could NOT be released (%v) - the task stays claimed until its lease expires", label, rerr)
			d.closeOutSay(key, "close-out: %s %s - and the claim could not be released; its lease must expire before an agent can take it", label, fmt.Sprintf(reason, args...))
			return
		}
		d.closeOutSay(key, "close-out: %s %s - claim released, left approved for the agent close-out", label, fmt.Sprintf(reason, args...))
	}

	mergeSHA, err := verifier.MergePR(ctx, dir, task.Repo, state.Number, state.HeadOID)
	if err != nil {
		release("could not merge PR #%d (%v)", state.Number, err)
		return
	}
	check := verifier.MergedOnRemote(ctx, dir, "origin", base, mergeSHA)
	if check.Status != delivery.Pass {
		release("merged PR #%d as %s but the merge could not be verified (%s)", state.Number, shortSHA(mergeSHA), check.Detail)
		return
	}
	decl := plane.CloseOutDelivery{
		Commit:            mergeSHA,
		PR:                fmt.Sprintf("#%d", state.Number),
		IntegrationBranch: base,
		MergeVerified:     true,
	}
	if err := client.CompleteCloseOut(ctx, task.ID, runID, decl); err != nil {
		release("merged PR #%d as %s but the plane refused to close the task (%v)", state.Number, shortSHA(mergeSHA), err)
		return
	}
	d.closeOutSay(key, "close-out: %s PR #%d merged as %s into %s (merge commit is an ancestor of the remote tip) - task closed done", label, state.Number, shortSHA(mergeSHA), base)
}

// logCloseOutConflict records the evidence for a conflicting PR: the task, the PR,
// the base and EVERY conflicting path, computed without touching a worktree.
//
// One line per head/base PAIR, not one per poll. The pair is the judged head's oid
// plus the base tip read fresh from the remote (`ls-remote`, not a fetch), so a push
// that changes the head — or a base that moves under it — earns a new line, while an
// unchanged conflict is silent. That is what keeps this a greppable evidence trail
// for deciding whether to build the conflict-only session path, rather than a line
// repeated every 15 seconds.
func (d *Supervisor) logCloseOutConflict(ctx context.Context, verifier *delivery.Verifier, dir, label, key string, task plane.CloseOutTask, state delivery.PRState, base string) {
	baseTip, err := verifier.RemoteTip(ctx, dir, "origin", base)
	if err != nil {
		d.closeOutSay(key+":conflict-tip", "close-out: %s PR #%d is CONFLICTING with %s, but its base tip could not be read (%v)", label, state.Number, base, err)
		return
	}
	pair := state.HeadOID + "@" + baseTip
	if d.closeOutConflicts == nil {
		d.closeOutConflicts = map[string]string{}
	}
	if d.closeOutConflicts[task.ID] == pair {
		return // this exact head/base pair has already been reported
	}

	head := strings.TrimSpace(state.Head)
	if head == "" {
		head = task.Branch
	}
	if err := verifier.FetchRef(ctx, dir, "origin", base); err != nil {
		d.closeOutSay(key+":conflict-fetch", "close-out: %s PR #%d is CONFLICTING with %s, but the base could not be fetched to name the paths (%v)", label, state.Number, base, err)
		return
	}
	if err := verifier.FetchRef(ctx, dir, "origin", head); err != nil {
		d.closeOutSay(key+":conflict-fetch", "close-out: %s PR #%d is CONFLICTING with %s, but its head could not be fetched to name the paths (%v)", label, state.Number, base, err)
		return
	}
	paths, err := verifier.ConflictPaths(ctx, dir, "origin/"+base, "origin/"+head)
	if err != nil {
		d.closeOutSay(key+":conflict-paths", "close-out: %s PR #%d is CONFLICTING with %s (head %s@%s, base %s@%s), but its conflicting paths could not be computed (%v)", label, state.Number, base, head, shortSHA(state.HeadOID), base, shortSHA(baseTip), err)
		return
	}
	d.closeOutConflicts[task.ID] = pair
	named := strings.Join(paths, ", ")
	if named == "" {
		named = "(none reported - the fetched tips merged cleanly, so the PR's state is stale)"
	}
	d.closeOutSay(key, "close-out: %s PR #%d is CONFLICTING with %s (head %s@%s, base %s@%s) - conflicting paths: %s - leaving it to the agent close-out",
		label, state.Number, base, head, shortSHA(state.HeadOID), base, shortSHA(baseTip), named)
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
	if d.o.CloseOutFor != nil {
		return d.o.CloseOutFor(endpoint, d.o.APIKey)
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

// closeOutRepoDir resolves the fleet's checkout of one task's repo under the
// machine root: <root>/<project slug>/<repo name>, which is the layout the workdir
// derivation materializes. It VERIFIES the directory is that repo's checkout rather
// than trusting the path: merging a task's PR into the base of whatever happens to
// sit at that name would be a correctness failure invisible in the log.
func closeOutRepoDir(root, slug, repo string) (string, error) {
	pr, err := ProjectRoot(root, slug)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(pr, basenameOfRepo(repo))
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("no checkout at %s (%v)", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory the fleet owns", dir)
	}
	remote, err := checkoutOriginRemote(dir)
	if err != nil {
		return "", fmt.Errorf("%s is not a checkout (%v)", dir, err)
	}
	if !delivery.MatchesRepoSlug(remote, repo) {
		return "", fmt.Errorf("%s is a checkout of %s, not %s", dir, remote, repo)
	}
	return dir, nil
}

// closeOutBase resolves the branch a task PR must target: the operator's per-repo
// PR base when one is configured (the makesPDF plugins, whose PRs target
// `integration`), else the project's integration branch (default `staging`).
func closeOutBase(cfg *config.Config, slug, repo string) string {
	if base := strings.TrimSpace(cfg.PRBaseFor(slug, repo)); base != "" {
		return base
	}
	return strings.TrimSpace(cfg.IntegrationBranchFor(slug))
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

// prNumbers renders the open PRs one branch has, for the "refusing to guess" line.
func prNumbers(prs []delivery.PRRef) string {
	parts := make([]string, 0, len(prs))
	for _, pr := range prs {
		parts = append(parts, fmt.Sprintf("#%d", pr.Number))
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
