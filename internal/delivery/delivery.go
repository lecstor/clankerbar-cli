// Package delivery checks, against local git, the delivery claims a harness
// session makes to the control plane.
//
// The plane is a pipe: it holds the backlog and takes a clanker at its word. When
// a session records a branch or declares a commit merged, nothing verifies either
// claim — and one task (CLA-134) read `done` for four days while ~900 lines of its
// work sat unpushed on one laptop, its own PR merging a stale snapshot.
//
// The driver is the right place to close that: it is already local, already in the
// git tree, and already watches the session stream for `claim_task` and
// `update_task` (CLA-242). This adds a check to an observation that already
// happens. It needs no new credentials and no plane change.
//
// # Fail open, not closed
//
// Every check has three outcomes, not two: Pass, Fail, and Unknown. Unknown is
// load-bearing. If the check cannot run — no git on PATH, no remote, a workdir
// that is not a repository, a remote tip we do not have locally — the answer is
// "could not verify", never "verified". A driver that reported a false pass would
// be worse than the gap it replaces, and one that blocked a legitimate closure
// because it could not find the tree would be worse still. Not knowing is not the
// same as knowing it is fine, and neither is grounds for overriding the session.
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
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
)

// Kind names which claim a check was about.
type Kind string

const (
	// BranchPushed: a branch recorded via `update_task(branch: ...)` is really on
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
}

// Empty reports that there is nothing here to check.
func (c Claim) Empty() bool {
	return c.Branch == "" && (c.Commit == "" || c.IntegrationBranch == "")
}

// Check is one verified (or unverifiable) assertion.
type Check struct {
	Kind   Kind
	Status Status
	// Detail is written to be read in a log at 3am by someone who was asleep when
	// it happened: it names what is unpushed or unmerged, not just that something
	// is wrong.
	Detail string
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
}

// New builds a Verifier rooted at a session's workdir. remote is the git remote
// the claims are judged against; empty means "origin, or the only remote there is".
func New(workdir, remote string) *Verifier {
	if remote == "" {
		remote = "origin"
	}
	return &Verifier{workdir: workdir, gitBin: "git", remote: remote}
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
		return Check{Kind: BranchPushed, Status: Pass, Detail: fmt.Sprintf(
			"%s/%s is at %s, matching the local tip", remote, branch, short(local))}
	}

	if !v.haveObject(ctx, repo, remoteSHA) {
		// The remote is at a commit we have never seen. That is the remote being
		// AHEAD (or rewritten), never the local-ahead failure this check exists for:
		// were we ahead, the remote tip would be one of our own ancestors and
		// therefore present. Try one targeted fetch, then say we do not know.
		_, _ = v.run(ctx, repo, "fetch", "--quiet", "--", remote, branch)
	}
	if !v.haveObject(ctx, repo, remoteSHA) {
		return Check{Kind: BranchPushed, Status: Unknown, Detail: fmt.Sprintf(
			"%s/%s is at %s, which is not in %s and could not be fetched — the remote is ahead or rewritten, so ancestry cannot be settled here",
			remote, branch, short(remoteSHA), repo)}
	}

	if v.isAncestor(ctx, repo, local, remoteSHA) {
		return Check{Kind: BranchPushed, Status: Pass, Detail: fmt.Sprintf(
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
	tip, err := v.lsRemote(ctx, repo, remote, integration)
	if err != nil {
		return Check{Kind: CommitMerged, Status: Unknown, Detail: fmt.Sprintf(
			"could not read %s: %v", remote, err)}
	}
	if tip == "" {
		return Check{Kind: CommitMerged, Status: Unknown, Detail: fmt.Sprintf(
			"%s has no branch %q to check the delivery against", remote, integration)}
	}
	if !v.haveObject(ctx, repo, tip) {
		_, _ = v.run(ctx, repo, "fetch", "--quiet", "--", remote, integration)
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

// resolveRepo finds the repository whose refs carry branch. See the package doc:
// linked worktrees share a ref database, so any working tree of the right
// repository will do, and the search only has to identify the repository.
//
// candidateRepos collects one working tree per distinct repository at or below the
// workdir, deduplicated by the shared git common directory so a repo and its
// linked worktrees count once. Bounded by maxDepth and maxEntries.
func (v *Verifier) candidateRepos(ctx context.Context) []string {
	if v.workdir == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string

	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if ctx.Err() != nil {
			return
		}
		common, root, inRepo := v.repoAt(ctx, dir)
		// `git rev-parse` answers for the nearest ENCLOSING repository, which may be
		// far above dir. Below the workdir that must not count: a `git init`-ed home
		// directory would otherwise match ~/dev itself, and the walk would stop there
		// having found no project at all. At the workdir itself an enclosing repo IS
		// the answer (the single-project case, where the workdir is the checkout or a
		// directory inside it) — but if it merely encloses, keep descending too.
		atRoot := inRepo && sameDir(root, dir)
		if inRepo && (atRoot || depth == 0) {
			if !seen[common] {
				seen[common] = true
				out = append(out, dir)
			}
		}
		if atRoot {
			// Do not descend: everything inside shares these refs, and a nested
			// repository is not part of the worktree convention this serves.
			return
		}
		if depth >= maxDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		probed := 0
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if probed++; probed > maxEntries {
				return
			}
			walk(filepath.Join(dir, e.Name()), depth+1)
		}
	}
	walk(v.workdir, 0)
	return out
}

// repoAt reports the shared git directory and the working-tree root for dir, and
// whether dir is inside a non-bare repository at all. The common dir (not the
// per-worktree one) is what makes a worktree and its parent repository compare
// equal; the root is what tells "this IS a checkout" from "something above it is".
//
// A bare repository is rejected: sessions never work in one, it has no working tree
// a clanker could have committed in, and — as a mirror or a local remote sitting
// beside the checkouts — it would answer "yes, that branch exists" while having no
// remote of its own to check anything against. It reports not-a-repo rather than
// stopping the walk, so a bare repo's internals are still skipped by the depth
// bound rather than probed directory by directory.
func (v *Verifier) repoAt(ctx context.Context, dir string) (common, root string, ok bool) {
	out, err := v.run(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil || out == "" {
		return "", "", false
	}
	top, err := v.run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return "", "", false // bare, or otherwise not a working tree
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	return realPath(out), realPath(top), true
}

// realPath normalises a path for comparison: symlinks resolved where possible
// (macOS temp dirs are /var -> /private/var), lexically cleaned otherwise.
func realPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

func sameDir(a, b string) bool { return realPath(a) == realPath(b) }

// resolveRemote picks the remote to judge against: the configured one when the
// repository has it, otherwise its only remote. A repo with several remotes and no
// `origin` keeps the configured name and fails the read loudly, rather than
// silently checking against whichever remote happened to sort first.
func (v *Verifier) resolveRemote(ctx context.Context, repo string) string {
	out, err := v.run(ctx, repo, "remote")
	if err != nil || out == "" {
		return v.remote
	}
	names := strings.Fields(out)
	for _, n := range names {
		if n == v.remote {
			return v.remote
		}
	}
	if len(names) == 1 {
		return names[0]
	}
	return v.remote
}

// lsRemote reads the remote's tip for a branch WITHOUT mutating anything. Empty
// SHA with a nil error means the remote genuinely has no such branch — which is a
// Fail, not an Unknown.
func (v *Verifier) lsRemote(ctx context.Context, repo, remote, branch string) (string, error) {
	out, err := v.run(ctx, repo, "ls-remote", "--heads", "--", remote, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "refs/heads/"+branch {
			return fields[0], nil
		}
	}
	return "", nil
}

func (v *Verifier) haveObject(ctx context.Context, repo, sha string) bool {
	_, err := v.run(ctx, repo, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func (v *Verifier) isAncestor(ctx context.Context, repo, a, b string) bool {
	_, err := v.run(ctx, repo, "merge-base", "--is-ancestor", a, b)
	return err == nil
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

// run executes one git command in dir and returns its trimmed stdout.
//
// The environment is deliberately hostile to interaction: an unattended run has
// no terminal, and a git that decides to ask for a password would hang the driver
// rather than answering the question.
//
// The context alone does NOT bound this. `git ls-remote` and `git fetch` spawn
// helpers (ssh, git-remote-https, a credential helper) that inherit the pipes
// behind these buffers, and os/exec's Wait blocks until every inheritor closes its
// write end — so a killed git can still leave the call sitting there. Measured at
// 8s against a 500ms deadline, returning a nil error, which would have read as a
// successful check. WaitDelay is what actually cuts it off, and the truncated read
// is reported as the failure it is.
func (v *Verifier) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, v.gitBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=echo",
		"SSH_ASKPASS=echo",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=10",
		"GCM_INTERACTIVE=never",
	)
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", args[0], err, firstLine(msg))
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
