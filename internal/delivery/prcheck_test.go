package delivery

// prcheck_test.go — CLA-310. Every refusal the gate can produce is tested
// against a fake `gh`, because the point of the gate is what it does with
// GitHub's ANSWERS, not that it can reach GitHub. The empty-rollup test is the
// one that would have caught PR #208; the naive-predicate test beside it pins
// WHY the inversion matters, so a future refactor cannot quietly reintroduce
// the wave-through.
//
// Since CLA-460 the verifier also probes `auth token --user` before its first
// API read, so every fake below serves that probe out of band (see
// writeGHTo). Every CLA-460 test pins both legs: which ACCOUNT the probe asked
// for (wantProbeOwner) and exactly which credentials the API read resolved.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- fixtures ---------------------------------------------------------------

const (
	jsonMergeablePassing = `{"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","statusCheckRollup":[` +
		`{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"},` +
		`{"__typename":"StatusContext","context":"docs","state":"SUCCESS"}],"url":"https://github.com/acme/widgets/pull/7",` +
		`"baseRefName":"staging","headRefName":"clanker/x","headRefOid":"abc1234def5678"}`
	jsonEmptyRollup = `{"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","statusCheckRollup":[]}`
	jsonConflicting = `{"mergeable":"CONFLICTING","mergeStateStatus":"DIRTY","statusCheckRollup":[]}`
	jsonUnknown     = `{"mergeable":"UNKNOWN","mergeStateStatus":"UNKNOWN","statusCheckRollup":null}`
	jsonPending     = `{"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","statusCheckRollup":[` +
		`{"__typename":"CheckRun","name":"ci","status":"IN_PROGRESS"}]}`
	jsonFailing = `{"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","statusCheckRollup":[` +
		`{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"FAILURE"},` +
		`{"__typename":"CheckRun","name":"docs","status":"COMPLETED","conclusion":"SUCCESS"}]}`

	// ghFakeToken is what every fake gh answers the `auth token --user`
	// credential probe with when GH_FAKE_TOKEN names it: the asserted VALUE,
	// so the chain "derive owner -> resolve THAT account -> plumb the resolved
	// value" is pinned end to end in the CLA-460 tests. The value alone proves
	// only that whatever account was asked reached the child; the probe's
	// recorded `user=` line is what pins WHICH account it was asked for.
	ghFakeToken = "gho_cla460-fake-owner-token"
)

// prEnv builds a workdir whose repo has a github.com-shaped origin — the URL
// is only configuration, so no network is ever touched.
func prEnv(t *testing.T) string {
	t.Helper()
	return repoWithRemote(t, "https://github.com/acme/widgets.git")
}

// repoWithRemote is prEnv with the origin URL spelled out, for the SSH-alias
// case where the URL itself is the thing under test.
func repoWithRemote(t *testing.T, remote string) string {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	work := filepath.Join(root, "repo")
	run(t, root, "git", "init", "-b", "main", work)
	configure(t, work)
	run(t, work, "git", "remote", "add", "origin", remote)
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
// its call counter, so a test can assert how many reads happened. The
// credential probe never reaches the counter — see writeGHTo.
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
	// Every fake serves the credential probe prCheck makes before its first
	// API read (CLA-460), out of band and before anything else — so the probe
	// consumes neither a fixture nor, for fakeGHSeq, a sequence slot. With
	// GH_FAKE_TOKEN unset, or GH_FAKE_FAIL=1, the probe answers empty or
	// fails: what callers read as "no resolvable token", the unscoped
	// fallback's trigger. The probe RETURNS here, so recordingGH's own
	// recording block never sees it: record the whole argv it was asked with
	// here, which pins both the account it named and the flag shape it used.
	body = `case "$1/$2" in
auth/token)
	if [ -n "${GH_RECORD-}" ]; then
		printf '=== auth token\nargv=%s\n' "$*" >> "$GH_RECORD"
	fi
	if [ "$GH_FAKE_FAIL" = 1 ]; then
		echo "no oauth token found for github.com account $4" >&2
		exit 1
	fi
	printf '%s\n' "${GH_FAKE_TOKEN-}"
	exit 0
;;
esac
` + body
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// recordingGH writes a fake gh that appends one section per invocation to
// $GH_RECORD — the argv head plus the exact GH_TOKEN/GITHUB_TOKEN the child
// resolved — then answers every non-probe invocation with output. The
// credential probe is recorded by writeGHTo's own arm, because it exits
// before this body runs.
func recordingGH(t *testing.T, output string) string {
	t.Helper()
	body := `{
	printf '=== %s %s\n' "$1" "$2"
	if [ -n "${GH_TOKEN+x}" ]; then printf 'ghtoken=%s\n' "$GH_TOKEN"; else printf 'ghtoken=UNSET\n'; fi
	if [ -n "${GITHUB_TOKEN+x}" ]; then printf 'githubtoken=%s\n' "$GITHUB_TOKEN"; else printf 'githubtoken=UNSET\n'; fi
} >> "$GH_RECORD"
cat <<'J'
` + output + `
J
`
	return writeGH(t, body)
}

// ghRecord reads the raw $GH_RECORD the fake gh wrote.
func ghRecord(t *testing.T, record string) string {
	t.Helper()
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("gh record missing - the fake gh never ran: %v", err)
	}
	return string(body)
}

// ghSections splits a GH_RECORD into per-invocation sections keyed by the
// argv head the fake saw ("pr view", "auth token"), each holding the lines the
// shim wrote: the resolved token lines for an API read, the probe's argv for
// the credential probe. A repeated key keeps only the LAST invocation's lines,
// so a caller pinning how MANY times a command ran counts its headers in
// ghRecord instead (see wantProbeOwner).
func ghSections(t *testing.T, record string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	cur := ""
	for _, line := range strings.Split(ghRecord(t, record), "\n") {
		switch {
		case strings.HasPrefix(line, "=== "):
			cur = strings.TrimPrefix(line, "=== ")
			out[cur] = nil
		case cur != "" && line != "":
			out[cur] = append(out[cur], line)
		}
	}
	return out
}

// wantProbeOwner pins the credential probe: it ran exactly once and asked gh
// for owner's token at github.com. The header count comes from the raw record,
// because ghSections would hide a second probe behind its last-wins sections;
// the recorded argv is what proves the flag and the argument shape. `--hostname`
// is part of the pin because without it gh answers for its DEFAULT host.
func wantProbeOwner(t *testing.T, record, owner string) {
	t.Helper()
	if n := strings.Count(ghRecord(t, record), "=== auth token"); n != 1 {
		t.Errorf("the credential probe ran %d times; want exactly 1", n)
	}
	probe := ghSections(t, record)["auth token"]
	want := "argv=auth token --user " + owner + " --hostname " + githubHost
	if len(probe) != 1 || probe[0] != want {
		t.Errorf("credential probe asked %v; want exactly [%s]", probe, want)
	}
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
// to hundreds of milliseconds each), not just two polls. The credential probe
// does not count against it — see writeGHTo.
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

// --- whose credentials the API reads carry (CLA-460) -------------------------

// THE pin: when the remote owner's token resolves, the PR read goes out AS
// that owner — GH_TOKEN carries exactly the value the probe answered, and the
// ambient spellings are DROPPED outright rather than raced. Decoy ambient
// values are planted on purpose: the disease here is an ambient credential
// answering for the wrong account, so the pin has to show one losing.
//
// The probe's ACCOUNT is pinned too: the credential half above alone would
// stay green if the probe asked gh for the wrong owner.
func TestPRCheckScopesTheAPIReadToTheRemoteOwner(t *testing.T) {
	dir := prEnv(t)
	record := filepath.Join(t.TempDir(), "gh-record.log")
	t.Setenv("GH_RECORD", record)
	t.Setenv("GH_FAKE_TOKEN", ghFakeToken)
	t.Setenv("GH_TOKEN", "ambient-decoy-not-the-owners")
	t.Setenv("GITHUB_TOKEN", "ambient-github-decoy")
	gh := recordingGH(t, jsonMergeablePassing)

	v := prVerifier(t, dir, gh)
	rep := verifyWith(t, v, Claim{Label: "CLA-460", PR: "7"})
	mustStatus(t, rep, PRVerified, Pass)

	// prEnv's origin is https://github.com/acme/widgets.git, so acme is the
	// owner; "widgets" is the repo name that a plausible bug would substitute.
	wantProbeOwner(t, record, "acme")

	pr := ghSections(t, record)["pr view"]
	if len(pr) == 0 {
		t.Fatal("the PR read never ran - scoping cannot have ridden it")
	}
	for _, l := range pr {
		if l != "ghtoken="+ghFakeToken && l != "githubtoken=UNSET" {
			t.Errorf("PR read carried %q; want ghtoken=%s and GITHUB_TOKEN dropped", l, ghFakeToken)
		}
	}
}

// An scp-like SSH remote is scoped by the RESOLVED slug, not the raw URL:
// githubOwner only parses http(s), so deriving from the URL left every SSH
// remote unscoped as the ambient account (CLA-776 review 1.review-1.1). The
// close-out scopes by the slug (ghAPIScope); the gate must do the same.
func TestPRCheckScopesAnSSHRemoteByItsResolvedSlug(t *testing.T) {
	dir := repoWithRemote(t, "git@github-personal:acme/widgets.git")
	// ssh -G resolves the alias to github.com, as a developer's config does.
	sshDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sshDir, "ssh"),
		[]byte("#!/bin/sh\nprintf 'host %s\\nhostname github.com\\n' \"$3\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	record := filepath.Join(t.TempDir(), "gh-record.log")
	t.Setenv("GH_RECORD", record)
	t.Setenv("GH_FAKE_TOKEN", ghFakeToken)
	t.Setenv("GH_TOKEN", "ambient-decoy-not-the-owners")
	gh := recordingGH(t, jsonMergeablePassing)

	v := prVerifier(t, dir, gh)
	rep := verifyWith(t, v, Claim{Label: "CLA-460", PR: "7"})
	mustStatus(t, rep, PRVerified, Pass)

	// The owner comes from the RESOLVED slug here, not from a plain https URL:
	// the alias says nothing, so "acme" is reachable only through
	// githubSlugResolving. Asking for the alias or the repo name is the bug
	// this pins.
	wantProbeOwner(t, record, "acme")

	pr := ghSections(t, record)["pr view"]
	if len(pr) == 0 {
		t.Fatal("the PR read never ran - scoping cannot have ridden it")
	}
	for _, l := range pr {
		if l != "ghtoken="+ghFakeToken && l != "githubtoken=UNSET" {
			t.Errorf("SSH-remote PR read carried %q; want ghtoken=%s and GITHUB_TOKEN dropped", l, ghFakeToken)
		}
	}
}

// The fallback is today's behavior, byte for byte: an owner whose token does
// not resolve leaves the PR read UNscoped, and the ambient credentials such a
// read has always seen survive untouched — stripping them would be a second
// behavior change hiding inside the fix. The check itself must still run and
// still pass.
func TestPRCheckRunsUnscopedWhenOwnerTokenDoesNotResolve(t *testing.T) {
	dir := prEnv(t)
	record := filepath.Join(t.TempDir(), "gh-record.log")
	t.Setenv("GH_RECORD", record)
	// No GH_FAKE_TOKEN: the probe answers empty — the unresolvable-owner case.
	t.Setenv("GH_TOKEN", "ambient-decoy-stays")
	t.Setenv("GITHUB_TOKEN", "github-decoy-stays")
	gh := recordingGH(t, jsonMergeablePassing)

	v := prVerifier(t, dir, gh)
	rep := verifyWith(t, v, Claim{Label: "CLA-460", PR: "7"})
	mustStatus(t, rep, PRVerified, Pass)

	// The fallback is about the ANSWER, not the question: the probe still named
	// the URL's owner, it just resolved no token for it.
	wantProbeOwner(t, record, "acme")

	pr := ghSections(t, record)["pr view"]
	if len(pr) == 0 {
		t.Fatal("the PR read never ran")
	}
	for _, l := range pr {
		if l != "ghtoken=ambient-decoy-stays" && l != "githubtoken=github-decoy-stays" {
			t.Errorf("unscoped fallback must leave ambient credentials untouched, got %q", l)
		}
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

// CLA-776's second hole, end to end: an origin spelled through an SSH host
// alias that resolves to github.com must be CHECKED, not reported as "not a
// github.com repository" and skipped. The fake ssh answers `ssh -G`, and the
// fake gh records the --repo it was asked for, so the test pins that the
// resolved slug reached gh rather than merely that something ran.
func TestPRCheckChecksAnSSHHostAliasRemote(t *testing.T) {
	root := repoWithRemote(t, "git@github-personal:lecstor/clankerbar-cli.git")
	forceSSH(t, "github-personal", "github.com")

	record := filepath.Join(t.TempDir(), "gh-argv")
	gh := writeGH(t, "printf '%s\\n' \"$*\" > "+record+"\ncat <<'J'\n"+jsonMergeablePassing+"\nJ\n")

	v := prVerifier(t, root, gh)
	rep := verifyWith(t, v, Claim{Label: "CLA-776", PR: "7"})
	mustStatus(t, rep, PRVerified, Pass)

	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("fake gh was never asked to read the PR: %v", err)
	}
	if !strings.Contains(string(argv), "--repo lecstor/clankerbar-cli") {
		t.Errorf("gh was asked for the wrong repository: %s", argv)
	}
}

// ... and an alias that resolves somewhere else is still not GitHub, so the
// check degrades exactly as it did before rather than pretending to run.
func TestPRCheckRefusesAnSSHAliasThatIsNotGitHub(t *testing.T) {
	root := repoWithRemote(t, "git@gitlab-personal:lecstor/clankerbar-cli.git")
	forceSSH(t, "gitlab-personal", "gitlab.com")

	v := prVerifier(t, root, fakeGH(t, jsonMergeablePassing))
	rep := verifyWith(t, v, Claim{Label: "CLA-776", PR: "7"})
	c := mustStatus(t, rep, PRVerified, Unknown)
	// The candidate search found no GitHub repository, so its message is the
	// one that names the alias attempt — the important part is that a non-GitHub
	// alias is a could-not-verify, not a pass and not a crash.
	if !strings.Contains(c.Detail, "has a github.com remote") || !strings.Contains(c.Detail, "ssh -G") {
		t.Errorf("detail should name the missing github.com remote and the alias check: %s", c.Detail)
	}
}

// forceSSH puts a fake `ssh` first on PATH whose `ssh -G <alias>` reports the
// given hostname, the way an operator's ~/.ssh/config entry would.
func forceSSH(t *testing.T, alias, hostname string) {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "ssh")
	// The host arrives after `--` (it comes from a remote URL, so it must not
	// be readable as an option); refuse an argv without the marker.
	body := "#!/bin/sh\nif [ \"$1\" != \"-G\" ] || [ \"$2\" != \"--\" ]; then exit 255; fi\ncase \"$3\" in\n" +
		alias + ") printf 'host " + alias + "\\nhostname " + hostname + "\\nport 22\\n' ;;\n" +
		"*) printf 'host %s\\nhostname %s\\n' \"$3\" \"$3\" ;;\nesac\n"
	if err := os.WriteFile(fake, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
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
	// The direct grammar is deliberately blind to an SSH host alias: only the
	// resolving form knows how to ask ssh, and mixing the two would make this
	// pure function spawn processes.
	if got, ok := githubSlug("git@github-personal:lecstor/clankerbar-cli.git"); ok {
		t.Errorf("githubSlug accepted an unresolved SSH alias as %q — the alias must go through githubSlugResolving", got)
	}
}

// CLA-776's second hole: a checkout whose origin is an SSH host alias that
// resolves to github.com was reported as "not a github.com repository", so the
// PR check was skipped rather than run (CLA-728 PR 230 on the Mac). The alias
// resolves through `ssh -G`; anything that does not land on github.com is
// still rejected, and an https URL is never alias-resolved (an SSH alias says
// nothing about https).
func TestGithubSlugResolvesAnSSHHostAlias(t *testing.T) {
	resolve := func(host string) (string, bool) {
		switch host {
		case "github-personal", "github-work":
			return "github.com", true
		case "gitlab-personal":
			return "gitlab.com", true
		}
		return "", false
	}
	cases := []struct{ url, want string }{
		{"git@github-personal:lecstor/clankerbar-cli.git", "lecstor/clankerbar-cli"},
		{"ssh://git@github-personal/lecstor/clankerbar-cli.git", "lecstor/clankerbar-cli"},
		{"ssh://git@github-personal:22/lecstor/clankerbar-cli.git", "lecstor/clankerbar-cli"},
		{"git@github-work:acme/widgets", "acme/widgets"},
		{"git@gitlab-personal:lecstor/clankerbar-cli.git", ""},     // resolves, but not to github.com
		{"git@unknown-alias:lecstor/clankerbar-cli.git", ""},       // nothing to resolve
		{"https://gitlab.com/lecstor/clankerbar-cli.git", ""},      // https is never alias-resolved
		{"https://github-personal/lecstor/clankerbar-cli.git", ""}, // ... and neither is this one
		{"/Users/j/dev/clankerbar-cli", ""},                        // a local path is not a remote host
	}
	for _, tc := range cases {
		if got, _ := githubSlugResolving(tc.url, resolve); got != tc.want {
			t.Errorf("githubSlugResolving(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}

	// A direct github.com host never pays for a lookup: ssh must not be
	// consulted when the answer is already in the URL.
	if got, ok := githubSlugResolving("git@github.com:lecstor/clankerbar-cli.git", func(string) (string, bool) {
		t.Fatal("the resolver was consulted for a direct github.com host")
		return "", false
	}); !ok || got != "lecstor/clankerbar-cli" {
		t.Errorf("direct github.com remote resolved as (%q, %v)", got, ok)
	}
}

// sshAliasHost asks ssh's OWN effective configuration, which is the whole point:
// guessing from ~/.ssh/config would miss an Include or a Match block. The fake
// mirrors `ssh -G`'s shape (one `hostname` line among many) and refuses an argv
// without the end-of-options marker, so the marker is pinned: a host taken from
// a remote URL could otherwise begin with '-' and be read as an OPTION.
func TestSSHAliasHostReadsSSHConfig(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "ssh")
	if err := os.WriteFile(fake, []byte(`#!/bin/sh
if [ "$1" != "-G" ] || [ "$2" != "--" ]; then
  printf 'usage: ssh [-46AaCfGgKkMNnqsTtVvXxYyZ] [-b bind_address] ...\n' >&2
  exit 255
fi
case "$3" in
github-personal) printf 'host github-personal\nuser git\nhostname github.com\nport 22\n' ;;
gitlab-personal) printf 'host gitlab-personal\nhostname gitlab.com\n' ;;
*) printf 'host %s\nhostname %s\n' "$3" "$3" ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if got, ok := sshAliasHost(context.Background(), "github-personal"); !ok || got != "github.com" {
		t.Errorf("sshAliasHost(github-personal) = (%q, %v), want (github.com, true)", got, ok)
	}
	if got, ok := sshAliasHost(context.Background(), "gitlab-personal"); !ok || got != "gitlab.com" {
		t.Errorf("sshAliasHost(gitlab-personal) = (%q, %v), want (gitlab.com, true)", got, ok)
	}
	if got, ok := sshAliasHost(context.Background(), "not-configured"); !ok || got != "not-configured" {
		// ssh -G answers an unknown alias by echoing it as its own hostname.
		// The caller compares the answer with github.com, so identity is not a
		// resolution — pinning that here keeps the guard honest.
		t.Errorf("sshAliasHost(not-configured) = (%q, %v), want the alias echoed back", got, ok)
	}
	if _, ok := githubSlugResolving("git@not-configured:lecstor/x.git", func(host string) (string, bool) {
		return sshAliasHost(context.Background(), host)
	}); ok {
		t.Error("an alias ssh echoes back was accepted as github.com")
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

// --- environment: gh must not decorate the JSON the gate parses (CLA-776) ---

// THE regression: GH_FORCE_TTY went to gh as "0" from CLA-310 until CLA-776.
// gh reads ANY value as "force terminal-style output" (a number is a column
// width), so 0 turned colour ON and every real `gh pr view --json` was coloured
// JSON that encoding/json rejected — invisible for two months because the fake
// gh below never coloured anything.
//
// Mutation check: put GH_FORCE_TTY back, at any value, and this fails.
func TestGHChildEnvironmentCarriesNoColourSwitch(t *testing.T) {
	t.Setenv("GH_FORCE_TTY", "0") // the value the bug shipped, now ambient
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("FORCE_COLOR", "1")

	gh := writeGH(t, "env\n")
	out, err := runGHBin(context.Background(), gh, t.TempDir(), nil, false, "pr", "view")
	if err != nil {
		t.Fatalf("fake gh: %v", err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if i := strings.IndexByte(line, '='); i > 0 {
			got[line[:i]] = line[i+1:]
		}
	}
	for _, name := range []string{"GH_FORCE_TTY", "CLICOLOR_FORCE", "FORCE_COLOR"} {
		if v, ok := got[name]; ok {
			t.Errorf("%s reached gh as %q — any value of GH_FORCE_TTY forces terminal-style output, and the colour-forcing pair overrides NO_COLOR", name, v)
		}
	}
	if got["NO_COLOR"] != "1" {
		t.Errorf("NO_COLOR = %q, want 1: the child must be told not to colour output this package parses", got["NO_COLOR"])
	}
	if got["CLICOLOR"] != "0" {
		t.Errorf("CLICOLOR = %q, want 0: the same requirement, in the spelling older gh versions read", got["CLICOLOR"])
	}

	// Per-invocation additions ride this spawn too (GH_TOKEN, CLA-460). They are
	// held to the same ownership rule: an addition must not be able to turn
	// colour back on behind the parser's back.
	out, err = runGHBin(context.Background(), gh, t.TempDir(),
		[]string{"GH_FORCE_TTY=1", "NO_COLOR=0", "CLICOLOR=1"}, false, "pr", "view")
	if err != nil {
		t.Fatalf("fake gh: %v", err)
	}
	got = map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if i := strings.IndexByte(line, '='); i > 0 {
			got[line[:i]] = line[i+1:]
		}
	}
	if v, ok := got["GH_FORCE_TTY"]; ok {
		t.Errorf("an extraEnv GH_FORCE_TTY reached gh as %q — the additions must not override the owned colour switches", v)
	}
	if got["NO_COLOR"] != "1" || got["CLICOLOR"] != "0" {
		t.Errorf("extraEnv overrode the colour settings: NO_COLOR=%q CLICOLOR=%q, want 1 / 0", got["NO_COLOR"], got["CLICOLOR"])
	}
}

// colouredWhenForcedGH mirrors real gh: its JSON is ANSI-coloured exactly when
// GH_FORCE_TTY is in its environment. With the gate's environment fixed the
// variable never arrives, so the same fake answers plain JSON and the check
// passes — which is what proves the fix, not the fixture.
func colouredWhenForcedGH(t *testing.T) string {
	t.Helper()
	coloured := "\x1b[1;37m{\x1b[m\n" +
		"  \x1b[1;34m\"mergeable\"\x1b[m\x1b[1;37m:\x1b[m \"MERGEABLE\",\n" +
		"  \x1b[1;34m\"mergeStateStatus\"\x1b[m\x1b[1;37m:\x1b[m \"CLEAN\",\n" +
		"  \x1b[1;34m\"statusCheckRollup\"\x1b[m\x1b[1;37m:\x1b[m [\n" +
		"    \x1b[1;37m{\x1b[m\n" +
		"      \x1b[1;34m\"__typename\"\x1b[m\x1b[1;37m:\x1b[m \"CheckRun\",\n" +
		"      \x1b[1;34m\"name\"\x1b[m\x1b[1;37m:\x1b[m \"ci\",\n" +
		"      \x1b[1;34m\"status\"\x1b[m\x1b[1;37m:\x1b[m \"COMPLETED\",\n" +
		"      \x1b[1;34m\"conclusion\"\x1b[m\x1b[1;37m:\x1b[m \"SUCCESS\"\n" +
		"    \x1b[1;37m}\x1b[m\n" +
		"  ],\n" +
		"  \x1b[1;34m\"url\"\x1b[m\x1b[1;37m:\x1b[m \"https://github.com/acme/widgets/pull/7\"\n" +
		"\x1b[1;37m}\x1b[m\n"
	body := "if [ -n \"${GH_FORCE_TTY+x}\" ]; then\n" +
		"cat <<'COLOURED'\n" + coloured + "COLOURED\n" +
		"else\ncat <<'PLAIN'\n" + jsonMergeablePassing + "\nPLAIN\nfi\n"
	return writeGH(t, body)
}

func TestColourEmittingGHIsParsedUnderTheFixedEnvironment(t *testing.T) {
	t.Setenv("GH_FORCE_TTY", "0") // an ambient value, as the operator machine had
	dir := prEnv(t)
	gh := colouredWhenForcedGH(t)

	// Prove the fixture mirrors real gh first: handed the variable, it emits
	// bytes encoding/json rejects. Without this the Pass below could just mean
	// the fake never coloured anything.
	cmd := exec.Command(gh, "pr", "view", "7")
	cmd.Env = append(os.Environ(), "GH_FORCE_TTY=0")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("fake gh: %v", err)
	}
	if !bytes.Contains(raw, []byte("\x1b")) {
		t.Fatal("fixture did not colour its output under GH_FORCE_TTY — it does not mirror gh, so this test proves nothing")
	}
	var probe ghPR
	if err := json.Unmarshal(raw, &probe); err == nil {
		t.Fatal("coloured gh output parsed — the fixture is not faithful to the bug")
	}

	v := prVerifier(t, dir, gh)
	rep := verifyWith(t, v, Claim{Label: "CLA-776", PR: "7"})
	mustStatus(t, rep, PRVerified, Pass)
}

// If gh decorates its output anyway — a future colour-forcing variable this
// package cannot strip — the delivery must be REFUSED, not logged as "could
// not verify … carrying on". A parse failure is a finding: gh ran and its
// answer is unintelligible, which is a defect in this gate's own contract
// rather than an absence of evidence.
func TestUnparseableGHOutputIsRefusedNotCarriedOn(t *testing.T) {
	dir := prEnv(t)
	gh := fakeGH(t, "\x1b[1;37m{\x1b[m\n  \x1b[1;34m\"mergeable\"\x1b[m: \"MERGEABLE\"\n\x1b[1;37m}\x1b[m")

	v := prVerifier(t, dir, gh)
	rep := verifyWith(t, v, Claim{Label: "CLA-776", PR: "7"})
	c := mustStatus(t, rep, PRVerified, Fail)
	if !rep.Failed() {
		t.Error("a parse failure must count as a failed delivery check")
	}
	if !strings.Contains(c.Detail, "could not parse") {
		t.Errorf("refusal must name the parse failure: %s", c.Detail)
	}
	if !strings.Contains(c.Detail, "doctor") {
		t.Errorf("refusal must point at the preflight that catches this up front: %s", c.Detail)
	}
}

// --- doctor's probe (CLA-776) ------------------------------------------------

// ProbeGHJSON is what `clankerbar doctor` runs. It must read the pull request
// through the GATE'S command and decoder — a preflight that parses something
// else would stop proving the gate works — and it must discover the PR rather
// than require an open one.
func TestProbeGHJSONReadsARealPRViewAndParsesIt(t *testing.T) {
	root := prEnv(t)
	record := filepath.Join(t.TempDir(), "argv")
	gh := writeGH(t, `printf '%s\n' "$*" >> '`+record+`'
if [ "$1/$2" = "pr/list" ]; then
  printf '[{"number":7}]\n'
else
  cat <<'J'
`+jsonMergeablePassing+`
J
fi
`)

	probe, err := ProbeGHJSON(context.Background(), gh, root)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Slug != "acme/widgets" || probe.Number != 7 {
		t.Errorf("probe = %+v, want acme/widgets#7", probe)
	}

	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("fake gh never ran: %v", err)
	}
	lines := string(argv)
	if !strings.Contains(lines, "pr list --repo acme/widgets") {
		t.Errorf("the probe did not discover the PR through gh: %q", lines)
	}
	// THE coupling: the probe decodes the exact selection the gate decodes.
	if want := "pr view 7 --repo acme/widgets --json " + prViewJSONFields; !strings.Contains(lines, want) {
		t.Errorf("the probe did not run the gate's own read.\nwant: %s\ngot:\n%s", want, lines)
	}
}

// A multi-project config gives each project its own workdir, so the probe must
// search EVERY workdir doctor hands it (CLA-776 review 1.review-3.1): a PR in
// the second project's checkout still exercises the gate's parse path. Before
// this, only cfg.WorkDir was searched and doctor reported NOT exercised.
func TestProbeGHJSONDirsSearchesEveryConfiguredWorkdir(t *testing.T) {
	noPR := repoWithRemote(t, "https://github.com/acme/first.git")
	withPR := repoWithRemote(t, "https://github.com/acme/second.git")
	record := filepath.Join(t.TempDir(), "argv")
	gh := writeGH(t, `printf '%s\n' "$*" >> '`+record+`'
case "$1/$2" in
pr/list)
  case "$*" in
  *"--repo acme/second"*) printf '[{"number":9}]\n' ;;
  *) printf '[]\n' ;;
  esac
  ;;
*)
  cat <<'J'
`+jsonMergeablePassing+`
J
  ;;
esac
`)

	probe, err := ProbeGHJSONDirs(context.Background(), gh, []string{noPR, withPR})
	if err != nil {
		t.Fatalf("probe across two workdirs: %v", err)
	}
	if probe.Slug != "acme/second" || probe.Number != 9 {
		t.Errorf("probe = %+v, want acme/second#9", probe)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("fake gh never ran: %v", err)
	}
	if lines := strings.Count(string(argv), "pr list"); lines != 2 {
		t.Errorf("pr list ran %d times, want once per workdir (2): %q", lines, string(argv))
	}
}

// The gate's workdir and its declared checkout can name the same repository;
// the probe deduplicates by git common dir so that repository is read once.
func TestProbeGHJSONDirsDeduplicatesRepositories(t *testing.T) {
	root := prEnv(t)
	record := filepath.Join(t.TempDir(), "argv")
	gh := writeGH(t, `printf '%s\n' "$*" >> '`+record+`'
if [ "$1/$2" = "pr/list" ]; then
  printf '[{"number":7}]\n'
else
  cat <<'J'
`+jsonMergeablePassing+`
J
fi
`)

	probe, err := ProbeGHJSONDirs(context.Background(), gh, []string{root, filepath.Join(root, "repo")})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.Slug != "acme/widgets" || probe.Number != 7 {
		t.Errorf("probe = %+v, want acme/widgets#7", probe)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("fake gh never ran: %v", err)
	}
	if lines := strings.Count(string(argv), "pr list"); lines != 1 {
		t.Errorf("pr list ran %d times for one repository, want 1: %q", lines, string(argv))
	}
}

// probeMaxRepos bounds how many repositories the probe TRIES; comparing before
// incrementing is what makes the constant bound what its name says.
func TestProbeGHJSONDirsTriesAtMostProbeMaxRepos(t *testing.T) {
	var roots []string
	for i := 0; i <= probeMaxRepos; i++ {
		roots = append(roots, repoWithRemote(t, fmt.Sprintf("https://github.com/acme/r%d.git", i)))
	}
	record := filepath.Join(t.TempDir(), "argv")
	gh := writeGH(t, `printf '%s\n' "$*" >> '`+record+`'
if [ "$1/$2" = "pr/list" ]; then
  printf '[]\n'
else
  printf '{}\n'
fi
`)

	if _, err := ProbeGHJSONDirs(context.Background(), gh, roots); !errors.Is(err, ErrNoPRProbeTarget) {
		t.Fatalf("got %v, want ErrNoPRProbeTarget", err)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("fake gh never ran: %v", err)
	}
	if lines := strings.Count(string(argv), "pr list"); lines != probeMaxRepos {
		t.Errorf("pr list ran %d times, want probeMaxRepos=%d: %q", lines, probeMaxRepos, string(argv))
	}
}

// The probe is classified by doctor with IsGHParseError, so a decorated answer
// must carry that type: a plain format error would be read as an unavailable
// prerequisite (WARN) instead of a broken gate (FAIL).
func TestProbeGHJSONReportsDecoratedOutputAsAParseError(t *testing.T) {
	root := prEnv(t)
	gh := writeGH(t, `if [ "$1/$2" = "pr/list" ]; then
  printf '[{"number":7}]\n'
else
  printf '\033[1;37m{\033[m\n  \033[1;34m"mergeable"\033[m\033[1;37m:\033[m "MERGEABLE"\n\033[1;37m}\033[m\n'
fi
`)

	_, err := ProbeGHJSON(context.Background(), gh, root)
	if err == nil {
		t.Fatal("coloured `gh pr view --json` parsed — the probe would pass doctor while the gate is broken")
	}
	if !IsGHParseError(err) {
		t.Fatalf("got %v (%T), want a ghParseError so doctor FAILs rather than warns", err, err)
	}
}

// A repository with no pull request is not a broken gate: the probe reports a
// no-target sentinel so doctor can say the parse path went unexercised.
func TestProbeGHJSONWithNoPRIsNoTarget(t *testing.T) {
	root := prEnv(t)
	_, err := ProbeGHJSON(context.Background(), writeGH(t, "printf '[]\\n'\n"), root)
	if !errors.Is(err, ErrNoPRProbeTarget) {
		t.Fatalf("got %v, want ErrNoPRProbeTarget", err)
	}
}

// A non-GitHub repository is skipped entirely: its remote says nothing about
// whether gh's JSON parses for the repos the gate judges.
func TestProbeGHJSONSkipsANonGitHubRepository(t *testing.T) {
	root := repoWithRemote(t, "https://gitlab.com/acme/widgets.git")
	// A gh that would fail loudly if the probe had run it.
	_, err := ProbeGHJSON(context.Background(), writeGH(t, "exit 42\n"), root)
	if !errors.Is(err, ErrNoPRProbeTarget) {
		t.Fatalf("got %v, want ErrNoPRProbeTarget", err)
	}
	if !strings.Contains(err.Error(), "no github.com repository") {
		t.Errorf("no-target error should name what was missing: %v", err)
	}
}

// The probe inherits the gate's fixed environment, so an ambient GH_FORCE_TTY
// cannot make the preflight fail (or the gate unreadable). The fixture mirrors
// real gh: it colours exactly when the variable reaches it.
func TestProbeGHJSONUsesTheColourFreeEnvironment(t *testing.T) {
	t.Setenv("GH_FORCE_TTY", "0") // the value the bug shipped, now ambient
	root := prEnv(t)
	gh := writeGH(t, `if [ "$1/$2" = "pr/list" ]; then
  printf '[{"number":7}]\n'
  exit 0
fi
if [ -n "${GH_FORCE_TTY+x}" ]; then
  printf '\033[1;37m{\033[m\n'
  exit 0
fi
cat <<'J'
`+jsonMergeablePassing+`
J
`)

	// Prove the fixture is faithful before trusting the probe's Pass.
	cmd := exec.Command(gh, "pr", "view", "7")
	cmd.Env = append(os.Environ(), "GH_FORCE_TTY=0")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("fake gh: %v", err)
	}
	var probe ghPR
	if json.Unmarshal(raw, &probe) == nil {
		t.Fatal("fixture did not colour under GH_FORCE_TTY — it does not mirror gh")
	}

	got, err := ProbeGHJSON(context.Background(), gh, root)
	if err != nil {
		t.Fatalf("probe with an ambient GH_FORCE_TTY: %v", err)
	}
	if got.Number != 7 {
		t.Errorf("probe = %+v, want #7", got)
	}
}
