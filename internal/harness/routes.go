package harness

import "time"

type RouteFallback struct {
	Route  string
	Next   string
	Reason string
	// ResetAt is the reset the provider STATED, or the zero time when it stated none. The driver owns
	// the fallback mark duration (the same probe interval the supervised wait uses) because the
	// adapter has no config.
	ResetAt time.Time
}

const (
	// RouteStartedKey and RouteFinishedKey are the Result.Raw keys naming the route the session
	// started on and the route that finished it. Both are set only when the adapter actually switched:
	// a single-route result carries neither, so its Raw stays exactly the shape it had before this
	// existed.
	RouteStartedKey  = "route_started"
	RouteFinishedKey = "route_finished"

	RouteFallbacksKey = "route_fallbacks"
)

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
