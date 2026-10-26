package cli

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// --- 11. power ----------------------------------------------------------------

// unknownSleepRemedy is shared by both ways doctor can fail to learn the sleep
// policy — `pmset -g` not running at all, and running but reporting no
// idle-sleep field. The two states are the same state, so they say the same
// thing; keeping one string means a future edit cannot improve the advice for
// one of them and leave the other behind. It has to be actionable for a reader
// who has just been told the command doctor would send them to did not answer,
// so it names the mitigation as well as the manual check.
const unknownSleepRemedy = "check manually with `pmset -g` — if idle sleep is enabled, an unattended run will freeze mid-wait. Either way, `clankerbar run` holds a no-idle-sleep assertion for the length of the run; for any other invocation use `caffeinate -i …`"

// checkPower answers the most basic precondition of an unattended run, and the
// one nothing else checks: will this machine still be awake to do the work?
//
// Timers run on the monotonic clock, which does not advance while a machine is
// suspended. So idle sleep does not pause a run, it FREEZES it — mid-wait,
// silently, in a way that looks exactly like a hang. A real run lost 5h31m of a
// 10h window to this, waking only for 45-second Power Nap bursts.
//
// Two facts decide it: whether anything holds a no-idle-sleep assertion, and what
// the active power source's sleep timeout is. `clankerbar run` now takes an
// assertion itself, so a green line here usually means that is working — but
// doctor is also run standalone, before any loop exists, which is exactly when
// the answer is worth having.
func checkPower(ctx context.Context, e doctorEnv) check {
	c := check{name: "power"}
	if e.goos != "darwin" {
		c.status = pass
		c.detail = "no sleep-policy checks for " + e.goos
		return c
	}

	if out, err := e.pmset(ctx, "-g", "assertions"); err == nil && holdsNoIdleSleep(out) {
		c.status = pass
		// Point-in-time, and said as such: assertions are commonly short-lived (the
		// `caffeinate` default is 300 seconds), so one held while doctor runs is not
		// proof one will be held all night. `clankerbar run` takes its own, tied to
		// its pid, which is the only kind that covers a whole run.
		c.detail = "a no-idle-sleep assertion is held right now — note assertions can be short-lived; `run` takes its own for the length of the run"
		return c
	}

	out, err := e.pmset(ctx, "-g")
	if err != nil {
		// Not being able to ask is not evidence of a problem, and a FAIL here would
		// block a cron gate over a missing binary.
		c.status = warn
		c.detail = "could not read the sleep settings: " + err.Error()
		c.remedy = unknownSleepRemedy
		return c
	}
	mins, found := idleSleepMinutes(out)
	switch {
	case !found:
		// Informationally identical to the read failing outright: the command
		// exited zero, but no idle-sleep field came back, so doctor does not know
		// the answer. A renamed, re-cased or locale-shifted field, or a VM whose
		// `pmset` omits it, used to render a green line on the exact question this
		// check exists to answer. Failing open on the unknown case is worse than
		// not checking, because the operator gets a green line telling them not to
		// look. Note what the WARN does and does not buy: only `fail` stops
		// `doctor && run`, so the documented cron gate opens either way — what
		// changes is that a human reading the output has a line worth reading. Do
		// not "restore consistency" by making this a FAIL; that would block the
		// gate over a machine that may well be fine, which is why the read-failure
		// branch above is a WARN too.
		c.status = warn
		c.detail = "no idle-sleep timeout reported for the active power source, so the sleep policy is unknown"
		c.remedy = unknownSleepRemedy
	case mins == 0:
		c.status = pass
		c.detail = "idle sleep is disabled for the active power source (a closed lid still sleeps)"
	default:
		c.status = warn
		c.detail = fmt.Sprintf("the machine idle-sleeps after %d min on the active power source, and nothing holds an assertion", mins)
		c.remedy = "`clankerbar run` holds one itself; for any other invocation use `caffeinate -i …`. Start the run on AC — plugging in later does NOT wake a sleeping Mac, and a closed lid sleeps regardless of either"
	}
	return c
}

// holdsNoIdleSleep reports whether `pmset -g assertions` shows a live
// PreventUserIdleSystemSleep. Only two line shapes count as evidence, because
// they are the only two pmset actually emits for this assertion (verified against
// live output on 2026-08-22):
//
//   - the summary row: exactly the assertion name followed by an integer count
//     and nothing else — held only when that count is non-zero;
//   - a per-process detail line under "Listed by owning process:", which begins
//     `pid <n>(<proc>):` and names the holder — held whenever one appears.
//
// Anything else mentioning the name is NOT PROVEN HELD and falls through, so
// checkPower goes on to the `pmset -g` settings read rather than reporting PASS
// from a shape it does not recognise. Falling through cannot invent a problem —
// it only defers to the more direct question — whereas the previous test ("the
// last token is not an integer means a detail line") answered YES, held for every
// unrecognised shape, including Apple appending a token to the summary row or a
// locale shifting its number format (CLA-306).
//
// Both branches match structure, not whitespace-split tokens, because splitting
// is where the original guess went wrong: the detail-line regex runs on the raw
// line since pmset prints the holding process's name verbatim inside the parens,
// and a name containing spaces ("Google Chrome Helper") leaves no single field
// carrying the `<n>(<proc>):` tail; the summary row demands exactly two fields
// because any token beyond name-plus-count is an unknown in BOTH directions —
// "1 (inactive)" no less than "0" (CLA-306 review).
var pidDetailLine = regexp.MustCompile(`^\s*pid\s+\d+\(.+\):(?:\s|$)`)

func holdsNoIdleSleep(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "PreventUserIdleSystemSleep") {
			continue
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) == 2 && fields[0] == "PreventUserIdleSystemSleep":
			// Summary-row shape. Parse the COUNT position, not the tail: trailing
			// tokens are exactly the unknown we must not guess from, so a row with
			// anything after the count — or a count that will not parse — is an
			// unrecognised variant and falls through rather than answer either way.
			if n, err := strconv.Atoi(fields[1]); err == nil && n > 0 {
				return true
			}
		case pidDetailLine.MatchString(line):
			// Per-process detail line: `pid 81237(caffeinate): … named: "…"`. Its
			// presence means a live process holds the assertion. Matched on the raw
			// line so a process name containing spaces or even nested parens
			// ("Google Chrome Helper (Renderer)") still reads as one
			// `<pid>(<name>):` head; greedy to the LAST `):` is safe because the
			// anchored pid head and the assertion-name pre-filter above already
			// exclude every other line shape.
			return true
		}
	}
	return false
}

// idleSleepMinutes pulls the `sleep` timeout out of `pmset -g`, which reports the
// settings currently in force for whichever power source is active — the right
// question, since battery and AC usually differ sharply.
func idleSleepMinutes(out string) (int, bool) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "sleep" {
			continue
		}
		if n, err := strconv.Atoi(fields[1]); err == nil {
			return n, true
		}
	}
	return 0, false
}
