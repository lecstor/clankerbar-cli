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
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// Run drives the daemon until STOP/HALT, a ceiling (max-iterations / budget), or
// context cancellation. An empty queue is NOT a stop condition — it idles and
// keeps polling. Returns nil on a graceful stop; an error only on an unexpected,
// non-retryable failure.
//
// Two things hold on every way out (CLA-491): a STOP marker this run never
// consumed is removed rather than left where the next start would read it as a
// fresh stop request, and the last log line names why the run ended.
func (d *Driver) Run(ctx context.Context) (runErr error) {
	// A caller may have opened the state dir already (tests, and any future entry
	// point that wants to fail before spawning anything). Opening it here is the
	// normal path.
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
	// Belt and braces on the way out (CLA-491). Every shutdown path that does not
	// pass a STOP read (a non-retryable phase failure, a budget or zero-spend
	// trip, max-iterations, HALT, a cancelled context) leaves a
	// pending STOP marker where it fell, and the next start used to read it as
	// fresh and stop itself seconds into its first cycle: the 2026-08-25 fleet
	// soft-stop left one behind clanker1, and the upgraded replacement DOA'd on
	// it. Registered here rather than at each return so a future shutdown path
	// cannot forget it. (The closure, not `defer d.sweepPendingStop(runErr)`:
	// arguments evaluate at registration, and runErr is not assigned yet.)
	defer func() { d.sweepPendingStop(runErr) }()
	// A run that ends in an orderly way holds nothing, so nothing is left for a
	// supervisor to hand back (CLA-708). Every let-go path removes this file as
	// it happens; this is the backstop for a path that forgets. A process that is
	// KILLED never reaches it, and that is exactly the case the supervisor's exit
	// check exists for.
	defer d.clearCurrentClaim()
	if src := d.cfg.Source(); src != "" {
		log.Printf("config: %s", src)
	}
	// Recomputed every iteration below, so a RELOAD that edits the interval is
	// seen from the next idle poll on. This first value is only what the banner
	// announces.
	idle := d.cfg.IdlePollInterval.OrDefault(60 * time.Second)
	d.loadRouteMarks()
	// Every harness this run will SPAWN, not just the configured one: a mixed
	// sequence's banner naming one of them is a log that disagrees with the
	// sessions underneath it from the first line (CLA-366). SpawnedHarnesses
	// rather than PhaseHarnesses, so a run-wide harness every phase overrides is
	// not announced as being driven when nothing will ever run on it - the banner
	// is the operator's first line of output, and it should describe the run.
	log.Printf("driving %s; state in %s; idle poll every %s", strings.Join(d.cfg.SpawnedHarnesses(), "+"), d.state.Path(), idle)
	// Say it once, loudly, if the pre-CLA-259 directory is still sitting in the
	// workdir: an operator who touches STOP there would otherwise watch the loop
	// ignore it and conclude the stop switch is broken.
	if legacy := d.cfg.LegacyStateDir(); legacy != "" {
		log.Printf("note: %s is left over from before the state dir moved out of the workdir — markers there are IGNORED, and its old transcripts are still world-readable inside your repo; move or delete it", legacy)
	}
	// A STOP marker already sitting here PREDATES this process: markers are only
	// ever read and consumed by a running daemon (the cycle top below, and
	// waitOrStop), so one found at start-up was left behind by a previous run
	// that exited without consuming it (CLA-491). Its intent died with that run;
	// acting on it here would stop this daemon before it did anything: exactly
	// what the upgraded clanker1 did seconds after its start banner. Consume it
	// with a loud note instead. A stop dropped AFTER this point is fresh and
	// still lands on every read below.
	if present, msg := d.readMarker("STOP"); present {
		_ = d.state.Remove("STOP")
		note := ""
		if msg != "" {
			note = ": " + msg
		}
		log.Printf("warning: STOP marker%s is left over from a previous run - consumed WITHOUT stopping this one; drop a fresh marker to stop THIS run", note)
	}

	start := time.Now()
	var totalTokens int
	var totalCost float64
	drains := 0

	// CLA-461: everything below runs on a context this Run owns, so a RESTART_NOW
	// marker can cancel it mid-session and ride the exact kill/cleanup/release
	// machinery a Ctrl-C rides. Cancelling the caller's ctx directly is not
	// available to us; deriving one is.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	d.watchRestartNow(runCtx, cancelRun)

	// CLA-466: whatever ends the run — STOP/HALT, a restart marker,
	// max-iterations, budget, signal or error — every target's final `stopping`
	// beacon goes out first. After it, console silence means the daemon is gone.
	defer d.fleetShutdown()

	for {
		if runCtx.Err() != nil {
			log.Print("cancelled — stopping")
			return nil
		}
		if present, msg := d.readMarker("HALT"); present {
			log.Printf("HALT present: %s — resolve and delete %s to resume", msg, filepath.Join(d.state.Path(), "HALT"))
			return nil
		}
		if present, _ := d.readMarker("STOP"); present {
			_ = d.state.Remove("STOP")
			log.Print("STOP requested — stopping")
			return nil
		}
		// The restart/reload markers (CLA-461), honoured exactly where STOP is:
		// the ITERATION boundary. Never a mid-drain phase seam — the run holds
		// the task there. Reaching this point means the previous drain has fully
		// unwound and handed its claim back, so a graceful restart holds no claim
		// across the boundary by construction (resume is the plane's job anyway).
		// A RESTART_NOW consumed here found no session in flight and acts as a
		// plain restart; when one IS in flight the watcher in watchRestartNow has
		// already cancelled the drain and this block is never reached.
		if present, msg := d.readMarker(MarkerRestart); present {
			_ = d.state.Remove(MarkerRestart)
			if msg != "" {
				log.Printf("RESTART requested: %s", msg)
			} else {
				log.Print("RESTART requested")
			}
			log.Print("re-executing at the iteration boundary")
			d.restartRequested.Store(true)
			return nil
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
			return nil
		}
		// Locked against a panel member's wait-side RELOAD (CLA-697 review finding
		// 1.review-1.2): both consume the same marker, and the read-then-remove is
		// not atomic — the lock is what makes the reload happen once, on one
		// goroutine, whatever else is parked in waitOrStop.
		d.controlMu.Lock()
		if present, _ := d.readMarker(MarkerReload); present {
			_ = d.state.Remove(MarkerReload)
			log.Print("RELOAD requested — re-reading the config file at the iteration boundary")
			d.applyReload()
		}
		d.controlMu.Unlock()
		idle := d.cfg.IdlePollInterval.OrDefault(60 * time.Second)
		if d.cfg.MaxIterations > 0 && drains >= d.cfg.MaxIterations {
			log.Printf("reached max-iterations (%d) — stopping", d.cfg.MaxIterations)
			return nil
		}
		if dim := d.budgetTrip(totalTokens, totalCost, time.Since(start)); dim != "" {
			log.Printf("budget reached: %s — stopping to leave headroom (tokens=%d cost=$%.2f elapsed=%s)",
				dim, totalTokens, totalCost, time.Since(start).Round(time.Second))
			return nil
		}

		// Gate on cheap control-plane reads: only spend a session when some target
		// has work to spawn for — ready work, or abandoned work the plane would
		// offer for takeover (Summary.Spawnable, CLA-274). When idle, keep polling
		// (and logging) so the loop reacts to answered questions / promotions /
		// newly filed work. With several targets (CLA-142) every queue is polled
		// each cycle, and the drain goes to the next spawnable one after the last
		// drained (round-robin) — a busy queue can't starve a quiet one.
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
		tokens, cost, handoffs, stop, err := d.drainPhases(runCtx, drains, d.cursor, target,
			spend{start: start, tokens: totalTokens, cost: totalCost})
		// Count the spend BEFORE deciding what to do with the outcome: a drain that
		// stopped or failed still burned what it burned, and the accumulator is what
		// the next iteration's breaker and log line are measured against. Handoff
		// respawns are charged here too (CLA-352): unlike phase sessions, which the
		// operator priced in when they configured `phases`, each respawn is a
		// session the SESSION chose to add, so it consumes an iteration of the
		// operator's ceiling rather than extending the run for free.
		totalTokens += tokens
		totalCost += cost
		// The per-target ledger (CLA-410): where a stored budget is in force for
		// THIS target, its ceilings are judged on its own spend, not on a total
		// every target feeds. Checked here — the same between-iterations moment
		// the run-wide breaker reads — and only when an overlay exists, so an
		// un-overlaid target's behaviour is untouched.
		d.tgtTokens[d.cursor] += tokens
		d.tgtCost[d.cursor] += cost
		if eff := d.targets[d.cursor].Cfg; eff != nil {
			if dim := eff.Budget.ExceededBy(d.tgtTokens[d.cursor], d.tgtCost[d.cursor], time.Since(start)); dim != "" {
				log.Printf("%sbudget reached: %s — stopping to leave headroom (target tokens=%d cost=$%.2f)",
					d.prefix(d.cursor), dim, d.tgtTokens[d.cursor], d.tgtCost[d.cursor])
				return nil
			}
		}
		// Hand the drain's tokens to the verdict that will judge it: a fruitless
		// drain is charged to the token-denominated quiet accumulator (CLA-343), and
		// the figure must be this drain's, not whatever total happened to be in
		// scope when judgeProgress runs.
		d.spent[d.cursor] = tokens
		// Handoff respawns consume iterations of the operator's ceiling too
		// (CLA-352) — see the comment above the accumulator.
		drains += handoffs
		// CLA-402: report the run's dead-phase tally with its denominator, so
		// the operator watching the daemon log sees the rate the run is
		// producing (the last line before the run ends IS the run total).
		d.logDeadTally()
		if err != nil {
			// Classify before acting (CLA-507). The run-wide causes are the ones
			// the poll loop already hard-stops on - a bad key or a missing project
			// selector is shared by every target, so no target can make progress
			// and sidelining would just defer the same exit. They never originate
			// in a drain today; the guard keeps that boundary explicit rather than
			// incidental. Context cancellation arrives as stop=true with err=nil,
			// so it needs no classification here.
			if errors.Is(err, backlog.ErrUnauthorized) || errors.Is(err, backlog.ErrProjectRequired) {
				return err
			}
			// Everything else out of drainPhases happened while trying to run
			// THIS target's session, so it sidelines THIS target: it sits out on
			// the escalating ladder while every other project drains on, and
			// self-heals once its harness succeeds again.
			d.sidelineTarget(d.cursor, drains, err)
			// CLA-562: the startup-failure escalation rides on top of the
			// sideline, mirroring the fleet trip. A startup failure is an ERROR
			// with no usage reported (no session ever ran); a dead phase is
			// err == nil, so one failure never ticks both counters. The sideline
			// stays for the first startupFailBound-1 attempts; only the Nth
			// escalates to a pause-and-raise. An error WITH usage proves a
			// session ran, so it breaks startup consecutiveness without tripping.
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
			// CLA-562: any drain that spawned (err == nil) proves the harness
			// starts, so it resets startup consecutiveness — including a drain
			// that ended in a dead-phase park or fleet trip, which still ran
			// sessions. The fleet counter is untouched here; its reset lives in
			// drainPhases (implement success only).
			d.startupFails[d.cursor] = 0
		}
		if stop {
			return nil
		}

		// CLA-564: every task boundary is a stop boundary, not just idle waits.
		// The cycle top reads these before the next poll, and this repeats the
		// read after the drain has fully unwound so the stop lands here —
		// before the next poll's side effects and the next spawn — rather than
		// depending on a single read a future fast path could bypass. A
		// file-exists read, never a wait, so the wired "loop straight back"
		// path below stays fast. HALT stops without being consumed (the
		// operator deletes it to resume); STOP is consumed on stop so a
		// restart is not stillborn.
		//
		// A cancelled context outranks both markers here exactly as at the
		// cycle top: falling through lets the top report the cancel, and the
		// exit sweep clears any pending STOP (CLA-491).
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

		// Blind mode has no counts to gate on, so it idles between sessions rather
		// than spinning. Note this is a PER-TASK pause now: the default prompt asks
		// for one task, so a blind run alternates task / idle / task, where the old
		// drain default spent one idle on the whole queue. An operator tuning
		// `idle_poll_interval` on a blind run is setting the gap between tasks.
		// In wired mode, loop straight back — the next cheap poll decides whether
		// more work appeared.
		if d.blind {
			log.Printf("idle — re-checking in %s", idle)
			if d.waitOrStop(runCtx, idle) {
				return nil
			}
		}
	}
}

// sweepPendingStop is Run's exit sweep (CLA-491): it removes a STOP marker the
// run never consumed, and logs the reason the run ended.
//
// A pending marker at exit means the operator dropped a soft stop against a LIVE
// daemon that then exited through a path with no STOP read (a non-retryable
// phase failure, a budget or zero-spend trip inside the drain, HALT beside it,
// a signal). The marker's intent is satisfied by ANY exit: the daemon is down
// either way, but leaving it would make the next start read a ghost and stop
// itself before doing anything. runErr carries how this run ended: an error
// return names it in the closing line (and so does every graceful path's own
// log line above), so the daemon log always says why.
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

// pickTarget polls every target's queue and chooses the one to drain next:
// round-robin over the targets with spawnable work, or over those not backed off
// when polling is blind. runContinue means nothing is spawnable and the run waited;
// runStop means the run ends, with err as its error.
func (d *Driver) pickTarget(runCtx context.Context, idle time.Duration) (Target, runNext, error) {
	var target Target
	if !d.blind {
		candidates := make([]bool, len(d.targets))
		anyCandidate := false
		for i := range d.targets {
			t := d.targets[i]
			sum, err := t.Poller.Poll(runCtx)
			switch {
			case errors.Is(err, backlog.ErrNotWired):
				log.Print("backlog polling not wired — blind mode: drain, then idle-poll by re-draining (wire backlog polling to gate on live counts cheaply)")
				d.blind = true
			case errors.Is(err, backlog.ErrUnauthorized):
				// A 401/403 means CLANKERBAR_API_KEY is bad — and the harness sessions
				// we'd spawn carry the SAME key, so blind-draining or idle-retrying just
				// burns dead work against a credential that won't self-heal. Hard-stop
				// LOUDLY with a non-nil error (non-zero exit — distinct from the graceful
				// nil stop STOP/budget return) so the operator fixes the key (CLA-132).
				log.Printf("%sbacklog auth failed (401/403) — check CLANKERBAR_API_KEY; stopping", d.prefix(i))
				return target, runStop, fmt.Errorf("backlog auth failed (401/403) — check CLANKERBAR_API_KEY: %w", err)
			case errors.Is(err, backlog.ErrProjectRequired):
				// A 400 project_required means the poll hit the LEGACY slug-less summary
				// route with an ACCOUNT-scoped key, which that route cannot bind to a
				// project. The remedy is a project selector, NOT abandoning the account
				// key (decision 2026-07-29): give the loop a slug to name in the path —
				// a `projects` list in the config, or an .mcp.json whose clankerbar URL
				// is /mcp/<slug> — or, for CI-style setups only, a project-scoped key.
				// Still a hard stop: the mismatch won't self-heal, and idle-retrying or
				// blind-draining would just burn doomed sessions (CLA-133/CLA-142).
				log.Printf("%sbacklog poll: the summary route needs a project selector — add a `projects` list to the config, or point the loop at an .mcp.json naming /mcp/<slug>; stopping", d.prefix(i))
				return target, runStop, fmt.Errorf("backlog poll needs a project selector (configure `projects` or an /mcp/<slug> .mcp.json; a project-scoped key also works for CI): %w", err)
			case err != nil:
				log.Printf("%sbacklog poll error: %v — retry in %s", d.prefix(i), err, idle)
			default:
				// The stored execution-config boundary (CLA-410): a moved
				// runConfigVersion — including the first poll, which is
				// "start of run" — refetches and re-overlays BEFORE the
				// spawn gate reads anything, so the drain that follows this
				// poll runs on the current document. Mid-session is
				// unreachable by construction: polls happen only here.
				d.checkRunConfigVersion(runCtx, i, &d.targets[i], sum.RunConfigVersion)
				// stale_claimable is reported beside claimable, not folded into it: an
				// operator watching the console has to be able to tell "spawning
				// because there is fresh work" from "spawning to recover an abandoned
				// branch", and before CLA-274 those were indistinguishable — the
				// second did not happen at all, and now that it does it must not look
				// like the first.
				log.Printf("%squeue: ready=%d claimable=%d stale_claimable=%d in_progress=%d open_questions=%d paused=%t (v%d)",
					d.prefix(i), sum.Ready, sum.Claimable, sum.StaleClaimable, sum.InProgress, sum.OpenQuestions, sum.Paused, sum.Version)
				// Console pause (CLA-76 plane / CLA-130 driver): the operator can pause
				// a run from the web console, PER PROJECT. Honour it BEFORE the spawn
				// gate so a paused project never gets a new session even when it has
				// work to spawn for — including a recovery session for abandoned work,
				// which a pause outranks like any other. Other projects keep draining.
				// Distinct from
				// STOP (which exits the instance) and from an empty queue. A pause
				// landing mid-drain is only seen here, between iterations, so it never
				// kills an in-flight session (exactly like STOP).
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
					d.judgeProgress(i, sum)
					// CLA-396: a target the fleet counter paused stays
					// paused until the operator answers the raised question
					// — the open-question count falls back to what it was at
					// the raise (the same "something was resolved" signal
					// judgeProgress uses for the quiet back-off).
					if d.fleetPaused[i] && sum.OpenQuestions <= d.fleetOpenQ[i] {
						d.fleetPaused[i] = false
						d.fleetRaised[i] = false
						log.Printf("%sfleet pause cleared — the operator answered the dead-phase question; resuming", d.prefix(i))
					}
					// CLA-562: the same resume signal for the startup-failure
					// pause - answering resumes, and the next startup failure
					// re-arms (the counter stays at the bound, so the next
					// startup failure trips again and raises fresh).
					if d.startupPaused[i] && sum.OpenQuestions <= d.startupOpenQ[i] {
						d.startupPaused[i] = false
						d.startupRaised[i] = false
						log.Printf("%sstartup-failure pause cleared - the operator answered the harness question; resuming", d.prefix(i))
					}
					if sum.Spawnable() && !d.backedOff(i) && !d.fleetPaused[i] && !d.startupPaused[i] {
						candidates[i] = true
						anyCandidate = true
					}
				}
			}
			// CLA-466: the presence beacon rides THIS poll — no new cadence. It
			// fires AFTER the switch, whose default arm writes the pause flags
			// fleetState reads (CLA-483); every poll beacons bar the two hard-stop
			// arms, which get their `stopping` from fleetShutdown. A session blocks
			// the poll, so the in-session renewal tick carries the same lap (CLA-771).
			d.beacon(i, t, d.fleetState(i))
			if d.blind {
				break
			}
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
		// Blind mode has no counts to route on - rotate across targets so every
		// queue still gets sessions. A target sitting out a harness failure
		// (CLA-507) is skipped here exactly as the wired gate skips it; the only
		// way skipUntil can be set in blind mode is that failure path, since
		// judgeProgress never runs without polls. Walking a full lap without
		// finding a drivable target means nothing is left, and that exit is the
		// honest one. The lap reads from the PRE-pass cursor: mutating d.cursor
		// per step would make later steps orbit the mutated value instead of
		// walking the ring.
		found := false
		orig := d.cursor
		for i := 1; i <= len(d.targets); i++ {
			idx := (orig + i) % len(d.targets)
			d.cursor = idx
			if !d.backedOff(idx) {
				found = true
				break
			}
		}
		if !found {
			return target, runStop, fmt.Errorf("every project's harness is failing - nothing left to drive, stopping:%s", d.sidelinedReport())
		}
		target = d.targets[d.cursor]
	}

	// There is work (or we're blind) — spend a session (or a phase sequence of
	// them, one task either way), retrying transient blips with backoff (a
	// fresh session reclaims any half-done task).
	return target, runFall, nil
}

// runNext says what Run does after choosing a target.
type runNext int

const (
	runFall     runNext = iota // drain the chosen target
	runContinue                // nothing to drain: take the next pass
	runStop                    // end the run
)
