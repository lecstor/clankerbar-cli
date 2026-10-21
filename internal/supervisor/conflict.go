package supervisor

// The conflict-only session (CLA-759).
//
// CLA-758's deterministic close-out integrates a CLEAN approved task with no agent
// session. A CONFLICTING one deliberately stood down and left the task for the
// agent close-out: the next clanker to finish the task it was on ran a FULL
// implement-phase session, with the whole drain protocol in its brief, to resolve
// a merge conflict. Removing known conflict sources is worth doing, but conflicts
// will always happen, so the fleet needs a fast, narrow way to handle them.
//
// This file starts that session. On the CONFLICTING stand-down (and only there)
// the supervisor:
//
//  1. takes the plane CLAIM itself, so the claim is the lock an agent close-out
//     and this path both race for. A refused claim means someone else has the
//     task: no session is started.
//  2. starts ONE session, on the fleet's IMPLEMENT harness/model unless the
//     config's `conflict` step names its own, and logs one line with the task,
//     the PR, the conflicting paths and the outcome.
//  3. lets the session do the work under that claim: merge the base into the
//     branch (never a rebase), resolve only the conflicting hunks, run the
//     repo-wide gate, push, and hand the task back. The brief is short and lives
//     in internal/config (config.ConflictBrief), beside every other brief.
//  4. after the session ends, reads the task back and makes the hand-back true:
//     a task the session left approved is the MECHANICAL outcome (resolved and
//     released for CLA-758 to merge once CI is green); one it moved to
//     `in_review` is BEHAVIOURAL (the resolution changed what the code does, so
//     it has not been reviewed); anything still held by this run is FAILED — the
//     claim is released and the task left approved, with the reason logged.
//
// At most ONE conflict session runs at a time across the fleet: the Supervisor's
// own atomic is only a fast in-process gate, and the bound that holds across
// supervisors is a shared, durable SLOT taken before the claim (conflictlock.go) —
// one file under the fleet's state root, locked exclusively while a session runs,
// released by the kernel if the holder dies. A second conflicting task (on this
// supervisor or any other sharing that state root) waits for the next poll, and
// the claim is what makes "a task already claimed is skipped" safe even then.
// Daemon-started sessions never run on Claude (operator rule), so a step resolving
// to Claude stands down loudly instead of spending a session.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

const (
	// conflictSessionBudget bounds ONE conflict-only session by wall clock. A
	// conflict resolution is a small job — a merge, a handful of hunks and one
	// gate — so the budget is generous rather than tight; its purpose is to
	// stop a wedged session holding the task's claim indefinitely, not to cut an
	// honest run short. (The harness's own per-session ceiling still applies
	// inside it, from the config.)
	conflictSessionBudget = 45 * time.Minute

	// conflictCallTimeout bounds one plane call the conflict path makes around
	// the session (the claim, the state read, the safety-net release). It is the
	// same budget the close-out gives a non-judgement call.
	conflictCallTimeout = closeOutCallTimeout

	// maxConflictAttempts bounds how many paid sessions one task may burn in this
	// supervisor's lifetime. A failed session leaves the task approved, so
	// without a bound the next poll would start another, and another: the exact
	// opposite of "the agent close-out remains the fallback". A task that keeps
	// failing falls back to the agent close-out, which is the designed
	// fallback — not a third paid experiment.
	maxConflictAttempts = 2
)

// conflictAttempt is how many conflict sessions one task has spent in this
// supervisor's lifetime.
type conflictAttempt struct {
	n int
}

// ConflictRunRequest is everything one conflict-only session is handed. The
// prompt is the rendered config.ConflictBrief; the rest is passed so the session
// launcher needs no second source of truth and a test can assert exactly what the
// fleet would have spawned.
type ConflictRunRequest struct {
	TaskID  string
	TaskRef string
	RunID   string
	Branch  string
	Base    string
	PR      string // "#12"
	Repo    string
	Paths   []string
	Gate    string
	Harness string
	Model   string
	WorkDir string
	// Worktree is the exact path the session creates its branch worktree at. The
	// supervisor names it (rather than letting the session choose) so it can
	// grant that directory into the session's permission scope.
	Worktree string
	Prompt   string
}

// startConflictSession takes the claim for one CONFLICTING approved task and
// launches the session that resolves it.
//
// It runs on the close-out pass goroutine, so the plane claim (fast) is made here
// and the session itself (slow) is detached onto its own goroutine. It is called
// only from the PRConflict stand-down, with the paths the evidence pass computed.
func (d *Supervisor) startConflictSession(ctx context.Context, cfg *config.Config, slug string, client plane.CloseOutAPI, task plane.CloseOutTask, pr int, base string, paths []string, dir string) {
	label := strings.TrimSpace(task.Ref)
	if label == "" {
		label = task.ID
	}

	// One at a time ON THIS SUPERVISOR. A second conflicting task is not dropped:
	// it stays approved, and the pass reaches it again on the next poll once this
	// session ends. The bound ACROSS supervisors is the shared slot below.
	if !d.conflictBusy.CompareAndSwap(false, true) {
		d.conflictSay("busy:"+task.ID, "conflict: %s PR #%d is CONFLICTING, but a conflict session is already running - "+
			"this task waits for the next poll", label, pr)
		return
	}
	var slot *conflictSlot
	started := false
	defer func() {
		if !started {
			// Every early return below must hand back BOTH: the in-process gate
			// and, once it has been taken, the fleet's slot.
			slot.release()
			d.conflictBusy.Store(false)
		}
	}()

	step := cfg.ConflictStep()
	if strings.EqualFold(step.Harness, "claude") {
		d.conflictSay("claude", "conflict: %s PR #%d is CONFLICTING, but the conflict step resolves to harness %q - "+
			"daemon-started sessions never run on Claude, so the task waits for the agent close-out", label, pr, step.Harness)
		return
	}
	if err := checkPermissionPolicy(cfg); err != nil {
		d.conflictSay("policy:"+slug, "conflict: %s PR #%d needs a conflict session, but the permission policy cannot be "+
			"verified (%v) - no session was started", label, pr, err)
		return
	}
	// Everything that can fail WITHOUT a plane write happens before the claim:
	// a failure after the claim must either release it or is a bug, and the
	// worktree area is the last precondition.
	gate := conflictGate(dir)
	worktree := conflictWorktree(cfg, dir, task.ID)
	// The brief makes the session create its worktree OUTSIDE the checkout, and the
	// only grant that makes that writable is Invocation.ExtraDirs. A harness that
	// drops ExtraDirs (codex by design — its sandbox's writable roots are the cwd
	// plus operator-owned config) would fail at step 2 of the brief AFTER the
	// session was paid for, so it is refused here, before the claim.
	if !conflictHarnessTakesExtraDirs(step.Harness) {
		d.conflictSay("harness:"+slug, "conflict: %s PR #%d is CONFLICTING, but the conflict step resolves to harness %q, "+
			"which does not translate ExtraDirs - it could not create the worktree at %s, so no session was started; "+
			"name a harness that can (opencode) or leave the conflict block unset", label, pr, step.Harness, worktree)
		return
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		d.conflictSay("worktree:"+task.ID, "conflict: %s PR #%d is CONFLICTING, but the worktree area %s could not be prepared (%v) - "+
			"no session was started", label, pr, filepath.Dir(worktree), err)
		return
	}

	// Refuse a further session once the bound is spent: a failure leaves the task
	// approved, so an unbounded retry would spend a paid session every poll
	// forever. The check runs BEFORE the claim so an exhausted task costs no
	// plane writes.
	if !d.conflictAttemptAllowed(task.ID) {
		d.conflictSay("exhausted:"+task.ID, "conflict: %s PR #%d is CONFLICTING but %d conflict sessions have already "+
			"failed at it - leaving it to the agent close-out instead of spending another",
			label, pr, maxConflictAttempts)
		return
	}

	// The FLEET's slot, not this process's: conflictBusy above serialises this
	// Supervisor value, while the slot is the shared lease every supervisor on the
	// fleet's state root takes (conflictlock.go). It is acquired BEFORE the claim,
	// so a task that has to wait costs no plane write and no provider token.
	slotPath := d.conflictSlotPath()
	var serr error
	slot, serr = d.acquireConflictSlot(fmt.Sprintf("host=%s pid=%d task=%s %s pr=#%d",
		d.conflictHostname(), os.Getpid(), task.ID, label, pr))
	if serr != nil {
		slot = nil
		if errors.Is(serr, errConflictSlotHeld) {
			held := conflictSlotHolder(slotPath)
			if held == "" {
				held = "the holder left no record"
			}
			d.conflictSay("slot:"+task.ID, "conflict: %s PR #%d is CONFLICTING, but another supervisor holds the fleet's "+
				"conflict-session slot (%s) - this task waits for the next poll", label, pr, held)
			return
		}
		d.conflictSay("slot:"+task.ID, "conflict: %s PR #%d is CONFLICTING, but the fleet's conflict-session slot could not "+
			"be taken (%v) - no session was started", label, pr, serr)
		return
	}

	// The claim is the lock. Taking it BEFORE spending a session is what makes
	// "a task already claimed is skipped" true rather than hopeful: an agent
	// close-out that won the race leaves this claim refused, and no session is
	// started. It is also the run id every call the session makes signs with.
	runID, err := client.ClaimCloseOut(ctx, task.ID)
	if err != nil {
		d.conflictSay("claimed:"+task.ID, "conflict: %s PR #%d is CONFLICTING but the claim was refused (%v) - "+
			"no conflict session; the task stays with whoever holds it", label, pr, err)
		return
	}
	d.noteConflictAttempt(task.ID)

	req := ConflictRunRequest{
		TaskID:   task.ID,
		TaskRef:  task.Ref,
		RunID:    runID,
		Branch:   task.Branch,
		Base:     base,
		PR:       fmt.Sprintf("#%d", pr),
		Repo:     task.Repo,
		Paths:    paths,
		Gate:     gate,
		Harness:  step.Harness,
		Model:    step.Model,
		WorkDir:  dir,
		Worktree: worktree,
	}
	req.Prompt = config.ConflictBrief(config.ConflictBriefInput{
		TaskID:   req.TaskID,
		TaskRef:  req.TaskRef,
		RunID:    req.RunID,
		Branch:   req.Branch,
		Base:     req.Base,
		PR:       req.PR,
		Repo:     req.Repo,
		WorkDir:  req.WorkDir,
		Worktree: req.Worktree,
		Paths:    req.Paths,
		Gate:     req.Gate,
	})

	started = true
	d.conflictWG.Add(1)
	go func() {
		// Declared first, so it runs LAST: the fleet's slot is held until every
		// other bit of this session's bookkeeping is done.
		defer slot.release()
		defer d.conflictWG.Done()
		defer d.conflictBusy.Store(false)
		d.runConflictSession(cfg, slug, client, req, step)
	}()
}

// runConflictSession runs one session and then settles what it left behind.
//
// The session's context is the supervisor lifetime (a fleet stop cancels the
// session rather than leaving it running past the operator's stop), bounded by
// conflictSessionBudget. Everything AFTER the session — the state read and any
// release — runs on a cleanup context that outlives the cancellation, exactly as
// the deterministic close-out's post-claim writes do: a claim taken must not be
// left hanging because the operator stopped the fleet while it was working.
func (d *Supervisor) runConflictSession(cfg *config.Config, slug string, client plane.CloseOutAPI, req ConflictRunRequest, step config.ConflictStep) {
	label := strings.TrimSpace(req.TaskRef)
	if label == "" {
		label = req.TaskID
	}

	ctx, cancel := context.WithTimeout(d.baseCtx(), conflictSessionBudget)
	defer cancel()
	run := d.o.ConflictRun
	if run == nil {
		// A Supervisor built by hand in a test may not have gone through
		// withDefaults; the production runner is still the safe default.
		run = defaultConflictRun
	}
	res, runErr := run(ctx, cfg, slug, req, step)
	d.finishConflictSession(client, req, label, res, runErr)
}

// finishConflictSession reads what the session left on the plane and makes the
// hand-back true. The session is INSTRUCTED to do it itself (mechanical/failed:
// release; behavioural: in_review), so the common case needs no repair call — but
// a session that died, crashed or forgot must not leave the task claimed by a run
// that has stopped.
func (d *Supervisor) finishConflictSession(client plane.CloseOutAPI, req ConflictRunRequest, label string, res harness.Result, runErr error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.baseCtx()), conflictCallTimeout)
	defer cancel()

	st, err := client.TaskState(ctx, req.TaskID)
	if err != nil {
		// Without the state there is no honest verdict. Release, because a claim
		// left held is worse than a missing classification: the task returns to
		// approved and the agent close-out can take it.
		rerr := client.ReleaseCloseOut(ctx, req.TaskID, req.RunID, true)
		why := fmt.Sprintf("the task state could not be read (%v)", err)
		if rerr != nil {
			why += fmt.Sprintf(" and the claim could not be released (%v); its lease must expire", rerr)
		} else {
			why += "; the claim was released and the task left approved"
		}
		log.Printf("conflict: %s PR %s - outcome: failed - %s", label, req.PR, why)
		return
	}

	kind, detail, marked := config.ConflictResolution(st.Outcome)
	if strings.EqualFold(st.Status, "in_review") {
		note := strings.TrimSpace(detail)
		if note == "" {
			note = "the resolution changed behaviour and needs review"
		}
		// NOTHING TO RELEASE HERE, and a release would be refused. The plane's status
		// write clears the claim on every transition away from `in_progress`
		// (`updateStatus`: `via.to !== "in_progress" ? { claimedByRun: null }`) and ends
		// the run as `done`, so the behavioural hand-back hands the task to the human
		// reviewer with no claim left behind — pinned by the plane's own test
		// "status in_review ends the run as done and releases the claim". A release
		// call here would hit the `not_holding` guard (it requires `in_progress` with a
		// claimedByRun), i.e. a guaranteed refusal for a task that is already free.
		log.Printf("conflict: %s PR %s - outcome: behavioural - conflicting paths: %s - %s",
			label, req.PR, conflictPaths(req.Paths), note)
		return
	}

	// Still held by the run this supervisor took: the session did not hand it
	// back. Release it, run or no run, and classify by the marker the session
	// left (a mechanical resolution whose release call failed is still
	// mechanical; a session that left no marker is a failure).
	//
	// The comparison is between run IDENTITIES, not the ids as strings: the claim
	// hands this supervisor the run CREDENTIAL (`<uuid>.<secret>`) while the
	// plane's `claimedByRun` holds the bare uuid, so comparing the raw strings
	// never matched - the release never fired, and our own dead run was
	// misreported as "another run".
	//
	// The release attests green ONLY for a mechanical resolution, whose merge
	// really landed. A failed session's merge never happened, and a session that
	// left no marker has not shown a landing either, so both attest false and the
	// plane's CI-repair ladder counts and bounds the PR (standing decision
	// 047d3dad rule 1) — the same rule the brief's FAILED hand-back spells out.
	checkGreen := marked && kind == config.ConflictResolutionMechanical
	if claimIsOurs(st.ClaimedByRun, req.RunID) {
		if rerr := client.ReleaseCloseOut(ctx, req.TaskID, req.RunID, checkGreen); rerr != nil {
			log.Printf("conflict: %s PR %s - outcome: failed - the claim could not be released (%v); its lease must expire - %s",
				label, req.PR, rerr, conflictFailureReason(res, runErr, detail))
			return
		}
	} else if held := strings.TrimSpace(st.ClaimedByRun); held != "" {
		// Our lease lapsed and another run took the task. Nothing of ours is
		// signed any more, and the new holder owns the outcome; say so rather
		// than claiming a status we can no longer see.
		log.Printf("conflict: %s PR %s - outcome: failed - the task is now held by another run (%s); the conflict session's result cannot be recorded",
			label, req.PR, held)
		return
	}

	switch {
	case marked && kind == config.ConflictResolutionMechanical:
		log.Printf("conflict: %s PR %s - outcome: mechanical - conflicting paths: %s - task left approved for the close-out",
			label, req.PR, conflictPaths(req.Paths))
	case marked && kind == config.ConflictResolutionFailed:
		log.Printf("conflict: %s PR %s - outcome: failed - %s - conflicting paths: %s - claim released, task left approved",
			label, req.PR, conflictFailureReason(res, runErr, detail), conflictPaths(req.Paths))
	default:
		// No marker at all: the session did not follow step 7. A run that ended
		// cleanly still may have pushed nothing; say so rather than calling it
		// mechanical by default, which would claim a review it never had.
		log.Printf("conflict: %s PR %s - outcome: failed - the session left no resolution marker - %s",
			label, req.PR, conflictFailureReason(res, runErr, detail))
	}
}

// conflictRunUUID is a run id's identity: the part before the first dot. A claim
// hands the supervisor the run CREDENTIAL (`<uuid>.<secret>`), while the plane's
// records — `claimedByRun` above all — hold the BARE uuid, so two ids name the
// same run exactly when their uuids match. Mirrors internal/loop's runUUID, which
// is the same split the plane makes (parseRunCredential); a bare id is its own
// uuid.
func conflictRunUUID(id string) string {
	id = strings.TrimSpace(id)
	if i := strings.IndexByte(id, '.'); i >= 0 {
		return id[:i]
	}
	return id
}

// claimIsOurs reports whether the task's recorded holder is the run this
// supervisor claimed under. Both sides are compared as run IDENTITIES: comparing
// the strings was a real defect (CLA-759 review 1.review-1.1), because the claim
// returns a credential and the plane records the bare uuid, so it never matched
// and the safety-net release never fired.
func claimIsOurs(claimedByRun, runID string) bool {
	mine := conflictRunUUID(runID)
	return mine != "" && conflictRunUUID(claimedByRun) == mine
}

// conflictHarnessTakesExtraDirs reports whether a harness translates
// Invocation.ExtraDirs into a writable-root grant for the session. It is an
// inverted ALLOWLIST on purpose: the conflict brief makes the session create a
// worktree OUTSIDE its checkout, so a harness whose sandbox ignores ExtraDirs can
// never do the job — and a new harness must be added here deliberately, rather than
// silently inheriting a session it cannot complete. Today:
//
//   - opencode gains read/edit/external_directory scope on every extra dir
//     (harness/opencode.go, scopeExtraDirs);
//   - claude maps them to --add-dir (and is refused earlier by the operator's
//     no-Claude-for-daemon-sessions rule, listed for completeness);
//   - codex deliberately does NOT: its workspace-write scope is the cwd plus the
//     operator-owned `writable_roots` in CODEX_HOME (harness/codex.go).
func conflictHarnessTakesExtraDirs(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "opencode", "claude":
		return true
	default:
		return false
	}
}

// conflictHostname is the machine identity written into the slot's holder record:
// the name reconcile resolved, else the OS's own, so a stand-down line on another
// supervisor names a host even when this value was never resolved (a test, or a
// supervisor that failed before its first reconcile).
func (d *Supervisor) conflictHostname() string {
	if h := strings.TrimSpace(d.hostname); h != "" {
		return h
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "unknown"
}

// conflictFailureReason composes the one-line "why" for a failed conflict
// session: the session's own stated reason when it left one, else what the run
// itself reported (a launcher error, an exit code, a diagnostic tail).
func conflictFailureReason(res harness.Result, runErr error, detail string) string {
	if strings.TrimSpace(detail) != "" {
		return detail
	}
	if runErr != nil {
		return runErr.Error()
	}
	if res.ExitCode != 0 || res.ExitSignal != 0 {
		return fmt.Sprintf("the session exited %d (signal %d)", res.ExitCode, res.ExitSignal)
	}
	if msg := strings.TrimSpace(res.FinalMessage); msg != "" {
		return firstLine(msg)
	}
	if msg := strings.TrimSpace(res.Stderr); msg != "" {
		return firstLine(msg)
	}
	return "the session produced no resolution"
}

// conflictPaths renders the conflicting paths for a log line.
func conflictPaths(paths []string) string {
	if len(paths) == 0 {
		return "(the fleet could not name them)"
	}
	return strings.Join(paths, ", ")
}

// conflictWorktree names the worktree the session must create. Two layouts, in
// the same order the fleet derives them:
//
//   - the per-project layout (`project_root`): <project root>/worktrees/, which
//     the workdir derivation already creates and which sits beside the repo
//     clones — the same area every fleet worktree lives in;
//   - an operator's own checkout: the conventional <checkout>-wt area beside it,
//     which is the shape the driver's own ExtraDirs grants (see extraDirsFor).
//
// Naming it here, rather than letting the session choose, is what lets the
// supervisor grant exactly that directory into the session's permission scope
// (defaultConflictRun's ExtraDirs): a worktree in a directory the session may not
// write fails at the first `git worktree add`.
func conflictWorktree(cfg *config.Config, dir, taskID string) string {
	dir = filepath.Clean(dir)
	name := "conflict-" + taskID
	if cfg != nil && cfg.ProjectRoot {
		return filepath.Join(filepath.Dir(dir), "worktrees", name)
	}
	return filepath.Join(dir+"-wt", name)
}

// conflictGate resolves the repo-wide check the conflict session must run,
// preferring the checkout's own toolchain so the two current projects (and any
// successor) need no extra config key:
//
//   - a `go.mod` at the checkout root -> `go test ./...`;
//   - a `package.json` -> `pnpm test`, the repo-wide gate this fleet's JS
//     projects use.
//
// Anything else returns "" and the brief points the session at the repo's own
// documented gate instead of inventing a command. This is a default, not a
// verdict: a repo with both files is a Go repo with JS tooling inside, and the
// Go suite is its repo-wide gate.
func conflictGate(dir string) string {
	if dir == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return "go test ./..."
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
		return "pnpm test"
	}
	return ""
}

// waitConflict is the bounded wait a fleet stop gives an in-flight conflict
// session: its context is cancelled already, but a claim it holds still has to be
// released by the session goroutine before the process exits.
func (d *Supervisor) waitConflict() {
	done := make(chan struct{})
	go func() {
		d.conflictWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(conflictCallTimeout + 5*time.Second):
		log.Print("conflict: a session was still running at shutdown - a claim it holds must expire before an agent can take it")
	}
}

// conflictAttemptAllowed reports whether another paid session may be spent on
// this task. Called only from the close-out pass goroutine.
func (d *Supervisor) conflictAttemptAllowed(taskID string) bool {
	d.conflictMu.Lock()
	defer d.conflictMu.Unlock()
	return d.conflictTried[taskID].n < maxConflictAttempts
}

// noteConflictAttempt records that a session is about to be spent on this task.
func (d *Supervisor) noteConflictAttempt(taskID string) {
	d.conflictMu.Lock()
	defer d.conflictMu.Unlock()
	if d.conflictTried == nil {
		d.conflictTried = map[string]conflictAttempt{}
	}
	prev := d.conflictTried[taskID]
	prev.n++
	d.conflictTried[taskID] = prev
}

// conflictSay logs one conflict-path line unless the line for that key is
// unchanged, the "say it once per change" discipline the close-out keeps. It has
// its own map and mutex because the start path runs on the close-out pass
// goroutine while the session goroutine can log its completion at the same time.
func (d *Supervisor) conflictSay(key, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	d.conflictMu.Lock()
	defer d.conflictMu.Unlock()
	if d.conflictSaid == nil {
		d.conflictSaid = map[string]string{}
	}
	if d.conflictSaid[key] == line {
		return
	}
	d.conflictSaid[key] = line
	log.Print(line)
}

// defaultConflictRun launches the real harness session for a conflict resolution.
// It is the production Options.ConflictRun, and it mirrors the daemon's own
// session composition: the same config, the same env overlay, the same MCP config
// the fleet's phases use, and the same per-session ceilings, so a conflict session
// is bounded and equipped exactly like any other session on that harness.
func defaultConflictRun(ctx context.Context, cfg *config.Config, slug string, req ConflictRunRequest, step config.ConflictStep) (harness.Result, error) {
	adapter, err := harness.Get(req.Harness)
	if err != nil {
		return harness.Result{}, err
	}
	inv, err := conflictInvocation(cfg, slug, req, step)
	if err != nil {
		return harness.Result{}, err
	}
	return adapter.Invoke(ctx, inv)
}

// conflictInvocation composes the invocation a conflict session runs. Split from
// the launcher so the composition — the one part that needs no paid process — is
// testable: the prompt, the model, the reach (ExtraDirs) and the ceilings.
func conflictInvocation(cfg *config.Config, slug string, req ConflictRunRequest, step config.ConflictStep) (harness.Invocation, error) {
	env, err := cfg.SessionEnv(req.Harness, slug)
	if err != nil {
		return harness.Invocation{}, fmt.Errorf("conflict session env: %w", err)
	}
	hc := cfg.SessionFor(req.Harness)
	project := conflictProject(cfg, slug)
	return harness.Invocation{
		Prompt:              req.Prompt,
		Model:               req.Model,
		WorkDir:             req.WorkDir,
		MCPConfigPath:       cfg.ResolveMCPConfig(req.Harness, project.MCPConfigPath, project.MCPConfigPaths),
		ConfigDir:           hc.ConfigDir,
		SettingsPath:        hc.SettingsPath,
		ExtraDirs:           []string{filepath.Dir(req.Worktree)},
		Env:                 env,
		EnvAllowlist:        cfg.SessionEnvAllowlist(req.Harness, slug),
		MaxTurns:            step.MaxTurns,
		MaxSessionTokens:    cfg.Budget.SessionTokenCeilingFor(req.Harness),
		MaxSessionWallClock: cfg.MaxSessionWallClock.Duration(),
	}, nil
}

// conflictProject finds the project block the supervisor built one instance
// from, so the session gets that project's own MCP config. An unmatched slug is
// the single-project shape, and the zero value keeps the top-level fallback.
func conflictProject(cfg *config.Config, slug string) config.Project {
	for _, p := range cfg.Projects {
		if strings.EqualFold(p.Slug, slug) {
			return p
		}
	}
	return config.Project{}
}
