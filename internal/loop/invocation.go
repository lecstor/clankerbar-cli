package loop

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/events"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// diffBase is the commit a brief's `git diff` starts from: the runner's merge-base SHA when
// worktree prep computed one, else origin/<prBase> (git computes the merge-base itself, and the
// prep fetch refreshed the ref). Empty only when no PR base is known: the placeholder then stays
// standing, as any other placeholder does when its fact is absent.
func diffBase(prBase, baseSHA string) string {
	if baseSHA != "" {
		return baseSHA
	}
	if prBase != "" {
		return "origin/" + prBase
	}
	return ""
}

// invocationFor builds the invocation for one PHASE: the target's invocation, carrying this phase's
// prompt and turn cap, with the previous phase's claim substituted into the prompt's placeholders.
// A session cannot know its own run id — it never called claim_task — so the driver, which
// watched the earlier phase's stream go by, is the only thing that can tell it.
func (d *Driver) invocationFor(t Target, phaseIdx int, ph config.Phase, prev *harness.Result, spawnDir, prBase string, phases []config.Phase, runner *runnerBriefing, baseSHA string) (harness.Invocation, error) {
	inv, _, err := d.invocationForPhase(t, phaseIdx, ph, prev, spawnDir, prBase, phases, runner, baseSHA)
	return inv, err
}

// invocationForPhase is invocationFor plus the phase as it was RESOLVED: a review-tier escalation
// rewrites the phase's tier and drops its pinned model, so a caller that reports the session's
// configured route set (the limit beacon's hold breakdown) must read the rule's routes, not the
// configured pin (review 1.review-1.3).
func (d *Driver) invocationForPhase(t Target, phaseIdx int, ph config.Phase, prev *harness.Result, spawnDir, prBase string, phases []config.Phase, runner *runnerBriefing, baseSHA string) (harness.Invocation, config.Phase, error) {
	tc := d.targetCfg(t)
	inv, err := d.invocationOn(t, tc.HarnessFor(ph), false)
	if err != nil {
		return inv, ph, err
	}
	if spawnDir != "" {
		inv.WorkDir = spawnDir
	}
	inv.ExtraDirs = d.extraDirsFor(t, spawnDir)
	inv.MaxTurns = ph.MaxTurns
	// The per-session runaway ceiling, resolved once per drain from the budget: the operator's own
	// dial, else 2x max_tokens, else the documented floor.
	inv.MaxSessionTokens = tc.Budget.SessionTokenCeilingFor(tc.HarnessFor(ph))
	// The per-session wall-clock cap, resolved by EffectivePhases: this phase's own, else the run-wide
	// one, else zero — and zero is OFF, which is the shipped default.
	inv.MaxSessionWallClock = ph.MaxWallClock.Duration()
	// CLA-739: the repeated-failing-tool-call kill. Default on (5), 0 off.
	inv.MaxRepeatedToolFailures = tc.RepeatedToolFailureBound()
	// The no-output watchdog, armed per harness: its adapter enforces the bounds, the loop only resolves them.
	inv.MaxSessionIdle, inv.MaxToolCallDuration = tc.SessionIdleFor(tc.HarnessFor(ph))
	inv.Prompt = ph.Prompt
	// The phase's tier, or the run-wide model when it names none. Reported when a tier was named and
	// resolved to nothing, because that is a typo in the operator's map and the session otherwise runs
	// on the default with nothing said — the quiet failure, since a tier is set precisely when the
	// default is not what was wanted.
	model, ok := tc.ModelForPhase(ph)
	if !ok {
		log.Printf("%sphase %q names tier %q, which resolves to no model on harness %q — running on that harness's default model instead",
			labelOf(t), ph.Label(phaseIdx), ph.Tier, tc.HarnessFor(ph))
	}

	// CLA-379: review-tier escalation evaluated at review-spawn time.
	if tc.Escalation.PathRules != nil || tc.Escalation.CategoryRules != nil {
		if isReviewPhase(ph) {
			// Resolve the branch name for the diff: from the previous claim's branch record, falling back to
			// reading the workdir's git refs if the claim carries none.
			branchName := ""
			if prev != nil && prev.Claim.Held() && prev.Claim.Branch != "" {
				branchName = prev.Claim.Branch
			}
			changedPaths, _ := d.diffAgainstIntegration(t, branchName, spawnDir)
			category := ""
			if prev != nil && prev.Claim.Held() && prev.Claim.Category != "" {
				category = prev.Claim.Category
			}
			tier, rule := tc.Escalation.Evaluate(changedPaths, category)
			if tier != "" {
				// A pinned Phase.Model would otherwise make the rule INERT: ModelForPhase resolves Model before
				// Tier, so setting the tier alone leaves the session on the pinned alias while the log claims
				// an escalation.
				raised := model
				ph.Tier = tier
				ph.Model = nil
				model, ok = tc.ModelForPhase(ph)
				if !ok {
					log.Printf("%sescalated tier %q resolves to no model — running on harness default", labelOf(t), tier)
				}
				// An escalation that lands on the model the phase already ran changed nothing, and logging one
				// would attribute spend to a rule that made no difference.
				if model != raised {
					log.Printf("%sreview escalated to tier %q: %s (changed paths: %v, category=%q, branch=%q)", labelOf(t), tier, rule, changedPaths, category, branchName)
				}
			}
		}
	}

	// CLA-782: the ordered route list this phase may fall back through, with this run's marks applied.
	choice := d.routeRoutesFor(tc, ph)
	switch {
	case len(choice.Routes) > 0:
		inv.Model = choice.Routes[0]
		inv.Routes = choice.Routes
	case !choice.WaitUntil.IsZero():
		return inv, ph, &routeWindowBlocked{phase: ph.Label(phaseIdx), until: choice.WaitUntil, routes: choice.WindowBlocked}
	default:
		inv.Model = model
		inv.Routes = nil
	}
	if prev != nil && prev.Claim.Held() {
		// The {{branch}} placeholder is filled from the recorded, VERIFIED hand-off: the checkpoint the
		// driver ran against this claim already confirmed the branch is on the origin remote, so the
		// review brief can name it instead of asserting an unverified phase-1 success.
		prompt := inv.Prompt
		// CLA-639 M5: a panel MEMBER writes the durable record and nothing else on the task, so its brief
		// is the panel variant — same core, no outcome write.
		panelMember := phaseIdx < len(phases) && phases[phaseIdx].IsPanelMember()
		prompt = variantPrompt(prompt, prev, panelMember)
		prompt = runnerPrompt(prompt, runner, panelMember)
		inv.Prompt = prompt
		// Seed the claim as well as the prompt. The session is told not to claim, so the adapter would
		// observe none — and Result.Claim.Held() is what gates the handback, the salvage and the
		// delivery check.
		inv.ResumeClaim = prev.Claim
	}
	// Every placeholder is filled in ONE pass (CLA-split review finding).
	phaseTotal := len(phases)
	// The attribution the brief renders and the done report's record is signed with are resolved once,
	// together, so the two can never disagree.
	attr, role := d.doneAttribution(t, ph, phases, phaseIdx, inv)
	repl := []string{
		config.PhaseHarnessPlaceholder, attributionOr(attr.Harness, "unknown harness"),
		config.PhaseModelPlaceholder, attributionOr(attr.Model, "harness default"),
		config.PhasePrevHarnessPlaceholder, attributionOr(attr.PrevHarness, "unknown harness"),
		config.PhasePrevModelPlaceholder, attributionOr(attr.PrevModel, "harness default"),
		config.PhaseNumberPlaceholder, strconv.Itoa(phaseIdx + 1),
		config.PhaseTotalPlaceholder, strconv.Itoa(phaseTotal),
	}
	if prBase != "" {
		repl = append(repl, config.PhasePRBasePlaceholder, prBase)
	}
	// {{baseSha}} is the one diff base: the runner's merge-base SHA when its worktree prep computed
	// one, else origin/<prBase>.
	if runner != nil {
		baseSHA = runner.baseSHA
	}
	if base := diffBase(prBase, baseSHA); base != "" {
		repl = append(repl, config.PhaseBasePlaceholder, base)
	}
	if prev != nil && prev.Claim.Held() {
		branch := verifiedBranch(prev)
		if branch == "" && runner != nil && (inv.Prompt == config.RunnerImplementBrief() || inv.Prompt == config.RunnerCloseOutBrief()) {
			// A fresh claim has no recorded branch yet; the runner created the worktree on the plane's name.
			branch = runner.task.Branch
		}
		repl = append(repl,
			config.PhaseTaskPlaceholder, prev.Claim.TaskID,
			config.PhaseRunPlaceholder, prev.Claim.RunID,
			config.PhaseBranchPlaceholder, branch,
		)
		// CLA-637: the durable review record's identity. The role is the driver's assignment for THIS
		// phase, and the tip is the origin commit the driver verified at the checkpoint that opened the
		// phase — the two facts a session cannot see for itself.
		if role != "" {
			repl = append(repl, config.ReviewerRolePlaceholder, role)
		}
		if prev.CheckpointTip != "" {
			repl = append(repl, config.ReviewedTipPlaceholder, prev.CheckpointTip)
		}
	}
	inv.Prompt = strings.NewReplacer(repl...).Replace(inv.Prompt)
	return inv, ph, nil
}

// invocationOn builds the invocation for one target ON A NAMED HARNESS: the project's workdir and
// MCP config over that harness's own fields. The harness-shaped fields (config dir, MCP config,
// settings, model) come from config.SessionFor, which fills the run-wide values in for the
// top-level harness only — so a single-harness run resolves to exactly what it always did, and a
// phase on another harness gets that harness's block or nothing, never the top level's
// claude-shaped paths.
func (d *Driver) invocationOn(t Target, harnessName string, probe bool) (harness.Invocation, error) {
	// The target's effective config supplies the model/tier half of SessionFor and decides which
	// harness the project's own MCP file belongs to; the env, prompt and workdir fallbacks stay on the
	// base config — they are machine-local core the plane never carries.
	tc := d.targetCfg(t)
	workdir := t.WorkDir
	if workdir == "" {
		workdir = d.cfg.WorkDir
	}
	hc := tc.SessionFor(harnessName)
	env, err := d.cfg.SessionEnv(harnessName, t.Name)
	if err != nil {
		return harness.Invocation{}, err
	}
	inv := harness.Invocation{
		Prompt:        d.cfg.Prompt,
		Model:         hc.Model.First(),
		WorkDir:       workdir,
		MCPConfigPath: tc.ResolveMCPConfig(harnessName, t.MCPConfigPath, t.MCPConfigPaths),
		ConfigDir:     hc.ConfigDir,
		SettingsPath:  hc.SettingsPath,
		Env:           env,
		EnvAllowlist:  d.cfg.SessionEnvAllowlist(harnessName, t.Name),
		Nice:          d.cfg.SessionNice,
		Probe:         probe,
		NoBacklogMCP:  probe,
	}
	// The agent event channel. The loop's top-level probe is a one-word liveness check, not a session
	// doing work, so it never emits.
	if !probe && d.eventSenderOn(tc, harnessName) {
		name := harnessName
		transcript := tc.Events.Transcript
		inv.NewEventSink = func() *events.Emitter { return d.newEventEmitter(name, transcript) }
	}
	// Console -> agent messages: independent of the event dials.
	if d.inboxOn(harnessName, probe) {
		inv.Inbox = &harness.InboxConfig{Origin: d.inboxOrigin, APIKey: d.inboxKey}
	}
	return inv, nil
}

// sessionDir resolves where a PHASE's sessions run: the checkout of the task they will work, not
// the multi-repo parent. The peek cannot race a session out of work (it holds nothing), and if
// another clanker does claim the peeked task first, the spawned session claims elsewhere and still
// works: the permission policy covers every declared repo regardless of cwd, which is what makes a
// best-effort cwd safe rather than load-bearing.
func (d *Driver) sessionDir(ctx context.Context, t Target, prev *harness.Result) (dir, repo string, err error) {
	if prev != nil && prev.Claim.Held() {
		if src, ok := t.Releaser.(plane.TaskRepoSource); ok {
			r, rerr := src.TaskRepo(ctx, prev.Claim.TaskID)
			switch {
			case rerr == nil:
				repo = r
			case errors.Is(rerr, plane.ErrNotWired):
				// No plane endpoint/key: nothing to read, the fallback chain runs.
			default:
				log.Printf("%scould not read the repo of resumed task %s (%v) — falling back to the primary repo", labelOf(t), claimLabel(prev.Claim), rerr)
			}
		}
	} else if peeker, ok := t.Releaser.(plane.TaskPeeker); ok {
		next, perr := peeker.PeekNextTask(ctx)
		switch {
		case perr == nil:
			repo = next.Repo
			if next.Repo != "" {
				log.Printf("%squeue head %s names repo %q — resolving it to this phase's start directory", labelOf(t), next.TaskID, next.Repo)
			}
		case errors.Is(perr, plane.ErrNotWired):
		default:
			log.Printf("%scould not peek the next task's repo (%v) — falling back to the primary repo", labelOf(t), perr)
		}
	}
	dir, err = d.checkoutFor(t, repo)
	return dir, repo, err
}

// checkoutFor resolves a repo identity to the local checkout this target runs the project in. With
// the per-project layout declared, the session must START inside the project root its permission
// scope is bounded to.
func (d *Driver) checkoutFor(t Target, repo string) (string, error) {
	workdir := t.WorkDir
	if workdir == "" {
		workdir = d.cfg.WorkDir
	}
	dir, err := config.ResolveCheckout(t.Repos, t.PrimaryRepo, workdir, repo)
	if err != nil {
		return dir, err
	}
	if t.ProjectRoot && dir != "" && !insideProjectRoot(workdir, dir) {
		return dir, fmt.Errorf("repo checkout %s resolves outside the project root %s - refusing to start the session outside its declared scope", dir, workdir)
	}
	return dir, nil
}

// extraDirsFor lists the directories a session's permission policy must cover BESIDES where it
// runs: every checkout the project declares, whatever directory the session starts in
// (agent-rule-scoping piece 2). Task worktrees are separate git roots OUTSIDE every declared repo,
// and a session that can create one but not edit in it is worse than one never started.
func (d *Driver) extraDirsFor(t Target, spawnDir string) []string {
	workdir := t.WorkDir
	if workdir == "" {
		workdir = d.cfg.WorkDir
	}
	// With the per-project layout declared, every grant must stay INSIDE the project root.
	root := ""
	if t.ProjectRoot && workdir != "" {
		root = workdir
	}
	declared := config.DeclaredCheckouts(t.Repos, t.PrimaryRepo, workdir)
	var out []string
	seen := map[string]bool{}
	add := func(dir string) {
		if dir == "" || dir == spawnDir || seen[dir] {
			return
		}
		seen[dir] = true
		out = append(out, dir)
	}
	for _, dir := range declared {
		if root != "" && !insideProjectRoot(root, dir) {
			log.Printf("%sdeclared checkout %s is outside the project root %s - not adding it to the session's permissions", labelOf(t), dir, workdir)
			continue
		}
		add(dir)
	}
	if spawnDir != "" {
		wt := filepath.Join(filepath.Dir(spawnDir), filepath.Base(spawnDir)+"-wt")
		if fi, err := os.Stat(wt); err == nil && fi.IsDir() {
			if root == "" || insideProjectRoot(root, wt) {
				add(wt)
			}
		}
	}
	// The PROJECT ROOT and its children, when the config declares the per-project layout
	// (`project_root`): a session starts in <project root>/<repo name>, and the rest of its project
	// — the other repos, and the worktrees directory every task worktree lives in — sits beside it
	// under the same root.
	if t.ProjectRoot {
		if scope := projectScope(workdir, spawnDir); scope != "" {
			add(scope)
			for _, child := range childDirs(scope) {
				add(child)
			}
		}
	}
	return out
}

// projectScope reports the PROJECT ROOT a session's reach is bounded to: the workdir, when the
// session starts strictly INSIDE it — the per-project layout, where the session runs in <project
// root>/<repo name> and the project's other repos and its worktrees sit beside that checkout.
func projectScope(workdir, spawnDir string) string {
	if workdir == "" || spawnDir == "" {
		return ""
	}
	wd, sd := absOrSelfPath(workdir), absOrSelfPath(spawnDir)
	if wd == sd {
		return ""
	}
	rel, err := filepath.Rel(wd, sd)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(filepath.ToSlash(rel), "../") {
		return ""
	}
	return wd
}

// insideProjectRoot reports whether path is the project root itself or lies inside it. Both sides
// are compared on canonical paths, so a symlinked checkout cannot masquerade as inside the root —
// the supervisor's derivation refuses symlinks at the derived checkout, and this is the driver-side
// containment the additive permission list needs.
func insideProjectRoot(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	r, p := canonicalPath(root), canonicalPath(path)
	if r == p {
		return true
	}
	rel, err := filepath.Rel(r, p)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	return rel != ".." && !strings.HasPrefix(rel, "../")
}

// canonicalPath resolves a path for containment comparison: absolute, cleaned, and with symlinks
// resolved when the path exists. A path that exists only lexically (a missing entry) is returned
// absolute-but-unresolved — the comparison then treats it by name, never as some other directory.
func canonicalPath(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	abs = filepath.Clean(abs)
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// childDirs lists the immediate subdirectories of dir that exist, sorted.
func childDirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}

// absOrSelfPath resolves a path against the process cwd, returning it unchanged when it cannot be
// resolved (the same posture as the harness package's absOrSelf: an unresolvable path must not
// become a different path).
func absOrSelfPath(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// variantPrompt swaps a built-in brief for the variant the carried checkpoint needs (branchless,
// unverified, panel member). prev must be a held claim.
func variantPrompt(prompt string, prev *harness.Result, panelMember bool) string {
	switch {
	case prompt == config.ReviewOnlyBrief():
		switch {
		case prev.Claim.Branch == "":
			prompt = config.NoCodeReviewOnlyBrief()
		case prev.CheckpointUnverified:
			if panelMember {
				prompt = config.PanelMemberUnverifiedReviewOnlyBrief()
			} else {
				prompt = config.UnverifiedReviewOnlyBrief()
			}
		case panelMember:
			prompt = config.PanelMemberReviewOnlyBrief()
		}
	case prompt == config.BuiltinReviewBrief():
		switch {
		case prev.Claim.Branch == "":
			prompt = config.NoCodeReviewBrief()
		case prev.CheckpointUnverified:
			prompt = config.UnverifiedReviewBrief()
		}
	case prompt == config.FixBrief() && prev.Claim.Branch == "":
		// The FIX phase has no unverified variant: its brief asserts no verification at all.
		prompt = config.NoCodeFixBrief()
	case prompt == config.FixVerifyBrief() && prev.Claim.Branch == "":
		// Same for the fix phase that a verification round follows, in the shape whose fix is a record
		// amendment.
		prompt = config.NoCodeFixVerifyBrief()
	case prompt == config.VerifyBrief() && prev.Claim.Branch == "":
		// A verification round on a branchless checkpoint has no code delta to re-review; its brief
		// adjudicates the record's findings.
		prompt = config.NoCodeVerifyBrief()
	}
	return prompt
}
