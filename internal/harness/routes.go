package harness

import "time"

// Model route fallback (CLA-782).
//
// A model field may name an ORDERED list of routes for the same model — a free
// provider route first, its paid twin after it — and the opencode adapter uses
// that list inside ONE Invoke: when a route hits a usage limit it resumes the
// SAME session on the next route, so the history survives and the work carries
// on. The adapter reports what it did through Result.Raw (the same channel the
// resurrection counter uses), and the driver records the marks so the REST of
// the run stays on the route that worked until the limited one's reset.
//
// The types live here rather than in the opencode adapter because the driver
// reads them and must not import an adapter package for the shape. Only opencode
// implements the switch today; a single-route Invocation on any adapter behaves
// exactly as it did before this existed.

// RouteFallback is one model route the adapter abandoned mid-session because it
// hit a usage limit, and the route it moved to.
type RouteFallback struct {
	// Route is the route that was denied, exactly as it was configured.
	Route string
	// Next is the route the session was resumed on. Empty when the denied route
	// was the LAST route: there was nowhere to switch to, and the limit stands
	// for the driver's existing limit branch to wait out or stop on.
	Next string
	// Reason is the adapter's own classification of the limit ("usage_limit",
	// "budget_exhausted"), for the log line.
	Reason string
	// ResetAt is the reset the provider STATED, or the zero time when it stated
	// none. The driver owns the fallback mark duration (the same probe interval
	// the supervised wait uses) because the adapter has no config.
	ResetAt time.Time
}

const (
	// RouteStartedKey and RouteFinishedKey are the Result.Raw keys naming the
	// route the session started on and the route that finished it. Both are set
	// only when the adapter actually switched: a single-route result carries
	// neither, so its Raw stays exactly the shape it had before this existed.
	RouteStartedKey  = "route_started"
	RouteFinishedKey = "route_finished"

	// RouteFallbacksKey is the Result.Raw key carrying []RouteFallback, in the
	// order the routes were denied.
	RouteFallbacksKey = "route_fallbacks"
)

// RouteFallbacks reads the adapter's report off a Result. All three values are
// zero when the session never switched.
func RouteFallbacks(res Result) (started, finished string, fallbacks []RouteFallback) {
	if res.Raw == nil {
		return "", "", nil
	}
	started, _ = res.Raw[RouteStartedKey].(string)
	finished, _ = res.Raw[RouteFinishedKey].(string)
	fallbacks, _ = res.Raw[RouteFallbacksKey].([]RouteFallback)
	return started, finished, fallbacks
}

// RouteFinished is the route that ended the session, or "" when it finished on
// the route it started on (or never reported).
func RouteFinished(res Result) string {
	_, finished, _ := RouteFallbacks(res)
	return finished
}
