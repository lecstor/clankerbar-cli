package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/secureurl"
)

// maxSessionNice is the highest session_nice the kernel honours: every value above 19 is clamped
// down to 19, so accepting one would let the config read as a stronger setting than the machine
// applies.
const maxSessionNice = 19

// StepHarnessError is Validate's refusal for a configured STEP whose harness this machine cannot
// run: a phase, or a review-panel member, that names a harness nothing registered, or one other
// than the run's top-level harness with no local `harnesses.<h>` block to give it its machine
// wiring. The prose stays in Error(); the step label and harness are carried apart so a caller can
// report them without matching on the message — `doctor` turns this one into a FAILED check
// naming the project, step and harness, because a stored document whose step this machine cannot
// spawn is a ratified policy that silently will not run here, not the ordinary "keep the local
// rules" trouble.
type StepHarnessError struct {
	// Step is the operator-facing label, e.g. `phases[1] ("review")` or `review_panel[2]
	// ("review-3")`.
	Step string
	// Harness is the resolved harness name the step asked for.
	Harness string
	// Problem is the short reason: "unknown harness" or "no local harness block".
	Problem string
	msg     string
}

func (e *StepHarnessError) Error() string { return e.msg }

// Validate normalizes path fields and checks the resolved config is runnable. The sections run in a
// fixed order and the first error wins: the harness must be known before the MCP auto-fill, and
// later checks read fields earlier ones fill in.
func (c *Config) Validate() error {
	if err := c.normalizePaths(); err != nil {
		return err
	}
	if err := c.validateRole(); err != nil {
		return err
	}
	if err := c.validateHarnessNames(); err != nil {
		return err
	}
	if err := c.validateLimits(); err != nil {
		return err
	}
	if err := c.validatePrompt(); err != nil {
		return err
	}
	if err := c.validateHarnessFlag(); err != nil {
		return err
	}
	if err := c.validatePhaseHarnesses(); err != nil {
		return err
	}
	if err := c.validateConflictStep(); err != nil {
		return err
	}
	if err := c.validatePanelBounds(); err != nil {
		return err
	}
	if err := c.validatePanelMembers(); err != nil {
		return err
	}
	if err := c.validatePanelExpands(); err != nil {
		return err
	}
	if err := c.validateReviewMethods(); err != nil {
		return err
	}
	if err := c.validatePhaseClaimTracking(); err != nil {
		return err
	}
	if err := c.validatePhaseShapes(); err != nil {
		return err
	}
	if err := c.validateLastPhase(); err != nil {
		return err
	}
	if err := c.validateFixPhase(); err != nil {
		return err
	}
	if err := c.validateVerifyPhase(); err != nil {
		return err
	}
	if err := c.validateReviewSuccessor(); err != nil {
		return err
	}
	if err := c.resolveTopMCPConfig(); err != nil {
		return err
	}
	if err := c.validateEndpoints(); err != nil {
		return err
	}
	if err := c.validateHarnessMCPConfigs(); err != nil {
		return err
	}
	if err := c.validateSettingsMode(); err != nil {
		return err
	}
	if err := c.validateProjects(); err != nil {
		return err
	}
	if err := c.validatePhaseProjectMCP(); err != nil {
		return err
	}
	if err := c.validateEnvDeclarations(); err != nil {
		return err
	}
	if err := c.validateEnvAllowlists(); err != nil {
		return err
	}
	if err := c.validatePauseWindows(); err != nil {
		return err
	}
	if err := c.validateRouteWindows(); err != nil {
		return err
	}
	return nil
}

// normalizePaths expands and absolutizes the run-wide and per-harness path fields; the workdir is
// pinned first because everything else is derived from it.
func (c *Config) normalizePaths() error {
	c.ConfigDir = expandHome(c.ConfigDir)
	c.WorkDir = expandHome(c.WorkDir)
	c.MCPConfigPath = expandHome(c.MCPConfigPath)
	c.SettingsPath = expandHome(c.SettingsPath)
	c.StateDir = expandHome(c.StateDir)

	// The workdir becomes absolute HERE, once, before anything is derived from it — the same
	// treatment MCPConfigPath/SettingsPath/ConfigDir get on the next three lines.
	if c.WorkDir == "" {
		c.workDirImplicit = true
	}
	absWorkDir, err := filepath.Abs(c.WorkDir)
	if err != nil {
		return fmt.Errorf("workdir %s: %w", c.WorkDir, err)
	}
	c.WorkDir = absWorkDir

	c.MCPConfigPath = mcpUnderWorkDir(c.MCPConfigPath, c.WorkDir)
	c.SettingsPath = underWorkDir(c.SettingsPath, c.WorkDir)
	c.ConfigDir = underWorkDir(c.ConfigDir, c.WorkDir)

	// Per-harness blocks get exactly the same path treatment as their run-wide twins, and for the same
	// reasons: a `~` that never expanded and a relative path resolved against whatever cwd the process
	// had are bugs that do not care which block the field sat in.
	for name, hc := range c.Harnesses {
		hc.ConfigDir = underWorkDir(expandHome(hc.ConfigDir), c.WorkDir)
		hc.MCPConfigPath = mcpUnderWorkDir(hc.MCPConfigPath, c.WorkDir)
		hc.SettingsPath = underWorkDir(expandHome(hc.SettingsPath), c.WorkDir)
		c.Harnesses[name] = hc
	}
	return nil
}

// validateRole refuses a local `role` outside the two roles an instance may serve. Only the LOCAL
// key is checked: the plane-supplied `roles.planner` is ratified policy for the projects that carry
// it, and one bad planner block must not refuse a worker drain that may serve other projects fine.
func (c *Config) validateRole() error {
	role := strings.TrimSpace(c.Role)
	if role == "" || ValidRole(role) {
		return nil
	}
	return fmt.Errorf("unknown role %q (want: %s or %s; an absent key means %s)", role, RoleWorker, RolePlanner, RoleWorker)
}

// validateHarnessNames refuses an unknown top-level harness, then unknown `harnesses`,
// `budget.per_harness` and `session_idle` keys, and a negative session_idle bound, in that order.
func (c *Config) validateHarnessNames() error {
	// Validate against the harness registry (not a hand-kept switch) so the accepted set can never
	// drift from what is actually registered — an unregistered value is rejected here before
	// harness.Get is consulted, and a newly registered adapter is accepted automatically.
	if !harness.Known(c.Harness) {
		return fmt.Errorf("unknown harness %q (want: %s)%s", c.Harness, strings.Join(harness.Names(), ", "), retiredHarnessHint(c.Harness))
	}
	// A `harnesses` block keyed by a name no adapter answers to is INERT: nothing ever looks that key
	// up, so its model, tier map and paths are read by nobody and the operator's settings quietly stop
	// applying.
	for _, name := range slices.Sorted(maps.Keys(c.Harnesses)) {
		if !harness.Known(name) {
			return fmt.Errorf("harnesses[%q]: unknown harness (want: %s)%s", name, strings.Join(harness.Names(), ", "), retiredHarnessHint(name))
		}
	}
	// A per-harness block keyed by a name no adapter answers to is a ceiling that can never trip:
	// nothing is ever charged to it, so the run is unbounded on exactly the side the operator meant to
	// bound.
	for _, name := range slices.Sorted(maps.Keys(c.Budget.PerHarness)) {
		if !harness.Known(name) {
			return fmt.Errorf("budget.per_harness[%q]: unknown harness (want: %s)%s", name, strings.Join(harness.Names(), ", "), retiredHarnessHint(name))
		}
	}
	// A session_idle block keyed by an unregistered harness is an inert watchdog, so the name is
	// refused here, in sorted order like the two maps above.
	for _, name := range slices.Sorted(maps.Keys(c.SessionIdle)) {
		if !harness.Known(name) {
			return fmt.Errorf("session_idle[%q]: unknown harness (want: %s)%s", name, strings.Join(harness.Names(), ", "), retiredHarnessHint(name))
		}
		b := c.SessionIdle[name]
		if b.MaxIdle != nil && *b.MaxIdle < 0 {
			return fmt.Errorf("session_idle[%q].max_idle is negative", name)
		}
		if b.MaxToolCall != nil && *b.MaxToolCall < 0 {
			return fmt.Errorf("session_idle[%q].max_tool_call is negative", name)
		}
	}
	return nil
}

// validateLimits refuses negative counts and caps and an over-long instance name.
func (c *Config) validateLimits() error {
	if c.MaxTurns < 0 {
		return errors.New("max_turns is negative")
	}
	if c.Budget.MaxSessionTokens < 0 {
		return errors.New("max_session_tokens is negative")
	}
	// A negative wall-clock cap would reach exec as an already-expired deadline, killing every session
	// the instant it started — a config typo that looks like a broken harness.
	if c.MaxSessionWallClock < 0 {
		return errors.New("max_session_wall_clock is negative")
	}
	// Refused rather than clamped: a negative here would read as "set" in the config and resolve to
	// the default anyway, which is the silent-inert shape doctor's negative-budget check exists to
	// stop.
	if c.MaxZeroSpendAttempts < 0 {
		return errors.New("max_zero_spend_attempts is negative")
	}
	// Refused rather than read as "off": 0 is how the check is turned off, and a negative is a typo
	// that would otherwise disable it silently.
	if c.MaxRepeatedToolFailures != nil && *c.MaxRepeatedToolFailures < 0 {
		return errors.New("max_repeated_tool_failures is negative")
	}
	// The priority offset a session runs at. Negative would mean scheduling the fleet ABOVE everything
	// else — only a privileged process can do it, and an unattended fleet is exactly what must not.
	if c.SessionNice < 0 {
		return errors.New("session_nice is negative (0 = the daemon's own priority, 10 is a reasonable shared-machine value)")
	}
	if c.SessionNice > maxSessionNice {
		return fmt.Errorf("session_nice %d exceeds %d (the kernel clamps every value above it)", c.SessionNice, maxSessionNice)
	}
	// Refused, not ignored: an operator who set this believes CI is protected from their sessions, and
	// a silent no-op leaves them believing it.
	if c.SessionNice > 0 && !harness.SessionPrioritySupported {
		return fmt.Errorf("session_nice %d is unsupported on %s: this platform has no nice(1) to lower a session's scheduling priority with", c.SessionNice, runtime.GOOS)
	}
	// The plane refuses an instance name over 100 characters, so a beacon carrying one would be
	// dropped on EVERY poll — reporting silently off.
	if len(strings.TrimSpace(c.InstanceName)) > MaxInstanceNameLen {
		return fmt.Errorf("instance_name exceeds %d characters (the plane's limit)", MaxInstanceNameLen)
	}
	return nil
}

// validatePrompt checks the prompt/phases pairing.
func (c *Config) validatePrompt() error {
	if len(c.Phases) == 0 && c.Prompt == "" {
		return errors.New("prompt is empty")
	}
	// Two answers to the same question. A phased task is asked its PHASE's prompt and never this one,
	// so a config carrying both is refused rather than resolved — an operator who set one and forgot
	// the other gets told, instead of wondering which of two prompts their sessions were handed.
	if len(c.Phases) > 0 && c.Prompt != "" && c.Prompt != defaultPrompt {
		return errors.New("config sets both `prompt` and `phases`; a phased task is asked its phase's prompt " +
			"and never `prompt`, so remove one of them")
	}
	return nil
}

// validateHarnessFlag refuses --harness/--model alongside a per-phase harness config.
func (c *Config) validateHarnessFlag() error {
	// Phases rest entirely on the adapter observing the session's claim: the handback across a seam,
	// the salvage, and the delivery check all gate on Result.Claim.Held().
	if c.harnessFromFlag || c.modelFromFlag {
		if mixed := len(c.Harnesses) > 0 || len(c.PhaseHarnesses()) > 1; mixed {
			flag := "--harness"
			if !c.harnessFromFlag {
				flag = "--model"
			}
			return fmt.Errorf("%s was given, but this config selects harnesses per phase (`harnesses` / a phase `harness`): "+
				"the flag assumes one harness for the whole run, so it would re-point which harness the run-wide fields "+
				"belong to — edit the config's `harnesses` block instead", flag)
		}
	}
	return nil
}

// validatePhaseHarnesses checks that every phase names a registered harness and, off the top-level
// harness, has a local block.
func (c *Config) validatePhaseHarnesses() error {
	// Every harness a phase names has to exist and, when it is not the top-level one, has to be
	// configured.
	for i, ph := range c.Phases {
		h := c.HarnessFor(ph)
		if !harness.Known(h) {
			step := fmt.Sprintf("phases[%d] (%q)", i, ph.Label(i))
			return &StepHarnessError{
				Step: step, Harness: h, Problem: "unknown harness",
				msg: fmt.Sprintf("%s: unknown harness %q (want: %s)%s",
					step, h, strings.Join(harness.Names(), ", "), retiredHarnessHint(h)),
			}
		}
		// A phase on some OTHER harness inherits none of the run-wide harness-shaped fields, by design
		// (see HarnessConfig), so with no block of its own it would spawn with no config dir, no MCP
		// config and no settings — a session with no clankerbar tools, which looks from the log like a
		// model that declined the work.
		if h == c.Harness {
			continue
		}
		hc, ok := c.Harnesses[h]
		if !ok || hc.Empty() {
			// An EMPTY block is the same failure as a missing one, and it has to be said that way: the
			// promise this rule makes is that no session spawns with none of its harness's fields, and
			// `"harnesses": {"opencode": {}}` spawns exactly that while satisfying a presence-of-key check.
			step := fmt.Sprintf("phases[%d] (%q)", i, ph.Label(i))
			return &StepHarnessError{
				Step: step, Harness: h, Problem: "no local harness block",
				msg: fmt.Sprintf("%s runs on harness %q, but `harnesses.%s` is missing or empty: "+
					"a phase on a harness other than the run's `harness` inherits none of config_dir / mcp_config_path / "+
					"settings_path / model, because each is that harness's own dialect — declare them under `harnesses.%s`",
					step, h, h, h),
			}
		}
	}
	return nil
}

// validateConflictStep checks the conflict-only session step.
func (c *Config) validateConflictStep() error {
	// The conflict-only session step. It names a harness the SUPERVISOR will start a session on, so it
	// is held to the same two rules as a phase's own harness above: it must exist in the registry, and
	// when it is not the run-wide one it must carry a block of its own — inheriting another
	// harness's dialect is the 3am failure the phase rule exists to prevent.
	if c.conflictDeclared() {
		h := c.ConflictStep().Harness
		if !harness.Known(h) {
			return fmt.Errorf("conflict.harness: unknown harness %q (want: %s)%s",
				h, strings.Join(harness.Names(), ", "), retiredHarnessHint(h))
		}
		if c.Conflict.MaxTurns < 0 {
			return errors.New("conflict.max_turns must not be negative")
		}
		if h != c.Harness {
			hc, ok := c.Harnesses[h]
			if !ok || hc.Empty() {
				return fmt.Errorf("conflict.harness runs on harness %q, but `harnesses.%s` is missing or empty: "+
					"a conflict session on a harness other than the run's `harness` inherits none of config_dir / "+
					"mcp_config_path / settings_path / model — declare them under `harnesses.%s`", h, h, h)
			}
		}
	}
	return nil
}

// validatePanelBounds checks the review panel is non-empty when it means something, its concurrency
// cap, and that it expands at most one built-in review phase.
func (c *Config) validatePanelBounds() error {
	// The review panel. Its members run one review phase each, so a member naming a harness is naming
	// a harness the sequence will spawn on — held to the same two rules as a phase's own harness
	// above, and refused here for the same reason: the alternative is a paid review session with none
	// of its harness's fields, or one that dies on a name nothing answers to, discovered at 3am rather
	// than at startup.
	if c.reviewPanelSet && len(c.EffectivePanel()) == 0 && c.expandsReviewPanel() {
		return errors.New("review_panel is empty: a panel is one or more reviewers, each run as its own review phase " +
			"(remove the key for a single reviewer, or list the members)")
	}
	// CLA-697: the concurrency cap is a count of sessions, so a negative one is meaningless rather
	// than a way to say "unbounded" — that is 0 (or the absent key).
	if c.ReviewPanelConcurrency < 0 {
		return fmt.Errorf("review_panel_concurrency is %d: it is the number of panel members that may run at once, "+
			"so it must be 0 (the whole panel, the default) or a positive count (1 = sequential)", c.ReviewPanelConcurrency)
	}
	// A panel expands exactly ONE built-in review phase. A sequence with more than one is refused
	// rather than resolved: the panel would multiply one and leave the other as a lone review, and
	// which one it picked would be positional trivia the operator could only discover from the log.
	if len(c.EffectivePanel()) > 0 && len(c.Phases) > 0 {
		var builtin []int
		for i, ph := range c.Phases {
			if ph.Prompt == "" && ph.Name == ReviewPhaseName {
				builtin = append(builtin, i)
			}
		}
		if len(builtin) > 1 {
			at := make([]string, len(builtin))
			for n, i := range builtin {
				at[n] = strconv.Itoa(i)
			}
			return fmt.Errorf("review_panel is set, but the sequence has %d built-in %q phases (at %s): a panel expands "+
				"exactly ONE built-in review phase, and the others would run as lone reviews — give the other review "+
				"phases their own prompts, or remove `review_panel` to run the sequence's reviews as written",
				len(builtin), ReviewPhaseName, strings.Join(at, ", "))
		}
	}
	return nil
}

// validatePanelMembers checks each review panel member: required seats, labels, names, harness and
// claim tracking.
func (c *Config) validatePanelMembers() error {
	panel := c.EffectivePanel()
	// CLA-784: an OPTIONAL member is a TRIAL seat — a reviewer the operator is evaluating, whose
	// failure is recorded against that seat and never blocks the fix.
	if len(panel) > 0 {
		required := 0
		for _, m := range panel {
			if !m.Optional {
				required++
			}
		}
		if required == 0 {
			return fmt.Errorf("review_panel has %d members and ALL are optional: an `optional` member is a TRIAL reviewer whose failure is recorded against that seat and never blocks the fix, so a panel with no required member has no review to produce — leave at least one member without `optional` (required is the default), or remove `review_panel`",
				len(panel))
		}
	}
	labels := map[string]int{}
	for i, m := range panel {
		label := PanelMemberLabel(panel, i)
		// Names become part of the iteration-log filename (the same rule a phase name follows), and one
		// member's log must not be another's.
		if j, dup := labels[label]; dup {
			return fmt.Errorf("review_panel[%d] (%q): duplicate member name — review_panel[%d] already uses %q; a "+
				"name is the member's log label, so each member needs its own", i, label, j, label)
		}
		labels[label] = i
		if name := strings.TrimSpace(m.Name); name != "" && !phaseNameRe.MatchString(name) {
			return fmt.Errorf("review_panel[%d]: name %q must be lowercase letters, digits and hyphens, with no leading, trailing or doubled hyphen (it becomes part of the iteration log filename)", i, name)
		}
		// The review-N family is owned by member POSITION: a member may write its own derived label, but
		// not another member's, or the logs, the INCOMPLETE lines and the dead-phase tally would disagree
		// about which member `review-2` is.
		if name := strings.TrimSpace(m.Name); name != "" && panelMemberPositionRe.MatchString(name) {
			derived := "review-" + strconv.Itoa(i+1)
			if name != derived {
				return fmt.Errorf("review_panel[%d]: name %q is in the reserved review-N family; this member's derived label is %q — name it something of its own, or remove `name` to let the position derive it", i, name, derived)
			}
		}
		// The label is also the iteration-log tag, the phasesRun entry and the dead-phase tally cell, so
		// it must not shadow a phase either: a member named `review` would merge with the real review
		// phase in every log and tally (ReviewShaped/escalation still key on the phase's prompt, so only
		// the reporting would be a lie — which is enough).
		if slices.Contains(BuiltinPhaseNames(), label) {
			return fmt.Errorf("review_panel[%d]: name %q is a built-in phase name; a member's name is its log label and iteration-log tag, so it must not shadow the %q phase — give the member a name of its own", i, label, label)
		}
		for j, ph := range c.Phases {
			if ph.Name != label {
				continue
			}
			if strings.TrimSpace(m.Name) == "" {
				return fmt.Errorf("review_panel[%d]'s derived label %q collides with phases[%d]; set `name` on this member to label it without shadowing the phase", i, label, j)
			}
			return fmt.Errorf("review_panel[%d]: name %q is already the name of phases[%d]; a member's name labels its logs and dead-phase tally, so it must not merge with a configured phase", i, label, j)
		}
		h := strings.TrimSpace(m.Harness)
		if h == "" {
			h = c.reviewPhaseHarness()
		}
		if !harness.Known(h) {
			step := fmt.Sprintf("review_panel[%d] (%q)", i, label)
			return &StepHarnessError{
				Step: step, Harness: h, Problem: "unknown harness",
				msg: fmt.Sprintf("%s: unknown harness %q (want: %s)%s",
					step, h, strings.Join(harness.Names(), ", "), retiredHarnessHint(h)),
			}
		}
		// A member runs one review phase of the sequence, so it is held to the same claim-tracking rule a
		// configured phase is (the rule above): a member that cannot observe or resume the task claim
		// would end the sequence early or hand the task back over its own work.
		if len(c.Phases) > 1 {
			caps, known := harness.CapabilitiesOf(h)
			if known && !caps.TracksClaims {
				return fmt.Errorf("review_panel[%d] (%q) runs on harness %q, which does not observe the session's task claim — "+
					"a phase either claims the task or resumes one it is handed, and this harness can do neither, so the "+
					"sequence would end early or hand the task back over its own work (only the claim-tracking harnesses "+
					"can run a phase in a sequence: use one, or drop the panel member)",
					i, label, h)
			}
		}
		if h == c.Harness {
			continue
		}
		hc, ok := c.Harnesses[h]
		if !ok || hc.Empty() {
			step := fmt.Sprintf("review_panel[%d] (%q)", i, label)
			return &StepHarnessError{
				Step: step, Harness: h, Problem: "no local harness block",
				msg: fmt.Sprintf("%s reviews on harness %q, but `harnesses.%s` is missing or empty: "+
					"a phase on a harness other than the run's `harness` inherits none of config_dir / mcp_config_path / "+
					"settings_path / model, because each is that harness's own dialect — declare them under `harnesses.%s`",
					step, h, h, h),
			}
		}
	}
	return nil
}

// validatePanelExpands refuses a panel the sequence has no built-in non-final review phase to expand.
func (c *Config) validatePanelExpands() error {
	// A panel multiplies the review phase, so a config that names one but has a review phase it cannot
	// multiply is two contracts for one sequence.
	if len(c.EffectivePanel()) > 0 && len(c.Phases) > 0 && !c.expandsReviewPanel() {
		return fmt.Errorf("review_panel is set, but the sequence has no built-in, non-final %q phase to expand: "+
			"a panel needs a built-in %q phase with a phase after it (the fix), because a FINAL review runs the "+
			"historical combined brief and a review phase with its own prompt owns its own contract. Remove "+
			"`review_panel` for a single reviewer, or make the review built-in and give it a phase after it",
			ReviewPhaseName, ReviewPhaseName)
	}
	return nil
}

// validatePhaseClaimTracking checks every phase of a multi-phase sequence runs on a claim-tracking harness.
func (c *Config) validatePhaseClaimTracking() error {
	// Phases rest on EVERY phase's adapter observing the session's claim, not only the claiming one's.
	if len(c.Phases) > 1 {
		for i, ph := range c.Phases {
			h := c.HarnessFor(ph)
			caps, known := harness.CapabilitiesOf(h)
			if !known || caps.TracksClaims {
				continue
			}
			return fmt.Errorf("phases[%d] (%q) runs on harness %q, which does not observe the session's task claim — "+
				"a phase either claims the task or resumes one it is handed, and this harness can do neither, so the "+
				"sequence would end early or hand the task back over its own work (only the claim-tracking harnesses "+
				"can run a phase in a sequence: use one, or drop `phases`)",
				i, ph.Label(i), h)
		}
	}
	return nil
}

// validatePhaseShapes checks each configured phase individually: prompt, name, limits, model/tier
// and first-phase placeholders.
func (c *Config) validatePhaseShapes() error {
	for i, ph := range c.Phases {
		if ph.Prompt == "" && builtinPhasePrompts[ph.Name] == "" {
			return fmt.Errorf("phases[%d]: no `prompt`, and %q is not a built-in phase name (built-ins: %s)",
				i, ph.Name, strings.Join(BuiltinPhaseNames(), ", "))
		}
		// The name reaches a log FILENAME, and statedir refuses one it does not like — which would cost
		// the phase its iteration log entirely, the one artifact an operator has to debug the sequence
		// with.
		if ph.Name != "" && !phaseNameRe.MatchString(ph.Name) {
			return fmt.Errorf("phases[%d]: name %q must be lowercase letters, digits and hyphens, with no leading, trailing or doubled hyphen (it becomes part of the iteration log filename)", i, ph.Name)
		}
		if ph.MaxTurns < 0 {
			return fmt.Errorf("phases[%d]: max_turns is negative", i)
		}
		if ph.MaxWallClock < 0 {
			return fmt.Errorf("phases[%d]: max_wall_clock is negative", i)
		}
		// A configured phase names its model ONE way (operator decision 3eddb7a5): a direct alias or a
		// tier bucket, never both.
		if first := ph.Model.First(); first != "" && strings.TrimSpace(ph.Tier) != "" {
			return fmt.Errorf("phases[%d] (%q) sets both `model` (%q) and `tier` (%q): a phase names its model one way — remove one of them (`model` is the alias it runs on, `tier` a bucket in `models`)",
				i, ph.Label(i), first, ph.Tier)
		}
		// The resume placeholders are filled from the claim the PREVIOUS phase left held, so the first
		// phase has nothing to fill them from.
		if i == 0 {
			effective := ph.Prompt
			if effective == "" {
				effective = builtinPhasePrompts[ph.Name]
			}
			for _, pl := range []string{PhaseTaskPlaceholder, PhaseTaskRefPlaceholder, PhaseRunPlaceholder, PhaseBranchPlaceholder, PhasePrevHarnessPlaceholder, PhasePrevModelPlaceholder, ReviewerRolePlaceholder, ReviewedTipPlaceholder} {
				if strings.Contains(effective, pl) {
					return fmt.Errorf("phases[0] (%q): its prompt carries %s, but the FIRST phase has no previous claim or phase to fill it from — it claims a task of its own, so a resume brief cannot go first",
						ph.Label(0), pl)
				}
			}
		}
	}
	return nil
}

// validateLastPhase refuses a sequence that ends on the implement brief.
func (c *Config) validateLastPhase() error {
	// A sequence whose last phase is the implement brief has no path to in_review: every task would
	// stop half-finished, forever.
	if n := len(c.Phases); n > 0 {
		lastPh := c.Phases[n-1]
		if lastPh.Prompt == "" && lastPh.Name == ImplementPhaseName {
			return fmt.Errorf("phases[%d]: the sequence ends on the %q brief, which tells its session to stop at the checkpoint — nothing would ever hand a task to review (add a %q phase, or give this one its own prompt)",
				n-1, ImplementPhaseName, ReviewPhaseName)
		}
	}
	return nil
}

// validateFixPhase checks the built-in fix brief is last (or followed by verify) and preceded by a review.
func (c *Config) validateFixPhase() error {
	// The fix phase is the one that carries the branch-shaped terminal step (PR, in_review), so it
	// must be LAST: a non-final fix would open the PR and hand the task over, and the phase after it
	// would resume a task that has already left the run.
	if n := len(c.Phases); n > 0 {
		for i, ph := range c.Phases {
			if ph.Prompt != "" || ph.Name != FixPhaseName {
				continue
			}
			verifyFollows := i+1 < n && c.Phases[i+1].Name == VerifyPhaseName && c.Phases[i+1].Prompt == ""
			if i != n-1 && !verifyFollows {
				return fmt.Errorf("phases[%d] (%q): the %q brief is the LAST phase of a sequence — it opens the PR and hands the task to in_review, so a phase after it would be spawned on a task that has already left the run (move %q to the end, or give this phase its own prompt)",
					i, ph.Label(i), FixPhaseName, FixPhaseName)
			}
			reviewed := false
			for _, prev := range c.Phases[:i] {
				if prev.Name == ReviewPhaseName {
					reviewed = true
					break
				}
			}
			if !reviewed {
				return fmt.Errorf("phases[%d] (%q): the %q brief applies a review's findings, but no %q phase precedes it — there would be no review record to read, and its only honest outcome is a reported failed hand-off (add a %q phase, or give this one its own prompt)",
					i, ph.Label(i), FixPhaseName, ReviewPhaseName, ReviewPhaseName)
			}
		}
	}
	return nil
}

// validateVerifyPhase checks the built-in verify brief position, predecessor and the review before it.
func (c *Config) validateVerifyPhase() error {
	// The verification round has one legal position and one legal predecessor.
	if n := len(c.Phases); n > 0 {
		for i, ph := range c.Phases {
			if ph.Prompt != "" || ph.Name != VerifyPhaseName {
				continue
			}
			if i != n-1 {
				return fmt.Errorf("phases[%d] (%q): the %q brief is the LAST phase of a sequence — it ends the verification loop (no open MUST), so a phase after it would never run (move %q to the end)",
					i, ph.Label(i), VerifyPhaseName, VerifyPhaseName)
			}
			if i == 0 || c.Phases[i-1].Prompt != "" || c.Phases[i-1].Name != FixPhaseName {
				return fmt.Errorf("phases[%d] (%q): the %q brief verifies a fix round, so it must directly follow the %q brief — with no fix between verification rounds the loop cannot make progress (add a %q phase before %q)",
					i, ph.Label(i), VerifyPhaseName, FixPhaseName, FixPhaseName, VerifyPhaseName)
			}
			reviewed := false
			for _, prev := range c.Phases[:i] {
				if prev.Name == ReviewPhaseName {
					reviewed = true
					break
				}
			}
			if !reviewed {
				return fmt.Errorf("phases[%d] (%q): the %q brief adjudicates a review's open MUSTs by id, but no %q phase precedes the fix it verifies — there would be no finding ids to adjudicate (add a %q phase before %q)",
					i, ph.Label(i), VerifyPhaseName, ReviewPhaseName, ReviewPhaseName, FixPhaseName)
			}
		}
	}
	return nil
}

// validateReviewSuccessor checks a non-final built-in review is followed by the fix brief or a
// phase with its own prompt.
func (c *Config) validateReviewSuccessor() error {
	// A non-final BUILT-IN review names the phase after it as the one that applies its findings, and
	// the driver's review→fix seam requires the review record to have been written.
	if n := len(c.Phases); n > 1 {
		for i, ph := range c.Phases[:n-1] {
			if ph.Prompt != "" || ph.Name != ReviewPhaseName {
				continue
			}
			next := c.Phases[i+1]
			if next.Prompt == "" && next.Name != FixPhaseName {
				return fmt.Errorf("phases[%d] (%q): the %q brief is REVIEW-ONLY — it records findings and changes nothing — so the phase after it must be the %q brief (or carry its own prompt saying who applies the findings); %q resolves to the %q built-in, which does neither",
					i, ph.Label(i), ReviewPhaseName, FixPhaseName, next.Label(i+1), next.Name)
			}
		}
	}
	return nil
}

// resolveTopMCPConfig defaults mcp_config_path to the workdir's .mcp.json and holds a discovered
// file to the discovered-file rule.
func (c *Config) resolveTopMCPConfig() error {
	// Default mcp_config_path to <workdir>/.mcp.json when that file exists.
	if c.MCPConfigPath == "" {
		c.MCPConfigPath = discoverMCPConfig(c.WorkDir)
		c.mcpDiscovered = c.MCPConfigPath != ""
		if err := c.checkDiscoveredMCPConfig(c.MCPConfigPath, "mcp_config_path", c.AllowLocalMCPServers); err != nil {
			return err
		}
	}
	return nil
}

// validateEndpoints settles the trusted origin and checks the URL and ref-name fields and the
// top-level MCP config.
func (c *Config) validateEndpoints() error {
	// Where the account-scoped API key is allowed to go, settled once, here.
	if c.BacklogURL == "" {
		c.BacklogURL = defaultBacklogURL
	}
	if _, err := secureurl.Origin(c.BacklogURL); err != nil {
		return fmt.Errorf("backlog_url: %w", err)
	}
	// health_url is read WITHOUT credentials (/health is public), so unlike backlog_url it is not held
	// to the bearer-token TLS floor - only to being a usable absolute URL.
	if err := validateHealthURL(c.HealthURL, "health_url"); err != nil {
		return err
	}
	// integration_branch reaches `git fetch`/`git ls-remote` in doctor's deploy_lag check; a
	// refspec-shaped value would mutate local refs during a preflight.
	if err := validateIntegrationBranch(c.IntegrationBranch, "integration_branch"); err != nil {
		return err
	}
	// pr_base values reach the phase briefs and the remote checks behind them, so they are held to the
	// same ref-name shape.
	if err := validatePRBase(c.PRBase, "pr_base"); err != nil {
		return err
	}
	// clone_url_base is concatenated with a repo identity to make the supervisor's clone URL, so a
	// value with no trailing separator would silently build a URL nobody can clone.
	c.CloneURLBase = strings.TrimSpace(c.CloneURLBase)
	if err := ValidateCloneURLBase(c.CloneURLBase); err != nil {
		return fmt.Errorf("clone_url_base: %w", err)
	}
	if err := c.checkMCPConfigOrigins(c.MCPConfigPath, "mcp_config_path"); err != nil {
		return err
	}
	if err := checkMCPConfigNamesClankerbar(c.MCPConfigPath, "mcp_config_path"); err != nil {
		return err
	}
	return nil
}

// validateHarnessMCPConfigs checks each per-harness MCP config and settings file.
func (c *Config) validateHarnessMCPConfigs() error {
	// Each per-harness MCP config points the SAME account key at a host, so it is held to the same
	// trusted origin.
	topSlug := slugFromMCPURL(mcpURLFromConfig(c.MCPConfigPath))
	for _, name := range sortedKeys(c.Harnesses) {
		hc := c.Harnesses[name]
		if err := c.checkMCPConfigOrigins(hc.MCPConfigPath, "harnesses."+name+".mcp_config_path"); err != nil {
			return err
		}
		if err := checkMCPConfigNamesClankerbar(hc.MCPConfigPath, "harnesses."+name+".mcp_config_path"); err != nil {
			return err
		}
		// The single-project split-brain, refused the same way the multi-project one is.
		if topSlug != "" && len(c.Projects) == 0 {
			if got := slugFromMCPURL(mcpURLFromConfig(hc.MCPConfigPath)); got != "" && got != topSlug {
				return fmt.Errorf("harnesses.%s.mcp_config_path names /mcp/%s, but the run polls /mcp/%s (from mcp_config_path) — "+
					"the poll would gate on one project while this harness's sessions claim and work another",
					name, got, topSlug)
			}
		}
		if err := refuseInsecureMode(hc.SettingsPath, groupOtherWrite); err != nil {
			return fmt.Errorf("harnesses.%s.settings_path: %w - it is the allow/deny policy the unattended session is gated by: chmod go-w %s",
				name, err, hc.SettingsPath)
		}
	}
	return nil
}

// validateSettingsMode refuses a group/other-writable settings file.
func (c *Config) validateSettingsMode() error {
	// The settings file IS the permission policy - the allow/deny rules that are the only thing gating
	// what an unattended session may call, since there is no human to prompt.
	if err := refuseInsecureMode(c.SettingsPath, groupOtherWrite); err != nil {
		return fmt.Errorf("settings_path: %w - it is the allow/deny policy the unattended session is gated by: chmod go-w %s", err, c.SettingsPath)
	}
	return nil
}

// validateProjects checks multi-project entries: slug, repos, paths and MCP configs.
func (c *Config) validateProjects() error {
	// The top-level repo map, for single-project configs (multi-project entries carry their own,
	// checked in the loop below).
	if err := validateRepos(c.Repos, "top level"); err != nil {
		return err
	}

	// Multi-project entries: slug required and unique; paths normalized; each project's mcp config
	// defaults to its own workdir's .mcp.json (falling back to the top-level one at invocation time
	// — see loop.Target).
	seen := make(map[string]bool, len(c.Projects))
	for i := range c.Projects {
		if err := c.validateProject(i, seen); err != nil {
			return err
		}
	}
	return nil
}

// validateProject checks one multi-project entry; seen carries the slugs already taken.
func (c *Config) validateProject(i int, seen map[string]bool) error {
	p := &c.Projects[i]
	if p.Slug == "" {
		return fmt.Errorf("projects[%d]: slug is required", i)
	}
	if err := validateRepos(p.Repos, fmt.Sprintf("projects[%d] (%s)", i, p.Slug)); err != nil {
		return err
	}
	if seen[p.Slug] {
		return fmt.Errorf("projects: duplicate slug %q", p.Slug)
	}
	seen[p.Slug] = true
	p.WorkDir = expandHome(p.WorkDir)
	if p.WorkDir != "" {
		// Absolute for the same reason the top-level workdir is, and so SessionWorkDirs can be compared
		// against a state dir path lexically.
		abs, err := filepath.Abs(p.WorkDir)
		if err != nil {
			return fmt.Errorf("projects[%d].workdir %s: %w", i, p.WorkDir, err)
		}
		p.WorkDir = abs
	}
	p.MCPConfigPath = expandHome(p.MCPConfigPath)
	// Against the workdir the SESSIONS for this project get - its own, falling back to the top-level
	// one, exactly as loop.Driver.invocation resolves it.
	effectiveWorkDir := p.WorkDir
	if effectiveWorkDir == "" {
		effectiveWorkDir = c.WorkDir
	}
	p.MCPConfigPath = mcpUnderWorkDir(p.MCPConfigPath, effectiveWorkDir)
	if p.MCPConfigPath == "" {
		p.MCPConfigPath = discoverMCPConfig(effectiveWorkDir)
		p.mcpDiscovered = p.MCPConfigPath != ""
		// Discovered, not named, exactly like the top-level default above — same rule, labelled with
		// THIS field so the remedy names the line the operator actually adds, and allowlisted per THIS
		// project.
		if err := c.checkDiscoveredMCPConfig(p.MCPConfigPath, fmt.Sprintf("projects[%d].mcp_config_path", i), c.AllowLocalMCPServersFor(p.Slug)); err != nil {
			return err
		}
	}
	if err := c.checkMCPConfigOrigins(p.MCPConfigPath, fmt.Sprintf("projects[%d].mcp_config_path", i)); err != nil {
		return err
	}
	if err := checkMCPConfigNamesClankerbar(p.MCPConfigPath, fmt.Sprintf("projects[%d].mcp_config_path", i)); err != nil {
		return err
	}
	if err := validateHealthURL(p.HealthURL, fmt.Sprintf("projects[%d].health_url", i)); err != nil {
		return err
	}
	if err := validateIntegrationBranch(p.IntegrationBranch, fmt.Sprintf("projects[%d].integration_branch", i)); err != nil {
		return err
	}
	if err := validatePRBase(p.PRBase, fmt.Sprintf("projects[%d].pr_base", i)); err != nil {
		return err
	}
	// The slug decides which queue is POLLED; the .mcp.json decides which project the sessions WORK.
	if fromMCP := slugFromMCPURL(mcpURLFromConfig(p.MCPConfigPath)); fromMCP != "" && fromMCP != p.Slug {
		return fmt.Errorf("projects[%d]: slug %q does not match its .mcp.json, which names /mcp/%s — the poll would gate on one project while sessions work another", i, p.Slug, fromMCP)
	}
	// The per-harness files get every check the project's own file just had: they are the same account
	// key pointed at the same kind of host, and the same split-brain is available through any one of
	// them.
	for _, name := range sortedKeys(p.MCPConfigPaths) {
		path := mcpUnderWorkDir(p.MCPConfigPaths[name], effectiveWorkDir)
		p.MCPConfigPaths[name] = path
		label := fmt.Sprintf("projects[%d].mcp_config_paths.%s", i, name)
		if err := c.checkMCPConfigOrigins(path, label); err != nil {
			return err
		}
		if err := checkMCPConfigNamesClankerbar(path, label); err != nil {
			return err
		}
		if fromMCP := slugFromMCPURL(mcpURLFromConfig(path)); fromMCP != "" && fromMCP != p.Slug {
			return fmt.Errorf("%s: slug %q does not match that file, which names /mcp/%s — the poll would gate on one project while sessions work another", label, p.Slug, fromMCP)
		}
	}
	return nil
}

// validatePhaseProjectMCP checks a phase on a non-top-level harness has a per-project MCP config
// for every project.
func (c *Config) validatePhaseProjectMCP() error {
	// A phase on a harness other than the run's needs its own MCP config PER PROJECT, because that
	// file carries both the schema (the harness's) and the slug (the project's).
	for i, ph := range c.Phases {
		h := c.HarnessFor(ph)
		if h == c.Harness {
			continue
		}
		for j := range c.Projects {
			if c.Projects[j].MCPConfigPaths[h] == "" {
				return fmt.Errorf("phases[%d] (%q) runs on harness %q, but projects[%d] (%q) declares no `mcp_config_paths.%s`: "+
					"that file carries both the harness's schema and the project's /mcp/<slug>, so one per project is needed "+
					"(the top-level `harnesses.%s.mcp_config_path` names a single project and cannot serve them all)",
					i, ph.Label(i), h, j, c.Projects[j].Slug, h, h)
			}
		}
	}
	return nil
}

// validateEnvDeclarations validates the session env declarations at all four levels.
func (c *Config) validateEnvDeclarations() error {
	// The session env declarations are validated at all FOUR levels here — names and commands are
	// static facts of the file, so a bad one is a config error, not a spawn-time surprise.
	if err := validateEnvDecls(c.Env, "env"); err != nil {
		return err
	}
	for _, name := range sortedKeys(c.Harnesses) {
		if err := validateEnvDecls(c.Harnesses[name].Env, "harnesses."+name+".env"); err != nil {
			return err
		}
	}
	for i := range c.Projects {
		if err := validateEnvDecls(c.Projects[i].Env, fmt.Sprintf("projects[%d].env", i)); err != nil {
			return err
		}
		for _, h := range sortedKeys(c.Projects[i].EnvPerHarness) {
			if err := validateEnvDecls(c.Projects[i].EnvPerHarness[h], fmt.Sprintf("projects[%d].env_per_harness.%s", i, h)); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateEnvAllowlists validates the inherited-env allowlists at all four levels.
func (c *Config) validateEnvAllowlists() error {
	// The inherited-env allowlists are validated at all four levels too: a name that can never match,
	// a duplicate, or an explicit empty list is a config error, not a spawn-time surprise.
	if err := validateEnvAllowlist(c.EnvAllowlist, "env_allowlist"); err != nil {
		return err
	}
	for _, name := range sortedKeys(c.Harnesses) {
		if err := validateEnvAllowlist(c.Harnesses[name].EnvAllowlist, "harnesses."+name+".env_allowlist"); err != nil {
			return err
		}
	}
	for i := range c.Projects {
		if err := validateEnvAllowlist(c.Projects[i].EnvAllowlist, fmt.Sprintf("projects[%d].env_allowlist", i)); err != nil {
			return err
		}
		for _, h := range sortedKeys(c.Projects[i].EnvAllowlistPerHarness) {
			// The KEY is checked against the harness registry too, and here that matters more than in the
			// sibling env maps: an inert `env_per_harness` entry merely fails to add a variable, whereas an
			// allowlist keyed by a name no adapter answers to is never consulted.
			if !harness.Known(h) {
				return fmt.Errorf("projects[%d].env_allowlist_per_harness[%q]: unknown harness (want: %s)%s",
					i, h, strings.Join(harness.Names(), ", "), retiredHarnessHint(h))
			}
			if err := validateEnvAllowlist(c.Projects[i].EnvAllowlistPerHarness[h], fmt.Sprintf("projects[%d].env_allowlist_per_harness.%s", i, h)); err != nil {
				return err
			}
		}
	}
	return nil
}

// discoverMCPConfig returns <workdir>/.mcp.json if that file exists, else "".
func discoverMCPConfig(workdir string) string {
	base := workdir
	if base == "" {
		base = "."
	}
	p := filepath.Join(base, ".mcp.json")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// sortedKeys is map iteration made deterministic, for the checks whose ERROR depends on which entry
// is reached first. A validation message that names a different block on each run of the same file
// reads as a flaky check rather than a config with two problems.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
