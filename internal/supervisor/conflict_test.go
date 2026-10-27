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
	d.instances[0].cfg.Conflict = &config.ConflictStep{Model: "gpt-x", MaxTurns: 40}

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
	d.instances[0].cfg.Conflict = &config.ConflictStep{Harness: "codex", Model: "gpt-x"}

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

func TestConflictSessionSafetyNetReleaseMatchesTheBareHolderUUID(t *testing.T) {
	f := conflictingFixture(t)
	// The claim hands the supervisor a run CREDENTIAL; the plane records the BARE
	// uuid in `claimedByRun`. The safety-net release must recognise its own run
	// across that difference (CLA-759 review 1.review-1.1) - and must NOT report the
	// holder as "another run" when it is our own stopped one.
	f.plane.serveTool("get_task", fmt.Sprintf(
		`{"task":{"id":%q,"ref":%q,"status":"approved","claimedByRun":%q,"outcome":"RESOLUTION: failed\n\n- b.txt cannot be resolved without guessing"}}`,
		closeOutTestTask, closeOutTestRef, closeOutTestRunUUID))
	d := f.supervisor(0)

	lines := f.run(d)

	releases := f.callsFor("release_task")
	if len(releases) != 1 {
		t.Fatalf("release_task calls = %d, want exactly the safety-net release (the holder is our own bare uuid): %v", len(releases), releases)
	}
	if got := releases[0].args["driver"]; got != true {
		t.Errorf("the safety-net release does not carry the driver marker: %v", releases[0].args)
	}
	if got := releases[0].args["checkGreen"]; got != false {
		t.Errorf("a failed resolution released with checkGreen=%v, want false (the merge never happened): %v", got, releases[0].args)
	}
	if strings.Contains(lines, "held by another run") {
		t.Errorf("our own stopped run was misreported as another holder:\n%s", lines)
	}
}

func TestConflictSessionLeavesAClaimAnotherRunHolds(t *testing.T) {
	f := conflictingFixture(t)
	// A DIFFERENT run's bare uuid: the task's lease lapsed and someone else took it.
	// Nothing of ours is signable any more, so no release may be sent under our id.
	f.plane.serveTool("get_task", fmt.Sprintf(
		`{"task":{"id":%q,"ref":%q,"status":"approved","claimedByRun":"some-other-run","outcome":"RESOLUTION: failed\n\n- b.txt"}}`,
		closeOutTestTask, closeOutTestRef))
	d := f.supervisor(0)

	lines := f.run(d)

	if got := f.callsFor("release_task"); len(got) != 0 {
		t.Errorf("a task held by another run was released under our id: %v", got)
	}
	if !strings.Contains(lines, "held by another run (some-other-run)") {
		t.Errorf("the pass does not name the other holder:\n%s", lines)
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
		`{"task":{"id":%q,"ref":%q,"status":"in_review","claimedByRun":null,"outcome":"RESOLUTION: behavioural\n\n**Tests**\n- go test ./...: pass\n\n- a.txt: kept the feature's field and the base's default"}}`,
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
		`{"task":{"id":%q,"ref":%q,"status":"approved","claimedByRun":"run-1","outcome":"RESOLUTION: failed\n\n- b.txt cannot be resolved without guessing"}}`,
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
