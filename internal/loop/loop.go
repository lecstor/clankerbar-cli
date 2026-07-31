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
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
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
}

// Driver runs the loop for one harness against one or more backlogs.
type Driver struct {
	cfg      *config.Config
	h        harness.Adapter
	targets  []Target
	paused   []bool // per-target console-pause state, to log transitions once
	cursor   int    // round-robin position over targets (last drained)
	stateDir string
	blind    bool // no cheap backlog read available — drain then idle-poll
}

// New builds a single-project Driver — the original mode, driven entirely by the
// top-level config fields.
func New(cfg *config.Config, h harness.Adapter, poller backlog.Poller) *Driver {
	return NewMulti(cfg, h, []Target{{Poller: poller}})
}

// NewMulti builds a Driver over several targets (CLA-142): one loop instance, one
// account key, many project queues — round-robin over whichever have claimable work.
func NewMulti(cfg *config.Config, h harness.Adapter, targets []Target) *Driver {
	return &Driver{cfg: cfg, h: h, targets: targets, paused: make([]bool, len(targets))}
}

// Run drives the daemon until STOP/HALT, a ceiling (max-iterations / budget), or
// context cancellation. An empty queue is NOT a stop condition — it idles and
// keeps polling. Returns nil on a graceful stop; an error only on an unexpected,
// non-retryable failure.
func (d *Driver) Run(ctx context.Context) error {
	d.stateDir = d.cfg.ResolveStateDir()
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		return err
	}
	if src := d.cfg.Source(); src != "" {
		log.Printf("config: %s", src)
	}
	idle := d.cfg.IdlePollInterval.OrDefault(60 * time.Second)
	log.Printf("driving %s; state in %s; idle poll every %s", d.h.Name(), d.stateDir, idle)

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
			log.Printf("HALT present: %s — resolve and delete %s to resume", msg, filepath.Join(d.stateDir, "HALT"))
			return nil
		}
		if present, _ := d.readMarker("STOP"); present {
			_ = os.Remove(filepath.Join(d.stateDir, "STOP"))
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
		// has claimable work. When idle, keep polling (and logging) so the loop
		// reacts to answered questions / promotions / newly filed work. With several
		// targets (CLA-142) every queue is polled each cycle, and the drain goes to
		// the next claimable one after the last drained (round-robin) — a busy queue
		// can't starve a quiet one.
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
					log.Printf("%squeue: ready=%d claimable=%d in_progress=%d open_questions=%d paused=%t (v%d)",
						d.prefix(i), sum.Ready, sum.Claimable, sum.InProgress, sum.OpenQuestions, sum.Paused, sum.Version)
					// Console pause (CLA-76 plane / CLA-130 driver): the operator can pause
					// a run from the web console, PER PROJECT. Honour it BEFORE the
					// claimable gate so a paused project never gets a new session even when
					// it has claimable work — other projects keep draining. Distinct from
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
						if sum.Claimable > 0 {
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
		tokens, cost, stop, err := d.drainWithRetries(ctx, drains, target, d.cfg.Budget.Deadline(start))
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
		totalTokens += tokens
		totalCost += cost

		// In blind mode a "work the backlog" session drains everything ready, so
		// idle before re-attempting. In wired mode, loop straight back — the next
		// cheap poll decides whether more work appeared.
		if d.blind {
			log.Printf("idle — re-checking in %s", idle)
			if d.waitOrStop(ctx, idle) {
				return nil
			}
		}
	}
}

// drainWithRetries runs one drain to a clean finish, absorbing usage-limit pauses
// (supervised wait) and transient blips (exponential backoff) by re-running the
// SAME session — neither costs a drain count. Returns the tokens/cost consumed on
// a clean finish; stop=true if a STOP/cancel landed during a wait; err only on a
// genuine, non-retryable failure (or exhausted retries).
//
// deadline is when the run's wall-clock ceiling expires (zero = none). It is
// passed down because the budget breaker only runs BETWEEN drains, and the
// supervised wait happens inside one — see waitPastDeadline.
func (d *Driver) drainWithRetries(ctx context.Context, drainNum int, t Target, deadline time.Time) (tokens int, cost float64, stop bool, err error) {
	retries := 0
	for {
		// Each attempt streams live to the terminal and to its own logfile. The name
		// carries the drain number and attempt counter as well as the timestamp: two
		// attempts in the same second (a sub-second backoff) would otherwise share a
		// name and os.Create would truncate the earlier attempt's log.
		inv := d.invocation(t, false)
		logPath := filepath.Join(d.stateDir, fmt.Sprintf("iteration-%s-d%d-a%d.log",
			time.Now().Format("20060102-150405"), drainNum, retries))
		f, ferr := os.Create(logPath)
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
		if ierr != nil {
			if ctx.Err() != nil {
				return tokens, cost, true, nil
			}
			// Couldn't launch the harness at all (bad PATH/flags/env) — not a blip.
			return tokens, cost, false, fmt.Errorf("invoke %s: %w", d.h.Name(), ierr)
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
			if until, over := waitPastDeadline(lim.ResetAt, deadline); over {
				log.Printf("iteration %d: the limit resets at %s, past this run's ceiling (%s) — stopping now rather than waiting; start a fresh run after the reset",
					drainNum, until.Format(time.Kitchen), deadline.Format(time.Kitchen))
				return tokens, cost, true, nil
			}
			if d.supervisedWait(ctx, lim, t) {
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

		return tokens, cost, false, fmt.Errorf("iteration %d: %s exited %d (non-retryable) — stopping", drainNum, d.h.Name(), res.ExitCode)
	}
}

// waitPastDeadline reports whether waiting for resetAt would carry the run past
// deadline. Both being known is required: an unknown reset (zero) is waited out
// as before, because the supervised wait polls for an EARLY lift and the limit
// may clear long before any stated time — and with no wall-clock ceiling there is
// nothing to be past.
func waitPastDeadline(resetAt, deadline time.Time) (time.Time, bool) {
	if resetAt.IsZero() || deadline.IsZero() {
		return time.Time{}, false
	}
	return resetAt, resetAt.After(deadline)
}

// supervisedWait pauses on a usage limit, then polls for an early reset rather
// than sleeping blindly to the stated reset. Returns true if the loop should stop
// (STOP marker or context cancel during the wait).
func (d *Driver) supervisedWait(ctx context.Context, lim harness.Limit, t Target) (stop bool) {
	interval := d.cfg.PollInterval.OrDefault(30 * time.Minute)
	log.Printf("paused%s — probing every %s for an early reset", resetSuffix(lim.ResetAt), interval)

	for {
		if d.waitOrStop(ctx, interval) {
			return true
		}
		if !lim.ResetAt.IsZero() && time.Now().After(lim.ResetAt) {
			log.Print("stated reset passed — resuming")
			return false
		}
		got, err := d.h.Probe(ctx, d.invocation(t, true))
		if err != nil {
			if ctx.Err() != nil {
				return true
			}
			log.Printf("probe error: %v — will retry next interval", err)
			continue
		}
		if !got.Limited {
			log.Print("limit lifted — resuming")
			return false
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
	end := time.Now().Add(dur)
	for {
		if present, _ := d.readMarker("STOP"); present {
			_ = os.Remove(filepath.Join(d.stateDir, "STOP"))
			log.Print("STOP requested during wait — stopping")
			return true
		}
		remaining := time.Until(end)
		if remaining <= 0 {
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
func (d *Driver) readMarker(name string) (bool, string) {
	b, err := os.ReadFile(filepath.Join(d.stateDir, name))
	if err != nil {
		return false, ""
	}
	line := strings.TrimSpace(string(b))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return true, line
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
