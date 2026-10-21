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
	"context"
	"fmt"
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
	if call.req.RunID != "run-1" {
		t.Errorf("the session was briefed with run %q, want the claim's run-1", call.req.RunID)
	}
	if !strings.Contains(call.req.Worktree, "conflict-"+closeOutTestTask) {
		t.Errorf("the worktree path %q does not name the task", call.req.Worktree)
	}
	if !strings.Contains(call.req.Prompt, call.req.Worktree) {
		t.Errorf("the brief does not name the worktree %q:\n%s", call.req.Worktree, call.req.Prompt)
	}
	for _, want := range []string{closeOutTestTask, closeOutTestRef, "run-1", closeOutTestHead, closeOutTestBase, "#12", "a.txt", "b.txt"} {
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
	if call.step.Harness != "opencode" || call.step.Model != "implement-model" {
		t.Errorf("the resolved step = %+v, want the implement step", call.step)
	}
	if !strings.Contains(lines, "outcome: mechanical") {
		t.Errorf("the pass did not log the session's outcome:\n%s", lines)
	}
}

func TestConflictSessionUsesItsOwnStep(t *testing.T) {
	f := conflictingFixture(t)
	d := f.supervisor(0)
	d.instances[0].cfg.Conflict = &config.ConflictStep{Harness: "codex", Model: "gpt-x", MaxTurns: 40}

	f.run(d)

	calls := f.conflictCalls()
	if len(calls) != 1 {
		t.Fatalf("conflict sessions = %d, want one", len(calls))
	}
	if calls[0].req.Harness != "codex" || calls[0].req.Model != "gpt-x" {
		t.Errorf("the session runs on %s/%s, want the conflict step's codex/gpt-x", calls[0].req.Harness, calls[0].req.Model)
	}
	if calls[0].step.MaxTurns != 40 {
		t.Errorf("max turns = %d, want the conflict step's 40", calls[0].step.MaxTurns)
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

	if got := f.callsFor("update_task"); len(got) != 0 {
		t.Errorf("a mechanical hand-back was repaired with update_task: %v (the session released the claim itself)", got)
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
		`{"task":{"id":%q,"ref":%q,"status":"in_review","claimedByRun":null,"outcome":"RESOLUTION: behavioural\n\n**Tests**\n- go test ./...: pass\n\n- a.txt: kept the feature's field and the base's default"}}`,
		closeOutTestTask, closeOutTestRef))
	d := f.supervisor(0)

	lines := f.run(d)

	if got := f.callsFor("update_task"); len(got) != 0 {
		t.Errorf("a behavioural hand-back was written a second time: %v", got)
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
		`{"task":{"id":%q,"ref":%q,"status":"approved","claimedByRun":"run-1","outcome":"RESOLUTION: failed\n\n- b.txt cannot be resolved without guessing"}}`,
		closeOutTestTask, closeOutTestRef))
	d := f.supervisor(0)

	lines := f.run(d)

	releases := f.callsFor("update_task")
	if len(releases) != 1 {
		t.Fatalf("update_task calls = %d, want exactly the safety-net release: %v", len(releases), releases)
	}
	if got := releases[0].args["release"]; got != true {
		t.Errorf("the repair call is not a release: %v", releases[0].args)
	}
	if got := releases[0].args["runId"]; got != "run-1" {
		t.Errorf("the release signs with runId %v, want run-1", got)
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

	if got := f.callsFor("update_task"); len(got) != 1 {
		t.Fatalf("update_task calls = %d, want the safety-net release: %v", len(got), got)
	}
	if got := f.callsFor("update_task")[0].args["release"]; got != true {
		t.Errorf("the repair call is not a release: %v", got)
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
		Projects:            []config.Project{{Slug: "acme", MCPConfigPath: "/fleet/acme/.mcp.json"}},
	}
	step := config.ConflictStep{Harness: "opencode", Model: "conflict-model", MaxTurns: 40}
	req := ConflictRunRequest{
		Harness:  "opencode",
		Model:    "conflict-model",
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
	if inv.MCPConfigPath != "/fleet/acme/.mcp.json" {
		t.Errorf("MCP config = %q, want the project's own file", inv.MCPConfigPath)
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
