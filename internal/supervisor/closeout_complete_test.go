package supervisor

// closeout_complete_test.go — CLA-1030's evidence retry. The fake plane records
// the arguments the close-out sends, which is the evidence the task asks for: the
// retry must keep the delivery identical, append to the stored outcome rather
// than replace it, and cite only the check the driver gated the merge on.

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

const closeOutEvidenceRefusal = `{"error":{"code":"evidence_required","message":"This outcome carries no verification evidence"}}`

// evidenceFixture builds a clean close-out whose complete_task refuses the FIRST
// call (the one that asserts no outcome) and whose get_task serves a stored
// outcome carrying no **Tests** section — the CLA-1010 shape.
func evidenceFixture(t *testing.T, storedOutcome string) *closeOutFixture {
	t.Helper()
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	f.plane.serveTool("get_task", fmt.Sprintf(
		`{"task":{"id":%q,"ref":%q,"status":"approved","heldByRun":false,"branch":%q,"outcome":%q}}`,
		closeOutTestTask, closeOutTestRef, closeOutTestHead, storedOutcome))
	f.plane.refuseWhen = func(tool string, args map[string]any) string {
		if tool == "complete_task" {
			if _, asserts := args["outcome"]; !asserts {
				return closeOutEvidenceRefusal
			}
		}
		return ""
	}
	return f
}

func TestCloseOutRetriesAnEvidenceRefusalWithTheDriversChecks(t *testing.T) {
	const stored = "CI repair: re-ran the gates after the conflict and everything passed."
	f := evidenceFixture(t, stored)
	d := f.supervisor(0)

	lines := f.run(d)

	closed := f.callsFor("complete_task")
	if len(closed) != 2 {
		t.Fatalf("complete_task calls = %d, want 2 (the refused close, then the evidence retry): %v", len(closed), f.plane.planeCalls())
	}
	first, retry := closed[0].args, closed[1].args
	if _, ok := first["outcome"]; ok {
		t.Errorf("the first close asserted an outcome; it must inherit the stored one: %v", first)
	}
	if _, ok := first["ciAttestation"]; ok {
		t.Errorf("the first close carried an attestation it had not earned yet: %v", first)
	}
	if got := retry["runId"]; got != closeOutTestRunCredential {
		t.Errorf("retry runId = %v, want the claim's run credential", got)
	}
	if got := retry["taskId"]; got != closeOutTestTask {
		t.Errorf("retry taskId = %v, want %q", got, closeOutTestTask)
	}

	delivery, ok := retry["delivery"].(map[string]any)
	if !ok {
		t.Fatalf("the retry carries no delivery block: %v", retry)
	}
	firstDelivery, _ := first["delivery"].(map[string]any)
	if fmt.Sprint(delivery) != fmt.Sprint(firstDelivery) {
		t.Errorf("retry delivery = %v, want the SAME declaration as the refused call (%v)", delivery, firstDelivery)
	}
	mergeSHA, _ := delivery["commit"].(string)
	if len(mergeSHA) != 40 {
		t.Fatalf("delivery.commit = %q, want the merge commit's full sha", mergeSHA)
	}
	if delivery["mergeVerified"] != true {
		t.Errorf("delivery.mergeVerified = %v, want true (the ancestor check passed)", delivery["mergeVerified"])
	}

	outcome, _ := retry["outcome"].(string)
	if !config.HasTestsSection(outcome) {
		t.Errorf("the asserted outcome would still be refused for missing evidence:\n%s", outcome)
	}
	if !strings.HasPrefix(outcome, stored) {
		t.Errorf("the asserted outcome does not keep the stored text as read:\n%q", outcome)
	}
	for _, want := range []string{
		"**Tests**",
		"- pr (PR #12, head " + f.headOID + "): pass",
		"- merge " + mergeSHA + " into " + closeOutTestBase,
		"mergeVerified: true",
	} {
		if !strings.Contains(outcome, want) {
			t.Errorf("the **Tests** section does not cite %q:\n%s", want, outcome)
		}
	}

	attestation, ok := retry["ciAttestation"].(map[string]any)
	if !ok {
		t.Fatalf("the retry carries no ciAttestation: %v", retry)
	}
	if attestation["check"] != "pr" || attestation["conclusion"] != "pass" {
		t.Errorf("ciAttestation = %+v, want the check this close-out gated the merge on (pr/pass)", attestation)
	}
	if attestation["branch"] != closeOutTestHead || attestation["pr"] != "#12" {
		t.Errorf("ciAttestation = %+v, want it to name the PR head branch and number it ran against", attestation)
	}

	lookups := f.callsFor("get_task")
	if len(lookups) != 1 {
		t.Fatalf("get_task calls = %d, want 1 (the stored outcome to append to): %v", len(lookups), f.plane.planeCalls())
	}
	if got := lookups[0].args["view"]; got != "compact" {
		t.Errorf("get_task view = %v, want compact (the baseline read, not the whole record)", got)
	}
	if records := f.callsFor("record_progress"); len(records) != 0 {
		t.Errorf("a close that succeeded on the evidence retry still recorded a recovery delivery: %v", records)
	}
	for _, want := range []string{"evidence_required", "retrying once", "task closed done", "driver's verified checks"} {
		if !strings.Contains(lines, want) {
			t.Errorf("the close-out line does not say %q:\n%s", want, lines)
		}
	}
}

// The retry is only worth making when there is something true to cite. The
// extractor refuses a verdict that is not a pass, a PR with no number and a
// judgement with no head revision — each would otherwise force a claim the
// driver cannot stand behind.
func TestCloseOutGatedCheckCitesOnlyAPassedJudgedPR(t *testing.T) {
	pass := delivery.PRState{
		PRRef: delivery.PRRef{Number: 12, Head: closeOutTestHead, HeadOID: "abc123"},
		Check: delivery.Check{Kind: delivery.PRVerified, Status: delivery.Pass},
	}
	gated, ok := closeOutGatedCheckFor(pass)
	if !ok {
		t.Fatal("a passed PR judgement is the gated check result the retry exists to cite")
	}
	if gated.Check != "pr" || gated.Conclusion != "pass" || gated.PR != "#12" ||
		gated.Branch != closeOutTestHead || gated.HeadOID != "abc123" {
		t.Errorf("gated = %+v, want the check name/conclusion with its PR, branch and head", gated)
	}

	cases := map[string]delivery.PRState{
		"no judged PR check": {PRRef: pass.PRRef},
		"the check was not a pass": {
			PRRef: pass.PRRef,
			Check: delivery.Check{Kind: delivery.PRVerified, Status: delivery.Fail},
		},
		"the check has no name": {
			PRRef: pass.PRRef,
			Check: delivery.Check{Status: delivery.Pass},
		},
		"no PR number": {
			PRRef: delivery.PRRef{Head: pass.Head, HeadOID: pass.HeadOID},
			Check: pass.Check,
		},
		"no judged head revision": {
			PRRef: delivery.PRRef{Number: pass.Number, Head: pass.Head},
			Check: pass.Check,
		},
	}
	for name, state := range cases {
		if _, ok := closeOutGatedCheckFor(state); ok {
			t.Errorf("%s: a citation was built with no gated check result behind it", name)
		}
	}
}

// Without a gated check result the close-out keeps today's log-and-hold: the
// delivery is recorded and the claim left to its lease, because a merged PR must
// never return to approved.
func TestCloseOutDoesNotRetryWithoutAGatedCheckToCite(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	f.plane.refuseWhen = func(tool string, _ map[string]any) string {
		if tool == "complete_task" {
			return closeOutEvidenceRefusal
		}
		return ""
	}
	d := f.supervisor(0)
	client := d.closeOutFor(d.instances[0].cfg, closeOutTestSlug)
	if client == nil {
		t.Fatal("the fixture's supervisor has no close-out client")
	}

	retried, err := d.completeCloseOut(client, closeOutTestRef, "task:"+closeOutTestTask, closeOutTestTask, closeOutTestRunCredential, plane.CloseOutDelivery{
		Commit:            "38e21749",
		PR:                "#12",
		IntegrationBranch: closeOutTestBase,
		MergeVerified:     true,
	}, nil)
	if err == nil {
		t.Fatal("the refusal must still be returned: nothing closed the task")
	}
	if retried {
		t.Error("retried = true with no gated check result; the driver must not assert evidence it never saw")
	}
	if got := len(f.callsFor("complete_task")); got != 1 {
		t.Errorf("complete_task calls = %d, want 1 (no retry): %v", got, f.plane.planeCalls())
	}
	if got := len(f.callsFor("get_task")); got != 0 {
		t.Errorf("get_task calls = %d, want 0 (there is nothing to append to): %v", got, f.plane.planeCalls())
	}
}

// A stored outcome that already carries a **Tests** section means the refusal is
// a different problem. Retrying with a second section could only repeat it, so
// the close-out logs and holds.
func TestCloseOutDoesNotRetryWhenTheStoredOutcomeAlreadyHasTests(t *testing.T) {
	const stored = "the repair note\n\n**Tests**\n- go test ./...: pass"
	f := evidenceFixture(t, stored)
	// Refuse the evidence-bearing call too: the guard must be read BEFORE it is
	// sent, not discovered by sending it again.
	f.plane.refuseWhen = func(tool string, _ map[string]any) string {
		if tool == "complete_task" {
			return closeOutEvidenceRefusal
		}
		return ""
	}
	d := f.supervisor(0)

	lines := f.run(d)

	if got := len(f.callsFor("complete_task")); got != 1 {
		t.Errorf("complete_task calls = %d, want 1 (a **Tests**-bearing outcome is not the refusal this retry answers): %v", got, f.plane.planeCalls())
	}
	if got := len(f.callsFor("get_task")); got != 1 {
		t.Errorf("get_task calls = %d, want 1 (the stored outcome is read before deciding): %v", got, f.plane.planeCalls())
	}
	if records := f.callsFor("record_progress"); len(records) != 1 {
		t.Errorf("record_progress calls = %d, want 1 (the merged delivery must still be recorded): %v", len(records), f.plane.planeCalls())
	}
	if !strings.Contains(lines, "already carries a **Tests** section") {
		t.Errorf("the close-out line does not say why it did not retry:\n%s", lines)
	}
}

// A stored outcome that cannot be read gives the retry no baseline to keep. In
// that case the close-out logs and holds, exactly as it does when it has no check
// to cite: an outcome built from the empty string would replace the record the
// retry exists to preserve.
func TestCloseOutDoesNotRetryWhenTheStoredOutcomeCannotBeRead(t *testing.T) {
	f := evidenceFixture(t, "a repair note with no evidence section")
	f.plane.refuseTool("get_task", "plane read failed")
	d := f.supervisor(0)

	lines := f.run(d)

	if got := len(f.callsFor("complete_task")); got != 1 {
		t.Errorf("complete_task calls = %d, want 1 (no retry from an unreadable baseline): %v", got, f.plane.planeCalls())
	}
	if got := len(f.callsFor("get_task")); got != 1 {
		t.Errorf("get_task calls = %d, want 1 (the baseline read that failed): %v", got, f.plane.planeCalls())
	}
	if records := f.callsFor("record_progress"); len(records) != 1 {
		t.Errorf("record_progress calls = %d, want 1 (the merged delivery must still be recorded): %v", len(records), f.plane.planeCalls())
	}
	if releases := f.releases(); releases != 0 {
		t.Errorf("release calls = %d after a landed merge: a merged PR must never return to approved", releases)
	}
	for _, want := range []string{"stored outcome could not be read", "not retrying"} {
		if !strings.Contains(lines, want) {
			t.Errorf("the close-out line does not say %q:\n%s", want, lines)
		}
	}
}

// An empty stored outcome leaves nothing to keep either: appending the section
// alone would overwrite whatever the plane meant by an absent body.
func TestCloseOutDoesNotRetryWhenTheStoredOutcomeIsEmpty(t *testing.T) {
	f := evidenceFixture(t, "")
	d := f.supervisor(0)

	lines := f.run(d)

	if got := len(f.callsFor("complete_task")); got != 1 {
		t.Errorf("complete_task calls = %d, want 1 (nothing to append the evidence to): %v", got, f.plane.planeCalls())
	}
	if got := len(f.callsFor("get_task")); got != 1 {
		t.Errorf("get_task calls = %d, want 1 (the empty baseline read): %v", got, f.plane.planeCalls())
	}
	if records := f.callsFor("record_progress"); len(records) != 1 {
		t.Errorf("record_progress calls = %d, want 1 (the merged delivery must still be recorded): %v", len(records), f.plane.planeCalls())
	}
	if releases := f.releases(); releases != 0 {
		t.Errorf("release calls = %d after a landed merge: a merged PR must never return to approved", releases)
	}
	for _, want := range []string{"stored outcome is empty", "no reviewed baseline"} {
		if !strings.Contains(lines, want) {
			t.Errorf("the close-out line does not say %q:\n%s", want, lines)
		}
	}
}

// A refusal for any other reason is the plane's considered answer to the call as
// made. Only evidence_required earns the retry; nothing else may.
func TestCloseOutDoesNotRetryAnotherRefusalCode(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	f.plane.refuseWhen = func(tool string, _ map[string]any) string {
		if tool == "complete_task" {
			return `{"error":{"code":"not_ready","message":"the task is not ready for this write"}}`
		}
		return ""
	}
	d := f.supervisor(0)

	lines := f.run(d)

	if got := len(f.callsFor("complete_task")); got != 1 {
		t.Errorf("complete_task calls = %d, want 1 (only evidence_required earns a retry): %v", got, f.plane.planeCalls())
	}
	if got := len(f.callsFor("get_task")); got != 0 {
		t.Errorf("get_task calls = %d, want 0 (another refusal is not evidence-shaped): %v", got, f.plane.planeCalls())
	}
	if records := f.callsFor("record_progress"); len(records) != 1 {
		t.Errorf("record_progress calls = %d, want 1: %v", len(records), f.plane.planeCalls())
	}
	if !strings.Contains(lines, "delivery RECORDED") {
		t.Errorf("the merged delivery was not recorded after the refusal:\n%s", lines)
	}
}

// A retry that is refused too is still the plane's answer: the delivery is
// recorded, the claim is left held, and the line says both things happened.
func TestCloseOutRecordsWhenTheEvidenceRetryIsRefusedToo(t *testing.T) {
	f := evidenceFixture(t, "a repair note with no evidence section")
	f.plane.refuseWhen = func(tool string, _ map[string]any) string {
		if tool == "complete_task" {
			return closeOutEvidenceRefusal
		}
		return ""
	}
	d := f.supervisor(0)

	lines := f.run(d)

	if got := len(f.callsFor("complete_task")); got != 2 {
		t.Fatalf("complete_task calls = %d, want 2 (the refused close and its one evidence retry): %v", got, f.plane.planeCalls())
	}
	if retry := f.callsFor("complete_task")[1].args; retry["outcome"] == nil {
		t.Error("the second call did not assert the evidence outcome")
	}
	if records := f.callsFor("record_progress"); len(records) != 1 {
		t.Fatalf("record_progress calls = %d, want 1 (the merge must be recorded): %v", len(records), f.plane.planeCalls())
	}
	if releases := f.releases(); releases != 0 {
		t.Errorf("release calls = %d after a landed merge: a merged PR must never return to approved", releases)
	}
	for _, want := range []string{"evidence retry was refused too", "the evidence_required retry was attempted and refused too", "delivery RECORDED", "left held"} {
		if !strings.Contains(lines, want) {
			t.Errorf("the close-out line does not say %q:\n%s", want, lines)
		}
	}
}

// The section states the ancestry verdict honestly. A merge that landed but whose
// check did not settle must not be rendered as verified.
func TestCloseOutTestsSectionStatesAnUnverifiedMerge(t *testing.T) {
	gated := closeOutGatedCheck{Check: "pr", Conclusion: "pass", PR: "#12", HeadOID: "abc123"}
	decl := plane.CloseOutDelivery{Commit: "merge456", IntegrationBranch: "staging", MergeVerified: true}

	verified := closeOutTestsSection(gated, decl)
	if !strings.Contains(verified, "mergeVerified: true") || strings.Contains(verified, "did not settle") {
		t.Errorf("verified section = %q, want the ancestor verdict stated", verified)
	}
	decl.MergeVerified = false
	unverified := closeOutTestsSection(gated, decl)
	if !strings.Contains(unverified, "mergeVerified: false") || !strings.Contains(unverified, "did not settle") {
		t.Errorf("unverified section = %q, want the unproven ancestry stated, never a pass", unverified)
	}
	if strings.Contains(unverified, "mergeVerified: true") {
		t.Errorf("unverified section claims a verified merge: %q", unverified)
	}
}

// The post-merge close writes (completeCloseOut, recordMergedDelivery) and the
// transient-failure retry that bounds them. Their source lives in
// closeout_complete.go, so these tests live here.

// A tool-level refusal of the final close is NOT retried (it is the plane's
// considered answer), and it must never become a release: the merge landed, so the
// PR is no longer OPEN and a release would strand the task on approved forever. The
// delivery is recorded instead, and the claim is left to its lease.
func TestCloseOutHoldsTheClaimWhenThePlaneRefusesTheClose(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	f.plane.refuseWhen = func(tool string, args map[string]any) string {
		if tool == "complete_task" {
			return `{"error":{"code":"not_ready","message":"the task is not ready for this write"}}`
		}
		return ""
	}
	d := f.supervisor(0)

	lines := f.run(d)

	if releases := f.releases(); releases != 0 {
		t.Fatalf("release calls = %d after the plane refused the close: a merged PR must never return to approved: %v", releases, f.plane.planeCalls())
	}
	if got := len(f.callsFor("complete_task")); got != 1 {
		t.Errorf("complete_task calls = %d, want 1 (a tool-level refusal is not retried): %v", got, f.plane.planeCalls())
	}
	records := f.callsFor("record_progress")
	if len(records) != 1 {
		t.Fatalf("record_progress calls = %d, want 1 (the delivery must be recorded so a successor can close the task): %v", len(records), f.plane.planeCalls())
	}
	delivery, ok := records[0].args["delivery"].(map[string]any)
	if !ok {
		t.Fatalf("the recovery record carries no delivery block: %v", records[0].args)
	}
	if got := delivery["pr"]; got != fmt.Sprintf("#%d", closeOutTestPR) {
		t.Errorf("recorded delivery.pr = %v, want #%d", got, closeOutTestPR)
	}
	if got, _ := delivery["commit"].(string); len(got) != 40 {
		t.Errorf("recorded delivery.commit = %q, want the merge commit's sha", got)
	}
	if _, ok := records[0].args["status"]; ok {
		t.Errorf("the recovery record changed the task's status: %v", records[0].args)
	}
	for _, want := range []string{"PR #" + fmt.Sprint(closeOutTestPR), "left held", "delivery RECORDED"} {
		if !strings.Contains(lines, want) {
			t.Errorf("the stand-down does not say %q:\n%s", want, lines)
		}
	}
}

// A transient close failure is retried: a 502 that clears in a moment must not
// cost the task its claim, and it must not be mistaken for an answer.
func TestCloseOutRetriesATransientCloseFailure(t *testing.T) {
	oldBackoff := closeOutCompleteBackoff
	closeOutCompleteBackoff = time.Millisecond
	defer func() { closeOutCompleteBackoff = oldBackoff }()

	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	var mu sync.Mutex
	attempts := 0
	f.plane.setStatusWhen(func(tool string, _ map[string]any) int {
		if tool != "complete_task" {
			return 0
		}
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts < 3 {
			return http.StatusBadGateway
		}
		return 0
	})
	d := f.supervisor(0)

	lines := f.run(d)

	if got := len(f.callsFor("complete_task")); got != 3 {
		t.Fatalf("complete_task calls = %d, want 3 (two 502s, then the close): %v", got, f.plane.planeCalls())
	}
	if releases := f.releases(); releases != 0 {
		t.Errorf("a merged PR was released during a transient close failure: %v", f.plane.planeCalls())
	}
	if records := f.callsFor("record_progress"); len(records) != 0 {
		t.Errorf("a close that succeeded on retry still recorded a recovery delivery: %v", records)
	}
	if !strings.Contains(lines, "task closed done") {
		t.Errorf("the close-out did not report the eventual close:\n%s", lines)
	}
	if !strings.Contains(lines, "failed transiently") {
		t.Errorf("the retry was not reported:\n%s", lines)
	}
}

// A close that never succeeds still never releases. The delivery is recorded (so a
// successor knows the merge SHA and PR are already merged) and the claim is left
// held for its lease.
func TestCloseOutRecordsTheDeliveryWhenTheCloseNeverSucceeds(t *testing.T) {
	oldBackoff := closeOutCompleteBackoff
	closeOutCompleteBackoff = time.Millisecond
	defer func() { closeOutCompleteBackoff = oldBackoff }()

	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	f.plane.setStatusWhen(func(tool string, _ map[string]any) int {
		if tool == "complete_task" {
			return http.StatusServiceUnavailable
		}
		return 0
	})
	d := f.supervisor(0)

	lines := f.run(d)

	if got := len(f.callsFor("complete_task")); got != closeOutCompleteAttempts {
		t.Fatalf("complete_task calls = %d, want the full retry bound %d: %v", got, closeOutCompleteAttempts, f.plane.planeCalls())
	}
	if releases := f.releases(); releases != 0 {
		t.Fatalf("release calls = %d after the close exhausted its retries: a merged PR must never return to approved: %v", releases, f.plane.planeCalls())
	}
	records := f.callsFor("record_progress")
	if len(records) != 1 {
		t.Fatalf("record_progress calls = %d, want 1 (the delivery must be recorded): %v", len(records), f.plane.planeCalls())
	}
	delivery, ok := records[0].args["delivery"].(map[string]any)
	if !ok {
		t.Fatalf("the recovery record carries no delivery block: %v", records[0].args)
	}
	sha, _ := delivery["commit"].(string)
	if len(sha) != 40 {
		t.Fatalf("recorded delivery.commit = %q, want the merge commit's full sha", sha)
	}
	for _, want := range []string{fmt.Sprintf("PR #%d", closeOutTestPR), "merged as " + sha, "left held", "delivery RECORDED"} {
		if !strings.Contains(lines, want) {
			t.Errorf("the failure line does not say %q:\n%s", want, lines)
		}
	}
}

// A plane that just answered four 502s on the close will usually fail the
// recovery write too, so the record gets the same bounded retry: the merge SHA
// and PR must not be lost exactly when the plane is the thing failing.
func TestCloseOutRetriesATransientDeliveryRecord(t *testing.T) {
	oldBackoff := closeOutCompleteBackoff
	closeOutCompleteBackoff = time.Millisecond
	defer func() { closeOutCompleteBackoff = oldBackoff }()

	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	var mu sync.Mutex
	records := 0
	f.plane.setStatusWhen(func(tool string, _ map[string]any) int {
		switch tool {
		case "complete_task":
			return http.StatusServiceUnavailable
		case "record_progress":
			mu.Lock()
			defer mu.Unlock()
			records++
			if records < 3 {
				return http.StatusBadGateway
			}
		}
		return 0
	})
	d := f.supervisor(0)

	lines := f.run(d)

	if got := len(f.callsFor("record_progress")); got != 3 {
		t.Fatalf("record_progress calls = %d, want 3 (two 502s, then the record): %v", got, f.plane.planeCalls())
	}
	if !strings.Contains(lines, "delivery RECORDED") {
		t.Errorf("the recorded delivery was not reported:\n%s", lines)
	}
}

// If even the recovery record is refused, that line is the only message a
// successor gets: it must say the merge SHA could not be written down rather than
// disappear with the record it meant to make.
func TestCloseOutSaysWhenTheDeliveryCouldNotBeRecorded(t *testing.T) {
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	f.plane.refuseWhen = func(tool string, _ map[string]any) string {
		switch tool {
		case "complete_task":
			return `{"error":{"code":"not_ready","message":"the task is not ready for this write"}}`
		case "record_progress":
			return `{"error":{"code":"not_holding","message":"the task is no longer held"}}`
		}
		return ""
	}
	d := f.supervisor(0)

	lines := f.run(d)

	if releases := f.releases(); releases != 0 {
		t.Fatalf("release calls = %d after a merged PR whose close and record both failed: %v", releases, f.plane.planeCalls())
	}
	if got := len(f.callsFor("record_progress")); got != 1 {
		t.Errorf("record_progress calls = %d, want 1 (a tool refusal is not retried): %v", got, f.plane.planeCalls())
	}
	if !strings.Contains(lines, "AND the delivery could not be recorded") {
		t.Errorf("the failure line does not say the delivery could not be recorded:\n%s", lines)
	}
}
