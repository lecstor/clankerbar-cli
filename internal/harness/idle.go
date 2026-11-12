package harness

import (
	"fmt"
	"sync"
	"time"
)

// idle.go - the no-output watchdog for a harness session.
//
// A session that stops producing stream output is indistinguishable from one
// that is thinking, except by the clock. A silent child is a hang - a wedged
// provider connection, a tool waiting on something that will never answer - and
// nothing else in the driver bounds it: the wall clock is off by default and the
// token ceiling only sees a session that is still SPENDING.
//
// While no tool call is open, the guard kills after `idle` without output. While
// one or more calls are open the idle clock is paused, because a long build or a
// slow MCP call is legitimate work with no output, and instead the oldest open
// call is bounded by `toolCap`.
//
// Zero idle disables everything, including the tool cap: with no watchdog there
// is nothing to pause or bound.

// idleGuard fires a kill when a session has been silent too long. seen,
// toolStarted, toolFinished, stop and tripped are all safe on a nil guard and on
// a disabled one, so a parser can hold one unconditionally.
//
// Goroutine-safe: the stream parser feeds it from the read loop while the timers
// fire from their own goroutines.
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

	// open is the tool calls started and not yet finished, oldest first. Empty
	// means the idle clock is running; non-empty means it is paused.
	open []idleCall
}

type idleCall struct {
	id    string
	start time.Time
}

// newIdleGuard returns a guard that kills after idle without output, or after
// toolCap on the oldest open tool call. A non-positive idle, or a nil kill,
// disables it. A non-positive toolCap leaves an open call unbounded.
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

// seen records one line of stream output. It restarts the idle clock unless a
// tool call is open, which pauses it.
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

// toolStarted records that a tool call with this id is in flight. The FIRST
// open call pauses the idle clock and starts the tool cap; a later one leaves the
// oldest call's deadline in place, because that is the call the cap names.
func (g *idleGuard) toolStarted(id string) {
	if !g.enabled() || id == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped || g.fired {
		return
	}
	for _, c := range g.open {
		if c.id == id {
			return
		}
	}
	g.open = append(g.open, idleCall{id: id, start: time.Now()})
	if len(g.open) != 1 {
		return
	}
	if g.toolCap > 0 {
		g.armLocked(g.toolCap, fmt.Sprintf("tool call %s open for %s", id, g.toolCap))
		return
	}
	g.disarmLocked()
}

// toolFinished records that a tool call has settled. When it was the oldest open
// call and others remain, the cap is re-armed for the new oldest; when none
// remain, the idle clock restarts from this output.
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

// armLocked replaces the armed deadline. The generation makes a callback that
// was already in flight when the deadline moved a no-op.
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
	g.mu.Unlock()
	g.kill(cause)
}
