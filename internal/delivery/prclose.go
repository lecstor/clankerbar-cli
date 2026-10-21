package delivery

// prclose.go — the pull-request surface a supervisor's deterministic CLOSE-OUT
// needs (CLA-758).
//
// An approved-awaiting-merge task is normally integrated by an agent session:
// claim the task, merge the PR, verify the merge landed, declare the delivery.
// Every step except resolving a conflict is mechanical, so the supervisor takes
// it itself and leaves a conflicting PR to the agent close-out. That needs the
// same JUDGEMENT the delivery gate makes (prcheck.go) plus the two acts the
// session would have taken: merge the PR, and read the paths a conflicting merge
// would hit.
//
// The judgement is REUSED rather than reimplemented: JudgePR drives prCheck's own
// polling loop and settles with settlePR, so this surface can never call a PR
// greener than the gate that judges an agent's delivery. The one deliberate
// difference is that `allow_unchecked_pr` cannot make an empty check rollup
// mergeable here: it only words the refusal as a warning, and PRReady still
// requires the gate's `prPass`, because a deterministic merge has nobody reading
// the log afterwards.
//
// Nothing here touches a worktree. The merge happens through the GitHub API, and
// the conflicting paths come from `git merge-tree` against refs this file fetches
// into the remote-tracking namespace — the same "no worktree" discipline the
// conflict log in the served protocol relies on.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// PRVerdict is the one word the close-out acts on.
type PRVerdict string

const (
	// PRReady: MERGEABLE, and a check rollup that ran and passed. The only
	// verdict a merge may follow.
	PRReady PRVerdict = "ready"

	// PRConflict: the PR conflicts with its base, so a merge would either fail
	// or (worse) be impossible to compute in the first place. The close-out
	// leaves it to the agent path, which can resolve a conflict.
	PRConflict PRVerdict = "conflict"

	// PRRefused: anything else the delivery gate refuses — failing, pending or
	// absent checks, an unresolvable merge state, a PR that could not be read.
	// Detail says which; the close-out must not merge on this verdict.
	PRRefused PRVerdict = "refused"
)

// PRRef is one open pull request as `gh pr list` serves it: the number the merge
// names, the base it targets, and the head revision a merge must be pinned to.
type PRRef struct {
	Number  int
	Base    string
	Head    string
	HeadOID string
}

// PRState is one pull request judged: what it is, what the delivery gate made of
// it, and the check that carries the reason.
type PRState struct {
	PRRef
	Verdict PRVerdict
	// Check is the settled delivery check. Pass only on PRReady; its Detail is
	// written to be read in a log, so the close-out's stand-down line is the
	// gate's own explanation rather than a second paraphrase of it.
	Check Check
}

// OpenPRs lists the OPEN pull requests in repo/slug whose head is head. The repo
// argument is a working directory gh is run in (it is also where the per-owner
// token is resolved); the slug names the GitHub repository the PR belongs to.
//
// An empty result is not an error: it means the branch's work is not waiting in
// a PR, which the caller reports and does nothing about.
func (v *Verifier) OpenPRs(ctx context.Context, repo, slug, head string) ([]PRRef, error) {
	if strings.TrimSpace(slug) == "" {
		return nil, errors.New("open PRs: no repository slug")
	}
	if strings.TrimSpace(head) == "" {
		return nil, errors.New("open PRs: no head branch")
	}
	if _, err := exec.LookPath(v.ghBin); err != nil {
		return nil, fmt.Errorf("open PRs: %s is not on PATH", v.ghBin)
	}
	out, err := v.runGHScoped(ctx, repo, v.ghAPIScope(ctx, repo, slug),
		"pr", "list", "--repo", slug, "--head", head, "--state", "open",
		"--json", "number,url,baseRefName,headRefName,headRefOid")
	if err != nil {
		return nil, err
	}
	var listed []struct {
		Number      int    `json:"number"`
		BaseRefName string `json:"baseRefName"`
		HeadRefName string `json:"headRefName"`
		HeadRefOid  string `json:"headRefOid"`
	}
	if jerr := json.Unmarshal([]byte(out), &listed); jerr != nil {
		// The same classification every gh JSON reader in this package uses
		// (CLA-776 review 1.review-1.6): gh ran and answered, and the answer is
		// not the JSON asked for. A generic error here would read as an
		// unavailable prerequisite while the gate calls the same fault a
		// refusal.
		return nil, &ghParseError{
			detail: fmt.Sprintf("could not parse gh pr list output: %v", jerr),
			err:    jerr,
		}
	}
	refs := make([]PRRef, 0, len(listed))
	for _, pr := range listed {
		refs = append(refs, PRRef{
			Number:  pr.Number,
			Base:    pr.BaseRefName,
			Head:    pr.HeadRefName,
			HeadOID: pr.HeadRefOid,
		})
	}
	return refs, nil
}

// JudgePR reads one pull request the way the delivery gate would — polling within
// the verifier's budget while GitHub computes mergeability or a check is still
// running — and classifies it for the close-out. It never returns an error: an
// unreadable PR is PRRefused with the reason in Check.Detail, which is the honest
// answer for a caller whose only correct response is "do not merge".
//
// Only PRReady is mergeable, and only the gate's `prPass` produces it — so the
// project's `allow_unchecked_pr` opt-out cannot make an empty rollup mergeable
// here. It reaches settlePR, and all it can do there is word the refusal as a
// warning; the verdict stays PRRefused either way. That is deliberate: a
// deterministic merge has nobody reading the log afterwards.
func (v *Verifier) JudgePR(ctx context.Context, repo, slug string, num int) PRState {
	state := PRState{PRRef: PRRef{Number: num}}
	if num <= 0 {
		state.Verdict = PRRefused
		state.Check = Check{Kind: PRVerified, Status: Unknown, Detail: "no pull request number to judge"}
		return state
	}
	if _, err := exec.LookPath(v.ghBin); err != nil {
		state.Verdict = PRRefused
		state.Check = Check{Kind: PRVerified, Status: Unknown, Detail: fmt.Sprintf(
			"%s is not on PATH, so PR %d cannot be judged", v.ghBin, num)}
		return state
	}
	scope := v.ghAPIScope(ctx, repo, slug)
	view, chk := v.pollPR(ctx, repo, slug, num, scope)
	j := judgePR(view)
	state.PRRef = PRRef{
		Number:  num,
		Base:    view.BaseRefName,
		Head:    view.HeadRefName,
		HeadOID: view.HeadRefOid,
	}
	// A read that named no base cannot be shown to target anything, and this
	// surface exists to merge into ONE configured branch. Whatever the check
	// said, an unnamed merge target is a refusal — never a pass that a caller
	// could act on.
	if chk.Status == Pass && strings.TrimSpace(view.BaseRefName) == "" {
		chk = Check{Kind: PRVerified, Status: Unknown, Detail: fmt.Sprintf(
			"PR %d was read without a base branch, so its merge target cannot be proven", num)}
	}
	state.Verdict = PRRefused
	switch {
	case chk.Status == Pass:
		state.Verdict = PRReady
	case j.verdict == prConflict:
		state.Verdict = PRConflict
	}
	state.Check = chk
	return state
}

// MergePR merges one pull request as a MERGE COMMIT — never a squash, never a
// rebase: the batch-integration flow depends on the history the merge commit
// keeps — and returns that merge commit's SHA.
//
// It names the BASE explicitly (GitHub's "merge a branch" endpoint) instead of
// merging through the pull request. A PR's base is a mutable property of the PR:
// whoever can retarget it can point it at `main` between the judgement and the
// merge, and the pull-request merge endpoint has no parameter that pins it — that
// endpoint merges into whatever base the PR carries when GitHub processes it. A
// request that names the branch is the only mechanism where the target is an
// invariant of the merge itself: a PR retargeted after the judgement is still
// merged into the configured integration branch, or not merged at all.
//
// expectedHeadOID pins the merge to the revision that was JUDGED: it is the
// request's `head`, so a push that lands between the judgement and this call is
// not what gets merged. It is REQUIRED — a caller that cannot name the revision it
// judged has no pin to offer, and dropping the pin silently is the hole this
// parameter exists to close.
func (v *Verifier) MergePR(ctx context.Context, repo, slug string, num int, base, expectedHeadOID string) (string, error) {
	if strings.TrimSpace(slug) == "" {
		return "", errors.New("merge PR: no repository slug")
	}
	if num <= 0 {
		return "", errors.New("merge PR: no pull request number")
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return "", errors.New("merge PR: no base branch to merge into")
	}
	head := strings.TrimSpace(expectedHeadOID)
	if head == "" {
		return "", fmt.Errorf("merge PR: no judged head revision to pin PR %d to, so the merge would not be pinned to what was judged", num)
	}
	if _, err := exec.LookPath(v.ghBin); err != nil {
		return "", fmt.Errorf("merge PR: %s is not on PATH", v.ghBin)
	}
	args := []string{
		"api", "--method", "POST",
		fmt.Sprintf("repos/%s/merges", slug),
		"-f", "base=" + base,
		"-f", "head=" + head,
		"-f", fmt.Sprintf("commit_message=Merge pull request #%d", num),
	}
	out, err := v.runGHScoped(ctx, repo, v.ghAPIScope(ctx, repo, slug), args...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		// 204: the base already contains the head, so there is no new merge
		// commit to name. That is not a delivery this call can record.
		return "", fmt.Errorf("PR %d's head %s is already merged into %s, so the merge produced no new commit", num, short(head), base)
	}
	var res struct {
		Merged  *bool  `json:"merged"`
		SHA     string `json:"sha"`
		Message string `json:"message"`
	}
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		return "", fmt.Errorf("could not parse the merge response: %v", jerr)
	}
	if res.Merged != nil && !*res.Merged {
		return "", fmt.Errorf("GitHub did not merge PR %d into %s: %s", num, base, strings.TrimSpace(res.Message))
	}
	if strings.TrimSpace(res.SHA) == "" {
		return "", fmt.Errorf("GitHub did not report a merge commit for PR %d into %s: %s", num, base, strings.TrimSpace(res.Message))
	}
	return res.SHA, nil
}

// IsolatedClone makes a THROWAWAY clone of repo for the close-out's remote reads,
// and returns it with the func that removes it.
//
// The close-out's checkout is the fleet's derived clone, and every session's
// worktree is linked into the same object database and ref store. A fetch there —
// the ancestry check's, or the conflict evidence's — takes ref locks a session is
// also taking, and a forced refspec rewrites a remote-tracking ref the session may
// be reading. Both are faults in somebody else's working tree, so the close-out
// reads in a clone of its own: `--shared` borrows repo's objects read-only (the
// history is local and cheap) and every ref written is private to this clone.
// Nothing done in it can be seen by a session.
//
// origin is pointed back at the REAL remote URL: a clone of the fleet's clone would
// otherwise answer `ls-remote origin` from that clone's own (possibly stale) refs
// instead of the remote's.
func (v *Verifier) IsolatedClone(ctx context.Context, repo, remote string) (string, func(), error) {
	url, err := v.run(ctx, repo, "remote", "get-url", remote)
	if err != nil {
		return "", nil, fmt.Errorf("isolated clone: read %s's URL: %v", remote, err)
	}
	if strings.TrimSpace(url) == "" {
		return "", nil, fmt.Errorf("isolated clone: %s has no URL", remote)
	}
	dir, err := os.MkdirTemp("", "clankerbar-closeout-")
	if err != nil {
		return "", nil, fmt.Errorf("isolated clone: %v", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if _, err := v.run(ctx, repo, "clone", "--quiet", "--shared", "--no-checkout", "--", repo, dir); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("isolated clone: %v", err)
	}
	if _, err := v.run(ctx, dir, "remote", "set-url", remote, url); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("isolated clone: point %s at %s: %v", remote, url, err)
	}
	return dir, cleanup, nil
}

// ConflictPaths computes the paths a merge of headRef into baseRef would conflict
// on, WITHOUT touching any worktree: `git merge-tree --write-tree --name-only`
// answers entirely from objects, writing only the merged tree it computed.
//
// The refs are used exactly as given, so a caller that wants freshly fetched tips
// fetches them first (FetchRef) and passes the remote-tracking refs. A command
// failure is an error; exit status 1 with paths is the conflict ANSWER, not a
// failure.
func (v *Verifier) ConflictPaths(ctx context.Context, repo, baseRef, headRef string) ([]string, error) {
	out, runErr := v.gitRunOut(ctx, repo, nil, "merge-tree", "--write-tree", "--name-only", baseRef, headRef)
	paths, parseErr := parseMergeTreeConflicts(out)
	if parseErr == nil {
		// Either the conflicts git named, or none at all (the trees merged
		// cleanly, which a caller that expected a conflict still needs to know).
		return paths, nil
	}
	if runErr == nil {
		return nil, fmt.Errorf("merge-tree %s..%s: %w", baseRef, headRef, parseErr)
	}
	return nil, fmt.Errorf("merge-tree %s..%s: %v (output: %s)", baseRef, headRef, runErr, firstLine(out))
}

// parseMergeTreeConflicts reads the path section of `git merge-tree --write-tree
// --name-only`: the first line is the computed tree's OID, the conflicted paths
// follow one per line, and a blank line ends the section before git's
// informational messages. An output that does not start with an object id is not
// that shape and is reported as an error rather than parsed into nonsense.
func parseMergeTreeConflicts(out string) ([]string, error) {
	lines := strings.Split(out, "\n")
	if len(lines) == 0 || !isCommitID(strings.TrimSpace(lines[0])) {
		return nil, errors.New("output does not start with a tree id")
	}
	var paths []string
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			break
		}
		paths = append(paths, strings.TrimSpace(line))
	}
	return paths, nil
}

// FetchRef fetches one branch from remote into its remote-tracking ref, forced:
// a task branch that had to be rebased, or a base that was rewritten, must still
// fetch rather than fail an ancestry check for want of the newer object. The
// fetch is scoped to the account that owns the remote, like every other network
// read here.
//
// The force rewrites the destination ref, so a caller must own the repository it
// passes: the close-out gives this an IsolatedClone, never a checkout a session is
// working in.
func (v *Verifier) FetchRef(ctx context.Context, repo, remote, branch string) error {
	if strings.TrimSpace(branch) == "" {
		return errors.New("fetch ref: no branch")
	}
	ref := "refs/heads/" + branch
	_, err := v.runScoped(ctx, repo, "fetch", "--quiet", "--no-tags", "--", remote, "+"+ref+":refs/remotes/"+remote+"/"+branch)
	return err
}

// RemoteTip reads the remote's tip for one branch. Empty with a nil error means
// the remote genuinely has no such branch.
func (v *Verifier) RemoteTip(ctx context.Context, repo, remote, branch string) (string, error) {
	return v.lsRemote(ctx, repo, remote, branch)
}

// MergedOnRemote answers: is sha an ancestor of remote's branch tip? It is the
// close-out's post-merge check — the same ancestor test `MergeVerified` records,
// run by the driver on the merge commit GitHub made rather than trusted from a
// session's declaration.
//
// The commit is fetched for when it is not local, which is the normal case
// immediately after a merge GitHub performed: the object arrives with the branch
// it was merged to. Everything that cannot be settled is Unknown, never a pass —
// the discipline the whole package keeps.
func (v *Verifier) MergedOnRemote(ctx context.Context, repo, remote, branch, sha string) Check {
	if !isCommitID(sha) {
		return Check{Kind: CommitMerged, Status: Unknown, Detail: fmt.Sprintf(
			"declared merge commit %q is not a commit id — a revision expression is not a delivery and is not checked as one", sha)}
	}
	return v.mergedOnRemote(ctx, repo, remote, sha, branch)
}

// ghAPIScope resolves the GH_TOKEN that scopes one gh invocation to the account
// owning slug ("owner/name"), or nil when no scoping is derivable. See the
// prcheck.go header: the ambient credential serves whichever account is active,
// which need not be the one that can see the repository.
func (v *Verifier) ghAPIScope(ctx context.Context, dir, slug string) []string {
	owner := strings.TrimSpace(slug)
	if i := strings.IndexByte(owner, '/'); i > 0 {
		owner = owner[:i]
	} else {
		owner = ""
	}
	if owner == "" {
		return nil
	}
	if token := v.ghTokenFor(ctx, dir, owner); token != "" {
		return []string{"GH_TOKEN=" + token}
	}
	return nil
}
