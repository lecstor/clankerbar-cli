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
// At most ONE conflict session runs at a time per supervisor: a second conflicting
// task waits for the next poll, and the claim is what makes that safe even when
// another machine's supervisor is racing for the same task. Daemon-started
// sessions never run on Claude (operator rule), so a step resolving to Claude
// stands down loudly instead of spending a session.

import (
	"context"
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
)

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
	Prompt  string
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

	// One at a time. A second conflicting task is not dropped: it stays approved,
	// and the pass reaches it again on the next poll once this session ends.
	if !d.conflictBusy.CompareAndSwap(false, true) {
		return
	}
	started := false
	defer func() {
		if !started {
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

	gate := conflictGate(dir)
	req := ConflictRunRequest{
		TaskID:  task.ID,
		TaskRef: task.Ref,
		RunID:   runID,
		Branch:  task.Branch,
		Base:    base,
		PR:      fmt.Sprintf("#%d", pr),
		Repo:    task.Repo,
		Paths:   paths,
		Gate:    gate,
		Harness: step.Harness,
		Model:   step.Model,
		WorkDir: dir,
	}
	req.Prompt = config.ConflictBrief(config.ConflictBriefInput{
		TaskID:  req.TaskID,
		TaskRef: req.TaskRef,
		RunID:   req.RunID,
		Branch:  req.Branch,
		Base:    req.Base,
		PR:      req.PR,
		Repo:    req.Repo,
		WorkDir: req.WorkDir,
		Paths:   req.Paths,
		Gate:    req.Gate,
	})

	started = true
	d.conflictWG.Add(1)
	go func() {
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
	res, runErr := d.o.ConflictRun(ctx, cfg, slug, req, step)
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
		log.Printf("conflict: %s PR %s - outcome: behavioural - conflicting paths: %s - %s",
			label, req.PR, conflictPaths(req.Paths), note)
		return
	}

	// Still held by the run this supervisor took: the session did not hand it
	// back. Release it, run or no run, and classify by the marker the session
	// left (a mechanical resolution whose release call failed is still
	// mechanical; a session that left no marker is a failure).
	if strings.TrimSpace(st.ClaimedByRun) == strings.TrimSpace(req.RunID) {
		if rerr := client.ReleaseCloseOut(ctx, req.TaskID, req.RunID, true); rerr != nil {
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

// behaviouralDetail prefers the marker's note, falling back to the plain fact.
func behaviouralDetail(kind, detail string) string {
	if strings.TrimSpace(detail) != "" {
		return detail
	}
	if kind != config.ConflictResolutionBehavioural {
		return "the resolution changed behaviour and needs review"
	}
	return "the resolution changed behaviour and needs review"
}

// conflictPaths renders the conflicting paths for a log line.
func conflictPaths(paths []string) string {
	if len(paths) == 0 {
		return "(the fleet could not name them)"
	}
	return strings.Join(paths, ", ")
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
	env, err := cfg.SessionEnv(req.Harness, slug)
	if err != nil {
		return harness.Result{}, fmt.Errorf("conflict session env: %w", err)
	}
	hc := cfg.SessionFor(req.Harness)
	project := conflictProject(cfg, slug)
	inv := harness.Invocation{
		Prompt:              req.Prompt,
		Model:               req.Model,
		WorkDir:             req.WorkDir,
		MCPConfigPath:       cfg.ResolveMCPConfig(req.Harness, project.MCPConfigPath, project.MCPConfigPaths),
		ConfigDir:           hc.ConfigDir,
		SettingsPath:        hc.SettingsPath,
		Env:                 env,
		EnvAllowlist:        cfg.SessionEnvAllowlist(req.Harness, slug),
		MaxTurns:            step.MaxTurns,
		MaxSessionTokens:    cfg.Budget.SessionTokenCeilingFor(req.Harness),
		MaxSessionWallClock: cfg.MaxSessionWallClock.Duration(),
	}
	return adapter.Invoke(ctx, inv)
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
