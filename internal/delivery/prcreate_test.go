package delivery

// prcreate_test.go — CLA-1035's runner-owned PR creation. The properties that
// matter are the ones a retry or a takeover turns on: an existing open PR is
// reused rather than duplicated, a create is retried exactly once, and a create
// whose answer was lost is adopted by re-reading the open PRs.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const prListOne = `[{"number":7,"baseRefName":"staging","headRefName":"clanker/x","headRefOid":"abc1234def5678"}]`

// prCreateGH is a fake gh for EnsurePR/CreatePR: `pr list` answers list, `pr
// create` counts its invocation and then succeeds or fails. Both record their
// argv and the resolved GH_TOKEN in $GH_RECORD.
type prCreateGH struct {
	list       string // stdout for `pr list`
	createFail bool
}

func (g prCreateGH) script() string {
	create := `echo "https://github.com/acme/widgets/pull/12"`
	if g.createFail {
		create = "echo 'HTTP 502: bad gateway' >&2\nexit 1"
	}
	return `printf '=== %s\n' "$*" >> "$GH_RECORD"
if [ -n "${GH_TOKEN+x}" ]; then printf 'ghtoken=%s\n' "$GH_TOKEN" >> "$GH_RECORD"; else printf 'ghtoken=UNSET\n' >> "$GH_RECORD"; fi
case "$1/$2" in
pr/list)
	cat <<'J'
` + g.list + `
J
;;
pr/create)
	echo x >> "$GH_COUNT"
	` + create + `
;;
*)
	echo "unexpected gh $*" >&2
	exit 9
;;
esac
`
}

// prCreateEnv wires a fake gh and returns the verifier, the record file and the
// create counter. The origin is a github.com URL, so the slug resolves without
// touching the network.
func prCreateEnv(t *testing.T, g prCreateGH) (*Verifier, string, string) {
	t.Helper()
	return prCreateEnvWith(t, g.script())
}

func prCreateEnvWith(t *testing.T, script string) (*Verifier, string, string) {
	t.Helper()
	t.Setenv("GH_FAKE_TOKEN", ghFakeToken)
	record := filepath.Join(t.TempDir(), "record")
	count := filepath.Join(t.TempDir(), "count")
	t.Setenv("GH_RECORD", record)
	t.Setenv("GH_COUNT", count)
	root := prEnv(t)
	v := New(root, "").WithGHBin(writeGH(t, script)).WithPRCreateBackoff(time.Millisecond)
	return v, record, count
}

// prSpecFor is the runner's spec: the task's repo slug, PR base and branch, and
// the title/body the runner assembles. prEnv puts the repository one level below
// the workdir.
func prSpecFor(v *Verifier) PRSpec {
	return PRSpec{
		Repo:  filepath.Join(v.workdir, "repo"),
		Slug:  "acme/widgets",
		Base:  "staging",
		Head:  "clanker/x",
		Title: "CLA-1035: runner-owned PR creation",
		Body:  "Task: CLA-1035\n",
	}
}

func recordLines(t *testing.T, path, needle string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, needle) {
			n++
		}
	}
	return n
}

func TestEnsurePRReusesTheOpenPRForTheBranch(t *testing.T) {
	v, record, _ := prCreateEnv(t, prCreateGH{list: prListOne})

	num, err := v.EnsurePR(context.Background(), prSpecFor(v))
	if err != nil {
		t.Fatalf("EnsurePR: %v", err)
	}
	if num != 7 {
		t.Errorf("num = %d, want the existing PR 7", num)
	}
	if n := recordLines(t, record, "=== pr list"); n != 1 {
		t.Errorf("pr list ran %d times, want once", n)
	}
	if n := recordLines(t, record, "=== pr create"); n != 0 {
		t.Errorf("pr create ran %d times over an existing open PR", n)
	}
}

// A PR for the branch against ANOTHER base is not the runner's PR: reusing it
// would hand over a task whose work targets the wrong branch.
func TestEnsurePRDoesNotReuseAPRAgainstAnotherBase(t *testing.T) {
	v, record, count := prCreateEnv(t, prCreateGH{list: `[{"number":7,"baseRefName":"main",` +
		`"headRefName":"clanker/x","headRefOid":"abc1234def5678"}]`})

	num, err := v.EnsurePR(context.Background(), prSpecFor(v))
	if err != nil {
		t.Fatalf("EnsurePR: %v", err)
	}
	if num != 12 {
		t.Errorf("num = %d, want the freshly created PR 12", num)
	}
	if n := recordLines(t, count, "x"); n != 1 {
		t.Errorf("pr create ran %d times, want once", n)
	}
	if n := recordLines(t, record, "=== pr create"); n != 1 {
		t.Errorf("pr create argv recorded %d times, want once", n)
	}
}

func TestEnsurePRCreatesWithTheRunnerSpecAndScopedCredentials(t *testing.T) {
	v, record, _ := prCreateEnv(t, prCreateGH{list: "[]"})

	num, err := v.EnsurePR(context.Background(), prSpecFor(v))
	if err != nil {
		t.Fatalf("EnsurePR: %v", err)
	}
	if num != 12 {
		t.Errorf("num = %d, want 12", num)
	}
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"=== pr create",
		"pr create --repo acme/widgets --base staging --head clanker/x",
		"--title CLA-1035: runner-owned PR creation",
		"--body Task: CLA-1035",
		"ghtoken=" + ghFakeToken,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("gh never received %q:\n%s", want, got)
		}
	}
}

func TestEnsurePRRetriesTheCreateOnceThenFailsClosed(t *testing.T) {
	v, _, count := prCreateEnv(t, prCreateGH{list: "[]", createFail: true})

	_, err := v.EnsurePR(context.Background(), prSpecFor(v))
	if err == nil {
		t.Fatal("EnsurePR succeeded over two failed creates")
	}
	if n := recordLines(t, count, "x"); n != 2 {
		t.Errorf("pr create ran %d times, want the first attempt plus the one retry", n)
	}
}

// A create whose response was lost may still have opened the PR: the retry
// re-reads the open PRs first, so it adopts that PR instead of opening a second.
func TestEnsurePRAdoptsAPRThatLandedDespiteAFailedCreate(t *testing.T) {
	script := `printf '=== %s %s\n' "$1" "$2" >> "$GH_RECORD"
case "$1/$2" in
pr/list)
	n=$(cat "$GH_LIST_COUNT" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$GH_LIST_COUNT"
	if [ "$n" -eq 1 ]; then echo '[]'; else cat <<'J'
` + prListOne + `
J
	fi
;;
pr/create)
	echo x >> "$GH_COUNT"
	echo 'HTTP 502' >&2
	exit 1
;;
esac
`
	t.Setenv("GH_LIST_COUNT", filepath.Join(t.TempDir(), "lists"))
	v, _, count := prCreateEnvWith(t, script)

	num, err := v.EnsurePR(context.Background(), prSpecFor(v))
	if err != nil {
		t.Fatalf("EnsurePR: %v", err)
	}
	if num != 7 {
		t.Errorf("num = %d, want the adopted PR 7", num)
	}
	if n := recordLines(t, count, "x"); n != 1 {
		t.Errorf("pr create ran %d times, want the first attempt only", n)
	}
}

// The slug is resolved from the working directory's origin when the caller does
// not name one, so the PR targets the repository the branch was pushed to.
func TestEnsurePRResolvesTheSlugFromTheOrigin(t *testing.T) {
	v, record, _ := prCreateEnv(t, prCreateGH{list: "[]"})
	spec := prSpecFor(v)
	spec.Slug = ""

	if _, err := v.EnsurePR(context.Background(), spec); err != nil {
		t.Fatalf("EnsurePR: %v", err)
	}
	if n := recordLines(t, record, "=== pr create"); n != 1 {
		t.Fatalf("pr create ran %d times, want once:\n%s", n, readFileString(t, record))
	}
	if !strings.Contains(readFileString(t, record), "--repo acme/widgets") {
		t.Errorf("the resolved origin slug never reached gh:\n%s", readFileString(t, record))
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func TestEnsurePRRefusesAnIncompleteSpec(t *testing.T) {
	v := New(t.TempDir(), "")
	cases := map[string]func(*PRSpec){
		"no repo":  func(s *PRSpec) { s.Repo = "" },
		"no base":  func(s *PRSpec) { s.Base = "" },
		"no head":  func(s *PRSpec) { s.Head = "" },
		"no title": func(s *PRSpec) { s.Title = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := prSpecFor(v)
			mutate(&spec)
			if _, err := v.EnsurePR(context.Background(), spec); err == nil {
				t.Fatal("EnsurePR accepted an incomplete spec")
			}
		})
	}
}

// The retry must not create blind when its re-read of the open PRs fails: the
// first create may have landed with its answer lost, and a blind second create
// would either duplicate the PR or be refused by GitHub, failing a hand-over
// whose PR is in fact open.
func TestEnsurePRFailsClosedWhenTheRetryReadFails(t *testing.T) {
	script := `printf '=== %s %s\n' "$1" "$2" >> "$GH_RECORD"
case "$1/$2" in
pr/list)
	n=$(cat "$GH_LIST_COUNT" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$GH_LIST_COUNT"
	if [ "$n" -eq 1 ]; then echo '[]'; else echo 'HTTP 502' >&2; exit 1; fi
	;;
pr/create)
	echo x >> "$GH_COUNT"
	echo 'HTTP 502' >&2
	exit 1
	;;
esac
`
	t.Setenv("GH_LIST_COUNT", filepath.Join(t.TempDir(), "lists"))
	v, _, count := prCreateEnvWith(t, script)

	_, err := v.EnsurePR(context.Background(), prSpecFor(v))
	if err == nil {
		t.Fatal("EnsurePR succeeded although the retry's open-PR read failed")
	}
	if !strings.Contains(err.Error(), "re-reading open pull requests") {
		t.Errorf("err = %v, want the failed re-read named", err)
	}
	if n := recordLines(t, count, "x"); n != 1 {
		t.Errorf("pr create ran %d times, want the first attempt only", n)
	}
}

// CreatePR is the one-shot form: it never reads the open PRs, so a caller that
// asked for a create gets exactly one.
func TestCreatePRCreatesWithoutListingOrReusing(t *testing.T) {
	v, record, count := prCreateEnv(t, prCreateGH{list: "[]"})
	spec := prSpecFor(v)
	spec.Slug = "" // resolved from the worktree's origin

	num, err := v.CreatePR(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if num != 12 {
		t.Errorf("num = %d, want 12", num)
	}
	if n := recordLines(t, count, "x"); n != 1 {
		t.Errorf("pr create ran %d times, want once", n)
	}
	if n := recordLines(t, record, "=== pr list"); n != 0 {
		t.Errorf("pr list ran %d times; CreatePR must not reuse", n)
	}
	got := readFileString(t, record)
	for _, want := range []string{
		"pr create --repo acme/widgets --base staging --head clanker/x",
		"--title CLA-1035: runner-owned PR creation",
		"--body Task: CLA-1035",
		"ghtoken=" + ghFakeToken,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("gh never received %q:\n%s", want, got)
		}
	}
}

func TestCreatePRRefusesAnIncompleteSpec(t *testing.T) {
	v := New(t.TempDir(), "")
	spec := prSpecFor(v)
	spec.Base = ""
	_, err := v.CreatePR(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "create PR:") {
		t.Fatalf("CreatePR(incomplete spec) = %v, want a create PR refusal", err)
	}
}

func TestParseCreatedPR(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want int
		ok   bool
	}{
		{"the URL alone", "https://github.com/acme/widgets/pull/123", 123, true},
		{"after gh's own progress lines", "Creating pull request for clanker/x into staging in acme/widgets\n\nhttps://github.com/acme/widgets/pull/9\n", 9, true},
		{"no URL", "something went sideways", 0, false},
		{"a zero number", "https://github.com/acme/widgets/pull/0", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, ok := parseCreatedPR(tc.out)
			if ok != tc.ok || n != tc.want {
				t.Errorf("parseCreatedPR(%q) = %d, %v; want %d, %v", tc.out, n, ok, tc.want, tc.ok)
			}
		})
	}
}
