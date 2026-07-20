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

// Driver runs the loop for one harness against one backlog.
type Driver struct {
	cfg      *config.Config
	h        harness.Adapter
	backlog  backlog.Poller
	stateDir string
	blind    bool // no cheap backlog read available — drain then idle-poll
}

// New builds a Driver.
func New(cfg *config.Config, h harness.Adapter, poller backlog.Poller) *Driver {
	return &Driver{cfg: cfg, h: h, backlog: poller}
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
		// TODO(track B): poll the clankerbar console-pause flag over MCP and treat
		// it like STOP — so an overnight run can be halted from the console.
		if d.cfg.MaxIterations > 0 && drains >= d.cfg.MaxIterations {
			log.Printf("reached max-iterations (%d) — stopping", d.cfg.MaxIterations)
			return nil
		}
		if d.cfg.Budget.Exceeded(totalTokens, totalCost, time.Since(start)) {
			log.Printf("budget reached (tokens=%d cost=$%.2f elapsed=%s) — stopping to leave headroom",
				totalTokens, totalCost, time.Since(start).Round(time.Second))
			return nil
		}

		// Gate on a cheap control-plane read: only spend a session when there is
		// claimable work. When idle, keep polling (and logging) so the loop reacts
		// to answered questions / promotions / newly filed work.
		if !d.blind {
			sum, err := d.backlog.Poll(ctx)
			switch {
			case errors.Is(err, backlog.ErrNotWired):
				log.Print("backlog polling not wired — blind mode: drain, then idle-poll by re-draining (wire backlog polling to gate on live counts cheaply)")
				d.blind = true
			case err != nil:
				log.Printf("backlog poll error: %v — retry in %s", err, idle)
				if d.waitOrStop(ctx, idle) {
					return nil
				}
				continue
			default:
				log.Printf("queue: ready=%d claimable=%d in_progress=%d open_questions=%d (v%d)",
					sum.Ready, sum.Claimable, sum.InProgress, sum.OpenQuestions, sum.Version)
				if sum.Claimable == 0 {
					if d.waitOrStop(ctx, idle) {
						return nil
					}
					continue
				}
			}
		}

		// There is work (or we're blind) — spend a session, retrying transient
		// blips with backoff (a fresh session reclaims any half-done task).
		drains++
		tokens, cost, stop, err := d.drainWithRetries(ctx, drains)
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
func (d *Driver) drainWithRetries(ctx context.Context, drainNum int) (tokens int, cost float64, stop bool, err error) {
	retries := 0
	for {
		// Each attempt streams live to the terminal and to its own logfile.
		inv := d.invocation(false)
		logPath := filepath.Join(d.stateDir, "iteration-"+time.Now().Format("20060102-150405")+".log")
		f, ferr := os.Create(logPath)
		if ferr == nil {
			inv.Console = io.MultiWriter(os.Stderr, f)
		} else {
			inv.Console = os.Stderr
			log.Printf("could not open iteration log %s: %v", logPath, ferr)
		}
		if retries == 0 {
			log.Printf("iteration %d — spawning %s (log: %s)", drainNum, d.h.Name(), logPath)
		} else {
			log.Printf("iteration %d — retry %d, spawning %s (log: %s)", drainNum, retries, d.h.Name(), logPath)
		}

		res, ierr := d.h.Invoke(ctx, inv)
		if f != nil {
			_ = f.Close()
		}
		if ierr != nil {
			if ctx.Err() != nil {
				return 0, 0, true, nil
			}
			// Couldn't launch the harness at all (bad PATH/flags/env) — not a blip.
			return 0, 0, false, fmt.Errorf("invoke %s: %w", d.h.Name(), ierr)
		}

		// A usage limit. A rolling-window subscription cap is waited out and the
		// session re-run; a hard budget/credit exhaustion (Stop) has no reset to
		// poll for, so the run stops cleanly and the operator resumes it once
		// they've topped up.
		if lim := d.h.DetectLimit(res); lim.Limited {
			if lim.Stop {
				log.Printf("iteration %d stopped: %s — no reset to wait for, stopping (resume once resolved)",
					drainNum, limitReason(lim))
				return 0, 0, true, nil
			}
			log.Printf("iteration %d hit a usage limit", drainNum)
			if d.supervisedWait(ctx, lim) {
				return 0, 0, true, nil
			}
			continue
		}

		if res.ExitCode == 0 {
			log.Printf("iteration %d done (tokens=%d cost=$%.4f)", drainNum, res.Tokens, res.CostUSD)
			return res.Tokens, res.CostUSD, false, nil
		}

		// Non-zero exit, not the usage cap: a transient server/network blip backs
		// off and retries; anything else is a genuine failure and stops.
		if d.h.IsTransient(res) {
			retries++
			if d.cfg.MaxRetries > 0 && retries > d.cfg.MaxRetries {
				return 0, 0, false, fmt.Errorf(
					"iteration %d: transient failures persisted after %d retries (check https://status.claude.com; rerun to resume)",
					drainNum, d.cfg.MaxRetries)
			}
			wait := d.backoff(retries)
			log.Printf("iteration %d transient failure (exit %d) — %s in %s (a fresh session reclaims any half-done task)",
				drainNum, res.ExitCode, retryLabel(retries, d.cfg.MaxRetries), wait)
			if d.waitOrStop(ctx, wait) {
				return 0, 0, true, nil
			}
			continue
		}

		return 0, 0, false, fmt.Errorf("iteration %d: %s exited %d (non-retryable) — stopping", drainNum, d.h.Name(), res.ExitCode)
	}
}

// supervisedWait pauses on a usage limit, then polls for an early reset rather
// than sleeping blindly to the stated reset. Returns true if the loop should stop
// (STOP marker or context cancel during the wait).
func (d *Driver) supervisedWait(ctx context.Context, lim harness.Limit) (stop bool) {
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
		got, err := d.h.Probe(ctx, d.invocation(true))
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

func (d *Driver) invocation(probe bool) harness.Invocation {
	return harness.Invocation{
		Prompt:        d.cfg.Prompt,
		Model:         d.cfg.Model,
		WorkDir:       d.cfg.WorkDir,
		MCPConfigPath: d.cfg.MCPConfigPath,
		ConfigDir:     d.cfg.ConfigDir,
		SettingsPath:  d.cfg.SettingsPath,
		Env:           d.cfg.EnvSlice(),
		Probe:         probe,
	}
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
