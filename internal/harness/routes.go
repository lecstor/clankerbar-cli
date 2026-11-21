package harness

import "time"

// RouteFallback is one model route the adapter abandoned mid-session because it hit a usage limit,
// and the route it moved to.
type RouteFallback struct {
	// Route is the route that was denied, exactly as it was configured.
	Route string
	// Next is the route the session was resumed on.
	Next   string
	Reason string
	// ResetAt is the reset the provider STATED, or the zero time when it stated none.
	ResetAt time.Time
}

const (
	// RouteStartedKey and RouteFinishedKey are the Result.Raw keys naming the route the session
	// started on and the route that finished it.
	RouteStartedKey  = "route_started"
	RouteFinishedKey = "route_finished"

	RouteFallbacksKey = "route_fallbacks"
)

// RouteFallbacks reads the adapter's report off a Result.
func RouteFallbacks(res Result) (started, finished string, fallbacks []RouteFallback) {
	if res.Raw == nil {
		return "", "", nil
	}
	started, _ = res.Raw[RouteStartedKey].(string)
	finished, _ = res.Raw[RouteFinishedKey].(string)
	fallbacks, _ = res.Raw[RouteFallbacksKey].([]RouteFallback)
	return started, finished, fallbacks
}

// RouteFinished is the route that ended the session, or "" when it finished on the route it started
// on (or never reported).
func RouteFinished(res Result) string {
	_, finished, _ := RouteFallbacks(res)
	return finished
}
