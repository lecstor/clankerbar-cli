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
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// diffBase is the commit a brief's `git diff` starts from: the runner's
// merge-base SHA when worktree prep computed one, else origin/<prBase> (git
// computes the merge-base itself, and the prep fetch refreshed the ref). Empty
// only when no PR base is known: the placeholder then stays standing, as any
// other placeholder does when its fact is absent.
func diffBase(prBase, baseSHA string) string {
	if baseSHA != "" {
		return baseSHA
	}
	if prBase != "" {
		return "origin/" + prBase
	}
	return ""
}

// invocationFor builds the invocation for one PHASE: the target's invocation,
// carrying this phase's prompt and turn cap, with the previous phase's claim
// substituted into the prompt's placeholders.
//
// spawnDir is where this phase's sessions run, resolved once per phase by
// sessionDir — the task repo's checkout when one is known, else the target's
// workdir exactly as before. It is resolved by the CALLER rather than here so a
// retry ladder re-spawns into the SAME directory it started in (the task has not
// changed between attempts), and so a resolution failure fails the phase before
// anything is spawned.
//
// Unlike the directory, the session env is resolved per CALL, not once per
// phase: every attempt re-runs its fromCommand declarations fresh, so a token
// rotated mid-ladder still reaches the next attempt (CLA-462).
//
// The substitution is what lets a later phase RESUME a run rather than claim a
// task. A session cannot know its own run id — it never called claim_task — so
// the driver, which watched the earlier phase's stream go by, is the only thing
// that can tell it. That is also why the ids come from the observed claim and
// not from config: they are per-task facts, discovered at runtime.
//
// A placeholder with no claim to fill it is LEFT STANDING rather than blanked.
// A literal {{runId}} in the log is a misconfigured sequence announcing itself;
// an empty string is a session quietly deciding to claim fresh work instead.
//
// prBase is the branch this task's repo PRs target, resolved per phase by
// drainPhase (CLA-608), and fills {{prBase}} in EVERY phase: the other three
// placeholders are per-run facts only a claim can carry, while the implement
// brief — phase 1, claim-free at spawn time — is itself one of the carriers, in
// its resumed-work rule. Empty leaves the placeholder standing, like the rest.
//
// runner is the runner-claimed briefing, nil for the session-claim path: it
// swaps the shipped briefs for the ones that assume the runner already holds the
// claim, and names the branch a fresh claim has not recorded yet.
//
// phases is the sequence as CAPTURED when the drain started, and supplies every
// positional/attribution fact here — the "PHASE i of N" total and the previous
// phase's harness/model. A RELOAD applied inside a wait swaps the live config
// under an in-flight sequence (CLA-664), and the sequence must keep its own
// shape and attribution until the next drain.
func (d *Driver) invocationFor(t Target, phaseIdx int, ph config.Phase, prev *harness.Result, spawnDir, prBase string, phases []config.Phase, runner *runnerBriefing) (harness.Invocation, error) {
	tc := d.targetCfg(t)
	inv, err := d.invocationOn(t, tc.HarnessFor(ph), false)
	if err != nil {
		return inv, err
	}
	if spawnDir != "" {
		inv.WorkDir = spawnDir
	}
	inv.ExtraDirs = d.extraDirsFor(t, spawnDir)
	inv.MaxTurns = ph.MaxTurns
	// The per-session runaway ceiling, resolved once per drain from the budget:
	// the operator's own dial, else 2x max_tokens, else the documented floor.
	// 0 never reaches here — SessionTokenCeiling has no disabled state, because
	// the whole point of CLA-343 is that nothing was able to stop the 285.9M
	// session.
	// Asked PER HARNESS since CLA-367: a token ceiling that lives in this
	// harness's own per_harness block derives the detector exactly as the global
	// dial does, so moving a ceiling into a block does not loosen it. And the
	// harness asked about is THIS PHASE's (CLA-366), not the run's — the ceiling
	// is compiled into the invocation that is about to spawn, so keying it on d.h
	// would hand an opencode phase the ceiling derived from claude's block.
	inv.MaxSessionTokens = tc.Budget.SessionTokenCeilingFor(tc.HarnessFor(ph))
	// The per-session wall-clock cap, resolved by EffectivePhases: this phase's
	// own, else the run-wide one, else zero — and zero is OFF, which is the
	// shipped default (CLA-368). Unlike the token ceiling this one HAS a disabled
	// state, because no default number is honest across models and providers.
	inv.MaxSessionWallClock = ph.MaxWallClock.Duration()
	// CLA-739: the repeated-failing-tool-call kill. Default on (5), 0 off.
	inv.MaxRepeatedToolFailures = tc.RepeatedToolFailureBound()
	// The no-output watchdog, armed per harness: its adapter enforces the bounds, the loop only resolves them.
	inv.MaxSessionIdle, inv.MaxToolCallDuration = tc.SessionIdleFor(tc.HarnessFor(ph))
	inv.Prompt = ph.Prompt
	// The phase's tier, or the run-wide model when it names none. Reported when a
	// tier was named and resolved to nothing, because that is a typo in the
	// operator's map and the session otherwise runs on the default with nothing
	// said — the quiet failure, since a tier is set precisely when the default is
	// not what was wanted.
	//
	// "resolves to no model" rather than "the map does not define it": ModelForTier
	// also says not-ok for a bucket that IS in the map and is mapped to blank or
	// whitespace, and telling that operator their map lacks a bucket they can see
	// sitting in it would send them looking in the wrong place — the more so
	// because the whitespace is the thing they cannot see. Label(), not Name, so a
	// phase carrying a custom prompt and no name is still identified.
	//
	// Resolved against the tier map of the harness THIS PHASE runs on, not the
	// run's: a bucket name is policy and travels, the alias inside it is a
	// provider's and does not. An opencode phase whose harness block names no
	// models therefore runs on opencode's own configured model — which is where
	// opencode's model lives — rather than on a claude alias its CLI would die on.
	model, ok := tc.ModelForPhase(ph)
	if !ok {
		log.Printf("%sphase %q names tier %q, which resolves to no model on harness %q — running on that harness's default model instead",
			labelOf(t), ph.Label(phaseIdx), ph.Tier, tc.HarnessFor(ph))
	}

	// CLA-379: review-tier escalation evaluated at review-spawn time.
	// The CLI reads the operator's escalation rules from config and applies
	// them mechanically. Only raises; never lowers. The matching rule
	// is logged so the spend is attributable, per the 2026-08-23 decision.
	if tc.Escalation.PathRules != nil || tc.Escalation.CategoryRules != nil {
		if isReviewPhase(ph) {
			// Resolve the branch name for the diff: from the previous claim's
			// branch record (CLA-379 plumbing), falling back to reading the
			// workdir's git refs if the claim carries none.
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
				// A pinned Phase.Model would otherwise make the rule INERT:
				// ModelForPhase resolves Model before Tier, so setting the tier
				// alone leaves the session on the pinned alias while the log
				// claims an escalation (CLA-379's whole point is to raise the
				// review to the escalated tier's alias). The rule fires on a
				// phase, not the operator's per-phase wish, so the tier wins and
				// the pinned alias is dropped.
				raised := model
				ph.Tier = tier
				ph.Model = nil
				model, ok = tc.ModelForPhase(ph)
				if !ok {
					log.Printf("%sescalated tier %q resolves to no model — running on harness default", labelOf(t), tier)
				}
				// An escalation that lands on the model the phase already ran
				// changed nothing, and logging one would attribute spend to a
				// rule that made no difference.
				if model != raised {
					log.Printf("%sreview escalated to tier %q: %s (changed paths: %v, category=%q, branch=%q)", labelOf(t), tier, rule, changedPaths, category, branchName)
				}
			}
		}
	}

	// CLA-782: the ordered route list this phase may fall back through, with
	// this run's marks applied. ModelForPhase (and therefore inv.Model, the spawn
	// line and the brief placeholder) keeps naming the FIRST routable route, so a
	// single-route config is unchanged; the full list is what the adapter switches
	// between when a provider denies the free route mid-session.
	routes := d.routeRoutesFor(tc, ph)
	if len(routes) > 0 {
		inv.Model = routes[0]
	} else {
		inv.Model = model
	}
	inv.Routes = routes
	if prev != nil && prev.Claim.Held() {
		// The {{branch}} placeholder is filled from the recorded, VERIFIED hand-off
		// (CLA-457): the checkpoint the driver ran against this claim already
		// confirmed the branch is on the origin remote, so the review brief can
		// name it instead of asserting an unverified phase-1 success. verifiedBranch
		// reads the accepted reports first (the plane record of what THIS run
		// recorded), falling back to the claim's own branch for a resumed-with-WIP
		// predecessor.
		//
		// CLA-497: a checkpoint evidenced by the PLANE'S RECORD - the task left
		// `ready` or declared a no-code delivery - names no branch, and the
		// branch-shaped builtin brief would tell its successor the hand-off
		// FAILED ("an empty branch field ... is a FAILED hand-off to report"),
		// the exact opposite of the checkpoint the driver just recorded. Swap in
		// the no-code brief for the BUILT-IN prompt only; an operator's custom
		// prompt owns its own wording. The empty claim branch is the
		// discriminator because drainPhase holds "claim.Branch is the verified
		// branch, or empty when the evidence was branch-less" by construction.
		//
		// CLA-576: an UNVERIFIED checkpoint carries its recorded branch but no
		// verification - the origin could not be read - so the branch-shaped
		// builtin brief's "a branch the driver verified to exist on the origin
		// remote" is the one claim that must not reach the successor. It gets
		// the unverified variant instead, again for the built-in prompt only.
		//
		// The split adds a THIRD mode: a non-final review runs the review-only
		// brief, whose variants must stay review-only (record, never fix or
		// hand over). The mode is selected by the resolved prompt text, not by a
		// flag, so a custom prompt is never rewritten into a shipped one.
		prompt := inv.Prompt
		// CLA-639 M5: a panel MEMBER writes the durable record and nothing else
		// on the task, so its brief is the panel variant — same core, no outcome
		// write. A branchless checkpoint has no panel variant and must not be
		// given one: its record IS the legacy block (no live holder accepts a
		// durable one), so the first member keeps the shipped no-code brief and
		// the driver skips the members after it (see drainPhases).
		panelMember := phaseIdx < len(phases) && phases[phaseIdx].IsPanelMember()
		prompt = variantPrompt(prompt, prev, panelMember)
		prompt = runnerPrompt(prompt, runner, panelMember)
		inv.Prompt = prompt
		// Seed the claim as well as the prompt. The session is told not to claim,
		// so the adapter would observe none — and Result.Claim.Held() is what gates
		// the handback, the salvage and the delivery check. Without this, the phase
		// that pushes the branch and opens the PR is the one running with all three
		// switched off.
		inv.ResumeClaim = prev.Claim
	}
	// Every placeholder is filled in ONE pass (CLA-split review finding). Three
	// sequential replacers ran over each other's OUTPUT, so a claim-derived value
	// that happened to contain another placeholder — a branch named
	// `clanker/{{model}}`, which is a legal git ref — would be corrupted by the
	// later pass. One replacer cannot rescan its own output, so a substituted
	// value is final whatever it contains.
	//
	// The attribution/position values are config- and spawn-derived facts only the
	// driver has: a session cannot reliably report its own model, and it cannot
	// see the previous phase's at all. prevModel is the PREVIOUS phase's resolved
	// model from the static config, which is exact for the shipped shapes: tier
	// escalation applies only to a review phase, and the shipped consumer of
	// {{prevModel}} is a review brief naming the IMPLEMENT phase.
	// The positional/attribution values are facts of the sequence that was
	// captured at drain start (CLA-664), not of the live config: a RELOAD landing
	// mid-drain must not rewrite the "PHASE i of N" text of the sequence already
	// in flight, nor attribute the previous phase to a shape it never ran.
	phaseTotal := len(phases)
	// The attribution the brief renders and the done report's record is signed
	// with are resolved once, together, so the two can never disagree.
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
	// {{baseSha}} is the one diff base: the runner's merge-base SHA when its
	// worktree prep computed one, else origin/<prBase>. Filled for every phase,
	// runner-claimed or not, because it needs no claim - only the repo's base.
	baseSHA := ""
	if runner != nil {
		baseSHA = runner.baseSHA
	}
	if base := diffBase(prBase, baseSHA); base != "" {
		repl = append(repl, config.PhaseBasePlaceholder, base)
	}
	if prev != nil && prev.Claim.Held() {
		branch := verifiedBranch(prev)
		if branch == "" && runner != nil && (inv.Prompt == config.RunnerImplementBrief() || inv.Prompt == config.RunnerCloseOutBrief()) {
			// A fresh claim has no recorded branch yet; the runner created the
			// worktree on the plane's name.
			branch = runner.task.Branch
		}
		repl = append(repl,
			config.PhaseTaskPlaceholder, prev.Claim.TaskID,
			config.PhaseRunPlaceholder, prev.Claim.RunID,
			config.PhaseBranchPlaceholder, branch,
		)
		// CLA-637: the durable review record's identity. The role is the
		// driver's assignment for THIS phase, and the tip is the origin commit
		// the driver verified at the checkpoint that opened the phase — the two
		// facts a session cannot see for itself. Filled for every phase rather
		// than keyed on the prompt text: a handoff respawn's prompt is the
		// continuation, not one of the briefs, and replacing an absent
		// placeholder is a no-op either way. The tip is only filled when the
		// checkpoint has one: the unverified and branchless variants carry no
		// {{reviewedTip}} at all, and the verified variant is only ever chosen
		// for a checkpoint whose verified branch check read a tip.
		if role != "" {
			repl = append(repl, config.ReviewerRolePlaceholder, role)
		}
		if prev.CheckpointTip != "" {
			repl = append(repl, config.ReviewedTipPlaceholder, prev.CheckpointTip)
		}
	}
	inv.Prompt = strings.NewReplacer(repl...).Replace(inv.Prompt)
	return inv, nil
}

// invocationOn builds the invocation for one target ON A NAMED HARNESS: the
// project's workdir and MCP config over that harness's own fields.
//
// The harness-shaped fields (config dir, MCP config, settings, model) come from
// config.SessionFor, which fills the run-wide values in for the top-level
// harness only — so a single-harness run resolves to exactly what it always did,
// and a phase on another harness gets that harness's block or nothing, never the
// top level's claude-shaped paths.
//
// The MCP config is the one field with two claimants, because it carries two
// facts at once: which PROJECT (its /mcp/<slug>) and which SCHEMA (the
// harness's). The project's per-harness file answers both and wins; the
// project's own file is the top-level harness's, so it only applies there; and
// the harness block's path is the single-project fallback. config.Validate
// refuses the combination that would silently resolve to the wrong project.
//
// The session env is composed HERE, at spawn time, fresh every call (CLA-462):
// the four-level env overlay with every fromCommand executed now, so a rotated
// token reaches this session without a daemon restart. An error — a failing,
// timed-out or empty-output command, an unreadable @path file — REFUSES the
// spawn: the caller reports it and no session starts, because a session without
// its declared env is the incident, not a degraded mode.
func (d *Driver) invocationOn(t Target, harnessName string, probe bool) (harness.Invocation, error) {
	// The target's effective config supplies the model/tier half of SessionFor
	// and decides which harness the project's own MCP file belongs to (CLA-410);
	// the env, prompt and workdir fallbacks stay on the base config — they are
	// machine-local core the plane never carries.
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
	return harness.Invocation{
		Prompt:        d.cfg.Prompt,
		Model:         hc.Model.First(),
		WorkDir:       workdir,
		MCPConfigPath: tc.ResolveMCPConfig(harnessName, t.MCPConfigPath, t.MCPConfigPaths),
		ConfigDir:     hc.ConfigDir,
		SettingsPath:  hc.SettingsPath,
		Env:           env,
		EnvAllowlist:  d.cfg.SessionEnvAllowlist(harnessName, t.Name),
		Probe:         probe,
	}, nil
}

// sessionDir resolves where a PHASE's sessions run: the checkout of the task
// they will work, not the multi-repo parent (CLA-437, agent-rule-scoping pieces
// 2 and 3). It also returns the id of the task the phase is BRIEFED on — exact
// for a resumed phase (the predecessor's claim), best-effort for a fresh one
// (the queue-head peek, the same read the session is about to make). The seam's
// checkpoint peek reads it after the phase ends (CLA-451); empty means the
// driver never knew which task to ask about, and the peek is skipped.
//
// Which task that is depends on the phase. A RESUMED phase (prev carries a held
// claim) knows its exact task, so its repo is read straight off the plane with
// get_task. A FRESH phase has claimed nothing yet — the claim happens inside the
// session — so the driver peeks at next_task, the same queue-head read the
// session is about to make, and starts in that task's repo. The peek cannot race
// a session out of work (it holds nothing), and if another clanker does claim the
// peeked task first, the spawned session claims elsewhere and still works: the
// permission policy covers every declared repo regardless of cwd, which is what
// makes a best-effort cwd safe rather than load-bearing.
//
// Resolution then runs config.ResolveCheckout over what the target declared:
// explicit repos entries, then the workdir-is-the-checkout convention, then
// <workdir>/<name>. An identity that matches nothing fails the iteration with
// repo_not_found — starting in the parent instead is exactly the wrong-repo-
// reads and cwd-scoped-grant shape this exists to kill — while an empty result
// (nothing configured anywhere) keeps the legacy workdir behaviour byte for byte.
//
// Lookup failures are degradation, not failure: a plane blip costs the spawn its
// per-task directory (it falls through to the primary-repo chain), never the
// iteration — the session about to spawn needs the plane too, and it can resolve
// nothing either way.
//
// The same read yields the task's repo IDENTITY, returned alongside so the
// caller can key the pr_base override by it (CLA-608) without a second plane
// read; "" is the same best-effort empty the checkout resolution already
// tolerates, and means only "no override can match".
func (d *Driver) sessionDir(ctx context.Context, t Target, prev *harness.Result) (string, string, string, error) {
	repo := ""
	briefed := ""
	if prev != nil && prev.Claim.Held() {
		briefed = prev.Claim.TaskID
		if src, ok := t.Releaser.(plane.TaskRepoSource); ok {
			r, err := src.TaskRepo(ctx, prev.Claim.TaskID)
			switch {
			case err == nil:
				repo = r
			case errors.Is(err, plane.ErrNotWired):
				// No plane endpoint/key: nothing to read, the fallback chain runs.
			default:
				log.Printf("%scould not read the repo of resumed task %s (%v) — falling back to the primary repo", labelOf(t), claimLabel(prev.Claim), err)
			}
		}
	} else if peeker, ok := t.Releaser.(plane.TaskPeeker); ok {
		next, err := peeker.PeekNextTask(ctx)
		switch {
		case err == nil:
			briefed = next.TaskID
			repo = next.Repo
			if next.Repo != "" {
				log.Printf("%squeue head %s names repo %q — resolving it to this phase's start directory", labelOf(t), next.TaskID, next.Repo)
			}
		case errors.Is(err, plane.ErrNotWired):
		default:
			log.Printf("%scould not peek the next task's repo (%v) — falling back to the primary repo", labelOf(t), err)
		}
	}
	workdir := t.WorkDir
	if workdir == "" {
		workdir = d.cfg.WorkDir
	}
	dir, err := config.ResolveCheckout(t.Repos, t.PrimaryRepo, workdir, repo)
	if err != nil {
		return dir, briefed, repo, err
	}
	// With the per-project layout declared, the session must START inside the
	// project root its permission scope is bounded to. ResolveCheckout prefers
	// an explicit `repos` path, so a stale override pointing at a developer
	// checkout would otherwise run the session outside the project and hand
	// that path to the policy (CLA-678 review round 2, finding 3). The
	// materialized config already rewrites the overrides; this is the
	// driver-side refusal that holds even for a config that adopted the layout
	// by hand.
	if t.ProjectRoot && dir != "" && !insideProjectRoot(workdir, dir) {
		return dir, briefed, repo, fmt.Errorf("repo checkout %s resolves outside the project root %s - refusing to start the session outside its declared scope", dir, workdir)
	}
	return dir, briefed, repo, nil
}

// extraDirsFor lists the directories a session's permission policy must cover
// BESIDES where it runs: every checkout the project declares, whatever directory
// the session starts in (agent-rule-scoping piece 2). This is the piece that
// makes a best-effort spawn cwd safe — a two-repo project whose session started
// in one repo can still reach the other.
//
// One implicit entry joins the declared ones: the conventional worktree area
// beside the spawn checkout (<checkout>-wt as its sibling, this fleet's
// documented layout). Task worktrees are separate git roots OUTSIDE every
// declared repo, and a session that can create one but not edit in it is worse
// than one never started. It is added only when the directory already exists —
// no speculation, no widening beyond what is on disk.
//
// With nothing declared and no worktree area found this returns nil, which leaves
// each adapter's policy exactly as it shipped before CLA-437.
func (d *Driver) extraDirsFor(t Target, spawnDir string) []string {
	workdir := t.WorkDir
	if workdir == "" {
		workdir = d.cfg.WorkDir
	}
	// With the per-project layout declared, every grant must stay INSIDE the
	// project root. The scope below is additive: declared checkouts are added
	// before it, so a stale `repos` override pointing at a developer checkout
	// would be granted outright even though the session is scoped to the
	// project — the escape the layout exists to close (CLA-678 review round 2,
	// finding 3). sessionDir refuses such a checkout as a spawn dir; this also
	// keeps it out of the reach list, alongside the conventional worktree area.
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
	// The PROJECT ROOT and its children (CLA-678), when the config declares the
	// per-project layout (`project_root`): a session starts in
	// <project root>/<repo name>, and the rest of its project — the other repos,
	// and the worktrees directory every task worktree lives in — sits beside it
	// under the same root. Granting the children is what makes cross-repo work
	// inside a project possible at all, and the project root itself is granted
	// so `external_directory` — the gate that decides a path-taking tool's reach
	// — is carved out for the same subtree rather than for one checkout.
	//
	// The flag is what keeps this off a developer's multi-repo parent: without
	// it a session starting in one of `~/dev`'s ~150 trees would be handed every
	// sibling it happens to have there. Nothing is granted on a guess.
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

// projectScope reports the PROJECT ROOT a session's reach is bounded to: the
// workdir, when the session starts strictly INSIDE it — the per-project layout,
// where the session runs in <project root>/<repo name> and the project's other
// repos and its worktrees sit beside that checkout. "" means there is no scope
// to widen, which is every legacy shape (the workdir is the checkout itself, or
// the session starts nowhere near it).
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

// insideProjectRoot reports whether path is the project root itself or lies
// inside it. Both sides are compared on canonical paths, so a symlinked
// checkout cannot masquerade as inside the root — the supervisor's derivation
// refuses symlinks at the derived checkout, and this is the driver-side
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

// canonicalPath resolves a path for containment comparison: absolute, cleaned,
// and with symlinks resolved when the path exists. A path that exists only
// lexically (a missing entry) is returned absolute-but-unresolved — the
// comparison then treats it by name, never as some other directory.
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

// childDirs lists the immediate subdirectories of dir that exist, sorted. It is
// how a project root's repos and its worktrees directory are found without a
// declaration: the layout is the declaration.
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

// absOrSelfPath resolves a path against the process cwd, returning it unchanged
// when it cannot be resolved (the same posture as the harness package's
// absOrSelf: an unresolvable path must not become a different path).
func absOrSelfPath(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// variantPrompt swaps a built-in brief for the variant the carried checkpoint
// needs (branchless, unverified, panel member). It is the one place that
// decision is made, so the report kind and the handoff note see the brief the
// session actually ran. prev must be a held claim.
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
		// The FIX phase has no unverified variant: its brief asserts no
		// verification at all. It reads the review's record (which names the
		// hand-off's real state) and its reviewed-tip comparison fails closed
		// when the branch is missing, so a variant would only restate what the
		// record already says.
		prompt = config.NoCodeFixBrief()
	case prompt == config.FixVerifyBrief() && prev.Claim.Branch == "":
		// Same for the fix phase that a verification round follows, in the
		// shape whose fix is a record amendment.
		prompt = config.NoCodeFixVerifyBrief()
	case prompt == config.VerifyBrief() && prev.Claim.Branch == "":
		// A verification round on a branchless checkpoint has no code delta
		// to re-review; its brief adjudicates the record's findings.
		prompt = config.NoCodeVerifyBrief()
	}
	return prompt
}
