package harness

import (
	"context"
	"fmt"
	"time"
)

// Model route fallback inside ONE opencode session (CLA-782).
//
// A config may name an ordered route list for a model — the free provider route
// first, the same model's paid route after it. When the free route is denied
// mid-session, switching to the paid route is not a new session: the transcript
// lives on opencode's side and `-s <sid> --model <next>` resumes it, carrying the
// history. Verified on 2.0.32 against the free/paid deepseek pair: a resumed run
// recalled a codeword from the first turn.
//
// The rules:
//   - only a LIMIT switches (DetectLimit): a transient 429 stays on the existing
//     retry ladder, because a blip is worth retrying on the route that already
//     has the context and the budget for it;
//   - a Stop on a route that is NOT the last one is just a switch: a hard budget
//     exhaustion on the free route says nothing about the paid route's account;
//   - a limit on the LAST route is left on the Result unchanged, for the driver's
//     existing limit branch (wait or stop) — this function never decides that;
//   - a Probe never falls back: it is a liveness check, not work.
//
// Every run is charged EXACTLY ONCE: a successful continuation through
// mergeRouteSwitch (mergeResume sums its tokens and cost), and a failed one
// explicitly before the error is returned. A resume re-sends the transcript, so
// it is real money even when the answer is a limit again.

// routeFallbackPrompt is the short continuation sent after a route switch. It
// says what happened and that the transcript is intact, because an agent that
// sees its own context continue without explanation may otherwise re-do work to
// re-orient. Terse on purpose: everything else is already in the transcript.
const routeFallbackPrompt = "The previous model route hit a usage limit. " +
	"Continue exactly where you left off; do not repeat work that is already done."

// switchRoutesOnLimit walks the remaining routes while the session's Result keeps
// classifying as a usage limit, resuming the same session id on each next route.
// It reports every abandoned route on the Result (see RouteFallback) and leaves
// the final Result as the session's real outcome — clean when a later route
// finished the work, still limited when none could.
//
// A runSession error on a next route is returned after its spend is charged: a
// launch failure is the driver's failure to act on, not a limit to hide.
func (o opencode) switchRoutesOnLimit(ctx context.Context, in Invocation, routes []string, res *Result, deadline time.Time) error {
	if len(routes) < 2 {
		return nil
	}
	started := in.ModelArg()
	if started == "" {
		return nil
	}
	current := started
	next := 1
	var fallbacks []RouteFallback
	for next < len(routes) {
		// An UNTRUSTED stream never switches, for the reason it is never
		// resurrected: a line overran the capture, so the limit event the
		// classifier read may not be the session's last word, and CLA-262 forbids
		// acting on parsed claim state (the continuation's seed) from such a
		// capture. The driver's own untrusted path takes the Result as it is.
		if res.Untrusted != "" {
			break
		}
		lim := o.DetectLimit(*res)
		if !lim.Limited {
			break
		}
		sid, _ := resumeTargets(*res)
		if sid == "" {
			// Nothing to resume into (the stream died before its first event), so
			// there is no session whose history a switch could preserve. The limit
			// stands for the driver.
			break
		}
		if !deadline.IsZero() && time.Until(deadline) < opencodeResumeWallClockFloor {
			if in.Console != nil {
				fmt.Fprint(in.Console, opencodeWallClockExhaustedMsg)
			}
			break
		}
		to := routes[next]
		next++
		if in.Console != nil {
			fmt.Fprintf(in.Console, "\n!! %s hit a usage limit (%s) — resuming the session on %s\n", current, lim.Reason, to)
		}
		contIn := in.OnRoute(to)
		// Seed the continuation's parser with the live claim exactly as the
		// resurrection continuation does: `run -s` streams only the new turn, so
		// without the seed a settle or a branch record it makes would go unseen.
		contIn.ResumeClaim = res.Claim
		if !deadline.IsZero() {
			contIn.MaxSessionWallClock = time.Until(deadline)
		}
		contRes, err := o.runSession(ctx, contIn, opencodeResumeArgs(contIn, sid, routeFallbackPrompt))
		fallbacks = append(fallbacks, RouteFallback{Route: current, Next: to, Reason: lim.Reason, ResetAt: lim.ResetAt})
		if err != nil {
			// Charge a continuation that failed to start before reporting the
			// error: whatever was parsed is real spend. A successful one is
			// charged by mergeRouteSwitch (mergeResume sums it), so charging here
			// too would count the same run twice.
			res.Tokens += contRes.Tokens
			res.CostUSD += contRes.CostUSD
			res.Usage.Add(contRes.Usage)
			res.UsageReported = res.UsageReported || contRes.UsageReported
			res.CostReported = res.CostReported || contRes.CostReported
			setRouteReport(res, started, current, fallbacks)
			return err
		}
		mergeRouteSwitch(res, contRes)
		current = to
	}
	// The route that ended the session can itself be denied, and it has no next
	// route: record it too, so the driver's marks cover every route this session
	// exhausted. The Result keeps the limit, which is what the loop's existing
	// limit branch reads.
	if lim := o.DetectLimit(*res); lim.Limited && res.Untrusted == "" {
		fallbacks = append(fallbacks, RouteFallback{Route: current, Reason: lim.Reason, ResetAt: lim.ResetAt})
	}
	setRouteReport(res, started, current, fallbacks)
	return nil
}

// setRouteReport writes the fallback report onto the Result. Nothing is written
// when the session never switched, so a single-route run's Raw is exactly the
// shape it had before this feature existed.
func setRouteReport(res *Result, started, finished string, fallbacks []RouteFallback) {
	if len(fallbacks) == 0 {
		return
	}
	if res.Raw == nil {
		res.Raw = map[string]any{}
	}
	res.Raw[RouteStartedKey] = started
	res.Raw[RouteFinishedKey] = finished
	res.Raw[RouteFallbacksKey] = fallbacks
}

// mergeRouteSwitch folds a continuation run made on a DIFFERENT model route into
// the result of the run it continues. It is mergeResume plus one deliberate
// difference: the abandoned route's streams are REPLACED, not appended.
//
// mergeResume appends Stdout/Stderr because a resurrection's two halves belong to
// one session on one provider and a post-mortem wants both. A route switch does
// not: DetectLimit and IsTransient scan those streams, so keeping the free
// route's "usage limit" text beside the paid route's clean finish would leave a
// successful session classified as limited. The abandoned stream is already in
// the iteration log (the adapter tees it live), so the Result keeps only the run
// that actually ended the session.
func mergeRouteSwitch(base *Result, add Result) {
	mergeResume(base, add)
	base.Stdout = add.Stdout
	base.Stderr = add.Stderr
	base.scans = nil
}
