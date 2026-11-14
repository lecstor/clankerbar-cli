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
//  2. starts ONE session, on the fleet's IMPLEMENT harness/model routes unless
//     the config's `conflict` step names its own, and logs one line with the
//     task, the PR, the conflicting paths and the outcome. An attempt that ends
//     on a usage limit is re-run on the next model in the list (CLA-890).
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
	"slices"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/loop"
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

	// maxConflictAttempts bounds how many conflict sessions one task may burn in
	// this supervisor's lifetime. One session may walk the step's model list once
	// (a quota fallback, CLA-890); that whole walk is one attempt here. A failed
	// session leaves the task approved, so without a bound the next poll would
	// start another, and another: the exact opposite of "the agent close-out
	// remains the fallback". A task that keeps failing falls back to the agent
	// close-out, which is the designed fallback — not a third paid experiment.
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
	// Model is the route this attempt runs on: the first entry of Routes, or the
	// harness's own default when both are empty.
	Model string
	// Routes is the ordered model route list this attempt may fall back through
	// (CLA-890): the conflict step's resolved list, or the suffix of it left after
	// a quota failure. A pinned single `conflict.model` is a one-entry list, so it
	// runs only that model.
	Routes  []string
	WorkDir string
	// Worktree is the exact path the session creates its branch worktree at. The
	// supervisor names it (rather than letting the session choose) so it can
	// grant that directory into the session's permission scope.
	Worktree string
	Prompt   string
	// PriorOutcome is the task's outcome as the CLAIM response served it, before
	// the session ran. The brief tells the session to copy it verbatim and append
	// its block, so the post-session read is classified only on what this session
	// added: an unchanged outcome is "no marker", never an older attempt's
	// attestation (CLA-889).
	PriorOutcome string
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
			"name a harness that can (opencode or copilot) or leave the conflict block unset", label, pr, step.Harness, worktree)
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
	claim, err := client.ClaimCloseOut(ctx, task.ID)
	if err != nil {
		d.conflictSay("claimed:"+task.ID, "conflict: %s PR #%d is CONFLICTING but the claim was refused (%v) - "+
			"no conflict session; the task stays with whoever holds it", label, pr, err)
		return
	}
	d.noteConflictAttempt(task.ID)

	req := ConflictRunRequest{
		TaskID:   task.ID,
		TaskRef:  task.Ref,
		RunID:    claim.RunID,
		Branch:   task.Branch,
		Base:     base,
		PR:       fmt.Sprintf("#%d", pr),
		Repo:     task.Repo,
		Paths:    paths,
		Gate:     gate,
		Harness:  step.Harness,
		Model:    step.Model.First(),
		Routes:   step.Model.List(),
		WorkDir:  dir,
		Worktree: worktree,

		PriorOutcome: claim.Outcome,
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

	// Classify only what THIS session wrote: the brief appends its block to the
	// stored outcome, so an outcome still identical to the claim-time baseline
	// carries older attempts' markers only, and none of them describes this
	// session (CLA-889).
	kind, detail, marked := config.ConflictResolutionSince(req.PriorOutcome, st.Outcome)
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

	// When the task still reads in_progress and held, the session did not hand it
	// back: release it and classify by the marker IT left (a mechanical resolution
	// whose release call failed is still mechanical; a session that wrote nothing is
	// a failure).
	//
	// The plane serves PRESENCE only (CLA-509), so there is no holder id to compare
	// against ours. The release call is the check instead: signing it with our own
	// claim succeeds when the claim is still ours, and the plane's holder refusal
	// means it is not — another run took the task, or our run is already over.
	//
	// The release attests green ONLY for a mechanical resolution, whose merge
	// really landed. A failed session's merge never happened, and a session that
	// left no marker has not shown a landing either, so both attest false and the
	// plane's CI-repair ladder counts and bounds the PR (standing decision
	// 047d3dad rule 1) — the same rule the brief's FAILED hand-back spells out.
	checkGreen := marked && kind == config.ConflictResolutionMechanical
	handback := "the claim is not held"
	if strings.EqualFold(st.Status, "approved") {
		handback = "the claim was already released, task left approved"
	}
	if strings.EqualFold(st.Status, "in_progress") && st.HeldByRun {
		rerr := client.ReleaseCloseOut(ctx, req.TaskID, req.RunID, checkGreen)
		switch {
		case rerr == nil:
			handback = "claim released, task left approved"
		case errors.Is(rerr, plane.ErrNotHolding):
			// Our claim is gone. Nothing of ours is signed any more, and the new
			// holder owns the outcome; say so rather than claiming a status we no
			// longer control.
			log.Printf("conflict: %s PR %s - outcome: failed - the task is now held by another run, so the conflict session's result cannot be recorded (%v) - %s",
				label, req.PR, rerr, conflictFailureReason(res, runErr, detail))
			return
		default:
			log.Printf("conflict: %s PR %s - outcome: failed - the claim could not be released (%v); its lease must expire - %s",
				label, req.PR, rerr, conflictFailureReason(res, runErr, detail))
			return
		}
	}

	switch {
	case marked && kind == config.ConflictResolutionMechanical:
		log.Printf("conflict: %s PR %s - outcome: mechanical - conflicting paths: %s - task left approved for the close-out",
			label, req.PR, conflictPaths(req.Paths))
	case marked && kind == config.ConflictResolutionFailed:
		log.Printf("conflict: %s PR %s - outcome: failed - %s - conflicting paths: %s - %s",
			label, req.PR, conflictFailureReason(res, runErr, detail), conflictPaths(req.Paths), handback)
	default:
		// No marker at all: the session did not follow step 7. A run that ended
		// cleanly still may have pushed nothing; say so rather than calling it
		// mechanical by default, which would claim a review it never had. The
		// hand-back is named here too, because this is exactly the shape a session
		// that died leaves behind (CLA-884).
		log.Printf("conflict: %s PR %s - outcome: failed - the session left no resolution marker - %s - %s",
			label, req.PR, conflictFailureReason(res, runErr, detail), handback)
	}
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
//   - copilot maps them to --add-dir too, which the CLI's path verification
//     includes among the writable roots (harness/copilot.go);
//   - codex deliberately does NOT: its workspace-write scope is the cwd plus the
//     operator-owned `writable_roots` in CODEX_HOME (harness/codex.go).
func conflictHarnessTakesExtraDirs(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "opencode", "claude", "copilot":
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
//
// Deliberately does not consult pause_windows: the schedule gates the loop's
// claims, and a conflict run is close-out of work already delivered.
func defaultConflictRun(ctx context.Context, cfg *config.Config, slug string, req ConflictRunRequest, step config.ConflictStep) (harness.Result, error) {
	adapter, err := harness.Get(req.Harness)
	if err != nil {
		return harness.Result{}, err
	}
	label := strings.TrimSpace(req.TaskRef)
	if label == "" {
		label = req.TaskID
	}
	return runConflictModelLadder(label, req, adapter, func(model string, routes []string) (harness.Result, error) {
		attempt := req
		attempt.Model, attempt.Routes = model, routes
		inv, err := conflictInvocation(cfg, slug, attempt, step)
		if err != nil {
			return harness.Result{}, err
		}
		return adapter.Invoke(ctx, inv)
	})
}

// conflictModelRun launches ONE conflict session pinned to model, offered the
// model route list from model onwards.
type conflictModelRun func(model string, routes []string) (harness.Result, error)

// conflictLimitReader is the one adapter capability the model ladder needs: the
// harness's own DetectLimit, the same classification the phase ladder's limit
// branch reads. Narrowed to one method so the ladder is testable without a paid
// process; harness.Adapter is the production value.
type conflictLimitReader interface {
	DetectLimit(harness.Result) harness.Limit
}

// runConflictModelLadder runs the conflict session on the step's first model and,
// when an attempt ends on a usage limit while a model is left, runs the session
// again on the next one (CLA-890).
//
// The classification is the harness's own DetectLimit — the same one the phase
// ladder's limit branch reads — so a limit the adapter already handled by
// switching routes mid-session never reaches here. What does reach here is the
// death it could not resume: a session that ended before it had one, which is the
// CLA-884 shape (the free route's quota was exhausted ~2s in, so there was no
// session id and no route switch, and the conflicting PR waited for a human).
//
// A limit on the LAST model returns that attempt's result unchanged: there is
// nowhere left to go, and the existing failure classification reports it. Every
// attempt is logged with the model it ran on, so a quota failure names the route
// it hit.
//
// It retries whether or not the attempt got as far as editing anything. The
// retry is a FRESH session running the same brief, and that brief is idempotent
// where work already landed: the merge reports "Already up to date", a resolved
// tree has no conflicting hunks left, and the gate and push are unchanged. So a
// retry after partial work costs one session and can still finish the hand-back;
// the case it exists for did nothing at all — CLA-884's quota death, where the
// session ended before it had an id to resume.
func runConflictModelLadder(label string, req ConflictRunRequest, a conflictLimitReader, run conflictModelRun) (harness.Result, error) {
	models := req.Routes
	if len(models) == 0 {
		// No route named at all: ONE attempt on the harness's own default. A
		// pinned single model arrives as a one-entry list, so it never falls back.
		models = []string{req.Model}
	}
	var res harness.Result
	var err error
	for i := 0; i < len(models); i++ {
		model := models[i]
		name := strings.TrimSpace(model)
		if name == "" {
			name = "the harness default"
		}
		log.Printf("conflict: %s PR %s - attempt on %s (route %d of %d)", label, req.PR, name, i+1, len(models))
		res, err = run(model, models[i:])
		lim := a.DetectLimit(res)
		if !lim.Limited {
			// The adapter may have switched routes MID-session and finished the
			// work on a later one; name that route too, or the attempt line reads
			// as if the model it started on did the job.
			if fin := strings.TrimSpace(harness.RouteFinished(res)); fin != "" && fin != name {
				log.Printf("conflict: %s PR %s - the session finished on %s after a route switch", label, req.PR, fin)
			}
			return res, err
		}
		// The route the attempt actually ENDED on. The adapter may have switched
		// routes mid-session already, and retrying a route it just exhausted buys
		// nothing; skipping to the one after it keeps the ladder and the
		// adapter's own switch from spending the same denial twice.
		if fin := strings.TrimSpace(harness.RouteFinished(res)); fin != "" {
			name = fin
			if idx := slices.Index(models, fin); idx > i {
				i = idx
			}
		}
		reason := loop.LimitStopReason(lim)
		if i == len(models)-1 {
			log.Printf("conflict: %s PR %s - %s ended on a usage limit (%s) and it is the last model in the list",
				label, req.PR, name, reason)
			return res, err
		}
		log.Printf("conflict: %s PR %s - %s hit a usage limit (%s) - retrying the session on %s",
			label, req.PR, name, reason, models[i+1])
	}
	return res, err
}

// conflictInvocation composes the invocation a conflict session runs. Split from
// the launcher so the composition — the one part that needs no paid process — is
// testable: the prompt, the model and its route list, the reach (ExtraDirs) and
// the ceilings.
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
		Routes:              req.Routes,
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
