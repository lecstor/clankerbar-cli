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

// routeProviderErrorPrompt is the counterpart for a route that died on a provider failure rather
// than a usage limit: same terse shape, so the session continues from its intact transcript.
const routeProviderErrorPrompt = "The previous model route failed with a provider error. " +
	"Continue exactly where you left off; do not repeat work that is already done."

// The reasons a route was abandoned for a provider-side death, distinguished on the report so the
// driver can mark the route for its shorter error cooldown instead of the usage-limit interval.
const (
	routeFailReasonProvider = "provider_error"
	routeFailReasonQuiet    = "quiet_death"
	routeFailReasonNoTool   = "no_tool_exit"
)

// switchRoutesOnLimit walks the remaining routes while the session's Result keeps classifying as a
// usage limit, resuming the same session id on each next route. A list of one has no walk, but still
// reports its route when the session ended limited, so the driver marks it; a plain-string model is
// such a list of one. An invocation with in.Routes nil (a phase with no model at all, or a turn on
// its own model) reports nothing.
func (o opencode) switchRoutesOnLimit(ctx context.Context, in Invocation, routes []string, res *Result, deadline time.Time, delivery *opencodeInbox) error {
	if len(in.Routes) == 0 || len(routes) == 0 {
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
			chargeAttempt(res, contRes)
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

// switchRoutesOnProviderError walks the remaining routes when the session died on a provider
// failure rather than a usage limit: a transient error, a quiet death the resurrection loop could
// not recover, or a non-zero exit before the session completed any tool call. A route with a
// session id is RESUMED so the transcript survives; one that died before naming a session is
// started FRESH with the same prompt. Every attempt's spend lands on the one Result.
func (o opencode) switchRoutesOnProviderError(ctx context.Context, in Invocation, routes []string, res *Result, deadline time.Time, delivery *opencodeInbox) error {
	if len(in.Routes) == 0 || len(routes) < 2 {
		return nil
	}
	started := in.ModelArg()
	if started == "" {
		return nil
	}
	current := started
	next := nextRouteIndex(routes, started)
	var fallbacks []RouteFallback
	for next < len(routes) {
		reason := o.routeFailoverReason(*res)
		if reason == "" {
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
		sid, _ := resumeTargets(*res)
		contIn := in.OnRoute(to)
		if !deadline.IsZero() {
			contIn.MaxSessionWallClock = time.Until(deadline)
		}
		var contRes Result
		var err error
		if sid != "" {
			if in.Console != nil {
				fmt.Fprintf(in.Console, "\n!! %s failed (%s) — resuming the session on %s\n", current, reason, to)
			}
			// Seed the continuation's parser with the live claim exactly as the limit switch does.
			contIn.ResumeClaim = res.Claim
			contRes, err = o.runSession(ctx, contIn, opencodeResumeArgs(contIn, sid, routeProviderErrorPrompt), delivery)
		} else {
			if in.Console != nil {
				fmt.Fprintf(in.Console, "\n!! %s failed (%s) before naming a session — starting fresh on %s\n", current, reason, to)
			}
			contRes, err = o.runSession(ctx, contIn, opencodeArgs(contIn), delivery)
		}
		fallbacks = append(fallbacks, RouteFallback{Route: current, Next: to, Reason: reason})
		if err != nil {
			chargeAttempt(res, contRes)
			setRouteReport(res, started, current, fallbacks)
			return err
		}
		mergeRouteSwitch(res, contRes)
		current = to
	}
	// The route that ended the session in the same shape has no next route: record it too, so the
	// driver marks it rather than sending the next phase straight back into it.
	if reason := o.routeFailoverReason(*res); reason != "" {
		fallbacks = append(fallbacks, RouteFallback{Route: current, Reason: reason})
	}
	setRouteReport(res, started, current, fallbacks)
	return nil
}

// routeFailoverReason names why this result should move to the next route, or "" when it should
// not. The most specific evidence wins: a classified provider error, then the quiet-death
// signature the resurrection loop has already failed to recover, then a death before the session
// completed any tool call.
func (o opencode) routeFailoverReason(res Result) string {
	if res.Untrusted != "" || res.ToolLoop != "" || res.IdleKilled != "" {
		return ""
	}
	// A usage limit belongs to the limit walk, which owns the route's mark and its stated reset.
	if o.DetectLimit(res).Limited {
		return ""
	}
	if o.IsTransient(res) {
		return routeFailReasonProvider
	}
	if o.ZeroUsageUnknown(res) {
		return routeFailReasonQuiet
	}
	// An adapter-caused end (our own wall-clock cap) or a signal is not a provider death: the
	// session was stopped, and switching routes would only spend another turn on the same stop.
	if res.ExitCode != 0 && res.ExitSignal == 0 && !o.WallClockCapped(res) &&
		res.ToolCalls == 0 && RecoveredErrors(res) == 0 {
		return routeFailReasonNoTool
	}
	return ""
}

// nextRouteIndex is the index after the route the session is on. A route the list does not name
// starts the walk at the second configured route, exactly as the limit switch's own index does.
func nextRouteIndex(routes []string, current string) int {
	for i, r := range routes {
		if r == current {
			return i + 1
		}
	}
	return 1
}

// chargeAttempt folds one attempt's spend onto the session Result for an attempt that does NOT
// merge (it errored): every process the adapter ran is paid for exactly once either way.
func chargeAttempt(res *Result, add Result) {
	res.Tokens += add.Tokens
	res.CostUSD += add.CostUSD
	res.Usage.Add(add.Usage)
	res.UsageReported = res.UsageReported || add.UsageReported
	res.CostReported = res.CostReported || add.CostReported
}

// setRouteReport writes the fallback report onto the Result. Nothing is written when the session
// neither switched nor ended limited, so a clean single-route run's Raw keeps its original shape.
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
