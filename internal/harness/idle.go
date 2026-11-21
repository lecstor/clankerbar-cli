package harness

import (
	"fmt"
	"sync"
	"time"
)

// idle.go - the no-output watchdog for a harness session.

// idleGuard fires a kill when a session has been silent too long. seen, toolStarted, toolFinished,
// stop and tripped are all safe on a nil guard and on a disabled one, so a parser can hold one
// unconditionally.
type idleGuard struct {
	idle    time.Duration
	toolCap time.Duration
	kill    func(cause string)

	mu      sync.Mutex
	timer   *time.Timer
	armed   int64 // generation of the armed timer; stale callbacks are ignored
	stopped bool
	fired   bool
	cause   string
	// openAtFire records whether a call was open when the timer fired. A kill landed on an open call
	// is the TOOL CAP ending a session that was waiting, not the idle clock finding a silent one, and
	// callers classify the two differently (a cap kill is not evidence the provider broke).
	openAtFire bool

	// open is the tool calls started and not yet finished, oldest first. Empty means the idle clock is
	// running; non-empty means it is paused.
	open []idleCall
}

type idleCall struct {
	id    string
	start time.Time
}

// newIdleGuard returns a guard that kills after idle without output, or after toolCap on the oldest
// open tool call. A non-positive idle, or a nil kill, disables it.
func newIdleGuard(idle, toolCap time.Duration, kill func(cause string)) *idleGuard {
	g := &idleGuard{idle: idle, toolCap: toolCap, kill: kill}
	if !g.enabled() {
		return g
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armLocked(g.idle, fmt.Sprintf("no output for %s", g.idle))
	return g
}

func (g *idleGuard) enabled() bool {
	return g != nil && g.idle > 0 && g.kill != nil
}

// seen records one line of stream output.
func (g *idleGuard) seen() {
	if !g.enabled() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped || g.fired || len(g.open) > 0 {
		return
	}
	g.armLocked(g.idle, fmt.Sprintf("no output for %s", g.idle))
}

// toolStarted records that a tool call with this id is in flight. The FIRST open call pauses the
// idle clock and starts the tool cap; a later one leaves the oldest call's deadline in place,
// because that is the call the cap names.
func (g *idleGuard) toolStarted(id string) {
	if !g.enabled() || id == "" {
		return
	}
	g.startCall(id, false)
}

// toolHandoff opens a SYNTHETIC call for a harness that cannot see a real call in flight:
// opencode's step_start, which begins the step whose tools opencode forwards only after they
// settle. A repeated handoff for the same id RESTARTS the deadline: a step_start always begins a
// new step, including one whose predecessor ended without a step_finish (a provider retry), and an
// old start must not cap the step now running.
func (g *idleGuard) toolHandoff(id string) {
	if !g.enabled() || g.toolCap <= 0 {
		return
	}
	g.startCall(id, true)
}

// startCall opens id, or with restart moves an already-open id's deadline to now. Only the OLDEST
// open call owns the cap: a later call's silence is covered by the one already in flight, and
// retargeting on every call would let a stream of short calls hide the stuck one.
func (g *idleGuard) startCall(id string, restart bool) {
	if !g.enabled() || id == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped || g.fired {
		return
	}
	now := time.Now()
	for i := range g.open {
		if g.open[i].id != id {
			continue
		}
		if !restart {
			return
		}
		g.open[i].start = now
		if i == 0 && g.toolCap > 0 {
			g.armLocked(g.toolCap, fmt.Sprintf("tool call %s open for %s", id, g.toolCap))
		}
		return
	}
	g.open = append(g.open, idleCall{id: id, start: now})
	if len(g.open) != 1 {
		return
	}
	if g.toolCap > 0 {
		g.armLocked(g.toolCap, fmt.Sprintf("tool call %s open for %s", id, g.toolCap))
		return
	}
	g.disarmLocked()
}

// toolFinished records that a tool call has settled. When it was the oldest open call and others
// remain, the cap is re-armed for the new oldest; when none remain, the idle clock restarts from
// this output.
func (g *idleGuard) toolFinished(id string) {
	if !g.enabled() || id == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped || g.fired {
		return
	}
	idx := -1
	for i, c := range g.open {
		if c.id == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	g.open = append(g.open[:idx], g.open[idx+1:]...)
	if len(g.open) == 0 {
		g.armLocked(g.idle, fmt.Sprintf("no output for %s", g.idle))
		return
	}
	if idx != 0 || g.toolCap <= 0 {
		return
	}
	oldest := g.open[0]
	remaining := time.Until(oldest.start.Add(g.toolCap))
	if remaining < 0 {
		remaining = 0
	}
	g.armLocked(remaining, fmt.Sprintf("tool call %s open for %s", oldest.id, g.toolCap))
}

// stop disarms the guard for good: a finished session has nothing left to kill.
func (g *idleGuard) stop() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = true
	g.disarmLocked()
}

// tripped is the verdict once the guard has fired, "" before.
func (g *idleGuard) tripped() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cause
}

// trippedWithOpenCall reports that the kill landed while a tool call was open: the TOOL CAP fired,
// not the idle clock. False before the guard fires, on a disabled guard, and for an idle-clock
// kill.
func (g *idleGuard) trippedWithOpenCall() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fired && g.openAtFire
}

// armLocked replaces the armed deadline. The generation makes a callback that was already in flight
// when the deadline moved a no-op.
func (g *idleGuard) armLocked(d time.Duration, cause string) {
	g.disarmLocked()
	gen := g.armed
	g.timer = time.AfterFunc(d, func() { g.fire(gen, cause) })
}

func (g *idleGuard) disarmLocked() {
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
	g.armed++
}

func (g *idleGuard) fire(gen int64, cause string) {
	g.mu.Lock()
	if g.stopped || g.fired || gen != g.armed {
		g.mu.Unlock()
		return
	}
	g.fired, g.cause, g.timer = true, cause, nil
	g.openAtFire = len(g.open) > 0
	g.mu.Unlock()
	g.kill(cause)
}
