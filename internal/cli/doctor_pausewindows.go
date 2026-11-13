package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// --- 14. pause windows -------------------------------------------------------

// checkPauseWindows reports the operator's recurring "start no new work"
// schedule and, right now, whether it is holding.
//
// The verdict is always PASS: a configured pause is a setting, not a fault, and
// an invalid schedule never reaches here (config.Validate refuses it, so doctor
// has already stopped at the config FAIL). What this adds is the one thing a
// schedule cannot tell you by reading it — whether the loop would claim work in
// the next minute, and when it will next stop.
func checkPauseWindows(cfg *config.Config) check {
	return checkPauseWindowsAt(cfg, time.Now())
}

// checkPauseWindowsAt is the check with its clock injected, so a test can pin
// both sides of a window without waiting for one.
func checkPauseWindowsAt(cfg *config.Config, now time.Time) check {
	c := check{name: "pause_windows"}
	p := cfg.PauseWindows
	if p == nil || len(p.Windows) == 0 {
		c.status = pass
		c.detail = "none configured"
		return c
	}

	c.status = pass
	c.detail = p.Describe()

	tz := strings.TrimSpace(p.Timezone)
	// Validate has already refused an unloadable zone, so this backstop only has
	// to keep the formatting sane for a config nothing validated.
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	if active, until := p.ActiveAt(now); active {
		c.info = append(c.info, fmt.Sprintf("now: paused until %s (%s)",
			until.In(loc).Format("Mon 15:04"), tz))
		return c
	}
	if start, ok := p.NextStart(now); ok {
		c.info = append(c.info, fmt.Sprintf("now: running; next pause %s (%s)",
			start.In(loc).Format("Mon 15:04"), tz))
		return c
	}
	c.info = append(c.info, "now: running")
	return c
}
