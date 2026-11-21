package loop

// Budget enforcement.

import (
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

// targetSpend is one target's own account: the totals a run-wide dial is judged against, and the
// per-harness split a per_harness block reads.
type targetSpend struct {
	tokens    int
	cost      float64
	byHarness map[string]harnessSpend
}

// spend is the run-scoped context a drain is measured in: when the run began (the local budget's
// clock) and which target it is draining (whose stored overlay, if any, governs the spend).
type spend struct {
	start time.Time
	ti    int
}

// budgetVerdict is what one budget check found: the ceiling that stopped it (empty for none) and
// the figures it measured, for the log line.
type budgetVerdict struct {
	dim     string
	tokens  int
	cost    float64
	elapsed time.Duration
}

// charge books a session's spend against the target it ran for and the harness that ran it. The key
// is the phase's harness NAME AS THE CONFIG SPELLS IT (HarnessFor), never the adapter's own Name().
func (d *Driver) charge(ti int, harnessName string, tokens int, cost float64) {
	if ti < 0 || ti >= len(d.tgtSpend) {
		// A Driver built by hand in a test may carry no target ledger; production always charges a target
		// index it just drained.
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

// targetSpendAt copies target i's account, so a caller reads a consistent snapshot instead of
// racing a parallel panel member's charge.
func (d *Driver) targetSpendAt(i int) targetSpend {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if i < 0 || i >= len(d.tgtSpend) {
		return targetSpend{}
	}
	s := d.tgtSpend[i]
	return targetSpend{tokens: s.tokens, cost: s.cost, byHarness: maps.Clone(s.byHarness)}
}

// localSpend is the combined spend of every target the LOCAL file governs: a target whose stored
// document sets its own budget is judged by that document instead, so its spend neither trips nor
// counts against the local ceiling.
func (d *Driver) localSpend() (tokens int, cost float64) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	for i := range d.targets {
		if d.overlayBudgetAt(i) || i >= len(d.tgtSpend) {
			continue
		}
		tokens += d.tgtSpend[i].tokens
		cost += d.tgtSpend[i].cost
	}
	return tokens, cost
}

// localSpentBy is the local account's per-harness split, aggregated over the targets the local file
// governs.
func (d *Driver) localSpentBy() map[string]harnessSpend {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	out := make(map[string]harnessSpend)
	for i := range d.targets {
		if d.overlayBudgetAt(i) || i >= len(d.tgtSpend) {
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

// tripBudget names the ceiling of b reached over this account, or "" if none has: the run-wide
// dials first, then each harness's own block against that harness's own spend. Any trip exhausts
// the account, so the order only decides which ceiling gets named in the log, and the global dials
// are named first because a config that sets both wants the run-wide promise reported as the one
// that was broken.
func tripBudget(b config.Budget, tokens int, cost float64, elapsed time.Duration, byHarness map[string]harnessSpend) string {
	if dim := b.ExceededBy(tokens, cost, elapsed); dim != "" {
		return dim
	}
	for _, name := range slices.Sorted(maps.Keys(byHarness)) {
		s := byHarness[name]
		if dim := b.ExceededByHarness(name, s.tokens, s.cost); dim != "" {
			// The harness name already leads the dimension, and the caller prints the account's totals after
			// the reason.
			return dim
		}
	}
	return ""
}

// budgetTrip names the ceiling the LOCAL file's budget has reached over the figures given, with the
// local account's per-harness split.
func (d *Driver) budgetTrip(tokens int, cost float64, elapsed time.Duration) string {
	return tripBudget(d.cfg.Budget, tokens, cost, elapsed, d.localSpentBy())
}

// targetElapsed is how long target i has been draining. The stored budget's wall clock runs on THIS
// clock, not the run's: a project polled into a daemon two hours old has not thereby spent two
// hours of its own ratified ceiling.
func (d *Driver) targetElapsed(i int) time.Duration {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if i < 0 || i >= len(d.tgtStart) || d.tgtStart[i].IsZero() {
		return 0
	}
	return time.Since(d.tgtStart[i])
}

// noteTargetStart stamps target i's clock at its first drain; later drains keep the original stamp.
func (d *Driver) noteTargetStart(i int) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if i >= 0 && i < len(d.tgtStart) && d.tgtStart[i].IsZero() {
		d.tgtStart[i] = time.Now()
	}
}

// budgetElapsed is the clock the budget in force for target i runs on: the target's own when a
// stored overlay sets its ceiling, else the run's.
func (d *Driver) budgetElapsed(i int, runElapsed time.Duration) time.Duration {
	runPaused, targetPaused := d.pauseCredit(i)
	if d.overlayBudgetAt(i) {
		elapsed := d.targetElapsed(i) - targetPaused
		if elapsed < 0 {
			return 0
		}
		return elapsed
	}
	runElapsed -= runPaused
	if runElapsed < 0 {
		return 0
	}
	return runElapsed
}

// notePauseHeld books wall-clock time a scheduled pause held target ti at a phase boundary.
func (d *Driver) notePauseHeld(ti int, held time.Duration) {
	if held <= 0 {
		return
	}
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	d.pausedRun += held
	if ti >= 0 && ti < len(d.pausedTargets) {
		d.pausedTargets[ti] += held
	}
}

// pauseCredit is how much wall clock to subtract from the budget clocks for target ti: the run-wide
// total and that target's own.
func (d *Driver) pauseCredit(ti int) (run, target time.Duration) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	run = d.pausedRun
	if ti >= 0 && ti < len(d.pausedTargets) {
		target = d.pausedTargets[ti]
	}
	return run, target
}

// targetBudgetTrip judges target i's account against the budget in force for it: its stored
// document's budget when one is in force, else the local file's budget over every target the local
// file still governs.
func (d *Driver) targetBudgetTrip(i int, runElapsed time.Duration) budgetVerdict {
	elapsed := d.budgetElapsed(i, runElapsed)
	if i >= 0 && i < len(d.targets) && d.overlayBudgetAt(i) {
		if eff := d.targets[i].Cfg; eff != nil {
			s := d.targetSpendAt(i)
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
		dim:     d.budgetTrip(tokens, cost, elapsed),
		tokens:  tokens,
		cost:    cost,
		elapsed: elapsed,
	}
}

// exhaustedAt reports whether target i has reached its ceiling for this run.
func (d *Driver) exhaustedAt(i int) bool {
	if i < 0 || i >= len(d.exhausted) {
		return false
	}
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	return d.exhausted[i]
}

// markExhausted flags the given targets as done for the run.
func (d *Driver) markExhausted(indices []int) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	for _, i := range indices {
		if i >= 0 && i < len(d.exhausted) {
			d.exhausted[i] = true
		}
	}
}

// markBudgetStopped records that a mid-drain check was the reason target i's drain ended.
// budgetStoppedAt reads it back for Run's stop handling, and clearBudgetStopped consumes it once
// the drain has been accounted for.
func (d *Driver) markBudgetStopped(i int) {
	if i < 0 || i >= len(d.budgetStopped) {
		return
	}
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	d.budgetStopped[i] = true
}

func (d *Driver) budgetStoppedAt(i int) bool {
	if i < 0 || i >= len(d.budgetStopped) {
		return false
	}
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	return d.budgetStopped[i]
}

func (d *Driver) clearBudgetStopped(i int) {
	if i < 0 || i >= len(d.budgetStopped) {
		return
	}
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	d.budgetStopped[i] = false
}

// allExhausted reports whether every target has reached its ceiling for this run — the stop a
// budget-scoped run ends on once nothing is left with headroom.
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

// markBudgetExhausted records a trip against the target(s) its account governs, and says so once
// per account. A stored budget governs its one target; the local file's budget is the run-wide
// promise over every target it still governs (no document, or a document that sets no budget), so
// they are exhausted together.
func (d *Driver) markBudgetExhausted(ti int, v budgetVerdict, midDrain bool) {
	if d.overlayBudgetAt(ti) {
		d.markExhausted([]int{ti})
		if midDrain {
			d.markBudgetStopped(ti)
		}
		log.Printf("%sbudget reached: %s — this project is exhausted for the rest of this run; the other projects keep draining (tokens=%d cost=$%.2f elapsed=%s)",
			d.prefix(ti), v.dim, v.tokens, v.cost, v.elapsed.Round(time.Second))
		return
	}

	var governed []string
	var indices []int
	for i := range d.targets {
		if d.overlayBudgetAt(i) {
			continue
		}
		indices = append(indices, i)
		name := d.targets[i].Name
		if name == "" {
			name = "this project"
		}
		governed = append(governed, name)
	}
	d.markExhausted(indices)
	if midDrain {
		d.markBudgetStopped(ti)
	}
	log.Printf("budget reached: %s — the local budget governs %s, so those projects are exhausted for the rest of this run; projects with a stored budget keep draining (tokens=%d cost=$%.2f elapsed=%s)",
		v.dim, strings.Join(governed, ", "), v.tokens, v.cost, v.elapsed.Round(time.Second))
}

// exhaustForWait marks a target (or, for the local account, every locally governed target) as done
// for the run when its remaining ceiling cannot cover a wait until a stated reset.
func (d *Driver) exhaustForWait(ti int, reason string) {
	if d.overlayBudgetAt(ti) {
		d.markExhausted([]int{ti})
		log.Printf("%s%s — this project is exhausted for the rest of this run; the other projects keep draining", d.prefix(ti), reason)
	} else {
		var indices []int
		for i := range d.targets {
			if !d.overlayBudgetAt(i) {
				indices = append(indices, i)
			}
		}
		d.markExhausted(indices)
		log.Printf("%s — the projects under the local budget are exhausted for the rest of this run; projects with a stored budget keep draining", reason)
	}
	d.markBudgetStopped(ti)
}

// exhaustUnmeasurable ends target ti's draining when a session's spend cannot be read and the
// ceiling that protects that spend is the target's own stored budget.
func (d *Driver) exhaustUnmeasurable(ti int, reason string) bool {
	if !d.overlayBudgetAt(ti) {
		return false
	}
	d.markExhausted([]int{ti})
	d.markBudgetStopped(ti)
	log.Printf("%s%s — this project is exhausted for the rest of this run; the other projects keep draining", d.prefix(ti), reason)
	return true
}

// sweepBudgets judges every target not yet exhausted against the budget in force for it and marks
// the ones that have reached a ceiling.
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

// budgetStop is the mid-drain form of the sweep: it judges target ti's account and, on a trip,
// exhausts it and records that THIS drain ended on the budget.
func (d *Driver) budgetStop(ti int, runElapsed time.Duration) budgetVerdict {
	v := d.targetBudgetTrip(ti, runElapsed)
	if v.dim == "" {
		return v
	}
	d.markBudgetExhausted(ti, v, true)
	return v
}
