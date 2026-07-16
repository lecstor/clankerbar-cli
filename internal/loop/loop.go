// Package loop is the driver: a long-running daemon that gates each iteration on
// a cheap backlog read, spends a fresh harness session only when there is
// claimable work, and — crucially — stays alive and keeps polling when the queue
// is empty, so it reacts when questions are answered, items are promoted, or new
// work is filed. On a usage limit it pauses and polls for an early reset. All
// durable state lives in the backlog (over MCP), so a session killed mid-task is
// reclaimed by the next iteration.
package loop

import (
	"context"
	"errors"
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
	cfg     *config.Config
	h       harness.Adapter
	backlog backlog.Poller
	blind   bool // no cheap backlog read available — drain then idle-poll
}

// New builds a Driver.
func New(cfg *config.Config, h harness.Adapter, poller backlog.Poller) *Driver {
	return &Driver{cfg: cfg, h: h, backlog: poller}
}

// Run drives the daemon until STOP/HALT, a ceiling (max-iterations / budget), or
// context cancellation. An empty queue is NOT a stop condition — it idles and
// keeps polling. Returns nil on a graceful stop; an error only on an unexpected
// failure worth surfacing.
func (d *Driver) Run(ctx context.Context) error {
	stateDir := d.cfg.ResolveStateDir()
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	if src := d.cfg.Source(); src != "" {
		log.Printf("config: %s", src)
	}
	idle := d.cfg.IdlePollInterval.OrDefault(60 * time.Second)
	log.Printf("driving %s; state in %s; idle poll every %s", d.h.Name(), stateDir, idle)

	start := time.Now()
	var totalTokens int
	var totalCost float64
	drains := 0

	for {
		if ctx.Err() != nil {
			log.Print("cancelled — stopping")
			return nil
		}
		if present, msg := readMarker(stateDir, "HALT"); present {
			log.Printf("HALT present: %s — resolve and delete %s to resume", msg, filepath.Join(stateDir, "HALT"))
			return nil
		}
		if present, _ := readMarker(stateDir, "STOP"); present {
			_ = os.Remove(filepath.Join(stateDir, "STOP"))
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
				if d.sleep(ctx, idle) {
					return nil
				}
				continue
			default:
				log.Printf("queue: ready=%d claimable=%d in_progress=%d open_questions=%d (v%d)",
					sum.Ready, sum.Claimable, sum.InProgress, sum.OpenQuestions, sum.Version)
				if sum.Claimable == 0 {
					if d.sleep(ctx, idle) {
						return nil
					}
					continue
				}
			}
		}

		// There is work (or we're blind) — spend a session.
		drains++
		log.Printf("iteration %d — spawning %s", drains, d.h.Name())
		res, err := d.h.Invoke(ctx, d.invocation(false))
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("iteration %d: invoke error: %v — stopping (unexpected)", drains, err)
			return err
		}
		totalTokens += res.Tokens
		totalCost += res.CostUSD

		if lim := d.h.DetectLimit(res); lim.Limited {
			log.Printf("iteration %d hit a usage limit", drains)
			if err := d.supervisedWait(ctx, lim); err != nil {
				return err
			}
			continue // retry — clankerbar reclaims any half-done task
		}
		log.Printf("iteration %d done (tokens=%d cost=$%.4f)", drains, res.Tokens, res.CostUSD)

		// In blind mode a "work the backlog" session drains everything ready, so
		// idle before re-attempting. In wired mode, loop straight back — the next
		// cheap poll decides whether more work appeared.
		if d.blind {
			log.Printf("idle — re-checking in %s", idle)
			if d.sleep(ctx, idle) {
				return nil
			}
		}
	}
}

// supervisedWait pauses on a usage limit, then polls for an early reset rather
// than sleeping blindly to the stated reset — the window frees semi-randomly and
// the stated time is only an upper bound.
func (d *Driver) supervisedWait(ctx context.Context, lim harness.Limit) error {
	interval := d.cfg.PollInterval.OrDefault(30 * time.Minute)
	log.Printf("paused%s — probing every %s for an early reset", resetSuffix(lim.ResetAt), interval)

	for {
		if d.sleep(ctx, interval) {
			return nil
		}
		if !lim.ResetAt.IsZero() && time.Now().After(lim.ResetAt) {
			log.Print("stated reset passed — resuming")
			return nil
		}
		got, err := d.h.Probe(ctx, d.invocation(true))
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("probe error: %v — will retry next interval", err)
			continue
		}
		if !got.Limited {
			log.Print("limit lifted — resuming")
			return nil
		}
		log.Print("still limited — continuing to wait")
	}
}

func (d *Driver) invocation(probe bool) harness.Invocation {
	return harness.Invocation{
		Prompt:        d.cfg.Prompt,
		Model:         d.cfg.Model,
		WorkDir:       d.cfg.WorkDir,
		MCPConfigPath: d.cfg.MCPConfigPath,
		Probe:         probe,
	}
}

// sleep waits dur or until the context is cancelled; it reports cancellation.
func (d *Driver) sleep(ctx context.Context, dur time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(dur):
		return false
	}
}

// readMarker reports whether a control marker file exists, and its first line.
func readMarker(dir, name string) (bool, string) {
	b, err := os.ReadFile(filepath.Join(dir, name))
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
