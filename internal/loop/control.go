package loop

// Restart and reload control (CLA-461). Three markers sit beside STOP/HALT in
// the state dir, written by `clankerbar ctl`:
//
//	RESTART       finish the in-flight session, then re-exec at the iteration boundary
//	RESTART_NOW   kill the in-flight session now (existing process-group kill),
//	              release any held claim, re-exec
//	RELOAD        re-read the config file at the iteration boundary, no exec
//
// They are honoured exactly where STOP is honoured: at the top of the run loop
// (the ITERATION boundary) and inside waitOrStop (idle polls and supervised
// limit waits). Never at a mid-drain phase seam — the run holds the task at a
// seam — and never inside a session's lifetime for anything but RESTART_NOW,
// whose whole point is to cut that lifetime short.

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// loadValidatedConfig is the fallback reloader when no closure was wired: the
// same Load+Validate production performs, minus the flag overrides only the
// caller knows.
func loadValidatedConfig(path string) (*config.Config, error) {
	c, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Control-marker file names. STOP/HALT stay string literals at their call sites
// (they predate this file); the restart family lives here so ctl and doctor can
// name the same files the loop reads.
const (
	MarkerRestart    = "RESTART"
	MarkerRestartNow = "RESTART_NOW"
	MarkerReload     = "RELOAD"
)

// restartCheckInterval is how often the daemon looks for a RESTART_NOW marker
// while it has work in flight. The graceful markers need no watcher — they are
// picked up at the next boundary by construction — but --now exists to end an
// in-flight session that may have hours left, so it is polled for. Two seconds
// keeps the operator's wait short next to a session measured in minutes without
// making the state dir hot.
const restartCheckInterval = 2 * time.Second

// SetReloader wires the closure a RELOAD marker uses to rebuild the config from
// disk. It must return a VALIDATED config (Load + ApplyFlagOverrides + Validate
// in production), because applyReload swaps it in without re-validating. Nil
// falls back to Load(cfg.Source()) + Validate — flag-only overrides do not
// survive that path, which is why production always sets one.
func (d *Driver) SetReloader(load func() (*config.Config, error)) {
	d.reloadConfig = load
}

// RestartRequested reports whether the run ended because a restart marker was
// honoured. The caller (cli.Run) turns this into a re-exec; Run itself only
// unwinds, exactly as it does for STOP.
func (d *Driver) RestartRequested() bool {
	return d.restartRequested.Load()
}

// watchRestartNow polls for a RESTART_NOW marker for as long as the run lives.
// On finding one it consumes the marker FIRST (so a crash between here and the
// re-exec cannot leave a surprise-restart behind for the next boot), flags the
// restart, and cancels the run context — which is the SAME cancellation a
// Ctrl-C rides: the adapters kill the whole process group with their WaitDelay
// backstop, salvage runs per its existing paths, and any held claim is released
// by the drain's deferred handback. The daemon then exits cleanly and cli.Run
// re-execs it.
func (d *Driver) watchRestartNow(ctx context.Context, cancel context.CancelFunc) {
	poll := d.restartPoll
	if poll <= 0 {
		poll = restartCheckInterval
	}
	go func() {
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if d.state == nil {
				continue
			}
			if present, msg := d.readMarker(MarkerRestartNow); present {
				_ = d.state.Remove(MarkerRestartNow)
				note := ""
				if msg != "" {
					note = ": " + msg
				}
				log.Printf("RESTART_NOW requested%s — killing the in-flight session, releasing any held claim, then re-executing", note)
				d.restartRequested.Store(true)
				cancel()
				return
			}
		}
	}()
}

// applyReload acts on a consumed RELOAD marker: re-read the config file and
// swap it in. Everything the driver reads through d.cfg picks the new values up
// from the next gate/iteration on (budgets, max-iterations, phases and their
// prompts, poll intervals); what does NOT follow is everything resolved before
// the loop started — the harness adapter built at startup, the target list and
// its pollers, and the process image and environment, all of which need a
// restart (that is what the exec path is for).
//
// A config that fails to reload is NOT fatal. The daemon was running fine on
// the old one; a broken edit mid-flight must not convert a dial tweak into an
// outage, so the error is logged loudly and the current config stays.
func (d *Driver) applyReload() {
	load := d.reloadConfig
	src := ""
	if d.cfg != nil {
		src = d.cfg.Source()
	}
	if load == nil {
		if src == "" {
			log.Print("RELOAD dropped: no config file is in play (defaults only) — continuing on the current config")
			return
		}
		path := src
		load = func() (*config.Config, error) { return loadValidatedConfig(path) }
	}
	fresh, err := load()
	if err != nil {
		log.Printf("RELOAD failed — keeping the current config: %v", err)
		return
	}
	d.cfg = fresh
	log.Printf("config reloaded from %s — dial edits apply from this iteration; harness/env/binary changes need a restart", fresh.Source())
}
