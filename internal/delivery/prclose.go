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
// difference is that `allow_unchecked_pr` does not reach this judgement: an empty
// check rollup is a refusal here whatever the project's delivery opt-out says,
// because a deterministic merge has nobody reading it afterwards.
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

// PRRef is one open pull request as `gh pr list` serves it.
type PRRef struct {
	Number  int
	URL     string
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
		URL         string `json:"url"`
		BaseRefName string `json:"baseRefName"`
		HeadRefName string `json:"headRefName"`
		HeadRefOid  string `json:"headRefOid"`
	}
	if jerr := json.Unmarshal([]byte(out), &listed); jerr != nil {
		return nil, fmt.Errorf("could not parse gh pr list output: %v", jerr)
	}
	refs := make([]PRRef, 0, len(listed))
	for _, pr := range listed {
		refs = append(refs, PRRef{
			Number:  pr.Number,
			URL:     pr.URL,
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
		URL:     view.URL,
		Base:    view.BaseRefName,
		Head:    view.HeadRefName,
		HeadOID: view.HeadRefOid,
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
// expectedHeadOID pins the merge to the revision that was JUDGED: the API's own
// `sha` parameter refuses the merge when the head has moved since, so a push that
// lands between the judgement and this call cannot be merged unreviewed and
// unjudged. Empty skips the pin (only for a caller that has no head to name).
func (v *Verifier) MergePR(ctx context.Context, repo, slug string, num int, expectedHeadOID string) (string, error) {
	if strings.TrimSpace(slug) == "" {
		return "", errors.New("merge PR: no repository slug")
	}
	if num <= 0 {
		return "", errors.New("merge PR: no pull request number")
	}
	if _, err := exec.LookPath(v.ghBin); err != nil {
		return "", fmt.Errorf("merge PR: %s is not on PATH", v.ghBin)
	}
	args := []string{
		"api", "--method", "PUT",
		fmt.Sprintf("repos/%s/pulls/%d/merge", slug, num),
		"-f", "merge_method=merge",
	}
	if head := strings.TrimSpace(expectedHeadOID); head != "" {
		args = append(args, "-f", "sha="+head)
	}
	out, err := v.runGHScoped(ctx, repo, v.ghAPIScope(ctx, repo, slug), args...)
	if err != nil {
		return "", err
	}
	var res struct {
		Merged  bool   `json:"merged"`
		SHA     string `json:"sha"`
		Message string `json:"message"`
	}
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		return "", fmt.Errorf("could not parse the merge response: %v", jerr)
	}
	if !res.Merged || strings.TrimSpace(res.SHA) == "" {
		return "", fmt.Errorf("GitHub did not merge PR %d: %s", num, strings.TrimSpace(res.Message))
	}
	return res.SHA, nil
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
	switch {
	case parseErr == nil && len(paths) > 0:
		return paths, nil
	case runErr == nil:
		// The trees merged cleanly. A caller that expected a conflict has the
		// answer it needs: there is nothing to name.
		return nil, nil
	case parseErr != nil:
		return nil, fmt.Errorf("merge-tree %s..%s: %v (output: %s)", baseRef, headRef, runErr, firstLine(out))
	default:
		return nil, fmt.Errorf("merge-tree %s..%s: %v (output: %s)", baseRef, headRef, runErr, firstLine(out))
	}
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
