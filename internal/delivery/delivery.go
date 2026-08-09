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

	repo, err := v.resolveRepo(ctx, c.Branch)
	if err != nil {
		rep.Checks = v.allUnknown(c, err.Error())
		return rep
	}
	rep.Repo = repo
	remote := v.resolveRemote(ctx, repo)

	if c.Branch != "" {
		rep.Checks = append(rep.Checks, v.checkBranch(ctx, repo, remote, c.Branch))
	}
	if c.Commit != "" && c.IntegrationBranch != "" {
		rep.Checks = append(rep.Checks, v.checkMerged(ctx, repo, remote, c.Commit, c.IntegrationBranch))
	}
	return rep
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
		_, _ = v.run(ctx, repo, "fetch", "--quiet", remote, branch)
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
	ahead := v.count(ctx, repo, remoteSHA+".."+local)
	return Check{Kind: BranchPushed, Status: Fail, Detail: fmt.Sprintf(
		"branch %q is %s ahead of %s/%s — local tip %s, remote %s; that work is UNPUSHED and exists only in %s",
		branch, plural(ahead, "commit"), remote, branch, short(local), short(remoteSHA), repo)}
}

// checkMerged answers: did the declared delivery really land on the integration
// branch? This is the same ancestor check the plane asks the clanker to attest to
// — run here rather than trusted.
//
// It is judged against the REMOTE tip of the integration branch, not a local one:
// a local `main` is routinely tens of commits stale, and a check against a stale
// ref answers a question nobody asked.
func (v *Verifier) checkMerged(ctx context.Context, repo, remote, commit, integration string) Check {
	sha, err := v.run(ctx, repo, "rev-parse", "--verify", "--quiet", commit+"^{commit}")
	if err != nil || sha == "" {
		return Check{Kind: CommitMerged, Status: Unknown, Detail: fmt.Sprintf(
			"commit %s is not in %s, so it cannot be traced to %s", short(commit), repo, integration)}
	}

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
		_, _ = v.run(ctx, repo, "fetch", "--quiet", remote, integration)
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
func (v *Verifier) resolveRepo(ctx context.Context, branch string) (string, error) {
	if v.workdir == "" {
		return "", fmt.Errorf("no workdir configured, so no repository to check against")
	}
	repos := v.candidateRepos(ctx)
	if len(repos) == 0 {
		return "", fmt.Errorf("%s is not a git repository and none was found below it, so the claim cannot be checked locally", v.workdir)
	}
	if branch == "" {
		return repos[0], nil
	}
	for _, r := range repos {
		if sha, err := v.run(ctx, r, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil && sha != "" {
			return r, nil
		}
	}
	return "", fmt.Errorf("no repository at or below %s has a branch %q (looked in %s)",
		v.workdir, branch, strings.Join(repos, ", "))
}

// candidateRepos collects one working tree per distinct repository at or below the
// workdir, deduplicated by the shared git common directory so a repo and its
// linked worktrees count once. Bounded by maxDepth and maxEntries.
func (v *Verifier) candidateRepos(ctx context.Context) []string {
	seen := map[string]bool{}
	var out []string

	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if ctx.Err() != nil {
			return
		}
		if common, ok := v.commonGitDir(ctx, dir); ok {
			if !seen[common] {
				seen[common] = true
				out = append(out, dir)
			}
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

// commonGitDir returns the repository's shared git directory for dir, and whether
// dir is a WORKING TREE of a repository. The common dir (not the per-worktree one)
// is what makes a worktree and its parent repository compare equal.
//
// A bare repository is rejected: sessions never work in one, it has no branch a
// clanker could have committed to, and — as a mirror or a local remote sitting
// beside the checkouts — it would answer "yes, that branch exists" while having no
// remote of its own to check anything against.
func (v *Verifier) commonGitDir(ctx context.Context, dir string) (string, bool) {
	out, err := v.run(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil || out == "" {
		return "", false
	}
	if bare, err := v.run(ctx, dir, "rev-parse", "--is-bare-repository"); err != nil || bare == "true" {
		return "", false
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	if resolved, err := filepath.EvalSymlinks(out); err == nil {
		out = resolved
	}
	return filepath.Clean(out), true
}

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

func (v *Verifier) count(ctx context.Context, repo, rng string) int {
	out, err := v.run(ctx, repo, "rev-list", "--count", rng)
	if err != nil {
		return 0
	}
	n := 0
	if _, err := fmt.Sscanf(out, "%d", &n); err != nil {
		return 0
	}
	return n
}

// run executes one git command in dir and returns its trimmed stdout.
//
// The environment is deliberately hostile to interaction: an unattended run has
// no terminal, and a git that decides to ask for a password would hang the driver
// until its context expires rather than answering the question.
func (v *Verifier) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, v.gitBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=echo",
		"SSH_ASKPASS=echo",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes",
		"GCM_INTERACTIVE=never",
	)
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
