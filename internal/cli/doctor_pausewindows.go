package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// --- 14. pause windows -------------------------------------------------------

// checkPauseWindows reports the operator's recurring "start no new work" schedule and, right now,
// whether it is holding. The verdict is always PASS: a configured pause is a setting, not a fault,
// and an invalid schedule never reaches here (config.Validate refuses it, so doctor has already
// stopped at the config FAIL).
func checkPauseWindows(cfg *config.Config) check {
	return checkPauseWindowsAt(cfg, time.Now())
}

// checkPauseWindowsAt is the check with its clock injected, so a test can pin both sides of a
// window without waiting for one.
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
	// Validate has already refused an unloadable zone, so this backstop only has to keep the
	// formatting sane for a config nothing validated.
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	if active, until := p.ActiveAt(now); active {
		c.info = append(c.info, fmt.Sprintf("now: paused until %s (%s)",
			until.In(loc).Format("Mon 15:04"), tz))
	} else if start, ok := p.NextStart(now); ok {
		c.info = append(c.info, fmt.Sprintf("now: running; next pause %s (%s)",
			start.In(loc).Format("Mon 15:04"), tz))
	} else {
		c.info = append(c.info, "now: running")
	}
	// CLA-906: the schedule holds a phase SEQUENCE at its next boundary, not only the next claim.
	c.info = append(c.info, "a session in flight is never interrupted: a phase sequence holds at its next phase boundary and resumes there when the window ends")
	return c
}
