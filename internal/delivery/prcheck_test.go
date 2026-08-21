package delivery

// prcheck_test.go — CLA-310. Every refusal the gate can produce is tested
// against a fake `gh`, because the point of the gate is what it does with
// GitHub's ANSWERS, not that it can reach GitHub. The empty-rollup test is the
// one that would have caught PR #208; the naive-predicate test beside it pins
// WHY the inversion matters, so a future refactor cannot quietly reintroduce
// the wave-through.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- fixtures ---------------------------------------------------------------

const (
	jsonMergeablePassing = `{"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","statusCheckRollup":[` +
		`{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"},` +
		`{"__typename":"StatusContext","context":"docs","state":"SUCCESS"}],"url":"https://github.com/acme/widgets/pull/7"}`
	jsonEmptyRollup = `{"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","statusCheckRollup":[]}`
	jsonConflicting = `{"mergeable":"CONFLICTING","mergeStateStatus":"DIRTY","statusCheckRollup":[]}`
	jsonUnknown     = `{"mergeable":"UNKNOWN","mergeStateStatus":"UNKNOWN","statusCheckRollup":null}`
	jsonPending     = `{"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","statusCheckRollup":[` +
		`{"__typename":"CheckRun","name":"ci","status":"IN_PROGRESS"}]}`
	jsonFailing = `{"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","statusCheckRollup":[` +
		`{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"FAILURE"},` +
		`{"__typename":"CheckRun","name":"docs","status":"COMPLETED","conclusion":"SUCCESS"}]}`
)

// prEnv builds a workdir whose repo has a github.com-shaped origin — the URL
// is only configuration, so no network is ever touched.
func prEnv(t *testing.T) string {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	work := filepath.Join(root, "repo")
	run(t, root, "git", "init", "-b", "main", work)
	configure(t, work)
	run(t, work, "git", "remote", "add", "origin", "https://github.com/acme/widgets.git")
	writeFile(t, filepath.Join(work, "f.txt"), "x")
	run(t, work, "git", "add", ".")
	run(t, work, "git", "commit", "-m", "init")
	return root
}

// fakeGH is a gh binary that always answers with output.
func fakeGH(t *testing.T, output string) string {
	t.Helper()
	return writeGH(t, fmt.Sprintf("cat <<'GHJSON'\n%s\nGHJSON\n", output))
}

// fakeGHSeq answers with outputs[0] on the first call, outputs[1] on the
// second, and repeats the last one from then on. It also returns the path of
// its call counter, so a test can assert how many reads happened.
func fakeGHSeq(t *testing.T, outputs ...string) (string, string) {
	t.Helper()
	if len(outputs) == 0 {
		t.Fatal("fakeGHSeq needs at least one output")
	}
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	var b strings.Builder
	b.WriteString("n=$(cat " + counter + " 2>/dev/null || echo 0)\n")
	b.WriteString("n=$((n+1))\n")
	b.WriteString("echo \"$n\" > " + counter + "\n")
	b.WriteString("case $n in\n")
	for i, out := range outputs {
		b.WriteString(fmt.Sprintf("  %d) cat <<'J%d'\n%s\nJ%d\n  ;;\n", i+1, i, out, i))
	}
	b.WriteString(fmt.Sprintf("  *) cat <<'JLAST'\n%s\nJLAST\n  ;;\nesac\n", outputs[len(outputs)-1]))
	return writeGHTo(t, filepath.Join(dir, "gh"), b.String()), counter
}

func writeGH(t *testing.T, body string) string {
	t.Helper()
	return writeGHTo(t, filepath.Join(t.TempDir(), "gh"), body)
}

func writeGHTo(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// prVerifier is the verifier under test with a fake gh and a polling window
// short enough for a unit test.
func prVerifier(t *testing.T, workdir, ghBin string) *Verifier {
	t.Helper()
	v := New(workdir, "origin")
	v.ghBin = ghBin
	v.prBudget = 150 * time.Millisecond
	v.prInterval = 10 * time.Millisecond
	return v
}

func verifyWith(t *testing.T, v *Verifier, c Claim) Report {
	t.Helper()
	return v.Verify(context.Background(), c)
}

// --- the gate ---------------------------------------------------------------

// THE test that would have caught PR #208: a delivery naming a PR whose check
// rollup is empty is REFUSED, and the refusal says so and what clears it.
// Mutation check: revert the inversion (make an empty rollup a pass, as the
// naive any-failure predicate does) and this test fails.
func TestEmptyCheckRollupIsRefusedNotPassed(t *testing.T) {
	dir := prEnv(t)
	v := prVerifier(t, dir, fakeGH(t, jsonEmptyRollup))

	rep := verifyWith(t, v, Claim{Label: "CLA-310", PR: "208"})
	c := mustStatus(t, rep, PRVerified, Fail)
	if !strings.Contains(c.Detail, "NO checks") {
		t.Errorf("refusal must say the rollup was empty: %s", c.Detail)
	}
	if !strings.Contains(c.Detail, "allow_unchecked_pr") {
		t.Errorf("refusal must name the opt-out that exists: %s", c.Detail)
	}
}

// Why the inversion is load-bearing: the natural implementation — refuse only
// when some check FAILED — returns "no failure found" for an empty rollup and
// waves the delivery through. This test pins what the naive predicate says
// about the same input the gate refuses, so the next reader cannot mistake
// the difference for an accident.
func TestNaiveAnyFailurePredicateWavesThroughAnEmptyRollup(t *testing.T) {
	naiveAnyFailure := func(rollup []ghCheck) bool {
		for _, c := range rollup {
			if !c.succeeded() {
				return true
			}
		}
		return false
	}
	var emptyRollup []ghCheck
	if naiveAnyFailure(emptyRollup) {
		t.Fatal("premise changed: the naive predicate now flags an empty rollup")
	}
	// The gate, on the identical input:
	if got := judgePR(ghPR{Mergeable: "MERGEABLE"}).verdict; got != prNoChecks {
		t.Fatalf("judgePR on an empty rollup = %v, want the prNoChecks refusal", got)
	}
}

func TestConflictingPRIsRefused(t *testing.T) {
	dir := prEnv(t)
	v := prVerifier(t, dir, fakeGH(t, jsonConflicting))

	rep := verifyWith(t, v, Claim{Label: "CLA-310", PR: "208"})
	c := mustStatus(t, rep, PRVerified, Fail)
	if !strings.Contains(c.Detail, "CONFLICTING") {
		t.Errorf("refusal must name the conflict: %s", c.Detail)
	}
	if !strings.Contains(c.Detail, "Resolve the conflict") {
		t.Errorf("refusal must name the action that clears it: %s", c.Detail)
	}
}

func TestMergeablePRWithPassingRollupIsAccepted(t *testing.T) {
	dir := prEnv(t)
	v := prVerifier(t, dir, fakeGH(t, jsonMergeablePassing))

	rep := verifyWith(t, v, Claim{Label: "CLA-310", PR: "7"})
	c := mustStatus(t, rep, PRVerified, Pass)
	if !strings.Contains(c.Detail, "MERGEABLE") || !strings.Contains(c.Detail, "2 checks") {
		t.Errorf("pass detail should name mergeability and the rollup: %s", c.Detail)
	}
}

func TestFailingCheckIsRefused(t *testing.T) {
	dir := prEnv(t)
	v := prVerifier(t, dir, fakeGH(t, jsonFailing))

	rep := verifyWith(t, v, Claim{Label: "CLA-310", PR: "7"})
	c := mustStatus(t, rep, PRVerified, Fail)
	if !strings.Contains(c.Detail, "FAILING") || !strings.Contains(c.Detail, "ci") {
		t.Errorf("refusal must name the failing check: %s", c.Detail)
	}
}

// --- skipped and neutral conclusions are not failures (review finding F1) ----

// SKIPPED and NEUTRAL mean a check RAN and did not fail - a path-filtered
// workflow had nothing to do, or the run decided its result does not count.
// Neither ever turns green, so calling them failures would refuse every
// delivery in repos that use either, forever: the wedge the no-CI opt-out
// exists to prevent, one shape narrower. They pass the gate and are named in
// the detail, so the pass never reads as all-green-on-the-merits.
func TestSkippedAndNeutralChecksAreNotFailures(t *testing.T) {
	j := judgePR(ghPR{
		Mergeable: "MERGEABLE",
		StatusCheckRollup: []ghCheck{
			{Name: "ci", Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Name: "docs", Status: "COMPLETED", Conclusion: "SKIPPED"},
			{Name: "lint", Status: "COMPLETED", Conclusion: "NEUTRAL"},
		},
	})
	if j.verdict != prPass || j.passed != 1 || j.neutral != 2 {
		t.Fatalf("skipped/neutral rollup: got %+v, want prPass with 1 passed, 2 neutral", j)
	}
	if len(j.failing) != 0 {
		t.Errorf("skipped/neutral must not land in the failing bucket: %+v", j.failing)
	}
}

// The carve-out is narrow: every other completed-but-not-SUCCESS conclusion -
// including ones this package has never seen - stays a refusal, because an
// unrecognised verdict is a finding, not a pass.
func TestGenuineFailureConclusionsStillRefuse(t *testing.T) {
	for _, conclusion := range []string{"FAILURE", "TIMED_OUT", "ACTION_REQUIRED", "CANCELLED", "STARTUP_FAILURE", "SOMETHING_NEW"} {
		j := judgePR(ghPR{
			Mergeable: "MERGEABLE",
			StatusCheckRollup: []ghCheck{
				{Name: "ci", Status: "COMPLETED", Conclusion: conclusion},
			},
		})
		if j.verdict != prChecksFailed {
			t.Errorf("conclusion %s: got %+v, want prChecksFailed", conclusion, j)
		}
	}
}

func TestSkippedCheckDoesNotWedgeADelivery(t *testing.T) {
	dir := prEnv(t)
	rollup := `{"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","statusCheckRollup":[` +
		`{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"},` +
		`{"__typename":"CheckRun","name":"docs","status":"COMPLETED","conclusion":"SKIPPED"}]}`
	v := prVerifier(t, dir, fakeGH(t, rollup))

	rep := verifyWith(t, v, Claim{Label: "CLA-310", PR: "7"})
	c := mustStatus(t, rep, PRVerified, Pass)
	if !strings.Contains(c.Detail, "1 check skipped/neutral") {
		t.Errorf("pass detail should name the skipped check rather than average it in: %s", c.Detail)
	}
}

// --- the PR field is a NUMBER before gh ever sees it (review finding F2) -----

// gh resolves a non-number argument as a BRANCH name, so a garbage or hostile
// PR field could make the gate verify whichever pull request is open for some
// unrelated branch and pass the delivery on it. Anything that is not a number
// (after the "#" a session likes to lead with) is refused without gh being
// consulted - this fake gh exits 42, so any invocation would show.
func TestPRFieldMustBeANumber(t *testing.T) {
	dir := prEnv(t)
	v := prVerifier(t, dir, writeGH(t, "exit 42\n"))

	for _, garbage := range []string{"staging", "main", "https://github.com/acme/widgets/pull/7"} {
		rep := verifyWith(t, v, Claim{Label: "CLA-310", PR: garbage})
		c := mustStatus(t, rep, PRVerified, Unknown)
		if !strings.Contains(c.Detail, "not a pull request NUMBER") {
			t.Errorf("PR field %q: detail should name the validation, not an outage: %s", garbage, c.Detail)
		}
	}
}

func TestHashPrefixedPRNumberIsAcceptedAndNormalized(t *testing.T) {
	dir := prEnv(t)
	argsFile := filepath.Join(t.TempDir(), "args")
	gh := writeGH(t, "printf '%s\\n' \"$@\" > "+argsFile+"\ncat <<'J'\n"+jsonMergeablePassing+"\nJ\n")
	v := prVerifier(t, dir, gh)

	rep := verifyWith(t, v, Claim{Label: "CLA-310", PR: "#7"})
	mustStatus(t, rep, PRVerified, Pass)

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("fake gh was not invoked: %v", err)
	}
	if !strings.Contains(string(args), "\n7\n") {
		t.Errorf("gh should receive the bare number, got args: %q", string(args))
	}
}

// mergeable: null must never read as MERGEABLE. A bounded wait that never
// resolves refuses, rather than hanging or assuming.
func TestUnresolvedMergeabilityRefusesAfterTheBound(t *testing.T) {
	dir := prEnv(t)
	v := prVerifier(t, dir, fakeGH(t, jsonUnknown))

	rep := verifyWith(t, v, Claim{Label: "CLA-310", PR: "7"})
	c := mustStatus(t, rep, PRVerified, Fail)
	if !strings.Contains(c.Detail, "UNKNOWN") {
		t.Errorf("refusal must say mergeability never resolved: %s", c.Detail)
	}
}

func TestPendingChecksRefuseAtTheBound(t *testing.T) {
	dir := prEnv(t)
	v := prVerifier(t, dir, fakeGH(t, jsonPending))

	rep := verifyWith(t, v, Claim{Label: "CLA-310", PR: "7"})
	c := mustStatus(t, rep, PRVerified, Fail)
	if !strings.Contains(c.Detail, "NOT finished") || !strings.Contains(c.Detail, "ci") {
		t.Errorf("refusal must name the pending check: %s", c.Detail)
	}
}

// The bounded wait is a WAIT: a PR that resolves mid-window is judged on its
// resolved state, so a normal CI that is seconds behind the push passes.
//
// The budget has to cover TWO gh process spawns (macOS spawns are slow, tens
// to hundreds of milliseconds each), not just two polls.
func TestPollingResolvesWhenGitHubCatchesUp(t *testing.T) {
	dir := prEnv(t)
	gh, counter := fakeGHSeq(t, jsonUnknown, jsonMergeablePassing)
	v := prVerifier(t, dir, gh)
	v.prBudget = 10 * time.Second

	rep := verifyWith(t, v, Claim{Label: "CLA-310", PR: "7"})
	mustStatus(t, rep, PRVerified, Pass)

	n, err := os.ReadFile(counter)
	if err != nil || strings.TrimSpace(string(n)) != "2" {
		t.Fatalf("expected exactly 2 gh reads (poll then resolve), got %q (%v)", n, err)
	}
}

// --- the no-CI decision -----------------------------------------------------

// The operator-answered decision on CLA-310, pinned: the empty-rollup refusal
// is downgraded to a WARN by the opt-out, and NOTHING ELSE is. A conflicted
// PR under the same opt-out is still a refusal, because a conflicted PR is
// the very state that produces an empty rollup.
func TestAllowUncheckedPRDowngradesOnlyTheEmptyRollup(t *testing.T) {
	dir := prEnv(t)

	v := prVerifier(t, dir, fakeGH(t, jsonEmptyRollup)).AllowUncheckedPR()
	rep := verifyWith(t, v, Claim{Label: "CLA-310", PR: "208"})
	c := mustStatus(t, rep, PRVerified, Warn)
	if !strings.Contains(c.Detail, "WARNING only") {
		t.Errorf("warned detail should say it is the opt-out talking: %s", c.Detail)
	}
	if rep.Failed() {
		t.Errorf("a WARN is not a failure: %s", render(rep))
	}

	vc := prVerifier(t, dir, fakeGH(t, jsonConflicting)).AllowUncheckedPR()
	repc := verifyWith(t, vc, Claim{Label: "CLA-310", PR: "208"})
	mustStatus(t, repc, PRVerified, Fail)
}

// --- degradation, which must never be a pass --------------------------------

func TestMissingGHIsAnExplicitRefusalToVerify(t *testing.T) {
	dir := prEnv(t)
	v := prVerifier(t, dir, "clankerbar-no-such-gh")

	rep := verifyWith(t, v, Claim{Label: "CLA-310", PR: "208"})
	c := mustStatus(t, rep, PRVerified, Unknown)
	if !strings.Contains(c.Detail, "not on PATH") {
		t.Errorf("degradation must name the missing prerequisite: %s", c.Detail)
	}
	if rep.Failed() {
		t.Errorf("cannot-verify is not a failure, but it must never be a pass either: %s", render(rep))
	}
}

func TestNonGitHubRemoteIsAnExplicitRefusalToVerify(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	work := filepath.Join(root, "repo")
	run(t, root, "git", "init", "-b", "main", work)
	configure(t, work)
	run(t, work, "git", "remote", "add", "origin", "https://gitlab.com/acme/widgets.git")
	writeFile(t, filepath.Join(work, "f.txt"), "x")
	run(t, work, "git", "add", ".")
	run(t, work, "git", "commit", "-m", "init")

	v := prVerifier(t, root, fakeGH(t, jsonMergeablePassing))
	rep := verifyWith(t, v, Claim{Label: "CLA-310", PR: "7"})
	c := mustStatus(t, rep, PRVerified, Unknown)
	if !strings.Contains(c.Detail, "has a github.com remote") {
		t.Errorf("detail should name the non-GitHub remote: %s", c.Detail)
	}
}

// A delivery naming a PR that does not exist is a false claim — refused, not
// ridden through as an outage.
func TestNonexistentPRIsRefused(t *testing.T) {
	dir := prEnv(t)
	gh := writeGH(t, "echo \"gh: Could not resolve to a PullRequest with the number 999.\" >&2\nexit 1\n")

	v := prVerifier(t, dir, gh)
	rep := verifyWith(t, v, Claim{Label: "CLA-310", PR: "999"})
	c := mustStatus(t, rep, PRVerified, Fail)
	if !strings.Contains(c.Detail, "not found") {
		t.Errorf("refusal should say the PR does not exist: %s", c.Detail)
	}
}

// --- scope ------------------------------------------------------------------

// No PR named, no PR check: the gate is inert on the ordinary branch/merge
// path, which is what keeps it from firing on every hand-off.
func TestNoPRNamedNoPRCheck(t *testing.T) {
	dir := prEnv(t)
	// A gh that would fail loudly if it were ever invoked.
	v := prVerifier(t, dir, writeGH(t, "exit 42\n"))

	rep := verifyWith(t, v, Claim{Label: "CLA-253", Branch: "clanker/x"})
	for _, c := range rep.Checks {
		if c.Kind == PRVerified {
			t.Fatalf("PR check ran without a PR named: %s", render(rep))
		}
	}
}

// A claim carrying ONLY a PR still gets checked — the fallback that finds the
// repository by its remote when the branch/commit checks have nothing to say.
func TestPROnlyClaimStillChecks(t *testing.T) {
	dir := prEnv(t)
	v := prVerifier(t, dir, fakeGH(t, jsonMergeablePassing))

	rep := verifyWith(t, v, Claim{Label: "CLA-310", PR: "7"})
	mustStatus(t, rep, PRVerified, Pass)
}

// --- pure pieces ------------------------------------------------------------

func TestGithubSlug(t *testing.T) {
	cases := []struct{ url, want string }{
		{"https://github.com/acme/widgets.git", "acme/widgets"},
		{"https://github.com/acme/widgets", "acme/widgets"},
		{"http://github.com/acme/widgets.git", "acme/widgets"},
		{"git@github.com:acme/widgets.git", "acme/widgets"},
		{"ssh://git@github.com/acme/widgets.git", "acme/widgets"},
		{"github.com:acme/widgets.git", "acme/widgets"},
		{"https://gitlab.com/acme/widgets.git", ""},
		{"https://github.enterprise.corp/acme/widgets.git", ""},
		{"https://mygithub.example.com/acme/widgets.git", ""},
		{"/Users/j/dev/clankerbar-cli", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got, _ := githubSlug(tc.url); got != tc.want {
			t.Errorf("githubSlug(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// The rollup mixes two entry shapes; both must be read, and an unrecognised
// __typename must be judged as a check run rather than skipped (skipping is
// how an empty-looking rollup would wave through).
func TestJudgePRReadsBothRollupShapes(t *testing.T) {
	pass := judgePR(ghPR{
		Mergeable: "MERGEABLE",
		StatusCheckRollup: []ghCheck{
			{Name: "ci", Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Context: "docs", State: "SUCCESS"},
		},
	})
	if pass.verdict != prPass || pass.passed != 2 {
		t.Errorf("both-success rollup: got %+v, want prPass with 2 passed", pass)
	}

	pending := judgePR(ghPR{
		Mergeable: "MERGEABLE",
		StatusCheckRollup: []ghCheck{
			{Name: "ci", Status: "IN_PROGRESS"},
			{Context: "docs", State: "PENDING"},
		},
	})
	if pending.verdict != prChecksPending || len(pending.pending) != 2 {
		t.Errorf("pending rollup: got %+v, want prChecksPending naming both", pending)
	}

	dirty := judgePR(ghPR{Mergeable: "UNKNOWN", MergeStateStatus: "DIRTY"})
	if dirty.verdict != prConflict {
		t.Errorf("DIRTY merge state: got %+v, want prConflict without waiting", dirty)
	}
}
