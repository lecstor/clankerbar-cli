package loop

// CLA-562: bound consecutive harness-startup failures  -  raise an operator
// question instead of sidelining forever. Mirrors the dead-phase escalation
// (CLA-396): after N consecutive startup failures (default 5) the target pauses
// until answered; answering resumes and the next startup failure re-arms.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

func startupErr() error {
	return errors.New("invoke opencode: exit 1: bad reasoningEffort value")
}

// startupTwoTargets builds a two-target driver  -  alpha broken (with a parking
// releaser so the trip can file), beta healthy  -  so one target's sideline never
// ends the run (allSidelined needs EVERY target benched). Rotation starts one
// past the cursor, so the first drain is beta and the drains then alternate
// beta, alpha, beta, alpha...; steps must be scripted in that order.
func startupTwoTargets(t *testing.T, h *fakeAdapter, alphaPoller, betaPoller *fakePoller, maxIter int) (*Driver, *parkingReleaser) {
	t.Helper()
	rel := &parkingReleaser{}
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = maxIter
	d := NewMulti(cfg, h, []Target{
		{Name: "alpha", Poller: alphaPoller, Releaser: rel, WorkDir: "/repos/alpha"},
		{Name: "beta", Poller: betaPoller, WorkDir: "/repos/beta"},
	})
	openTestStateDir(t, d)
	// Force-elapse the sideline backoff before every selection, so consecutive
	// failures drain back-to-back without sleeping real 15m/30m time.
	clear := func(int) {
		for i := range d.skipUntil {
			if d.skipUntil[i].After(time.Now()) {
				d.skipUntil[i] = time.Now().Add(-time.Millisecond)
			}
		}
	}
	alphaPoller.onCall = clear
	betaPoller.onCall = clear
	return d, rel
}

// The bound is FIVE: four consecutive startup failures sideline only (backoff,
// no question, no pause), and the fifth escalates to a pause-and-raise.
func TestStartupBound_FourSidelineFifthPausesAndRaises(t *testing.T) {
	mkSteps := func(alphaFails int) []invokeStep {
		// Drains alternate beta-ok, alpha-err starting with beta.
		steps := []invokeStep{}
		for i := 0; i < alphaFails; i++ {
			steps = append(steps, invokeStep{res: okResult(0, 0)}) // beta
			steps = append(steps, invokeStep{err: startupErr()})  // alpha
		}
		return steps
	}
	t.Run("four failures sideline without pausing", func(t *testing.T) {
		h := &fakeAdapter{steps: mkSteps(4)}
		alpha := &fakePoller{sum: backlog.Summary{Claimable: 1}}
		beta := &fakePoller{sum: backlog.Summary{Claimable: 1}}
		d, rel := startupTwoTargets(t, h, alpha, beta, 8)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.Run(ctx); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if got := d.startupFails[0]; got != 4 {
			t.Errorf("startupFails[alpha] = %d, want 4", got)
		}
		if d.startupPaused[0] {
			t.Error("startup pause set before the bound  -  the first four must sideline only")
		}
		if len(rel.questions) != 0 {
			t.Errorf("filed %d questions before the bound, want 0: %+v", len(rel.questions), rel.questions)
		}
		if d.harnessFails[0] != 4 {
			t.Errorf("harnessFails[alpha] = %d, want 4  -  backoff still climbs under the bound", d.harnessFails[0])
		}
	})

	t.Run("fifth failure pauses and raises once", func(t *testing.T) {
		h := &fakeAdapter{steps: mkSteps(5)}
		alpha := &fakePoller{sum: backlog.Summary{Claimable: 1}}
		beta := &fakePoller{sum: backlog.Summary{Claimable: 1}}
		d, rel := startupTwoTargets(t, h, alpha, beta, 10)
		logs := captureLogs(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.Run(ctx); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if got := d.startupFails[0]; got != 5 {
			t.Errorf("startupFails[alpha] = %d, want 5", got)
		}
		if !d.startupPaused[0] {
			t.Error("startup pause not set on the fifth consecutive failure")
		}
		if len(rel.questions) != 1 {
			t.Fatalf("filed %d questions, want 1: %+v", len(rel.questions), rel.questions)
		}
		q := rel.questions[0]
		if q.taskID != "" {
			t.Errorf("startup question taskId = %q, want empty  -  project-level, not pinned to a bystander drain", q.taskID)
		}
		if q.kind != "decision" {
			t.Errorf("startup question kind = %q, want decision", q.kind)
		}
		if !strings.Contains(q.body, "consecutive harness-startup failures") {
			t.Errorf("startup question body does not say what tripped it: %q", q.body)
		}
		if !strings.Contains(q.body, "bad reasoningEffort") {
			t.Errorf("startup question body lost the harness exit signature: %q", q.body)
		}
		if !strings.Contains(q.body, "/repos/alpha") {
			t.Errorf("startup question body lost the workdir: %q", q.body)
		}
		if strings.Contains(q.body, "in a row") {
			t.Errorf("startup question body echoes the sideline log's \"harness failure N in a row\" phrasing with the startup count, which disagrees with the sideline count once a with-usage error interleaves (it resets the startup run but not the sideline ladder): %q", q.body)
		}
		if out := logs.String(); !strings.Contains(out, "consecutive harness-startup failures") {
			t.Errorf("trip log line missing:\n%s", out)
		}
		// Beta never failed: its counters stay clean and it kept draining.
		if got := d.startupFails[1]; got != 0 {
			t.Errorf("startupFails[beta] = %d, want 0  -  one project's failures must not pause another", got)
		}
		if d.startupPaused[1] {
			t.Error("beta paused by alpha's failures")
		}
	})
}

// A sixth consecutive failure while the pause is in force raises nothing more
//  -  exactly one question per episode  -  and the paused target stops draining.
func TestStartupTrip_RaisesOncePerEpisode(t *testing.T) {
	// 5 alpha fails to trip (10 drains) + one more beta drain while paused = 11.
	steps := []invokeStep{}
	for i := 0; i < 5; i++ {
		steps = append(steps, invokeStep{res: okResult(0, 0)})
		steps = append(steps, invokeStep{err: startupErr()})
	}
	steps = append(steps, invokeStep{res: okResult(0, 0)}) // beta while alpha paused
	h := &fakeAdapter{steps: steps}
	alpha := &fakePoller{sum: backlog.Summary{Claimable: 1}}
	beta := &fakePoller{sum: backlog.Summary{Claimable: 1}}
	d, rel := startupTwoTargets(t, h, alpha, beta, 11)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rel.questions) != 1 {
		t.Errorf("filed %d questions across a paused episode, want 1: %+v", len(rel.questions), rel.questions)
	}
	// Drains: 5 beta + 5 alpha + 1 beta-while-paused = 11 invokes; alpha drained
	// exactly five times (its fifth tripped, the sixth poll stayed paused).
	alphaDrains := 0
	for _, inv := range h.invocations {
		if inv.WorkDir == "/repos/alpha" {
			alphaDrains++
		}
	}
	if alphaDrains != 5 {
		t.Errorf("alpha drained %d times, want 5  -  the paused target must not drain again", alphaDrains)
	}
}

// Answering resumes, and the next startup failure re-arms: it pauses again and
// raises a fresh question. Same shape as the dead-phase re-arm.
func TestStartupReArm_AnswerResumesNextFailureRaisesFresh(t *testing.T) {
	// Drains: beta,alpha x5 (trip 1 on drain 10), beta while paused (11), alpha
	// after answer (12, trip 2). 12 drains, 6 beta oks + 6 alpha errs.
	steps := []invokeStep{}
	for i := 0; i < 6; i++ {
		steps = append(steps, invokeStep{res: okResult(0, 0)})
		steps = append(steps, invokeStep{err: startupErr()})
	}
	h := &fakeAdapter{steps: steps}
	spawnable := func(openQ int) backlog.Summary {
		return backlog.Summary{Claimable: 1, OpenQuestions: openQ}
	}
	alphaSums := []backlog.Summary{}
	for i := 0; i < 10; i++ {
		alphaSums = append(alphaSums, spawnable(0))
	}
	alphaSums = append(alphaSums, spawnable(1)) // poll 10: question open  -  stays paused
	alphaSums = append(alphaSums, spawnable(0)) // poll 11: answered  -  resume
	alpha := &fakePoller{sums: alphaSums, sum: backlog.Summary{Claimable: 0}}
	beta := &fakePoller{sum: backlog.Summary{Claimable: 1}}
	d, rel := startupTwoTargets(t, h, alpha, beta, 12)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rel.questions) != 2 {
		t.Fatalf("filed %d questions, want 2  -  one per episode: %+v", len(rel.questions), rel.questions)
	}
}

// Startup failures and dead phases are different counters: one failure must not
// tick both.
func TestStartupAndDeadCountersAreDistinct(t *testing.T) {
	t.Run("startup failures do not tick the fleet dead counter", func(t *testing.T) {
		steps := []invokeStep{}
		for i := 0; i < 3; i++ {
			steps = append(steps, invokeStep{res: okResult(0, 0)})
			steps = append(steps, invokeStep{err: startupErr()})
		}
		h := &fakeAdapter{steps: steps}
		alpha := &fakePoller{sum: backlog.Summary{Claimable: 1}}
		beta := &fakePoller{sum: backlog.Summary{Claimable: 1}}
		d, _ := startupTwoTargets(t, h, alpha, beta, 6)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.Run(ctx); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if got := d.fleetDead[0]; got != 0 {
			t.Errorf("fleetDead[alpha] = %d, want 0  -  a startup failure (no session ran) must not tick the dead-phase counter", got)
		}
		if d.fleetPaused[0] {
			t.Error("fleet pause set by startup failures  -  wrong counter tripped")
		}
	})

	t.Run("dead phases do not tick the startup counter", func(t *testing.T) {
		h := &fakeAdapter{steps: []invokeStep{
			{res: held(deadResult(), openClaim())},
			{res: held(deadResult(), openClaim())},
		}}
		rel := &parkingReleaser{}
		cfg := fastCfg()
		cfg.Phases = twoPhases()
		cfg.Prompt = ""
		cfg.StateDir = t.TempDir()
		d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
		openTestStateDir(t, d)
		d.cfg.WorkDir = t.TempDir()
		d.newVerifier = func(string, bool) deliveryVerifier { return passVerifier() }
		// One drain with two dead phases (first retried). err == nil, so the
		// startup counter must stay at zero.
		if _, _, _, _, err := drainPhasesHandoffs(t, d, 1); err != nil {
			t.Fatalf("drainPhases: %v", err)
		}
		if got := d.startupFails[0]; got != 0 {
			t.Errorf("startupFails = %d, want 0  -  a dead phase (session ran) must not tick the startup counter", got)
		}
		if d.startupPaused[0] {
			t.Error("startup pause set by dead phases  -  wrong counter tripped")
		}
	})

	t.Run("an error with usage is not a startup failure", func(t *testing.T) {
		// A session that ran, reported usage, then failed non-retryably still
		// sidelines (harnessFails++) but must not climb the startup bound.
		h := &fakeAdapter{steps: []invokeStep{
			{res: okResult(0, 0)}, // beta
			{res: harness.Result{ExitCode: 2, Tokens: 100, CostUSD: 0.01, Raw: map[string]any{"kind": "fail"}}},
		}}
		alpha := &fakePoller{sum: backlog.Summary{Claimable: 1}}
		beta := &fakePoller{sum: backlog.Summary{Claimable: 1}}
		d, rel := startupTwoTargets(t, h, alpha, beta, 2)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.Run(ctx)
		if got := d.harnessFails[0]; got != 1 {
			t.Errorf("harnessFails[alpha] = %d, want 1  -  a post-usage failure still sidelines", got)
		}
		if got := d.startupFails[0]; got != 0 {
			t.Errorf("startupFails[alpha] = %d, want 0  -  usage proves a session ran, so this is not a startup failure", got)
		}
		if len(rel.questions) != 0 {
			t.Errorf("filed %d questions for a post-usage failure, want 0", len(rel.questions))
		}
	})
}

// A drain that succeeds resets startup consecutiveness  -  the harness starts
// again, so the next failure starts a fresh run.
func TestStartupSuccessResetsTheCounter(t *testing.T) {
	// Alpha: fail, fail, success, fail = never reaches five in a row. With beta
	// interleaved the drain order is beta,alpha,beta,alpha,beta,alpha(ok),...
	// Script explicitly: beta ok, alpha err, beta ok, alpha err, beta ok,
	// alpha ok (success resets), beta ok, alpha err (fresh run of one).
	h := &fakeAdapter{steps: []invokeStep{
		{res: okResult(0, 0)}, // drain 1 beta
		{err: startupErr()},  // drain 2 alpha fail 1
		{res: okResult(0, 0)}, // drain 3 beta
		{err: startupErr()},  // drain 4 alpha fail 2
		{res: okResult(0, 0)}, // drain 5 beta
		{res: okResult(0, 0)}, // drain 6 alpha SUCCESS
		{res: okResult(0, 0)}, // drain 7 beta
		{err: startupErr()},  // drain 8 alpha fail 1 (fresh)
	}}
	alpha := &fakePoller{sum: backlog.Summary{Claimable: 1}}
	beta := &fakePoller{sum: backlog.Summary{Claimable: 1}}
	d, rel := startupTwoTargets(t, h, alpha, beta, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := d.startupFails[0]; got != 1 {
		t.Errorf("startupFails[alpha] = %d, want 1  -  the success between failures must reset the run", got)
	}
	if len(rel.questions) != 0 {
		t.Errorf("filed %d questions, want 0  -  no run of five was ever reached", len(rel.questions))
	}
}

// A startup-paused target reads draining on presence, not idle: it is alive
// and spawning nothing new until the operator answers, exactly like a
// fleet-paused one. Before the fix it read idle, hiding the wait it just asked
// for from the console.
func TestStartupPaused_PresenceReadsDraining(t *testing.T) {
	d := NewMulti(fastCfg(), &fakeAdapter{}, []Target{{Poller: busyPoller()}})
	if got := d.fleetState(0); got.Kind != fleet.StateIdle {
		t.Fatalf("fresh target state = %v, want idle", got)
	}
	d.startupPaused[0] = true
	if got := d.fleetState(0); got.Kind != fleet.StateDraining {
		t.Errorf("startup-paused state = %v, want draining", got)
	}
	d.startupPaused[0] = false
	if got := d.fleetState(0); got.Kind != fleet.StateIdle {
		t.Errorf("resumed target state = %v, want idle", got)
	}
	// Bounds-checked like its siblings: a hand-built Driver degrades to idle.
	empty := &Driver{}
	if got := empty.fleetState(0); got.Kind != fleet.StateIdle {
		t.Errorf("empty-driver state = %v, want idle", got)
	}
}
