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

// phaseTally counts phase sessions run and dead for one (phase, harness) cell.
type phaseTally struct {
	run  int
	dead int
}

// tallyDead books one finished phase session into the run's dead-phase tally.
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
func (d *Driver) tallyDead(phase, harness string, res harness.Result, capped, ceiling, wallclock bool) {
	if res.Claim.TaskID == "" {
		return
	}
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
	}
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

// logDeadTally prints the run's dead-phase tally so far, one line per
// (phase, harness) cell, each with its denominator. A line is printed after
// every drain, so the operator watching the daemon log sees the rate grow —
// and the last line before the run ends IS the run total.
func (d *Driver) logDeadTally() {
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
		parts = append(parts, fmt.Sprintf("%s/%s: %d dead of %d (%.1f%%)", k.phase, k.harness, t.dead, t.run, rate))
	}
	log.Printf("dead-phase tally: %s", strings.Join(parts, "; "))
}
