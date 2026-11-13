package loop

import (
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
)

// quietBackoff is how long a target sits out once its fruitless spend crosses
// the quiet-token ladder: 15m, 30m, 1h, then 2h. It never reaches "never" — the
// blocker is usually an operator answering a question, and a target that can
// never come back would need a restart to notice.
//
// rung counts how many quietTokenThresholds the accumulator has crossed, offset
// by quietThreshold so the FIRST crossing lands on the same 15m the old
// session-count ladder started at.
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

// quietThreshold is how many quietTokenThresholds of fruitless spend it takes
// to back a target off — kept as the ladder's base rung so quietBackoff's shape
// is unchanged from the session-count version.
//
// The old "three, not one or two" rationale survives in token terms: a single
// fruitless drain is ordinary, and so is a second — a genuinely large task can
// span several sessions before anything reaches a reviewer, and backing off
// then would punish exactly the deep work this loop exists to do. ~80M of
// fruitless spend is a different claim, and against the run this was written
// for — which repeated the same no-op ten times at ~26M each — the threshold
// still saves most of them.
const quietThreshold = 3

// quietTokenThreshold is the token-denominated replacement for the old
// session-count trigger (CLA-343). Calibration: the old threshold of 3
// fruitless SESSIONS fired at ~78M tokens (~26M per measured iteration — the
// audit's own figure); 80M lands within one drain of that (the third drain at
// 26M each totals 78M, just under, and the fourth crosses). The drift is
// deliberate and lenient, and it buys the property the session count could
// never give: ONE huge fruitless drain trips the first 15-minute sit-out on its
// own — the 285.9M runaway would have earned it at under a third of its spend
// instead of running three sessions past it.
const quietTokenThreshold = 80_000_000

// judgeProgress reads this poll as the verdict on the target's last drain, and
// backs the target off once its fruitless drains have spent enough tokens
// achieving nothing.
//
// Progress is `Settled()` rising — work reaching a reviewer or finishing. A drain
// that only recorded why it could not proceed bumps `version` but settles
// nothing, which is exactly the run this exists to stop repeating. A fruitless
// drain charges its tokens (Driver.spent) to the accumulator; progress clears
// the accumulator.
//
// An ANSWERED QUESTION is the one kind of progress `Settled()` cannot see, and it
// is the very thing the back-off's own message asks the operator to go and do
// (CLA-248). Answering a BLOCKING question takes its task `blocked -> ready` on
// the plane, and a non-blocking one never moved its task at all; either way
// neither `InReview` nor `Done` shifts, so a falling `OpenQuestions` is the only
// trace of it a poll can read. Both signals are watched, and both baselines move
// on every poll: the verdict is always about the change since the last look.
func (d *Driver) judgeProgress(i int, sum backlog.Summary) {
	settled := sum.Settled()
	openQs, wasOpen := sum.OpenQuestions, d.openQs[i]

	// IDLE IS NOT FRUITLESS. `quiet` counts consecutive DRAINS that settled
	// nothing, and a poll with nothing claimable is not a drain — the gate below
	// will not spend a session, so this target has not failed at anything. Without
	// this the count is immortal: it survives the blocker being parked (`parked`
	// is not in Settled(), so nothing else clears it), an arbitrarily long idle
	// stretch, and a complete turnover of the queue. A task filed a week later
	// then inherits a back-off it did nothing to earn and serves the escalated
	// 2h wait on its FIRST fruitless drain — defeating quietThreshold, which
	// exists precisely so a large task spanning sessions is not punished.
	//
	// ONLY WITH NO VERDICT OUTSTANDING, and that condition is load-bearing rather
	// than defensive. `claimable == 0` does not only mean "the queue is empty" — it
	// also means "everything ready is claimed right now", and the poll immediately
	// after a drain is exactly when that is most likely, because a session that
	// ends still holding a task it pushed work on is deliberately NOT handed back
	// (see releaseHeldClaim). Forgetting the count there would not merely decline
	// to charge the drain, it would CANCEL the verdict on one that already ran — so
	// a target whose every session ends holding its task would alternate spawn /
	// forget forever and never back off at all. Requiring the verdict to be settled
	// first costs one extra poll of an idle stretch and nothing else: the drain is
	// judged, and the NEXT poll — still idle — does the forgetting.
	//
	// Clearing skipUntil too, and not only the count: an in-flight wait is moot
	// while there is nothing to spawn for, and leaving it set would make the next
	// task filed sit out the tail of a wait somebody else's blocker earned.
	//
	// RESIDUAL, stated rather than implied. A target whose only claimable task is
	// held ELSEWHERE for more than one poll — another driver on the same project,
	// an agent working it by hand — reads as idle for that stretch, so it forgets
	// and lifts. Such a target backs off at the base 15m every quietThreshold
	// fruitless drains instead of escalating toward 2h: the breaker still bounds
	// the spend, it just stops tightening. That is accepted rather than narrowed
	// away, because the two obvious narrowings are both worse. Requiring
	// `in_progress == 0` reinstates the immortal count for any project holding
	// abandoned WIP (CLA-274), which is precisely the population most likely to
	// have some. Requiring N consecutive idle polls does not discriminate at all:
	// a task held elsewhere is held for a lease, which is many poll intervals.
	// Nothing in a summary count separates "quiet because there is no work" from
	// "quiet because somebody else has it" — only whether WE are still spending
	// sessions does, and the guard above is the part of that signal worth having.
	//
	// This cannot spin. A drain needs a poll where the target IS spawnable, so the
	// reset never applies to the poll that authorises a session; the count climbs
	// from zero the ordinary way, and reaching the threshold again takes
	// quietThreshold fresh fruitless drains.
	if !sum.Spawnable() && !d.pending[i] {
		if d.quietTokens[i] > 0 {
			log.Printf("%snothing to spawn for — idle, not fruitless; forgetting %d tokens of drain(s) that settled nothing and clearing any no-progress back-off", d.prefix(i), d.quietTokens[i])
		}
		d.quietTokens[i] = 0
		// An idle stretch clears the no-progress (quiet) back-off - with no work
		// there is nothing to judge, and the blocker may have resolved itself.
		// A CLA-507 harness sit-out is different: the harness itself is broken,
		// and an empty queue does not fix it. Clearing skipUntil here would cut
		// the laddered sit-out short the moment the queue momentarily empties
		// (a sibling daemon consuming the work), so it is preserved until it
		// elapses or the harness succeeds again.
		if d.harnessFails[i] == 0 {
			d.skipUntil[i] = time.Time{}
		}
		d.baseline[i], d.openQs[i] = settled, openQs
		return
	}

	if !d.pending[i] {
		// No drain outstanding. Progress can still arrive from elsewhere — the
		// operator merging a PR, another machine's loop — and it means whatever was
		// stuck may now be unstuck, so let the target back in immediately.
		//
		// Note what the merge case actually rides on, because it is not obvious and
		// nothing here can pin it: the plane approves a task back to `ready` BEFORE
		// its PR is merged and it is marked `done`. The first write DROPS `Settled()`
		// by one and lowers the baseline with it here, so the later `done` reads as a
		// rise. It is that two-step, not the merge, that this branch sees, and only
		// because a poll lands between the two writes - which an idle poll of a minute
		// against a human-paced approve-then-merge ordinarily does. `in_review -> done`
		// with no poll in between is invisible, exactly like `blocked -> ready`.
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

	// The strike is earned whatever else moved: a session that answered its own
	// question has not thereby delivered anything, and the drain is being judged on
	// what it SETTLED. But an answer that landed while the drain was running is the
	// operator doing the very thing the back-off message asked for, and this poll is
	// the only one that can ever see it: the baseline advances just below, so a fall
	// consumed here is gone rather than deferred. Take the strike, skip the sit-out.
	// The target gets one immediate retry against the answer; if that settles
	// nothing too, the retry's spend is charged like any other, so the rung it
	// reaches depends on how big it was (a whole-threshold retry escalates at once,
	// a smaller one repeats the band) — the retry is never an exemption.
	answered := openQs < wasOpen
	d.quietTokens[i] += spent
	d.baseline[i], d.openQs[i] = settled, openQs
	// Bands, matching the old session-count ladder at the ~26M-per-drain
	// calibration: 3 fruitless drains (~78M) earned 15m, 4 (~104M) earned 30m,
	// 5 (~130M) earned 1h, 6+ (~156M) earned the 2h cap. In whole thresholds:
	// acc in [80M,160M) → rung 3 → 15m; [160M,240M) → 30m; [240M,320M) → 1h;
	// 320M+ → 2h. One huge fruitless drain (≥80M) trips the FIRST band on its
	// own — the property the session count could never give (CLA-343).
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
	// Name the RIGHT cause. The original sentence asserts there is claimable work
	// and lists the reasons a ready task resists being finished — all of which are
	// wrong, and misdirecting, when the only thing that opened the gate was an
	// abandoned branch (CLA-274). Sending an operator to look for an unanswered
	// question when the real subject is stranded WIP costs them the diagnosis.
	if sum.Claimable == 0 && sum.StaleClaimable > 0 {
		log.Printf("%s%d tokens spent on sessions that settled nothing — backing off for %s. Nothing is READY; the sessions were spawned to recover %d abandoned branch(es), and are not finishing them. Check the last iteration log, and the branch itself.",
			d.prefix(i), d.quietTokens[i], wait, sum.StaleClaimable)
		return
	}
	log.Printf("%s%d tokens spent on sessions that settled nothing — backing off for %s. The queue says there is claimable work, so something is stopping it being DONE: an unanswered question, a gate, a toolchain the session cannot run. Check the last iteration log.",
		d.prefix(i), d.quietTokens[i], wait)
}

// backedOff reports whether a target is currently sitting out, logging the moment
// it becomes eligible again so the silence has a visible end.
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

// sidelineTarget marks target i failed after drainPhases returned an error: the
// failure happened while running THIS project's session, so this project sits out
// while the others drain on. The sit-out uses the quiet ladder's shape: 15m,
// 30m, 1h, then a 2h cap, keyed on consecutive failures rather than fruitless
// tokens, and it self-heals: the operator fixes the config (or the checkout), the
// next attempt after the sit-out succeeds, and Run resets the count.
//
// The log line is the operator's whole diagnosis for the exact incident that
// motivated this (2026-08-26): which project died, in which workdir, the
// harness's own error text - and the reassurance that the fleet carried on.
func (d *Driver) sidelineTarget(i int, drainNum int, err error) {
	d.harnessFails[i]++
	d.harnessErrs[i] = err
	wait := failureBackoff(d.harnessFails[i])
	d.skipUntil[i] = time.Now().Add(wait)
	log.Printf("%siteration %d: sidelining this project for %s (harness failure %d in a row; workdir %s) - the OTHER projects keep draining. Cause: %v",
		d.prefix(i), drainNum, wait, d.harnessFails[i], d.workdirOf(d.targets[i]), err)
}

// failureBackoff maps a run of consecutive harness failures onto the same ladder
// quietBackoff serves. judgeProgress's first trip lands at rung quietThreshold
// (15m); failure N starts one rung earlier than N+1 so each repeat escalates one
// band: 15m, 30m, 1h, then quietBackoff's own 2h cap.
func failureBackoff(fails int) time.Duration {
	return quietBackoff(quietThreshold - 1 + fails)
}

// allSidelined reports whether every target is CURRENTLY sitting out a harness
// failure: nothing left to drive, so ending the run beats idle-polling forever.
// A target counts only while its failure back-off is live - skipUntil zeroed by
// an elapsed wait means it is drivable again, whatever its history, and a target
// that has never failed since its last success never counts.
//
// A target paused for an operator answer (fleet or startup trip, CLA-396/562)
// never counts as sidelined: the pause is meant to wait for the answer, not to
// exit. Without this a fleet of one that trips its startup bound would exit on
// the same iteration it paused, losing the wait it just asked for.
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

// sidelinedReport renders one line per target with the error that benched it, for
// the run-wide exit when every target is sidelined.
func (d *Driver) sidelinedReport() string {
	var b strings.Builder
	for i := range d.targets {
		fmt.Fprintf(&b, "\n  [%s] (workdir %s): %v", d.targets[i].Name, d.workdirOf(d.targets[i]), d.harnessErrs[i])
	}
	return b.String()
}

// workdirOf resolves where a target's sessions run, for log lines: the target's
// own workdir when it declares one, the config's otherwise - the same resolution
// the invocation gets, so the log names the directory the session actually died in.
func (d *Driver) workdirOf(t Target) string {
	if t.WorkDir != "" {
		return t.WorkDir
	}
	return d.cfg.WorkDir
}

// failureDetailMax bounds how much of the harness's diagnostic text reaches the
// stop message. A session's stderr can run to megabytes; the operator needs
// enough to recognise the failure and quote it in a bug report, not the log.
const failureDetailMax = 400

// failureDetail renders a harness diagnostic for a one-line stop message: the
// TAIL of the text, non-printables stripped, whitespace collapsed, bounded.
//
// The tail rather than the head, deliberately. The scope starts with stderr,
// which on a real run leads with startup noise that is identical on every
// session; the thing that killed this one is the last thing said. Truncating
// from the front would reliably show the operator the one part that is never
// the answer.
//
// Non-printables are stripped because this changes what the text is FOR. Up to
// now a diagnostic was only ever fed to regexp.MatchString, where a control byte
// is inert; here it is rendered to a terminal, where ESC is executed. The
// scoping rules inherited from CLA-258 answer "what may a classifier READ", which
// is a different question from "what may be PRINTED", and stderr reaches this
// verbatim from the harness. Stripping is cheap and the answer does not depend
// on trusting the upstream CLI's output.
//
// Empty in, empty out — and the caller appends it, so a harness with nothing to
// say leaves the existing message exactly as it was rather than trailing a
// colon and a blank.
func failureDetail(diag string) string {
	s := oneLine(diag)
	if s == "" {
		return ""
	}
	// Counted and sliced in RUNES, not bytes: the harness's own text carries "·"
	// in every usage-limit notice and "→" in rendered tool markers, so a byte
	// slice would routinely cut a rune in half and emit U+FFFD at the seam.
	//
	// "..." rather than U+2026: this string exists to be pasted into a bug
	// report, and a single-character ellipsis is exactly what arrives as
	// mojibake when terminal output is copied out.
	if r := []rune(s); len(r) > failureDetailMax {
		s = "..." + string(r[len(r)-failureDetailMax:])
	}
	return ": " + s
}

// oneLine renders arbitrary harness text as a single printable line: control
// characters are stripped because this text reaches a terminal, where ESC is
// executed, and whitespace is collapsed so a multi-line diagnostic cannot break
// a one-line marker or log entry. The scoping rules that answer "what may a
// classifier READ" are a different question; this is about what may be PRINTED.
func oneLine(s string) string {
	printable := strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			return r
		}
		return -1
	}, s)
	return strings.Join(strings.Fields(printable), " ")
}

// droppedNote says how much of a session's output the retained window did not
// keep, when it kept less than all of it.
//
// It rides on the message that ENDS a run, because that is the message whose
// weight depends on it: "exited 1 (non-retryable): <text>" reads as "this was the
// failure", when what the classifier actually saw was the last couple of MiB. An
// operator deciding whether the classifier merely does not know this failure yet
// needs to know which of those they are looking at. Empty when nothing was
// dropped, which is the ordinary case.
func droppedNote(dropped int64) string {
	if dropped <= 0 {
		return ""
	}
	return fmt.Sprintf(" (note: %.1f MiB of this session's output was past the retained window, so the classification read only the tail)",
		float64(dropped)/(1<<20))
}
