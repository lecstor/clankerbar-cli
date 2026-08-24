package delivery

// prcheck.go — the delivery path's check on the pull request a session names
// (CLA-310).
//
// CLA-309 documented the disease: a PR that CONFLICTS with its base gets no
// `pull_request` event at all, because GitHub cannot compute
// `refs/pull/<n>/merge` — so it reports ZERO checks rather than a failing one,
// and every reader sees quiet rather than broken. PR #208 shipped to in_review
// in exactly that state, concluded "no CI runs here", and turned out to have
// skipped 78 tests. The clankerbar repo's CI now guards the conflict case
// itself; this check is the side that does not depend on any workflow being
// configured correctly, because it runs in every repo the driver works.
//
// # The two conditions
//
// A delivery naming a PR is verified only when BOTH hold:
//
//  1. The PR is MERGEABLE. CONFLICTING is a refusal — it is the state that
//     produces the silence, and independently a reason not to hand the work to
//     a human.
//  2. The PR carries a check rollup that actually ran and passed. An EMPTY
//     rollup is a refusal, not a pass. That inversion is the whole point: the
//     natural implementation (`any(check.conclusion == FAILURE)`) returns
//     false for a PR with no checks and waves it through — the exact bug. See
//     TestNaiveAnyFailurePredicateWavesThroughAnEmptyRollup, which pins what
//     the naive predicate would have said about the same input.
//
// # Laziness, and the bounded wait
//
// GitHub computes `mergeable` in the background; the first read is often
// UNKNOWN, and treating that as MERGEABLE reintroduces the hole. The check
// polls until it resolves, within prPollBudget; a wait that exhausts is an
// explicit refusal, never an assumption and never a hang. The same window
// absorbs a rollup whose checks are still registering or running.
//
// # The no-CI repo
//
// A repo may legitimately have no CI at all, and refusing every delivery
// there would wedge the driver shut. The shipped decision (operator-answered
// on CLA-310): the empty-rollup condition is a HARD refusal by default, with
// the per-project config opt-out `allow_unchecked_pr: true` downgrading it to
// a WARN; the MERGEABLE condition is NEVER relaxed. Silence-reads-as-pass is
// the bug this family exists to kill, so the safe state is the default and
// the loose state is a visible, operator-owned config line. Pinned by
// TestAllowUncheckedPRDowngradesOnlyTheEmptyRollup.
//
// # Mechanism
//
// The stated default for API access is the `gh` CLI: operator machines
// already authenticate GitHub through it, and it inherits that auth for free.
// When `gh` is absent the check degrades to an explicit refusal-to-verify
// (Unknown), never a pass — the same three-way discipline as every other
// check in this package. `doctor` reports `gh` availability so the gate's
// prerequisite is seen before it fires.
//
// # Whose credentials the API reads carry (CLA-460)
//
// CLA-458 scoped this package's GIT reads to the account that owns the
// github.com remote, because the ambient credential helper serves whichever
// account happens to be ACTIVE. The gh API reads have the same disease by a
// different mechanism: `gh pr view` authenticates as the active account too,
// so a repo owned by the non-active account fails with "Could not resolve to
// a PullRequest" and a real delivery degrades to Unknown/WARN. Git takes a
// per-command credential through config rewrite; gh takes one through the
// GH_TOKEN environment variable, which outranks its keyring. So before its
// first poll, prCheck derives the owner from the remote URL exactly as the
// git side does, resolves THAT account's token through `gh auth token
// --user`, and hands gh the token as GH_TOKEN in the child's environment —
// env, never argv, because `-c`-style arguments print to ps(1) while child
// env is already this driver's hygiene model (CLANKERBAR_API_KEY rides the
// same way). An ambient GH_TOKEN/GITHUB_TOKEN is DROPPED from such a child
// rather than appended over: duplicate env entries have no portable
// precedence, and which credential a child resolves is not something to bet
// on. Every fallback lands on byte-identical pre-CLA-460 behavior — ssh
// remotes, non-github hosts, and owners gh cannot resolve a token for all
// run unscoped, exactly as before.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// PRVerified is the check on the pull request a session named in a delivery:
// mergeable, and carrying a check rollup that actually ran and passed.
const PRVerified Kind = "pr"

// The polling window. Budgeted well inside the loop's deliveryCheckTimeout so
// one slow PR cannot starve the git checks that share the report's deadline;
// long enough to ride out `mergeable`'s lazy first read and a CI that is
// seconds behind the push.
const (
	prPollBudget   = 30 * time.Second
	prPollInterval = 2 * time.Second

	// ghWaitDelay matches the git runner's cut-off discipline: os/exec's Wait
	// blocks until every child holding the pipes closes them, so an explicit
	// WaitDelay is what actually bounds a wedged helper.
	ghWaitDelay = 5 * time.Second
)

// ghCheck is one entry of `gh pr view --json statusCheckRollup`. The rollup
// mixes two shapes: legacy StatusContext entries (commit statuses, with
// `context` and `state`) and CheckRun entries (actions checks, with `name`,
// `status` and `conclusion`). Both must be understood, because a repo using
// either alone is normal.
type ghCheck struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Context    string `json:"context"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

func (c ghCheck) label() string {
	switch {
	case c.Name != "":
		return c.Name
	case c.Context != "":
		return c.Context
	default:
		return c.Typename
	}
}

// isCheckRun distinguishes the two rollup shapes. __typename decides when it
// is present (real gh always sends it); without it, a legacy-shaped entry —
// a state and nothing else — is a StatusContext. An unrecognised __typename
// is treated as a CheckRun, whose fields are the ones a future shape most
// likely keeps.
func (c ghCheck) isCheckRun() bool {
	if strings.EqualFold(c.Typename, "StatusContext") {
		return false
	}
	if c.Typename == "" && c.State != "" && c.Status == "" && c.Conclusion == "" {
		return false
	}
	return true
}

// completed reports whether this entry has reached a verdict at all.
func (c ghCheck) completed() bool {
	if c.isCheckRun() {
		return strings.EqualFold(c.Status, "COMPLETED")
	}
	s := strings.ToUpper(strings.TrimSpace(c.State))
	return s != "PENDING" && s != "EXPECTED"
}

// succeeded reports whether the verdict, once reached, was success.
func (c ghCheck) succeeded() bool {
	if c.isCheckRun() {
		return strings.EqualFold(c.Conclusion, "SUCCESS")
	}
	return strings.EqualFold(strings.TrimSpace(c.State), "SUCCESS")
}

// neutral reports a check run that COMPLETED WITHOUT FAILING and will never
// go green either: SKIPPED (a path-filtered workflow had nothing to do on
// this push) and NEUTRAL (the run decided its result does not count).
// Classifying these as failures would refuse every delivery in repos that
// use either - forever, because a skipped check never turns green - which is
// the same wedge the no-CI opt-out exists to prevent, narrowed to one shape.
// They are still named in a passing detail, so a human reading the log knows
// the rollup was not all-green-on-the-merits. Any OTHER completed conclusion
// that is not SUCCESS stays in the failure bucket: a conclusion this package
// does not recognise is a finding, not a pass.
func (c ghCheck) neutral() bool {
	if !c.isCheckRun() {
		return false // commit statuses have no neutral shape
	}
	switch strings.ToUpper(strings.TrimSpace(c.Conclusion)) {
	case "SKIPPED", "NEUTRAL":
		return true
	}
	return false
}

// ghPR is the slice of `gh pr view --json ...` the gate reads.
type ghPR struct {
	Mergeable         string    `json:"mergeable"`         // MERGEABLE | CONFLICTING | UNKNOWN
	MergeStateStatus  string    `json:"mergeStateStatus"`  // CLEAN | DIRTY | BLOCKED | ...
	StatusCheckRollup []ghCheck `json:"statusCheckRollup"` // null when none
	URL               string    `json:"url"`
}

// prVerdict is the classified outcome of one read of the PR. prConflict,
// prChecksFailed and prPass settle immediately; the rest are WAITING verdicts
// — each names why the PR is not yet decidable, which is what the
// exhausted-wait settlement reports.
type prVerdict int

const (
	prPass prVerdict = iota
	prConflict
	prChecksFailed
	prChecksPending // rollup present but not finished; poll
	prNoChecks      // mergeable but empty rollup; poll (CI may not have registered)
	prMergeableUnknown
)

// prJudgement is a verdict plus the labels and counts its message names.
type prJudgement struct {
	verdict prVerdict
	passed  int // rollup entries that completed successfully (prPass)
	neutral int // completed without failing and never going green: SKIPPED/NEUTRAL
	failing []string
	pending []string
}

// judgePR classifies one read. Pure, so the inversion at its heart is unit-
// testable without a gh process: judgePR(ghPR{Mergeable: "MERGEABLE"}).verdict
// is prNoChecks, the refusal the naive any-failure predicate would have missed.
//
// Three verdicts WAIT rather than settle: an uncomputed `mergeable`, a rollup
// whose checks have not finished, and a MERGEABLE PR with no rollup yet (a
// just-pushed CI registers late). Each is polled within the budget and only
// becomes its refusal when the window closes — but they are distinct verdicts,
// because the refusal must say which of the three it was.
func judgePR(v ghPR) prJudgement {
	state := strings.ToUpper(strings.TrimSpace(v.Mergeable))
	dirty := strings.ToUpper(strings.TrimSpace(v.MergeStateStatus)) == "DIRTY"
	// DIRTY is honoured even while `mergeable` still reads UNKNOWN: it is the
	// same fact arrived from the other field, and a conflict needs no second
	// opinion.
	if state == "CONFLICTING" || dirty {
		return prJudgement{verdict: prConflict}
	}
	if state != "MERGEABLE" {
		// UNKNOWN, empty, or anything unrecognised: GitHub has not answered.
		// Assuming MERGEABLE here is the hole this gate exists to close.
		return prJudgement{verdict: prMergeableUnknown}
	}
	if len(v.StatusCheckRollup) == 0 {
		// THE inversion: no checks is a finding, not an absence of findings.
		return prJudgement{verdict: prNoChecks}
	}
	j := prJudgement{}
	for _, c := range v.StatusCheckRollup {
		switch {
		case !c.completed():
			j.pending = append(j.pending, c.label())
		case c.succeeded():
			j.passed++
		case c.neutral():
			j.neutral++
		default:
			j.failing = append(j.failing, c.label())
		}
	}
	switch {
	case len(j.failing) > 0:
		j.verdict = prChecksFailed
	case len(j.pending) > 0:
		j.verdict = prChecksPending
	default:
		j.verdict = prPass
	}
	return j
}

// settlePR renders a judgement as the check the driver records. Every refusal
// names WHICH condition failed and the action that clears it — "delivery
// rejected" sends an agent hunting through settings files, which four runs
// have already burned whole iterations on.
func settlePR(j prJudgement, prNum, slug string, waited time.Duration, allowUnchecked bool) Check {
	name := func(labels []string) string { return strings.Join(labels, ", ") }
	switch j.verdict {
	case prPass:
		skipped := ""
		if j.neutral > 0 {
			// Named, not averaged in: a rollup that "passed" with a skipped
			// job is not the same fact as an all-green one, and the reader of
			// a 3am log should not have to guess which happened.
			skipped = fmt.Sprintf(" (%s skipped/neutral)", plural(j.neutral, "check"))
		}
		return Check{Kind: PRVerified, Status: Pass, Detail: fmt.Sprintf(
			"PR %s (%s) is MERGEABLE and its %s passed%s",
			prNum, slug, plural(j.passed, "check"), skipped)}
	case prConflict:
		return Check{Kind: PRVerified, Status: Fail, Detail: fmt.Sprintf(
			"PR %s (%s) is CONFLICTING with its base — the delivery is refused. "+
				"A conflicted PR runs NO checks (GitHub cannot compute the merge ref), so quiet CI proves nothing. "+
				"Resolve the conflict, push, and confirm CI runs and goes green before declaring the delivery again",
			prNum, slug)}
	case prChecksFailed:
		return Check{Kind: PRVerified, Status: Fail, Detail: fmt.Sprintf(
			"PR %s (%s) has FAILING checks (%s) — the delivery is refused. "+
				"Fix what failed, push, and let CI go green before declaring the delivery again",
			prNum, slug, name(j.failing))}
	case prChecksPending:
		return Check{Kind: PRVerified, Status: Fail, Detail: fmt.Sprintf(
			"PR %s (%s) has checks that have NOT finished (%s) — the delivery is refused: a rollup that has not run is not a passing one. "+
				"Wait for CI to complete and go green, then declare the delivery again",
			prNum, slug, name(j.pending))}
	case prNoChecks:
		if allowUnchecked {
			return Check{Kind: PRVerified, Status: Warn, Detail: fmt.Sprintf(
				"PR %s (%s) carries NO checks — WARNING only (allow_unchecked_pr: true). "+
					"Confirm CI actually ran on this PR: a conflicted PR starts none, and silence is not a pass",
				prNum, slug)}
		}
		return Check{Kind: PRVerified, Status: Fail, Detail: fmt.Sprintf(
			"PR %s (%s) carries NO checks — the delivery is REFUSED: an empty check rollup is not a pass. "+
				"Confirm CI runs on this PR (a conflicted PR starts none), wait for it to go green, then declare again. "+
				"To accept unchecked PRs for this project, set allow_unchecked_pr: true",
			prNum, slug)}
	case prMergeableUnknown:
		return Check{Kind: PRVerified, Status: Fail, Detail: fmt.Sprintf(
			"GitHub did not report whether PR %s (%s) is mergeable (still UNKNOWN after ~%s) — the delivery is refused rather than assumed. "+
				"This is normally GitHub still computing; try again shortly",
			prNum, slug, waited.Round(time.Second))}
	default:
		// Unreachable while every verdict has a case; kept because this
		// package never guesses.
		return Check{Kind: PRVerified, Status: Unknown, Detail: fmt.Sprintf("PR %s could not be judged", prNum)}
	}
}

// githubSlug extracts owner/name from a git remote URL, or reports that the
// remote is not a github.com one. The known prefix forms are matched
// explicitly rather than searching for "github.com" anywhere in the string —
// a host like "mygithub.example.com" contains the needle and must not match.
func githubSlug(remoteURL string) (string, bool) {
	u := strings.TrimSpace(remoteURL)
	u = strings.TrimSuffix(u, ".git")
	for _, p := range []string{
		"https://github.com/",
		"http://github.com/",
		"ssh://git@github.com/",
		"git@github.com:",
		"github.com:",
	} {
		if i := strings.Index(u, p); i >= 0 {
			rest := strings.TrimLeft(u[i+len(p):], "/")
			parts := strings.Split(rest, "/")
			if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
				return parts[0] + "/" + parts[1], true
			}
			return "", false
		}
	}
	return "", false
}

// ghNotFoundError marks a `gh` failure that means THE PR IS NOT THERE, rather
// than that the tool could not run. A delivery naming a PR that does not
// exist is a false claim to refuse, not an outage to ride through.
type ghNotFoundError struct{ detail string }

func (e *ghNotFoundError) Error() string { return e.detail }

// normalizePR accepts the shapes a session actually reports - "42", "#42" -
// and rejects everything else BEFORE gh sees it. The rejection is the point:
// gh resolves a non-number argument as a BRANCH name, so an unvalidated
// string would make this check verify whichever pull request happens to be
// open for some unrelated branch - and could pass a delivery on it. A PR is
// a number; anything else is not evidence about THIS delivery.
func normalizePR(pr string) string {
	p := strings.TrimPrefix(strings.TrimSpace(pr), "#")
	if p == "" {
		return ""
	}
	for _, r := range p {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return p
}

// prCheck verifies the PR named in a delivery: mergeable, with a check rollup
// that actually ran and passed. See the file comment for the two conditions,
// the bounded wait, the no-CI decision, and whose credentials the reads carry.
//
// The repository is the one the earlier checks resolved (they share this
// Report), or the first candidate whose origin is a github.com remote — the
// PR number belongs to whoever owns that remote, which is the only guess this
// check allows itself, and only under the ordinary layout.
func (v *Verifier) prCheck(ctx context.Context, repos []string, prNum string, rep *Report) Check {
	num := normalizePR(prNum)
	if num == "" {
		return Check{Kind: PRVerified, Status: Unknown, Detail: fmt.Sprintf(
			"PR %q is not a pull request NUMBER - the delivery's pull request goes unverified, which is not a pass. "+
				"Report the bare number (42 or #42), not a branch or URL", prNum)}
	}
	prNum = num

	if _, err := exec.LookPath(v.ghBin); err != nil {
		return Check{Kind: PRVerified, Status: Unknown, Detail: fmt.Sprintf(
			"PR %s cannot be verified: %s is not on PATH — the delivery's pull request goes unchecked, which is not a pass. Install the GitHub CLI so this gate can run", prNum, v.ghBin)}
	}

	repo := rep.Repo
	if repo == "" {
		for _, r := range repos {
			if url, err := v.remoteURL(ctx, r); err == nil {
				if _, ok := githubSlug(url); ok {
					repo = r
					break
				}
			}
		}
	}
	if repo == "" && len(repos) > 0 {
		return Check{Kind: PRVerified, Status: Unknown, Detail: fmt.Sprintf(
			"PR %s cannot be verified: no repository at or below %s has a github.com remote, so where the pull request lives cannot be told", prNum, v.workdir)}
	}
	if repo == "" {
		return Check{Kind: PRVerified, Status: Unknown, Detail: fmt.Sprintf(
			"PR %s cannot be verified: no repository at or below %s could be located", prNum, v.workdir)}
	}
	url, err := v.remoteURL(ctx, repo)
	if err != nil {
		return Check{Kind: PRVerified, Status: Unknown, Detail: fmt.Sprintf(
			"PR %s cannot be verified: could not read the remote of %s: %v", prNum, repo, err)}
	}
	slug, ok := githubSlug(url)
	if !ok {
		return Check{Kind: PRVerified, Status: Unknown, Detail: fmt.Sprintf(
			"PR %s cannot be verified: the remote of %s (%s) is not a github.com repository", prNum, repo, url)}
	}

	// Scope every API read below to the account that owns the remote, the way
	// CLA-458 scoped the git ones. Resolved once here, not per poll: the polls
	// then share one identity, and the worst case stays a couple of gh spawns
	// per verification. Empty means "run unscoped" — the fallback the header
	// documents.
	var ghScope []string
	if owner := githubOwner(url); owner != "" {
		if token := v.ghTokenFor(ctx, repo, owner); token != "" {
			ghScope = []string{"GH_TOKEN=" + token}
		}
	}

	start := time.Now()
	deadline := start.Add(v.prBudget)
	for {
		j, err := v.readPR(ctx, repo, slug, prNum, ghScope)
		if err != nil {
			var nf *ghNotFoundError
			if errors.As(err, &nf) {
				return Check{Kind: PRVerified, Status: Fail, Detail: fmt.Sprintf(
					"PR %s was not found in %s — the delivery names a pull request that does not exist; correct the number and declare again", prNum, slug)}
			}
			return Check{Kind: PRVerified, Status: Unknown, Detail: fmt.Sprintf(
				"PR %s could not be verified: %v", prNum, err)}
		}
		switch j.verdict {
		case prPass, prConflict, prChecksFailed:
			// Decidable now: settle on what this read said.
			return settlePR(j, prNum, slug, time.Since(start), v.allowUncheckedPR)
		}
		// A waiting verdict. Once the window closes it becomes the refusal it
		// now names — never an assumption.
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			return settlePR(j, prNum, slug, time.Since(start), v.allowUncheckedPR)
		}
		select {
		case <-ctx.Done():
			return Check{Kind: PRVerified, Status: Unknown, Detail: fmt.Sprintf(
				"PR %s verification was interrupted: %v", prNum, ctx.Err())}
		case <-time.After(v.prInterval):
		}
	}
}

// readPR makes one `gh pr view` call and classifies what came back. The
// waiting verdicts (prMergeableUnknown, prNoChecks, prChecksPending) mean
// "true so far as it went, but not yet decidable" — the caller polls within
// its budget. scope carries the caller's credential scoping, nil to run
// unscoped.
func (v *Verifier) readPR(ctx context.Context, dir, slug, prNum string, scope []string) (prJudgement, error) {
	out, err := v.runGHScoped(ctx, dir, scope, "pr", "view", prNum,
		"--repo", slug,
		"--json", "mergeable,mergeStateStatus,statusCheckRollup,url")
	if err != nil {
		return prJudgement{verdict: prMergeableUnknown}, err
	}
	var view ghPR
	if jerr := json.Unmarshal([]byte(out), &view); jerr != nil {
		return prJudgement{verdict: prMergeableUnknown}, fmt.Errorf("could not parse gh pr view output: %v", jerr)
	}
	return judgePR(view), nil
}

// runGH executes one gh command in dir and returns its trimmed stdout, with
// the same interaction-hostile environment and cut-off discipline as the git
// runner: an unattended run has no terminal to answer prompts with, and a
// wedged helper must not hold the check past its WaitDelay.
func (v *Verifier) runGH(ctx context.Context, dir string, args ...string) (string, error) {
	return v.runGHScoped(ctx, dir, nil, args...)
}

// runGHScoped is runGH plus environment additions for THIS invocation — the
// shape credential scoping has to take, since a token must reach this child
// and no other. When additions ride along, the ambient GH_TOKEN and
// GITHUB_TOKEN are dropped from the child's environment first rather than
// appended over: an env array may carry duplicates and which entry a child
// resolves is unspecified, so shadowing is not a guarantee a credential is
// allowed to lean on. Empty additions leave the environment untouched,
// byte-for-byte the pre-scoping behavior.
func (v *Verifier) runGHScoped(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	env := append(os.Environ(),
		"GH_NO_UPDATE_NOTIFIER=1",
		"GH_FORCE_TTY=0",
		"GIT_TERMINAL_PROMPT=0",
	)
	if len(extraEnv) > 0 {
		scoped := make([]string, 0, len(env)+len(extraEnv))
		for _, e := range env {
			if i := strings.IndexByte(e, '='); i > 0 && isGhTokenVar(e[:i]) {
				continue
			}
			scoped = append(scoped, e)
		}
		env = append(scoped, extraEnv...)
	}
	cmd := exec.CommandContext(ctx, v.ghBin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = ghWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		low := strings.ToLower(msg)
		if strings.Contains(low, "could not resolve to a pullrequest") ||
			strings.Contains(low, "not found") ||
			strings.Contains(low, "no pull requests") {
			return "", &ghNotFoundError{detail: fmt.Sprintf("gh %s: %s", args[0], firstLine(msg))}
		}
		if msg != "" {
			return "", fmt.Errorf("gh %s: %w: %s", args[0], err, firstLine(msg))
		}
		return "", fmt.Errorf("gh %s: %w", args[0], err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// isGhTokenVar reports whether an environment NAME is one of the variables gh
// reads a token from ahead of its keyring. Both spellings must go when a
// scoped token rides the invocation, or a stale ambient value could win by
// accident of position.
func isGhTokenVar(name string) bool {
	switch name {
	case "GH_TOKEN", "GITHUB_TOKEN":
		return true
	}
	return false
}
