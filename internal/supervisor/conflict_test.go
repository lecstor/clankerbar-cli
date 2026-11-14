package supervisor

// The conflict-only session's tests (CLA-759).
//
// They drive the real close-out pass against the same git fixture and plane wire
// shape CLA-758's tests use, and replace ONLY the session launcher with a fake:
// the session is a paid model call, so what a test can pin is what the supervisor
// hands it (the brief, the harness/model, the claim's run id, the paths, the
// gate), that it starts one at a time and only for an unclaimed conflict, and
// what it does with the state the session leaves behind.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

const (
	conflictTestTask2 = "task-2"
	conflictTestRef2  = "CLA-2"
)

// conflictingFixture is the world every test here starts from: a task whose PR
// really conflicts on a.txt and b.txt, a clean policy dial, and the two branches
// pushed to a bare remote.
func conflictingFixture(t *testing.T) *closeOutFixture {
	t.Helper()
	f := newCloseOutFixture(t, true)
	f.servePlane(t, "clanker")
	f.writePR(closeOutTestBase, "CONFLICTING", "DIRTY",
		`[{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"}]`)
	return f
}

func TestConflictStandDownStartsOneConflictSession(t *testing.T) {
	f := conflictingFixture(t)
	d := f.supervisor(0)

	lines := f.run(d)

	calls := f.conflictCalls()
	if len(calls) != 1 {
		t.Fatalf("conflict sessions = %d, want exactly one:\n%s", len(calls), lines)
	}
	call := calls[0]

	// The claim is the supervisor's, and the session is briefed to sign with the
	// run it came back with. Without that, the session could not call heartbeat
	// or hand the task back at all.
	if got := f.callsFor("claim_task"); len(got) != 1 {
		t.Fatalf("claim_task calls = %d, want exactly one (the conflict session's claim): %v", len(got), got)
	}
	if call.req.RunID != closeOutTestRunCredential {
		t.Errorf("the session was briefed with run %q, want the claim's credential %q", call.req.RunID, closeOutTestRunCredential)
	}
	if !strings.Contains(call.req.Worktree, "conflict-"+closeOutTestTask) {
		t.Errorf("the worktree path %q does not name the task", call.req.Worktree)
	}
	if !strings.Contains(call.req.Prompt, call.req.Worktree) {
		t.Errorf("the brief does not name the worktree %q:\n%s", call.req.Worktree, call.req.Prompt)
	}
	for _, want := range []string{closeOutTestTask, closeOutTestRef, closeOutTestRunCredential, closeOutTestHead, closeOutTestBase, "#12", "a.txt", "b.txt"} {
		if !strings.Contains(call.req.Prompt, want) {
			t.Errorf("the brief does not name %q:\n%s", want, call.req.Prompt)
		}
	}
	// The conflict contract itself, not just its inputs.
	for _, want := range []string{"never rebase", "only the conflicting hunks", "RESOLUTION: mechanical", "RESOLUTION: behavioural", "RESOLUTION: failed"} {
		if !strings.Contains(strings.ToLower(call.req.Prompt), strings.ToLower(want)) {
			t.Errorf("the brief does not carry %q:\n%s", want, call.req.Prompt)
		}
	}
	// The fleet's implement step by default (the fixture's config names opencode
	// and implement-model), so an operator who never mentions conflicts still
	// gets a session on the model that implemented the branch.
	if call.req.Harness != "opencode" || call.req.Model != "implement-model" {
		t.Errorf("the session runs on %s/%s, want the implement step opencode/implement-model", call.req.Harness, call.req.Model)
	}
	if call.step.Harness != "opencode" || call.step.Model.First() != "implement-model" {
		t.Errorf("the resolved step = %+v, want the implement step", call.step)
	}
	if !slices.Equal(call.req.Routes, []string{"implement-model"}) {
		t.Errorf("session routes = %v, want the implement step's route list", call.req.Routes)
	}
	if !strings.Contains(lines, "outcome: mechanical") {
		t.Errorf("the pass did not log the session's outcome:\n%s", lines)
	}
}

// CLA-890: an unset conflict block inherits the implement step's WHOLE model
// route list — the free route first and its paid twin behind it — so a quota on
// the first has somewhere to fall back to.
func TestConflictSessionInheritsTheImplementStepsWholeModelList(t *testing.T) {
	f := conflictingFixture(t)
	d := f.supervisor(0)
	cfg := d.instances[0].cfg
	cfg.Model = ""
	cfg.Harnesses = map[string]config.HarnessConfig{
		"opencode": {Model: config.ModelRoutes{"opencode/deepseek-v4.1-flash-free", "opencode-go/deepseek-v4.1-flash"}},
	}

	f.run(d)

	calls := f.conflictCalls()
	if len(calls) != 1 {
		t.Fatalf("conflict sessions = %d, want one", len(calls))
	}
	want := []string{"opencode/deepseek-v4.1-flash-free", "opencode-go/deepseek-v4.1-flash"}
	if !slices.Equal(calls[0].step.Model.List(), want) {
		t.Errorf("resolved conflict models = %v, want the implement step's whole list %v", calls[0].step.Model.List(), want)
	}
	if !slices.Equal(calls[0].req.Routes, want) {
		t.Errorf("session routes = %v, want the whole list %v", calls[0].req.Routes, want)
	}
	if calls[0].req.Model != want[0] {
		t.Errorf("session starts on %q, want the first route %q", calls[0].req.Model, want[0])
	}
}

func TestConflictSessionUsesItsOwnStep(t *testing.T) {
	f := conflictingFixture(t)
	d := f.supervisor(0)
	d.instances[0].cfg.Conflict = &config.ConflictStep{Model: config.ModelRoutes{"gpt-x"}, MaxTurns: 40}

	f.run(d)

	calls := f.conflictCalls()
	if len(calls) != 1 {
		t.Fatalf("conflict sessions = %d, want one", len(calls))
	}
	if calls[0].req.Model != "gpt-x" {
		t.Errorf("the session runs on %s, want the conflict step's gpt-x", calls[0].req.Model)
	}
	if calls[0].step.MaxTurns != 40 {
		t.Errorf("max turns = %d, want the conflict step's 40", calls[0].step.MaxTurns)
	}
}

func TestConflictSessionRefusesAHarnessThatCannotTakeExtraDirs(t *testing.T) {
	f := conflictingFixture(t)
	d := f.supervisor(0)
	// codex deliberately does not translate Invocation.ExtraDirs, so it could not
	// create the worktree the brief names - a paid session that fails at step 2.
	d.instances[0].cfg.Conflict = &config.ConflictStep{Harness: "codex", Model: config.ModelRoutes{"gpt-x"}}

	lines := f.run(d)

	if got := len(f.conflictCalls()); got != 0 {
		t.Errorf("conflict sessions = %d on a harness that cannot take the worktree grant, want none", got)
	}
	if got := f.callsFor("claim_task"); len(got) != 0 {
		t.Errorf("a refused harness still claimed the task: %v", got)
	}
	if !strings.Contains(lines, "does not translate ExtraDirs") {
		t.Errorf("the stand-down does not name the grant the harness cannot take:\n%s", lines)
	}
}

// The allowlist is inverted on purpose: a harness that translates ExtraDirs into
// a writable root has to be named deliberately, and copilot's --add-dir is one.
func TestConflictHarnessExtraDirAllowlist(t *testing.T) {
	for _, tc := range []struct {
		harness string
		want    bool
	}{
		{"opencode", true},
		{"claude", true},
		{"copilot", true},
		{"codex", false},
		{"", false},
	} {
		if got := conflictHarnessTakesExtraDirs(tc.harness); got != tc.want {
			t.Errorf("conflictHarnessTakesExtraDirs(%q) = %v, want %v", tc.harness, got, tc.want)
		}
	}
}

func TestConflictSessionRunsOneAtATime(t *testing.T) {
	f := conflictingFixture(t)
	// Two approved tasks in the queue, both conflicting. The second must wait for
	// the first session to end rather than starting beside it.
	f.plane.serveTool("list_tasks", fmt.Sprintf(
		`{"tasks":[{"id":%q,"ref":%q,"branch":%q,"repo":%q},{"id":%q,"ref":%q,"branch":%q,"repo":%q}],"count":2,"total":2,"view":"full"}`,
		closeOutTestTask, closeOutTestRef, closeOutTestHead, closeOutTestRepo,
		conflictTestTask2, conflictTestRef2, closeOutTestHead, closeOutTestRepo))

	release := make(chan struct{})
	f.setConflictRun(func(context.Context, *config.Config, string, ConflictRunRequest, config.ConflictStep) (harness.Result, error) {
		<-release
		return harness.Result{}, nil
	})

	d := f.supervisor(0)
	// The pass runs to completion with the first session still blocked, which is
	// exactly the window the second task's busy check has to see. Then the
	// session is released and the pass's wait collects it.
	f.runWith(d, func() { close(release) })

	if got := len(f.conflictCalls()); got != 1 {
		t.Errorf("conflict sessions = %d, want exactly one while the first was running", got)
	}
	if got := f.callsFor("claim_task"); len(got) != 1 {
		t.Errorf("claim_task calls = %d, want one (the second task waits): %v", len(got), got)
	}

	// Once the first session has ended, the second task is no longer held back:
	// the next poll reaches it. (task-1 leaves the approved queue because its
	// claim moved it to in_progress — the fake plane does not model that, so the
	// listing is updated to what the plane would serve.)
	f.plane.serveTool("list_tasks", fmt.Sprintf(
		`{"tasks":[{"id":%q,"ref":%q,"branch":%q,"repo":%q}],"count":1,"total":1,"view":"full"}`,
		conflictTestTask2, conflictTestRef2, closeOutTestHead, closeOutTestRepo))
	f.run(d)

	calls := f.conflictCalls()
	if len(calls) != 2 {
		t.Fatalf("conflict sessions after the first ended = %d, want 2", len(calls))
	}
	if calls[1].req.TaskID != conflictTestTask2 {
		t.Errorf("the second session was for %q, want the waiting task %q", calls[1].req.TaskID, conflictTestTask2)
	}
}

func TestConflictSessionSafetyNetReleasesAStrandedClaim(t *testing.T) {
	f := conflictingFixture(t)
	// The plane's REAL stranded shape since CLA-509: presence only, no run id. A
	// conflict session that dies before handing the task back leaves exactly this,
	// and the safety-net release must fire on presence alone - the supervisor
	// signs with the credential its own claim returned.
	f.plane.serveTool("get_task", fmt.Sprintf(
		`{"task":{"id":%q,"ref":%q,"status":"in_progress","heldByRun":true,"branch":%q,"outcome":"RESOLUTION: failed\n\n- b.txt cannot be resolved without guessing"}}`,
		closeOutTestTask, closeOutTestRef, closeOutTestHead))
	d := f.supervisor(0)

	lines := f.run(d)

	releases := f.callsFor("release_task")
	if len(releases) != 1 {
		t.Fatalf("release_task calls = %d, want exactly the safety-net release: %v", len(releases), releases)
	}
	if got := releases[0].args["driver"]; got != true {
		t.Errorf("the safety-net release does not carry the driver marker: %v", releases[0].args)
	}
	if got := releases[0].args["runId"]; got != closeOutTestRunCredential {
		t.Errorf("the release signs with runId %v, want the claim's credential %q", got, closeOutTestRunCredential)
	}
	if got := releases[0].args["checkGreen"]; got != false {
		t.Errorf("a failed resolution released with checkGreen=%v, want false (the merge never happened): %v", got, releases[0].args)
	}
	if strings.Contains(lines, "held by another run") {
		t.Errorf("our own stranded claim was misreported as another holder:\n%s", lines)
	}
	if !strings.Contains(lines, "outcome: failed") || !strings.Contains(lines, "claim released, task left approved") {
		t.Errorf("the failure line does not say the claim was released:\n%s", lines)
	}
}

// serveClaimOutcome rewrites the claim response so the conflict path takes the
// given outcome as its pre-session baseline. The plane serves the task view
// (outcome included) beside the run credential at the claim, and that view is the
// only copy of the pre-session text a holder can get (CLA-509).
func (f *closeOutFixture) serveClaimOutcome(outcome string) {
	f.plane.serveTool("claim_task", fmt.Sprintf(
		`{"task":{"id":%q,"outcome":%q},"run":{"id":%q}}`,
		closeOutTestTask, outcome, closeOutTestRunCredential))
}

// CLA-889: the brief tells the session to copy the stored outcome verbatim and
// append its block, so a re-offered task carries older attempts' markers ABOVE
// this session's. The safety-net release classifies only what THIS session wrote:
// an unchanged outcome is "no marker" (never an older attempt's attestation), and
// a changed one is read on its LAST marker.
func TestConflictSessionClassifiesOnlyThisSessionsWrite(t *testing.T) {
	const (
		oldMechanical = "The reviewed evidence.\n\nRESOLUTION: mechanical\n\n**Tests**\n- go test ./...: pass\n\n- a.txt: kept both sides"
		newFailed     = "RESOLUTION: failed\n\n- the free quota was exhausted before the merge"
		newMechanical = "RESOLUTION: mechanical\n\n**Tests**\n- go test ./...: pass\n\n- a.txt: kept both sides"
	)

	for _, tc := range []struct {
		name       string
		prior      string
		after      string
		checkGreen bool
		wantLog    []string
	}{
		{
			name:  "a dead retry writes nothing, so the older mechanical is not this session's attestation",
			prior: oldMechanical,
			after: oldMechanical,
			// The merge this session was to make never happened: a green
			// attestation would exempt the PR from the plane's repair ladder.
			checkGreen: false,
			wantLog:    []string{"outcome: failed", "the session left no resolution marker", "claim released, task left approved"},
		},
		{
			name:       "a session that wrote prose but no marker is still a failure",
			prior:      oldMechanical,
			after:      oldMechanical + "\n\nI could not finish.",
			checkGreen: false,
			wantLog:    []string{"outcome: failed", "the session left no resolution marker", "claim released, task left approved"},
		},
		{
			name:       "a new failed marker wins over the older mechanical",
			prior:      oldMechanical,
			after:      oldMechanical + "\n\n" + newFailed,
			checkGreen: false,
			wantLog:    []string{"outcome: failed", "the free quota was exhausted", "claim released, task left approved"},
		},
		{
			name:       "a new mechanical marker is the session's own green attestation",
			prior:      oldMechanical,
			after:      oldMechanical + "\n\n" + newMechanical,
			checkGreen: true,
			wantLog:    []string{"outcome: mechanical"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := conflictingFixture(t)
			f.serveClaimOutcome(tc.prior)
			// The stranded shape: the session died before handing the task back.
			f.plane.serveTool("get_task", fmt.Sprintf(
				`{"task":{"id":%q,"ref":%q,"status":"in_progress","heldByRun":true,"outcome":%q}}`,
				closeOutTestTask, closeOutTestRef, tc.after))
			d := f.supervisor(0)

			lines := f.run(d)

			releases := f.callsFor("release_task")
			if len(releases) != 1 {
				t.Fatalf("release_task calls = %d, want the one safety-net release: %v", len(releases), releases)
			}
			if got := releases[0].args["checkGreen"]; got != tc.checkGreen {
				t.Errorf("checkGreen = %v, want %v (only this session's marker may classify the release): %v", got, tc.checkGreen, releases[0].args)
			}
			for _, want := range tc.wantLog {
				if !strings.Contains(lines, want) {
					t.Errorf("the log does not say %q:\n%s", want, lines)
				}
			}
		})
	}
}

// The exact CLA-884 shape: a conflict session dies without writing anything, so
// the outcome carries no RESOLUTION marker at all. The task is still in_progress
// and held, and the safety-net release must fire and say it handed the claim back.
func TestConflictSessionNoMarkerStillReleasesTheStrandedClaim(t *testing.T) {
	f := conflictingFixture(t)
	f.plane.serveTool("get_task", fmt.Sprintf(
		`{"task":{"id":%q,"ref":%q,"status":"in_progress","heldByRun":true,"outcome":"I looked at the conflict."}}`,
		closeOutTestTask, closeOutTestRef))
	d := f.supervisor(0)

	lines := f.run(d)

	releases := f.callsFor("release_task")
	if len(releases) != 1 {
		t.Fatalf("release_task calls = %d, want exactly the safety-net release: %v", len(releases), releases)
	}
	if got := releases[0].args["checkGreen"]; got != false {
		t.Errorf("a no-marker session released with checkGreen=%v, want false (the merge never happened): %v", got, releases[0].args)
	}
	for _, want := range []string{"the session left no resolution marker", "claim released, task left approved"} {
		if !strings.Contains(lines, want) {
			t.Errorf("the incident-shaped log does not say %q:\n%s", want, lines)
		}
	}
}

func TestConflictSessionLogsAReleaseRefusedAsNotHolding(t *testing.T) {
	f := conflictingFixture(t)
	// The task is still in_progress and held, but the release is refused as
	// not-holding: our lease lapsed and another run took it. The refusal is the
	// only holder signal the plane serves now, and it must be reported as such.
	f.plane.serveTool("get_task", fmt.Sprintf(
		`{"task":{"id":%q,"ref":%q,"status":"in_progress","heldByRun":true,"outcome":"RESOLUTION: failed\n\n- b.txt"}}`,
		closeOutTestTask, closeOutTestRef))
	f.plane.refuseTool("release_task", `{"error":{"code":"not_holding","message":"release ends a held claim, but the run holding it is already over"}}`)
	d := f.supervisor(0)

	lines := f.run(d)

	if got := len(f.callsFor("release_task")); got != 1 {
		t.Fatalf("release_task calls = %d, want the one refused safety-net release", got)
	}
	if !strings.Contains(lines, "held by another run") {
		t.Errorf("a not-holding refusal was not logged as the task being held by another run:\n%s", lines)
	}
	if strings.Contains(lines, "claim released") {
		t.Errorf("a refused release was reported as a hand-back:\n%s", lines)
	}
}

func TestConflictSessionSkippedWhenTheTaskIsClaimed(t *testing.T) {
	f := conflictingFixture(t)
	f.plane.refuseTool("claim_task", "task is held by another run")
	d := f.supervisor(0)

	lines := f.run(d)

	if got := len(f.conflictCalls()); got != 0 {
		t.Errorf("conflict sessions = %d for a task already claimed, want none", got)
	}
	if !strings.Contains(lines, "claim was refused") {
		t.Errorf("the pass did not say the claim was refused:\n%s", lines)
	}
}

func TestConflictSessionRefusesClaude(t *testing.T) {
	f := conflictingFixture(t)
	d := f.supervisor(0)
	d.instances[0].cfg.Conflict = &config.ConflictStep{Harness: "claude"}

	lines := f.run(d)

	if got := f.callsFor("claim_task"); len(got) != 0 {
		t.Errorf("a Claude conflict session claimed the task: %v", got)
	}
	if got := len(f.conflictCalls()); got != 0 {
		t.Errorf("conflict sessions = %d on Claude, want none", got)
	}
	if !strings.Contains(lines, "never run on Claude") {
		t.Errorf("the stand-down does not name the Claude rule:\n%s", lines)
	}
}

func TestConflictSessionMechanicalLeavesTheTaskApproved(t *testing.T) {
	f := conflictingFixture(t)
	d := f.supervisor(0)

	lines := f.run(d)

	if got := f.callsFor("release_task"); len(got) != 0 {
		t.Errorf("a mechanical hand-back was repaired with release_task: %v (the session released the claim itself)", got)
	}
	if !strings.Contains(lines, "outcome: mechanical") {
		t.Errorf("the pass did not log the mechanical outcome:\n%s", lines)
	}
	if !strings.Contains(lines, "a.txt, b.txt") {
		t.Errorf("the outcome line does not name the conflicting paths:\n%s", lines)
	}
}

func TestConflictSessionBehaviouralMovesTheTaskBackToReview(t *testing.T) {
	f := conflictingFixture(t)
	f.plane.serveTool("get_task", fmt.Sprintf(
		`{"task":{"id":%q,"ref":%q,"status":"in_review","heldByRun":false,"outcome":"RESOLUTION: behavioural\n\n**Tests**\n- go test ./...: pass\n\n- a.txt: kept the feature's field and the base's default"}}`,
		closeOutTestTask, closeOutTestRef))
	d := f.supervisor(0)

	lines := f.run(d)

	if got := f.callsFor("release_task"); len(got) != 0 {
		t.Errorf("a behavioural hand-back was released a second time: %v", got)
	}
	for _, want := range []string{"outcome: behavioural", "kept the feature's field"} {
		if !strings.Contains(lines, want) {
			t.Errorf("the outcome line does not say %q:\n%s", want, lines)
		}
	}
}

func TestConflictSessionFailureReleasesTheClaim(t *testing.T) {
	f := conflictingFixture(t)
	f.plane.serveTool("get_task", fmt.Sprintf(
		`{"task":{"id":%q,"ref":%q,"status":"in_progress","heldByRun":true,"outcome":"RESOLUTION: failed\n\n- b.txt cannot be resolved without guessing"}}`,
		closeOutTestTask, closeOutTestRef))
	d := f.supervisor(0)

	lines := f.run(d)

	releases := f.callsFor("release_task")
	if len(releases) != 1 {
		t.Fatalf("release_task calls = %d, want exactly the safety-net release: %v", len(releases), releases)
	}
	if got := releases[0].args["driver"]; got != true {
		t.Errorf("the safety-net release does not carry the driver marker: %v", releases[0].args)
	}
	if got := releases[0].args["runId"]; got != closeOutTestRunCredential {
		t.Errorf("the release signs with runId %v, want the claim's credential %q", got, closeOutTestRunCredential)
	}
	for _, want := range []string{"outcome: failed", "cannot be resolved without guessing"} {
		if !strings.Contains(lines, want) {
			t.Errorf("the failure line does not say %q:\n%s", want, lines)
		}
	}
}

func TestConflictSessionReleasesWhenThePlaneCannotBeRead(t *testing.T) {
	f := conflictingFixture(t)
	f.plane.refuseTool("get_task", "plane read failed")
	d := f.supervisor(0)

	lines := f.run(d)

	if got := f.callsFor("release_task"); len(got) != 1 {
		t.Fatalf("release_task calls = %d, want the safety-net release: %v", len(got), got)
	}
	if got := f.callsFor("release_task")[0].args["driver"]; got != true {
		t.Errorf("the safety-net release does not carry the driver marker: %v", got)
	}
	if !strings.Contains(lines, "outcome: failed") || !strings.Contains(lines, "claim was released") {
		t.Errorf("the failure line does not say what happened:\n%s", lines)
	}
}

func TestConflictSessionIsBounded(t *testing.T) {
	f := conflictingFixture(t)
	d := f.supervisor(0)

	f.run(d)
	f.run(d)
	lines := f.run(d)

	if got := len(f.conflictCalls()); got != maxConflictAttempts {
		t.Errorf("conflict sessions = %d, want the bound %d at one unchanged head", got, maxConflictAttempts)
	}
	if !strings.Contains(lines, "leaving it to the agent close-out") {
		t.Errorf("the exhausted bound does not say it falls back to the agent close-out:\n%s", lines)
	}
}

func TestConflictInvocationComposesTheSession(t *testing.T) {
	cfg := &config.Config{
		Harness:             "opencode",
		Model:               "run-model",
		MaxTurns:            150,
		MaxSessionWallClock: config.Duration(20 * time.Minute),
		// Not .mcp.json: that name is gated out of opencode's hand-off (CLA-318).
		Projects: []config.Project{{Slug: "acme", MCPConfigPath: "/fleet/acme/opencode-mcp.json"}},
	}
	step := config.ConflictStep{Harness: "opencode", Model: config.ModelRoutes{"conflict-model"}, MaxTurns: 40}
	req := ConflictRunRequest{
		Harness:  "opencode",
		Model:    "conflict-model",
		Routes:   []string{"conflict-model"},
		WorkDir:  "/fleet/acme/widgets",
		Worktree: "/fleet/acme/worktrees/conflict-t1",
		Prompt:   "the brief",
	}

	inv, err := conflictInvocation(cfg, "acme", req, step)
	if err != nil {
		t.Fatalf("conflictInvocation: %v", err)
	}
	if inv.Prompt != "the brief" || inv.Model != "conflict-model" || inv.WorkDir != "/fleet/acme/widgets" {
		t.Errorf("invocation = %+v, want the request's prompt/model/workdir", inv)
	}
	if !slices.Equal(inv.Routes, []string{"conflict-model"}) {
		t.Errorf("invocation routes = %v, want the step's route list handed to the adapter", inv.Routes)
	}
	if inv.MCPConfigPath != "/fleet/acme/opencode-mcp.json" {
		t.Errorf("MCP config = %q, want the project's own file", inv.MCPConfigPath)
	}
	cfg.Projects[0].MCPConfigPath = "/fleet/acme/.mcp.json"
	if gated, err := conflictInvocation(cfg, "acme", req, step); err != nil || gated.MCPConfigPath != "" {
		t.Errorf("opencode conflict session handed %q (err %v), want \"\" for a Claude-shaped .mcp.json", gated.MCPConfigPath, err)
	}
	if len(inv.ExtraDirs) != 1 || inv.ExtraDirs[0] != "/fleet/acme/worktrees" {
		t.Errorf("ExtraDirs = %v, want the worktree area granted", inv.ExtraDirs)
	}
	if inv.MaxTurns != 40 {
		t.Errorf("MaxTurns = %d, want the conflict step's 40", inv.MaxTurns)
	}
	if inv.MaxSessionWallClock != 20*time.Minute {
		t.Errorf("MaxSessionWallClock = %s, want the config's 20m", inv.MaxSessionWallClock)
	}
}

func TestConflictWorktreeFollowsTheProjectLayout(t *testing.T) {
	const dir = "/fleet/acme/widgets"
	if got, want := conflictWorktree(&config.Config{ProjectRoot: true}, dir, "t1"), "/fleet/acme/worktrees/conflict-t1"; got != want {
		t.Errorf("project-root worktree = %q, want %q", got, want)
	}
	if got, want := conflictWorktree(&config.Config{}, dir, "t1"), "/fleet/acme/widgets-wt/conflict-t1"; got != want {
		t.Errorf("operator-checkout worktree = %q, want %q", got, want)
	}
}

func TestConflictSessionGateFollowsTheCheckout(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		want string
	}{
		{"go repo", "go.mod", "go test ./..."},
		{"js repo", "package.json", "pnpm test"},
		{"unknown repo", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := conflictingFixture(t)
			if tc.file != "" {
				writeFile(t, f.dir+"/"+tc.file, "x\n")
			}
			d := f.supervisor(0)
			f.run(d)

			calls := f.conflictCalls()
			if len(calls) != 1 {
				t.Fatalf("conflict sessions = %d, want one", len(calls))
			}
			if calls[0].req.Gate != tc.want {
				t.Errorf("gate = %q, want %q", calls[0].req.Gate, tc.want)
			}
			if tc.want != "" && !strings.Contains(calls[0].req.Prompt, tc.want) {
				t.Errorf("the brief does not name the gate %q:\n%s", tc.want, calls[0].req.Prompt)
			}
		})
	}
}

// The fleet's conflict-session slot (CLA-759 review 1.review-2.1 / 3.1 / 4.1 /
// 1.5). The per-Supervisor atomic cannot bound the fleet: these pin the SHARED
// lease that does.

func TestConflictSlotIsSharedAcrossSupervisors(t *testing.T) {
	dir := t.TempDir()
	d1 := &Supervisor{cacheDir: dir}
	d2 := &Supervisor{cacheDir: dir}

	first, err := d1.acquireConflictSlot("host=a pid=1 task=t1")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if path := d1.conflictSlotPath(); conflictSlotHolder(path) == "" || !strings.Contains(conflictSlotHolder(path), "task=t1") {
		t.Errorf("the slot's record does not name its holder (%q)", conflictSlotHolder(path))
	}

	second, err := d2.acquireConflictSlot("host=a pid=2 task=t2")
	if !errors.Is(err, errConflictSlotHeld) {
		t.Fatalf("a second supervisor acquired the slot the first holds: slot=%v err=%v", second, err)
	}
	if second != nil {
		t.Errorf("a refused acquisition returned a slot: %+v", second)
	}

	first.release()
	second, err = d2.acquireConflictSlot("host=a pid=2 task=t2")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	second.release()
}

func TestConflictSlotCannotBeTakenWithoutAStateRoot(t *testing.T) {
	d := &Supervisor{}
	if _, err := d.acquireConflictSlot("host=a pid=1 task=t1"); err == nil {
		t.Fatal("a supervisor with no state root acquired the fleet's slot - the bound must fail CLOSED")
	} else if errors.Is(err, errConflictSlotHeld) {
		t.Fatalf("a missing state root was reported as another holder: %v", err)
	}
}

func TestConflictSlotIgnoresAStaleHolderRecord(t *testing.T) {
	dir := t.TempDir()
	d := &Supervisor{cacheDir: dir}
	// A dead holder's record is left in the file. The LOCK decides who holds the
	// slot, never the file's contents: the kernel released the dead holder's lock,
	// which is this design's stale-holder recovery.
	if err := os.WriteFile(d.conflictSlotPath(), []byte("host=ghost pid=999999 task=dead acquired 2020-01-01T00:00:00Z\n"), 0o644); err != nil {
		t.Fatalf("seed a stale record: %v", err)
	}

	slot, err := d.acquireConflictSlot("host=live pid=1 task=t1")
	if err != nil {
		t.Fatalf("a stale record blocked acquisition: %v", err)
	}
	if rec := conflictSlotHolder(d.conflictSlotPath()); !strings.Contains(rec, "host=live") {
		t.Errorf("the holder record was not replaced (%q)", rec)
	}
	slot.release()
}

func TestConflictSessionsAreSerialisedAcrossSupervisors(t *testing.T) {
	f := conflictingFixture(t)
	release := make(chan struct{})
	f.setConflictRun(func(context.Context, *config.Config, string, ConflictRunRequest, config.ConflictStep) (harness.Result, error) {
		<-release
		return harness.Result{}, nil
	})

	d1 := f.supervisor(0)
	d2 := f.supervisor(0)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	d1.reconcileCloseOuts() // starts a session, which blocks on release
	d2.reconcileCloseOuts() // must find the fleet's slot held and start nothing

	if got := len(f.conflictCalls()); got != 1 {
		t.Errorf("conflict sessions while the first runs = %d, want exactly one ACROSS BOTH supervisors", got)
	}
	if got := f.callsFor("claim_task"); len(got) != 1 {
		t.Errorf("claim_task calls = %d, want one: the waiting supervisor must cost no plane write: %v", len(got), got)
	}
	if !strings.Contains(buf.String(), "holds the fleet's conflict-session slot") {
		t.Errorf("the second supervisor did not say the fleet's slot is held:\n%s", buf.String())
	}

	close(release)
	d1.waitConflict()

	// The first session has ended, so the slot is free: the next pass starts one.
	f.setConflictRun(nil)
	d2.reconcileCloseOuts()
	d2.waitConflict()
	if got := len(f.conflictCalls()); got != 2 {
		t.Errorf("conflict sessions after the first ended = %d, want 2 (the slot was released)", got)
	}
}

func TestConflictSessionSkippedWhenTheConflictIsStale(t *testing.T) {
	// The branches touch different files, so the fetched tips merge CLEANLY - every
	// bit of evidence that the PR's CONFLICTING read is stale, exactly the state a
	// mechanical resolution leaves behind (it pushes a new head and the task stays
	// approved). A session here would be a second paid attempt at no conflict.
	f := newCloseOutFixture(t, false)
	f.servePlane(t, "clanker")
	f.writePR(closeOutTestBase, "CONFLICTING", "DIRTY",
		`[{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"}]`)
	d := f.supervisor(0)

	lines := f.run(d)

	if got := len(f.conflictCalls()); got != 0 {
		t.Errorf("conflict sessions = %d for a stale CONFLICTING read, want none", got)
	}
	if got := f.callsFor("claim_task"); len(got) != 0 {
		t.Errorf("a stale conflict was claimed: %v", got)
	}
	if !strings.Contains(lines, "the conflict state is stale, so no conflict session was started") {
		t.Errorf("the pass does not say why no session was started:\n%s", lines)
	}
}

// conflictQuotaEvent is the CLA-884 shape: opencode's terminal provider error
// event for the exhausted free route. The REAL opencode adapter classifies it
// below, so the ladder is pinned to the same scan the phase path uses rather
// than to a test-private notion of "quota".
const conflictQuotaEvent = `{"type":"error","error":{"type":"provider.quota","message":"Free usage limit reached"}}`

// conflictLimitScan is the ladder's one adapter dependency, answered by the real
// opencode adapter.
type conflictLimitScan struct{}

func (conflictLimitScan) DetectLimit(res harness.Result) harness.Limit {
	a, err := harness.Get("opencode")
	if err != nil {
		panic(err)
	}
	return a.DetectLimit(res)
}

// TestConflictModelLadderFallsBackOnAUsageLimit is CLA-890's bar: a conflict
// session whose first model ends on a quota result is re-run on the next model in
// the list. The retry is offered only the routes still left, so the adapter's own
// mid-session switch cannot be handed a route behind the one it is on.
func TestConflictModelLadderFallsBackOnAUsageLimit(t *testing.T) {
	req := ConflictRunRequest{
		PR:     "#12",
		Model:  "opencode/deepseek-v4.1-flash-free",
		Routes: []string{"opencode/deepseek-v4.1-flash-free", "opencode-go/deepseek-v4.1-flash"},
	}
	var ran []string
	var offered [][]string

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	res, err := runConflictModelLadder("CLA-2", req, conflictLimitScan{}, func(model string, routes []string) (harness.Result, error) {
		ran = append(ran, model)
		offered = append(offered, routes)
		if model == req.Routes[0] {
			return harness.Result{ExitCode: 1, Stdout: conflictQuotaEvent}, nil
		}
		return harness.Result{FinalMessage: "conflict CLA-2: mechanical"}, nil
	})
	if err != nil {
		t.Fatalf("ladder: %v", err)
	}
	if want := []string{req.Routes[0], req.Routes[1]}; !slices.Equal(ran, want) {
		t.Errorf("models run = %v, want the free route then its paid twin %v", ran, want)
	}
	if len(offered) != 2 || !slices.Equal(offered[0], req.Routes) {
		t.Errorf("attempt route lists = %v, want the first offered the whole list", offered)
	}
	if len(offered) != 2 || !slices.Equal(offered[1], []string{req.Routes[1]}) {
		t.Errorf("attempt route lists = %v, want the retry offered only the routes still left", offered)
	}
	if res.FinalMessage != "conflict CLA-2: mechanical" {
		t.Errorf("final result = %+v, want the second model's clean session", res)
	}
	for _, want := range []string{
		"attempt on " + req.Routes[0] + " (route 1 of 2)",
		"attempt on " + req.Routes[1] + " (route 2 of 2)",
		req.Routes[0] + " hit a usage limit",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the attempt log does not say %q:\n%s", want, buf.String())
		}
	}
}

// A pinned single `conflict.model` must run only that model: when it is the last
// model in the list there is no fallback, and the limit stands for the failure
// classification that already exists.
func TestConflictModelLadderStopsAtAPinnedSingleModel(t *testing.T) {
	req := ConflictRunRequest{PR: "#12", Model: "opencode-go/paid", Routes: []string{"opencode-go/paid"}}
	var ran []string

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	res, err := runConflictModelLadder("CLA-2", req, conflictLimitScan{}, func(model string, routes []string) (harness.Result, error) {
		ran = append(ran, model)
		return harness.Result{ExitCode: 1, Stdout: conflictQuotaEvent}, nil
	})
	if err != nil {
		t.Fatalf("ladder: %v", err)
	}
	if !slices.Equal(ran, []string{"opencode-go/paid"}) {
		t.Errorf("models run = %v, want only the pinned model", ran)
	}
	if lim := (conflictLimitScan{}).DetectLimit(res); !lim.Limited {
		t.Errorf("the ladder returned %+v, want the pinned model's limited result unchanged", res)
	}
	if !strings.Contains(buf.String(), "it is the last model in the list") {
		t.Errorf("the ladder did not say the pinned model was the last one:\n%s", buf.String())
	}
}

// A clean attempt ends the ladder: the second model is not a retry budget, only
// a fallback for a limit.
func TestConflictModelLadderKeepsAFirstModelsCleanResult(t *testing.T) {
	req := ConflictRunRequest{PR: "#12", Model: "free", Routes: []string{"free", "paid"}}
	var ran []string

	res, err := runConflictModelLadder("CLA-2", req, conflictLimitScan{}, func(model string, routes []string) (harness.Result, error) {
		ran = append(ran, model)
		return harness.Result{FinalMessage: "conflict CLA-2: mechanical"}, nil
	})
	if err != nil {
		t.Fatalf("ladder: %v", err)
	}
	if !slices.Equal(ran, []string{"free"}) {
		t.Errorf("models run = %v, want one clean attempt", ran)
	}
	if res.FinalMessage != "conflict CLA-2: mechanical" {
		t.Errorf("result = %+v, want the clean first attempt", res)
	}
}

// No model anywhere is ONE attempt on the harness's own default, not zero.
func TestConflictModelLadderRunsOnceOnTheHarnessDefault(t *testing.T) {
	var ran []string
	if _, err := runConflictModelLadder("CLA-2", ConflictRunRequest{PR: "#12"}, conflictLimitScan{}, func(model string, routes []string) (harness.Result, error) {
		ran = append(ran, model)
		return harness.Result{}, nil
	}); err != nil {
		t.Fatalf("ladder: %v", err)
	}
	if !slices.Equal(ran, []string{""}) {
		t.Errorf("models run = %v, want one attempt with no model named", ran)
	}
}

// The adapter may have switched routes mid-session and hit the limit on a LATER
// one. The ladder then skips past the route that actually ended the session,
// rather than re-running a route just proven exhausted.
func TestConflictModelLadderSkipsTheRouteTheSessionEndedOn(t *testing.T) {
	const free, paid, third = "free", "paid", "third"
	req := ConflictRunRequest{PR: "#12", Model: free, Routes: []string{free, paid, third}}
	var ran []string
	var offered [][]string

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	res, err := runConflictModelLadder("CLA-2", req, conflictLimitScan{}, func(model string, routes []string) (harness.Result, error) {
		ran = append(ran, model)
		offered = append(offered, routes)
		if model == free {
			return harness.Result{ExitCode: 1, Stdout: conflictQuotaEvent, Raw: map[string]any{
				harness.RouteFinishedKey: paid,
			}}, nil
		}
		return harness.Result{FinalMessage: "conflict CLA-2: mechanical"}, nil
	})
	if err != nil {
		t.Fatalf("ladder: %v", err)
	}
	if want := []string{free, third}; !slices.Equal(ran, want) {
		t.Errorf("models run = %v, want %v (the paid route the session already exhausted is skipped)", ran, want)
	}
	if len(offered) != 2 || !slices.Equal(offered[1], []string{third}) {
		t.Errorf("attempt route lists = %v, want the retry offered only %q", offered, third)
	}
	if res.FinalMessage != "conflict CLA-2: mechanical" {
		t.Errorf("final result = %+v, want the third route's clean session", res)
	}
	if !strings.Contains(buf.String(), paid+" hit a usage limit") {
		t.Errorf("the attempt log does not name the route that hit the limit:\n%s", buf.String())
	}
}

// The adapter's own mid-session switch can finish the job: the ladder must not
// retry then, and the log must name the route that actually did the work rather
// than only the one the attempt started on.
func TestConflictModelLadderNamesTheRouteASwitchFinishedOn(t *testing.T) {
	const free, paid = "free", "paid"
	var ran []string

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	res, err := runConflictModelLadder("CLA-2", ConflictRunRequest{PR: "#12", Model: free, Routes: []string{free, paid}}, conflictLimitScan{}, func(model string, routes []string) (harness.Result, error) {
		ran = append(ran, model)
		return harness.Result{Raw: map[string]any{
			harness.RouteFinishedKey: paid,
		}}, nil
	})
	if err != nil {
		t.Fatalf("ladder: %v", err)
	}
	if !slices.Equal(ran, []string{free}) {
		t.Errorf("models run = %v, want one attempt: the switch already finished the session", ran)
	}
	if got := (conflictLimitScan{}).DetectLimit(res); got.Limited {
		t.Errorf("the ladder returned a limited result for a clean finish: %+v", got)
	}
	if !strings.Contains(buf.String(), "finished on "+paid) {
		t.Errorf("the attempt log does not name the route the switch finished on:\n%s", buf.String())
	}
}
