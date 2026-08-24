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
			name:       "trailing whitespace on the marker line is ignored",
			final:      config.HandoffMarker + "  \nDo the next thing.",
			wantPrompt: "Do the next thing.",
			wantFound:  true,
		},
		{
			// An indented marker is a quotation, not an emission: a session that
			// explains the mechanism (or explains why it decided against a
			// handoff) inside a list item indents it, and that must not be
			// mistaken for a real handoff block.
			name:       "an indented marker line does not match",
			final:      "  " + config.HandoffMarker + "\nDo the next thing.",
			wantPrompt: "",
			wantFound:  false,
		},
		{
			// A fenced code block is the OTHER natural way a session quotes the
			// marker while explaining the mechanism — unindented, so the
			// indentation check alone would not catch it.
			name:       "a marker line inside a fenced code block does not match",
			final:      "Here's how it works:\n```\n" + config.HandoffMarker + "\n<the prompt>\n```\nI decided not to.",
			wantPrompt: "",
			wantFound:  false,
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
	// The successor is not handed the predecessor's prompt bare: it is framed
	// by config.HandoffPreamble, with the "resume, don't claim" contract a
	// phase resume gets from its brief, placeholders substituted from the
	// predecessor's claim.
	wantPrompt := strings.NewReplacer(
		config.PhaseTaskPlaceholder, "t-1",
		config.PhaseRunPlaceholder, "r-1",
	).Replace(config.HandoffPreamble) + "Resume CLA-352: the parser is written; wire it into drainPhases and add the guards."
	if got := h.invocations[1].Prompt; got != wantPrompt {
		t.Errorf("the successor was asked %q, want %q", got, wantPrompt)
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

// CLA-353: a handoff respawn replaces ph.Prompt wholesale with
// config.HandoffPreamble plus the predecessor's own prompt (CLA-352) — the
// built-in brief's own text is not otherwise part of it. A session that hands
// off mid-review must not thereby drop the terminal PR-then-update_task step for
// its successor, which is exactly what an earlier version of this fix would have
// done: the brief said more, but only the phase's FIRST session ever saw it.
func TestDrainPhases_AHandoffDuringReviewCarriesTheTerminalStepForward(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: checkpointed(10, 0.10)},
		{res: handoffResult("Findings fixed: the nil-check and the missing test. Pushing the rest next.")},
		{res: okResult(1, 0)},
	}}
	d, _ := phaseDriver(t, h, twoPhases())

	_, _, handoffs, stop, err := drainPhasesHandoffs(t, d, 1)
	if err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 3 {
		t.Fatalf("spawned %d sessions, want 3: implement, review, review's handoff successor", h.invokeCalls)
	}
	successor := h.invocations[2].Prompt
	if !strings.Contains(successor, "update_task(taskId, runId, status: \"in_review\"") {
		t.Errorf("the review phase's handoff successor lost the terminal update_task step: %q", successor)
	}
	if !strings.Contains(successor, "PR") || !strings.Contains(successor, "staging") {
		t.Errorf("the review phase's handoff successor lost the PR-targeting-staging step: %q", successor)
	}
	if handoffs != 1 {
		t.Errorf("handoffs = %d, want 1", handoffs)
	}
}

// CLA-353 review finding: HandoffContinuation matches on phase NAME alone, so
// without this gate an operator who reuses the name "review" for a phase
// carrying their own custom Prompt would get the shipped brief's terminal step
// (its own update_task/PR contract) force-injected into a handoff respawn of a
// brief that never established that contract — clashing with whatever terminal
// step, if any, the operator's own prompt names. Only a phase actually running
// the BUILT-IN brief (no configured Prompt of its own) may carry it forward.
func TestDrainPhases_ACustomPromptNamedReviewGetsNoInjectedTerminalStep(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{
		{res: handoffResult("Handing off my own custom review-shaped work.")},
		{res: okResult(1, 0)},
	}}
	d, _ := phaseDriver(t, h, []config.Phase{
		{Name: "review", Prompt: "Do your own custom review-shaped job, whatever that means here."},
	})

	_, _, handoffs, stop, err := drainPhasesHandoffs(t, d, 1)
	if err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	successor := h.invocations[1].Prompt
	if strings.Contains(successor, "update_task(taskId, runId, status: \"in_review\"") {
		t.Errorf("a phase named %q running its OWN custom prompt got the shipped review brief's terminal step force-injected on handoff: %q", "review", successor)
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
		d, rel := phaseDriver(t, h, nil)
		d.cfg.Prompt = "Work the next backlog item."
		d.cfg.MaxIterations = 1

		_, _, handoffs, _, err := drainPhasesHandoffs(t, d, 1)
		if err != nil {
			t.Fatalf("drainPhases: %v", err)
		}
		if h.invokeCalls != 1 || handoffs != 0 {
			t.Errorf("invokes=%d handoffs=%d, want 1 and 0 — this drain was the last iteration the ceiling allows", h.invokeCalls, handoffs)
		}
		// CLA-421, release side of the refused-handoff seam: nothing was
		// pushed, so there is no takeover hand-off to preserve and the queue
		// may have the task straight away — no waiting out the lease.
		if len(rel.calls) != 1 {
			t.Errorf("the task was handed back %d times, want once: %+v", len(rel.calls), rel.calls)
		}
	})
	t.Run("ceiling already spent with work pushed: the lease is kept for the takeover hand-off", func(t *testing.T) {
		// CLA-421, keep side of the refused-handoff seam — the deliberate
		// choice, pinned so it stays one. A guard-refused handoff that still
		// holds pushed work leaves the lease to expire: releasing would post
		// `ready` over the branch and discard the plane's requiresTakeover
		// hand-off, so the next clanker would start from nothing beside work
		// it cannot see. The expiry sweep re-offers the task as a takeover
		// with the branch attached — the same disposition the budget-stop
		// seam already pins (TestDrainPhases_WhatTheSeamOwes...), decided
		// here for the guard-refused handoff rather than fallen into.
		res := handoffResult("more")
		res.Claim.HasWIP = true
		h := &fakeAdapter{steps: []invokeStep{{res: res}}}
		d, rel := phaseDriver(t, h, nil)
		d.cfg.Prompt = "Work the next backlog item."
		d.cfg.MaxIterations = 1

		_, _, handoffs, _, err := drainPhasesHandoffs(t, d, 1)
		if err != nil {
			t.Fatalf("drainPhases: %v", err)
		}
		if h.invokeCalls != 1 || handoffs != 0 {
			t.Errorf("invokes=%d handoffs=%d, want 1 and 0", h.invokeCalls, handoffs)
		}
		if len(rel.calls) != 0 {
			t.Errorf("the driver released a claim holding pushed work %+v — the takeover hand-off would be lost: %+v", res.Claim, rel.calls)
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

// CLA-421: the incident shape. A session ends cleanly, its final message ends
// with a handoff block, and the adapter observed NO claim from it — broken MCP
// wiring, or claim events the parser never saw — so the driver holds no
// task/run ids and cannot seed a successor's resume preamble. The marker is
// refused for THAT reason, named as itself rather than folded into "no longer
// holds a task" (which describes a settled task, a different situation), and
// the sequence falls back to the standard path without spawning anything.
func TestDrainPhases_AMarkerWithNoObservedClaimIsRefusedForThatReason(t *testing.T) {
	unobserved := handoffResult("Resume: wire the parser in and pin the guards.")
	unobserved.Claim = harness.Claim{} // the driver never saw a claim
	h := &fakeAdapter{steps: []invokeStep{{res: unobserved}}}
	d, rel := phaseDriver(t, h, nil)
	d.cfg.Prompt = "Work the next backlog item."
	logged := captureLogs(t)

	_, _, handoffs, stop, err := drainPhasesHandoffs(t, d, 1)
	if err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 1 || handoffs != 0 {
		t.Errorf("invokes=%d handoffs=%d, want 1 and 0 — with no ids to seed, no successor may spawn", h.invokeCalls, handoffs)
	}
	// Nothing was ever observed, so nothing is owed: the driver must not touch
	// ANY lease on the strength of a claim it cannot see. A possibly-live one
	// goes to the plane's expiry sweep, which keeps a recorded branch attached
	// as the takeover hand-off.
	if len(rel.calls) != 0 {
		t.Errorf("the driver released on a claim it never observed (%+v): %+v", unobserved.Claim, rel.calls)
	}
	out := logged.String()
	if !strings.Contains(out, "observed no claim from this session") {
		t.Errorf("the refusal must state its own reason — no claim was observed, so no ids exist to seed a resume; got:\n%s", out)
	}
	if !strings.Contains(out, "takeover hand-off") {
		t.Errorf("the refusal must name where a possibly-live lease goes — the expiry sweep, branch attached; got:\n%s", out)
	}
	if strings.Contains(out, "no longer holds a task") {
		t.Errorf("the old ambiguous wording should be gone — a never-observed claim is not a settled one; got:\n%s", out)
	}
}

// CLA-421: the two "no run to resume" shapes are different situations, and the
// log says which one happened. A settled task is a final, correct refusal;
// an unobserved claim means the ids were never seen and a possibly-live lease
// goes to the expiry sweep with its takeover hand-off intact. An operator
// reading the daemon log must not have to guess between them — and the
// zero-claim case must never wear the settled wording, because it reads as
// "nothing is owed" about a lease that may be ticking.
func TestDetectHandoff_TheTwoNoRunRefusalsNameThemselves(t *testing.T) {
	d, _ := phaseDriver(t, &fakeAdapter{}, nil)
	for _, tc := range []struct {
		name    string
		res     harness.Result
		want    []string
		notWant []string
	}{
		{
			name: "settled task",
			res: func() harness.Result {
				r := handoffResult("more")
				r.Claim.Settled = true
				return r
			}(),
			want:    []string{"settled t-1", "no run for a successor to resume"},
			notWant: []string{"observed no claim"},
		},
		{
			name: "no claim observed",
			res: func() harness.Result {
				r := handoffResult("more")
				r.Claim = harness.Claim{}
				return r
			}(),
			want:    []string{"observed no claim", "task/run ids", "takeover hand-off"},
			notWant: []string{"settled t-1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := captureLogs(t)
			if got := detectHandoff(1, d.targets[0], tc.res); got != "" {
				t.Fatalf("detectHandoff = %q, want a refusal", got)
			}
			out := logged.String()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("the refusal is missing %q; got:\n%s", want, out)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(out, notWant) {
					t.Errorf("the refusal carries the other shape's wording %q; got:\n%s", notWant, out)
				}
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
