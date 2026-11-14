package loop

// Budget enforcement (CLA-410, CLA-480). Two accounts are judged, and the
// distinction is the whole point of the stored run-config overlay:
//
//   - a target with a stored document in force is judged by THAT document's
//     budget over its own spend, on its own clock. The local file's dials are
//     not a second, tighter bound for it — a ratified looser ceiling wins.
//   - every target with no overlay in force is judged together by the local
//     file's budget, over their combined spend, on the run's clock: the
//     instance-global pool the README documents.
//
// A trip exhausts the account's targets for the rest of the run. Run keeps
// round-robining the others and stops only when every target is exhausted, so
// one project's ceiling can no longer take its siblings down.
//
// The ledgers live on the Driver and are charged where the loop counts a
// session's spend (countTrustedSpend and the supervised wait's probe), so every
// check — between drains, between phases, between attempts, and inside a
// paused wait — reads the same live accounts rather than a figure threaded
// down from the last boundary.

import (
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// harnessSpend is one harness's accumulated spend, for the per-harness blocks.
type harnessSpend struct {
	tokens int
	cost   float64
}

// targetSpend is one target's own account: the totals a run-wide dial is judged
// against, and the per-harness split a per_harness block reads.
type targetSpend struct {
	tokens    int
	cost      float64
	byHarness map[string]harnessSpend
}

// budgetVerdict is what one budget check found: the ceiling that stopped it
// (empty for none) and the figures it measured, for the log line.
type budgetVerdict struct {
	dim     string
	tokens  int
	cost    float64
	elapsed time.Duration
}

// charge books a session's spend against the target it ran for and the harness
// that ran it. It is called at the two places a session's spend enters a run —
// a counted attempt and a supervised wait's probe — so the ledger each breaker
// reads cannot drift from the memory the iteration logs and the fleet record
// report.
//
// The key is the phase's harness NAME AS THE CONFIG SPELLS IT (HarnessFor), never
// the adapter's own Name(). The two agree in production - an adapter is fetched
// from the registry by that same key - but only one of them is the currency the
// breaker spends: the per_harness blocks are keyed by config name. An adapter
// whose Name() diverged would silently disable its own per-harness ceiling rather
// than failing, so the ledger asks the config what a session IS and asks the
// adapter only behavioural questions.
//
// An UNTRUSTED session is charged nowhere, for the reason endUntrustedDrain
// gives: a figure parsed out of a stream with a hole in it is not a measurement.
func (d *Driver) charge(ti int, harnessName string, tokens int, cost float64) {
	if ti < 0 || ti >= len(d.tgtSpend) {
		// A Driver built by hand in a test may carry no target ledger; production
		// always charges a target index it just drained.
		return
	}
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	s := &d.tgtSpend[ti]
	s.tokens += tokens
	s.cost += cost
	if s.byHarness == nil {
		s.byHarness = make(map[string]harnessSpend)
	}
	h := s.byHarness[harnessName]
	h.tokens += tokens
	h.cost += cost
	s.byHarness[harnessName] = h
}

// targetSpendAt copies target i's account, so a caller reads a consistent
// snapshot instead of racing a parallel panel member's charge.
func (d *Driver) targetSpendAt(i int) targetSpend {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if i < 0 || i >= len(d.tgtSpend) {
		return targetSpend{}
	}
	s := d.tgtSpend[i]
	return targetSpend{tokens: s.tokens, cost: s.cost, byHarness: maps.Clone(s.byHarness)}
}

// localSpend is the combined spend of every target the LOCAL file governs: a
// target with a stored overlay in force is judged by that document instead, so
// its spend neither trips nor counts against the local ceiling.
func (d *Driver) localSpend() (tokens int, cost float64) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	for i := range d.targets {
		if d.targets[i].Cfg != nil || i >= len(d.tgtSpend) {
			continue
		}
		tokens += d.tgtSpend[i].tokens
		cost += d.tgtSpend[i].cost
	}
	return tokens, cost
}

// localSpentBy is the local account's per-harness split, aggregated over the
// targets the local file governs.
func (d *Driver) localSpentBy() map[string]harnessSpend {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	out := make(map[string]harnessSpend)
	for i := range d.targets {
		if d.targets[i].Cfg != nil || i >= len(d.tgtSpend) {
			continue
		}
		for name, s := range d.tgtSpend[i].byHarness {
			agg := out[name]
			agg.tokens += s.tokens
			agg.cost += s.cost
			out[name] = agg
		}
	}
	return out
}

// tripBudget names the ceiling of b reached over this account, or "" if none
// has: the run-wide dials first, then each harness's own block against that
// harness's own spend.
//
// Any trip exhausts the account, so the order only decides which ceiling gets
// named in the log, and the global dials are named first because a config that
// sets both wants the run-wide promise reported as the one that was broken.
// Harnesses are consulted in name order so a run with two blocks reports the
// same dial every time rather than whichever the map yielded first.
func tripBudget(b config.Budget, tokens int, cost float64, elapsed time.Duration, byHarness map[string]harnessSpend) string {
	if dim := b.ExceededBy(tokens, cost, elapsed); dim != "" {
		return dim
	}
	for _, name := range slices.Sorted(maps.Keys(byHarness)) {
		s := byHarness[name]
		if dim := b.ExceededByHarness(name, s.tokens, s.cost); dim != "" {
			// That harness's OWN totals ride along, because every caller prints
			// the figures after the reason: in a mixed-harness run two dollar
			// figures on one line read as a contradiction, which is the defect
			// Budget.ExceededBy's doc comment exists to prevent, one level down.
			return fmt.Sprintf("%s (%s so far: tokens=%d cost=$%.2f)", dim, name, s.tokens, s.cost)
		}
	}
	return ""
}

// budgetTrip names the ceiling the LOCAL file's budget has reached over the
// figures given, with the local account's per-harness split. Kept as the
// local-account check; a target with a stored overlay asks targetBudgetTrip.
func (d *Driver) budgetTrip(tokens int, cost float64, elapsed time.Duration) string {
	return tripBudget(d.cfg.Budget, tokens, cost, elapsed, d.localSpentBy())
}

// targetElapsed is how long target i has been draining. The stored budget's
// wall clock runs on THIS clock, not the run's: a project polled into a daemon
// two hours old has not thereby spent two hours of its own ratified ceiling.
// Zero until the target's first drain.
func (d *Driver) targetElapsed(i int) time.Duration {
	if i < 0 || i >= len(d.tgtStart) || d.tgtStart[i].IsZero() {
		return 0
	}
	return time.Since(d.tgtStart[i])
}

// budgetElapsed is the clock the budget in force for target i runs on: the
// target's own when a stored overlay governs it, else the run's.
func (d *Driver) budgetElapsed(i int, runElapsed time.Duration) time.Duration {
	if i >= 0 && i < len(d.targets) && d.targets[i].Cfg != nil {
		return d.targetElapsed(i)
	}
	return runElapsed
}

// targetBudgetTrip judges target i's account against the budget in force for
// it: its stored document when one is in force, else the local file's budget
// over every target the local file still governs.
func (d *Driver) targetBudgetTrip(i int, runElapsed time.Duration) budgetVerdict {
	if i >= 0 && i < len(d.targets) {
		if eff := d.targets[i].Cfg; eff != nil {
			s := d.targetSpendAt(i)
			elapsed := d.targetElapsed(i)
			return budgetVerdict{
				dim:     tripBudget(eff.Budget, s.tokens, s.cost, elapsed, s.byHarness),
				tokens:  s.tokens,
				cost:    s.cost,
				elapsed: elapsed,
			}
		}
	}
	tokens, cost := d.localSpend()
	return budgetVerdict{
		dim:     d.budgetTrip(tokens, cost, runElapsed),
		tokens:  tokens,
		cost:    cost,
		elapsed: runElapsed,
	}
}

// exhaustedAt reports whether target i has reached its ceiling for this run.
func (d *Driver) exhaustedAt(i int) bool {
	return i >= 0 && i < len(d.exhausted) && d.exhausted[i]
}

// allExhausted reports whether every target has reached its ceiling: the run's
// one honest stop while a budget that is not the local pool's is in force.
func (d *Driver) allExhausted() bool {
	if len(d.targets) == 0 {
		return false
	}
	for i := range d.targets {
		if !d.exhaustedAt(i) {
			return false
		}
	}
	return true
}

// markBudgetExhausted records a trip against the target(s) its account governs,
// and says so once per account. An overlay budget governs its one target; the
// local file's budget is the run-wide promise over every target it still
// governs, so they are exhausted together.
//
// midDrain marks the trip as the reason THIS drain ended, so Run continues with
// the siblings instead of reading the drain's stop as a run-wide stop.
func (d *Driver) markBudgetExhausted(ti int, v budgetVerdict, midDrain bool) {
	if ti >= 0 && ti < len(d.targets) && d.targets[ti].Cfg != nil {
		if ti < len(d.exhausted) {
			d.exhausted[ti] = true
		}
		if midDrain && ti < len(d.budgetStopped) {
			d.budgetStopped[ti] = true
		}
		log.Printf("%sbudget reached: %s — this project is exhausted for the rest of this run; the other projects keep draining (tokens=%d cost=$%.2f elapsed=%s)",
			d.prefix(ti), v.dim, v.tokens, v.cost, v.elapsed.Round(time.Second))
		return
	}

	var governed []string
	for i := range d.targets {
		if d.targets[i].Cfg != nil {
			continue
		}
		if i < len(d.exhausted) {
			d.exhausted[i] = true
		}
		name := d.targets[i].Name
		if name == "" {
			name = "this project"
		}
		governed = append(governed, name)
	}
	if midDrain && ti >= 0 && ti < len(d.budgetStopped) {
		d.budgetStopped[ti] = true
	}
	log.Printf("budget reached: %s — the local budget governs %s, so those projects are exhausted for the rest of this run; projects with a stored budget keep draining (tokens=%d cost=$%.2f elapsed=%s)",
		v.dim, strings.Join(governed, ", "), v.tokens, v.cost, v.elapsed.Round(time.Second))
}

// exhaustForWait marks a target (or, for the local account, every locally
// governed target) as done for the run when its remaining ceiling cannot cover a
// wait until a stated reset. It is not a trip — no ceiling has been reached — but
// the effect is the same: no more sessions for that account this run, while the
// others carry on.
func (d *Driver) exhaustForWait(ti int, reason string) {
	if ti >= 0 && ti < len(d.targets) && d.targets[ti].Cfg != nil {
		if ti < len(d.exhausted) {
			d.exhausted[ti] = true
		}
		log.Printf("%s%s — this project is exhausted for the rest of this run; the other projects keep draining", d.prefix(ti), reason)
	} else {
		for i := range d.targets {
			if d.targets[i].Cfg == nil && i < len(d.exhausted) {
				d.exhausted[i] = true
			}
		}
		log.Printf("%s — the projects under the local budget are exhausted for the rest of this run; projects with a stored budget keep draining", reason)
	}
	if ti >= 0 && ti < len(d.budgetStopped) {
		d.budgetStopped[ti] = true
	}
}

// sweepBudgets judges every target not yet exhausted against the budget in
// force for it and marks the ones that have reached a ceiling. Returns whether
// every target is now exhausted.
func (d *Driver) sweepBudgets(runElapsed time.Duration) bool {
	for i := range d.targets {
		if d.exhaustedAt(i) {
			continue
		}
		v := d.targetBudgetTrip(i, runElapsed)
		if v.dim == "" {
			continue
		}
		d.markBudgetExhausted(i, v, false)
	}
	return d.allExhausted()
}

// budgetStop is the mid-drain form of the sweep: it judges target ti's account
// and, on a trip, exhausts it and records that THIS drain ended on the budget.
// Run reads that record to continue with the siblings instead of treating the
// drain's stop as a run-wide stop.
func (d *Driver) budgetStop(ti int, runElapsed time.Duration) budgetVerdict {
	v := d.targetBudgetTrip(ti, runElapsed)
	if v.dim == "" {
		return v
	}
	d.markBudgetExhausted(ti, v, true)
	return v
}
