package release

import (
	"fmt"
	"strings"
	"time"
)

// The release gate. A promotion merge publishes binaries the world installs, so
// the question "did CI pass?" has to be answered about the COMMIT THAT SHIPS -
// the merge commit on `main` - and not about the PR that produced it.
//
// Why that distinction is the whole point: `main`'s protection has
// `strict: false`, so a PR may merge without being up to date with its base. CI
// runs on `refs/pull/<n>/merge`, which is the merge as it stood WHEN THE RUN
// HAPPENED. If the base advances afterwards, the commit that actually lands is a
// combination nothing ever compiled, and the PR's green check is evidence about
// a different tree.
//
// And the state that matters most is the one that is easiest to get wrong: a
// check that is ABSENT. There is no run to read, every field is a zero value,
// and every naive shape ("no failures found") reports a pass. It is not a pass.
// It is the absence of evidence, and this file never lets it become one.

// DefaultRequiredCheck is the check the release gate waits on.
//
// This string is COUPLED TO `.github/workflows/ci.yml`: GitHub names a check run
// after the job that produced it, so this matches only because that workflow's
// job is named `ci`. Rename the job - or add a matrix, which turns the name into
// `ci (ubuntu-latest)` - and the gate stops finding its check. It would not
// error: it would wait out its whole timeout and then refuse the release,
// silently ending publishing.
//
// TestRequiredCheckMatchesTheCIWorkflow pins the two together so that rename
// fails a test instead of a release.
const DefaultRequiredCheck = "ci"

// Verdict is what the gate concluded about a required check.
type Verdict int

const (
	// VerdictPending is "no answer yet" - the check has not appeared, or has
	// appeared and not finished. ABSENT LANDS HERE, never on VerdictPass: a check
	// that has not been created yet is indistinguishable from one that never will
	// be, and only a deadline can tell those apart. Await turns an expired
	// Pending into VerdictFail.
	VerdictPending Verdict = iota
	// VerdictPass is the ONLY value that may publish anything.
	VerdictPass
	// VerdictFail is a red check, a check that ended in any non-success state
	// (including skipped and cancelled), or a check that never arrived before the
	// deadline.
	VerdictFail
)

func (v Verdict) String() string {
	switch v {
	case VerdictPending:
		return "pending"
	case VerdictPass:
		return "pass"
	case VerdictFail:
		return "fail"
	default:
		return fmt.Sprintf("Verdict(%d)", int(v))
	}
}

// CheckRun is the slice of GitHub's check-run object this gate reads.
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // queued | in_progress | completed
	Conclusion string `json:"conclusion"` // success | failure | neutral | cancelled | skipped | timed_out | action_required | ""
}

// Evaluate reports the state of the required check among the runs reported for a
// commit, with the reason it concluded that - the reason is logged, so a red
// release is diagnosable without re-running anything.
//
// Rules, in order:
//   - No run by that name        -> PENDING ("absent"). Never a pass.
//   - Any matching run unfinished -> PENDING.
//   - All matching runs finished  -> PASS only if EVERY conclusion is `success`.
//
// The last clause is conservative on purpose. GitHub's default `filter=latest`
// returns one run per name, but a re-run or a matrixed job can still put several
// under one name, and "one of the two `ci` runs failed" must not be a release.
// `skipped` is a failure here for the same reason absence is: a check that
// declined to run has tested nothing.
func Evaluate(required string, runs []CheckRun) (Verdict, string) {
	var matched []CheckRun
	for _, r := range runs {
		if r.Name == required {
			matched = append(matched, r)
		}
	}

	if len(matched) == 0 {
		names := make([]string, 0, len(runs))
		for _, r := range runs {
			names = append(names, r.Name)
		}
		seen := "none at all"
		if len(names) > 0 {
			seen = strings.Join(names, ", ")
		}
		return VerdictPending, fmt.Sprintf("no check named %q on this commit (checks present: %s)", required, seen)
	}

	for _, r := range matched {
		if r.Status != "completed" {
			return VerdictPending, fmt.Sprintf("check %q is %q, not completed", required, r.Status)
		}
	}

	for _, r := range matched {
		if r.Conclusion != "success" {
			return VerdictFail, fmt.Sprintf("check %q concluded %q, which is not success", required, r.Conclusion)
		}
	}

	return VerdictPass, fmt.Sprintf("check %q passed (%d run(s))", required, len(matched))
}

// Clock is the time this gate runs on, injected so the deadline behaviour is
// unit-testable without a test that actually waits.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
}

// RealClock is the production Clock.
type RealClock struct{}

func (RealClock) Now() time.Time        { return time.Now() }
func (RealClock) Sleep(d time.Duration) { time.Sleep(d) }

// Await polls until the required check reaches a terminal verdict or the timeout
// expires, and is the function the workflow actually calls.
//
// It exists because the release workflow and `ci` are triggered by the SAME push
// to `main`, so at the moment the gate first looks, the check it is gating on
// routinely does not exist yet. Failing immediately on that would make the gate
// useless; treating it as a pass would make it dangerous. So it waits - and
// **the deadline resolves to VerdictFail, not to a pass**. That is the single
// most important line in this file: a check that never arrives blocks the
// release exactly as a red one does.
//
// A fetch error is not fatal on its own (the API is flaky and the deadline is
// the real bound), but the last error is carried into the timeout reason so a
// gate that failed because it could never read anything says so.
func Await(required string, fetch func() ([]CheckRun, error), timeout, interval time.Duration, clock Clock) (Verdict, string) {
	deadline := clock.Now().Add(timeout)
	lastReason := "no poll completed before the deadline"

	for {
		runs, err := fetch()
		if err != nil {
			lastReason = fmt.Sprintf("could not read checks: %v", err)
		} else {
			verdict, reason := Evaluate(required, runs)
			if verdict != VerdictPending {
				return verdict, reason
			}
			lastReason = reason
		}

		// Check the deadline AFTER a poll, so a zero timeout still gets one real
		// look rather than failing a green commit it never examined.
		if !clock.Now().Before(deadline) {
			return VerdictFail, fmt.Sprintf(
				"timed out after %s waiting for check %q: %s - an unresolved check is NOT a pass",
				timeout, required, lastReason)
		}

		clock.Sleep(interval)
	}
}
