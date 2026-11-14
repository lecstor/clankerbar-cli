package loop

// CLA-870: `clankerbar review run`. The tests drive the REAL git plumbing —
// revisions, the temporary worktree, the maps builder — with a stub harness, so
// the things that can go wrong quietly are the things asserted: the worktree is
// outside the reviewed repository and always removed, the report carries the
// resolved SHAs and the run's real spend, and the maps method's notes and stage
// models reach the session and the JSON.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

type reviewRunTestRepo struct {
	t   *testing.T
	dir string
}

func newReviewRunRepo(t *testing.T) *reviewRunTestRepo {
	t.Helper()
	r := &reviewRunTestRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q")
	r.git("config", "user.email", "review-run@example.com")
	r.git("config", "user.name", "review run test")
	r.git("config", "commit.gpgsign", "false")
	return r
}

func (r *reviewRunTestRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *reviewRunTestRepo) write(name, content string) {
	r.t.Helper()
	path := filepath.Join(r.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *reviewRunTestRepo) commit(message string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "-m", message)
	return r.git("rev-parse", "HEAD")
}

// reviewRunDone builds the message the review brief asks for, with reviewedTip
// set to the commit the report must name.
func reviewRunDone(tip string, findings string) string {
	body := fmt.Sprintf(`{"verdict":"findings","reviewedTip":%q,"findings":[%s],"omitted":2}`, tip, findings)
	return "The review is complete.\n\n" + config.DoneMarker + "\n" + body
}

const reviewRunMustFinding = `{"severity":"must","summary":"The guard is missing","evidenceKind":"gate","evidence":"go test ./... fails","file":"main.go","line":3,"kind":"bug","requiredFix":"Restore the guard"}`

// TestRunReviewSinglePassReport drives one standalone review: the JSON shape,
// the resolved SHAs, the temp worktree (outside the repository, removed after),
// and the report check the adapter re-emits against.
func TestRunReviewSinglePassReport(t *testing.T) {
	r := newReviewRunRepo(t)
	r.write("README.md", "base\n")
	base := r.commit("base")
	r.write("README.md", "head\n")
	head := r.commit("head")
	beforeStatus := r.git("status", "--porcelain")
	beforeBranch := r.git("rev-parse", "--abbrev-ref", "HEAD")

	var got harness.Invocation
	opts := ReviewRunOptions{
		Repo:    r.dir,
		Base:    base[:7], // a short SHA must be reported resolved
		Head:    "",       // defaults to HEAD
		Method:  "single",
		Harness: "opencode",
		Model:   "test/review-model",
		Title:   "Fix the guards",
		cfg:     &config.Config{},
		invoke: func(_ context.Context, inv harness.Invocation) (harness.Result, error) {
			got = inv
			return harness.Result{
				FinalMessage:  reviewRunDone(head, reviewRunMustFinding),
				Usage:         harness.TokenUsage{Input: 100, Output: 20, Reasoning: 5, CacheRead: 30, CacheWrite: 7},
				CostUSD:       0.42,
				CostReported:  true,
				UsageReported: true,
			}, nil
		},
	}

	report, err := RunReview(context.Background(), opts)
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if report.Base != base || report.Head != head {
		t.Errorf("report commits = %s..%s, want the resolved %s..%s", report.Base, report.Head, base, head)
	}
	if report.Method != config.ReviewMethodSinglePass {
		t.Errorf("method = %q, want %q", report.Method, config.ReviewMethodSinglePass)
	}
	if len(report.Models) != 1 || report.Models["review"] != "test/review-model" {
		t.Errorf("models = %v, want {\"review\": \"test/review-model\"}", report.Models)
	}
	if len(report.Findings) != 1 || report.Findings[0].Severity != "must" || report.Findings[0].RequiredFix != "Restore the guard" {
		t.Errorf("findings = %+v, want the one MUST the session reported", report.Findings)
	}
	if report.Omitted != 2 {
		t.Errorf("omitted = %d, want 2", report.Omitted)
	}
	if report.NotesTurns == nil || len(report.NotesTurns) != 0 {
		t.Errorf("notes_turns = %#v, want an empty non-nil list for single-pass", report.NotesTurns)
	}
	wantUsage := ReviewRunUsage{Input: 100, Output: 20, Reasoning: 5, CacheRead: 30, CacheWrite: 7, CostUSD: 0.42}
	if report.Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", report.Usage, wantUsage)
	}
	if report.Turns != 1 {
		t.Errorf("turns = %d, want 1", report.Turns)
	}
	if report.TimeMS < 0 {
		t.Errorf("time_ms = %d, want a non-negative elapsed time", report.TimeMS)
	}

	if got.WorkDir == "" {
		t.Fatal("the invocation ran with no workdir")
	}
	if strings.HasPrefix(got.WorkDir, r.dir) {
		t.Errorf("the session ran inside the reviewed repository (%s); it must run in a temporary worktree", got.WorkDir)
	}
	if _, err := os.Stat(got.WorkDir); !os.IsNotExist(err) {
		t.Errorf("the temporary worktree still exists after a successful run: %s", got.WorkDir)
	}
	if got.MCPConfigPath != "" {
		t.Errorf("MCPConfigPath = %q, want empty: a standalone review must not carry the plane's MCP config", got.MCPConfigPath)
	}
	if !got.AllowUnclaimed {
		t.Error("AllowUnclaimed = false: a standalone review holds no claim and the adapter would refuse its turns")
	}
	if got.CheckReport == nil {
		t.Fatal("CheckReport is nil: a bad report would reach the JSON as if it were valid")
	}
	if err := got.CheckReport(reviewRunDone(head, reviewRunMustFinding)); err != nil {
		t.Errorf("CheckReport refused the valid report: %v", err)
	}
	if err := got.CheckReport("no report here"); err == nil {
		t.Error("CheckReport accepted a message with no done report")
	}
	for _, want := range []string{base, head, "Fix the guards", "git diff " + base + ".." + head} {
		if !strings.Contains(got.Prompt, want) {
			t.Errorf("the brief does not carry %q:\n%s", want, got.Prompt)
		}
	}

	if after := r.git("status", "--porcelain"); after != beforeStatus {
		t.Errorf("the reviewed repository's working tree changed: %q -> %q", beforeStatus, after)
	}
	if after := r.git("rev-parse", "--abbrev-ref", "HEAD"); after != beforeBranch {
		t.Errorf("the reviewed repository switched from %s to %s", beforeBranch, after)
	}
	if worktrees := r.git("worktree", "list", "--porcelain"); strings.Count(worktrees, "worktree ") != 1 {
		t.Errorf("the repository carries worktree metadata after the run:\n%s", worktrees)
	}
}

// TestRunReviewMapsSessionTurnsAndModels pins the maps method's wiring: the
// plan's turns reach the invocation, the notes are the notes turns' messages
// (never the findings), the JSON names both stage models, and the parsed report
// is the findings turn's.
func TestRunReviewMapsSessionTurnsAndModels(t *testing.T) {
	r := newReviewRunRepo(t)
	r.write("sample.go", "package sample\n\nfunc Alpha(x int) int {\n\treturn x + 1\n}\n\nfunc Beta(x int) int {\n\treturn x + 2\n}\n")
	base := r.commit("base")
	r.write("sample.go", "package sample\n\nfunc Alpha(x int) int {\n\treturn x + 10\n}\n\nfunc Beta(x int) int {\n\treturn x + 20\n}\n")
	head := r.commit("head")

	var got harness.Invocation
	opts := ReviewRunOptions{
		Repo:          r.dir,
		Base:          base,
		Head:          head,
		Method:        config.ReviewMethodMapsSession,
		Harness:       "opencode",
		Model:         "test/phase-model",
		NotesModel:    "test/notes-model",
		FindingsModel: "test/findings-model",
		ChunkSize:     1,
		cfg:           &config.Config{},
		invoke: func(_ context.Context, inv harness.Invocation) (harness.Result, error) {
			got = inv
			// Every planned turn ran: one message per turn, the last the report.
			messages := make([]string, 0, len(inv.Turns)+1)
			for i := 0; i < len(inv.Turns); i++ {
				messages = append(messages, fmt.Sprintf("notes turn %d", i+1))
			}
			messages = append(messages, reviewRunDone(head, reviewRunMustFinding))
			return harness.Result{TurnMessages: messages, FinalMessage: messages[len(messages)-1]}, nil
		},
	}

	report, err := RunReview(context.Background(), opts)
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if len(got.Turns) == 0 {
		t.Fatal("the maps-session invocation carries no follow-up turns")
	}
	if got.Console != nil {
		t.Errorf("Console = %v, want nil in a test that did not ask for one", got.Console)
	}
	if got.Turns[len(got.Turns)-1].Model != "test/findings-model" {
		t.Errorf("the findings turn runs on %q, want test/findings-model", got.Turns[len(got.Turns)-1].Model)
	}
	if len(report.Findings) != 1 || report.Findings[0].Severity != "must" {
		t.Errorf("findings = %+v, want the findings turn's one MUST", report.Findings)
	}
	// The plan's notes turns are every turn but the last; notesTurns equals
	// len(Turns) because the last follow-up turn is the findings turn.
	wantNotes := len(got.Turns)
	if len(report.NotesTurns) != wantNotes {
		t.Fatalf("notes_turns = %d, want %d (the messages before the findings turn): %#v",
			len(report.NotesTurns), wantNotes, report.NotesTurns)
	}
	for i, note := range report.NotesTurns {
		if want := fmt.Sprintf("notes turn %d", i+1); note != want {
			t.Errorf("notes_turns[%d] = %q, want %q", i, note, want)
		}
	}
	if report.Turns != 1+len(got.Turns) {
		t.Errorf("turns = %d, want %d (every turn that ran)", report.Turns, 1+len(got.Turns))
	}
	if report.Models["notes"] != "test/notes-model" || report.Models["findings"] != "test/findings-model" {
		t.Errorf("models = %v, want the two stage models", report.Models)
	}
	if len(report.Models) != 2 {
		t.Errorf("models = %v, want exactly notes and findings for the maps method", report.Models)
	}
	if !strings.Contains(got.Prompt, "M1.") {
		t.Errorf("the maps section did not reach the first turn:\n%s", got.Prompt)
	}
}

// TestRunReviewCleanupOnFailure proves the temporary worktree is removed when
// the session fails, and that the report the run did produce is still returned
// beside the error.
func TestRunReviewCleanupOnFailure(t *testing.T) {
	r := newReviewRunRepo(t)
	r.write("README.md", "base\n")
	base := r.commit("base")
	r.write("README.md", "head\n")
	head := r.commit("head")

	var worktree string
	removed := false
	opts := ReviewRunOptions{
		Repo:    r.dir,
		Base:    base,
		Head:    head,
		Harness: "opencode",
		cfg:     &config.Config{},
		invoke: func(_ context.Context, inv harness.Invocation) (harness.Result, error) {
			worktree = inv.WorkDir
			return harness.Result{ExitCode: 1}, errors.New("the harness died")
		},
		removeWorktree: func(repo, path string) error {
			removed = true
			if _, err := os.Stat(path); err != nil {
				t.Errorf("cleanup ran with the worktree already gone: %v", err)
			}
			return removeReviewWorktree(repo, path)
		},
	}

	report, err := RunReview(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "the harness died") {
		t.Fatalf("RunReview error = %v, want the harness failure", err)
	}
	if report == nil {
		t.Fatal("RunReview returned no report beside the error; the JSON the run produced would be lost")
	}
	if report.Base != base || report.Head != head {
		t.Errorf("report commits = %s..%s, want %s..%s", report.Base, report.Head, base, head)
	}
	if !removed {
		t.Error("the cleanup seam was never called after a failed session")
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Errorf("the temporary worktree survived a failed run: %s", worktree)
	}
	if worktrees := r.git("worktree", "list", "--porcelain"); strings.Count(worktrees, "worktree ") != 1 {
		t.Errorf("the repository carries worktree metadata after a failed run:\n%s", worktrees)
	}
}

// TestRunReviewUnparseableReportFails is the done-when's exit contract: a
// session that ends with no usable report fails the command, after the one
// re-emit the adapter performs. The JSON still carries what ran.
func TestRunReviewUnparseableReportFails(t *testing.T) {
	r := newReviewRunRepo(t)
	r.write("README.md", "base\n")
	base := r.commit("base")
	r.write("README.md", "head\n")
	head := r.commit("head")

	opts := ReviewRunOptions{
		Repo:    r.dir,
		Base:    base,
		Head:    head,
		Harness: "opencode",
		cfg:     &config.Config{},
		invoke: func(context.Context, harness.Invocation) (harness.Result, error) {
			return harness.Result{FinalMessage: "I reviewed it and it looks fine."}, nil
		},
	}
	report, err := RunReview(context.Background(), opts)
	if err == nil {
		t.Fatal("RunReview accepted a session with no done report")
	}
	if !strings.Contains(err.Error(), "without a usable done report") {
		t.Errorf("error = %v, want it to name the missing done report", err)
	}
	if report == nil {
		t.Fatal("RunReview returned no report beside the parse error")
	}
	if report.Findings == nil || len(report.Findings) != 0 {
		t.Errorf("findings = %#v, want an empty non-nil list", report.Findings)
	}
	if report.Usage != (ReviewRunUsage{}) {
		t.Errorf("usage = %+v, want zero for a session that reported none", report.Usage)
	}
}

// TestRunReviewResolvesRevisionsAndRefusesBadInput covers the failures that must
// happen before a paid session: an unknown method, an unknown revision, and a
// harness that cannot run the maps method's turns.
func TestRunReviewResolvesRevisionsAndRefusesBadInput(t *testing.T) {
	r := newReviewRunRepo(t)
	r.write("README.md", "base\n")
	base := r.commit("base")
	r.write("README.md", "head\n")
	head := r.commit("head")

	var calls int
	stub := func(context.Context, harness.Invocation) (harness.Result, error) {
		calls++
		return harness.Result{FinalMessage: reviewRunDone(head, reviewRunMustFinding)}, nil
	}
	baseOpts := func() ReviewRunOptions {
		return ReviewRunOptions{Repo: r.dir, Base: base, Head: head, Harness: "opencode", cfg: &config.Config{}, invoke: stub}
	}

	t.Run("unknown method", func(t *testing.T) {
		o := baseOpts()
		o.Method = "map-session"
		if _, err := RunReview(context.Background(), o); err == nil || !strings.Contains(err.Error(), "unknown review method") {
			t.Fatalf("error = %v, want the unknown method", err)
		}
	})
	t.Run("unknown revision", func(t *testing.T) {
		o := baseOpts()
		o.Base = "0000000000000000000000000000000000000000"
		if _, err := RunReview(context.Background(), o); err == nil || !strings.Contains(err.Error(), "--base") {
			t.Fatalf("error = %v, want it to name --base", err)
		}
	})
	t.Run("missing base", func(t *testing.T) {
		o := baseOpts()
		o.Base = ""
		if _, err := RunReview(context.Background(), o); err == nil || !strings.Contains(err.Error(), "--base is required") {
			t.Fatalf("error = %v, want the required base", err)
		}
	})
	t.Run("maps on a harness without turns", func(t *testing.T) {
		o := baseOpts()
		o.Method = config.ReviewMethodMapsSession
		o.Harness = "claude"
		if _, err := RunReview(context.Background(), o); err == nil || !strings.Contains(err.Error(), "cannot continue a session") {
			t.Fatalf("error = %v, want the harness refusal", err)
		}
	})
	if calls != 0 {
		t.Errorf("the stub harness ran %d times on refusals, want 0", calls)
	}
	if worktrees := r.git("worktree", "list", "--porcelain"); strings.Count(worktrees, "worktree ") != 1 {
		t.Errorf("a refused run left worktree metadata:\n%s", worktrees)
	}
}

func TestReviewRunMethod(t *testing.T) {
	for in, want := range map[string]string{
		"":               config.ReviewMethodSinglePass,
		"single":         config.ReviewMethodSinglePass,
		"single-pass":    config.ReviewMethodSinglePass,
		" maps-session ": config.ReviewMethodMapsSession,
	} {
		got, err := NormalizeReviewMethod(in)
		if err != nil {
			t.Errorf("NormalizeReviewMethod(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeReviewMethod(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := NormalizeReviewMethod("panel"); err == nil {
		t.Error("reviewRunMethod accepted an unknown method")
	}
}

// TestReviewRunReportJSONShape pins the wire shape the eval harness reads:
// findings and notes_turns are always arrays (never null), and every field the
// done-when names is present.
func TestReviewRunReportJSONShape(t *testing.T) {
	report := ReviewRunReport{
		Base:       "b",
		Head:       "h",
		Method:     config.ReviewMethodSinglePass,
		Models:     map[string]string{"review": ""},
		Findings:   []config.DoneFinding{},
		NotesTurns: []string{},
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"base", "head", "method", "models", "findings", "omitted", "notes_turns", "usage", "time_ms", "turns"} {
		if _, ok := got[key]; !ok {
			t.Errorf("the report JSON carries no %q: %s", key, data)
		}
	}
	if string(got["findings"]) != "[]" {
		t.Errorf("findings = %s, want [] not null", got["findings"])
	}
	if string(got["notes_turns"]) != "[]" {
		t.Errorf("notes_turns = %s, want [] not null", got["notes_turns"])
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(got["usage"], &usage); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"input", "output", "reasoning", "cache_read", "cache_write", "cost_usd"} {
		if _, ok := usage[key]; !ok {
			t.Errorf("usage carries no %q: %s", key, got["usage"])
		}
	}
}
