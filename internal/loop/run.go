package loop

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// Run drives the daemon until STOP/HALT, a ceiling (max-iterations / budget), or context
// cancellation. Returns nil on a graceful stop; an error only on an unexpected, non-retryable
// failure.
func (d *Driver) Run(ctx context.Context) (runErr error) {
	// A caller may have opened the state dir already (tests, and any future entry point that wants to
	// fail before spawning anything).
	if d.state == nil {
		dir, err := d.cfg.ResolveStateDir()
		if err != nil {
			return err
		}
		st, err := statedir.Open(dir, d.cfg.SessionWorkDirs()...)
		if err != nil {
			return err
		}
		d.state = st
		defer func() {
			_ = st.Close()
			d.state = nil
		}()
	}
	d.clearStaleLimitStop()
	// Belt and braces on the way out.
	defer func() { d.sweepPendingStop(runErr) }()
	// A run that ends in an orderly way holds nothing, so nothing is left for a supervisor to hand
	// back.
	defer d.clearCurrentClaim()
	if src := d.cfg.Source(); src != "" {
		log.Printf("config: %s", src)
	}
	// Recomputed every iteration below, so a RELOAD that edits the interval is seen from the next idle
	// poll on.
	idle := d.cfg.IdlePollInterval.OrDefault(60 * time.Second)
	d.loadRouteMarks()
	// Every harness this run will SPAWN, not just the configured one: a mixed sequence's banner naming
	// one of them is a log that disagrees with the sessions underneath it from the first line.
	log.Printf("driving %s; state in %s; idle poll every %s", strings.Join(d.cfg.SpawnedHarnesses(), "+"), d.state.Path(), idle)
	// Say it once, loudly, if the pre-CLA-259 directory is still sitting in the workdir: an operator
	// who touches STOP there would otherwise watch the loop ignore it and conclude the stop switch is
	// broken.
	if legacy := d.cfg.LegacyStateDir(); legacy != "" {
		log.Printf("note: %s is left over from before the state dir moved out of the workdir — markers there are IGNORED, and its old transcripts are still world-readable inside your repo; move or delete it", legacy)
	}
	// A STOP marker already sitting here PREDATES this process: markers are only ever read and
	// consumed by a running daemon (the cycle top below, and waitOrStop), so one found at start-up was
	// left behind by a previous run that exited without consuming it.
	if present, msg := d.readMarker("STOP"); present {
		_ = d.state.Remove("STOP")
		note := ""
		if msg != "" {
			note = ": " + msg
		}
		log.Printf("warning: STOP marker%s is left over from a previous run - consumed WITHOUT stopping this one; drop a fresh marker to stop THIS run", note)
	}

	start := time.Now()
	drains := 0

	// CLA-461: everything below runs on a context this Run owns, so a RESTART_NOW marker can cancel it
	// mid-session and ride the exact kill/cleanup/release machinery a Ctrl-C rides.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	d.watchRestartNow(runCtx, cancelRun)

	// CLA-466: whatever ends the run — STOP/HALT, a restart marker, max-iterations, budget, signal
	// or error — every target's final `stopping` beacon goes out first.
	defer d.fleetShutdown()

	for {
		if runCtx.Err() != nil {
			log.Print("cancelled — stopping")
			return nil
		}
		if d.checkMarkers() {
			return nil
		}
		idle := d.cfg.IdlePollInterval.OrDefault(60 * time.Second)
		if d.cfg.MaxIterations > 0 && drains >= d.cfg.MaxIterations {
			log.Printf("reached max-iterations (%d) — stopping", d.cfg.MaxIterations)
			return nil
		}
		// The budget breaker's between-iterations pass: every target's own account is judged against the
		// budget in force for it, and a target that reaches a ceiling stops draining while its siblings
		// carry on.
		if d.sweepBudgets(time.Since(start)) {
			log.Print("every project's budget is exhausted — stopping")
			return nil
		}

		// Gate on cheap control-plane reads: only spend a session when some target has work to spawn for
		// — ready work, or abandoned work the plane would offer for takeover.
		target, next, perr := d.pickTarget(runCtx, idle)
		if perr != nil {
			return perr
		}
		switch next {
		case runStop:
			return nil
		case runContinue:
			continue
		}

		drains++
		// The next poll of this target judges whether the drain settled anything.
		d.pending[d.cursor] = true
		// The target's own budget clock starts at its first drain: a stored max_wall_clock measures how
		// long THIS project has been drained, not how long the daemon has been up for its siblings.
		d.noteTargetStart(d.cursor)
		tokens, _, handoffs, stop, err := d.drainPhases(runCtx, drains, d.cursor, target,
			spend{start: start, ti: d.cursor})
		// The drain's spend is already in the target's ledger: charge() books it as each session's result
		// is counted, which is what lets the mid-drain checks and this between-iterations sweep read the
		// same live account.
		allGone := d.sweepBudgets(time.Since(start))
		if allGone {
			log.Print("every project's budget is exhausted — stopping")
			return nil
		}
		// Hand the drain's tokens to the verdict that will judge it: a fruitless drain is charged to the
		// token-denominated quiet accumulator, and the figure must be this drain's, not whatever total
		// happened to be in scope when judgeProgress runs.
		d.spent[d.cursor] = tokens
		// Handoff respawns consume iterations of the operator's ceiling too — see the comment above the
		// accumulator.
		drains += handoffs
		// CLA-402: report the run's dead-phase tally with its denominator, so the operator watching the
		// daemon log sees the rate the run is producing (the last line before the run ends IS the run
		// total).
		d.logDeadTally()
		if err != nil {
			// Classify before acting. The run-wide causes are the ones the poll loop already hard-stops on -
			// a bad key or a missing project selector is shared by every target, so no target can make
			// progress and sidelining would just defer the same exit.
			if errors.Is(err, backlog.ErrUnauthorized) || errors.Is(err, backlog.ErrProjectRequired) {
				return err
			}
			// Everything else out of drainPhases happened while trying to run THIS target's session, so it
			// sidelines THIS target: it sits out on the escalating ladder while every other project drains
			// on, and self-heals once its harness succeeds again.
			d.sidelineTarget(d.cursor, drains, err)
			// CLA-562: the startup-failure escalation rides on top of the sideline, mirroring the fleet
			// trip.
			if tokens == 0 {
				d.startupFails[d.cursor]++
				if d.startupFails[d.cursor] >= startupFailBound {
					d.startupTrip(runCtx, target, d.cursor, drains, err)
				}
			} else {
				d.startupFails[d.cursor] = 0
			}
			if d.allSidelined() {
				return fmt.Errorf("every project's harness is failing - nothing left to drive, stopping:%s", d.sidelinedReport())
			}
		} else {
			if d.harnessFails[d.cursor] > 0 {
				log.Printf("%sthe harness succeeded again - rejoining the rotation", d.prefix(d.cursor))
				d.harnessFails[d.cursor] = 0
				d.harnessErrs[d.cursor] = nil
			}
			// CLA-562: any drain that spawned (err == nil) proves the harness starts, so it resets startup
			// consecutiveness — including a drain that ended in a dead-phase park or fleet trip, which
			// still ran sessions.
			d.startupFails[d.cursor] = 0
		}
		// The run ends when every target has reached the budget in force for it (handled above, before
		// the drain's outcome).
		if stop {
			if d.budgetStoppedAt(d.cursor) {
				d.clearBudgetStopped(d.cursor)
				// markBudgetExhausted has already named the account and its projects; this line only says the
				// drain is unwinding rather than ending the daemon.
				log.Printf("%sbudget exhausted mid-drain — continuing with the projects that still have headroom", d.prefix(d.cursor))
			} else {
				return nil
			}
		}

		// CLA-564: every task boundary is a stop boundary, not just idle waits.
		if runCtx.Err() == nil {
			if present, msg := d.readMarker("HALT"); present {
				log.Printf("HALT present: %s — resolve and delete %s to resume", msg, filepath.Join(d.state.Path(), "HALT"))
				return nil
			}
			if present, _ := d.readMarker("STOP"); present {
				_ = d.state.Remove("STOP")
				log.Print("STOP requested — stopping")
				return nil
			}
		}

		// Blind mode has no counts to gate on, so it idles between sessions rather than spinning.
		if d.blind {
			log.Printf("idle — re-checking in %s", idle)
			if d.waitOrStop(runCtx, idle) {
				return nil
			}
		}
	}
}

// sweepPendingStop is Run's exit sweep: it removes a STOP marker the run never consumed, and logs
// the reason the run ended.
func (d *Driver) sweepPendingStop(runErr error) {
	if d.state == nil {
		return
	}
	present, _ := d.readMarker("STOP")
	if !present {
		if runErr != nil {
			log.Printf("run ended: %v", runErr)
		}
		return
	}
	_ = d.state.Remove("STOP")
	why := "stopped for another reason"
	if runErr != nil {
		why = runErr.Error()
	}
	log.Printf("run ended (%s) without consuming the STOP marker - removed it so the next start is not stopped by it", why)
}

// pickTarget polls every target's queue and chooses the one to drain next: round-robin over the
// targets with spawnable work, or over those not backed off when polling is blind.
func (d *Driver) pickTarget(runCtx context.Context, idle time.Duration) (Target, runNext, error) {
	var target Target
	// Read once per pass, so one boundary landing mid-pass cannot give a target a different answer
	// from its siblings.
	sched := d.scheduledPause()
	if !d.blind {
		candidates := make([]bool, len(d.targets))
		anyCandidate := false
		// The local BEACON takes no target, so this pass writes it ONCE, from the state that best speaks
		// for the daemon.
		localState := fleet.State{Kind: fleet.StateIdle}
		reported := false
		for i := range d.targets {
			t := d.targets[i]
			sum, err := t.Poller.Poll(runCtx)
			switch {
			case errors.Is(err, backlog.ErrNotWired):
				log.Print("backlog polling not wired — blind mode: drain, then idle-poll by re-draining (wire backlog polling to gate on live counts cheaply)")
				d.blind = true
			case errors.Is(err, backlog.ErrUnauthorized):
				// A 401/403 means CLANKERBAR_API_KEY is bad — and the harness sessions we'd spawn carry the
				// SAME key, so blind-draining or idle-retrying just burns dead work against a credential that
				// won't self-heal.
				log.Printf("%sbacklog auth failed (401/403) — check CLANKERBAR_API_KEY; stopping", d.prefix(i))
				return target, runStop, fmt.Errorf("backlog auth failed (401/403) — check CLANKERBAR_API_KEY: %w", err)
			case errors.Is(err, backlog.ErrProjectRequired):
				// A 400 project_required means the poll hit the LEGACY slug-less summary route with an
				// ACCOUNT-scoped key, which that route cannot bind to a project.
				log.Printf("%sbacklog poll: the summary route needs a project selector — add a `projects` list to the config, or point the loop at an .mcp.json naming /mcp/<slug>; stopping", d.prefix(i))
				return target, runStop, fmt.Errorf("backlog poll needs a project selector (configure `projects` or an /mcp/<slug> .mcp.json; a project-scoped key also works for CI): %w", err)
			case err != nil:
				log.Printf("%sbacklog poll error: %v — retry in %s", d.prefix(i), err, idle)
			default:
				// The stored execution-config boundary: a moved runConfigVersion — including the first poll,
				// which is "start of run" — refetches and re-overlays BEFORE the spawn gate reads anything,
				// so the drain that follows this poll runs on the current document.
				d.checkRunConfigVersion(runCtx, i, &d.targets[i], sum.RunConfigVersion)
				// stale_claimable is reported beside claimable, not folded into it: an operator watching the
				// console has to be able to tell "spawning because there is fresh work" from "spawning to
				// recover an abandoned branch", and before CLA-274 those were indistinguishable — the second
				// did not happen at all, and now that it does it must not look like the first.
				log.Printf("%squeue: ready=%d claimable=%d stale_claimable=%d in_progress=%d open_questions=%d paused=%t (v%d)",
					d.prefix(i), sum.Ready, sum.Claimable, sum.StaleClaimable, sum.InProgress, sum.OpenQuestions, sum.Paused, sum.Version)
				// Console pause: the operator can pause a run from the web console, PER PROJECT.
				if sum.Paused {
					if !d.paused[i] {
						d.paused[i] = true
						log.Printf("%sconsole pause active — not spawning new sessions; idle-polling until resumed", d.prefix(i))
					}
				} else {
					if d.paused[i] {
						d.paused[i] = false
						log.Printf("%sconsole pause cleared — resuming", d.prefix(i))
					}
					// A poll taken while the loop has stopped spawning cannot tell "our last drain settled
					// nothing" from "nothing happened because we stopped", so the verdict waits until the loop is
					// live again.
					if !sched && !d.exhaustedAt(i) {
						d.judgeProgress(i, sum)
					}
					// CLA-396: a target the fleet counter paused stays paused until the operator answers the
					// raised question — the open-question count falls back to what it was at the raise (the same
					// "something was resolved" signal judgeProgress uses for the quiet back-off).
					if d.fleetPaused[i] && sum.OpenQuestions <= d.fleetOpenQ[i] {
						d.fleetPaused[i] = false
						d.fleetRaised[i] = false
						log.Printf("%sfleet pause cleared — the operator answered the dead-phase question; resuming", d.prefix(i))
					}
					// CLA-562: the same resume signal for the startup-failure pause - answering resumes, and the
					// next startup failure re-arms (the counter stays at the bound, so the next startup failure
					// trips again and raises fresh).
					if d.startupPaused[i] && sum.OpenQuestions <= d.startupOpenQ[i] {
						d.startupPaused[i] = false
						d.startupRaised[i] = false
						log.Printf("%sstartup-failure pause cleared - the operator answered the harness question; resuming", d.prefix(i))
					}
					if sum.Spawnable() && !d.backedOff(i) && !sched && !d.fleetPaused[i] && !d.startupPaused[i] && !d.exhaustedAt(i) {
						candidates[i] = true
						anyCandidate = true
					}
				}
			}
			// CLA-466: the presence beacon rides THIS poll — no new cadence.
			st := d.fleetState(i)
			d.sendBeacon(i, t, st)
			localState = pollBeaconState(localState, st)
			reported = true
			if d.blind {
				break
			}
		}
		if reported {
			d.writeLocalBeacon(localState)
		}
		if !d.blind {
			if !anyCandidate {
				if d.waitOrStop(runCtx, idle) {
					return target, runStop, nil
				}
				return target, runContinue, nil
			}
			for i := 1; i <= len(d.targets); i++ {
				idx := (d.cursor + i) % len(d.targets)
				if candidates[idx] {
					d.cursor = idx
					break
				}
			}
			target = d.targets[d.cursor]
		}
	}
	if d.blind {
		// An active window chooses no target at all, before the rotation below can reach for one.
		if sched {
			return target, d.idleThroughPause(runCtx, idle), nil
		}
		// Blind mode has no counts to route on - rotate across targets so every queue still gets
		// sessions.
		found := false
		orig := d.cursor
		for i := 1; i <= len(d.targets); i++ {
			idx := (orig + i) % len(d.targets)
			d.cursor = idx
			if !d.backedOff(idx) && !d.exhaustedAt(idx) {
				found = true
				break
			}
		}
		if !found {
			return target, runStop, fmt.Errorf("every project's harness is failing - nothing left to drive, stopping:%s", d.sidelinedReport())
		}
		target = d.targets[d.cursor]
	}

	// There is work (or we're blind) — spend a session (or a phase sequence of them, one task either
	// way), retrying transient blips with backoff (a fresh session reclaims any half-done task).
	return target, runFall, nil
}

// runNext says what Run does after choosing a target.
type runNext int

const (
	runFall     runNext = iota // drain the chosen target
	runContinue                // nothing to drain: take the next pass
	runStop                    // end the run
)

// checkMarkers reads the operator's control markers at the iteration boundary: HALT, STOP, RESTART
// and RESTART_NOW end the run (true), RELOAD re-reads the config.
func (d *Driver) checkMarkers() bool {
	if present, msg := d.readMarker("HALT"); present {
		log.Printf("HALT present: %s — resolve and delete %s to resume", msg, filepath.Join(d.state.Path(), "HALT"))
		return true
	}
	if present, _ := d.readMarker("STOP"); present {
		_ = d.state.Remove("STOP")
		log.Print("STOP requested — stopping")
		return true
	}
	// The restart/reload markers, honoured exactly where STOP is: the ITERATION boundary and, for STOP
	// and RESTART, a verified phase seam.
	if present, msg := d.readMarker(MarkerRestart); present {
		_ = d.state.Remove(MarkerRestart)
		if msg != "" {
			log.Printf("RESTART requested: %s", msg)
		} else {
			log.Print("RESTART requested")
		}
		log.Print("re-executing at the iteration boundary")
		d.restartRequested.Store(true)
		return true
	}
	if present, msg := d.readMarker(MarkerRestartNow); present {
		_ = d.state.Remove(MarkerRestartNow)
		if msg != "" {
			log.Printf("RESTART_NOW requested: %s", msg)
		} else {
			log.Print("RESTART_NOW requested")
		}
		log.Print("nothing in flight here — re-executing immediately")
		d.restartRequested.Store(true)
		return true
	}
	// Locked against a panel member's wait-side RELOAD: both consume the same marker, and the
	// read-then-remove is not atomic — the lock is what makes the reload happen once, on one
	// goroutine, whatever else is parked in waitOrStop.
	d.controlMu.Lock()
	if present, _ := d.readMarker(MarkerReload); present {
		_ = d.state.Remove(MarkerReload)
		log.Print("RELOAD requested — re-reading the config file at the iteration boundary")
		d.applyReload()
	}
	d.controlMu.Unlock()
	return false
}
