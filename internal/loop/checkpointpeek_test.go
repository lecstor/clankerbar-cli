package loop

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// The CLA-451 checkpoint peek: when a phase ends checkpointable but its stream
// carried NO claim state at all - the session claimed through a channel the
// driver cannot observe (a raw API call with the same key, a harness quirk, a
// renamed tool) - the driver asks the plane about the task it BRIEFED before
// declaring the phase empty. A plane answer of held-with-branch becomes the
// phase's checkpoint, gated by the same origin-remote evidence an observed
// claim faces; every guard (untrusted stream, dead phase, not held, peek
// failure) keeps today's behavior.

// peekReleaser is a fakeReleaser that can also read the queue head and one
// task's holder state — the shape the real mcpReleaser has since CLA-437 and
// CLA-451. `state` answers TaskState per task id; a missing id decodes to a
// zero TaskState ("ready", no run, no branch), like the plane's nulls.
type peekReleaser struct {
	fakeReleaser
	next     plane.NextTask
	state    map[string]plane.TaskState
	stateErr error
	asked    []string // task ids TaskState was called with, in order
}

func (r *peekReleaser) PeekNextTask(context.Context) (plane.NextTask, error) {
	return r.next, nil
}

func (r *peekReleaser) TaskState(_ context.Context, taskID string) (plane.TaskState, error) {
	r.asked = append(r.asked, taskID)
	if r.stateErr != nil {
		return plane.TaskState{}, r.stateErr
	}
	return r.state[taskID], nil
}

// blindSpotDriver builds the two-phase driver the peek tests drive: phase 1's
// session ends cleanly having observed NO claim, and the plane holds the
// briefed task t-1 for run r-curl with branch clanker/x recorded.
func blindSpotDriver(t *testing.T, h *fakeAdapter, rel *peekReleaser) *Driver {
	t.Helper()
	cfg := fastCfg()
	cfg.Phases = twoPhases()
	cfg.Prompt = ""
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)
	// Same stub the other seam tests use: a worktree-shaped workdir and a
	// BranchPushed check that passes. Tests that exercise the evidence gate
	// override this.
	d.cfg.WorkDir = t.TempDir()
	d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
	return d
}

func TestDrainPhases_ABlindSpotClaimIsRecoveredFromThePlaneAtTheSeam(t *testing.T) {
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{
		// Phase 1 ends cleanly, real usage, and the stream carried NOTHING about
		// claims: the raw-curl shape. Today this ends the drain.
		{res: okResult(10, 0.10)},
		{res: okResult(5, 0.05)},
	}}
	rel := &peekReleaser{
		next: plane.NextTask{TaskID: "t-1"},
		state: map[string]plane.TaskState{
			"t-1": {Status: "in_progress", Ref: "CLA-9", ClaimedByRun: "r-curl", Branch: "clanker/x"},
		},
	}
	d := blindSpotDriver(t, h, rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — the plane-held briefed task IS the checkpoint, so the review phase runs exactly as after an observed claim", h.invokeCalls)
	}
	p2 := h.invocations[1].Prompt
	if !strings.Contains(p2, "t-1") || !strings.Contains(p2, "r-curl") {
		t.Errorf("phase 2 was not told which run to resume; prompt = %q", p2)
	}
	if strings.Contains(p2, config.PhaseTaskPlaceholder) || strings.Contains(p2, config.PhaseRunPlaceholder) {
		t.Errorf("a placeholder survived into the resumed brief: %q", p2)
	}
	wantClaim := harness.Claim{TaskID: "t-1", Ref: "CLA-9", RunID: "r-curl", HasWIP: true, Branch: "clanker/x"}
	if got := h.invocations[1].ResumeClaim; got != wantClaim {
		t.Errorf("phase 2 ResumeClaim = %+v, want %+v; without the seed the salvage, handback and delivery checks are all inert", got, wantClaim)
	}

	out := logs.String()
	if !strings.Contains(out, "treating it as the phase checkpoint") {
		t.Errorf("the log does not say the plane record was taken as the checkpoint:\n%s", out)
	}
	if strings.Contains(out, "ended without holding the task") {
		t.Errorf("the standard empty-seam line still fired over a recovered checkpoint:\n%s", out)
	}
	// The synthesized hold is NOT releasable (HasWIP), so the sequence-end
	// deferred handback leaves the lease to expire rather than posting ready
	// over pushed work — the same disposition an observed phase-1 hold gets.
	if len(rel.calls) != 0 {
		t.Errorf("released the recovered claim %d time(s): %+v — a held claim with a branch must never be handed back", len(rel.calls), rel.calls)
	}
	if strings.Join(rel.asked, ",") != "t-1" {
		t.Errorf("the plane was asked about %v, want exactly the briefed task t-1", rel.asked)
	}
}

// Each mutation keeps today's behavior: the drain ends after phase 1, the
// standard ending line fires, and the guards that make the peek unsafe mean it
// is never even asked.
func TestDrainPhases_CheckpointPeekMutationsKeepTodaysBehavior(t *testing.T) {
	for _, tc := range []struct {
		name string

		// The phase-1 result (no claim state unless stated).
		res func() harness.Result
		rel func() *peekReleaser

		wantAsked   int    // how many times the plane may be asked
		wantLog     string // a line that MUST appear
		notLog      string // a line that must NOT appear ("" = don't care)
		description string
	}{
		{
			name: "task not held on the plane",
			res:  func() harness.Result { return okResult(1, 0) },
			rel: func() *peekReleaser {
				return &peekReleaser{
					next:  plane.NextTask{TaskID: "t-1"},
					state: map[string]plane.TaskState{"t-1": {Status: "ready"}}, // released or never claimed
				}
			},
			wantAsked:   1,
			wantLog:     "ended without holding the task",
			notLog:      "treating it as the phase checkpoint",
			description: "a task nobody holds has no lease worth keeping and nothing to resume",
		},
		{
			name: "held without a recorded branch",
			res:  func() harness.Result { return okResult(1, 0) },
			rel: func() *peekReleaser {
				return &peekReleaser{
					next:  plane.NextTask{TaskID: "t-1"},
					state: map[string]plane.TaskState{"t-1": {Status: "in_progress", ClaimedByRun: "r-curl"}},
				}
			},
			wantAsked:   1,
			wantLog:     "ended without holding the task",
			notLog:      "treating it as the phase checkpoint",
			description: "a bare hold is the empty exit CLA-457 refuses to advance on, whatever the channel that produced it",
		},
		{
			name: "settled on the plane (the session finished the job)",
			res:  func() harness.Result { return okResult(1, 0) },
			rel: func() *peekReleaser {
				return &peekReleaser{
					next:  plane.NextTask{TaskID: "t-1"},
					state: map[string]plane.TaskState{"t-1": {Status: "in_review"}},
				}
			},
			wantAsked:   1,
			wantLog:     "ended without holding the task",
			description: "a task already in review has nothing left for phase 2 to do",
		},
		{
			name: "the peek itself fails",
			res:  func() harness.Result { return okResult(1, 0) },
			rel: func() *peekReleaser {
				r := &peekReleaser{
					next:     plane.NextTask{TaskID: "t-1"},
					stateErr: errors.New("plane 503"),
				}
				// Would have said held-with-branch had it answered.
				r.state = map[string]plane.TaskState{"t-1": {Status: "in_progress", ClaimedByRun: "r-curl", Branch: "clanker/x"}}
				return r
			},
			wantAsked: 1,
			wantLog:   "the plane read of t-1 failed",
			notLog:    "treating it as the phase checkpoint",
			// The standard ending follows a failed peek, so both lines appear.
			description: "a plane blip degrades to today's ending, whose lease expiry preserves the takeover hand-off",
		},
		{
			name: "untrusted stream",
			res: func() harness.Result {
				res := okResult(1, 0)
				res.Untrusted = "a line overran the reader"
				return res
			},
			rel: func() *peekReleaser {
				return &peekReleaser{
					next:  plane.NextTask{TaskID: "t-1"},
					state: map[string]plane.TaskState{"t-1": {Status: "in_progress", ClaimedByRun: "r-curl", Branch: "clanker/x"}},
				}
			},
			wantAsked:   0,
			wantLog:     "ended without holding the task",
			notLog:      "treating it as the phase checkpoint",
			description: "CLA-262 stays absolute: a stream that could not be read whole is never filled in from the plane",
		},
		{
			name: "dead shape (finish reason unknown, nothing produced)",
			res:  func() harness.Result { return deadResult() },
			rel: func() *peekReleaser {
				return &peekReleaser{
					next:  plane.NextTask{TaskID: "t-1"},
					state: map[string]plane.TaskState{"t-1": {Status: "in_progress", ClaimedByRun: "r-curl", Branch: "clanker/x"}},
				}
			},
			wantAsked:   0,
			wantLog:     "ended without holding the task",
			notLog:      "treating it as the phase checkpoint",
			description: "CLA-386 stays absolute: a session that died producing nothing is not a checkpoint no matter what the plane says",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			h := &fakeAdapter{steps: []invokeStep{{res: tc.res()}}}
			rel := tc.rel()
			d := blindSpotDriver(t, h, rel)

			_, _, stop, err := drainPhasesOnce(t, d)
			if err != nil || stop {
				t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
			}
			if h.invokeCalls != 1 {
				t.Fatalf("spawned %d sessions, want 1 (%s)", h.invokeCalls, tc.description)
			}
			if len(rel.asked) != tc.wantAsked {
				t.Errorf("asked the plane %d time(s) (%v), want %d", len(rel.asked), rel.asked, tc.wantAsked)
			}
			if out := logs.String(); !strings.Contains(out, tc.wantLog) {
				t.Errorf("log does not carry %q:\n%s", tc.wantLog, out)
			} else if tc.notLog != "" && strings.Contains(out, tc.notLog) {
				t.Errorf("log carries %q, which must stay today's behavior:\n%s", tc.notLog, out)
			}
		})
	}
}

// A capped blind-spot phase checkpoints on the plane record too: the cap path's
// HasWIP requirement exists so phase 2 is only ever handed something durable,
// and a branch the PLANE confirms satisfies that better than one read off a
// stream.
func TestDrainPhases_ACappedBlindSpotPhaseCheckpointsOnThePlaneRecord(t *testing.T) {
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{
		{res: turnCappedResult()}, // capped, clean worktree, NO observed claim
		{res: okResult(5, 0.05)},
	}}
	rel := &peekReleaser{
		next:  plane.NextTask{TaskID: "t-1"},
		state: map[string]plane.TaskState{"t-1": {Status: "in_progress", ClaimedByRun: "r-curl", Branch: "clanker/x"}},
	}
	d := blindSpotDriver(t, h, rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("spawned %d sessions, want 2 — the plane's confirmed branch makes the cap survivable exactly as HasWIP would", h.invokeCalls)
	}
	if got := h.invocations[1].ResumeClaim; got.TaskID != "t-1" || got.RunID != "r-curl" {
		t.Errorf("phase 2 ResumeClaim = %+v, want the plane-recovered t-1/r-curl", got)
	}
	if out := logs.String(); !strings.Contains(out, "treating it as the phase checkpoint") {
		t.Errorf("the log does not say the plane record was taken as the checkpoint:\n%s", out)
	}
}

// The token-ceiling and wall-clock arms of the peek's orderly-end disjunct are
// the same recovery as the capped arm, pinned separately: a ceiling/wall-clock
// kill is an orderly cut-off mid-thought, and with the plane confirming a
// recorded branch (verified on origin) it is exactly as survivable as a cap.
func TestDrainPhases_ACeilingOrWallClockBlindSpotPhaseCheckpointsOnThePlaneRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  func() harness.Result
	}{
		{"token ceiling", tokenCeilingResult},
		{"wall clock", wallClockResult},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			h := &fakeAdapter{steps: []invokeStep{
				{res: tc.res()}, // orderly cut-off, clean worktree, NO observed claim
				{res: okResult(5, 0.05)},
			}}
			rel := &peekReleaser{
				next:  plane.NextTask{TaskID: "t-1"},
				state: map[string]plane.TaskState{"t-1": {Status: "in_progress", ClaimedByRun: "r-curl", Branch: "clanker/x"}},
			}
			d := blindSpotDriver(t, h, rel)

			if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
				t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
			}
			if h.invokeCalls != 2 {
				t.Fatalf("spawned %d sessions, want 2 — a plane-confirmed branch makes a ceiling/wall-clock cut-off survivable, exactly like the cap arm", h.invokeCalls)
			}
			if got := h.invocations[1].ResumeClaim; got.TaskID != "t-1" || got.RunID != "r-curl" {
				t.Errorf("phase 2 ResumeClaim = %+v, want the plane-recovered t-1/r-curl", got)
			}
			if out := logs.String(); !strings.Contains(out, "treating it as the phase checkpoint") {
				t.Errorf("the log does not say the plane record was taken as the checkpoint:\n%s", out)
			}
		})
	}
}

// A plane record alone is not enough: since CLA-457 a checkpoint means a branch
// VERIFIED on the origin remote, and a recovered claim faces the same gate.
// A recorded-but-unverifiable branch keeps today's ending — and because the
// recovered hold IS live with a recorded branch, the sequence end leaves the
// lease to expire with its takeover hand-off intact rather than releasing it.
func TestDrainPhases_ARecoveredClaimStillFacesTheEvidenceGate(t *testing.T) {
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(1, 0)}}}
	rel := &peekReleaser{
		next:  plane.NextTask{TaskID: "t-1"},
		state: map[string]plane.TaskState{"t-1": {Status: "in_progress", ClaimedByRun: "r-curl", Branch: "clanker/phantom"}},
	}
	d := blindSpotDriver(t, h, rel)
	v := &fakeVerifier{report: delivery.Report{Checks: []delivery.Check{{
		Kind: delivery.BranchPushed, Status: delivery.Fail, Detail: "unpushed — local tip ahead of origin",
	}}}}
	d.newVerifier = func(string, bool) deliveryVerifier { return v }

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want 1 — a phantom branch is the false-premise advance CLA-457 closed, whichever channel recorded it", h.invokeCalls)
	}
	out := logs.String()
	if !strings.Contains(out, "could not be verified on the origin remote") {
		t.Errorf("the log does not say why the plane record was refused as evidence:\n%s", out)
	}
	if strings.Contains(out, "treating it as the phase checkpoint") {
		t.Errorf("an unverifiable branch was taken as a checkpoint:\n%s", out)
	}
	if len(rel.calls) != 0 {
		t.Errorf("released the recovered hold %d time(s): %+v — a live lease with a recorded branch expires, keeping the takeover hand-off", len(rel.calls), rel.calls)
	}
}

// Without a briefed task id — no queue-head peek, no predecessor claim — there
// is nothing to ask the plane about, and today's ending stands untouched.
func TestDrainPhases_NoBriefedTaskSkipsThePeek(t *testing.T) {
	logs := captureLogs(t)
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(1, 0)}}}
	// A releaser that implements TaskState but was never told a queue head, so
	// sessionDir's fresh-phase peek returned an empty id.
	rel := &peekReleaser{}
	d := blindSpotDriver(t, h, rel)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want 1", h.invokeCalls)
	}
	if len(rel.asked) != 0 {
		t.Errorf("asked the plane about %v with no briefed task; there is nothing honest to ask", rel.asked)
	}
	if out := logs.String(); !strings.Contains(out, "ended without holding the task") {
		t.Errorf("the standard ending did not fire:\n%s", out)
	}
}
