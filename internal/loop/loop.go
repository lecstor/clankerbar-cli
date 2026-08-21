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
	"maps"
	"os"
	"path/filepath"
	"slices"
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

// errZeroSpendLoop marks the one give-up the budget breaker could not have
// reached: a run of attempts that each ended before the harness reported any
// usage, so nothing fed the token or cost ceiling and the retry ladder would have
// gone on indefinitely (CLA-288).
//
// It is a distinct, wrapped sentinel rather than a plain string because the three
// ways a run can end early are three different things for an operator to do next:
// a budget stop means retune the ceiling, a wall-clock stop means the window was
// too short, and this one means the harness is not running - none of which is
// legible from "the run stopped".
var errZeroSpendLoop = errors.New("zero-spend attempt loop")

// errUnclassifiedRetryLoop marks the other give-up no ceiling would have reached:
// a ladder of retries taken on a failure the adapter could not NAME, only judge
// by heuristic (harness.Adapter.IsUnclassifiedTransient).
//
// It is the sibling of errZeroSpendLoop and exists for the mirror-image reason.
// That one bounds attempts that spend NOTHING, so the budget breaker never sees
// them. This one bounds attempts that spend NORMALLY on a failure that will
// happen again: a deterministic post-usage crash satisfies the heuristic every
// time, and none of the ordinary dials stop it — max_retries and budget are off
// by default, and max_zero_spend_attempts is reset by the very usage the
// heuristic requires. Left unbounded it re-spawns a full paid session on the
// same task at the backoff ceiling, all night (CLA-381).
//
// Also distinct because the operator's next move is distinct: this one means the
// adapter has no pattern for what went wrong, so the fix is to read the
// diagnostic in the error and — if it is a real blip — teach the adapter's
// transient patterns about it.
var errUnclassifiedRetryLoop = errors.New("unclassified retry loop")

// unclassifiedRetryBound is how many retries one phase's ladder may take on
// heuristically-judged failures before it gives up.
//
// Deliberately a constant and not a config dial. The dials an operator sets are
// about how much they are willing to spend on failures the tool UNDERSTANDS;
// this bounds guesses, and the honest number of guesses is small at any budget.
// Two leaves room for a genuine one-off blip the patterns have not caught up
// with — the case the heuristic exists for — while a deterministic failure costs
// three paid sessions instead of a night's worth.
const unclassifiedRetryBound = 2

// perTaskDeadBound is how many consecutive dead phases on the SAME task park it
// (CLA-396, raised from 2 by the 2026-08-20 operator decision). A task really
// can be cursed — a brief that reliably kills every session sent at it — but at
// 2 the budget fired on ordinary bad luck: at the observed 37.5% per-session
// death rate, two consecutive deaths is a 14% roll, and CLA-390 was parked on
// exactly that and had to be un-parked by hand. Four consecutive deaths is a
// ~2% roll — a signal rather than noise — while a task that dies once per drain
// and succeeds between (the retry re-claims the same task within the drain)
// never accumulates past one here, because the counter resets when a phase of
// the task advances (see the reset below).
const perTaskDeadBound = 4

// fleetDeadBound is how many consecutive dead phases ACROSS tasks pause the
// loop for that target and raise one project-level question (CLA-396). One
// more than the per-task bound, on purpose: a single cursed task parks at 4
// (its own counter fires first, and the drain breaks before any fifth death),
// so the fleet counter can only reach 5 across DIFFERENT tasks — the shape
// only a fleet-wide fault produces. A task that dies once looks identical to
// a healthy task hit by a provider outage, and only a run of deaths spanning
// tasks distinguishes the two; 5 consecutive deaths at the observed 37.5% rate
// is a ~0.7% roll, and it stays conservative after CLA-406 removes the
// retryable class from the numerator.
const fleetDeadBound = 5

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

	// MCPConfigPaths is this project's MCP config PER HARNESS, for a sequence
	// whose phases are not all on one harness (CLA-366). MCPConfigPath above is
	// the top-level harness's file; a phase on another harness needs the same
	// project in that harness's schema, which is a different file. Empty on every
	// single-harness run, and config.Validate refuses the combination that would
	// need an entry here and lacks one.
	MCPConfigPaths map[string]string

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

	// undeclared counts the sessions this run that ended still holding a task
	// whose work they had already pushed — the phase did its job and then did not
	// hand the task over (CLA-384). Nothing is lost: the branch is recorded and a
	// takeover resumes. What is lost is the money, twice, because the next clanker
	// pays to rediscover where the last one got to. Measured 2026-08-19 on v0.8.1,
	// this was 3 of 4 review phases in one evening and $31.30 of that run's spend,
	// and the only way to notice was diffing task statuses against the log by hand.
	// So it is counted and the count is said out loud, per occurrence: a run that
	// is silently repeating its own work should be visible without forensics.
	undeclared int

	// Per-target no-progress state. `claimable > 0` is a claim that work is
	// available, not that it can be DONE: a task can be claimable and still
	// unworkable — gated on an unanswered question, needing a toolchain the
	// harness may not run, blocked on something no session can resolve. The gate
	// cannot tell, so it spawns, the session correctly declines, and the operator
	// pays for the same report every cycle. One real run spent ten iterations
	// re-writing the same two questions. These back a repeatedly fruitless target
	// off instead.
	//
	// The accumulator is denominated in TOKENS, not sessions (CLA-343): the old
	// count of 3 fruitless sessions cost ~78M tokens before it first fired (~26M
	// per measured iteration), and one huge fruitless drain could repeat that
	// spend without ever climbing the count. Tokens since last progress make the
	// first sit-out land at the same ~80M while letting a single runaway drain
	// trip it on its own.
	quietTokens []int       // tokens spent on this target's drains that settled nothing
	spent       []int       // the last drain's tokens, awaiting its progress verdict
	baseline    []int       // settled count when this target's last drain began
	openQs      []int       // open-question count at this target's last poll
	pending     []bool      // a drain of this target is awaiting its progress verdict
	skipUntil   []time.Time // a backed-off target is ineligible until this time

	// The fleet dead-phase counter (CLA-396). Per-target because a multi-target
	// driver runs several projects, and "the fleet" is one project's sessions:
	// a run of dead phases on project A must not pause project B. The per-task
	// budget in drainPhases cannot see a provider failing for every task — each
	// individual task dies only once, so no per-task counter ever trips — and
	// these exist to catch exactly that: consecutive dead phases across
	// DIFFERENT tasks mean the harness or provider is broken, not the tasks.
	// The counter resets ONLY on a successful implement phase (see the reset in
	// drainPhases), because review phases run on the claude harness and cannot
	// exhibit the zero-usage death: a claude success vouching for the opencode
	// gateway would hide the very fault the detector exists to find.
	fleetDead []int // consecutive dead phases across all tasks on this target

	// fleetPaused marks a target whose fleet counter has tripped: the loop stops
	// spawning sessions for it until the operator answers the raised question —
	// a known-failing provider must stop burning sessions. Measured as the
	// open-question count falling back to what it was at the raise (fleetOpenQ),
	// the same "something was resolved" signal judgeProgress uses.
	fleetPaused []bool

	// fleetRaised records that this target's fleet question is OPEN, so a trip
	// raises exactly ONE project-level question rather than one per poll while
	// the pause is in force. Cleared when the pause clears, so a later trip —
	// a new episode — raises a fresh question.
	fleetRaised []bool

	// fleetOpenQ is the open-question count at the raise: the baseline the
	// resume signal is measured against. Captured from the last poll (d.openQs)
	// at trip time, so it does not include the question just raised; the pause
	// clears once the count falls back to it or below.
	fleetOpenQ []int

	// newVerifier builds the delivery checker for a workdir (CLA-253). A field so
	// tests can substitute one; production always gets internal/delivery.
	newVerifier func(workdir string) deliveryVerifier

	// newSalvager builds the stranded-work rescuer for a workdir (CLA-314). Same
	// shape and the same reason as newVerifier.
	newSalvager func(workdir string) workSalvager

	// newAdapter resolves a harness NAME to its adapter, for a sequence whose
	// phases are not all on one harness (CLA-366). Production is harness.Get; a
	// field so tests can hand out their own fakes per name.
	//
	// It is only consulted for a phase naming some OTHER harness — a phase on the
	// run's own harness gets d.h, the adapter the caller constructed the Driver
	// with. That is what keeps every existing test (which passes a fake and never
	// names a harness on a phase) driving its fake rather than reaching for a real
	// binary through the registry.
	newAdapter func(name string) (harness.Adapter, error)

	// spentBy is this run's spend per harness name, for the per-harness breakers
	// (CLA-367). It hangs off the Driver rather than riding in `spend` because it
	// is the RUN's account: a per-harness total outlives the drain that produced
	// it, while the drain-local accumulators threaded through `spend` do not. See
	// charge and budgetTrip. Lazily built; a run that configures no per-harness
	// block still fills it, which costs one map and keeps the accounting honest if
	// a block is added mid-file.
	spentBy map[string]harnessSpend

	// deadTally is this run's dead-phase tally, keyed by phase label and harness
	// name (CLA-402): how many phase sessions ran and how many of those died
	// producing nothing. Like spentBy it is the RUN's account — the rate is
	// measured over a run, not a drain — so it hangs off the Driver and is
	// reported from Run. Lazily built; see tallyDead and logDeadTally.
	deadTally map[tallyKey]*phaseTally
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
		quietTokens: make([]int, n),
		spent:       make([]int, n),
		baseline:    make([]int, n),
		openQs:      make([]int, n),
		pending:     make([]bool, n),
		skipUntil:   make([]time.Time, n),
		fleetDead:   make([]int, n),
		fleetPaused: make([]bool, n),
		fleetRaised: make([]bool, n),
		fleetOpenQ:  make([]int, n),
		newVerifier: func(workdir string) deliveryVerifier { return delivery.New(workdir, "") },
		newSalvager: func(workdir string) workSalvager { return salvage.New(workdir, "") },
		newAdapter:  harness.Get,
	}
}

// adapterFor resolves the harness that runs one phase.
//
// The run's own harness resolves to d.h without consulting the registry, which
// is both cheaper and the thing that keeps a Driver built with a substituted
// adapter honest: a phase that names nothing, or names the configured harness,
// runs on exactly the adapter the caller handed over.
//
// An unresolvable name is an error rather than a fallback to d.h. config.Validate
// has already refused an unregistered harness, so reaching here means the
// registry and the config disagree, and quietly running the phase on the other
// harness would be the wrong half of a mixed sequence — the review phase spawning
// on the implement harness, with nothing in the log to say so.
func (d *Driver) adapterFor(ph config.Phase) (harness.Adapter, error) {
	name := ph.Harness
	if name == "" || name == d.cfg.Harness {
		return d.h, nil
	}
	return d.newAdapter(name)
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
						if sum.Spawnable() && !d.backedOff(i) && !d.fleetPaused[i] {
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

		// There is work (or we're blind) — spend a session (or a phase sequence of
		// them, one task either way), retrying transient blips with backoff (a
		// fresh session reclaims any half-done task).
		drains++
		// The next poll of this target judges whether the drain settled anything.
		d.pending[d.cursor] = true
		tokens, cost, handoffs, stop, err := d.drainPhases(ctx, drains, d.cursor, target,
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

// harnessSpend is one harness's accumulated spend across a run, for the
// per-harness breakers (config.Budget.PerHarness, CLA-367).
type harnessSpend struct {
	tokens int
	cost   float64
}

// charge books a session's spend against the harness that ran it, in the ledger
// the per-harness breakers read.
//
// It is charged at the two places a session's spend enters a run — a drain
// attempt's result and a supervised wait's probe — which are the same two places
// the run-wide accumulators are added to, so the two accounts cannot drift. The
// run-wide ones stay exactly where they were, threaded through the drain as
// locals: the global dials have to keep behaving byte for byte as they always
// have, so this ledger is additive rather than a replacement for them.
//
// An UNTRUSTED session is charged to neither, for the reason endUntrustedDrain
// gives: a figure parsed out of a stream with a hole in it is not a measurement.
//
// The key is the phase's harness NAME AS THE CONFIG SPELLS IT (HarnessFor), never
// the adapter's own Name(). The two agree in production - an adapter is fetched
// from the registry by that same key - but only one of them is the currency the
// breaker spends: budgetTrip looks these keys up in Budget.PerHarness, which the
// operator keyed by config name. An adapter whose Name() diverged would silently
// disable its own per-harness ceiling rather than failing, so the ledger asks the
// config what a session IS and asks the adapter only behavioural questions.
//
// A run's phases can each name their own harness (CLA-366), so the caller charges
// the PHASE's harness, not the run's: on a mixed sequence the two call sites split
// the run's spend across one entry per harness, and on a single-harness run they
// both land on the same key and it tracks the run total exactly as before. That is
// the whole reason the ledger is keyed by name — passing d.h.Name() from a phase
// running on some other harness would bill opencode's tokens to claude's breaker,
// which is precisely the mis-accounting the per-harness ceilings exist to prevent.
func (d *Driver) charge(harnessName string, tokens int, cost float64) {
	if d.spentBy == nil {
		d.spentBy = make(map[string]harnessSpend)
	}
	s := d.spentBy[harnessName]
	s.tokens += tokens
	s.cost += cost
	d.spentBy[harnessName] = s
}

// budgetTrip names the ceiling that has stopped this run, or "" if none has: the
// run-wide dials first, measured against the run totals the caller threads down,
// then each harness's own block against that harness's own spend.
//
// Any trip stops the whole run, so the order only decides which ceiling gets
// named in the log, and the global dials are named first because a config that
// sets both wants the run-wide promise reported as the one that was broken.
// Harnesses are consulted in name order so a run with two blocks reports the same
// dial every time rather than whichever the map yielded first.
func (d *Driver) budgetTrip(tokens int, cost float64, elapsed time.Duration) string {
	if dim := d.cfg.Budget.ExceededBy(tokens, cost, elapsed); dim != "" {
		return dim
	}
	for _, name := range slices.Sorted(maps.Keys(d.spentBy)) {
		s := d.spentBy[name]
		if dim := d.cfg.Budget.ExceededByHarness(name, s.tokens, s.cost); dim != "" {
			// That harness's OWN totals ride along, because every caller prints
			// the RUN's figures after the reason: in a mixed-harness run two
			// dollar figures on one line read as a contradiction, which is the
			// defect Budget.ExceededBy's doc comment exists to prevent, one level
			// down.
			return fmt.Sprintf("%s (%s so far: tokens=%d cost=$%.2f)", dim, name, s.tokens, s.cost)
		}
	}
	return ""
}

// drainPhases runs ONE drain as its configured sequence of phase sessions —
// each a separate harness process, so the context resets between them. With no
// phases configured that sequence is one session and this is exactly the old
// behaviour; see config.Phase for why the split exists and config.EffectivePhases
// for why "unphased" is expressed as a single phase rather than a second path.
//
// Two things have to be true across a seam, and they are the whole design:
//
//  1. The claim is NOT handed back. The next phase resumes the same run with
//     heartbeat(runId) rather than claiming, so the lease has to survive the gap.
//     A handback here would put the task in the queue while we are still on it.
//  2. If the next phase will never run, the handback skipped at the seam happens
//     here instead — otherwise a budget stop leaves the task leased to nobody.
func (d *Driver) drainPhases(ctx context.Context, drainNum, ti int, t Target, prior spend) (tokens int, cost float64, handoffs int, stop bool, err error) {
	phases := d.cfg.EffectivePhases()
	// The claim a phase deliberately left held for its successor, and the source
	// of the task/run ids that successor's prompt needs.
	var carried *harness.Result

	// A session-authored successor prompt, pending from the last session's
	// handoff block (CLA-352). Non-empty means the next session is a handoff
	// respawn continuing the SAME phase, not the next configured one.
	nextPrompt := ""
	chain := 0   // consecutive handoff respawns, against handoffChainCap
	spawned := 0 // sessions this drain has launched, phase or respawn
	// Consecutive dead phases this TASK has produced (CLA-386). The retry budget
	// is per task, not per run: a single silent death is plausibly transient, a
	// second on the same task is not, and the same task reached fresh in a later
	// run earns its retry again. deadTask names which task the counter counts for,
	// because the retry is a fresh claiming session (the dead claim was handed
	// back): if next_task hands it a DIFFERENT task, that task has died once, not
	// twice, and must not be parked for a second death it did not have (review
	// finding). A dead phase always holds a claim, so deadTask is set whenever the
	// classification fires.
	deadRetries := 0
	deadTask := ""

	for i := 0; i < len(phases); {
		ph := phases[i]
		last := i == len(phases)-1
		isHandoff := nextPrompt != ""
		promptBytes := 0
		if isHandoff {
			// The respawn continues this phase's job under its knobs — turn cap
			// and tier still apply. The session's own prompt is framed by
			// config.HandoffPreamble, not handed over bare: the successor needs
			// the same "resume, don't claim" contract a phase resume gets from
			// its brief, and a session-authored prompt cannot be trusted to
			// carry that forward on its own. promptBytes is the session's own
			// prompt alone — what the size cap was measured against — not the
			// preamble-inflated total, so the log line stays comparable to it.
			//
			// config.HandoffContinuation (CLA-353) is appended after it for the
			// same reason: the phase's OWN brief — not only the preamble's
			// contract — is otherwise dropped on a handoff, since ph.Prompt is
			// replaced wholesale rather than framing the built-in text. For the
			// review phase that would silently drop the PR-then-update_task
			// terminal step the moment a session hands off mid-review.
			//
			// Gated on the phase actually RUNNING the built-in brief (review
			// finding): HandoffContinuation matches on name alone, so an
			// operator who reuses the name "review" for a phase carrying their
			// own custom Prompt would otherwise have the shipped terminal step
			// force-injected into a handoff respawn of a brief that never
			// established that contract in the first place — clashing with
			// whatever terminal step their own prompt names, if any.
			// d.cfg.Phases (the unresolved config, not EffectivePhases' output)
			// is index-aligned with phases: EffectivePhases builds its slice
			// 1:1 from c.Phases, never adding or dropping an entry.
			promptBytes = len(nextPrompt)
			continuation := ""
			if i < len(d.cfg.Phases) && d.cfg.Phases[i].Prompt == "" {
				continuation = config.HandoffContinuation(ph.Name)
			}
			ph.Prompt = config.HandoffPreamble + nextPrompt + continuation
			nextPrompt = ""
		}

		if spawned > 0 {
			// The breaker at a phase boundary — the third place a session ends and
			// the loop decides whether to spawn another, alongside between-drains
			// and between-attempts, and the finest granularity the ceiling has had.
			// It is not decoration: one measured task spent 92% of a whole run's
			// 75M ceiling inside a single session, which a between-drains check
			// cannot see coming and cannot interrupt. A handoff respawn gets the
			// same check, BEFORE the respawn: a session cannot spend its way past
			// the ceiling by handing off to itself.
			if dim := d.budgetTrip(prior.tokens+tokens, prior.cost+cost, time.Since(prior.start)); dim != "" {
				boundary := "the " + ph.Label(i) + " phase boundary"
				if isHandoff {
					boundary = "the handoff respawn"
				}
				log.Printf("iteration %d: budget reached at %s: %s — stopping (tokens=%d cost=$%.2f elapsed=%s)",
					drainNum, boundary, dim, prior.tokens+tokens, prior.cost+cost,
					time.Since(prior.start).Round(time.Second))
				stop = true
				break
			}
		}

		// Unphased runs keep their log names byte-identical: no phase tag. A
		// handoff respawn is tagged too, so the state dir shows the chain the way
		// the daemon log does.
		tag := ""
		if len(phases) > 1 {
			tag = "-p" + ph.Label(i)
		}
		if isHandoff {
			handoffs++
			chain++
			tag += fmt.Sprintf("-h%d", chain)
			log.Printf("%siteration %d: handoff respawn %d — spawning a fresh session on the predecessor's own %d-byte prompt, framed by the driver's resume preamble (counts as an iteration)",
				labelOf(t), drainNum, chain, promptBytes)
		}
		spawned++

		ptokens, pcost, pstop, end, perr := d.drainPhase(ctx, drainNum, i, tag, ph, last, carried, t, spend{
			start:  prior.start,
			tokens: prior.tokens + tokens,
			cost:   prior.cost + cost,
		})
		tokens += ptokens
		cost += pcost
		// What, if anything, is still owed a handback. Three cases, and getting any
		// of them wrong is a task either stranded on a dead lease or posted back to
		// the queue over work already in review:
		//
		//   - the phase handed the claim back itself → nothing is owed;
		//   - the phase observed claim state → that is now the authoritative view,
		//     including Settled and HasWIP, which releaseHeldClaim reads;
		//   - the phase observed nothing (it never launched) → KEEP the predecessor's,
		//     or a phase 2 that failed to start would silently drop the handback for
		//     the task phase 1 is still holding.
		//
		// A phase that went off-brief and claimed a DIFFERENT task is its own case.
		// noteClaimed replaces the observed claim wholesale, so the seam releases
		// that task and reports `released` — which would drop the predecessor's
		// still-live claim and hand back a task the sequence was never meant to
		// touch. Keep what we had: the task phase 1 is holding is the one owed a
		// handback, whatever its successor went and did.
		switch {
		case carried != nil && end.claim != nil && end.claim.Claim.TaskID != carried.Claim.TaskID:
			log.Printf("iteration %d: the %s phase claimed %s, which is NOT the task it was resuming (%s) — it was told not to claim at all; keeping the original for the handback",
				drainNum, ph.Label(i), end.claim.Claim.TaskID, carried.Claim.TaskID)
		case end.released:
			carried = nil
		case end.claim != nil:
			carried = end.claim
		}

		// A run-level stop wins over a dead phase. drainPhase returns stop=true
		// for a budget trip, credit exhaustion (lim.Stop), and a resumed phase
		// ending on a live lease — and on those paths end.dead can be set too (a
		// credit-starved session is exactly the shape that dies on reason
		// "unknown" with no branch). Retrying a dead phase against a stopped run
		// burns another session into the same limit and, on the second, parks a
		// task for what was actually a run-wide outage instead of stopping the
		// daemon. A run stop is final; the dead branch only decides retry vs park
		// when the run is continuing.
		if pstop {
			stop = true
			break
		}

		// CLA-386: a dead phase is a FAILED phase, not a completed one, and it is
		// not "ended without holding the task" either — it never finished the job
		// it was spawned to do. The claim was released back in drainPhase (a retry
		// re-claims, exactly like a transient one), so the only question left is
		// retry vs park. Consecutive dead phases on the SAME task: retry up to the
		// per-task budget, then park — a task that can kill four full sessions
		// reaches the operator rather than a fifth (CLA-396, raised from two by
		// the 2026-08-20 operator decision).
		//
		// Deliberately BEFORE the error break below: the dead classification
		// names a phase that produced nothing whatever its exit code, so a dead
		// phase is retried-then-parked, never a run-failing error — the non-zero
		// exit that would otherwise stop the daemon is itself part of the silent
		// death, not a verdict.
		//
		// The retry is scoped to the FIRST phase (`i == 0`), the one that claimed
		// its own task: a retry cannot re-seed a resumed phase — invocationFor
		// only substitutes the task/run placeholders and sets ResumeClaim when the
		// predecessor claim is present, which the dead handback cleared — so
		// retrying a dead RESUMED phase would spawn a session whose brief still
		// carries the literal {{taskId}}/{{runId}} and whose handback, salvage and
		// delivery checks are all switched off. A resumed phase's dead signature
		// still vetoes its checkpoint below (the false-premise guard applies to
		// every phase); it just is not retried or parked here — the drain ends and
		// the task returns to the queue, where a fresh claiming session retries it
		// with a valid seed (review finding). The operator decision's "retry the
		// implement phase N times, then park" is exactly `i == 0` in the shipped
		// [implement, review] sequence.
		//
		// CLA-396: a dead phase feeds BOTH counters, but the per-task park is
		// checked and taken FIRST — before the fleet counter's trip is even
		// consulted. The fleet counter persists on the Driver across drains and
		// only resets on a successful implement phase, so it can carry a residual
		// count left over from an earlier, unrelated task's death into THIS
		// task's own run of deaths. Checking fleet first would let that leftover
		// tip the fleet bound one death before this task reaches its own —
		// pre-empting a park this task legitimately earned with a fleet-wide
		// pause that blames the provider for what is, this time, actually the
		// task. Per-task takes priority: a task that has itself earned a park is
		// parked, full stop; only once that is ruled out does a fleet-wide run
		// of deaths across DIFFERENT tasks get to trip the fleet pause. A session
		// that never got past its claim is excluded from both by construction:
		// `end.dead` requires `res.Claim.Held()`, and a refused claim (lease_live,
		// a lost race) observes no task id, so its death counts toward neither
		// counter. This is the discriminator CLA-402's retrospective scan must
		// agree with.
		if end.dead {
			taskID := ""
			if end.claim != nil {
				taskID = end.claim.Claim.TaskID
			}

			if !last && i == 0 && taskID != "" && taskID == deadTask && deadRetries >= perTaskDeadBound-1 {
				log.Printf("%siteration %d: the %s phase died producing nothing for the %dth consecutive time on this task — parking it per the 2026-08-20 decision (%d consecutive dead phases, then park)",
					labelOf(t), drainNum, ph.Label(i), deadRetries+1, perTaskDeadBound)
				d.parkDeadPhase(ctx, t, ph.Label(i), end.claim)
				// The task is parked for the operator and this drain is over; the
				// daemon carries on with the next task. Not a stop: the run did
				// not fail, one task reached a human.
				break
			}

			d.fleetDead[ti]++
			if d.fleetDead[ti] >= fleetDeadBound {
				d.fleetTrip(ctx, t, ti, drainNum, end.claim)
				// The fleet trip pauses this target and this drain is over; the
				// in-flight task was already released with the dead phase. Not a
				// stop: the run itself did not fail.
				break
			}

			if !last && i == 0 {
				// Either the first dead phase this task produced, or a dead phase
				// on a task other than the one counted above: that task has died
				// once, and earns its own retry ladder rather than being parked
				// for deaths it did not have.
				if taskID != deadTask {
					deadRetries = 1
					deadTask = taskID
				} else {
					deadRetries++
				}
				log.Printf("%siteration %d: the %s phase died producing nothing (final step reason %q, no branch recorded) — retrying it (%d of %d) before any review brief sees the task",
					labelOf(t), drainNum, ph.Label(i), deadReason(end.claim), deadRetries, perTaskDeadBound)
				continue
			}
		}

		if perr != nil {
			err = perr
			break
		}

		// A handoff the session emitted and drainPhase judged safe (clean exit,
		// trusted stream, task still held, prompt within the size cap). Two guards
		// remain, and both refuse toward the STANDARD path — the next configured
		// brief, or the drain ending — never toward truncating or rewriting the
		// session's prompt (CLA-352).
		if end.handoff != "" {
			switch {
			case chain >= handoffChainCap:
				log.Printf("%siteration %d: handoff refused — this task has already chained %d consecutive handoff respawns, the cap; falling back to the standard brief so a session cannot chain itself indefinitely",
					labelOf(t), drainNum, chain)
			case d.cfg.MaxIterations > 0 && drainNum+handoffs >= d.cfg.MaxIterations:
				log.Printf("%siteration %d: handoff refused — a respawn counts as an iteration and max-iterations (%d) is already spent; falling back to the normal path",
					labelOf(t), drainNum, d.cfg.MaxIterations)
			default:
				// Same phase index: the successor continues this phase's job. The
				// budget breaker at the top of the loop runs before the spawn.
				nextPrompt = end.handoff
				continue
			}
		}
		// This session ended without a respawn being accepted, so any chain is
		// broken: a later handoff starts counting from a standard-brief session.
		chain = 0

		// A non-final phase that did not reach its checkpoint leaves the next one
		// nothing to resume: the session settled the task itself (worked past its
		// brief and finished it), its stream could not be trusted to say either way,
		// or the adapter does not observe claims at all. The sequence is over, and
		// none of those is a failure — a task that reached in_review in one session
		// is a task that got done.
		if !last && !end.checkpoint {
			log.Printf("iteration %d: the %s phase ended without holding the task — nothing for the next phase to resume, so this drain ends here",
				drainNum, ph.Label(i))
			break
		}
		// A phase that ended dead was retried above and, on the retry, either
		// parked or advanced — either way this phase's dead-phase budget is spent.
		// Resetting on the advance keeps the retry per-PHASE rather than per-run:
		// two dead phases separated by a healthy one are not "consecutive", and
		// each phase earns its own retry ladder (CLA-386).
		deadRetries = 0
		deadTask = ""
		// CLA-396: a successful IMPLEMENT phase resets the FLEET-wide dead-phase
		// counter. Only the implement phase runs the harness that can exhibit the
		// zero-usage death — the review phase runs on the claude harness, which
		// cannot — so a claude success must not vouch for the opencode gateway:
		// letting any success reset the counter would hide a fleet fault from the
		// detector that exists to find it. Reaching here means this phase did not
		// die and did not fail, so for the implement phase it is the "successful
		// implement" the reset is keyed to.
		if ph.Name == config.ImplementPhaseName {
			d.fleetDead[ti] = 0
		}
		i++
	}

	// The deferred handback (2, above). Skipped at every seam that led to another
	// phase; owed at the one that did not — including a final-phase handoff the
	// guards above refused, whose claim was held for a successor that never ran.
	carried = d.releaseCarried(ctx, t, carried)
	return tokens, cost, handoffs, stop, err
}

// phaseEnd is how a phase reports what it left behind, so drainPhases can decide
// whether to run the next one and what is still owed a handback.
//
// The three fields are deliberately independent. A phase can end holding a claim
// without it being a checkpoint (it was the last one), and can observe claim
// state without owing anything (it settled the task, or already handed it back).
type phaseEnd struct {
	// claim is the final attempt's result when that result carried claim state at
	// all — the thing a deferred handback acts on, and the source of the ids the
	// next phase's prompt is seeded with. nil when the phase observed nothing:
	// a launch failure, or an adapter that does not track claims. nil means "I
	// know nothing", NOT "nothing is owed" — the caller keeps what it had.
	claim *harness.Result

	// released reports that this phase already handed a genuinely-held claim back
	// itself, so nothing is owed and a second handback would post `ready` over
	// whatever came next.
	released bool

	// checkpoint reports an orderly end still holding the task, with the sequence
	// meant to continue into the next phase.
	checkpoint bool

	// dead reports the CLA-386 dead-phase signature: the session's final step
	// finished with reason "unknown" and no branch was recorded on the task, so
	// the phase produced nothing. It is a FAILED phase, not a completed one —
	// the seam must not hand it on to the next phase's brief. The claim has
	// already been released (a retry re-claims); drainPhases owns the retry-once-
	// then-park decision and its per-task budget.
	dead bool

	// handoff is the successor prompt the session emitted in its final message's
	// handoff block, already past the parse-time guards (marker present, prompt
	// non-empty and under the size cap, clean exit, trusted stream, task still
	// held — see detectHandoff). Non-empty implies checkpoint: the lease is
	// being held for the successor. drainPhases owns the remaining guards — the
	// chain cap, max-iterations, the budget breaker — and either respawns on it
	// or falls back to the standard path (CLA-352).
	handoff string
}

// deadPhase reports the CLA-386 dead-phase signature: the session's final step
// finished with reason "unknown" — opencode's marker for a session that died
// without producing a final answer — AND no branch is recorded on the task, so
// nothing durable came out of it. The two conjuncts together are what "produced
// nothing" means: the reason names the death, the missing branch rules out the
// one thing that would have made the phase survivable anyway. A session that
// pushed work and THEN died on an unknown reason has still produced something,
// and its phase keeps its checkpoint.
//
// It reads the reason off the adapter's Raw map rather than asking the adapter:
// the key is stable across the harnesses that observe it, and no Adapter method
// needs to grow for a signal only opencode emits today.
func deadPhase(res harness.Result) bool {
	r, _ := res.Raw[harness.FinishReasonKey].(string)
	return r == harness.FinishReasonUnknown && !res.Claim.HasWIP
}

// drainWithRetries runs a single unphased drain — the whole task in one session.
// It is what a config with no `phases` gets, and it is kept as its own entry
// point because that is the shape every existing test drives.
//
// The phase comes from EffectivePhases, not an inline literal, so the unphased
// path carries the same resolution every other path does — the top-level or
// default turn cap (CLA-343), which is precisely what the inline zero-value
// version failed to do.
func (d *Driver) drainWithRetries(ctx context.Context, drainNum int, t Target, prior spend) (tokens int, cost float64, stop bool, err error) {
	ph := d.cfg.EffectivePhases()[0]
	tokens, cost, stop, _, err = d.drainPhase(ctx, drainNum, 0, "", ph, true, nil, t, prior)
	return tokens, cost, stop, err
}

// drainPhase runs ONE phase to a clean finish, absorbing usage-limit pauses
// (supervised wait) and transient blips (exponential backoff) by re-running the
// SAME session — neither costs a drain count. Returns the tokens/cost consumed on
// a clean finish; stop=true if a STOP/cancel landed during a wait; err only on a
// genuine, non-retryable failure (or exhausted retries).
//
// held is non-nil only when this phase ended cleanly, is not the last, and left
// the task's claim open for its successor — see drainPhases, which either carries
// it to the next phase or hands it back.
//
// prev is the claim the PREVIOUS phase left held, and is what fills the task/run
// placeholders in this phase's prompt. nil for the first phase, which has no run
// to resume and claims one of its own.
//
// prior carries the run's ceilings down into the drain, because the budget breaker
// in Run only runs BETWEEN drains and every wait in here happens inside one — see
// the check at the top of the loop, and waitPastBudget.
// phaseIdx is this phase's 0-based position in the sequence, carried only so a
// phase with no name can still be NAMED in a log line — ph.Label(phaseIdx) falls
// back to "phase 2". An unphased drain passes 0 and never reaches those lines.
func (d *Driver) drainPhase(ctx context.Context, drainNum int, phaseIdx int, tag string, ph config.Phase, last bool, prev *harness.Result, t Target, prior spend) (tokens int, cost float64, stop bool, end phaseEnd, err error) {
	// The adapter for THIS phase, resolved once: everything below — the spawn, the
	// classification of what came back, and the probe of any usage limit it hit —
	// has to be the harness that actually ran, not the run's default. Resolved
	// here rather than per attempt so a retry ladder cannot straddle two harnesses.
	a, aerr := d.adapterFor(ph)
	if aerr != nil {
		return 0, 0, false, end, fmt.Errorf("iteration %d: phase %q: %w", drainNum, ph.Label(phaseIdx), aerr)
	}
	retries := 0
	// Consecutive attempts that ended without the harness reporting any usage -
	// the spend the budget breaker above can never see (CLA-288).
	//
	// Deliberately scoped to THIS phase's attempt ladder, which is where the
	// unbounded loop lives: every re-spawn of an attempt comes back through the
	// top of this loop, and it is the ladder - not the phase sequence - that has
	// no other bound under `max_retries: 0`. A phased run therefore allows the
	// bound per phase, and a handoff respawn (which needs a clean exit and a held
	// claim, so a silently dying attempt cannot produce one) starts a fresh count
	// under drainPhases' own chain cap.
	noUsage := 0
	// Retries this ladder has taken on a failure the adapter judged retryable
	// WITHOUT recognising it (harness.Adapter.IsUnclassifiedTransient) - see
	// errUnclassifiedRetryLoop. Scoped per phase like noUsage, and for the same
	// reason: the ladder is what has no other bound.
	//
	// Never reset. A classified retry in between does not make the next guess any
	// better informed, and resetting would let a failure that alternates between a
	// recognised blip and an unrecognised one loop forever - which is the exact
	// shape of a provider degrading: real 5xx blips interleaved with stream drops
	// nothing has a pattern for.
	unclassified := 0
	for {
		// The breaker, from inside the drain. Every path that loops back here has
		// just waited — a supervised wait on a usage limit, an exponential backoff
		// on a transient blip — and each of those waits used to be unbounded by
		// anything but the wall clock, which is only one of three dials and the one
		// an operator is least likely to have set. A drain that re-spawns a paid
		// session on a loop is the expensive failure this stops, whatever put it
		// there (CLA-258).
		if dim := d.budgetTrip(prior.tokens+tokens, prior.cost+cost, time.Since(prior.start)); dim != "" {
			log.Printf("iteration %d: budget reached mid-drain: %s — stopping (tokens=%d cost=$%.2f elapsed=%s)",
				drainNum, dim, prior.tokens+tokens, prior.cost+cost, time.Since(prior.start).Round(time.Second))
			return tokens, cost, true, end, nil
		}

		// The breaker's blind spot, bounded. The check above can only stop spend it
		// was told about; an attempt that died before its harness reported usage
		// added nothing to either accumulator, so a ladder of those attempts leaves
		// a token or cost ceiling permanently unreachable and only max_wall_clock -
		// the dial an operator is least likely to have set as a spend bound - can
		// end the run. Reached only on an attempt re-spawn (the first pass has
		// counted nothing), so a single silent attempt that ends the phase some
		// other way never trips it.
		if bound := d.cfg.ZeroSpendAttemptBound(); noUsage >= bound {
			return tokens, cost, false, end, fmt.Errorf(
				"iteration %d: %w: %d consecutive attempts died before %s reported any usage, so nothing the budget breaker reads was ever fed - the sessions are failing early rather than working (see the attempt logs in the state dir; raise max_zero_spend_attempts to allow more)",
				drainNum, errZeroSpendLoop, noUsage, a.Name())
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
		inv := d.invocationFor(t, phaseIdx, ph, prev)
		logName := fmt.Sprintf("iteration-%s-d%d%s-a%d-%s.log",
			time.Now().Format("20060102-150405"), drainNum, tag, retries, randomTail())
		f, ferr := d.state.Create(logName)
		logPath := filepath.Join(d.state.Path(), logName)
		if ferr == nil {
			inv.Console = io.MultiWriter(os.Stderr, f)
		} else {
			inv.Console = os.Stderr
			log.Printf("could not open iteration log %s: %v", logPath, ferr)
		}
		if retries == 0 {
			log.Printf("iteration %d %s— spawning %s (log: %s)", drainNum, labelOf(t), a.Name(), logPath)
		} else {
			log.Printf("iteration %d %s— retry %d, spawning %s (log: %s)", drainNum, labelOf(t), retries, a.Name(), logPath)
		}

		res, ierr := a.Invoke(ctx, inv)
		if f != nil {
			_ = f.Close()
		}
		// The orderly-cap classifications, computed here (before the salvage) so
		// the salvage can tell whether a clean tree means "produced nothing" — a
		// question the caps answer: a capped session is an orderly end with its
		// own marker, not a silent death, and an untrusted stream's finish reason
		// cannot be read at all. Purely adapter reads of `res`, so moving them up
		// changes nothing else (review finding).
		capped := a.TurnCapped(res)
		ceiling := a.TokenCeilingHit(res)
		wallclock := a.WallClockCapped(res)
		zeroUsage := a.ZeroUsageUnknown(res)
		producedNothing := res.Untrusted == "" && !capped && !ceiling && !wallclock && deadPhase(res)
		// Rescue whatever the session left uncommitted, FIRST — before the handback,
		// because a successful salvage changes what the handback should do: a task
		// with a branch recorded on it is no longer safe to release (CLA-314).
		d.salvageStrandedWork(ctx, t, &res, producedNothing)
		// Hand back anything the session was still holding, BEFORE deciding what to
		// do next — every branch below either waits, retries or returns, and all of
		// them leave the lease unattended. Above the ierr check too: Invoke returns
		// a fully parsed Result alongside a Wait failure, so a claim observed on
		// that stream is real and must not be dropped just because the process died
		// untidily. A launch failure yields a zero Result, which releases nothing.
		//
		// UNLESS this phase reached its checkpoint with another phase still to run.
		// Then the sequence is not over: the next phase resumes THIS run with
		// heartbeat(runId) instead of claiming, so handing the lease back here
		// would post the task to the queue while we are still working it, and the
		// next phase would find someone else's task or none at all. drainPhases
		// owns the handback in that case and performs it the moment the sequence
		// ends for any other reason.
		//
		// Only an ORDERLY end holds it open, and each conjunct is load-bearing: a
		// retry deliberately re-claims a half-done task with a fresh session
		// (which is why an ordinary non-zero exit still releases), and an untrusted
		// stream must not be read for claim state at all, because the settle we
		// never saw may be in the bytes that never arrived (CLA-262).
		//
		// A turn-capped end CAN be orderly — but only if something durable came out
		// of it. The salvage above is not a guarantee: it commits nothing on a
		// clean worktree, and refuses outright on a diverged remote or a tree in
		// the middle of a merge. A phase 1 that burned its turns reading and
		// planning without writing therefore leaves no branch at all, and calling
		// that a checkpoint spawns a session told "an earlier session has already
		// implemented, committed and pushed" and pointed at a branch that is not
		// there — a paid session that can only fail, or worse, move the task to
		// review over no work.
		//
		// HasWIP is exactly that signal: it is set by the only two things that make
		// a checkpoint real — the phase recording its own branch, or the salvage
		// recording one for it and the PLANE accepting the record.
		// A session the adapter ended on its wall-clock cap is the third member of
		// the same family: an orderly cut-off mid-thought, whose survivability rests
		// on the salvage exactly as a turn cap's does — so it earns a checkpoint on
		// the same terms, HasWIP and all (CLA-368). Classified by the PHASE's adapter
		// (`a`), never the run's: "did this session end on its own wall clock" is a
		// question only the harness that RAN it can answer, and under per-phase
		// harnesses (CLA-366) that harness is not necessarily d.h.
		checkpointable := res.ExitCode == 0 || ((capped || ceiling || wallclock) && res.Claim.HasWIP)
		// CLA-386: a session whose final step finished with reason "unknown" and
		// left no branch recorded produced NOTHING — a dead phase, not a completed
		// one. It must not earn a checkpoint even on a zero exit, because a
		// checkpoint is what hands the next phase a brief that claims "an earlier
		// session has already implemented, committed and pushed". Two real
		// implement phases died exactly this way and the seam advanced to review
		// on the false premise; the signal has to veto the checkpoint.
		//
		// A capped session is excluded even when its stream carries "unknown": the
		// wall-clock kill can land mid-turn, but a cap is an orderly end with its
		// own marker — retrying it would re-spend against the same cap and parking
		// it would read a doing-its-job backstop as a silent death.
		dead := !last && res.Untrusted == "" && res.Claim.Held() && !capped && !ceiling && !wallclock && deadPhase(res)
		// CLA-402: book this session into the run's dead-phase tally, per phase
		// label and harness, whatever the seam decided about retrying it. The
		// tally's dead classification deliberately drops the `!last` conjunct
		// that guards the checkpoint veto — a dead LAST phase is still a dead
		// phase, and the rate is a measurement, not a seam decision (see
		// deadtally.go).
		d.tallyDead(ph.Label(phaseIdx), d.cfg.HarnessFor(ph), res, capped, ceiling, wallclock)
		end = phaseEnd{}
		if res.Claim.TaskID != "" {
			end.claim = &res
		}
		// A handoff block ending the session's final message (CLA-352) holds the
		// task open exactly like a phase seam — even on the LAST phase, where
		// there is otherwise no successor to hold it for. The remaining guards
		// live in drainPhases; if they refuse, the deferred handback there
		// releases what this held.
		handoff := detectHandoff(drainNum, t, res)
		if (!last || handoff != "") && res.Untrusted == "" && checkpointable && res.Claim.Held() && !dead {
			end.checkpoint = true
			end.handoff = handoff
			if handoff != "" {
				log.Printf("%siteration %d: the session ended its final message with a handoff block, still holding %s — keeping the lease for its successor",
					labelOf(t), drainNum, res.Claim.TaskID)
			} else {
				log.Printf("%siteration %d: phase reached its checkpoint holding %s — keeping the lease for the next phase",
					labelOf(t), drainNum, res.Claim.TaskID)
			}
		} else {
			// `released` records only a handback that could actually have happened:
			// a zero Result from a failed launch releases nothing, and reporting it
			// as released would erase a predecessor's claim that is still live.
			end.released = res.Claim.Held()
			end.dead = dead
			// last && a clean exit is the shape CLA-384 is about: the phase ran to
			// completion, pushed, and simply did not declare. A non-zero exit reached
			// the same place by crashing, which is a different failure with its own
			// reporting, and an earlier phase reaching here did not have declaring as
			// its job at all.
			d.releaseHeldClaim(ctx, t, res, last && res.ExitCode == 0)
		}
		// Then check what it said it delivered. After the handback, because a dead
		// lease is time-sensitive and a git check is not; on every exit, because a
		// session that pushed nothing and died is exactly as likely to have recorded
		// a branch as one that finished cleanly.
		d.verifyDeliveries(ctx, t, res)

		if ierr != nil {
			if ctx.Err() != nil {
				return tokens, cost, true, end, nil
			}
			// Couldn't launch the harness at all (bad PATH/flags/env) — not a blip.
			return tokens, cost, false, end, fmt.Errorf("invoke %s: %w", a.Name(), ierr)
		}

		// A session whose stream could not be read whole (CLA-262). Everything below
		// this line reads a figure parsed out of that stream, so counting the spend,
		// classifying the exit or retrying on the strength of it are three ways to
		// make a confident decision on data with a hole in it.
		if res.Untrusted != "" {
			// held is nil here by construction — an untrusted stream never holds a
			// phase open — so the sequence ends and nothing is carried forward.
			utokens, ucost, ustop, uerr := d.endUntrustedDrain(drainNum, d.cfg.HarnessFor(ph), res, tokens, cost)
			return utokens, ucost, ustop, end, uerr
		}

		// Count THIS attempt's spend toward the budget breaker regardless of how it
		// ends — usage-limit, transient, stop, or clean. A failed/retried attempt
		// still burned tokens, and a "leave headroom" breaker must err toward seeing
		// real spend, not under-counting it. Each attempt is a distinct session, so
		// summing per attempt (and returning the accumulator, not the final res)
		// counts every session exactly once.
		tokens += res.Tokens
		cost += res.CostUSD
		d.charge(d.cfg.HarnessFor(ph), res.Tokens, res.CostUSD)

		// A usage limit. A rolling-window subscription cap is waited out and the
		// session re-run; a hard budget/credit exhaustion (Stop) has no reset to
		// poll for, so the run stops cleanly and the operator resumes it once
		// they've topped up.
		if lim := a.DetectLimit(res); lim.Limited {
			if lim.Stop {
				log.Printf("iteration %d stopped: %s — no reset to wait for, stopping (resume once resolved)",
					drainNum, limitReason(lim))
				return tokens, cost, true, end, nil
			}
			log.Printf("iteration %d hit a usage limit", drainNum)
			// A RESUMED phase must not wait one out. It is working a live lease —
			// 30 minutes, and nothing driver-side heartbeats it — so a supervised
			// wait of hours would re-spawn the phase against a run the plane has
			// long since swept or handed to another clanker, with a runId in its
			// prompt that no longer means anything. End the sequence instead: the
			// branch is pushed, the lease lapses, and the task comes back as a
			// takeover, which is the designed recovery for exactly this.
			if prev != nil {
				log.Printf("iteration %d: a resumed phase hit a usage limit — ending the sequence rather than waiting out a reset on a 30-minute lease; the task returns as a takeover with its branch recorded",
					drainNum)
				return tokens, cost, true, end, nil
			}
			// Whatever this attempt held open is not held open any more: every path
			// below either waits or retries with a FRESH session, which re-claims.
			// Pre-phases the handback ran on every attempt, so this could not arise.
			end = d.releaseCarriedEnd(ctx, t, end)
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
				return tokens, cost, true, end, nil
			}
			// The wait's own probe spend lands in the SAME accumulator as the
			// sessions', so it reaches the breaker at the top of this loop, the one in
			// Run between drains, and the iteration's cost line — rather than a
			// separate figure nothing is measured against (CLA-287).
			// Probed on the harness that hit the limit, in that harness's own
			// session shape: a usage limit is a property of one provider's account,
			// so probing the run's default harness would answer a question nobody
			// asked, and spend on it.
			ptokens, pcost, pstop := d.supervisedWait(ctx, lim, a, ph, t, spend{
				start:  prior.start,
				tokens: prior.tokens + tokens,
				cost:   prior.cost + cost,
			})
			tokens += ptokens
			cost += pcost
			if pstop {
				return tokens, cost, true, end, nil
			}
			continue
		}

		// Below the limit branch, so a usage-limit attempt is counted NEITHER way.
		// A session the subscription cap turned away often reports nothing at all
		// - under claude the notice can arrive on stderr with no stream, and under
		// codex as free text - so counting it as silence would end the run on the
		// ordinary overnight quota wait, and blame a harness that is starting
		// perfectly well. It is not a reset either: the cap is a known cause with
		// its own breakers (the budget check inside supervisedWait, waitPastBudget,
		// and the probes' own spend), and clearing the count on it would let a
		// limit between silences hide the loop this bounds.
		//
		// Everything from here down is an attempt that RAN and ended on its own
		// terms, which is where "did it tell us anything?" is the right question -
		// a different question from what it told us, since a report of zero is
		// still a report (CLA-288). Counted here, enforced at the top of the loop,
		// so the bound fires on the decision to spawn again rather than on this
		// attempt's exit.
		if res.UsageReported {
			noUsage = 0
		} else {
			noUsage++
		}

		if res.ExitCode == 0 {
			// A clean exit is the shape the CLA-398 quiet death wears, so the
			// marker gets named here rather than inferred downstream: a final
			// step_finish with reason "unknown" and all-zero usage used to read
			// as "iteration done (tokens=0 cost=$0.00)" — indistinguishable from
			// a cheap clean run, which is exactly what made these deaths
			// invisible. The adapter's own marker is the only thing that can
			// tell them apart.
			if zeroUsage {
				log.Printf("iteration %d: the session ended with the zero-usage-unknown signature (final step reason %q, all-zero usage) — a quiet death, not a clean run",
					drainNum, harness.FinishReasonUnknown)
			} else {
				log.Printf("iteration %d done (tokens=%d cost=$%.4f)", drainNum, tokens, cost)
			}
			return tokens, cost, false, end, nil
		}

		// A turn-capped end is this PHASE ending, never the run failing. It exits
		// non-zero and matches neither the limit scan nor the transient one, so
		// without this it would fall through to the non-retryable branch below and
		// stop the daemon — the backstop killing what it was added to protect.
		// Not retried either: the cap would fire again at the same place.
		if capped {
			log.Printf("iteration %d: the session hit its turn cap (tokens=%d cost=$%.4f) — ending this phase; anything uncommitted was salvaged above",
				drainNum, tokens, cost)
			return tokens, cost, false, end, nil
		}

		// A session the ADAPTER killed for crossing its per-session token ceiling
		// is the same shape as a turn cap: an orderly end with the salvage's
		// problem to handle, never a failure and never a retry. Retrying would
		// re-spend against the same runaway ceiling; failing would stop the run
		// over a kill that did its job (CLA-343).
		//
		// The cost is reported as unmeasured, never as a number: the result event
		// — the only carrier of total_cost_usd — is deliberately not parsed on a
		// killed stream, so res.CostUSD is 0 and printing it would under-report
		// the drain's spend as exactly nothing. The tokens are the honest figure.
		if ceiling {
			log.Printf("iteration %d: the session crossed its per-session token ceiling (tokens=%d; cost not captured — killed mid-stream) — ending this phase; anything uncommitted was salvaged above",
				drainNum, tokens)
			return tokens, cost, false, end, nil
		}

		// A session the ADAPTER ended for outliving its wall-clock cap is the same
		// shape again: the phase ends, and neither a retry nor a failure is right —
		// a retry would spend the same hours over again and reach the same deadline,
		// and failing would stop the run over a cap doing its job (CLA-368).
		//
		// Spend IS reported here, unlike the token-ceiling branch: opencode's usage
		// arrives per step_finish and is summed all the way to the kill, so the
		// figures are the honest cost of the session up to the moment it ended.
		//
		// What the line must NOT say is that the salvage handled the tree. The
		// salvage returns immediately without a claim (see salvageStrandedWork),
		// and the only adapter that enforces this cap today is the one that does
		// not observe claims — so on every kill that can currently happen, nothing
		// was committed and the work is still sitting in the worktree. Saying
		// otherwise would be the reassuring falsehood doctor's own checks exist to
		// remove (CLA-290). The claim-held wording is the one a claim-observing
		// adapter will earn later; it is not what opencode gets today.
		if wallclock {
			if res.Claim.Held() {
				log.Printf("iteration %d: the session outlived its wall-clock cap (tokens=%d cost=$%.4f) — ending this phase; anything uncommitted was salvaged above",
					drainNum, tokens, cost)
			} else {
				log.Printf("iteration %d: the session outlived its wall-clock cap (tokens=%d cost=$%.4f) — ending this phase. NOTHING was salvaged: this harness does not observe the session's task claim, so whatever the session left uncommitted is still in the worktree",
					drainNum, tokens, cost)
			}
			return tokens, cost, false, end, nil
		}

		// Non-zero exit, not the usage cap: a transient server/network blip backs
		// off and retries; anything else is a genuine failure and stops.
		if a.IsTransient(res) {
			// A RESUMED phase does not retry, for the same reason it does not wait
			// out a usage limit — and this path is the more dangerous of the two,
			// because MaxRetries defaults to 0, meaning never give up. Seeding the
			// resumed claim made the seam's handback live for phase 2, so a blip
			// here hands the task back mid-sequence while the retry is handed the
			// very same heartbeat(runId) brief; and where the handback is declined
			// (HasWIP), the backoff ladder walks past the 30-minute lease in about
			// six steps and then re-spawns, indefinitely, against a run the plane
			// has swept. End the sequence: the branch is pushed, the lease lapses,
			// and the task comes back as a takeover.
			if prev != nil {
				log.Printf("iteration %d: a resumed phase hit a transient failure (%s) — ending the sequence rather than retrying on a 30-minute lease nothing renews; the task returns as a takeover with its branch recorded",
					drainNum, res.ExitString())
				return tokens, cost, true, end, nil
			}
			// A retry the adapter could not NAME is bounded on its own, because
			// nothing else bounds it: the dials are off by default and the
			// zero-spend counter is reset by the usage this heuristic requires. The
			// diagnostic goes in the error because it is the whole remedy - the
			// adapter has no pattern for this text, so an operator who can see it
			// can say whether one should be added (CLA-381).
			if a.IsUnclassifiedTransient(res) {
				unclassified++
				if unclassified > unclassifiedRetryBound {
					return tokens, cost, false, end, fmt.Errorf(
						"iteration %d: %w: %d retries on %s failures that matched no known-transient pattern - retried because the session had reported usage, but the same unrecognised failure keeps happening, so this is a real fault rather than a blip%s",
						drainNum, errUnclassifiedRetryLoop, unclassified-1, a.Name(), failureDetail(a.Diagnostic(res)))
				}
			}
			// As in the limit branch: the retry is a fresh session that re-claims,
			// so nothing may stay held open across it.
			end = d.releaseCarriedEnd(ctx, t, end)
			retries++
			if d.cfg.MaxRetries > 0 && retries > d.cfg.MaxRetries {
				return tokens, cost, false, end, fmt.Errorf(
					"iteration %d: transient failures persisted after %d retries (check https://status.claude.com; rerun to resume)",
					drainNum, d.cfg.MaxRetries)
			}
			wait := d.backoff(retries)
			log.Printf("iteration %d transient failure (%s) — %s in %s (a fresh session reclaims any half-done task)",
				drainNum, res.ExitString(), retryLabel(retries, d.cfg.MaxRetries), wait)
			if d.waitOrStop(ctx, wait) {
				return tokens, cost, true, end, nil
			}
			continue
		}

		// Stopping here ends the whole run, so say WHAT was judged non-retryable.
		// Without it the operator gets "exited 1 (non-retryable)" and no way to
		// tell a genuine failure from a blip the classifier merely does not know
		// yet — which is the one thing they need in order to report the gap. The
		// text is the harness's own diagnostic scope, never the raw stream, so
		// the agent's narration is not quoted back at them (CLA-258).
		return tokens, cost, false, end, fmt.Errorf("iteration %d: %s %s (non-retryable) — stopping%s%s",
			drainNum, a.Name(), res.ExitString(), failureDetail(a.Diagnostic(res)), droppedNote(res.OutputDropped))
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
//
// The harness NAME is the phase's own, not the run's: whether a spend ceiling is
// live is a per-harness question since CLA-367, and under per-phase harnesses
// (CLA-366) the session that went unreadable is not necessarily on d.h. Asking
// d.h would let an unreadable opencode session be waved through on the grounds
// that CLAUDE has no ceiling set.
func (d *Driver) endUntrustedDrain(drainNum int, harnessName string, res harness.Result, tokens int, cost float64) (int, float64, bool, error) {
	log.Printf("iteration %d UNTRUSTED — %s", drainNum, res.Untrusted)
	log.Printf("iteration %d: not counting this session's parsed spend (tokens=%d cost=$%.4f — a floor, not a total), not classifying its exit (%s), and not handing back any claim it appeared to hold",
		drainNum, res.Tokens, res.CostUSD, res.ExitString())
	if !d.cfg.Budget.CountsSpendFor(harnessName) {
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
func (d *Driver) salvageStrandedWork(ctx context.Context, t Target, res *harness.Result, producedNothing bool) {
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
			if producedNothing {
				// CLA-386: the tree is clean because nothing was ever written, not
				// because the work landed. The line must say so plainly — "nothing to
				// salvage" reads identically either way, and the only other
				// discriminator (a `verified <ref>: origin/<branch>` line) is an
				// absence, which is the weakest possible signal. Gated on the driver's
				// own classification (not the raw deadPhase predicate): a capped or
				// untrusted session is an orderly end whose finish reason is its own
				// marker or cannot be read, and must not be handed the emphatic line.
				log.Printf("%sphase produced nothing for %s — the worktree is clean because no work was ever written to it, not because it was committed: %s",
					labelOf(t), label, out.Detail)
			} else {
				log.Printf("%snothing to salvage for %s: %s", labelOf(t), label, out.Detail)
			}
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

// deadReason renders the finish reason a dead phase was classified on, for a log
// line. The classification fired on FinishReasonUnknown, so a nil claim — a
// session that observed no task — still has a reason to name.
//
// The adapter's OWN terminal_reason marker wins when present: ZeroUsageReason
// (CLA-398) names the quiet-death signature more precisely than the bare
// "unknown" it rides on, so the operator's log says which flavour of death this
// was instead of the driver re-inferring it from the zero usage downstream.
func deadReason(res *harness.Result) string {
	if res != nil {
		if r, ok := res.Raw[harness.TerminalReasonKey].(string); ok && r != "" {
			return r
		}
		if r, ok := res.Raw[harness.FinishReasonKey].(string); ok && r != "" {
			return r
		}
	}
	return harness.FinishReasonUnknown
}

// fleetTrip pauses a target whose fleet dead-phase counter has reached
// fleetDeadBound (CLA-396): that many consecutive dead phases across tasks mean
// the harness or provider is broken right now, not the tasks, and every further
// session would pay the same death rate for nothing. The target stops spawning
// until the operator answers the raised project-level question (the open-question
// count falls back to what it was at the raise — see the poll gate in Run).
//
// The question is PROJECT-level — ask_question with no taskId — because a fleet
// trip is not one task's triage: pinning it to whichever task happened to be in
// flight would make the operator answer about a bystander. It is filed as a
// non-blocking `decision` — there is no task to block (the loop's pause is the
// enforcement), and the operator's ruling on a fleet incident is a project-level
// judgment worth standing in the decision log, unlike the park's one-task
// clarification.
//
// "Exactly one" is guaranteed by fleetRaised: while the pause is in force the
// flag is set, so a later trip in the same episode does not re-raise. The flag
// clears with the pause, so a NEW episode — the operator answered, the loop
// resumed, and the fleet died again — raises a fresh question.
//
// If the question cannot be filed, the pause is NOT set: a pause the operator
// can never be told about and can never resume is a permanent stall, which is
// worse than the fall-through (per-task parking still bounds each task). The
// dead-phase counter stays at the bound, so the next dead phase tries again —
// loudly.
func (d *Driver) fleetTrip(ctx context.Context, t Target, ti, drainNum int, res *harness.Result) {
	pk, ok := t.Releaser.(plane.ParkAPI)
	if !ok {
		log.Printf("%siteration %d: %d consecutive dead phases across tasks, but the releaser cannot file a project question — NOT pausing (the operator could never resume it); the per-task budget still bounds each individual task",
			d.prefix(ti), drainNum, d.fleetDead[ti])
		return
	}
	if !d.fleetRaised[ti] {
		d.fleetRaised[ti] = true
		// The baseline the resume signal is measured against: the open-question
		// count at the last poll, BEFORE the question below raises it by one.
		d.fleetOpenQ[ti] = d.openQs[ti]
		body := fmt.Sprintf(
			"**%d consecutive dead phases across tasks — the provider or harness looks broken, not the tasks.** Each task died producing nothing (final step reason %q, no branch recorded) before this, so the driver paused this project rather than burning more sessions against a provider that is already known to be failing.\n\nThe loop is PAUSED for this project until you answer. Answering resumes it; the next dead phase will pause it again and raise a fresh question. Iteration logs are in %s.",
			d.fleetDead[ti], deadReason(res), d.state.Path())
		options := []string{
			"The provider recovered — resume draining this project",
			"Still investigating — I will answer when it is safe to resume",
			"Switch or restart the harness — try again",
			"Something else is wrong — stop the daemon",
		}
		pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer pcancel()
		if err := pk.AskProjectQuestion(pctx, body, options, "decision"); err != nil {
			log.Printf("%siteration %d: the fleet dead-phase question could not be filed: %v — NOT pausing (the operator could never resume it); the next dead phase will retry the raise",
				d.prefix(ti), drainNum, err)
			d.fleetRaised[ti] = false
			return
		}
	}
	d.fleetPaused[ti] = true
	log.Printf("%siteration %d: %d consecutive dead phases across tasks — the provider or harness looks broken, not the tasks; pausing this project until the operator answers the raised question",
		d.prefix(ti), drainNum, d.fleetDead[ti])
}

// parkDeadPhase parks the task after four consecutive dead phases, filing an
// OPEN question so the park reaches the operator instead of vanishing into the
// Done tab (CLA-395). The question is a non-blocking `clarification` — the task
// is already parked, so there is nothing left to block, and a `decision` would
// read as project-wide standing law rather than one task's triage. Its body and
// options are modelled on the plane's own escalateExhausted shape: what
// happened, the dead-phase signature, how many sessions it cost, where the
// iteration logs are, and options covering retry / re-scope / leave parked /
// drop (CLA-386, per the 2026-08-20 operator decision: retry the implement
// phase four times, then park; the budget is per task).
//
// The phase is dead — its sessions are gone — so nobody is left to declare the
// failure; the driver must. The claim was released before this ran (the retry
// re-claimed, then died again), so the park is a plain status write signed by
// the dead phase's own run.
func (d *Driver) parkDeadPhase(ctx context.Context, t Target, phaseLabel string, res *harness.Result) {
	if res == nil || res.Claim.TaskID == "" {
		log.Printf("%sdead-phase park: no claim observed, so there is no task to park", labelOf(t))
		return
	}
	pk, ok := t.Releaser.(plane.ParkAPI)
	if !ok {
		log.Printf("%sCANNOT PARK %s — four consecutive dead phases (final step reason %q, no branch recorded), but the releaser cannot park or raise a question; the task stays in the queue and the next claim will retry it again",
			labelOf(t), claimLabel(res.Claim), deadReason(res))
		return
	}
	sig := deadReason(res)
	ref := claimLabel(res.Claim)
	outcome := fmt.Sprintf(
		"Parked after four consecutive dead phases: the %s session died producing nothing (final step reason %q, no branch recorded) on each attempt. Per the 2026-08-20 operator decision, a task that can kill four full sessions reaches the operator rather than a fifth; the retry budget is per task. Iteration logs are in %s.",
		phaseLabel, sig, d.state.Path())
	body := fmt.Sprintf(
		"**%s has defeated four sessions and is now parked.** The %s phase died producing nothing (final step reason %q, no branch recorded) on four consecutive attempts, so the driver stopped rather than spending a fifth session on it.\n\n"+
			"The task is `parked` — nothing will pick it up until an operator decides what to do with it. The iteration logs are in %s.",
		ref, phaseLabel, sig, d.state.Path())
	options := []string{
		"Set it back to `ready` — the blocker is gone, let the fleet try again",
		"Leave it parked — I will work it myself",
		"It needs re-scoping before anyone tries again",
		"Drop it — this is not worth another attempt",
	}

	pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer pcancel()
	if err := pk.Park(pctx, res.Claim.TaskID, res.Claim.RunID, outcome); err != nil {
		log.Printf("%spark failed for %s: %v — the task is left for the next claim to retry it again",
			labelOf(t), claimLabel(res.Claim), err)
		return
	}
	if err := pk.AskQuestion(pctx, res.Claim.TaskID, body, options, false, "clarification"); err != nil {
		// The task IS parked — it will not be retried — so saying it was "left
		// for the next claim" would be the opposite of what happened. The park
		// outcome stands alone; the missing question is the failure the operator
		// needs to know about (review finding).
		log.Printf("%sparked %s — four consecutive dead phases, but the question for the operator could not be filed: %v",
			labelOf(t), claimLabel(res.Claim), err)
		return
	}
	log.Printf("%sparked %s — four consecutive dead phases, question filed for the operator",
		labelOf(t), claimLabel(res.Claim))
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
// releaseCarried hands back a claim a phase was holding open for a successor
// that is not going to run, and returns nil so the caller cannot hand it back
// twice. A nil carried claim is a no-op, so callers need not check first.
//
// It exists because "held open across the seam" and "retry this phase with a
// fresh session" are contradictory: before phases, releaseHeldClaim ran on every
// attempt and neither the limit wait nor the transient backoff could leave a
// lease unattended. Both loop back to a session that re-claims, so both have to
// undo the hold first.
func (d *Driver) releaseCarried(ctx context.Context, t Target, carried *harness.Result) *harness.Result {
	if carried == nil {
		return nil
	}
	d.releaseHeldClaim(ctx, t, *carried, false)
	return nil
}

// releaseCarriedEnd is releaseCarried for a phase about to loop back to a FRESH
// session — a usage-limit wait, a transient backoff. It hands back whatever the
// attempt was holding open and returns an end that owes nothing further, so a
// seam's hold cannot survive into a retry that is going to re-claim anyway.
func (d *Driver) releaseCarriedEnd(ctx context.Context, t Target, end phaseEnd) phaseEnd {
	// Only a hold that actually happened needs undoing. Every other end has
	// ALREADY been through the handback in drainPhase's seam block, and releasing
	// again would hand the task back twice — which on a usage-limit exit is the
	// common path, not a corner: the seam releases (the phase did not reach its
	// checkpoint), and then the limit branch loops back to wait.
	if !end.checkpoint || end.claim == nil {
		return end
	}
	// Read BEFORE the call, for the same reason drainPhase reads it: a zero or
	// settled claim releases nothing, and reporting it as released would erase a
	// live claim upstream.
	held := end.claim.Claim.Held()
	d.releaseCarried(ctx, t, end.claim)
	return phaseEnd{released: held}
}

// countUndeclared says whether THIS call site is the one where a held-with-work
// claim means the CLA-384 failure: a phase that finished cleanly and did not hand
// the task over. Only the seam's final-phase handback passes true. Every other
// caller reaches the same branch for a reason that is not a failure to declare -
// a budget trip between phases, a crash, a transient retry, a usage-limit wait, a
// refused handoff - and counting those would make the number mean nothing, which
// is the only thing it has going for it.
func (d *Driver) releaseHeldClaim(ctx context.Context, t Target, res harness.Result, countUndeclared bool) {
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
		// Leaving the lease alone is right — see Releasable. Whether it is also a
		// FAILURE depends entirely on which call site got here; only that caller
		// knows, so only that caller says so.
		if countUndeclared {
			d.undeclared++
			log.Printf("%ssession ended holding %s, which has pushed work — leaving the lease to expire so the takeover hand-off survives (undeclared hand-offs this run: %d — the phase finished but never moved the task on, so the next clanker pays to rediscover it)",
				labelOf(t), res.Claim.TaskID, d.undeclared)
			return
		}
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

// quietBackoff is how long a target sits out once its fruitless spend crosses
// the quiet-token ladder: 15m, 30m, 1h, then 2h. It never reaches "never" — the
// blocker is usually an operator answering a question, and a target that can
// never come back would need a restart to notice.
//
// rung counts how many quietTokenThresholds the accumulator has crossed, offset
// by quietThreshold so the FIRST crossing lands on the same 15m the old
// session-count ladder started at.
func quietBackoff(rung int) time.Duration {
	const (
		base = 15 * time.Minute
		cap_ = 2 * time.Hour
	)
	d := base
	for i := quietThreshold; i < rung; i++ {
		if d *= 2; d >= cap_ {
			return cap_
		}
	}
	return d
}

// quietThreshold is how many quietTokenThresholds of fruitless spend it takes
// to back a target off — kept as the ladder's base rung so quietBackoff's shape
// is unchanged from the session-count version.
//
// The old "three, not one or two" rationale survives in token terms: a single
// fruitless drain is ordinary, and so is a second — a genuinely large task can
// span several sessions before anything reaches a reviewer, and backing off
// then would punish exactly the deep work this loop exists to do. ~80M of
// fruitless spend is a different claim, and against the run this was written
// for — which repeated the same no-op ten times at ~26M each — the threshold
// still saves most of them.
const quietThreshold = 3

// quietTokenThreshold is the token-denominated replacement for the old
// session-count trigger (CLA-343). Calibration: the old threshold of 3
// fruitless SESSIONS fired at ~78M tokens (~26M per measured iteration — the
// audit's own figure); 80M lands within one drain of that (the third drain at
// 26M each totals 78M, just under, and the fourth crosses). The drift is
// deliberate and lenient, and it buys the property the session count could
// never give: ONE huge fruitless drain trips the first 15-minute sit-out on its
// own — the 285.9M runaway would have earned it at under a third of its spend
// instead of running three sessions past it.
const quietTokenThreshold = 80_000_000

// judgeProgress reads this poll as the verdict on the target's last drain, and
// backs the target off once its fruitless drains have spent enough tokens
// achieving nothing.
//
// Progress is `Settled()` rising — work reaching a reviewer or finishing. A drain
// that only recorded why it could not proceed bumps `version` but settles
// nothing, which is exactly the run this exists to stop repeating. A fruitless
// drain charges its tokens (Driver.spent) to the accumulator; progress clears
// the accumulator.
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
		if d.quietTokens[i] > 0 {
			log.Printf("%snothing to spawn for — idle, not fruitless; forgetting %d tokens of drain(s) that settled nothing and clearing any back-off", d.prefix(i), d.quietTokens[i])
		}
		d.quietTokens[i], d.skipUntil[i] = 0, time.Time{}
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
		if d.quietTokens[i] > 0 {
			switch {
			case settled > d.baseline[i]:
				log.Printf("%sprogress from elsewhere — clearing the no-progress back-off", d.prefix(i))
				d.quietTokens[i], d.skipUntil[i] = 0, time.Time{}
			case openQs < wasOpen:
				log.Printf("%sopen questions fell %d -> %d, so something was resolved: clearing the no-progress back-off",
					d.prefix(i), wasOpen, openQs)
				d.quietTokens[i], d.skipUntil[i] = 0, time.Time{}
			}
		}
		d.baseline[i], d.openQs[i] = settled, openQs
		return
	}

	d.pending[i] = false
	spent := d.spent[i]
	d.spent[i] = 0
	if settled > d.baseline[i] {
		d.quietTokens[i] = 0
		d.baseline[i], d.openQs[i] = settled, openQs
		return
	}

	// The strike is earned whatever else moved: a session that answered its own
	// question has not thereby delivered anything, and the drain is being judged on
	// what it SETTLED. But an answer that landed while the drain was running is the
	// operator doing the very thing the back-off message asked for, and this poll is
	// the only one that can ever see it: the baseline advances just below, so a fall
	// consumed here is gone rather than deferred. Take the strike, skip the sit-out.
	// The target gets one immediate retry against the answer; if that settles
	// nothing too, the retry's spend is charged like any other, so the rung it
	// reaches depends on how big it was (a whole-threshold retry escalates at once,
	// a smaller one repeats the band) — the retry is never an exemption.
	answered := openQs < wasOpen
	d.quietTokens[i] += spent
	d.baseline[i], d.openQs[i] = settled, openQs
	// Bands, matching the old session-count ladder at the ~26M-per-drain
	// calibration: 3 fruitless drains (~78M) earned 15m, 4 (~104M) earned 30m,
	// 5 (~130M) earned 1h, 6+ (~156M) earned the 2h cap. In whole thresholds:
	// acc in [80M,160M) → rung 3 → 15m; [160M,240M) → 30m; [240M,320M) → 1h;
	// 320M+ → 2h. One huge fruitless drain (≥80M) trips the FIRST band on its
	// own — the property the session count could never give (CLA-343).
	if d.quietTokens[i] < quietTokenThreshold {
		return
	}
	if answered {
		log.Printf("%s%d tokens spent on sessions that settled nothing, but open questions fell %d -> %d while the last one ran - retrying once before backing off",
			d.prefix(i), d.quietTokens[i], wasOpen, openQs)
		return
	}
	wait := quietBackoff(2 + d.quietTokens[i]/quietTokenThreshold)
	d.skipUntil[i] = time.Now().Add(wait)
	// Name the RIGHT cause. The original sentence asserts there is claimable work
	// and lists the reasons a ready task resists being finished — all of which are
	// wrong, and misdirecting, when the only thing that opened the gate was an
	// abandoned branch (CLA-274). Sending an operator to look for an unanswered
	// question when the real subject is stranded WIP costs them the diagnosis.
	if sum.Claimable == 0 && sum.StaleClaimable > 0 {
		log.Printf("%s%d tokens spent on sessions that settled nothing — backing off for %s. Nothing is READY; the sessions were spawned to recover %d abandoned branch(es), and are not finishing them. Check the last iteration log, and the branch itself.",
			d.prefix(i), d.quietTokens[i], wait, sum.StaleClaimable)
		return
	}
	log.Printf("%s%d tokens spent on sessions that settled nothing — backing off for %s. The queue says there is claimable work, so something is stopping it being DONE: an unanswered question, a gate, a toolchain the session cannot run. Check the last iteration log.",
		d.prefix(i), d.quietTokens[i], wait)
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
func (d *Driver) supervisedWait(ctx context.Context, lim harness.Limit, a harness.Adapter, ph config.Phase, t Target, sofar spend) (tokens int, cost float64, stop bool) {
	interval := d.cfg.PollInterval.OrDefault(30 * time.Minute)
	grace := 1 * time.Minute

	waitUntilReset := func() time.Duration {
		if lim.ResetAt.IsZero() || time.Now().After(lim.ResetAt) || time.Now().Equal(lim.ResetAt) {
			return interval
		}
		// A known reset in the future: wait until ResetAt + grace, capped at now+
		// interval so STOP stays responsive (a multi-hour uninterrupted sleep makes
		// the stop switch feel dead). The cap is only when reset is in the future.
		untilReset := time.Until(lim.ResetAt.Add(grace))
		untilInterval := interval
		if untilReset < untilInterval {
			return untilReset
		}
		return untilInterval
	}

	waitingForReset := !lim.ResetAt.IsZero() && time.Now().Before(lim.ResetAt)
	if waitingForReset {
		log.Printf("paused%s — waiting until that reset (with grace), capped at poll interval", resetSuffix(lim.ResetAt))
	} else {
		log.Printf("paused%s — probing every %s for an early reset", resetSuffix(lim.ResetAt), interval)
	}

	for {
		if dim := d.budgetTrip(sofar.tokens+tokens, sofar.cost+cost, time.Since(sofar.start)); dim != "" {
			log.Printf("budget reached while paused: %s — stopping rather than waiting out the limit (tokens=%d cost=$%.2f elapsed=%s)",
				dim, sofar.tokens+tokens, sofar.cost+cost, time.Since(sofar.start).Round(time.Second))
			return tokens, cost, true
		}
		waitDur := waitUntilReset()
		if d.waitOrStop(ctx, waitDur) {
			return tokens, cost, true
		}
	// Only fall back to interval polling (instead of resuming immediately) when
		// the reset actually passed during this supervised wait. When the reset was
		// already past at entry (e.g. a resumed session whose reset expired during the
		// previous phase), keep the old behaviour: resume straight away without
		// requiring a probe to confirm it.
		if !lim.ResetAt.IsZero() && time.Now().After(lim.ResetAt) && waitingForReset {
			log.Print("stated reset passed — falling back to interval polling (reset claim is not a guarantee)")
			waitingForReset = false
		} else if !lim.ResetAt.IsZero() && time.Now().After(lim.ResetAt) && !waitingForReset {
			// Reset was already past at entry: resume immediately, preserving the
			// existing behaviour (see TestDrainWithRetries_StatedResetPassed).
			log.Print("stated reset already past — resuming")
			return tokens, cost, false
		}
		got, err := a.Probe(ctx, d.invocationOn(t, d.cfg.HarnessFor(ph), true))
		// Count the probe BEFORE reading its verdict, and before the error branch: a
		// probe that failed still spawned the harness and still spent. Every path out
		// of here — resume, stop, or another lap — carries it.
		tokens += got.Tokens
		cost += got.CostUSD
		d.charge(d.cfg.HarnessFor(ph), got.Tokens, got.CostUSD)
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
			if errors.Is(err, harness.ErrUntrusted) && d.cfg.Budget.CountsSpendFor(d.cfg.HarnessFor(ph)) {
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

// invocationFor builds the invocation for one PHASE: the target's invocation,
// carrying this phase's prompt and turn cap, with the previous phase's claim
// substituted into the prompt's placeholders.
//
// The substitution is what lets a later phase RESUME a run rather than claim a
// task. A session cannot know its own run id — it never called claim_task — so
// the driver, which watched the earlier phase's stream go by, is the only thing
// that can tell it. That is also why the ids come from the observed claim and
// not from config: they are per-task facts, discovered at runtime.
//
// A placeholder with no claim to fill it is LEFT STANDING rather than blanked.
// A literal {{runId}} in the log is a misconfigured sequence announcing itself;
// an empty string is a session quietly deciding to claim fresh work instead.
func (d *Driver) invocationFor(t Target, phaseIdx int, ph config.Phase, prev *harness.Result) harness.Invocation {
	inv := d.invocationOn(t, d.cfg.HarnessFor(ph), false)
	inv.MaxTurns = ph.MaxTurns
	// The per-session runaway ceiling, resolved once per drain from the budget:
	// the operator's own dial, else 2x max_tokens, else the documented floor.
	// 0 never reaches here — SessionTokenCeiling has no disabled state, because
	// the whole point of CLA-343 is that nothing was able to stop the 285.9M
	// session.
	// Asked PER HARNESS since CLA-367: a token ceiling that lives in this
	// harness's own per_harness block derives the detector exactly as the global
	// dial does, so moving a ceiling into a block does not loosen it. And the
	// harness asked about is THIS PHASE's (CLA-366), not the run's — the ceiling
	// is compiled into the invocation that is about to spawn, so keying it on d.h
	// would hand an opencode phase the ceiling derived from claude's block.
	inv.MaxSessionTokens = d.cfg.Budget.SessionTokenCeilingFor(d.cfg.HarnessFor(ph))
	// The per-session wall-clock cap, resolved by EffectivePhases: this phase's
	// own, else the run-wide one, else zero — and zero is OFF, which is the
	// shipped default (CLA-368). Unlike the token ceiling this one HAS a disabled
	// state, because no default number is honest across models and providers.
	inv.MaxSessionWallClock = ph.MaxWallClock.Duration()
	inv.Prompt = ph.Prompt
	// The phase's tier, or the run-wide model when it names none. Reported when a
	// tier was named and resolved to nothing, because that is a typo in the
	// operator's map and the session otherwise runs on the default with nothing
	// said — the quiet failure, since a tier is set precisely when the default is
	// not what was wanted.
	//
	// "resolves to no model" rather than "the map does not define it": ModelForTier
	// also says not-ok for a bucket that IS in the map and is mapped to blank or
	// whitespace, and telling that operator their map lacks a bucket they can see
	// sitting in it would send them looking in the wrong place — the more so
	// because the whitespace is the thing they cannot see. Label(), not Name, so a
	// phase carrying a custom prompt and no name is still identified.
	//
	// Resolved against the tier map of the harness THIS PHASE runs on, not the
	// run's: a bucket name is policy and travels, the alias inside it is a
	// provider's and does not. An opencode phase whose harness block names no
	// models therefore runs on opencode's own configured model — which is where
	// opencode's model lives — rather than on a claude alias its CLI would die on.
	model, ok := d.cfg.ModelForPhase(ph)
	if !ok {
		log.Printf("%sphase %q names tier %q, which resolves to no model on harness %q — running on that harness's default model instead",
			labelOf(t), ph.Label(phaseIdx), ph.Tier, d.cfg.HarnessFor(ph))
	}
	inv.Model = model
	if prev != nil && prev.Claim.Held() {
		inv.Prompt = strings.NewReplacer(
			config.PhaseTaskPlaceholder, prev.Claim.TaskID,
			config.PhaseRunPlaceholder, prev.Claim.RunID,
		).Replace(inv.Prompt)
		// Seed the claim as well as the prompt. The session is told not to claim,
		// so the adapter would observe none — and Result.Claim.Held() is what gates
		// the handback, the salvage and the delivery check. Without this, the phase
		// that pushes the branch and opens the PR is the one running with all three
		// switched off.
		inv.ResumeClaim = prev.Claim
	}
	return inv
}

// invocation builds the harness invocation for one target: the target's workdir
// and .mcp.json (which select the project) over the config's global fields.
func (d *Driver) invocation(t Target, probe bool) harness.Invocation {
	return d.invocationOn(t, d.cfg.Harness, probe)
}

// invocationOn builds the invocation for one target ON A NAMED HARNESS: the
// project's workdir and MCP config over that harness's own fields.
//
// The harness-shaped fields (config dir, MCP config, settings, model) come from
// config.SessionFor, which fills the run-wide values in for the top-level
// harness only — so a single-harness run resolves to exactly what it always did,
// and a phase on another harness gets that harness's block or nothing, never the
// top level's claude-shaped paths.
//
// The MCP config is the one field with two claimants, because it carries two
// facts at once: which PROJECT (its /mcp/<slug>) and which SCHEMA (the
// harness's). The project's per-harness file answers both and wins; the
// project's own file is the top-level harness's, so it only applies there; and
// the harness block's path is the single-project fallback. config.Validate
// refuses the combination that would silently resolve to the wrong project.
func (d *Driver) invocationOn(t Target, harnessName string, probe bool) harness.Invocation {
	workdir := t.WorkDir
	if workdir == "" {
		workdir = d.cfg.WorkDir
	}
	hc := d.cfg.SessionFor(harnessName)
	return harness.Invocation{
		Prompt:        d.cfg.Prompt,
		Model:         hc.Model,
		WorkDir:       workdir,
		MCPConfigPath: d.cfg.ResolveMCPConfig(harnessName, t.MCPConfigPath, t.MCPConfigPaths),
		ConfigDir:     hc.ConfigDir,
		SettingsPath:  hc.SettingsPath,
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
