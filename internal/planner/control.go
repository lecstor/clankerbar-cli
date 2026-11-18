package planner

// Control markers for a planner instance (CLA-922 review 1.review-1.1).
//
// `clankerbar ctl` writes the same markers a drain reads, and an operator who
// runs it against a planner config expects the same result. The planner has no
// iteration boundary, so the TURN boundary is where they are honoured: at the
// top of every poll tick, which is between turns because a turn blocks the loop.
// The names are the drain's names; RESTART/RESTART_NOW re-exec through
// cli.runPlanner, exactly as the drain re-execs through cli.runDriver.

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

const (
	markerRestart    = "RESTART"
	markerRestartNow = "RESTART_NOW"
	markerReload     = "RELOAD"
)

// checkControl reads and acts on the operator's markers. A true return ends the
// run (stop, halt or restart); RELOAD acts and continues.
func (p *Planner) checkControl() bool {
	if p.state == nil {
		return false
	}
	if present, msg := p.readMarker("HALT"); present {
		log.Printf("planner: HALT present: %s - resolve and delete %s to resume", msg, filepath.Join(p.state.Path(), "HALT"))
		return true
	}
	if present, _ := p.readMarker("STOP"); present {
		_ = p.state.Remove("STOP")
		log.Print("planner: STOP requested - stopping")
		return true
	}
	// Both restart markers are honoured at the turn boundary: there is no
	// session in flight here, so RESTART_NOW's kill-now distinction has nothing
	// to cut short. Consume first, so a crash before the re-exec cannot leave a
	// surprise restart behind.
	if present, msg := p.readMarker(markerRestart); present {
		_ = p.state.Remove(markerRestart)
		logRestart(markerRestart, msg)
		p.restartRequested = true
		return true
	}
	if present, msg := p.readMarker(markerRestartNow); present {
		_ = p.state.Remove(markerRestartNow)
		logRestart(markerRestartNow, msg)
		p.restartRequested = true
		return true
	}
	if present, _ := p.readMarker(markerReload); present {
		_ = p.state.Remove(markerReload)
		log.Print("planner: RELOAD requested - re-reading the config file at the turn boundary")
		p.applyReload()
	}
	return false
}

func logRestart(marker, msg string) {
	if msg != "" {
		log.Printf("planner: %s requested: %s", marker, msg)
	} else {
		log.Printf("planner: %s requested", marker)
	}
	log.Print("planner: re-executing at the turn boundary")
}

// applyReload swaps in a freshly loaded config. Like the drain's reload, what it
// cannot change is everything resolved before the loop started: the target list,
// their pollers and fleet reporters, and the role itself (the planner/worker
// choice happened in cli.Run). Those need a restart, so a document that moves
// them is reported rather than half-applied.
func (p *Planner) applyReload() {
	load := p.reloadConfig
	if load == nil {
		src := ""
		if p.base != nil {
			src = p.base.Source()
		}
		if src == "" {
			log.Print("planner: RELOAD dropped: no config file is in play (defaults only) - continuing on the current config")
			return
		}
		path := src
		load = func() (*config.Config, error) { return loadValidated(path) }
	}
	fresh, err := load()
	if err != nil {
		log.Printf("planner: RELOAD failed - keeping the current config: %v", err)
		return
	}
	if fresh.EffectiveRole() != config.RolePlanner {
		log.Printf("planner: RELOAD would switch the role to %q - a restart is needed to change loops; keeping the current config", fresh.EffectiveRole())
		return
	}
	if !sameTargets(fresh, p.targets) {
		log.Print("planner: RELOAD names a different project set - a restart is needed to change targets; keeping the current config")
		return
	}
	p.base = fresh
	// The per-target resolved configs were cloned from the OLD base; drop them
	// so the next turn re-applies each project's stored document onto the new one.
	p.resolved = make([]*projectRole, len(p.targets))
	log.Printf("planner: config reloaded from %s - machine wiring applies from the next turn; harness/env/binary changes need a restart", fresh.Source())
}

// sameTargets reports whether a freshly loaded config would build the same
// target list runPlanner built at startup: its declared projects, or the single
// unnamed target of single-project mode.
func sameTargets(c *config.Config, targets []Target) bool {
	if len(c.Projects) == 0 {
		return len(targets) == 1 && targets[0].Name == ""
	}
	if len(c.Projects) != len(targets) {
		return false
	}
	for i, p := range c.Projects {
		if targets[i].Name != p.Slug {
			return false
		}
	}
	return true
}

// loadValidated is the fallback reloader when no closure was wired: Load +
// Validate, the same pair a direct config read performs.
func loadValidated(path string) (*config.Config, error) {
	c, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// readMarker reads a marker's first line; a missing file is simply absent.
func (p *Planner) readMarker(name string) (bool, string) {
	if p.state == nil {
		return false, ""
	}
	b, err := p.state.ReadFile(name)
	if err != nil {
		return false, ""
	}
	line := strings.TrimSpace(string(b))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return true, line
}
