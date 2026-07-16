// Package loop is the driver: it respawns fresh harness sessions that drain the
// backlog, enforces the budget circuit breaker, and turns a usage-limit pause
// into a supervised wait that polls for an early reset. All durable state lives
// in the backlog (over MCP), so a killed session's task is reclaimed by the next.
package loop

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// Driver runs the loop for one harness against one backlog.
type Driver struct {
	cfg *config.Config
	h   harness.Adapter
}

// New builds a Driver.
func New(cfg *config.Config, h harness.Adapter) *Driver {
	return &Driver{cfg: cfg, h: h}
}

// Run drives iterations until the backlog is dry (HALT marker), a stop is
// requested, a limit/budget/iteration ceiling is reached, or the context is
// cancelled. It returns nil on a graceful stop; an error only on an unexpected
// failure worth surfacing.
func (d *Driver) Run(ctx context.Context) error {
	stateDir := d.cfg.ResolveStateDir()
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	if src := d.cfg.Source(); src != "" {
		log.Printf("config: %s", src)
	}
	log.Printf("driving %s; state in %s", d.h.Name(), stateDir)

	start := time.Now()
	var totalTokens int
	var totalCost float64

	for iter := 1; ; iter++ {
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
			log.Printf("STOP requested — stopping before iteration %d", iter)
			return nil
		}
		// TODO(track B): poll the clankerbar console-pause flag over MCP here and
		// treat it like STOP — so an overnight run can be halted from the console.
		if d.cfg.MaxIterations > 0 && iter > d.cfg.MaxIterations {
			log.Printf("reached max-iterations (%d) — stopping", d.cfg.MaxIterations)
			return nil
		}
		if d.cfg.Budget.Exceeded(totalTokens, totalCost, time.Since(start)) {
			log.Printf("budget reached (tokens=%d cost=$%.2f elapsed=%s) — stopping to leave headroom",
				totalTokens, totalCost, time.Since(start).Round(time.Second))
			return nil
		}

		log.Printf("iteration %d — spawning %s", iter, d.h.Name())
		res, err := d.h.Invoke(ctx, d.invocation(false))
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("iteration %d: invoke error: %v — stopping (unexpected)", iter, err)
			return err
		}
		totalTokens += res.Tokens
		totalCost += res.CostUSD

		if lim := d.h.DetectLimit(res); lim.Limited {
			log.Printf("iteration %d hit a usage limit", iter)
			if err := d.supervisedWait(ctx, lim); err != nil {
				return err
			}
			continue // retry — clankerbar reclaims any half-done task
		}

		// TODO: a clankerbar-native "backlog is dry" signal (e.g. the drain
		// session writes HALT, per the skill, or we read backlog counts over MCP)
		// so we stop promptly on an empty queue instead of respawning into nothing.
		log.Printf("iteration %d done (tokens=%d cost=$%.4f)", iter, res.Tokens, res.CostUSD)
	}
}

// supervisedWait pauses on a usage limit, then polls for an early reset rather
// than sleeping blindly to the stated reset — Anthropic frees the 5-hour window
// semi-randomly, and the stated time is only an upper bound.
func (d *Driver) supervisedWait(ctx context.Context, lim harness.Limit) error {
	interval := d.cfg.PollInterval.OrDefault(30 * time.Minute)
	log.Printf("paused%s — probing every %s for an early reset", resetSuffix(lim.ResetAt), interval)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
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
