package cli

import (
	"fmt"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// --- 1d. session priority ----------------------------------------------------

// checkSessionPriority reports the scheduling priority the run will start its
// harness sessions at.
//
// The dial's whole purpose is INVISIBLE at runtime: a lowered session priority
// shows itself only as CI finishing sooner on a machine something else shares,
// so nothing on the terminal would say whether the dial is on. Both ordinary
// values PASS — a priority is a setting, not a fault — and the line names what
// the value actually does to the tree, because "10" alone does not tell an
// operator which direction 10 goes.
//
// A positive value also needs nice(1) on the daemon's PATH. Without it every
// spawn fails before the harness starts, so doctor FAILs rather than promising a
// priority the run cannot apply; the platform build tag alone cannot see that.
func checkSessionPriority(cfg *config.Config, e doctorEnv) check {
	c := check{name: "session_nice"}
	switch {
	case cfg.SessionNice < 0:
		// Validation refuses this before doctor runs. Fail closed anyway, and
		// never report a default for a value that was not the default.
		c.status = fail
		c.detail = fmt.Sprintf("%d is negative", cfg.SessionNice)
		c.remedy = "set session_nice to 0, or a positive value up to 19"
		return c
	case cfg.SessionNice == 0:
		c.status = pass
		c.detail = "0 (default) - sessions run at the daemon's own priority"
		return c
	case !harness.SessionPrioritySupported:
		// Validate refuses this before doctor runs, so reaching it means the
		// config was not the one validated. Fail closed anyway: the operator set
		// the dial believing it protects CI.
		c.status = fail
		c.detail = fmt.Sprintf("%d is unsupported on this platform", cfg.SessionNice)
		c.remedy = "set session_nice to 0: there is no nice(1) to lower a session's priority with here"
		return c
	}
	// nice(1) is what applies the value; a unix host without it accepts the config
	// and then fails every session at spawn, which is precisely the silent-broken
	// state doctor exists to catch.
	if _, err := e.lookPath("nice"); err != nil {
		c.status = fail
		c.detail = fmt.Sprintf("%d is set, but nice(1) is not on PATH", cfg.SessionNice)
		c.remedy = "install nice(1) on the daemon's PATH, or set session_nice to 0"
		return c
	}
	c.status = pass
	c.detail = fmt.Sprintf("%d - every harness session, and everything it spawns, starts up to %d below the daemon", cfg.SessionNice, cfg.SessionNice)
	c.info = append(c.info, "so a CI runner sharing this machine wins the CPU when they contend, and sessions still use all of it when CI is idle")
	return c
}
