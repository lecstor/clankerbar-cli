package loop

// stall_test.go — CLA-584: the output-cap stall is NAMED in the daemon log.
//
// The stall itself is detected and (once) resumed inside the adapter; what the
// driver owes is the log. MAK-123's post-mortem found only the generic
// "the phase finished but never moved the task on" line in daemon.log, with
// nothing saying the model had spent its whole output budget thinking and
// emitted nothing — so these tests pin the named line, the task ref it carries,
// and that the generic line is REPLACED rather than joined.

import (
	"context"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// stalledHeldResult is what a stalled-and-not-recovered session leaves behind:
// the adapter's stall marker with the step's reasoning count, and a claim with
// pushed work — the combination that must leave the lease to expire rather than
// hand the task back (Claim.Releasable).
func stalledHeldResult() harness.Result {
	return harness.Result{
		ExitCode: 0,
		Claim: harness.Claim{
			TaskID: "15977aab-9cec-4f1f-b4aa-6776876f9413",
			Ref:    "MAK-123",
			RunID:  "r-1",
			HasWIP: true,
			Branch: "clanker/15977aab-render-analytics-charts-authed-paid-tab",
		},
		Raw: map[string]any{
			harness.FinishReasonKey:       harness.FinishReasonLength,
			harness.TerminalReasonKey:     harness.OutputCapReason,
			harness.OutputCapReasoningKey: 32000,
		},
	}
}

func TestReleaseHeldClaim_NamesTheOutputCapStall(t *testing.T) {
	logs := captureLogs(t)
	d := &Driver{}

	if released := d.releaseHeldClaim(context.Background(), Target{Name: "makespdf"}, stalledHeldResult(), true); released {
		t.Error("a claim with pushed work must not be released to the queue")
	}

	out := logs.String()
	if !strings.Contains(out, "stalled: output cap hit with no output (reasoning=32000)") {
		t.Errorf("the daemon log must name the stall and the reasoning it burned; got: %q", out)
	}
	if !strings.Contains(out, "MAK-123") {
		t.Errorf("the named line must carry the task ref; got: %q", out)
	}
	if strings.Contains(out, "never moved the task on") {
		t.Errorf("the generic line must be replaced, not joined, for this case; got: %q", out)
	}
	if d.undeclared != 1 {
		t.Errorf("undeclared hand-offs = %d, want 1 — a stalled phase that never moved the task on is still an undeclared hand-off", d.undeclared)
	}
}

// The other ends keep the generic line: a held-with-work session that did NOT
// stall must not be labelled a stall.
func TestReleaseHeldClaim_KeepsTheGenericLineWithoutAStall(t *testing.T) {
	logs := captureLogs(t)
	res := stalledHeldResult()
	res.Raw[harness.FinishReasonKey] = harness.FinishReasonUnknown
	res.Raw[harness.TerminalReasonKey] = harness.ZeroUsageReason
	delete(res.Raw, harness.OutputCapReasoningKey)

	d := &Driver{}
	d.releaseHeldClaim(context.Background(), Target{Name: "makespdf"}, res, true)

	out := logs.String()
	if !strings.Contains(out, "never moved the task on") {
		t.Errorf("a non-stall exit must keep the generic hand-off line; got: %q", out)
	}
	if strings.Contains(out, "stalled:") {
		t.Errorf("a non-stall exit must not be named as the stall; got: %q", out)
	}
}

// The exit paths that pass countUndeclared=false still name the stall: the
// cause is worth saying wherever the session is left holding pushed work.
func TestReleaseHeldClaim_NamesTheStallWithoutTheCounter(t *testing.T) {
	logs := captureLogs(t)
	d := &Driver{}

	d.releaseHeldClaim(context.Background(), Target{Name: "makespdf"}, stalledHeldResult(), false)

	out := logs.String()
	if !strings.Contains(out, "stalled: output cap hit with no output (reasoning=32000)") {
		t.Errorf("the named line must not depend on countUndeclared; got: %q", out)
	}
	if strings.Contains(out, "undeclared hand-offs") {
		t.Errorf("countUndeclared=false must not report the undeclared counter; got: %q", out)
	}
}

// A stalled session with NO pushed work is releasable, so it goes back to the
// queue — and the log still says why it stopped, in place of the generic
// hand-back line. This is the shape that leaves nothing behind (a stall before
// any branch was pushed), where "handed back to the queue" alone would send the
// next clanker to rediscover the cause.
func TestReleaseHeldClaim_NamesTheStallOnTheReleasePath(t *testing.T) {
	logs := captureLogs(t)
	rel := &fakeReleaser{}
	d := &Driver{}
	res := stalledHeldResult()
	res.Claim.HasWIP = false
	res.Claim.Branch = ""

	if released := d.releaseHeldClaim(context.Background(), Target{Name: "makespdf", Releaser: rel}, res, true); !released {
		t.Error("a stalled claim with no pushed work must still be released to the queue")
	}

	out := logs.String()
	if !strings.Contains(out, "stalled: output cap hit with no output (reasoning=32000)") {
		t.Errorf("the release path must name the stall too; got: %q", out)
	}
	if !strings.Contains(out, "MAK-123") {
		t.Errorf("the named release line must carry the task ref; got: %q", out)
	}
	if strings.Contains(out, "(the session ended still holding it)") {
		t.Errorf("the named line must replace the generic hand-back line for a stall, not join it; got: %q", out)
	}
	if len(rel.calls) != 1 || rel.calls[0].taskID != res.Claim.TaskID {
		t.Errorf("the release must still reach the plane once for this task; calls = %+v", rel.calls)
	}
}

// A releasable claim that did NOT stall keeps the generic hand-back line: only
// the stall is named, not every release.
func TestReleaseHeldClaim_KeepsTheGenericLineOnTheReleasePathWithoutAStall(t *testing.T) {
	logs := captureLogs(t)
	rel := &fakeReleaser{}
	d := &Driver{}
	res := stalledHeldResult()
	res.Claim.HasWIP = false
	delete(res.Raw, harness.TerminalReasonKey)
	delete(res.Raw, harness.OutputCapReasoningKey)

	if released := d.releaseHeldClaim(context.Background(), Target{Name: "makespdf", Releaser: rel}, res, true); !released {
		t.Error("a releasable claim must be handed back")
	}

	out := logs.String()
	if !strings.Contains(out, "(the session ended still holding it)") {
		t.Errorf("a non-stall release must keep the generic hand-back line; got: %q", out)
	}
	if strings.Contains(out, "stalled:") {
		t.Errorf("a non-stall release must not be named as the stall; got: %q", out)
	}
}
