package loop

// pr_handover.go — the runner owns the pull request a runner-claimed task is
// reviewed through (CLA-1035). A session used to open it and reclaim its tree
// in the gap before its done report; a session that died there left a PR the
// task never named and a worktree nobody removed. The runner now opens (or
// reuses) the PR, records delivery.pr, and only then hands the task over.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// prHandOverTimeout bounds the whole PR step - the open read, the create and its
// one retry - so a wedged gh cannot hold a finished phase open indefinitely.
const prHandOverTimeout = 2 * time.Minute

// handOverPR opens (or reuses) the PR for branch, records delivery.pr, and
// reports whether the hand-over may proceed. It fails closed: a task whose PR
// cannot be opened or linked is not handed to in_review, because that is exactly
// the state this step exists to prevent. The caller keeps its claim live, so the
// ordinary handback preserves the pushed branch for takeover.
func (r *phaseRun) handOverPR(ctx context.Context, at *phaseAttempt, w plane.ReportWriter, st plane.TaskState, rep config.DoneReport, branch string) bool {
	if strings.TrimSpace(branch) == "" {
		r.noteReportProblem("the task names no branch to open a pull request from, so it was not handed over")
		return false
	}
	number, err := r.ensurePR(ctx, at, st, rep, branch)
	if err != nil {
		r.noteReportProblem("opening the pull request for %s failed (%v) — the task was not handed over, and its pushed branch is kept for takeover", branch, err)
		return false
	}
	pr := fmt.Sprintf("#%d", number)
	// delivery.pr lands BEFORE submit_for_review: a later refusal of the
	// hand-over must still leave the PR linked to the task.
	if err := w.RecordDeliveryPR(ctx, at.res.Claim.TaskID, at.res.Claim.RunID, pr); err != nil {
		r.noteReportWrite("recording delivery.pr "+pr, err)
		return false
	}
	log.Printf("%siteration %d: the %s phase: PR %s is open for %s (base %s) — the runner recorded it as the task's delivery",
		labelOf(r.t), r.drainNum, r.phaseLabel(), pr, branch, r.prBase)
	return true
}

// worktreeReclaimTimeout bounds the local git commands that remove the tree.
const worktreeReclaimTimeout = 30 * time.Second

// reclaimHandedOverWorktree removes the task worktree the runner prepared, after
// the hand-over landed. It never fails the phase: the task is already in
// in_review with its PR open and linked, so a leftover tree is a warning. It
// outlives a cancellation for the same reason the handback does: a stop that
// lands here must not leave the tree behind.
func (r *phaseRun) reclaimHandedOverWorktree(ctx context.Context, branch string) {
	if strings.TrimSpace(r.opts.taskDir) == "" {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), worktreeReclaimTimeout)
	defer cancel()
	if err := reclaimTaskWorktree(rctx, r.opts.taskDir, branch); err != nil {
		log.Printf("%siteration %d: the %s phase: the task is handed over and its PR is linked, but the runner could not reclaim the task worktree %s (%v) — leaving it in place",
			labelOf(r.t), r.drainNum, r.phaseLabel(), r.opts.taskDir, err)
		return
	}
	log.Printf("%siteration %d: the %s phase: reclaimed the task worktree %s after the hand-over",
		labelOf(r.t), r.drainNum, r.phaseLabel(), r.opts.taskDir)
}

// ensurePR is the one call the two terminal reports share: the runner's PR
// opener, given the deterministic spec the task already carries.
func (r *phaseRun) ensurePR(ctx context.Context, at *phaseAttempt, st plane.TaskState, rep config.DoneReport, branch string) (int, error) {
	if strings.TrimSpace(at.inv.WorkDir) == "" {
		return 0, errors.New("there is no working tree to open the pull request from")
	}
	if r.d.newPROpener == nil {
		return 0, errors.New("this driver has no pull request opener configured")
	}
	if strings.TrimSpace(r.prBase) == "" {
		return 0, errors.New("the repository's PR base branch could not be resolved")
	}
	vctx, cancel := context.WithTimeout(ctx, prHandOverTimeout)
	defer cancel()
	return r.d.newPROpener(at.inv.WorkDir).EnsurePR(vctx, delivery.PRSpec{
		Repo:  at.inv.WorkDir,
		Slug:  r.taskRepoSlug(),
		Base:  r.prBase,
		Head:  branch,
		Title: r.prTitle(st, branch),
		Body:  r.prBody(st, rep, branch),
	})
}

// taskRepoSlug is the GitHub repository the task's work belongs to: the task's
// own repo identity, else the project's primary repo. Empty lets the opener
// resolve it from the worktree's origin, which is what the branch was pushed to.
func (r *phaseRun) taskRepoSlug() string {
	if r.opts.runner != nil {
		if slug := strings.TrimSpace(r.opts.runner.task.Repo); slug != "" {
			return slug
		}
	}
	return strings.TrimSpace(r.t.PrimaryRepo)
}

// prTitle is "<ref>: <task title>", so the PR is always matched back to its task
// (the promotion deck's mapping depends on the ref). With no ref, the title
// alone; with no title, the ref alone; with neither, the branch - gh refuses an
// empty title, and the branch still identifies the work.
func (r *phaseRun) prTitle(st plane.TaskState, branch string) string {
	title := ""
	if r.opts.runner != nil {
		title = strings.TrimSpace(r.opts.runner.task.Title)
	}
	ref := strings.TrimSpace(st.Ref)
	if ref == "" && r.opts.runner != nil {
		ref = strings.TrimSpace(r.opts.runner.task.Ref)
	}
	switch {
	case ref != "" && title != "":
		return ref + ": " + title
	case title != "":
		return title
	case ref != "":
		return ref
	}
	return branch
}

// prBody assembles the PR body deterministically from what the task already
// records: the task ref, the branch, the session's summary, its tests (or the
// stored outcome's **Tests** section when the report omitted them), and the
// round's verdict.
func (r *phaseRun) prBody(st plane.TaskState, rep config.DoneReport, branch string) string {
	tests := strings.TrimSpace(rep.Tests)
	if tests == "" {
		tests = outcomeTests(st.Outcome)
	}
	ref := strings.TrimSpace(st.Ref)
	if ref == "" && r.opts.runner != nil {
		ref = strings.TrimSpace(r.opts.runner.task.Ref)
	}
	var b strings.Builder
	if ref != "" {
		fmt.Fprintf(&b, "Task: %s\n", ref)
	}
	fmt.Fprintf(&b, "Branch: `%s`\n", branch)
	if summary := strings.TrimSpace(rep.Summary); summary != "" {
		fmt.Fprintf(&b, "\n## Summary\n\n%s\n", summary)
	}
	if tests != "" {
		fmt.Fprintf(&b, "\n## Tests\n\n%s\n", tests)
	}
	if verdict := strings.TrimSpace(rep.Verdict); verdict != "" {
		fmt.Fprintf(&b, "\n## Review\n\n%s\n", verdict)
	}
	return strings.TrimSpace(b.String()) + "\n"
}

// outcomeTestsHeading matches the **Tests** heading config.RenderTestsSection
// writes, plus the markdown spellings the plane's guard accepts.
var outcomeTestsHeading = regexp.MustCompile(`(?im)^[ \t]*(?:\*\*[ \t]*tests\b[^*\n]*\*\*|__[ \t]*tests\b[^_\n]*__|#{1,6}[ \t]+tests\b)[ \t]*$`)

// outcomeTests lifts the body of an outcome's **Tests** section, so a fix
// report that omitted its own tests still gives the PR the gates the run
// actually executed.
var outcomeSectionHeading = regexp.MustCompile(`(?m)^[ \t]*(?:#{1,6}[ \t]+\S|\*\*[^*\n]+\*\*[ \t]*$)`)

func outcomeTests(outcome string) string {
	loc := outcomeTestsHeading.FindStringIndex(outcome)
	if loc == nil {
		return ""
	}
	rest := strings.TrimLeft(outcome[loc[1]:], "\n")
	var body []string
	for _, line := range strings.Split(rest, "\n") {
		if outcomeSectionHeading.MatchString(line) {
			break
		}
		body = append(body, line)
	}
	return strings.TrimSpace(strings.Join(body, "\n"))
}
