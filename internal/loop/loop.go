// Package loop is the driver: a long-running daemon that gates each iteration on
// a cheap backlog read, spends a fresh harness session only when there is
// claimable work, and — crucially — stays alive and keeps polling when the queue
// is empty, so it reacts when questions are answered, items are promoted, or new
// work is filed. On a usage limit it pauses and polls for an early reset; on a
// transient blip it backs off and retries. All durable state lives in the backlog
// (over MCP), so a session killed mid-task is reclaimed by the next iteration.
package loop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
	"github.com/lecstor/clankerbar-cli/internal/salvage"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// Target is one backlog a Driver drives: a project's cheap poller plus where that
// project's drain sessions run (CLA-142). A single-project Driver has exactly one,
// unnamed target; a multi-project Driver has one per configured project.
type Target struct {
	// Name is the project slug, used to prefix log lines when there is more than
	// one target. Empty for the single default target.
	Name string

	// Poller is this project's cheap backlog read.
	Poller backlog.Poller

	// WorkDir is where this project's sessions run. Empty = the config's workdir.
	WorkDir string

	// MCPConfigPath points this project's sessions at its .mcp.json (the file
	// whose /mcp/<slug> URL selects the project). Empty = the config's path.
	MCPConfigPath string

	// Releaser hands back a claim a session left holding (CLA-242). Nil disables
	// the handback for this target, which costs only the reclaim an expiring lease
	// would have cost anyway — so a target without one degrades, it does not break.
	Releaser plane.Releaser
}

// Driver runs the loop for one harness against one or more backlogs.
type Driver struct {
	cfg     *config.Config
	h       harness.Adapter
	targets []Target
	paused  []bool // per-target console-pause state, to log transitions once
	cursor  int    // round-robin position over targets (last drained)
	state   *statedir.Dir
	blind   bool // no cheap backlog read available — drain then idle-poll

	// Per-target no-progress state. `claimable > 0` is a claim that work is
	// available, not that it can be DONE: a task can be claimable and still
	// unworkable — gated on an unanswered question, needing a toolchain the
	// harness may not run, blocked on something no session can resolve. The gate
	// cannot tell, so it spawns, the session correctly declines, and the operator
	// pays for the same report every cycle. One real run spent ten iterations
	// re-writing the same two questions. These back a repeatedly fruitless target
	// off instead.
	quiet     []int       // consecutive drains of this target that settled nothing
	baseline  []int       // settled count when this target's last drain began
	openQs    []int       // open-question count at this target's last poll
	pending   []bool      // a drain of this target is awaiting its progress verdict
	skipUntil []time.Time // a backed-off target is ineligible until this time

	// newVerifier builds the delivery checker for a workdir (CLA-253). A field so
	// tests can substitute one; production always gets internal/delivery.
	newVerifier func(workdir string) deliveryVerifier

	// newSalvager builds the stranded-work rescuer for a workdir (CLA-314). Same
	// shape and the same reason as newVerifier.
	newSalvager func(workdir string) workSalvager
}

// deliveryVerifier is the driver's view of internal/delivery, narrowed to the one
// call it makes.
type deliveryVerifier interface {
	Verify(ctx context.Context, c delivery.Claim) delivery.Report
}

// workSalvager is the driver's view of internal/salvage, narrowed the same way.
type workSalvager interface {
	Salvage(ctx context.Context, taskID, label string) salvage.Outcome
}

// New builds a single-project Driver — the original mode, driven entirely by the
// top-level config fields.
func New(cfg *config.Config, h harness.Adapter, poller backlog.Poller) *Driver {
	return NewMulti(cfg, h, []Target{{Poller: poller}})
}

// NewMulti builds a Driver over several targets (CLA-142): one loop instance, one
// account key, many project queues — round-robin over whichever have claimable work.
func NewMulti(cfg *config.Config, h harness.Adapter, targets []Target) *Driver {
	n := len(targets)
	return &Driver{
		cfg: cfg, h: h, targets: targets,
		paused:      make([]bool, n),
		quiet:       make([]int, n),
		baseline:    make([]int, n),
		openQs:      make([]int, n),
		pending:     make([]bool, n),
		skipUntil:   make([]time.Time, n),
		newVerifier: func(workdir string) deliveryVerifier { return delivery.New(workdir, "") },
		newSalvager: func(workdir string) workSalvager { return salvage.New(workdir, "") },
	}
}

// Run drives the daemon until STOP/HALT, a ceiling (max-iterations / budget), or
// context cancellation. An empty queue is NOT a stop condition — it idles and
// keeps polling. Returns nil on a graceful stop; an error only on an unexpected,
// non-retryable failure.
func (d *Driver) Run(ctx context.Context) error {
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
	if src := d.cfg.Source(); src != "" {
		log.Printf("config: %s", src)
	}
	idle := d.cfg.IdlePollInterval.OrDefault(60 * time.Second)
	log.Printf("driving %s; state in %s; idle poll every %s", d.h.Name(), d.state.Path(), idle)
	// Say it once, loudly, if the pre-CLA-259 directory is still sitting in the
	// workdir: an operator who touches STOP there would otherwise watch the loop
	// ignore it and conclude the stop switch is broken.
	if legacy := d.cfg.LegacyStateDir(); legacy != "" {
		log.Printf("note: %s is left over from before the state dir moved out of the workdir — markers there are IGNORED, and its old transcripts are still world-readable inside your repo; move or delete it", legacy)
	}

	start := time.Now()
	var totalTokens int
	var totalCost float64
	drains := 0

	for {
		if ctx.Err() != nil {
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
		if d.cfg.MaxIterations > 0 && drains >= d.cfg.MaxIterations {
			log.Printf("reached max-iterations (%d) — stopping", d.cfg.MaxIterations)
			return nil
		}
		if dim := d.cfg.Budget.ExceededBy(totalTokens, totalCost, time.Since(start)); dim != "" {
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
		var target Target
		if !d.blind {
			candidates := make([]bool, len(d.targets))
			anyCandidate := false
			for i := range d.targets {
				t := d.targets[i]
				sum, err := t.Poller.Poll(ctx)
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
					return fmt.Errorf("backlog auth failed (401/403) — check CLANKERBAR_API_KEY: %w", err)
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
					return fmt.Errorf("backlog poll needs a project selector (configure `projects` or an /mcp/<slug> .mcp.json; a project-scoped key also works for CI): %w", err)
				case err != nil:
					log.Printf("%sbacklog poll error: %v — retry in %s", d.prefix(i), err, idle)
				default:
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
						if sum.Spawnable() && !d.backedOff(i) {
							candidates[i] = true
							anyCandidate = true
						}
					}
				}
				if d.blind {
					break
				}
			}
			if !d.blind {
				if !anyCandidate {
					if d.waitOrStop(ctx, idle) {
						return nil
					}
					continue
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
			// Blind mode has no counts to route on — rotate across targets so every
			// queue still gets sessions.
			d.cursor = (d.cursor + 1) % len(d.targets)
			target = d.targets[d.cursor]
		}

		// There is work (or we're blind) — spend a session, retrying transient
		// blips with backoff (a fresh session reclaims any half-done task).
		drains++
		// The next poll of this target judges whether the drain settled anything.
		d.pending[d.cursor] = true
		tokens, cost, stop, err := d.drainWithRetries(ctx, drains, target,
			spend{start: start, tokens: totalTokens, cost: totalCost})
		// Count the spend BEFORE deciding what to do with the outcome: a drain that
		// stopped or failed still burned what it burned, and the accumulator is what
		// the next iteration's breaker and log line are measured against.
		totalTokens += tokens
		totalCost += cost
		if err != nil {
			return err
		}
		if stop {
			return nil
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
			if d.waitOrStop(ctx, idle) {
				return nil
			}
		}
	}
}

// spend is what the run had already consumed when this drain began, plus when it
// began — the budget breaker's whole view, handed down from Run.
//
// It is handed down because the ceilings are CUMULATIVE across the run while a
// drain only sees its own attempts, and because a drain can wait for hours
// (supervised wait, transient backoff) without ever returning to the breaker
// between drains. Measuring a mid-drain decision against this drain's spend alone
// would let a run walk past a ceiling it had already reached.
type spend struct {
	start  time.Time
	tokens int
	cost   float64
}

// drainWithRetries runs one drain to a clean finish, absorbing usage-limit pauses
// (supervised wait) and transient blips (exponential backoff) by re-running the
// SAME session — neither costs a drain count. Returns the tokens/cost consumed on
// a clean finish; stop=true if a STOP/cancel landed during a wait; err only on a
// genuine, non-retryable failure (or exhausted retries).
//
// prior carries the run's ceilings down into the drain, because the budget breaker
// in Run only runs BETWEEN drains and every wait in here happens inside one — see
// the check at the top of the loop, and waitPastBudget.
func (d *Driver) drainWithRetries(ctx context.Context, drainNum int, t Target, prior spend) (tokens int, cost float64, stop bool, err error) {
	retries := 0
	for {
		// The breaker, from inside the drain. Every path that loops back here has
		// just waited — a supervised wait on a usage limit, an exponential backoff
		// on a transient blip — and each of those waits used to be unbounded by
		// anything but the wall clock, which is only one of three dials and the one
		// an operator is least likely to have set. A drain that re-spawns a paid
		// session on a loop is the expensive failure this stops, whatever put it
		// there (CLA-258).
		if dim := d.cfg.Budget.ExceededBy(prior.tokens+tokens, prior.cost+cost, time.Since(prior.start)); dim != "" {
			log.Printf("iteration %d: budget reached mid-drain: %s — stopping (tokens=%d cost=$%.2f elapsed=%s)",
				drainNum, dim, prior.tokens+tokens, prior.cost+cost, time.Since(prior.start).Round(time.Second))
			return tokens, cost, true, nil
		}

		// Each attempt streams live to the terminal and to its own logfile. The name
		// carries the drain number and attempt counter as well as the timestamp: two
		// attempts in the same second (a sub-second backoff) would otherwise share a
		// name, and a name already taken is REFUSED rather than truncated.
		//
		// The random tail is not decoration. This log carries the session's whole
		// output — prompts, tool arguments, tool output, occasionally a token — and
		// the rest of the name is derivable to within a second by the session that
		// produced it. Unguessable names mean a session that CAN write to the state
		// dir (an operator who pointed state_dir back inside the workdir) still
		// cannot pre-plant a decoy at the name we are about to use. statedir.Create
		// refuses an existing path either way; this stops it turning into a denial
		// of the log itself.
		inv := d.invocation(t, false)
		logName := fmt.Sprintf("iteration-%s-d%d-a%d-%s.log",
			time.Now().Format("20060102-150405"), drainNum, retries, randomTail())
		f, ferr := d.state.Create(logName)
		logPath := filepath.Join(d.state.Path(), logName)
		if ferr == nil {
			inv.Console = io.MultiWriter(os.Stderr, f)
		} else {
			inv.Console = os.Stderr
			log.Printf("could not open iteration log %s: %v", logPath, ferr)
		}
		if retries == 0 {
			log.Printf("iteration %d %s— spawning %s (log: %s)", drainNum, labelOf(t), d.h.Name(), logPath)
		} else {
			log.Printf("iteration %d %s— retry %d, spawning %s (log: %s)", drainNum, labelOf(t), retries, d.h.Name(), logPath)
		}

		res, ierr := d.h.Invoke(ctx, inv)
		if f != nil {
			_ = f.Close()
		}
		// Rescue whatever the session left uncommitted, FIRST — before the handback,
		// because a successful salvage changes what the handback should do: a task
		// with a branch recorded on it is no longer safe to release (CLA-314).
		d.salvageStrandedWork(ctx, t, &res)
		// Hand back anything the session was still holding, BEFORE deciding what to
		// do next — every branch below either waits, retries or returns, and all of
		// them leave the lease unattended. Above the ierr check too: Invoke returns
		// a fully parsed Result alongside a Wait failure, so a claim observed on
		// that stream is real and must not be dropped just because the process died
		// untidily. A launch failure yields a zero Result, which releases nothing.
		d.releaseHeldClaim(ctx, t, res)
		// Then check what it said it delivered. After the handback, because a dead
		// lease is time-sensitive and a git check is not; on every exit, because a
		// session that pushed nothing and died is exactly as likely to have recorded
		// a branch as one that finished cleanly.
		d.verifyDeliveries(ctx, t, res)

		if ierr != nil {
			if ctx.Err() != nil {
				return tokens, cost, true, nil
			}
			// Couldn't launch the harness at all (bad PATH/flags/env) — not a blip.
			return tokens, cost, false, fmt.Errorf("invoke %s: %w", d.h.Name(), ierr)
		}

		// A session whose stream could not be read whole (CLA-262). Everything below
		// this line reads a figure parsed out of that stream, so counting the spend,
		// classifying the exit or retrying on the strength of it are three ways to
		// make a confident decision on data with a hole in it.
		if res.Untrusted != "" {
			return d.endUntrustedDrain(drainNum, res, tokens, cost)
		}

		// Count THIS attempt's spend toward the budget breaker regardless of how it
		// ends — usage-limit, transient, stop, or clean. A failed/retried attempt
		// still burned tokens, and a "leave headroom" breaker must err toward seeing
		// real spend, not under-counting it. Each attempt is a distinct session, so
		// summing per attempt (and returning the accumulator, not the final res)
		// counts every session exactly once.
		tokens += res.Tokens
		cost += res.CostUSD

		// A usage limit. A rolling-window subscription cap is waited out and the
		// session re-run; a hard budget/credit exhaustion (Stop) has no reset to
		// poll for, so the run stops cleanly and the operator resumes it once
		// they've topped up.
		if lim := d.h.DetectLimit(res); lim.Limited {
			if lim.Stop {
				log.Printf("iteration %d stopped: %s — no reset to wait for, stopping (resume once resolved)",
					drainNum, limitReason(lim))
				return tokens, cost, true, nil
			}
			log.Printf("iteration %d hit a usage limit", drainNum)
			// Waiting out a reset that lands past the wall-clock ceiling buys
			// nothing: the budget check runs between drains, so the loop would
			// sleep through the window, run one session on the freshly reset quota
			// and stop on the very next check — having spent the night waiting for
			// headroom it then declines to use. Stop now and say when the quota
			// returns, so the operator can start a fresh run against it.
			remaining, bounded := d.cfg.Budget.Remaining(time.Since(prior.start))
			if until, over := waitPastBudget(lim.ResetAt, remaining, bounded); over {
				log.Printf("iteration %d: the limit resets %s, in %s — more than the %s left of this run's ceiling; stopping now rather than waiting, so start a fresh run after the reset",
					drainNum, until.Format("Mon 15:04"), time.Until(until).Round(time.Minute), remaining.Round(time.Minute))
				return tokens, cost, true, nil
			}
			// The wait's own probe spend lands in the SAME accumulator as the
			// sessions', so it reaches the breaker at the top of this loop, the one in
			// Run between drains, and the iteration's cost line — rather than a
			// separate figure nothing is measured against (CLA-287).
			ptokens, pcost, pstop := d.supervisedWait(ctx, lim, t, spend{
				start:  prior.start,
				tokens: prior.tokens + tokens,
				cost:   prior.cost + cost,
			})
			tokens += ptokens
			cost += pcost
			if pstop {
				return tokens, cost, true, nil
			}
			continue
		}

		if res.ExitCode == 0 {
			log.Printf("iteration %d done (tokens=%d cost=$%.4f)", drainNum, tokens, cost)
			return tokens, cost, false, nil
		}

		// Non-zero exit, not the usage cap: a transient server/network blip backs
		// off and retries; anything else is a genuine failure and stops.
		if d.h.IsTransient(res) {
			retries++
			if d.cfg.MaxRetries > 0 && retries > d.cfg.MaxRetries {
				return tokens, cost, false, fmt.Errorf(
					"iteration %d: transient failures persisted after %d retries (check https://status.claude.com; rerun to resume)",
					drainNum, d.cfg.MaxRetries)
			}
			wait := d.backoff(retries)
			log.Printf("iteration %d transient failure (exit %d) — %s in %s (a fresh session reclaims any half-done task)",
				drainNum, res.ExitCode, retryLabel(retries, d.cfg.MaxRetries), wait)
			if d.waitOrStop(ctx, wait) {
				return tokens, cost, true, nil
			}
			continue
		}

		// Stopping here ends the whole run, so say WHAT was judged non-retryable.
		// Without it the operator gets "exited 1 (non-retryable)" and no way to
		// tell a genuine failure from a blip the classifier merely does not know
		// yet — which is the one thing they need in order to report the gap. The
		// text is the harness's own diagnostic scope, never the raw stream, so
		// the agent's narration is not quoted back at them (CLA-258).
		return tokens, cost, false, fmt.Errorf("iteration %d: %s exited %d (non-retryable) — stopping%s%s",
			drainNum, d.h.Name(), res.ExitCode, failureDetail(d.h.Diagnostic(res)), droppedNote(res.OutputDropped))
	}
}

// endUntrustedDrain closes out a drain whose session output could not be read
// whole, without acting on anything that stream said (CLA-262).
//
// # Why the spend is not counted
//
// It is not that the figure is inconvenient — it is that it is not a measurement.
// The `result` event carrying a claude session's whole tokens-and-cost total is
// the LAST thing on the stream, so a stream cut short reports zero for a session
// that may have cost hundreds of dollars, and a partial sum from an adapter that
// accumulates per step is a lower bound of unknown looseness. Adding either to the
// accumulator would put a made-up number in front of the breaker and in the
// iteration's cost line.
//
// # Why a spend ceiling then STOPS the run
//
// Because the ceiling is a promise, and this is the driver saying it can no longer
// keep it. An operator who set `max_tokens` or `max_cost_usd` has asked to be
// protected from exactly the shape this is: sessions whose cost nothing can see.
// One unaccounted session is survivable; a night of them against an inert ceiling
// is the failure CLA-287 and CLA-258 were both about. Stopping is clean, loud, and
// resumable — the operator restarts the run once they know why.
//
// With no spend ceiling set there is nothing to break: the wall clock does not
// depend on anything the child said, so the run carries on and the no-progress
// back-off is what bounds a target that keeps producing these.
func (d *Driver) endUntrustedDrain(drainNum int, res harness.Result, tokens int, cost float64) (int, float64, bool, error) {
	log.Printf("iteration %d UNTRUSTED — %s", drainNum, res.Untrusted)
	log.Printf("iteration %d: not counting this session's parsed spend (tokens=%d cost=$%.4f — a floor, not a total), not classifying its exit (%d), and not handing back any claim it appeared to hold",
		drainNum, res.Tokens, res.CostUSD, res.ExitCode)
	if !d.cfg.Budget.CountsSpend() {
		return tokens, cost, false, nil
	}
	log.Printf("iteration %d: stopping — a token/cost ceiling is set and this session's real spend cannot be known, so the ceiling can no longer be honoured. Check the iteration log, then rerun to resume.", drainNum)
	return tokens, cost, true, nil
}

// salvageStrandedWork commits and pushes the work a dead session left in its
// worktree, and records the branch on the task so another machine can pick it up
// (CLA-314).
//
// # Why it runs on EVERY ending, not only on a usage limit
//
// The worktree is what is being rescued, and it looks identical whether the
// session was killed by a limit, died in a crash, or simply stopped with the work
// unfinished — while the cost of losing it is the same in all three. The limit,
// besides, is detected by parsing the stream AFTER the fact, so gating on it
// would skip exactly the sessions whose stream could not be read: the ones we
// know least about, and the likeliest to have stranded something. Running it when
// it was not needed costs nothing — a clean worktree commits nothing, pushes
// nothing and records nothing.
//
// # Why it is safe on the untrusted path
//
// CLA-262 forbids acting on a truncated stream's CLAIM-STATE: a settle we never
// saw may be in the bytes that never arrived, so handing the task back could post
// `ready` over work already in review. This does something different in kind.
// Recording a branch carries no status, so it cannot move a task, clear a holder
// or revert a review — it is additive, and its worst case on a task that really
// did reach review is that the branch field names the branch the work is on
// anyway. What it acts on is the local worktree, which no truncation can
// misreport. A claim we SAW settle is skipped entirely: Held() is false, and this
// is a rescue, not a tidy-up.
//
// # What a success does to the handback
//
// Claim.HasWIP is set only when the plane ACCEPTED the branch, and that is what
// makes the claim non-releasable below: the lease is left to expire so the task
// stays a takeover and the hand-off survives. If the record failed, the task has
// no branch on it, releasing to `ready` is still the better move, and the old
// path runs unchanged.
func (d *Driver) salvageStrandedWork(ctx context.Context, t Target, res *harness.Result) {
	if d.newSalvager == nil || !res.Claim.Held() {
		return
	}
	// Detached from ctx, like the handback and unlike the delivery checks: a
	// cancelled run (Ctrl-C, SIGTERM) is precisely a session killed mid-task, so
	// this is when work goes missing, not when it can be skipped. Bounded, so a
	// wedged git or an unreachable remote cannot hold up the shutdown.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), salvageTimeout)
	defer cancel()

	out := d.newSalvager(d.invocation(t, false).WorkDir).Salvage(sctx, res.Claim.TaskID, res.Claim.Ref)
	label := claimLabel(res.Claim)
	switch out.Status {
	case salvage.Nothing:
		// Silent on the ordinary case. A session that committed its own work leaves
		// a clean tree, and a log line every iteration saying so would bury the ones
		// that matter.
		if out.Worktree != "" {
			log.Printf("%snothing to salvage for %s: %s", labelOf(t), label, out.Detail)
		}
		return
	case salvage.Refused:
		log.Printf("%sSTRANDED WORK LEFT AS IS — %s: %s", labelOf(t), label, out.Detail)
		return
	case salvage.Failed:
		log.Printf("%sSALVAGE FAILED — %s: %s", labelOf(t), label, out.Detail)
		return
	}

	log.Printf("%ssalvaged %s: %s", labelOf(t), label, out.Detail)
	rec, ok := t.Releaser.(plane.Recorder)
	if !ok {
		log.Printf("%sthe branch %s is pushed but cannot be recorded on %s (no plane writes configured) — a clanker on this machine can still find it by name",
			labelOf(t), out.Branch, label)
		return
	}
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer rcancel()

	if err := rec.RecordBranch(rctx, res.Claim.TaskID, res.Claim.RunID, out.Branch); err != nil {
		if !errors.Is(err, plane.ErrNotWired) {
			log.Printf("%scould not record branch %s on %s: %v — the work IS pushed, so fetch that branch by name rather than starting again",
				labelOf(t), out.Branch, label, err)
		}
		return
	}
	// Only now. HasWIP is a statement about the TASK, not about the disk: it means
	// the next clanker will be told there is work to fetch.
	res.Claim.HasWIP = true
	log.Printf("%srecorded %s as the hand-off branch for %s — the next clanker takes it over instead of starting again",
		labelOf(t), out.Branch, label)
}

// salvageTimeout bounds one rescue: a status read, a commit, and a push over the
// network. Longer than the delivery checks' minute because this one uploads.
const salvageTimeout = 3 * time.Minute

// claimLabel is the task's most human-readable identifier.
func claimLabel(c harness.Claim) string {
	if c.Ref != "" {
		return c.Ref
	}
	return c.TaskID
}

// releaseHeldClaim hands a task the session was still holding back to the queue,
// instead of leaving its lease to run out with nobody heartbeating it.
//
// It runs on EVERY exit from a session, deliberately. A usage limit, a transient
// blip, an outright failure and a clean finish with the work unfinished all leave
// the same dead lease behind — and the usage-limit case is the worst of the four,
// because the driver then goes to sleep for as long as the reset takes. One real
// run slept ninety minutes on a live claim and cost the task a reclaim it had not
// earned; the budget is two before the plane parks the task for the operator.
//
// A claim with pushed work is left alone; Claim.Releasable says why.
func (d *Driver) releaseHeldClaim(ctx context.Context, t Target, res harness.Result) {
	// A stream read only in part tells us which calls we SAW, never which the
	// session made: the settle that released the task may simply be in the bytes
	// that never arrived. Handing the task back on that reading posts `ready` over
	// work already in review, which is the one outcome worse than a lease expiring
	// (CLA-262). Let it expire instead — the plane's own sweep does the right thing
	// with it, and keeps the takeover hand-off if there is one.
	if res.Untrusted != "" {
		if res.Claim.Held() {
			log.Printf("%ssession appeared to end holding %s, but its output could not be read whole — leaving the lease to expire rather than releasing a task that may already be settled",
				labelOf(t), res.Claim.TaskID)
		}
		return
	}
	if !res.Claim.Held() {
		return
	}
	if !res.Claim.Releasable() {
		log.Printf("%ssession ended holding %s, which has pushed work — leaving the lease to expire so the takeover hand-off survives",
			labelOf(t), res.Claim.TaskID)
		return
	}
	if t.Releaser == nil {
		return
	}
	// Detach from ctx: a cancelled run (Ctrl-C, SIGTERM) is exactly when a claim
	// would otherwise be abandoned, so the handback has to outlive the signal that
	// prompted it. Bounded, so a wedged plane cannot hold up the shutdown.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	if err := t.Releaser.Release(rctx, res.Claim.TaskID, res.Claim.RunID); err != nil {
		if !errors.Is(err, plane.ErrNotWired) {
			log.Printf("%scould not hand %s back: %v — its lease will expire instead", labelOf(t), res.Claim.TaskID, err)
		}
		return
	}
	log.Printf("%shanded %s back to the queue (the session ended still holding it)", labelOf(t), res.Claim.TaskID)
}

// verifyDeliveries checks, against local git, what the session told the plane it
// delivered — and says so out loud (CLA-253).
//
// The plane is a pipe: it stores a recorded branch and a declared merge and takes
// the clanker at its word, because it has no access to a git host and, by the
// standing decision behind this, is not being given any. The driver is already
// local and already in the tree, so it is the one place the claim can simply be
// looked at.
//
// # Warn, do not refuse
//
// A failed check is logged loudly and names what is unpushed or unmerged. It does
// NOT override the session's report, and this is a deliberate first cut rather
// than an oversight: refusing would mean the driver reverting a status a session
// chose, on the strength of a check that can be wrong about which tree it is
// looking at. Loud logging is the floor, it is cheap, and it is reversible.
// Recorded as a decision on CLA-253.
//
// # Fail open
//
// A check that could not run reports "could not verify" and the run carries on.
// Blocking a legitimate closure because the tree could not be found would be worse
// than the gap this replaces — and an Unknown is never allowed to read as a pass:
// the attestation below is written only for a check that actually ran.
func (d *Driver) verifyDeliveries(ctx context.Context, t Target, res harness.Result) {
	if len(res.Reports) == 0 || d.newVerifier == nil {
		return
	}
	// Unlike the handback, this is NOT worth outliving a Ctrl-C. The handback races
	// a lease that is already ticking; a delivery claim will still be wrong in the
	// morning, and the checks reach the network, so insisting on them would add a
	// visible stall to every interactive shutdown.
	if ctx.Err() != nil {
		return
	}

	v := d.newVerifier(d.invocation(t, false).WorkDir)
	for _, rep := range res.Reports {
		claim := delivery.Claim{Label: rep.Label(), Branch: rep.Branch}
		if rep.ClaimsMerge() {
			claim.Commit, claim.IntegrationBranch = rep.Commit, rep.IntegrationBranch
		}
		// Detached and bounded PER REPORT: a shared deadline would silently degrade
		// every claim after the first slow one to "cannot check". Detached because a
		// cancel arriving mid-check should not turn a real answer into a half one —
		// the guard above is what makes cancellation prompt.
		vctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryCheckTimeout)
		out := v.Verify(vctx, claim)
		cancel()

		for _, c := range out.Checks {
			switch c.Status {
			case delivery.Fail:
				log.Printf("%sDELIVERY UNVERIFIED — %s: %s", labelOf(t), rep.Label(), c.Detail)
			case delivery.Unknown:
				log.Printf("%scould not verify %s: %s — carrying on", labelOf(t), rep.Label(), c.Detail)
			default:
				log.Printf("%sverified %s: %s", labelOf(t), rep.Label(), c.Detail)
			}
		}
		d.attestMerge(ctx, t, rep, out)
	}
}

// deliveryCheckTimeout bounds one session's worth of git checks, including the
// `ls-remote` that goes over the network.
const deliveryCheckTimeout = 60 * time.Second

// attestMerge writes the driver's verdict onto the task's delivery record, so the
// answer survives the log. Only for a check that RAN: `mergeVerified` means "I ran
// the ancestor check and this is what it said", and writing it off the back of a
// check that could not run would be the same false attestation this exists to
// catch. Best-effort throughout — an unreachable or unwired plane costs the record,
// not the run, and the loud log above already carries the finding.
func (d *Driver) attestMerge(ctx context.Context, t Target, rep harness.Report, out delivery.Report) {
	verified, ran := out.MergeVerified()
	if !ran || rep.TaskID == "" || rep.RunID == "" {
		return
	}
	a, ok := t.Releaser.(plane.Attester)
	if !ok {
		return
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	if err := a.AttestMergeVerified(actx, rep.TaskID, rep.RunID, plane.Delivery{
		Commit:            rep.Commit,
		IntegrationBranch: rep.IntegrationBranch,
		PR:                rep.PR,
	}, verified); err != nil {
		if !errors.Is(err, plane.ErrNotWired) {
			log.Printf("%scould not record the merge check for %s: %v", labelOf(t), rep.Label(), err)
		}
		return
	}
	log.Printf("%srecorded delivery.mergeVerified=%t for %s", labelOf(t), verified, rep.Label())
}

// quietBackoff is how long a target sits out after `quiet` consecutive drains
// that settled nothing: 15m, 30m, 1h, then 2h. It never reaches "never" — the
// blocker is usually an operator answering a question, and a target that can
// never come back would need a restart to notice.
func quietBackoff(quiet int) time.Duration {
	const (
		base = 15 * time.Minute
		cap_ = 2 * time.Hour
	)
	d := base
	for i := quietThreshold; i < quiet; i++ {
		if d *= 2; d >= cap_ {
			return cap_
		}
	}
	return d
}

// quietThreshold is how many fruitless drains it takes to back a target off.
//
// Three, not one or two. A single fruitless drain is ordinary, and so is a
// second: a genuinely large task can span several sessions before anything
// reaches a reviewer, and backing off then would punish exactly the deep work
// this loop exists to do. Three consecutive sessions that settle nothing is a
// different claim — and against the run this was written for, which repeated the
// same no-op ten times, a threshold of three still saves seven of them.
const quietThreshold = 3

// judgeProgress reads this poll as the verdict on the target's last drain, and
// backs the target off once it has spent enough sessions achieving nothing.
//
// Progress is `Settled()` rising — work reaching a reviewer or finishing. A drain
// that only recorded why it could not proceed bumps `version` but settles
// nothing, which is exactly the run this exists to stop repeating.
//
// An ANSWERED QUESTION is the one kind of progress `Settled()` cannot see, and it
// is the very thing the back-off's own message asks the operator to go and do
// (CLA-248). Answering a BLOCKING question takes its task `blocked -> ready` on
// the plane, and a non-blocking one never moved its task at all; either way
// neither `InReview` nor `Done` shifts, so a falling `OpenQuestions` is the only
// trace of it a poll can read. Both signals are watched, and both baselines move
// on every poll: the verdict is always about the change since the last look.
func (d *Driver) judgeProgress(i int, sum backlog.Summary) {
	settled := sum.Settled()
	openQs, wasOpen := sum.OpenQuestions, d.openQs[i]

	// IDLE IS NOT FRUITLESS. `quiet` counts consecutive DRAINS that settled
	// nothing, and a poll with nothing claimable is not a drain — the gate below
	// will not spend a session, so this target has not failed at anything. Without
	// this the count is immortal: it survives the blocker being parked (`parked`
	// is not in Settled(), so nothing else clears it), an arbitrarily long idle
	// stretch, and a complete turnover of the queue. A task filed a week later
	// then inherits a back-off it did nothing to earn and serves the escalated
	// 2h wait on its FIRST fruitless drain — defeating quietThreshold, which
	// exists precisely so a large task spanning sessions is not punished.
	//
	// ONLY WITH NO VERDICT OUTSTANDING, and that condition is load-bearing rather
	// than defensive. `claimable == 0` does not only mean "the queue is empty" — it
	// also means "everything ready is claimed right now", and the poll immediately
	// after a drain is exactly when that is most likely, because a session that
	// ends still holding a task it pushed work on is deliberately NOT handed back
	// (see releaseHeldClaim). Forgetting the count there would not merely decline
	// to charge the drain, it would CANCEL the verdict on one that already ran — so
	// a target whose every session ends holding its task would alternate spawn /
	// forget forever and never back off at all. Requiring the verdict to be settled
	// first costs one extra poll of an idle stretch and nothing else: the drain is
	// judged, and the NEXT poll — still idle — does the forgetting.
	//
	// Clearing skipUntil too, and not only the count: an in-flight wait is moot
	// while there is nothing to spawn for, and leaving it set would make the next
	// task filed sit out the tail of a wait somebody else's blocker earned.
	//
	// RESIDUAL, stated rather than implied. A target whose only claimable task is
	// held ELSEWHERE for more than one poll — another driver on the same project,
	// an agent working it by hand — reads as idle for that stretch, so it forgets
	// and lifts. Such a target backs off at the base 15m every quietThreshold
	// fruitless drains instead of escalating toward 2h: the breaker still bounds
	// the spend, it just stops tightening. That is accepted rather than narrowed
	// away, because the two obvious narrowings are both worse. Requiring
	// `in_progress == 0` reinstates the immortal count for any project holding
	// abandoned WIP (CLA-274), which is precisely the population most likely to
	// have some. Requiring N consecutive idle polls does not discriminate at all:
	// a task held elsewhere is held for a lease, which is many poll intervals.
	// Nothing in a summary count separates "quiet because there is no work" from
	// "quiet because somebody else has it" — only whether WE are still spending
	// sessions does, and the guard above is the part of that signal worth having.
	//
	// This cannot spin. A drain needs a poll where the target IS spawnable, so the
	// reset never applies to the poll that authorises a session; the count climbs
	// from zero the ordinary way, and reaching the threshold again takes
	// quietThreshold fresh fruitless drains.
	if !sum.Spawnable() && !d.pending[i] {
		if d.quiet[i] > 0 {
			log.Printf("%snothing to spawn for — idle, not fruitless; forgetting %d drain(s) that settled nothing and clearing any back-off", d.prefix(i), d.quiet[i])
		}
		d.quiet[i], d.skipUntil[i] = 0, time.Time{}
		d.baseline[i], d.openQs[i] = settled, openQs
		return
	}

	if !d.pending[i] {
		// No drain outstanding. Progress can still arrive from elsewhere — the
		// operator merging a PR, another machine's loop — and it means whatever was
		// stuck may now be unstuck, so let the target back in immediately.
		//
		// Note what the merge case actually rides on, because it is not obvious and
		// nothing here can pin it: the plane approves a task back to `ready` BEFORE
		// its PR is merged and it is marked `done`. The first write DROPS `Settled()`
		// by one and lowers the baseline with it here, so the later `done` reads as a
		// rise. It is that two-step, not the merge, that this branch sees, and only
		// because a poll lands between the two writes - which an idle poll of a minute
		// against a human-paced approve-then-merge ordinarily does. `in_review -> done`
		// with no poll in between is invisible, exactly like `blocked -> ready`.
		if d.quiet[i] > 0 {
			switch {
			case settled > d.baseline[i]:
				log.Printf("%sprogress from elsewhere — clearing the no-progress back-off", d.prefix(i))
				d.quiet[i], d.skipUntil[i] = 0, time.Time{}
			case openQs < wasOpen:
				log.Printf("%sopen questions fell %d -> %d, so something was resolved: clearing the no-progress back-off",
					d.prefix(i), wasOpen, openQs)
				d.quiet[i], d.skipUntil[i] = 0, time.Time{}
			}
		}
		d.baseline[i], d.openQs[i] = settled, openQs
		return
	}

	d.pending[i] = false
	if settled > d.baseline[i] {
		d.quiet[i] = 0
		d.baseline[i], d.openQs[i] = settled, openQs
		return
	}

	// The strike is earned whatever else moved: a session that answered its own
	// question has not thereby delivered anything, and the drain is being judged on
	// what it SETTLED. But an answer that landed while the drain was running is the
	// operator doing the very thing the back-off message asked for, and this poll is
	// the only one that can ever see it: the baseline advances just below, so a fall
	// consumed here is gone rather than deferred. Take the strike, skip the sit-out.
	// The target gets one immediate retry against the answer, and if that settles
	// nothing too, the ladder is already a rung higher for it.
	answered := openQs < wasOpen
	d.quiet[i]++
	d.baseline[i], d.openQs[i] = settled, openQs
	if d.quiet[i] < quietThreshold {
		return
	}
	if answered {
		log.Printf("%s%d consecutive sessions settled nothing, but open questions fell %d -> %d while the last one ran - retrying once before backing off",
			d.prefix(i), d.quiet[i], wasOpen, openQs)
		return
	}
	wait := quietBackoff(d.quiet[i])
	d.skipUntil[i] = time.Now().Add(wait)
	// Name the RIGHT cause. The original sentence asserts there is claimable work
	// and lists the reasons a ready task resists being finished — all of which are
	// wrong, and misdirecting, when the only thing that opened the gate was an
	// abandoned branch (CLA-274). Sending an operator to look for an unanswered
	// question when the real subject is stranded WIP costs them the diagnosis.
	if sum.Claimable == 0 && sum.StaleClaimable > 0 {
		log.Printf("%s%d consecutive sessions settled nothing — backing off for %s. Nothing is READY; the sessions were spawned to recover %d abandoned branch(es), and are not finishing them. Check the last iteration log, and the branch itself.",
			d.prefix(i), d.quiet[i], wait, sum.StaleClaimable)
		return
	}
	log.Printf("%s%d consecutive sessions settled nothing — backing off for %s. The queue says there is claimable work, so something is stopping it being DONE: an unanswered question, a gate, a toolchain the session cannot run. Check the last iteration log.",
		d.prefix(i), d.quiet[i], wait)
}

// backedOff reports whether a target is currently sitting out, logging the moment
// it becomes eligible again so the silence has a visible end.
func (d *Driver) backedOff(i int) bool {
	if d.skipUntil[i].IsZero() {
		return false
	}
	if time.Now().Before(d.skipUntil[i]) {
		return true
	}
	d.skipUntil[i] = time.Time{}
	log.Printf("%sback-off elapsed — trying again", d.prefix(i))
	return false
}

// sleepStall reports how much real time a wait lost to system suspension, and
// whether that is worth saying out loud.
//
// The threshold is absolute rather than proportional on purpose: five minutes of
// unexplained wall clock is worth one log line whether it interrupted a one-minute
// idle poll or a thirty-minute limit wait, and a proportional rule would either
// spam the short waits or stay silent through the long ones. Small overshoots are
// ordinary scheduling and say nothing.
func sleepStall(intended, wall time.Duration) (lost time.Duration, stalled bool) {
	const floor = 5 * time.Minute
	if lost = wall - intended; lost < floor {
		return 0, false
	}
	return lost, true
}

// waitPastBudget reports whether waiting for resetAt would cost more time than
// the run has left. An unknown reset (zero) is waited out as before, because the
// supervised wait polls for an EARLY lift and the limit may clear long before any
// stated time; with no ceiling set there is nothing to be past.
//
// remaining comes from the breaker's own clock — MONOTONIC elapsed, which does
// not advance while the machine is suspended. The wait is measured on the wall
// clock, because a quota reset is a wall-clock event. Those are the right two
// clocks for this question and the mixture is deliberate.
//
// The earlier form compared the reset against a wall-clock DEADLINE while the
// breaker counted monotonic, so the two diverged by exactly the time the machine
// spent asleep — the very thing this file's sleep detection exists to report. An
// eight-hour run that lost five hours to a closed lid would stop with seven hours
// of budget unspent, and tell the operator it had reached a ceiling it had not.
func waitPastBudget(resetAt time.Time, remaining time.Duration, bounded bool) (time.Time, bool) {
	if resetAt.IsZero() || !bounded {
		return time.Time{}, false
	}
	return resetAt, time.Until(resetAt) > remaining
}

// supervisedWait pauses on a usage limit, then polls for an early reset rather
// than sleeping blindly to the stated reset. Returns the probe spend it incurred,
// and stop=true if the loop should stop (STOP marker, context cancel, or a budget
// ceiling reached during the wait).
//
// sofar is everything the run has spent, including this drain's attempts. The
// breaker is consulted HERE and not only by the caller, because this loop does not
// return to the caller while the limit persists: it waits, probes, and waits again
// for as long as it takes. Without this, the one shape the task names — a drain
// stuck in a supervised wait — was the one shape no ceiling could end, and it is
// the shape codex always produces, since it states no reset for waitPastBudget to
// measure (CLA-258).
//
// The probes are themselves paid sessions, so their spend is added to `sofar` as it
// accrues and returned to the caller for the run's accumulator. A breaker that can
// only be tripped by spend it can SEE was blind to exactly the spend this loop
// generates: a week-long cap polled every 30 minutes is ~336 unaccounted sessions,
// and this is the one loop with no other way out (CLA-287).
func (d *Driver) supervisedWait(ctx context.Context, lim harness.Limit, t Target, sofar spend) (tokens int, cost float64, stop bool) {
	interval := d.cfg.PollInterval.OrDefault(30 * time.Minute)
	log.Printf("paused%s — probing every %s for an early reset", resetSuffix(lim.ResetAt), interval)

	for {
		if dim := d.cfg.Budget.ExceededBy(sofar.tokens+tokens, sofar.cost+cost, time.Since(sofar.start)); dim != "" {
			log.Printf("budget reached while paused: %s — stopping rather than waiting out the limit (tokens=%d cost=$%.2f elapsed=%s)",
				dim, sofar.tokens+tokens, sofar.cost+cost, time.Since(sofar.start).Round(time.Second))
			return tokens, cost, true
		}
		if d.waitOrStop(ctx, interval) {
			return tokens, cost, true
		}
		if !lim.ResetAt.IsZero() && time.Now().After(lim.ResetAt) {
			log.Print("stated reset passed — resuming")
			return tokens, cost, false
		}
		got, err := d.h.Probe(ctx, d.invocation(t, true))
		// Count the probe BEFORE reading its verdict, and before the error branch: a
		// probe that failed still spawned the harness and still spent. Every path out
		// of here — resume, stop, or another lap — carries it.
		tokens += got.Tokens
		cost += got.CostUSD
		if err != nil {
			if ctx.Err() != nil {
				return tokens, cost, true
			}
			// A probe whose own output could not be read is not a blip that clears
			// itself: it reports no verdict AND no spend, so a wait that keeps polling
			// on it re-spawns a paid session every interval against a ceiling that can
			// never see the cost — the one shape this loop's breaker exists to end
			// (CLA-262, and the same reasoning as endUntrustedDrain). With no spend
			// ceiling there is nothing to protect, so it waits on as before.
			if errors.Is(err, harness.ErrUntrusted) && d.cfg.Budget.CountsSpend() {
				log.Printf("paused, and the probe's own output cannot be read (%v) — stopping rather than polling on: its spend cannot be counted, so a token/cost ceiling can no longer be honoured. Rerun after the reset.", err)
				return tokens, cost, true
			}
			log.Printf("probe error: %v — will retry next interval", err)
			continue
		}
		if !got.Limit.Limited {
			log.Print("limit lifted — resuming")
			return tokens, cost, false
		}
		log.Print("still limited — continuing to wait")
	}
}

// backoff is the exponential delay before transient retry n (1-based): 30s, 60s,
// 120s, ..., capped at RetryCap. Computed by doubling to avoid shift overflow.
func (d *Driver) backoff(n int) time.Duration {
	ceil := d.cfg.RetryCap.OrDefault(300 * time.Second)
	b := 30 * time.Second
	for i := 1; i < n; i++ {
		b *= 2
		if b >= ceil {
			return ceil
		}
	}
	if b > ceil {
		return ceil
	}
	return b
}

func retryLabel(n, max int) string {
	if max > 0 {
		return fmt.Sprintf("retry %d/%d", n, max)
	}
	return fmt.Sprintf("retry %d", n)
}

// failureDetailMax bounds how much of the harness's diagnostic text reaches the
// stop message. A session's stderr can run to megabytes; the operator needs
// enough to recognise the failure and quote it in a bug report, not the log.
const failureDetailMax = 400

// failureDetail renders a harness diagnostic for a one-line stop message: the
// TAIL of the text, non-printables stripped, whitespace collapsed, bounded.
//
// The tail rather than the head, deliberately. The scope starts with stderr,
// which on a real run leads with startup noise that is identical on every
// session; the thing that killed this one is the last thing said. Truncating
// from the front would reliably show the operator the one part that is never
// the answer.
//
// Non-printables are stripped because this changes what the text is FOR. Up to
// now a diagnostic was only ever fed to regexp.MatchString, where a control byte
// is inert; here it is rendered to a terminal, where ESC is executed. The
// scoping rules inherited from CLA-258 answer "what may a classifier READ", which
// is a different question from "what may be PRINTED", and stderr reaches this
// verbatim from the harness. Stripping is cheap and the answer does not depend
// on trusting the upstream CLI's output.
//
// Empty in, empty out — and the caller appends it, so a harness with nothing to
// say leaves the existing message exactly as it was rather than trailing a
// colon and a blank.
func failureDetail(diag string) string {
	printable := strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			return r
		}
		return -1
	}, diag)

	s := strings.Join(strings.Fields(printable), " ")
	if s == "" {
		return ""
	}
	// Counted and sliced in RUNES, not bytes: the harness's own text carries "·"
	// in every usage-limit notice and "→" in rendered tool markers, so a byte
	// slice would routinely cut a rune in half and emit U+FFFD at the seam.
	//
	// "..." rather than U+2026: this string exists to be pasted into a bug
	// report, and a single-character ellipsis is exactly what arrives as
	// mojibake when terminal output is copied out.
	if r := []rune(s); len(r) > failureDetailMax {
		s = "..." + string(r[len(r)-failureDetailMax:])
	}
	return ": " + s
}

// droppedNote says how much of a session's output the retained window did not
// keep, when it kept less than all of it.
//
// It rides on the message that ENDS a run, because that is the message whose
// weight depends on it: "exited 1 (non-retryable): <text>" reads as "this was the
// failure", when what the classifier actually saw was the last couple of MiB. An
// operator deciding whether the classifier merely does not know this failure yet
// needs to know which of those they are looking at. Empty when nothing was
// dropped, which is the ordinary case.
func droppedNote(dropped int64) string {
	if dropped <= 0 {
		return ""
	}
	return fmt.Sprintf(" (note: %.1f MiB of this session's output was past the retained window, so the classification read only the tail)",
		float64(dropped)/(1<<20))
}

// invocation builds the harness invocation for one target: the target's workdir
// and .mcp.json (which select the project) over the config's global fields.
func (d *Driver) invocation(t Target, probe bool) harness.Invocation {
	workdir := t.WorkDir
	if workdir == "" {
		workdir = d.cfg.WorkDir
	}
	mcp := t.MCPConfigPath
	if mcp == "" {
		mcp = d.cfg.MCPConfigPath
	}
	return harness.Invocation{
		Prompt:        d.cfg.Prompt,
		Model:         d.cfg.Model,
		WorkDir:       workdir,
		MCPConfigPath: mcp,
		ConfigDir:     d.cfg.ConfigDir,
		SettingsPath:  d.cfg.SettingsPath,
		Env:           d.cfg.EnvSlice(),
		Probe:         probe,
	}
}

// prefix returns the per-target log prefix ("[slug] "), or "" when the instance
// drives a single unnamed target — so single-project logs read exactly as before.
func (d *Driver) prefix(i int) string {
	return labelOf(d.targets[i])
}

func labelOf(t Target) string {
	if t.Name == "" {
		return ""
	}
	return "[" + t.Name + "] "
}

// waitOrStop waits up to dur, but stays responsive to a STOP marker (consuming it)
// and to context cancellation. Reports whether the loop should stop.
func (d *Driver) waitOrStop(ctx context.Context, dur time.Duration) bool {
	const chunk = 3 * time.Second
	// Round(0) strips the monotonic reading, so wallStart measures REAL elapsed
	// time. Everything else here runs on the monotonic clock, which does not
	// advance while the machine is suspended — so comparing the two is the only
	// way a slept-through wait becomes visible. Without it, a laptop that idle
	// sleeps turns a 30-minute pause into a multi-hour silence that looks exactly
	// like a hung process.
	wallStart := time.Now().Round(0)
	end := time.Now().Add(dur)
	for {
		if present, _ := d.readMarker("STOP"); present {
			_ = d.state.Remove("STOP")
			log.Print("STOP requested during wait — stopping")
			return true
		}
		remaining := time.Until(end)
		if remaining <= 0 {
			if lost, stalled := sleepStall(dur, time.Now().Round(0).Sub(wallStart)); stalled {
				log.Printf("wait of %s took %s of wall clock — the machine was suspended for ~%s; timers are frozen while it sleeps, so an unattended run stalls silently (macOS: `caffeinate -i clankerbar run …`, and start it on AC — plugging in later does not wake a sleeping Mac)",
					dur, time.Now().Round(0).Sub(wallStart).Round(time.Second), lost.Round(time.Second))
			}
			return false
		}
		w := chunk
		if remaining < w {
			w = remaining
		}
		select {
		case <-ctx.Done():
			return true
		case <-time.After(w):
		}
	}
}

// readMarker reports whether a control marker file exists, and its first line.
//
// A marker is the operator's switch, so it is read through the state dir handle:
// a symlink at the name is refused rather than followed, and the read is capped.
// Following one would let whoever planted it choose a file for us to open and a
// line of it to echo into the daemon's log.
func (d *Driver) readMarker(name string) (bool, string) {
	b, err := d.state.ReadFile(name)
	if err != nil {
		return false, ""
	}
	line := strings.TrimSpace(string(b))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return true, line
}

// randomTail is the unguessable part of an iteration log's name. crypto/rand,
// not math/rand: the whole point is that the session whose transcript this is
// cannot predict it, and a seeded PRNG in a daemon it can watch start is not
// unpredictable. rand.Read never fails on any supported platform (it panics
// internally rather than returning an error), so there is no fallback branch to
// get wrong.
func randomTail() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func resetSuffix(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " (stated reset " + t.Format("Mon 15:04") + ")"
}

// limitReason renders a limit's reason for a log line, defaulting when a harness
// leaves it blank.
func limitReason(lim harness.Limit) string {
	if lim.Reason != "" {
		return lim.Reason
	}
	return "usage limit"
}
