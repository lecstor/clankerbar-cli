// Package delivery checks, against local git, the delivery claims a harness
// session makes to the control plane.
//
// The plane is a pipe: it holds the backlog and takes a clanker at its word. When
// a session records a branch or declares a commit merged, nothing verifies either
// claim — and one task (CLA-134) read `done` for four days while ~900 lines of its
// work sat unpushed on one laptop, its own PR merging a stale snapshot.
//
// The driver is the right place to close that: it is already local, already in
// the git tree, and already watches the session stream for `claim_task` and
// the plane's task writes (CLA-242). This adds a check to an observation that already
// happens. It needs no new configured credentials - GitHub access rides the
// operator's own `gh` auth, resolved per remote below - and no plane change.
//
// # Fail open, not closed
//
// Every check has four outcomes, not two: Pass, Fail, Unknown, and Warn.
// Unknown is load-bearing. If the check cannot run — no git on PATH, no
// remote, a workdir that is not a repository, a remote tip we do not have
// locally — the answer is "could not verify", never "verified". A driver that
// reported a false pass would be worse than the gap it replaces, and one that
// blocked a legitimate closure because it could not find the tree would be
// worse still. Not knowing is not the same as knowing it is fine, and neither
// is grounds for overriding the session. Warn is CLA-310's: a check that ran
// and found only the loose side of an operator's explicit opt-out — never
// emitted by default.
//
// # Which working tree
//
// The driver spawns sessions in a workdir, but the work happens in a per-task
// worktree the driver did not create and does not track — and the workdir is
// routinely a multi-repo parent (`~/dev`) that is not a repository at all.
//
// The resolution is narrower than it first looks: linked worktrees SHARE the
// repository's ref database, so the specific directory a session worked in is
// irrelevant. Every check here reads refs (`refs/heads/<branch>`, `ls-remote`,
// `merge-base`), so any working tree of the right repository answers identically.
// What must be found is the REPOSITORY whose refs carry the branch, which is a
// bounded search of the workdir and its first two directory levels — enough to
// reach both `~/dev/<repo>` and `~/dev/<repo>-wt/<task>`. A repository is never
// descended into, so a tree of worktrees costs one probe, not one per worktree.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Status is the outcome of one check. The three-way split is the whole point: see
// the package doc on failing open.
type Status string

const (
	// Pass: the check ran and the claim holds.
	Pass Status = "pass"
	// Fail: the check ran and the claim does not hold. This is the CLA-134 case.
	Fail Status = "fail"
	// Unknown: the check could not run. NOT a pass.
	Unknown Status = "unknown"
	// Warn: the check ran and found the loose side of an explicit operator
	// opt-out (CLA-310's allow_unchecked_pr). It is neither a pass — the detail
	// says what to confirm — nor a failure, because the operator chose it.
	Warn Status = "warn"
)

// Kind names which claim a check was about.
type Kind string

const (
	// BranchPushed: a branch recorded via `record_progress(branch: ...)` is really on
	// the remote, and the local tip is an ancestor of the remote tip.
	BranchPushed Kind = "branch"
	// CommitMerged: a declared `delivery.commit` is an ancestor of the declared
	// `delivery.integrationBranch`.
	CommitMerged Kind = "merge"
)

// Claim is what a session told the plane it delivered.
type Claim struct {
	// Label identifies the task in logs ("CLA-253"); cosmetic.
	Label string

	// Branch is the work-in-progress branch recorded as the handover record.
	Branch string

	// Commit and IntegrationBranch are the declared delivery: the commit that
	// carried the work, and the branch it is claimed to have landed on.
	Commit            string
	IntegrationBranch string

	// PR is the pull request the session named as carrying the delivery
	// (CLA-310). Verified through the `gh` CLI — mergeable (or already merged,
	// when GitHub no longer computes mergeability), with a check rollup that
	// actually ran and passed; see prcheck.go for the two conditions, the
	// bounded wait, and the no-CI decision.
	PR string

	// Repo is the repository the claim lives in ("owner/name"). When set, it resolves ambiguity when the same branch exists in multiple repos (CLA-351).
	Repo string
}

// Empty reports that there is nothing here to check.
func (c Claim) Empty() bool {
	return c.Branch == "" && (c.Commit == "" || c.IntegrationBranch == "") && c.PR == ""
}

// Check is one verified (or unverifiable) assertion.
type Check struct {
	Kind   Kind
	Status Status
	// Detail is written to be read in a log at 3am by someone who was asleep when
	// it happened: it names what is unpushed or unmerged, not just that something
	// is wrong.
	Detail string
	// Tip is the REMOTE commit a passing BranchPushed check read (CLA-637) —
	// the origin tip the hand-off names, carried so the driver's checkpoint can
	// hand the review phase the exact revision to review and its seam can
	// require the durable review record to name that same revision. Full SHA,
	// because the reviewer compares it with `git rev-parse HEAD`. Empty on
	// anything but a Pass: there is no verified remote tip to name.
	Tip string
}

// Report is everything the driver learned about one session's delivery claim.
type Report struct {
	Claim Claim
	// Repo is the working tree the checks ran in. Empty when none was resolved.
	Repo   string
	Checks []Check
}

// Failed reports whether any check ran and came back false. Unknown is not a
// failure — it is an absence of knowledge.
func (r Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// MergeVerified reports what the merge check concluded: (verified, ran). When ran
// is false the driver must not attest anything — an honest `mergeVerified` is only
// written for a check that actually happened.
func (r Report) MergeVerified() (verified, ran bool) {
	for _, c := range r.Checks {
		if c.Kind != CommitMerged || c.Status == Unknown {
			continue
		}
		return c.Status == Pass, true
	}
	return false, false
}

// Repos lists one working tree per distinct git repository at or below workdir -
// the search described in this package's doc, exposed for the driver's salvage
// path (CLA-314).
//
// It is shared rather than reimplemented because salvage has to find the SAME
// repository these checks do. Two implementations of "which tree did the session
// work in" would agree until the day they did not, and on that day one of them
// would be committing.
func Repos(ctx context.Context, workdir string) []string {
	return New(workdir, "").candidateRepos(ctx)
}

// Remote picks the remote to act on in repo: the preferred one when the
// repository has it, otherwise its only remote. Shared with salvage for the same
// reason as Repos - a rescue has to push to the remote these checks will then
// judge it against. The workdir plays no part in this one, hence the empty one.
func Remote(ctx context.Context, repo, preferred string) string {
	return New("", preferred).resolveRemote(ctx, repo)
}

// ProbeRemote reports the remote the delivery checks will judge a claim against
// in repo, or why none can be read: no remote at all, or several and none of them
// preferred. It is the rule Verifier.resolveRemote applies, with the unreadable
// cases made explicit so a caller can report the reason instead of silently
// defaulting to a remote that is not there.
//
// run is the caller's own git exec: doctor bounds every command it runs, and the
// verifier's is the one its checks use. Shared for the same reason as Repos - a
// preflight that named a different remote than the check would tell the operator
// the wrong thing about the check it exists to vouch for.
func ProbeRemote(ctx context.Context, run func(ctx context.Context, dir string, args ...string) (string, error), repo, preferred string) (string, error) {
	out, err := run(ctx, repo, "remote")
	if err != nil {
		return "", err
	}
	names := strings.Fields(out)
	for _, n := range names {
		if n == preferred {
			return n, nil
		}
	}
	switch len(names) {
	case 0:
		return "", errors.New("no remote configured")
	case 1:
		return names[0], nil
	default:
		return "", fmt.Errorf("%d remotes and none is %s", len(names), preferred)
	}
}

// maxDepth is how far below the workdir a repository is looked for. Two levels
// reaches `~/dev/<repo>` and `~/dev/<repo>-wt/<task-id>`, which is the layout the
// per-task worktree convention produces. Deeper is a fishing expedition.
const maxDepth = 2

// maxEntries caps how many directories are probed at any one level, so pointing
// the driver at a home directory costs a bounded number of `git rev-parse` calls
// rather than an unbounded walk.
const maxEntries = 128

// Verifier checks delivery claims against the git repositories under a workdir.
// The zero value is not usable; build one with New.
type Verifier struct {
	workdir string
	gitBin  string
	remote  string

	// allowUncheckedPR is CLA-310's operator opt-out: an empty check rollup on
	// a named PR is downgraded from a refusal to a Warn. The MERGEABLE
	// condition is never relaxed by it.
	allowUncheckedPR bool

	// ghBin and the polling window drive the PR check (prcheck.go), and ghBin
	// also resolves the per-owner token that scopes remote reads (CLA-458).
	// Fields so tests can substitute a fake gh and shrink the wait; New fills
	// the production values.
	ghBin      string
	prBudget   time.Duration
	prInterval time.Duration

	// prCreateBackoff is the wait before the ONE retry of `gh pr create`
	// (prcreate.go). A field so a test can shrink it; New fills the production
	// value.
	prCreateBackoff time.Duration
}

// prCreateRetryBackoff is the production wait between the first create attempt
// and the one retry: long enough for a transient GitHub/API blip, short enough
// that a hand-over is not held up by it.
const prCreateRetryBackoff = 5 * time.Second

// New builds a Verifier rooted at a session's workdir. remote is the git remote
// the claims are judged against; empty means "origin, or the only remote there is".
func New(workdir, remote string) *Verifier {
	if remote == "" {
		remote = "origin"
	}
	return &Verifier{
		workdir:         workdir,
		gitBin:          "git",
		remote:          remote,
		ghBin:           "gh",
		prBudget:        prPollBudget,
		prInterval:      prPollInterval,
		prCreateBackoff: prCreateRetryBackoff,
	}
}

// AllowUncheckedPR applies the no-CI opt-out (CLA-310): an empty check rollup
// on a delivery's PR becomes a WARN instead of a refusal. It never relaxes
// the mergeability condition, and it changes nothing for repos whose PRs do
// carry checks. Returns the verifier, for chaining off New.
//
// It deliberately does NOT reach the close-out's judgement (JudgePR): a
// deterministic merge has nobody reading it afterwards, so an empty rollup stays
// a refusal there whatever the project's delivery opt-out says.
func (v *Verifier) AllowUncheckedPR() *Verifier {
	v.allowUncheckedPR = true
	return v
}

// WithGHBin points the verifier at an alternative gh binary, for the callers that
// are not the delivery gate: a supervisor's close-out shares this judgement, and its
// tests substitute a fake gh exactly as this package's own do.
func (v *Verifier) WithGHBin(bin string) *Verifier {
	if strings.TrimSpace(bin) != "" {
		v.ghBin = bin
	}
	return v
}

// WithPollWindow shrinks (or widens) the window a PR is polled within, for callers
// whose tests cannot wait out the production 30 seconds. A non-positive value leaves
// the field alone, so a caller can set only the one it cares about.
func (v *Verifier) WithPollWindow(budget, interval time.Duration) *Verifier {
	if budget > 0 {
		v.prBudget = budget
	}
	if interval > 0 {
		v.prInterval = interval
	}
	return v
}

// PollWindow is the budget a PR read is polled within. A caller that has to wait
// for something GitHub has just changed to become visible — the close-out waiting
// for a merge commit it made to appear on the base — uses it as its own retry
// window, so a test that shrinks the judgement's window shrinks that wait with it.
func (v *Verifier) PollWindow() time.Duration { return v.prBudget }

// WithPRCreateBackoff shrinks (or widens) the wait before EnsurePR's one retry,
// for callers whose tests cannot wait out the production five seconds.
func (v *Verifier) WithPRCreateBackoff(d time.Duration) *Verifier {
	if d >= 0 {
		v.prCreateBackoff = d
	}
	return v
}

// Verify checks a claim and returns what it found. It never returns an error:
// everything that could go wrong is a check whose Status is Unknown, because the
// caller's only correct response to "could not check" is to say so and carry on.
func (v *Verifier) Verify(ctx context.Context, c Claim) Report {
	rep := Report{Claim: c}
	if c.Empty() {
		return rep
	}

	if _, err := exec.LookPath(v.gitBin); err != nil {
		rep.Checks = v.allUnknown(c, "git is not on PATH, so nothing can be checked locally")
		return rep
	}

	// One search, but the two claims are resolved INDEPENDENTLY. They are located
	// by different evidence — a branch name, a commit object — and letting one
	// unresolvable claim mark the other unknown loses real checks: a clanker that
	// tidies up (`git branch -d`) after a merge would suppress the merge check,
	// which is the one the bar cares about most, over a missing ref that is missing
	// precisely because the work landed.
	repos := v.candidateRepos(ctx)
	if len(repos) == 0 {
		rep.Checks = v.allUnknown(c, fmt.Sprintf(
			"%s is not a git repository and none was found below it, so the claim cannot be checked locally", v.workdir))
		return rep
	}

	if c.Branch != "" {
		rep.Checks = append(rep.Checks, v.branchCheck(ctx, repos, c.Branch, &rep))
	}
	if c.Commit != "" && c.IntegrationBranch != "" {
		rep.Checks = append(rep.Checks, v.mergeCheck(ctx, repos, c.Commit, c.IntegrationBranch, &rep))
	}
	if c.PR != "" {
		// Runs after the git checks so it can prefer the repository they
		// resolved (rep.Repo); a PR number belongs to a repository, and the
		// branch/commit checks are what identify it.
		rep.Checks = append(rep.Checks, v.prCheck(ctx, repos, c.PR, &rep))
	}
	return rep
}

// branchCheck locates the repository carrying the branch, then checks it.
//
// A branch NAME is not an identity: two separate clones of the same project under
// one workdir (the session's tree, and a `gh pr checkout` review clone beside it)
// both carry `clanker/x`, and picking whichever the directory listing happened to
// return first would answer about the wrong tree — turning the exact
// unpushed-work failure this exists to catch into a Pass. Ambiguity is reported,
// not guessed at.
func (v *Verifier) branchCheck(ctx context.Context, repos []string, branch string, rep *Report) Check {
	// If the claim names a repo, filter the candidates to those whose remote URL
	// matches the slug (CLA-351). If none match, fall back honestly: the refusal
	// message names the ambiguous repos as before, rather than hiding the mismatch.
	if rep.Claim.Repo != "" {
		filtered := v.resolveRepoByURL(ctx, repos, rep.Claim.Repo)
		if len(filtered) > 0 {
			repos = filtered
		}
	}
	var found []string
	for _, r := range repos {
		if sha, err := v.run(ctx, r, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil && sha != "" {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 0:
		return Check{Kind: BranchPushed, Status: Unknown, Detail: fmt.Sprintf(
			"no repository at or below %s has a branch %q (looked in %s)",
			v.workdir, branch, strings.Join(repos, ", "))}
	case 1:
	default:
		return Check{Kind: BranchPushed, Status: Unknown, Detail: fmt.Sprintf(
			"%d repositories below %s carry a branch %q (%s) — which one the session worked in cannot be told apart, and checking the wrong one is worse than not checking",
			len(found), v.workdir, branch, strings.Join(found, ", "))}
	}
	rep.Repo = found[0]
	return v.checkBranch(ctx, found[0], v.resolveRemote(ctx, found[0]), branch)
}

// mergeCheck locates the repository containing the declared commit, then checks it.
//
// Unlike a branch name, a commit id IS an identity, so several candidates holding
// it are necessarily clones of the same history and answer alike; the first will
// do. What is NOT safe is taking the declared commit as a revision expression:
// `rev-parse` would happily resolve "main", "HEAD" or the integration branch
// itself, each of which is trivially its own ancestor, letting the party under
// check manufacture a green attestation. It must be a commit id, and the object it
// names must be the one it spells.
func (v *Verifier) mergeCheck(ctx context.Context, repos []string, commit, integration string, rep *Report) Check {
	if !isCommitID(commit) {
		return Check{Kind: CommitMerged, Status: Unknown, Detail: fmt.Sprintf(
			"declared delivery commit %q is not a commit id — a revision expression (a branch, HEAD) is not a delivery and is not checked as one", commit)}
	}
	for _, r := range repos {
		sha, err := v.run(ctx, r, "rev-parse", "--verify", "--quiet", commit+"^{commit}")
		if err != nil || sha == "" || !strings.HasPrefix(sha, strings.ToLower(commit)) {
			continue
		}
		if rep.Repo == "" {
			rep.Repo = r
		}
		return v.checkMerged(ctx, r, v.resolveRemote(ctx, r), sha, integration)
	}
	return Check{Kind: CommitMerged, Status: Unknown, Detail: fmt.Sprintf(
		"commit %s is in no repository at or below %s, so it cannot be traced to %s (looked in %s)",
		short(commit), v.workdir, integration, strings.Join(repos, ", "))}
}

// isCommitID reports whether s spells an abbreviated or full object id. Seven is
// git's own minimum useful abbreviation; anything shorter is ambiguous by design.
func isCommitID(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// allUnknown marks every check the claim asked for as unrunnable for one shared
// reason, so a missing git or an unresolvable repo reads the same as any other
// "could not check" rather than silently dropping the claim.
func (v *Verifier) allUnknown(c Claim, reason string) []Check {
	var out []Check
	if c.Branch != "" {
		out = append(out, Check{Kind: BranchPushed, Status: Unknown, Detail: reason})
	}
	if c.Commit != "" && c.IntegrationBranch != "" {
		out = append(out, Check{Kind: CommitMerged, Status: Unknown, Detail: reason})
	}
	if c.PR != "" {
		out = append(out, Check{Kind: PRVerified, Status: Unknown, Detail: reason})
	}
	return out
}

// checkBranch answers: is this branch really on the remote, and is everything
// local already there?
//
// A local branch AHEAD of its remote is the exact CLA-134 failure — the handover
// record points at commits that exist on one laptop.
func (v *Verifier) checkBranch(ctx context.Context, repo, remote, branch string) Check {
	local, err := v.run(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil || local == "" {
		return Check{Kind: BranchPushed, Status: Unknown, Detail: fmt.Sprintf(
			"%s has no local branch %q, so there is nothing local to compare against the remote", repo, branch)}
	}

	remoteSHA, err := v.lsRemote(ctx, repo, remote, branch)
	if err != nil {
		return Check{Kind: BranchPushed, Status: Unknown, Detail: fmt.Sprintf(
			"could not read %s: %v", remote, err)}
	}
	if remoteSHA == "" {
		return Check{Kind: BranchPushed, Status: Fail, Detail: fmt.Sprintf(
			"branch %q is NOT on %s — its local tip %s exists only in %s, so the recorded hand-off points at nothing another machine can fetch",
			branch, remote, short(local), repo)}
	}
	if remoteSHA == local {
		return Check{Kind: BranchPushed, Status: Pass, Tip: remoteSHA, Detail: fmt.Sprintf(
			"%s/%s is at %s, matching the local tip", remote, branch, short(local))}
	}

	if !v.haveObject(ctx, repo, remoteSHA) {
		// The remote is at a commit we have never seen. That is the remote being
		// AHEAD (or rewritten), never the local-ahead failure this check exists for:
		// were we ahead, the remote tip would be one of our own ancestors and
		// therefore present. Try one targeted fetch, then say we do not know.
		_, _ = v.runScoped(ctx, repo, "fetch", "--quiet", "--", remote, branch)
	}
	if !v.haveObject(ctx, repo, remoteSHA) {
		return Check{Kind: BranchPushed, Status: Unknown, Detail: fmt.Sprintf(
			"%s/%s is at %s, which is not in %s and could not be fetched — the remote is ahead or rewritten, so ancestry cannot be settled here",
			remote, branch, short(remoteSHA), repo)}
	}

	if v.isAncestor(ctx, repo, local, remoteSHA) {
		return Check{Kind: BranchPushed, Status: Pass, Tip: remoteSHA, Detail: fmt.Sprintf(
			"local tip %s is an ancestor of %s/%s (%s)", short(local), remote, branch, short(remoteSHA))}
	}
	return Check{Kind: BranchPushed, Status: Fail, Detail: fmt.Sprintf(
		"branch %q is ahead of %s/%s%s — local tip %s, remote %s; that work is UNPUSHED and exists only in %s",
		branch, remote, branch, v.aheadBy(ctx, repo, remoteSHA, local), short(local), short(remoteSHA), repo)}
}

// checkMerged answers: did the declared delivery really land on the integration
// branch? This is the same ancestor check the plane asks the clanker to attest to
// — run here rather than trusted.
//
// It is judged against the REMOTE tip of the integration branch, not a local one:
// a local `main` is routinely tens of commits stale, and a check against a stale
// ref answers a question nobody asked.
// sha is already resolved and confirmed present in repo by mergeCheck.
func (v *Verifier) checkMerged(ctx context.Context, repo, remote, sha, integration string) Check {
	return v.mergedOnRemote(ctx, repo, remote, sha, integration)
}

// mergedOnRemote is the ancestor test both the delivery gate (checkMerged) and the
// supervisor's close-out (MergedOnRemote) run, so the two can never disagree about
// what "merged" means.
//
// The commit is fetched FOR when it is not local. That case is unreachable from the
// delivery gate — mergeCheck only calls this once it has resolved sha in the tree —
// and is the normal one for a close-out, where GitHub made the merge commit and the
// driver has never seen it. One fetch of the integration branch brings both: the
// merge commit is on it by definition, and the tip it was merged to is what the
// ancestry is judged against.
func (v *Verifier) mergedOnRemote(ctx context.Context, repo, remote, sha, integration string) Check {
	tip, err := v.lsRemote(ctx, repo, remote, integration)
	if err != nil {
		return Check{Kind: CommitMerged, Status: Unknown, Detail: fmt.Sprintf(
			"could not read %s: %v", remote, err)}
	}
	if tip == "" {
		return Check{Kind: CommitMerged, Status: Unknown, Detail: fmt.Sprintf(
			"%s has no branch %q to check the delivery against", remote, integration)}
	}
	if !v.haveObject(ctx, repo, tip) || !v.haveObject(ctx, repo, sha) {
		_, _ = v.runScoped(ctx, repo, "fetch", "--quiet", "--no-tags", "--", remote, "+refs/heads/"+integration+":refs/remotes/"+remote+"/"+integration)
	}
	if !v.haveObject(ctx, repo, sha) {
		return Check{Kind: CommitMerged, Status: Unknown, Detail: fmt.Sprintf(
			"commit %s is not in %s and could not be fetched from %s/%s — ancestry cannot be settled here",
			short(sha), repo, remote, integration)}
	}
	if !v.haveObject(ctx, repo, tip) {
		return Check{Kind: CommitMerged, Status: Unknown, Detail: fmt.Sprintf(
			"%s/%s is at %s, which could not be fetched into %s — ancestry cannot be settled here",
			remote, integration, short(tip), repo)}
	}

	if v.isAncestor(ctx, repo, sha, tip) {
		return Check{Kind: CommitMerged, Status: Pass, Detail: fmt.Sprintf(
			"%s is an ancestor of %s/%s (%s)", short(sha), remote, integration, short(tip))}
	}
	return Check{Kind: CommitMerged, Status: Fail, Detail: fmt.Sprintf(
		"%s is NOT an ancestor of %s/%s (%s) — the delivery was declared merged and is not",
		short(sha), remote, integration, short(tip))}
}

// aheadBy renders " by N commits" when the count can be read, and nothing at all
// when it cannot. A count that silently defaults to zero would print the loudest
// line in the feature as "is 0 commits ahead", which reads like a bug in the
// checker rather than the finding it is.
func (v *Verifier) aheadBy(ctx context.Context, repo, from, to string) string {
	out, err := v.run(ctx, repo, "rev-list", "--count", from+".."+to)
	if err != nil {
		return ""
	}
	n := 0
	if _, err := fmt.Sscanf(out, "%d", &n); err != nil || n <= 0 {
		return ""
	}
	return " by " + plural(n, "commit")
}
