package loop

// review_run.go — `clankerbar review run` (CLA-870): review ONE local
// base..head outside any task, claim or plane, and emit the findings as JSON.
//
// It is a driver, not a second review: the brief is the review phase's own
// substance without a task (config.ReviewRunBrief), the maps are built by the
// phase's planner (planMapsSession), and the report is the phase's done report
// read by the same ParseDone/ValidateReview pair. What it does NOT do is any
// part of the task lifecycle — no claim, no MCP config, no plane record — so the
// eval harness scores the review the driver really runs, not a reimplementation
// of it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/reviewmap"
)

// ReviewRunOptions is one standalone review. Base is required; Head defaults to
// HEAD. Method is config.ReviewMethodSinglePass or config.ReviewMethodMapsSession;
// empty means single-pass. Harness is a registered adapter name (the CLI defaults
// it to opencode, which the maps-session method needs).
type ReviewRunOptions struct {
	Repo          string
	Base          string
	Head          string
	Method        string
	Harness       string
	Model         string
	NotesModel    string
	FindingsModel string
	ChunkSize     int
	FlowMax       int
	Title         string
	Description   string
	Console       io.Writer

	// Seams for the tests; production leaves them nil and gets the real harness,
	// the discovered config and a real `git worktree remove`.
	invoke         func(context.Context, harness.Invocation) (harness.Result, error)
	cfg            *config.Config
	removeWorktree func(repo, path string) error
}

// ReviewRunReport is the JSON document the command emits. Every field is a fact
// the caller needs to score a run: the resolved commits (never the raw flag
// values), the model each stage ran on, the findings in the phase's own shape,
// the notes the map method wrote, the session's spend, and how many turns ran.
type ReviewRunReport struct {
	Base       string               `json:"base"`
	Head       string               `json:"head"`
	Method     string               `json:"method"`
	Models     map[string]string    `json:"models"`
	Findings   []config.DoneFinding `json:"findings"`
	Omitted    int                  `json:"omitted"`
	NotesTurns []string             `json:"notes_turns"`
	Usage      ReviewRunUsage       `json:"usage"`
	TimeMS     int64                `json:"time_ms"`
	Turns      int                  `json:"turns"`
}

// ReviewRunUsage is the session's token accounting in the harness package's
// normalised buckets, plus the money the harness reported (0 when it reported
// none, which only cost-reporting adapters distinguish).
type ReviewRunUsage struct {
	Input      int     `json:"input"`
	Output     int     `json:"output"`
	Reasoning  int     `json:"reasoning"`
	CacheRead  int     `json:"cache_read"`
	CacheWrite int     `json:"cache_write"`
	CostUSD    float64 `json:"cost_usd"`
}

// reviewRunStage is what building the invocation decided beyond the invocation
// itself: the models JSON names per stage, and how many of the result's turn
// messages are notes rather than findings.
type reviewRunStage struct {
	models     map[string]string
	notesTurns int
}

// RunReview reviews opts.Base..opts.Head in a temporary detached worktree at
// Head, removes that worktree whatever happened, and returns the parsed report.
//
// The worktree is the isolation contract: the reviewed repository's own checkout
// is only ever read from (rev-parse, worktree add/remove), so a dirty tree, a
// branch in the middle of something, or a review that edits files cannot disturb
// it. When the run produced a report but cleanup failed, the report is returned
// WITH the error so a caller can still write the JSON; the command exits non-zero
// either way.
func RunReview(ctx context.Context, opts ReviewRunOptions) (*ReviewRunReport, error) {
	start := time.Now()
	method, err := NormalizeReviewMethod(opts.Method)
	if err != nil {
		return nil, err
	}
	repo, err := reviewRunRepo(opts.Repo)
	if err != nil {
		return nil, err
	}
	base, err := reviewRunCommit(repo, "base", opts.Base)
	if err != nil {
		return nil, err
	}
	headRev := strings.TrimSpace(opts.Head)
	if headRev == "" {
		headRev = "HEAD"
	}
	head, err := reviewRunCommit(repo, "head", headRev)
	if err != nil {
		return nil, err
	}
	cfg := opts.cfg
	if cfg == nil {
		cfg, err = config.Load("")
		if err != nil {
			return nil, fmt.Errorf("load config: %w", err)
		}
	}
	adapter, err := harness.Get(opts.Harness)
	if err != nil {
		return nil, fmt.Errorf("harness %q: %w", opts.Harness, err)
	}
	if method == config.ReviewMethodMapsSession && !adapter.Capabilities().RunsTurnSequences {
		return nil, fmt.Errorf("--method maps-session runs its notes and findings turns in one session, and harness %q cannot continue a session: use a harness that can, or the single-pass method", opts.Harness)
	}
	invoke := opts.invoke
	if invoke == nil {
		invoke = adapter.Invoke
	}
	remove := opts.removeWorktree
	if remove == nil {
		remove = removeReviewWorktree
	}

	root, err := os.MkdirTemp("", "clankerbar-review-*")
	if err != nil {
		return nil, fmt.Errorf("create the review worktree: %w", err)
	}
	wt := filepath.Join(root, "worktree")
	if out, err := reviewGit(repo, "worktree", "add", "--detach", wt, head); err != nil {
		os.RemoveAll(root)
		// A partially-registered worktree must not outlive the failure.
		reviewGit(repo, "worktree", "prune")
		return nil, fmt.Errorf("check out %s in a temporary worktree: %w%s", head, err, reviewGitDetail(out))
	}

	report, runErr := reviewRunSession(ctx, opts, cfg, method, invoke, wt, base, head, start)
	if cleanupErr := remove(repo, wt); cleanupErr != nil {
		cleanupErr = fmt.Errorf("remove the review worktree: %w", cleanupErr)
		if runErr != nil {
			return report, errors.Join(runErr, cleanupErr)
		}
		return report, cleanupErr
	}
	return report, runErr
}

// reviewRunSession builds the invocation, runs it and parses what it produced.
// Split from RunReview so the worktree's create/remove brackets every path here.
func reviewRunSession(ctx context.Context, opts ReviewRunOptions, cfg *config.Config, method string, invoke func(context.Context, harness.Invocation) (harness.Result, error), wt, base, head string, start time.Time) (*ReviewRunReport, error) {
	inv, stage, err := reviewRunInvocation(opts, cfg, method, wt, base, head)
	if err != nil {
		return nil, err
	}
	res, runErr := invoke(ctx, inv)
	report := &ReviewRunReport{
		Base:       base,
		Head:       head,
		Method:     method,
		Models:     stage.models,
		Findings:   []config.DoneFinding{},
		NotesTurns: []string{},
		Usage:      reviewRunUsage(res),
		TimeMS:     time.Since(start).Milliseconds(),
		Turns:      reviewRunTurns(method, res),
	}
	if method == config.ReviewMethodMapsSession {
		n := min(stage.notesTurns, len(res.TurnMessages))
		report.NotesTurns = append(report.NotesTurns, res.TurnMessages[:n]...)
	}

	rep, parseErr := reviewRunParseDone(res.FinalMessage, head)
	if parseErr == nil {
		if rep.Findings != nil {
			report.Findings = rep.Findings
		}
		report.Omitted = int(rep.Omitted)
	}
	switch {
	case res.Untrusted != "":
		return report, fmt.Errorf("the review session's stream could not be read whole (%s), so its report and spend are not trustworthy", res.Untrusted)
	case runErr != nil:
		return report, fmt.Errorf("the review session failed: %w", runErr)
	case res.TurnStopped != 0:
		reason := res.TurnStopReason
		if reason == "" {
			reason = "an unreported end"
		}
		return report, fmt.Errorf("the review session stopped at turn %d of its sequence (%s), so the review did not complete", res.TurnStopped, reason)
	case res.ToolLoop != "":
		return report, fmt.Errorf("the review session was killed for a tool loop (%s), which is not an orderly end, so its report is not a completed review", res.ToolLoop)
	case res.IdleKilled != "":
		return report, fmt.Errorf("the review session was killed for silence (%s), which is not an orderly end, so its report is not a completed review", res.IdleKilled)
	case res.ExitCode != 0 && harness.RecoveredErrors(res) == 0:
		return report, fmt.Errorf("the review session ended %s, which is not an orderly end, so its done report is not applied", res.ExitString())
	case parseErr != nil:
		return report, fmt.Errorf("the review session ended without a usable done report after the one re-emit: %w", parseErr)
	}
	return report, nil
}

// reviewRunInvocation composes the session: the taskless brief, the maps
// section when the method asks for it, and the harness/session fields the
// phase driver would resolve from config. It deliberately leaves MCPConfigPath
// empty: this command has no plane identity to hand a session, and a session
// that tried a clankerbar call would be talking to the operator's backlog in a
// review that must not touch it.
func reviewRunInvocation(opts ReviewRunOptions, cfg *config.Config, method, wt, base, head string) (harness.Invocation, reviewRunStage, error) {
	stage := reviewRunStage{models: map[string]string{}}
	env, err := cfg.SessionEnv(opts.Harness, "")
	if err != nil {
		return harness.Invocation{}, stage, fmt.Errorf("review session env: %w", err)
	}
	hc := cfg.SessionFor(opts.Harness)
	idle, toolCall := cfg.SessionIdleFor(opts.Harness)
	// A standalone review has no phase of its own, so it takes the run-wide cap
	// and then the built-in default, exactly as resolveMaxTurns does for a phase
	// with no cap. A harness that honours Invocation.MaxTurns (claude) would
	// otherwise have no turn backstop at all.
	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = config.DefaultMaxTurns
	}
	inv := harness.Invocation{
		Prompt:                  config.ReviewRunBrief(config.ReviewRunBriefOptions{Base: base, Head: head, Title: opts.Title, Description: opts.Description}),
		Model:                   strings.TrimSpace(opts.Model),
		WorkDir:                 wt,
		ConfigDir:               hc.ConfigDir,
		SettingsPath:            hc.SettingsPath,
		Console:                 opts.Console,
		Env:                     env,
		EnvAllowlist:            cfg.SessionEnvAllowlist(opts.Harness, ""),
		MaxSessionTokens:        cfg.Budget.SessionTokenCeilingFor(opts.Harness),
		MaxRepeatedToolFailures: cfg.RepeatedToolFailureBound(),
		MaxTurns:                maxTurns,
		MaxSessionIdle:          idle,
		MaxToolCallDuration:     toolCall,
		MaxSessionWallClock:     cfg.MaxSessionWallClock.Duration(),
		AllowUnclaimed:          true,
		CheckReport:             func(final string) error { _, err := reviewRunParseDone(final, head); return err },
	}
	stage.models["review"] = inv.Model
	if method != config.ReviewMethodMapsSession {
		return inv, stage, nil
	}

	rng := reviewmap.Range{Repo: wt, Base: base, Head: head}
	diff, err := reviewmap.Diff(rng)
	if err != nil {
		return inv, stage, fmt.Errorf("map the change: %w", err)
	}
	mapOpts := config.MapsSessionOptions{
		ChunkSize:     opts.ChunkSize,
		NotesModel:    opts.NotesModel,
		FindingsModel: opts.FindingsModel,
		FlowMax:       opts.FlowMax,
	}.Resolved()
	flow, err := reviewmap.FlowMap(diff, rng, mapOpts.FlowMax)
	if err != nil {
		return inv, stage, fmt.Errorf("map the changed definitions: %w", err)
	}
	// A standalone review has no task, so its findings turn grades MUST against
	// the repo's gates and correctness/safety only — never a done-when bar.
	plan := planMapsSession(mapOpts, inv.Model, base, diff, flow, config.MapsSessionSeverityTaskless, config.MapsSessionDoneReportEnding)
	var reportModel string
	inv, reportModel = wireMapsPlan(inv, plan)
	stage.models = map[string]string{"notes": plan.notesModel, "findings": reportModel}
	stage.notesTurns = plan.notesTurns
	return inv, stage, nil
}

// reviewRunParseDone reads the session's final message as the phase's review
// report. The error is what CheckReport steers the one re-emit with, and what a
// run that still failed ends with, so it names the marker and the fields rather
// than saying "unparseable".
func reviewRunParseDone(final, head string) (config.DoneReport, error) {
	rep, found, err := config.ParseDone(final)
	if err != nil {
		return config.DoneReport{}, err
	}
	if !found {
		return config.DoneReport{}, fmt.Errorf("the final message carries no done report: it must end with a line that is exactly %q followed by one JSON object", config.DoneMarker)
	}
	if err := rep.ValidateReview(head); err != nil {
		return config.DoneReport{}, err
	}
	return rep, nil
}

// reviewRunUsage normalises one Result's spend for the report.
func reviewRunUsage(res harness.Result) ReviewRunUsage {
	return ReviewRunUsage{
		Input:      res.Usage.Input,
		Output:     res.Usage.Output,
		Reasoning:  res.Usage.Reasoning,
		CacheRead:  res.Usage.CacheRead,
		CacheWrite: res.Usage.CacheWrite,
		CostUSD:    res.CostUSD,
	}
}

// reviewRunTurns reports the review's own turns: one for a single-pass
// invocation, and for a maps-session one entry per turn that ran (TurnMessages
// includes a turn that ended the sequence early). The harness's one report
// re-emit and a stall or interrupt resume are the harness's turns, not the
// review's, so neither is counted.
func reviewRunTurns(method string, res harness.Result) int {
	if method != config.ReviewMethodMapsSession {
		return 1
	}
	return len(res.TurnMessages)
}

// NormalizeReviewMethod normalises the --method vocabulary. "single" is
// accepted as the shorthand for the shipped method so the flag reads naturally,
// but the report always names the canonical value. The CLI and the driver share
// it so the two cannot disagree about what a spelling means.
func NormalizeReviewMethod(method string) (string, error) {
	switch strings.TrimSpace(method) {
	case "", "single", config.ReviewMethodSinglePass:
		return config.ReviewMethodSinglePass, nil
	case config.ReviewMethodMapsSession:
		return config.ReviewMethodMapsSession, nil
	default:
		return "", fmt.Errorf("unknown review method %q: want %q (or \"single\") or %q", method, config.ReviewMethodSinglePass, config.ReviewMethodMapsSession)
	}
}

// reviewRunRepo resolves the checkout the review reads from. The worktree add
// runs inside it, so it must be a real path now rather than a later surprise.
func reviewRunRepo(repo string) (string, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		repo = "."
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", fmt.Errorf("resolve --repo %q: %w", repo, err)
	}
	if fi, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("--repo %q: %w", repo, err)
	} else if !fi.IsDir() {
		return "", fmt.Errorf("--repo %q is not a directory", repo)
	}
	return abs, nil
}

// reviewRunCommit resolves a revision to the full commit SHA the report and the
// brief carry. Resolving before the worktree exists means a typo fails with the
// revision named, not with git's worktree error.
func reviewRunCommit(repo, label, rev string) (string, error) {
	rev = strings.TrimSpace(rev)
	if rev == "" {
		return "", fmt.Errorf("--%s is required: the review diffs --base..--head", label)
	}
	out, err := reviewGit(repo, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve --%s %q to a commit in %s: %w%s", label, rev, repo, err, reviewGitDetail(out))
	}
	return strings.TrimSpace(out), nil
}

// reviewGit runs one git command in dir and returns its combined output.
func reviewGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// reviewGitDetail renders git's own message for an error line, or nothing when
// it said nothing (the command's exit status is then the whole story).
func reviewGitDetail(out string) string {
	if s := strings.TrimSpace(out); s != "" {
		return ": " + s
	}
	return ""
}

// removeReviewWorktree removes the temporary worktree and its enclosing temp
// root, then prunes the metadata git keeps for it. Best effort on every step:
// the directory may already be gone, and the prune is what keeps a failed
// removal from leaving a stale worktree record in the operator's repository.
func removeReviewWorktree(repo, path string) error {
	var errs []error
	if out, err := reviewGit(repo, "worktree", "remove", "--force", path); err != nil {
		errs = append(errs, fmt.Errorf("git worktree remove %s: %w%s", path, err, reviewGitDetail(out)))
	}
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		errs = append(errs, fmt.Errorf("remove %s: %w", filepath.Dir(path), err))
	}
	if out, err := reviewGit(repo, "worktree", "prune"); err != nil {
		errs = append(errs, fmt.Errorf("git worktree prune: %w%s", err, reviewGitDetail(out)))
	}
	return errors.Join(errs...)
}
