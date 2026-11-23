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
	// started on and the route that finished it. Both are set only when the session switched routes or
	// ended limited on its route list; a clean single-route result carries neither.
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

// RouteFinished is the route that ended the session, or "" when no route report was written (a
// clean session that never switched). A limited one-route session reports its own route.
func RouteFinished(res Result) string {
	_, finished, _ := RouteFallbacks(res)
	return finished
}
