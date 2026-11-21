package loop

// deadtally.go — the run's dead-phase tally.

import (
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// tallyKey is one cell of the run's dead-phase tally: a phase label and the harness that ran it,
// named the way the config spells both (HarnessFor).
type tallyKey struct {
	phase   string
	harness string
}

// phaseTally counts phase sessions run and dead for one (phase, harness) cell, plus the
// infrastructure misses kept OUT of the dead rate: a review session that never reached the backlog
// because the clankerbar MCP server was not available is not a reviewer that died, and counting it
// as dead is how a harness/infra problem becomes a false review-quality signal.
type phaseTally struct {
	run   int
	dead  int
	noMCP int
}

// tallyDead books one finished phase session into the run's dead-phase tally, and reports whether
// this exit was booked as a DEAD phase. A session counts as a run when it got past its claim —
// claimed a task, or resumed one it was seeded with — because a session that never got past its
// claim (a refused takeover, a launch that observed nothing) is a correct refusal, not a phase run,
// and increments neither counter.
func (d *Driver) tallyDead(phase, harness string, res harness.Result, capped, ceiling, wallclock bool) (bookedDead bool) {
	if res.Claim.TaskID == "" {
		return false
	}
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if d.deadTally == nil {
		d.deadTally = map[tallyKey]*phaseTally{}
	}
	k := tallyKey{phase: phase, harness: harness}
	t := d.deadTally[k]
	if t == nil {
		t = &phaseTally{}
		d.deadTally[k] = t
	}
	t.run++
	// CLA-739: a tool-loop kill is a death in the rate too, so the panel's failed-member path finds it
	// already booked.
	if res.Untrusted == "" && res.Claim.Held() && !capped && !ceiling && !wallclock && (deadPhase(res) || res.ToolLoop != "") {
		t.dead++
		return true
	}
	return false
}

// unbookDead removes a dead booking tallyDead made for an exit that is later classified as an
// infrastructure miss rather than a death. The run count is deliberately NOT decremented: the
// session did run; only its dead classification was wrong.
func (d *Driver) unbookDead(phase, harness string) {
	if d.deadTally == nil {
		return
	}
	t := d.deadTally[tallyKey{phase: phase, harness: harness}]
	if t != nil && t.dead > 0 {
		t.dead--
	}
}

// tallyNoMCP books an infrastructure miss: a review-only session that ended with no successful
// clankerbar MCP call and no review record.
func (d *Driver) tallyNoMCP(phase, harness string) {
	if d.deadTally == nil {
		d.deadTally = map[tallyKey]*phaseTally{}
	}
	k := tallyKey{phase: phase, harness: harness}
	t := d.deadTally[k]
	if t == nil {
		t = &phaseTally{}
		d.deadTally[k] = t
	}
	t.noMCP++
}

// tallyEmpty books one held-but-empty implement exit into the run's dead-phase tally as a dead
// phase.
func (d *Driver) tallyEmpty(phase, harness string) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if d.deadTally == nil {
		d.deadTally = map[tallyKey]*phaseTally{}
	}
	k := tallyKey{phase: phase, harness: harness}
	t := d.deadTally[k]
	if t == nil {
		t = &phaseTally{}
		d.deadTally[k] = t
	}
	t.dead++
}

// panelMemberFailBound is how many consecutive tasks the SAME review-panel member may record
// nothing on before the driver says so loudly. Deliberately a small constant of its own rather than
// the dead-phase ladder: perTaskDeadBound (4) is the budget for a phase whose death strands the
// task and is retried inside one drain, and fleetDeadBound (5) pauses a whole target — neither
// fits a member that is skipped past, on purpose, so the panel and the fix still run.
const panelMemberFailBound = 3

// panelMemberFailed counts one task on which the member recorded nothing and returns the member's
// streak of consecutive such tasks. Keyed by the member's log label (the positional `review-N` when
// unnamed), which is stable across tasks because the panel is positional; the Driver holds it so
// the streak spans drains, and each drain runs the panel at most once per task.
func (d *Driver) panelMemberFailed(member string) int {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if d.panelFailStreak == nil {
		d.panelFailStreak = map[string]int{}
	}
	d.panelFailStreak[member]++
	return d.panelFailStreak[member]
}

// panelMemberRecorded clears a member's failure streak: the member recorded this task, so its next
// failure is not consecutive with the last.
func (d *Driver) panelMemberRecorded(member string) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	delete(d.panelFailStreak, member)
}

// logDeadTally prints the run's dead-phase tally so far, one line per (phase, harness) cell, each
// with its denominator. A line is printed after every drain, so the operator watching the daemon
// log sees the rate grow — and the last line before the run ends IS the run total.
func (d *Driver) logDeadTally() {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if len(d.deadTally) == 0 {
		return
	}
	keys := make([]tallyKey, 0, len(d.deadTally))
	for k := range d.deadTally {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b tallyKey) int {
		if a.phase != b.phase {
			return strings.Compare(a.phase, b.phase)
		}
		return strings.Compare(a.harness, b.harness)
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		t := d.deadTally[k]
		rate := 0.0
		if t.run > 0 {
			rate = 100 * float64(t.dead) / float64(t.run)
		}
		line := fmt.Sprintf("%s/%s: %d dead of %d (%.1f%%)", k.phase, k.harness, t.dead, t.run, rate)
		if t.noMCP > 0 {
			line += fmt.Sprintf(", %d no-mcp", t.noMCP)
		}
		parts = append(parts, line)
	}
	log.Printf("dead-phase tally: %s", strings.Join(parts, "; "))
}
