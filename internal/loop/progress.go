package loop

import (
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
)

// quietBackoff is how long a target sits out once its fruitless spend crosses the quiet-token
// ladder: 15m, 30m, 1h, then 2h.
func quietBackoff(rung int) time.Duration {
	const (
		base = 15 * time.Minute
		cap_ = 2 * time.Hour
	)
	d := base
	for i := quietThreshold; i < rung; i++ {
		if d *= 2; d >= cap_ {
			return cap_
		}
	}
	return d
}

// quietThreshold is how many quietTokenThresholds of fruitless spend it takes to back a target off
// — kept as the ladder's base rung so quietBackoff's shape is unchanged from the session-count
// version.
const quietThreshold = 3

// quietTokenThreshold is the token-denominated replacement for the old session-count trigger.
const quietTokenThreshold = 80_000_000

// judgeProgress reads this poll as the verdict on the target's last drain, and backs the target off
// once its fruitless drains have spent enough tokens achieving nothing. A drain that only recorded
// why it could not proceed bumps `version` but settles nothing, which is exactly the run this
// exists to stop repeating.
func (d *Driver) judgeProgress(i int, sum backlog.Summary) {
	settled := sum.Settled()
	openQs, wasOpen := sum.OpenQuestions, d.openQs[i]

	// IDLE IS NOT FRUITLESS. `quiet` counts consecutive DRAINS that settled nothing, and a poll with
	// nothing claimable is not a drain — the gate below will not spend a session, so this target has
	// not failed at anything.
	if !sum.Spawnable() && !d.pending[i] {
		if d.quietTokens[i] > 0 {
			log.Printf("%snothing to spawn for — idle, not fruitless; forgetting %d tokens of drain(s) that settled nothing and clearing any no-progress back-off", d.prefix(i), d.quietTokens[i])
		}
		d.quietTokens[i] = 0
		// An idle stretch clears the no-progress (quiet) back-off - with no work there is nothing to
		// judge, and the blocker may have resolved itself.
		if d.harnessFails[i] == 0 {
			d.skipUntil[i] = time.Time{}
		}
		d.baseline[i], d.openQs[i] = settled, openQs
		return
	}

	if !d.pending[i] {
		// No drain outstanding. Progress can still arrive from elsewhere — the operator merging a PR,
		// another machine's loop — and it means whatever was stuck may now be unstuck, so let the
		// target back in immediately.
		if d.quietTokens[i] > 0 {
			switch {
			case settled > d.baseline[i]:
				log.Printf("%sprogress from elsewhere — clearing the no-progress back-off", d.prefix(i))
				d.quietTokens[i], d.skipUntil[i] = 0, time.Time{}
			case openQs < wasOpen:
				log.Printf("%sopen questions fell %d -> %d, so something was resolved: clearing the no-progress back-off",
					d.prefix(i), wasOpen, openQs)
				d.quietTokens[i], d.skipUntil[i] = 0, time.Time{}
			}
		}
		d.baseline[i], d.openQs[i] = settled, openQs
		return
	}

	d.pending[i] = false
	spent := d.spent[i]
	d.spent[i] = 0
	if settled > d.baseline[i] {
		d.quietTokens[i] = 0
		d.baseline[i], d.openQs[i] = settled, openQs
		return
	}

	// The strike is earned whatever else moved: a session that answered its own question has not
	// thereby delivered anything, and the drain is being judged on what it SETTLED.
	answered := openQs < wasOpen
	d.quietTokens[i] += spent
	d.baseline[i], d.openQs[i] = settled, openQs
	// Bands, matching the old session-count ladder at the ~26M-per-drain calibration: 3 fruitless
	// drains (~78M) earned 15m, 4 (~104M) earned 30m, 5 (~130M) earned 1h, 6+ (~156M) earned the 2h
	// cap.
	if d.quietTokens[i] < quietTokenThreshold {
		return
	}
	if answered {
		log.Printf("%s%d tokens spent on sessions that settled nothing, but open questions fell %d -> %d while the last one ran - retrying once before backing off",
			d.prefix(i), d.quietTokens[i], wasOpen, openQs)
		return
	}
	wait := quietBackoff(2 + d.quietTokens[i]/quietTokenThreshold)
	d.skipUntil[i] = time.Now().Add(wait)
	// Name the RIGHT cause.
	if sum.Claimable == 0 && sum.StaleClaimable > 0 {
		log.Printf("%s%d tokens spent on sessions that settled nothing — backing off for %s. Nothing is READY; the sessions were spawned to recover %d abandoned branch(es), and are not finishing them. Check the last iteration log, and the branch itself.",
			d.prefix(i), d.quietTokens[i], wait, sum.StaleClaimable)
		return
	}
	log.Printf("%s%d tokens spent on sessions that settled nothing — backing off for %s. The queue says there is claimable work, so something is stopping it being DONE: an unanswered question, a gate, a toolchain the session cannot run. Check the last iteration log.",
		d.prefix(i), d.quietTokens[i], wait)
}

// backedOff reports whether a target is currently sitting out, logging the moment it becomes
// eligible again so the silence has a visible end.
func (d *Driver) backedOff(i int) bool {
	if d.skipUntil[i].IsZero() {
		return false
	}
	if time.Now().Before(d.skipUntil[i]) {
		return true
	}
	d.skipUntil[i] = time.Time{}
	log.Printf("%sback-off elapsed — trying again", d.prefix(i))
	return false
}

// sidelineTarget marks target i failed after drainPhases returned an error: the failure happened
// while running THIS project's session, so this project sits out while the others drain on.
func (d *Driver) sidelineTarget(i int, drainNum int, err error) {
	d.harnessFails[i]++
	d.harnessErrs[i] = err
	wait := failureBackoff(d.harnessFails[i])
	d.skipUntil[i] = time.Now().Add(wait)
	log.Printf("%siteration %d: sidelining this project for %s (harness failure %d in a row; workdir %s) - the OTHER projects keep draining. Cause: %v",
		d.prefix(i), drainNum, wait, d.harnessFails[i], d.workdirOf(d.targets[i]), err)
}

// failureBackoff maps a run of consecutive harness failures onto the same ladder quietBackoff
// serves. judgeProgress's first trip lands at rung quietThreshold (15m); failure N starts one rung
// earlier than N+1 so each repeat escalates one band: 15m, 30m, 1h, then quietBackoff's own 2h cap.
func failureBackoff(fails int) time.Duration {
	return quietBackoff(quietThreshold - 1 + fails)
}

// allSidelined reports whether every target is CURRENTLY sitting out a harness failure: nothing
// left to drive, so ending the run beats idle-polling forever. A target counts only while its
// failure back-off is live - skipUntil zeroed by an elapsed wait means it is drivable again,
// whatever its history, and a target that has never failed since its last success never counts.
func (d *Driver) allSidelined() bool {
	if len(d.targets) == 0 {
		return false
	}
	now := time.Now()
	for i := range d.targets {
		if d.fleetPaused[i] || d.startupPaused[i] {
			return false
		}
		if d.harnessFails[i] == 0 || d.skipUntil[i].IsZero() || !now.Before(d.skipUntil[i]) {
			return false
		}
	}
	return true
}

// sidelinedReport renders one line per target with the error that benched it, for the run-wide exit
// when every target is sidelined.
func (d *Driver) sidelinedReport() string {
	var b strings.Builder
	for i := range d.targets {
		fmt.Fprintf(&b, "\n  [%s] (workdir %s): %v", d.targets[i].Name, d.workdirOf(d.targets[i]), d.harnessErrs[i])
	}
	return b.String()
}

// workdirOf resolves where a target's sessions run, for log lines: the target's own workdir when it
// declares one, the config's otherwise - the same resolution the invocation gets, so the log names
// the directory the session actually died in.
func (d *Driver) workdirOf(t Target) string {
	if t.WorkDir != "" {
		return t.WorkDir
	}
	return d.cfg.WorkDir
}

// failureDetailMax bounds how much of the harness's diagnostic text reaches the stop message. A
// session's stderr can run to megabytes; the operator needs enough to recognise the failure and
// quote it in a bug report, not the log.
const failureDetailMax = 400

// failureDetail renders a harness diagnostic for a one-line stop message: the TAIL of the text,
// non-printables stripped, whitespace collapsed, bounded. The tail rather than the head,
// deliberately.
func failureDetail(diag string) string {
	s := oneLine(diag)
	if s == "" {
		return ""
	}
	// Counted and sliced in RUNES, not bytes: the harness's own text carries "·" in every usage-limit
	// notice and "→" in rendered tool markers, so a byte slice would routinely cut a rune in half
	// and emit U+FFFD at the seam.
	if r := []rune(s); len(r) > failureDetailMax {
		s = "..." + string(r[len(r)-failureDetailMax:])
	}
	return ": " + s
}

// oneLine renders arbitrary harness text as a single printable line: control characters are
// stripped because this text reaches a terminal, where ESC is executed, and whitespace is collapsed
// so a multi-line diagnostic cannot break a one-line marker or log entry.
func oneLine(s string) string {
	printable := strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			return r
		}
		return -1
	}, s)
	return strings.Join(strings.Fields(printable), " ")
}

// droppedNote says how much of a session's output the retained window did not keep, when it kept
// less than all of it.
func droppedNote(dropped int64) string {
	if dropped <= 0 {
		return ""
	}
	return fmt.Sprintf(" (note: %.1f MiB of this session's output was past the retained window, so the classification read only the tail)",
		float64(dropped)/(1<<20))
}
