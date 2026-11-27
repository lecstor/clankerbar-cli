package loop

// Restart and reload control.

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// loadValidatedConfig is the fallback reloader when no closure was wired: the same Load+Validate
// production performs, minus the flag overrides only the caller knows.
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

// Control-marker file names.
const (
	MarkerRestart    = "RESTART"
	MarkerRestartNow = "RESTART_NOW"
	MarkerReload     = "RELOAD"
)

// restartCheckInterval is how often the daemon looks for a RESTART_NOW marker while it has work in
// flight. The graceful markers need no watcher — they are picked up at the next boundary by
// construction — but --now exists to end an in-flight session that may have hours left, so it is
// polled for.
const restartCheckInterval = 2 * time.Second

// SetReloader wires the closure a RELOAD marker uses to rebuild the config from disk. Nil falls
// back to Load(cfg.Source()) + Validate — flag-only overrides do not survive that path, which is
// why production always sets one.
func (d *Driver) SetReloader(load func() (*config.Config, error)) {
	d.reloadConfig = load
}

// RestartRequested reports whether the run ended because a restart marker was honoured. The caller
// (cli.Run) turns this into a re-exec; Run itself only unwinds, exactly as it does for STOP.
func (d *Driver) RestartRequested() bool {
	return d.restartRequested.Load()
}

// watchRestartNow polls for a RESTART_NOW marker for as long as the run lives. On finding one it
// consumes the marker FIRST (so a crash between here and the re-exec cannot leave a
// surprise-restart behind for the next boot), flags the restart, and cancels the run context —
// which is the SAME cancellation a Ctrl-C rides: the adapters kill the whole process group with
// their WaitDelay backstop, salvage runs per its existing paths, and any held claim is released by
// the drain's deferred handback.
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

// applyReload acts on a consumed RELOAD marker: re-read the config file and swap it in. A config
// that fails to reload is NOT fatal.
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
	// A stopped route is out until the operator resolves the account; the RELOAD that follows that
	// work is the retry, so the sidelines it carried are cleared here (CLA-1024). Marks are kept —
	// they are the provider's own timed statement, which a config edit does not lift.
	if stopped := d.clearStoppedRoutes(); len(stopped) > 0 {
		sort.Strings(stopped)
		log.Printf("RELOAD cleared the Stop sideline on %s — the next session or probe may try them again", strings.Join(stopped, ", "))
	}
	// A stored run-config overlay was built from the OLD base; mark it for a rebuild on the fresh one
	// at the next poll — still this boundary, never mid-session.
	d.invalidateRunConfigs()
	log.Printf("config reloaded from %s — dial edits apply from this iteration; harness/env/binary changes need a restart", fresh.Source())
}
