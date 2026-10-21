package config

// The supervisor's conflict-only session step (CLA-759).
//
// CLA-758's deterministic close-out integrates a CLEAN approved task with no
// agent session. A CONFLICTING one still waited for an agent close-out: the next
// clanker to finish the task it was on ran a FULL implement-phase session, with
// the whole drain protocol in its brief, to resolve a merge conflict.
//
// This is the narrow replacement: one short session that claims the task, merges
// the PR's base into its branch, resolves ONLY the conflicting hunks, runs the
// repo-wide gate and pushes. The supervisor starts it (and the supervisor's own
// package carries that code and its tests); this file owns the STEP's config and
// the brief, because both are operator-facing text the config package already
// owns for every other brief.
//
// The step is deliberately its own top-level block rather than another entry in
// `phases`: a phase is a step in ONE task's sequence, while this runs outside any
// sequence, for one task the supervisor picked, at most one at a time. It still
// reads like the rest of the pipeline — A step with a harness and a model — and
// defaults to the fleet's IMPLEMENT step, because resolving a conflict is
// implementation work that wants the model already trusted with the task's code.

import "strings"

// ConflictStep is the config block controlling the supervisor's conflict-only
// session.
//
// Resolution order for every field is "this block, else the implement step": an
// unset block runs the conflict session on exactly the harness and model that
// implemented the branch, which is the fleet's documented default and needs no
// repetition in the config file. A set field pins only itself, so an operator can
// name a cheaper model for conflict resolution without repeating the harness.
type ConflictStep struct {
	// Harness pins the harness the conflict session runs on. Empty = the
	// harness of the config's IMPLEMENT step (the first phase named
	// "implement", else the run-wide harness).
	Harness string `json:"harness,omitempty"`

	// Model pins the model alias directly, skipping the tier map. Empty = the
	// implement step's resolved model, so the conflict session inherits the
	// model that implemented the branch rather than the harness's default.
	Model string `json:"model,omitempty"`

	// MaxTurns caps the session's turns (harness permitting). 0 = the fleet's
	// effective cap, exactly as a phase with no cap of its own reads it. The
	// supervisor ALSO bounds the session by wall clock, so a harness whose CLI
	// takes no turn flag is still bounded.
	MaxTurns int `json:"max_turns,omitempty"`
}

// implemented reports whether the config declares a conflict block at all.
func (c *Config) conflictDeclared() bool { return c != nil && c.Conflict != nil }

// ConflictStep resolves the effective conflict step: the block's own fields where
// it names them, and the implement step's harness/model everywhere else.
//
// Total, never an error: an unset or partly set block is the documented default,
// not a refusal. Validate is where an unknown harness is refused, using the same
// registry check every other harness-naming field gets.
func (c *Config) ConflictStep() ConflictStep {
	if c == nil {
		return ConflictStep{}
	}
	step := ConflictStep{}
	if c.Conflict != nil {
		step = *c.Conflict
	}
	implHarness, implModel := c.implementStep()
	step.Harness = strings.TrimSpace(step.Harness)
	step.Model = strings.TrimSpace(step.Model)
	if step.Harness == "" {
		step.Harness = implHarness
	}
	if step.Model == "" {
		if step.Harness == implHarness {
			step.Model = implModel
		} else {
			// A different harness is a different dialect with its own default
			// model; another harness's alias would be a name it does not serve.
			step.Model = c.SessionFor(step.Harness).Model
		}
	}
	if step.MaxTurns <= 0 {
		step.MaxTurns = c.MaxTurns
	}
	return step
}

// implementStep answers the harness and model of the config's implement step: the
// first configured phase named "implement", or the run-wide harness/model for an
// unphased run (where EffectivePhases synthesizes a single implement phase).
//
// The model is resolved through the phase's own tier, so a tiered review panel
// beside an untiered implement phase cannot shift the conflict session's model.
func (c *Config) implementStep() (harnessName, model string) {
	for _, ph := range c.Phases {
		if ph.Name != ImplementPhaseName {
			continue
		}
		h := c.HarnessFor(ph)
		m, _ := c.ModelForPhase(ph)
		if strings.TrimSpace(m) == "" {
			m = c.SessionFor(h).Model
		}
		return h, m
	}
	return c.Harness, c.SessionFor(c.Harness).Model
}

// conflictHarness is the harness the conflict block names, or "" when the config
// declares no conflict step (in which case the session runs on the implement
// harness, which the harness lists already carry).
func (c *Config) conflictHarness() string {
	if !c.conflictDeclared() {
		return ""
	}
	return strings.TrimSpace(c.Conflict.Harness)
}

// ConflictResolution kinds. The brief requires the session to begin its outcome
// with an exact `RESOLUTION: <kind>` line, so the supervisor can tell a
// mechanical resolution from a failed one after the session has ended: both
// leave the task approved with the claim released, and the plane status alone
// cannot distinguish them.
const (
	ConflictResolutionMechanical  = "mechanical"
	ConflictResolutionBehavioural = "behavioural"
	ConflictResolutionFailed      = "failed"
)

// ConflictResolutionMarker is the exact line prefix the brief prescribes.
const ConflictResolutionMarker = "RESOLUTION: "

// ConflictResolution reads the marker the conflict session wrote into the task's
// outcome. It returns the kind, the first explanatory line after the marker (the
// note for a behavioural resolution, the reason for a failure) and whether a
// marker was found at all. Total and forgiving: the outcome is written by a
// model, so a marker among prose is found line by line rather than required to
// be the whole outcome.
func ConflictResolution(outcome string) (kind, detail string, ok bool) {
	lines := strings.Split(outcome, "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, ConflictResolutionMarker) {
			continue
		}
		kind = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, ConflictResolutionMarker)))
		switch kind {
		case ConflictResolutionMechanical, ConflictResolutionBehavioural, ConflictResolutionFailed:
		default:
			continue
		}
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
		return kind, detail, true
	}
	return "", "", false
}

// ConflictBriefInput is everything one conflict-only session's brief names. Every
// field is rendered into the brief verbatim, so an empty one would leave a hole a
// model would fill by guessing; the supervisor only renders the brief once it has
// resolved all of them (the gate may be the repo's documented fallback wording,
// never empty).
type ConflictBriefInput struct {
	TaskID  string
	TaskRef string
	RunID   string
	Branch  string
	Base    string
	PR      string // "#12"
	Repo    string
	WorkDir string
	Paths   []string
	// Gate is the repo-wide check command, or "" when it could not be derived
	// from the checkout; the brief then points at the repo's own documented
	// gate instead of inventing a command.
	Gate string
}

// ConflictBrief renders the short brief a conflict-only session runs. It is NOT
// the implement brief: the session must not claim anything (the supervisor
// already did), must not work another task, and must not review or merge.
//
// The instructions are the task's own contract (CLA-759):
//
//   - merge, NEVER rebase: the branch is already pushed, and rewriting published
//     history invalidates the fleet's tip checks;
//   - resolve ONLY the conflicting hunks: no unrelated edits, no formatter sweep,
//     no dependency bump hiding inside a conflict resolution;
//   - run the repo-wide gate and push;
//   - then hand back in exactly one of three states: mechanical (the task stays
//     approved for the deterministic close-out), behavioural/unsure (the code
//     changed behaviour, so it goes back to review) or failed (the claim is
//     released and the task stays approved for the agent close-out).
//
// The outcome markers are load-bearing rather than decorative: the supervisor
// reads them after the session ends to tell a mechanical hand-back from a failed
// one, because both leave the task in the same plane status.
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
		"2. WORKTREE. Your checkout is " + in.WorkDir + ". Create a worktree of the PR's HEAD BRANCH - never resolve the merge in " +
		"the shared checkout:\n" +
		"   git -C " + in.WorkDir + " fetch origin " + in.Base + " " + in.Branch + "\n" +
		"   git -C " + in.WorkDir + " worktree add <path> " + in.Branch + "\n" +
		"Put <path> under the project's worktrees/ directory when one exists (`" + in.WorkDir + "/../worktrees/`), else beside the " +
		"checkout. Do all remaining work in that worktree.\n\n" +
		"3. MERGE, NEVER REBASE. In the worktree run `git merge --no-ff origin/" + in.Base + "`. The branch is already PUSHED: a rebase " +
		"or a force-push rewrites published history and invalidates the fleet's tip check, so never do either, and never merge the PR " +
		"yourself.\n\n" +
		"4. RESOLVE ONLY THE CONFLICTING HUNKS. The conflicting paths are: " + paths + ". Resolve ONLY the hunks git reports as " +
		"conflicting in those files, and nothing else: no reformatting, no renames, no dependency bumps, no deleting a side just to " +
		"make the merge quiet. If the merge reports a conflicting path outside that list, stop and report FAILED (the base moved or " +
		"the checkout is stale); do not invent a resolution for it. Never commit conflict markers.\n\n" +
		"5. GATE. Run " + gateCommand + " in the worktree. It must pass before you push.\n\n" +
		"6. PUSH. Commit the merge (a merge commit, not a squash) and push the branch: `git push origin HEAD:" + in.Branch + "`. Never " +
		"force-push, and never push to " + in.Base + ".\n\n" +
		"7. HAND THE TASK BACK - exactly one of these, and nothing else:\n" +
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
		"close-out:\n" +
		"     update_task(taskId: \"" + in.TaskID + "\", runId: \"" + in.RunID + "\", outcome: \"RESOLUTION: failed\\n\\n- <why>\")\n" +
		"     update_task(taskId: \"" + in.TaskID + "\", runId: \"" + in.RunID + "\", release: true, checkGreen: true)\n\n" +
		"8. STOP. Do not review, do not open or merge the PR, do not file tasks, and do not move the task except exactly as step 7 " +
		"says. Remove the worktree you created once the push succeeded (`git -C " + in.WorkDir + " worktree remove --force <path>`). " +
		"Your FINAL message is one line: `conflict " + ref + ": <mechanical|behavioural|failed>`."
}
