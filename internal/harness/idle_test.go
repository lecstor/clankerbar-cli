package harness

import (
	"sync"
	"testing"
	"time"
)

// idleRecorder collects the guard's kills; the callback runs on the timer's
// goroutine, so the slice is behind a lock.
type idleRecorder struct {
	mu    sync.Mutex
	kills []string
}

func (r *idleRecorder) kill(cause string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kills = append(r.kills, cause)
}

func (r *idleRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.kills...)
}

// waitIdleCause polls for the guard's verdict, so the tests never depend on a
// timer landing inside a fixed sleep.
func waitIdleCause(t *testing.T, g *idleGuard, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if c := g.tripped(); c != "" {
			return c
		}
		time.Sleep(5 * time.Millisecond)
	}
	return g.tripped()
}

// waitIdleKills polls for the recorder to show at least want kills: fire()
// publishes its cause just before it calls kill, so a test that only waited on
// the cause could observe the verdict a moment early.
func waitIdleKills(t *testing.T, r *idleRecorder, want int, within time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if got := r.all(); len(got) >= want {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
	return r.all()
}

// A session that emits nothing at all is killed, once, with the idle cause.
func TestIdleGuardKillsASilentSession(t *testing.T) {
	rec := &idleRecorder{}
	g := newIdleGuard(50*time.Millisecond, time.Second, rec.kill)
	defer g.stop()

	if got := waitIdleCause(t, g, 2*time.Second); got != "no output for 50ms" {
		t.Errorf("cause = %q, want %q", got, "no output for 50ms")
	}
	if kills := waitIdleKills(t, rec, 1, 2*time.Second); len(kills) != 1 || kills[0] != "no output for 50ms" {
		t.Errorf("kills = %q, want exactly one kill with the idle cause", kills)
	}
}

// Output restarts the idle clock: a session that keeps emitting is not a hang.
func TestIdleGuardOutputRestartsTheClock(t *testing.T) {
	rec := &idleRecorder{}
	g := newIdleGuard(500*time.Millisecond, 0, rec.kill)
	defer g.stop()

	for i := 0; i < 5; i++ {
		g.seen()
		time.Sleep(20 * time.Millisecond)
	}
	if got := g.tripped(); got != "" {
		t.Errorf("cause = %q after steady output, want none", got)
	}
	if kills := rec.all(); len(kills) != 0 {
		t.Errorf("kills = %q after steady output, want none", kills)
	}
}

// A long open tool call pauses the idle clock, so the TOOL CAP is what fires -
// never the (much shorter) idle bound.
func TestIdleGuardPausesForAnOpenToolCallAndAppliesTheToolCap(t *testing.T) {
	rec := &idleRecorder{}
	g := newIdleGuard(50*time.Millisecond, 150*time.Millisecond, rec.kill)
	defer g.stop()

	g.toolStarted("call_1")
	if got := waitIdleCause(t, g, 2*time.Second); got != "tool call call_1 open for 150ms" {
		t.Errorf("cause = %q, want the tool-cap cause", got)
	}
}

// When every call has finished, the idle clock starts again.
func TestIdleGuardResumesTheIdleClockWhenTheLastToolFinishes(t *testing.T) {
	rec := &idleRecorder{}
	g := newIdleGuard(150*time.Millisecond, time.Second, rec.kill)
	defer g.stop()

	g.toolStarted("call_1")
	time.Sleep(60 * time.Millisecond)
	g.toolFinished("call_1")
	if got := g.tripped(); got != "" {
		t.Fatalf("cause = %q immediately after the call finished, want none", got)
	}
	if got := waitIdleCause(t, g, 2*time.Second); got != "no output for 150ms" {
		t.Errorf("cause = %q, want the idle cause once the clock resumed", got)
	}
}

// A zero idle disables the whole guard, tool cap included.
func TestIdleGuardZeroDisablesEverything(t *testing.T) {
	rec := &idleRecorder{}
	g := newIdleGuard(0, 20*time.Millisecond, rec.kill)
	defer g.stop()

	g.toolStarted("call_1")
	time.Sleep(80 * time.Millisecond)
	if got := g.tripped(); got != "" {
		t.Errorf("cause = %q with the guard disabled, want none", got)
	}
	if kills := rec.all(); len(kills) != 0 {
		t.Errorf("kills = %q with the guard disabled, want none", kills)
	}
}

// stop disarms the timer: nothing fires after a session has ended.
func TestIdleGuardStopDisarmsTheKill(t *testing.T) {
	rec := &idleRecorder{}
	g := newIdleGuard(30*time.Millisecond, time.Second, rec.kill)
	g.stop()
	time.Sleep(90 * time.Millisecond)
	if kills := rec.all(); len(kills) != 0 {
		t.Errorf("kills = %q after stop, want none", kills)
	}
	if got := g.tripped(); got != "" {
		t.Errorf("cause = %q after stop, want none", got)
	}
}

// When the OLDEST open call finishes and others remain, the cap moves to the new
// oldest call rather than firing early for the one that is gone.
func TestIdleGuardRetargetsTheCapToTheNextOldestCall(t *testing.T) {
	rec := &idleRecorder{}
	g := newIdleGuard(50*time.Millisecond, 200*time.Millisecond, rec.kill)
	defer g.stop()

	g.toolStarted("first")
	time.Sleep(40 * time.Millisecond)
	g.toolStarted("second")
	g.toolFinished("first")
	// "second" started 40ms in, so its own cap expires around 240ms.
	time.Sleep(60 * time.Millisecond)
	if got := g.tripped(); got != "" {
		t.Fatalf("cause = %q, want none yet: the cap belongs to the new oldest call", got)
	}
	if got := waitIdleCause(t, g, 2*time.Second); got != "tool call second open for 200ms" {
		t.Errorf("cause = %q, want the cap naming the call still open", got)
	}
}
