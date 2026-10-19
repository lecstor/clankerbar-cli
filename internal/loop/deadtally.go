package loop

// deadtally.go — the run's dead-phase tally (CLA-402).
//
// The dead-phase rate is the number that decides whether a fix to the opencode
// gateway worked, and until CLA-402 the only way to get it was a hand scan of
// the iteration logs (CLA-396 counted 6 dead of 16 implement sessions on
// 2026-08-20 that way, in the middle of an investigation). This file makes the
// driver count it live: every phase session that got past its claim is a phase
// run, and one that then died producing nothing is a phase dead, broken down
// by phase name and harness, and reported with its denominator where the
// operator watches — the daemon log.
//
// The predicate is the driver's own deadPhase classification, plus the
// operator's exclusion (2026-08-20 decision f518a454): a session that never
// got past its claim — a refused takeover — increments NEITHER counter, because
// such a session satisfies both conjuncts of deadPhase() while being a correct
// refusal, not a death. The one deliberate difference from the `dead` variable
// in drainPhase is the absence of the `!last` conjunct: that conjunct exists to
// veto a checkpoint the phase owes, which a LAST phase owes nobody — but a dead
// last phase is still a dead phase, and the rate is a measurement, not a seam
// decision. Counting it keeps the live tally and the retrospective scan
// (internal/deadscan) agreeing on what a dead session is.

import (
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// tallyKey is one cell of the run's dead-phase tally: a phase label and the
// harness that ran it, named the way the config spells both (HarnessFor).
type tallyKey struct {
	phase   string
	harness string
}

// phaseTally counts phase sessions run and dead for one (phase, harness) cell,
// plus the infrastructure misses kept OUT of the dead rate (CLA-703): a review
// session that never reached the backlog because the clankerbar MCP server was
// not available is not a reviewer that died, and counting it as dead is how a
// harness/infra problem becomes a false review-quality signal.
type phaseTally struct {
	run   int
	dead  int
	noMCP int
}

// tallyDead books one finished phase session into the run's dead-phase tally,
// and reports whether this exit was booked as a DEAD phase. The report is what
// lets a caller that books failures of its own (the review panel's member
// handler, CLA-639) avoid counting the same exit twice.
//
// It is called once per session (per Invoke), with the same classification
// inputs drainPhase computed for the seam. A session counts as a run when it
// got past its claim — claimed a task, or resumed one it was seeded with —
// because a session that never got past its claim (a refused takeover, a
// launch that observed nothing) is a correct refusal, not a phase run, and
// increments neither counter. Of the runs, one is dead when the driver's own
// dead-phase classification fires: the final step finished with reason
// "unknown" (opencode's marker for a session that died without a final
// answer), no branch was recorded, and no cap or stream failure explains the
// end — the "produced nothing" shape CLA-386 and CLA-398 were built around.
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
	if res.Untrusted == "" && res.Claim.Held() && !capped && !ceiling && !wallclock && deadPhase(res) {
		t.dead++
		return true
	}
	return false
}

// unbookDead removes a dead booking tallyDead made for an exit that is later
// classified as an infrastructure miss (CLA-703) rather than a death. It is the
// counterpart of the booking and exists because the booking has to happen when
// the exit arrives, before the review seam has decided whether the session ever
// reached the backlog. The run count is deliberately NOT decremented: the
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

// tallyNoMCP books an infrastructure miss: a review-only session that ended with
// no successful clankerbar MCP call and no review record. It is counted
// SEPARATELY from dead so the operator can see that the member's harness or the
// MCP wiring was at fault, not the reviewer.
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

// tallyEmpty books one held-but-empty implement exit into the run's dead-phase
// tally as a dead phase (CLA-457). It is called ONLY at the seam, where the
// driver has just classified the exit as empty — the 2026-08-24 fleet-incident
// shape: a clean stop still holding the task with no branch recorded
// anywhere the plane can fetch. That is the same "produced nothing" class as
// the CLA-386 quiet death, so it feeds the same rate an operator watches; it is
// booked SCENED here rather than folded into the generic deadPhase predicate
// because the classification lives at the seam (implement, non-last, would-be
// checkpoint), and only the seam has that context. The run was already counted
// by tallyDead; this adds its dead.
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

// panelMemberFailBound is how many consecutive tasks the SAME review-panel
// member (CLA-639 M4) may record nothing on before the driver says so loudly.
//
// Deliberately a small constant of its own rather than the dead-phase ladder:
// perTaskDeadBound (4) is the budget for a phase whose death strands the task
// and is retried inside one drain, and fleetDeadBound (5) pauses a whole
// target — neither fits a member that is skipped past, on purpose, so the panel
// and the fix still run. Three consecutive tasks is where a member's failure
// stops looking like a blip: the member has a fixed harness and model, its
// prompt is the same every time, and a member that records nothing three times
// running is a member whose harness, model or brief is not producing reviews.
// The warning names the member, its harness and its model; the streak resets
// the moment that member records, and nothing is parked on it — parking a task
// because ONE reviewer of four wobbled would strand the panel's real findings.
const panelMemberFailBound = 3

// panelMemberFailed counts one task on which the member recorded nothing and
// returns the member's streak of consecutive such tasks. Keyed by the member's
// log label (the positional `review-N` when unnamed), which is stable across
// tasks because the panel is positional; the Driver holds it so the streak
// spans drains, and each drain runs the panel at most once per task.
func (d *Driver) panelMemberFailed(member string) int {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if d.panelFailStreak == nil {
		d.panelFailStreak = map[string]int{}
	}
	d.panelFailStreak[member]++
	return d.panelFailStreak[member]
}

// panelMemberRecorded clears a member's failure streak: the member recorded
// this task, so its next failure is not consecutive with the last.
func (d *Driver) panelMemberRecorded(member string) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	delete(d.panelFailStreak, member)
}

// logDeadTally prints the run's dead-phase tally so far, one line per
// (phase, harness) cell, each with its denominator. A line is printed after
// every drain, so the operator watching the daemon log sees the rate grow —
// and the last line before the run ends IS the run total.
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
