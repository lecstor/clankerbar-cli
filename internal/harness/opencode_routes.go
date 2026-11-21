package harness

import (
	"context"
	"fmt"
	"time"
)

// Model route fallback inside ONE opencode session.

// routeFallbackPrompt is the short continuation sent after a route switch. Terse on purpose:
// everything else is already in the transcript.
const routeFallbackPrompt = "The previous model route hit a usage limit. " +
	"Continue exactly where you left off; do not repeat work that is already done."

// switchRoutesOnLimit walks the remaining routes while the session's Result keeps classifying as a
// usage limit, resuming the same session id on each next route.
func (o opencode) switchRoutesOnLimit(ctx context.Context, in Invocation, routes []string, res *Result, deadline time.Time, delivery *opencodeInbox) error {
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
		// An UNTRUSTED stream never switches, for the reason it is never resurrected: a line overran the
		// capture, so the limit event the classifier read may not be the session's last word, and CLA-262
		// forbids acting on parsed claim state (the continuation's seed) from such a capture.
		if res.Untrusted != "" {
			break
		}
		lim := o.DetectLimit(*res)
		if !lim.Limited {
			break
		}
		sid, _ := resumeTargets(*res)
		if sid == "" {
			// Nothing to resume into (the stream died before its first event), so there is no session whose
			// history a switch could preserve.
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
		// Seed the continuation's parser with the live claim exactly as the resurrection continuation
		// does: `run -s` streams only the new turn, so without the seed a settle or a branch record it
		// makes would go unseen.
		contIn.ResumeClaim = res.Claim
		if !deadline.IsZero() {
			contIn.MaxSessionWallClock = time.Until(deadline)
		}
		contRes, err := o.runSession(ctx, contIn, opencodeResumeArgs(contIn, sid, routeFallbackPrompt), delivery)
		fallbacks = append(fallbacks, RouteFallback{Route: current, Next: to, Reason: lim.Reason, ResetAt: lim.ResetAt})
		if err != nil {
			// Charge a continuation that failed to start before reporting the error: whatever was parsed is
			// real spend.
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
	// The route that ended the session can itself be denied, and it has no next route: record it too,
	// so the driver's marks cover every route this session exhausted.
	if lim := o.DetectLimit(*res); lim.Limited && res.Untrusted == "" {
		fallbacks = append(fallbacks, RouteFallback{Route: current, Reason: lim.Reason, ResetAt: lim.ResetAt})
	}
	setRouteReport(res, started, current, fallbacks)
	return nil
}

// setRouteReport writes the fallback report onto the Result. Nothing is written when the session
// never switched, so a single-route run's Raw is exactly the shape it had before this feature
// existed.
func setRouteReport(res *Result, started, finished string, fallbacks []RouteFallback) {
	if len(fallbacks) == 0 {
		return
	}
	if res.Raw == nil {
		res.Raw = map[string]any{}
	}
	if existing, _ := res.Raw[RouteStartedKey].(string); existing == "" {
		res.Raw[RouteStartedKey] = started
	}
	res.Raw[RouteFinishedKey] = finished
	prior, _ := res.Raw[RouteFallbacksKey].([]RouteFallback)
	res.Raw[RouteFallbacksKey] = append(append([]RouteFallback(nil), prior...), fallbacks...)
}

// mergeRouteSwitch folds a continuation run made on a DIFFERENT model route into the result of the
// run it continues. mergeResume appends Stdout/Stderr because a resurrection's two halves belong to
// one session on one provider and a post-mortem wants both.
func mergeRouteSwitch(base *Result, add Result) {
	mergeResume(base, add)
	base.Stdout = add.Stdout
	base.Stderr = add.Stderr
	base.scans = nil
}
