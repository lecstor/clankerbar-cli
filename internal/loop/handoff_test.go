package loop

import (
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// handoffResult is a session that ended cleanly, still holding its task, with
// its final message ending in a handoff block asking for prompt.
func handoffResult(prompt string) harness.Result {
	res := held(okResult(1, 0), openClaim())
	res.FinalMessage = "Exploration done; constraints are in docs/, branch state on the task.\n" +
		config.HandoffMarker + "\n" + prompt
	return res
}

// ---------------------------------------------------------------------------
// Marker detection: the parse layer alone.

func TestParseHandoff_Detection(t *testing.T) {
	long := strings.Repeat("x", config.HandoffPromptMax+1)
	for _, tc := range []struct {
		name       string
		final      string
		wantPrompt string
		wantFound  bool
		refused    bool
	}{
		{name: "no marker", final: "All done, task handed to review.", wantFound: false},
		{
			name:       "marker line followed by the prompt",
			final:      "Pivot reached.\n" + config.HandoffMarker + "\nResume: implement the parser.\nThen run the tests.",
			wantPrompt: "Resume: implement the parser.\nThen run the tests.",
			wantFound:  true,
		},
		{
			name:       "surrounding whitespace on the marker line is ignored",
			final:      "  " + config.HandoffMarker + "  \nDo the next thing.",
			wantPrompt: "Do the next thing.",
			wantFound:  true,
		},
		{
			// The block is defined as ENDING the message, so a session that quotes
			// the marker while discussing it and then emits a real block gets the
			// real one — everything above the last marker is not the prompt.
			name:       "the last marker wins",
			final:      config.HandoffMarker + "\nstale half\n" + config.HandoffMarker + "\nthe real prompt",
			wantPrompt: "the real prompt",
			wantFound:  true,
		},
		{
			// The built-in briefs quote the marker INLINE in a sentence; a session
			// echoing its brief must not read as handing off.
			name:      "an inline mention is not a marker line",
			final:     "end your final message with " + config.HandoffMarker + " followed by the prompt",
			wantFound: false,
		},
		{name: "marker with no prompt is refused", final: "done\n" + config.HandoffMarker, wantFound: true, refused: true},
		{name: "marker with only whitespace after it is refused", final: config.HandoffMarker + "\n  \n\t\n", wantFound: true, refused: true},
		{
			name:      "an over-cap prompt is refused, not truncated",
			final:     config.HandoffMarker + "\n" + long,
			wantFound: true,
			refused:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt, found, refusal := parseHandoff(tc.final)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v (refusal=%q)", found, tc.wantFound, refusal)
			}
			if (refusal != "") != tc.refused {
				t.Fatalf("refusal = %q, want refused=%v", refusal, tc.refused)
			}
			if prompt != tc.wantPrompt {
				t.Errorf("prompt = %q, want %q", prompt, tc.wantPrompt)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The driver: a handoff respawns a fresh session on the emitted prompt, with
// the same run continuity as a phase resume.

func TestDrainPhases_AHandoffRespawnsAFreshSessionOnTheEmittedPrompt(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: handoffResult("Resume CLA-352: the parser is written; wire it into drainPhases and add the guards.")},
		{res: okResult(1, 0)},
	}}
	d, rel := phaseDriver(t, h, nil)
	d.cfg.Prompt = "Work the next backlog item."

	_, _, handoffs, stop, err := drainPhasesHandoffs(t, d, 1)
	if err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — the whole feature is that the marker respawns a successor", h.invokeCalls)
	}
	if got := h.invocations[1].Prompt; got != "Resume CLA-352: the parser is written; wire it into drainPhases and add the guards." {
		t.Errorf("the successor was asked %q, not the session-authored prompt", got)
	}
	// Same run continuity rules as a phase resume: the successor is seeded with
	// the predecessor's claim, so the salvage, handback and delivery checks stay
	// live for the session doing the pushing.
	if got := h.invocations[1].ResumeClaim; got.TaskID != "t-1" || got.RunID != "r-1" {
		t.Errorf("the successor's ResumeClaim = %+v, want the claim the predecessor held", got)
	}
	if handoffs != 1 {
		t.Errorf("handoffs = %d, want 1 — Run charges each respawn against max_iterations", handoffs)
	}
	// The claim survives the handoff seam and is released once, at the end.
	if len(rel.calls) != 1 {
		t.Errorf("the task was handed back %d times, want exactly once at the end of the sequence: %+v", len(rel.calls), rel.calls)
	}
	// The chain is visible in the state dir the way phases are.
	logs := iterationLogs(t, d.state.Path())
	both := strings.Join(logs, " ")
	if !strings.Contains(both, "-h1-") {
		t.Errorf("no iteration log carries the handoff tag -h1-: %v", logs)
	}
}

// A handoff mid-sequence continues the SAME phase — its turn cap still applies
// — and the configured next phase still runs afterwards on its standard brief.
func TestDrainPhases_AHandoffMidSequenceContinuesTheSamePhase(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: handoffResult("Continue implementing: the design is settled, write the code.")},
		{res: checkpointed(1, 0)},
		{res: okResult(1, 0)},
	}}
	d, _ := phaseDriver(t, h, []config.Phase{
		{Name: "implement", MaxTurns: 40},
		{Name: "review"},
	})

	_, _, handoffs, stop, err := drainPhasesHandoffs(t, d, 1)
	if err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3: implement, its handoff successor, review", h.invokeCalls)
	}
	if got := h.invocations[1].MaxTurns; got != 40 {
		t.Errorf("the handoff successor ran with MaxTurns=%d, want the phase's own 40 — a respawn continues the phase, knobs included", got)
	}
	if !strings.Contains(h.invocations[2].Prompt, "PHASE 2") {
		t.Errorf("the phase after the handoff chain was not the standard review brief: %q", h.invocations[2].Prompt)
	}
	if handoffs != 1 {
		t.Errorf("handoffs = %d, want 1", handoffs)
	}
}

// The runaway-chain cap: a session cannot chain itself indefinitely. At the cap
// the emitted prompt is refused and the sequence falls back to the standard
// path.
func TestDrainPhases_TheHandoffChainCapEndsARunawayChain(t *testing.T) {
	// Every session, initial and respawned alike, asks for another handoff.
	steps := make([]invokeStep, 0, handoffChainCap+2)
	for range [handoffChainCap + 2]struct{}{} {
		steps = append(steps, invokeStep{res: handoffResult("again")})
	}
	h := &fakeAdapter{steps: steps}
	d, rel := phaseDriver(t, h, nil)
	d.cfg.Prompt = "Work the next backlog item."

	_, _, handoffs, stop, err := drainPhasesHandoffs(t, d, 1)
	if err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if want := 1 + handoffChainCap; h.invokeCalls != want {
		t.Fatalf("spawned %d sessions, want %d — the chain must stop at the cap", h.invokeCalls, want)
	}
	if handoffs != handoffChainCap {
		t.Errorf("handoffs = %d, want %d", handoffs, handoffChainCap)
	}
	// The refused handoff falls back to the normal path: the drain ends and the
	// held task is handed back for the queue's standard pickup.
	if len(rel.calls) != 1 {
		t.Errorf("the task was handed back %d times, want once: %+v", len(rel.calls), rel.calls)
	}
}

// The size cap guard: over-cap means refused and logged, and the driver falls
// back to the normal path — no respawn, no truncation.
func TestDrainPhases_AnOversizeHandoffPromptIsRefusedWithFallback(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: handoffResult(strings.Repeat("x", config.HandoffPromptMax+1))},
	}}
	d, rel := phaseDriver(t, h, nil)
	d.cfg.Prompt = "Work the next backlog item."

	_, _, handoffs, stop, err := drainPhasesHandoffs(t, d, 1)
	if err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want 1 — an over-cap prompt must not spawn a successor", h.invokeCalls)
	}
	if handoffs != 0 {
		t.Errorf("handoffs = %d, want 0", handoffs)
	}
	if len(rel.calls) != 1 {
		t.Errorf("the task was handed back %d times, want once — the normal path for a clean end still holding it: %+v", len(rel.calls), rel.calls)
	}
}

// A respawn counts as an iteration: with max_iterations already spent, the
// handoff is refused rather than letting a session extend the run past the
// operator's ceiling.
func TestDrainPhases_AHandoffCountsAgainstMaxIterations(t *testing.T) {
	t.Run("ceiling already spent: refused", func(t *testing.T) {
		h := &fakeAdapter{steps: []invokeStep{{res: handoffResult("more")}}}
		d, _ := phaseDriver(t, h, nil)
		d.cfg.Prompt = "Work the next backlog item."
		d.cfg.MaxIterations = 1

		_, _, handoffs, _, err := drainPhasesHandoffs(t, d, 1)
		if err != nil {
			t.Fatalf("drainPhases: %v", err)
		}
		if h.invokeCalls != 1 || handoffs != 0 {
			t.Errorf("invokes=%d handoffs=%d, want 1 and 0 — this drain was the last iteration the ceiling allows", h.invokeCalls, handoffs)
		}
	})
	t.Run("one iteration of headroom buys exactly one respawn", func(t *testing.T) {
		h := &fakeAdapter{steps: []invokeStep{
			{res: handoffResult("first")},
			{res: handoffResult("second")},
		}}
		d, _ := phaseDriver(t, h, nil)
		d.cfg.Prompt = "Work the next backlog item."
		d.cfg.MaxIterations = 2

		_, _, handoffs, _, err := drainPhasesHandoffs(t, d, 1)
		if err != nil {
			t.Fatalf("drainPhases: %v", err)
		}
		if h.invokeCalls != 2 || handoffs != 1 {
			t.Errorf("invokes=%d handoffs=%d, want 2 and 1", h.invokeCalls, handoffs)
		}
	})
}

// The budget check runs BEFORE the respawn, not after: a session that blew the
// ceiling cannot spend more by handing off to itself.
func TestDrainPhases_BudgetIsCheckedBeforeTheRespawn(t *testing.T) {
	over := handoffResult("more")
	over.Tokens = 500
	h := &fakeAdapter{steps: []invokeStep{{res: over}}}
	d, _ := phaseDriver(t, h, nil)
	d.cfg.Prompt = "Work the next backlog item."
	d.cfg.Budget = config.Budget{MaxTokens: 100}

	_, _, handoffs, stop, err := drainPhasesHandoffs(t, d, 1)
	if err != nil {
		t.Fatalf("drainPhases: %v", err)
	}
	if !stop {
		t.Error("the budget was blown before the respawn and the run did not stop")
	}
	if h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want 1 — the successor must never launch past the ceiling", h.invokeCalls)
	}
	if handoffs != 0 {
		t.Errorf("handoffs = %d, want 0 — a respawn that never spawned is not an iteration", handoffs)
	}
}

// The guards on WHAT may hand off: only a clean exit still holding the task.
func TestDrainPhases_AHandoffNeedsACleanExitAndAHeldClaim(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  harness.Result
	}{
		{
			// A settled task leaves no run to resume; the marker is ignored.
			name: "settled task",
			res: func() harness.Result {
				r := handoffResult("more")
				r.Claim.Settled = true
				return r
			}(),
		},
		{
			// CLA-262: an untrusted stream is not read for a respawn prompt any
			// more than for claim state — the block may be cut mid-prompt.
			name: "untrusted stream",
			res: func() harness.Result {
				r := handoffResult("more")
				r.Untrusted = "a line overran the reader"
				return r
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeAdapter{steps: []invokeStep{{res: tc.res}}}
			d, _ := phaseDriver(t, h, nil)
			d.cfg.Prompt = "Work the next backlog item."

			_, _, handoffs, _, err := drainPhasesHandoffs(t, d, 1)
			if err != nil {
				t.Fatalf("drainPhases: %v", err)
			}
			if h.invokeCalls != 1 || handoffs != 0 {
				t.Errorf("invokes=%d handoffs=%d, want 1 and 0", h.invokeCalls, handoffs)
			}
		})
	}
}

// Run-level accounting: a drain's respawns are charged against max_iterations,
// so a run capped at 2 iterations that spends both on one task spawns no third
// session even with the queue still claimable.
func TestRun_HandoffRespawnsCountAgainstMaxIterations(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 2
	h := &fakeAdapter{steps: []invokeStep{
		{res: handoffResult("finish it")},
		{res: okResult(1, 0)},
	}}
	if err := runLoop(t, cfg, h, busyPoller()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if h.invokeCalls != 2 {
		t.Errorf("spawned %d sessions, want 2 — the respawn consumed the second iteration, so no second drain may start", h.invokeCalls)
	}
}
