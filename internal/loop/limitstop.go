package loop

import (
	"errors"
	"log"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// LimitStopName is the marker a daemon leaves in its state dir when it ends on
// a Stop-class usage limit (budget or credits exhausted, no reset to wait for).
// First line is the limit reason. The supervisor reads it to leave the child
// stopped instead of respawning it into the same wall; deleting the file is
// the operator's resume. See docs/limit-stop.md.
const LimitStopName = "LIMIT_STOP"

// LimitStopReason is the reason a Stop limit is recorded under.
func LimitStopReason(lim harness.Limit) string {
	if r := strings.TrimSpace(lim.Reason); r != "" {
		return strings.SplitN(r, "\n", 2)[0]
	}
	return "usage limit"
}

// writeLimitStop records the marker; a failed write is logged and the run still
// ends (the supervisor then falls back to treating the exit as a crash).
func (d *Driver) writeLimitStop(lim harness.Limit) {
	if d.state == nil {
		return
	}
	// ErrExists: a parallel panel member already recorded the stop.
	if err := d.state.WriteFile(LimitStopName, []byte(LimitStopReason(lim)+"\n")); err != nil && !errors.Is(err, statedir.ErrExists) {
		log.Printf("could not write %s marker: %v", LimitStopName, err)
	}
}

// clearStaleLimitStop removes a marker left by a previous run. A supervised
// spawn never reaches here with one (the supervisor holds the instance
// stopped first), so this is the unsupervised start acting as the resume.
func (d *Driver) clearStaleLimitStop() {
	if present, msg := d.readMarker(LimitStopName); present {
		_ = d.state.Remove(LimitStopName)
		log.Printf("cleared %s marker from a previous run (%s)", LimitStopName, msg)
	}
}
