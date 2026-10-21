package delivery

// prclose_test.go — CLA-758's PR surface. The judgement is the gate's own, so the
// tests here are about the three things that are NEW: finding the open PR for a
// branch, merging it as a merge commit (pinned to the revision that was judged,
// under the same scoped identity), and reading a conflict's paths without a
// worktree. MergedOnRemote's three-way answer is pinned against a real remote,
// because "the merge commit landed" is the claim the whole close-out rests on.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseMergeTreeConflicts(t *testing.T) {
	const tree = "65cf846cf9dbdffd5d1b721a76e52f421607c724"
	cases := []struct {
		name string
		out  string
		want []string
		err  bool
	}{
		{
			name: "two conflicted paths then git's messages",
			out:  tree + "\na.txt\nb.txt\n\nAuto-merging a.txt\nCONFLICT (content): Merge conflict in a.txt\n",
			want: []string{"a.txt", "b.txt"},
		},
		{
			name: "clean merge is the tree id alone",
			out:  tree + "\n",
		},
		{
			name: "not a merge-tree answer",
			out:  "fatal: not a valid object name\n",
			err:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMergeTreeConflicts(tc.out)
			if tc.err {
				if err == nil {
					t.Fatalf("parse %q: want an error, got %v", tc.out, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse %q: %v", tc.out, err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("paths = %v, want %v", got, tc.want)
			}
		})
	}
}

// conflictEnv builds a repo whose main and clanker/feature both rewrite a.txt and
// b.txt, so a merge of one into the other conflicts on BOTH.
func conflictEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.commit("main", "a.txt", "one\ntwo\nthree")
	e.commit("main", "b.txt", "x\ny")
	e.push("main")

	e.checkout("clanker/feature")
	e.commit("clanker/feature", "a.txt", "one\nTWO-feature\nthree")
	e.commit("clanker/feature", "b.txt", "x\nY-feature")
	e.push("clanker/feature")

	e.checkout("main")
	e.commit("main", "a.txt", "one\nTWO-base\nthree")
	e.commit("main", "b.txt", "x\nY-base")
	e.push("main")
	return e
}

func TestConflictPathsNamesEveryConflictedFile(t *testing.T) {
	e := conflictEnv(t)

	paths, err := New(e.work, "").ConflictPaths(context.Background(), e.work, "main", "clanker/feature")
	if err != nil {
		t.Fatalf("ConflictPaths: %v", err)
	}
	if len(paths) != 2 || paths[0] != "a.txt" || paths[1] != "b.txt" {
		t.Fatalf("paths = %v, want a.txt and b.txt", paths)
	}
}

func TestConflictPathsOnACleanPairNamesNothing(t *testing.T) {
	e := newEnv(t)
	e.commit("main", "base.txt", "base")
	e.push("main")
	e.checkout("clanker/feature")
	e.commit("clanker/feature", "work.txt", "work")

	paths, err := New(e.work, "").ConflictPaths(context.Background(), e.work, "main", "clanker/feature")
	if err != nil {
		t.Fatalf("ConflictPaths: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths = %v, want none for a clean merge", paths)
	}
}

func TestMergedOnRemoteAnswersAncestry(t *testing.T) {
	e := newEnv(t)
	e.commit("main", "base.txt", "base")
	e.push("main")

	e.checkout("clanker/merged")
	e.commit("clanker/merged", "work.txt", "work")
	e.push("clanker/merged")

	e.checkout("clanker/unmerged")
	e.commit("clanker/unmerged", "other.txt", "other")
	e.push("clanker/unmerged")
	unmerged := e.head()

	e.checkout("main")
	e.git("merge", "--no-ff", "-m", "merge feature", "clanker/merged")
	e.push("main")
	merged := e.head()

	// A second clone, as the close-out has: it has never seen the merge commit.
	clone := filepath.Join(e.root, "fleet")
	run(t, e.root, "git", "clone", e.remote, clone)
	v := New(clone, "")

	if c := v.MergedOnRemote(context.Background(), clone, "origin", "main", merged); c.Status != Pass {
		t.Errorf("the merge commit is on main but the check said %s: %s", c.Status, c.Detail)
	}
	if c := v.MergedOnRemote(context.Background(), clone, "origin", "main", unmerged); c.Status != Fail {
		t.Errorf("an unmerged commit should Fail, got %s: %s", c.Status, c.Detail)
	}
	if c := v.MergedOnRemote(context.Background(), clone, "origin", "main", "main"); c.Status != Unknown {
		t.Errorf("a revision expression is not a delivery and must be Unknown, got %s: %s", c.Status, c.Detail)
	}
	if c := v.MergedOnRemote(context.Background(), clone, "origin", "main", "0123456789abcdef0123456789abcdef01234567"); c.Status == Pass {
		t.Errorf("a commit nobody can fetch answered Pass; it must never assume: %s", c.Detail)
	}
}

func TestOpenPRsListsTheBranchsOpenPR(t *testing.T) {
	root := prEnv(t)
	repo := filepath.Join(root, "repo")
	const listed = `[{"number":7,"url":"https://github.com/acme/widgets/pull/7",` +
		`"baseRefName":"staging","headRefName":"clanker/x","headRefOid":"abc1234def5678"}]`
	v := New(root, "").WithGHBin(fakeGH(t, listed))

	prs, err := v.OpenPRs(context.Background(), repo, "acme/widgets", "clanker/x")
	if err != nil {
		t.Fatalf("OpenPRs: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("prs = %v, want one", prs)
	}
	if prs[0].Number != 7 || prs[0].Base != "staging" || prs[0].Head != "clanker/x" || prs[0].HeadOID != "abc1234def5678" {
		t.Errorf("pr = %+v, want #7 on staging from clanker/x at abc1234def5678", prs[0])
	}
}

func TestOpenPRsNamesNoBranchOrSlug(t *testing.T) {
	v := New(t.TempDir(), "")
	if _, err := v.OpenPRs(context.Background(), t.TempDir(), "", "b"); err == nil {
		t.Error("OpenPRs with no slug should refuse")
	}
	if _, err := v.OpenPRs(context.Background(), t.TempDir(), "acme/widgets", ""); err == nil {
		t.Error("OpenPRs with no head should refuse")
	}
}

func TestJudgePRMapsTheDeliveryGate(t *testing.T) {
	root := prEnv(t)
	repo := filepath.Join(root, "repo")
	cases := []struct {
		name string
		json string
		want PRVerdict
		says string
	}{
		{"mergeable and green", jsonMergeablePassing, PRReady, ""},
		{"conflicting", jsonConflicting, PRConflict, ""},
		{"empty rollup", jsonEmptyRollup, PRRefused, "NO checks"},
		{"failing check", jsonFailing, PRRefused, "FAILING checks"},
		{"no base branch", `{"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","baseRefName":"","headRefName":"clanker/x",` +
			`"headRefOid":"abc1234def5678","statusCheckRollup":[{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"}]}`,
			PRRefused, "without a base branch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := New(root, "").WithGHBin(fakeGH(t, tc.json)).WithPollWindow(120*time.Millisecond, 20*time.Millisecond)
			state := v.JudgePR(context.Background(), repo, "acme/widgets", 7)
			if state.Verdict != tc.want {
				t.Fatalf("verdict = %s, want %s (%s)", state.Verdict, tc.want, state.Check.Detail)
			}
			if state.Verdict == PRReady && state.Base == "" {
				t.Fatal("a pass without a base is exactly what must never be mergeable")
			}
			if tc.says != "" && !strings.Contains(state.Check.Detail, tc.says) {
				t.Errorf("detail does not say %q: %s", tc.says, state.Check.Detail)
			}
		})
	}
}

func TestJudgePRWithNoGHIsARefusalNotAPass(t *testing.T) {
	root := prEnv(t)
	v := New(root, "").WithGHBin(filepath.Join(t.TempDir(), "no-such-gh"))

	state := v.JudgePR(context.Background(), filepath.Join(root, "repo"), "acme/widgets", 7)

	if state.Verdict != PRRefused {
		t.Fatalf("verdict = %s, want %s", state.Verdict, PRRefused)
	}
	if !strings.Contains(state.Check.Detail, "PATH") {
		t.Errorf("detail should name the missing gh: %s", state.Check.Detail)
	}
}

func TestMergePRMergesAsAMergeCommitPinnedToTheJudgedHeadAndBase(t *testing.T) {
	root := prEnv(t)
	repo := filepath.Join(root, "repo")
	t.Setenv("GH_FAKE_TOKEN", ghFakeToken)
	argsFile := filepath.Join(t.TempDir(), "args")
	const mergeSHA = "38e217491234567890abcdef1234567890abcdef"
	script := "printf '%s\\n' \"$*\" >> " + argsFile + "\n" +
		"printf 'token=%s\\n' \"${GH_TOKEN-}\" >> " + argsFile + "\n" +
		"cat <<'J'\n{\"merged\":true,\"sha\":\"" + mergeSHA + "\",\"message\":\"merged\"}\nJ\n"
	v := New(root, "").WithGHBin(writeGHTo(t, filepath.Join(t.TempDir(), "gh"), script))

	sha, err := v.MergePR(context.Background(), repo, "acme/widgets", 7, "staging", "abc1234def5678")
	if err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	if sha != mergeSHA {
		t.Errorf("sha = %q, want %q", sha, mergeSHA)
	}
	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read the recorded invocation: %v", err)
	}
	got := string(recorded)
	// The base is a REQUEST parameter, not the PR's own property: that is what a
	// retarget between the judgement and the merge cannot redirect.
	for _, want := range []string{
		"api --method POST repos/acme/widgets/merges",
		"base=staging",
		"head=abc1234def5678",
		"token=" + ghFakeToken,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the merge invocation does not carry %q:\n%s", want, got)
		}
	}
	// The pull-request merge endpoint follows the PR's current base and pins no
	// base at all; a close-out must never use it.
	for _, forbidden := range []string{"/pulls/7/merge", "merge_method="} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the merge went through the PR endpoint (%q), which follows a retargetable base:\n%s", forbidden, got)
		}
	}
}

func TestMergePRRefusesWithoutAJudgedHeadOrABase(t *testing.T) {
	root := prEnv(t)
	repo := filepath.Join(root, "repo")
	// Any invocation at all is a failure: the guard must run before gh is spawned.
	argsFile := filepath.Join(t.TempDir(), "args")
	script := "printf '%s\\n' \"$*\" >> " + argsFile + "\nexit 0"
	v := New(root, "").WithGHBin(writeGHTo(t, filepath.Join(t.TempDir(), "gh"), script))

	if _, err := v.MergePR(context.Background(), repo, "acme/widgets", 7, "staging", ""); err == nil {
		t.Error("a merge with no judged head oid must be refused: dropping the pin is the hole it closes")
	}
	if _, err := v.MergePR(context.Background(), repo, "acme/widgets", 7, "", "abc1234def5678"); err == nil {
		t.Error("a merge with no base branch must be refused: the target would be the PR's own, retargetable base")
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Error("gh was invoked for a merge that had no pin or no base")
	}
}

func TestMergePRRefusesWhenGitHubDidNotMerge(t *testing.T) {
	root := prEnv(t)
	v := New(root, "").WithGHBin(fakeGH(t, `{"merged":false,"message":"Pull Request is not mergeable"}`))

	_, err := v.MergePR(context.Background(), filepath.Join(root, "repo"), "acme/widgets", 7, "staging", "abc1234def5678")
	if err == nil {
		t.Fatal("a merge GitHub did not make must be an error")
	}
	if !strings.Contains(err.Error(), "not mergeable") {
		t.Errorf("error does not carry GitHub's reason: %v", err)
	}
}

func TestMergePRRefusesAnAlreadyMergedHead(t *testing.T) {
	root := prEnv(t)
	// The merges endpoint answers 204 with an empty body when the base already
	// contains the head: there is no merge commit to name, so it is not a delivery.
	v := New(root, "").WithGHBin(writeGHTo(t, filepath.Join(t.TempDir(), "gh"), "exit 0"))

	_, err := v.MergePR(context.Background(), filepath.Join(root, "repo"), "acme/widgets", 7, "staging", "abc1234def5678")
	if err == nil {
		t.Fatal("a merge that produced no new commit must be an error")
	}
	if !strings.Contains(err.Error(), "already merged") {
		t.Errorf("error does not say the head was already merged: %v", err)
	}
}

func TestIsolatedCloneReadsTheRealRemoteAndTouchesNothingShared(t *testing.T) {
	e := newEnv(t)
	e.commit("main", "base.txt", "base")
	e.push("main")
	clone := filepath.Join(e.root, "fleet")
	run(t, e.root, "git", "clone", e.remote, clone)

	// A commit that only the remote has: the isolated clone must see it after a
	// fetch, while the fleet clone's own refs stay exactly as they were.
	e.checkout("main")
	e.commit("main", "later.txt", "later")
	e.push("main")
	later := e.head()
	before := strings.TrimSpace(run(t, clone, "git", "rev-parse", "refs/remotes/origin/main"))

	v := New(clone, "")
	iso, cleanup, err := v.IsolatedClone(context.Background(), clone, "origin")
	if err != nil {
		t.Fatalf("IsolatedClone: %v", err)
	}
	defer cleanup()
	if iso == clone {
		t.Fatal("the isolated clone is the shared checkout")
	}
	if err := v.FetchRef(context.Background(), iso, "origin", "main"); err != nil {
		t.Fatalf("fetch in the isolated clone: %v", err)
	}
	if got := strings.TrimSpace(run(t, iso, "git", "rev-parse", "refs/remotes/origin/main")); got != later {
		t.Errorf("isolated origin/main = %s, want the remote's %s", got, later)
	}
	if after := strings.TrimSpace(run(t, clone, "git", "rev-parse", "refs/remotes/origin/main")); after != before {
		t.Errorf("the shared checkout's origin/main moved (%s -> %s): the close-out wrote into a session's refs", before, after)
	}
}

func TestFetchRefUpdatesTheRemoteTrackingRef(t *testing.T) {
	e := newEnv(t)
	e.commit("main", "base.txt", "base")
	e.push("main")
	clone := filepath.Join(e.root, "fleet")
	run(t, e.root, "git", "clone", e.remote, clone)

	e.checkout("main")
	e.commit("main", "later.txt", "later")
	e.push("main")

	v := New(clone, "")
	if err := v.FetchRef(context.Background(), clone, "origin", "main"); err != nil {
		t.Fatalf("FetchRef: %v", err)
	}
	tip, err := v.RemoteTip(context.Background(), clone, "origin", "main")
	if err != nil {
		t.Fatalf("RemoteTip: %v", err)
	}
	if got := strings.TrimSpace(run(t, clone, "git", "rev-parse", "refs/remotes/origin/main")); got != tip {
		t.Errorf("origin/main = %s, want the fetched remote tip %s", got, tip)
	}
	if missing, err := v.RemoteTip(context.Background(), clone, "origin", "no-such-branch"); err != nil || missing != "" {
		t.Errorf("a missing branch should read as empty, got %q / %v", missing, err)
	}
}
