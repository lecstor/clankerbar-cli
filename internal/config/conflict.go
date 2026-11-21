package config

// The supervisor's conflict-only session step.

import "strings"

// ConflictStep is the config block controlling the supervisor's conflict-only session. A set field
// pins only itself, so an operator can name a cheaper model for conflict resolution without
// repeating the harness.
type ConflictStep struct {
	// Harness pins the harness the conflict session runs on. Empty = the harness of the config's
	// IMPLEMENT step (the first phase named "implement", else the run-wide harness).
	Harness string `json:"harness,omitempty"`

	// Model pins the model alias (or an ordered route list, like a phase's own `model`) directly,
	// skipping the tier map. Empty = the implement step's WHOLE route list, so the conflict session
	// inherits the fallback chain that implemented the branch rather than its first entry alone: a
	// quota on the first route is exactly the case the list exists for.
	Model ModelRoutes `json:"model,omitempty"`

	// MaxTurns caps the session's turns (harness permitting). 0 = the fleet's effective cap, exactly
	// as a phase with no cap of its own reads it.
	MaxTurns int `json:"max_turns,omitempty"`
}

// implemented reports whether the config declares a conflict block at all.
func (c *Config) conflictDeclared() bool { return c != nil && c.Conflict != nil }

// ConflictStep resolves the effective conflict step: the block's own fields where it names them,
// and the implement step's harness/model routes everywhere else. Total, never an error: an unset or
// partly set block is the documented default, not a refusal.
func (c *Config) ConflictStep() ConflictStep {
	if c == nil {
		return ConflictStep{}
	}
	step := ConflictStep{}
	if c.Conflict != nil {
		step = *c.Conflict
	}
	implHarness, implRoutes := c.implementStep()
	step.Harness = strings.TrimSpace(step.Harness)
	if step.Harness == "" {
		step.Harness = implHarness
	}
	if step.Model.Empty() {
		if step.Harness == implHarness {
			step.Model = implRoutes
		} else {
			// A different harness is a different dialect with its own default route list; another harness's
			// alias would be a name it does not serve.
			step.Model = c.SessionFor(step.Harness).Model
		}
	}
	// The routes are trimmed and de-duplicated in ONE place, on the way out, so every consumer (the
	// log line, the spawn, the fallback ladder) reads the same answer whether the block pinned them or
	// they were inherited.
	step.Model = ModelRoutes(step.Model.List())
	if step.MaxTurns <= 0 {
		step.MaxTurns = c.MaxTurns
	}
	// The config's turn-cap chain has no "uncapped" value (see Config.MaxTurns): an unphased run,
	// which this session is not part of, still resolves through DefaultMaxTurns.
	if step.MaxTurns <= 0 {
		step.MaxTurns = DefaultMaxTurns
	}
	return step
}

// implementStep answers the harness and the WHOLE model route list of the config's implement step:
// the first configured phase named "implement", or the run-wide harness/model for an unphased run
// (where EffectivePhases synthesizes a single implement phase). The routes are resolved through the
// phase's own tier, so a tiered review panel beside an untiered implement phase cannot shift the
// conflict session's models.
func (c *Config) implementStep() (harnessName string, model ModelRoutes) {
	for _, ph := range c.Phases {
		if ph.Name != ImplementPhaseName {
			continue
		}
		h := c.HarnessFor(ph)
		routes, _ := c.ModelRoutesForPhase(ph)
		return h, ModelRoutes(routes)
	}
	return c.Harness, c.SessionFor(c.Harness).Model
}

// conflictHarness is the harness the conflict block names, or "" when the config declares no
// conflict step (in which case the session runs on the implement harness, which the harness lists
// already carry).
func (c *Config) conflictHarness() string {
	if !c.conflictDeclared() {
		return ""
	}
	return strings.TrimSpace(c.Conflict.Harness)
}

// ConflictResolution kinds.
const (
	ConflictResolutionMechanical  = "mechanical"
	ConflictResolutionBehavioural = "behavioural"
	ConflictResolutionFailed      = "failed"
)

// ConflictResolutionMarker is the exact line prefix the brief prescribes.
const ConflictResolutionMarker = "RESOLUTION: "

// ConflictResolution reads the LAST resolution marker in outcome.
func ConflictResolution(outcome string) (kind, detail string, ok bool) {
	lines := strings.Split(outcome, "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, ConflictResolutionMarker) {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, ConflictResolutionMarker)))
		switch k {
		case ConflictResolutionMechanical, ConflictResolutionBehavioural, ConflictResolutionFailed:
		default:
			continue
		}
		kind, detail, ok = k, "", true
		inTests := false
		for _, after := range lines[i+1:] {
			after = strings.TrimSpace(after)
			if strings.HasPrefix(after, "**Tests**") {
				inTests = true
				continue
			}
			if inTests {
				if after == "" {
					inTests = false
				}
				continue
			}
			if after == "" || strings.HasPrefix(after, "#") {
				continue
			}
			after = strings.TrimSpace(strings.TrimLeft(after, "-*+ "))
			if after == "" {
				continue
			}
			detail = after
			break
		}
	}
	return kind, detail, ok
}

// ConflictResolutionSince reads the marker THIS session appended, given the outcome the claim
// response served BEFORE the session (prior) and the outcome served after it (current). An
// unchanged outcome means the session wrote nothing, and therefore left no marker — the
// accumulated text's older markers belong to earlier attempts and must not classify this one.
func ConflictResolutionSince(prior, current string) (kind, detail string, ok bool) {
	if strings.TrimSpace(current) == strings.TrimSpace(prior) {
		return "", "", false
	}
	fresh := current
	if p := strings.TrimRight(prior, " \t\r\n"); p != "" && strings.HasPrefix(current, p) {
		fresh = current[len(p):]
	}
	return ConflictResolution(fresh)
}

// ConflictBriefInput is everything one conflict-only session's brief names. Every field is rendered
// into the brief verbatim, so an empty one would leave a hole a model would fill by guessing; the
// supervisor only renders the brief once it has resolved all of them (the gate may be the repo's
// documented fallback wording, never empty).
type ConflictBriefInput struct {
	TaskID  string
	TaskRef string
	RunID   string
	Branch  string
	Base    string
	PR      string // "#12"
	Repo    string
	WorkDir string
	// Worktree is the exact path the session must create its branch worktree at.
	Worktree string
	Paths    []string
	// Gate is the repo-wide check command, or "" when it could not be derived from the checkout; the
	// brief then points at the repo's own documented gate instead of inventing a command.
	Gate string
}

// ConflictBrief renders the short brief a conflict-only session runs. The instructions are the
// task's own contract: - merge, NEVER rebase: the branch is already pushed, and rewriting published
// history invalidates the fleet's tip checks; - resolve ONLY the conflicting hunks: no unrelated
// edits, no formatter sweep, no dependency bump hiding inside a conflict resolution; - run the
// repo-wide gate and push; - then hand back in exactly one of three states: mechanical (the task
// stays approved for the deterministic close-out), behavioural/unsure (the code changed behaviour,
// so it goes back to review) or failed (the claim is released and the task stays approved for the
// agent close-out).
func ConflictBrief(in ConflictBriefInput) string {
	paths := strings.Join(in.Paths, ", ")
	if paths == "" {
		paths = "the files the merge reports as conflicting (the fleet could not name them in advance)"
	}
	gate := strings.TrimSpace(in.Gate)
	gateCommand := gate
	gateWording := "`" + gate + "`"
	if gate == "" {
		gateCommand = "the repo's own documented full gate (the command its AGENTS.md, CLAUDE.md or CI names)"
		gateWording = "that gate"
	}
	ref := strings.TrimSpace(in.TaskRef)
	if ref == "" {
		ref = in.TaskID
	}
	return "You are a CONFLICT-ONLY session for the fleet supervisor. The supervisor already CLAIMED task " + ref +
		" (" + in.TaskID + ") for you on run " + in.RunID + ": the claim is held, so do NOT call claim_task, do NOT call " +
		"next_task, and do NOT work any other task. Its PR " + in.PR + " (" + in.Repo + ") is CONFLICTING with " + in.Base +
		". Your whole job is to resolve that conflict on branch " + in.Branch + " and hand the task back in exactly one of the " +
		"states in step 7. No other work.\n\n" +
		"1. KEEP THE LEASE. Call heartbeat(runId: \"" + in.RunID + "\") now, and again every ~10 minutes while you work. Every " +
		"plane call you make signs with taskId \"" + in.TaskID + "\" and runId \"" + in.RunID + "\".\n\n" +
		"2. WORKTREE. Your checkout is " + in.WorkDir + ". Create a worktree of the PR's HEAD BRANCH at exactly " + in.Worktree +
		" - never resolve the merge in the shared checkout:\n" +
		"   git -C " + in.WorkDir + " fetch origin " + in.Base + " " + in.Branch + "\n" +
		"   git -C " + in.WorkDir + " worktree add --detach " + in.Worktree + " origin/" + in.Branch + "\n" +
		"If that path already exists from an earlier attempt, remove it first (`git -C " + in.WorkDir + " worktree remove --force " +
		in.Worktree + "`). Do all remaining work in that worktree. The worktree is detached on purpose - it does not own or move " +
		"the local branch, and the push below names the remote branch explicitly.\n\n" +
		"3. MERGE, NEVER REBASE. In that worktree run `git -C " + in.Worktree + " merge --no-ff origin/" + in.Base + "`. The branch is already PUSHED: a rebase " +
		"or a force-push rewrites published history and invalidates the fleet's tip check, so never do either, and never merge the PR " +
		"yourself.\n\n" +
		"4. RESOLVE ONLY THE CONFLICTING HUNKS. The conflicting paths are: " + paths + ". Resolve ONLY the hunks git reports as " +
		"conflicting in those files, and nothing else: no reformatting, no renames, no dependency bumps, no deleting a side just to " +
		"make the merge quiet. If the merge reports a conflicting path outside that list, stop and report FAILED (the base moved or " +
		"the checkout is stale); do not invent a resolution for it. Never commit conflict markers.\n\n" +
		"5. GATE. In the worktree (`cd " + in.Worktree + "` first) run " + gateCommand + ". It must pass before you push.\n\n" +
		"6. PUSH. The merge commit is the commit `git merge` made - do not squash or amend it - and push the branch: " +
		"`git -C " + in.Worktree + " push origin HEAD:" + in.Branch + "`. Never force-push, and never push to " + in.Base + ".\n\n" +
		"7. HAND THE TASK BACK - exactly one of these, and nothing else. FIRST keep the task's record: call " +
		"get_task(taskId: \"" + in.TaskID + "\") and copy the `outcome` it carries VERBATIM into the outcome you write, with your " +
		"RESOLUTION block and **Tests** section APPENDED after it (write only your block if it is empty). That stored outcome is the " +
		"review evidence that earned the approval, and the close-out records no outcome of its own - replacing it would destroy that " +
		"evidence. Then:\n" +
		"   - MECHANICAL - every resolution is renumbering, keeping both sides of a list, or regenerating a pinned figure, with NO " +
		"change to what the code does. That code does not need a new review, so leave the task approved for the supervisor to merge " +
		"once CI is green. First record the resolution, then release the claim:\n" +
		"     update_task(taskId: \"" + in.TaskID + "\", runId: \"" + in.RunID + "\", outcome: \"RESOLUTION: mechanical\\n\\n**Tests**\\n- " +
		gateWording + ": pass\\n\\n- <each conflicting path>: <how it was resolved>\")\n" +
		"     update_task(taskId: \"" + in.TaskID + "\", runId: \"" + in.RunID + "\", release: true, checkGreen: true)\n" +
		"   - BEHAVIOURAL - a resolution changes behaviour in any way, or you are UNSURE. That code has NOT been reviewed, so move " +
		"the task back to review, naming what changed:\n" +
		"     update_task(taskId: \"" + in.TaskID + "\", runId: \"" + in.RunID + "\", status: \"in_review\", outcome: \"RESOLUTION: " +
		"behavioural\\n\\n**Tests**\\n- " + gateWording + ": pass\\n\\n- <each conflicting path>: <what changed and why>\")\n" +
		"   - FAILED - the gate will not go green, or a hunk cannot be resolved without guessing. Do NOT push a broken or " +
		"half-resolved branch: discard the merge, record why, then release the claim and leave the task approved for the agent " +
		"close-out. The merge never happened, so the release attests checkGreen: false - that is what makes the plane's CI-repair " +
		"ladder count and bound a green-but-unmergeable PR instead of exempting it:\n" +
		"     update_task(taskId: \"" + in.TaskID + "\", runId: \"" + in.RunID + "\", outcome: \"RESOLUTION: failed\\n\\n- <why>\")\n" +
		"     update_task(taskId: \"" + in.TaskID + "\", runId: \"" + in.RunID + "\", release: true, checkGreen: false)\n\n" +
		"8. STOP. Do not review, do not open or merge the PR, do not file tasks, and do not move the task except exactly as step 7 " +
		"says. Remove the worktree before you finish, whatever the outcome (`git -C " + in.WorkDir + " worktree remove --force " + in.Worktree + "`), " +
		"so a retry starts clean. " +
		"Your FINAL message is one line: `conflict " + ref + ": <mechanical|behavioural|failed>`."
}
