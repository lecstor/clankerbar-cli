package fleet

// wire.go — the JSON shape marshalReport sends: exactly what the plane's
// parseFleetReport reads (CLA-465, CLA-630, CLA-691, CLA-703, CLA-728, CLA-908,
// CLA-1026). Kept beside the reporter so every wire bound and state restriction
// lives in one place rather than at each producer's call site.

import (
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"
)

// wireResetAt renders a `limited` state's stated reset instant in the one shape
// the plane accepts (an ISO-8601 instant with an explicit zone), or "" — which
// `omitempty` drops — when the harness stated none (CLA-728). UTC so the bytes
// never depend on the sending machine's zone; second precision because a
// stated reset is a boundary, not a sub-second deadline.
//
// Restricted to StateLimited on purpose: the plane reads resetAt only there, so
// a reset left on some other state is dropped here rather than shipped as a
// field the plane would ignore — one representation of "no reset", not two.
func wireResetAt(st State) string {
	if st.Kind != StateLimited || st.ResetAt.IsZero() {
		return ""
	}
	return st.ResetAt.UTC().Format(time.RFC3339)
}

// wirePauseReason and wirePauseUntil render a draining state's pause payload
// (CLA-908), or the empty string — which `omitempty` drops — on any other kind.
// The plane reads the pair only on `draining`, and `pauseUntil` only beside the
// schedule reason, so a value left on another state is dropped here rather than
// shipped as a key the plane would ignore.
func wirePauseReason(st State) string {
	if st.Kind != StateDraining {
		return ""
	}
	return st.PauseReason
}

func wirePauseUntil(st State) string {
	if wirePauseReason(st) != PauseReasonSchedule || st.PauseUntil.IsZero() {
		return ""
	}
	return st.PauseUntil.UTC().Format(time.RFC3339)
}

// The plane's bounds on `state.model` and `state.routes` (CLA-1025/CLA-1026):
// MAX_ROUTE_NAME, MAX_ROUTE_SCHEDULE and MAX_ROUTE_HOLDS in @clankerbar/shared.
// The plane REFUSES the whole report — presence refresh included — on an
// over-bound value, so the reporter must never send one.
const (
	maxRouteHolds    = 16
	maxRouteName     = 120
	maxRouteSchedule = 100
)

// wireModel renders an iteration state's live route, or "" — which `omitempty`
// drops — on any other kind or when no route was resolved. The plane reads
// `model` only on `iteration`, so a value left on another state is dropped here
// rather than shipped as a key the plane would ignore.
func wireModel(st State) string {
	if st.Kind != StateIteration {
		return ""
	}
	model := strings.TrimSpace(st.Model)
	if model == "" {
		return ""
	}
	if len(model) > maxRouteName {
		// The plane refuses the whole report on this, losing unrelated rows and the
		// presence refresh; a route name can only come from config, so this is a
		// config bug to see rather than a value to ship.
		log.Printf("fleet: dropping state.model %q: over the plane's %d-character route bound", model, maxRouteName)
		return ""
	}
	return model
}

// wireRoutes renders a paused state's per-route hold list, or nil — which
// `omitempty` drops — on any other kind or when nothing is held. The plane reads
// `routes` only on `limited` and `draining`, so a list left on another state is
// dropped here for the same reason as resetAt. The FIRST maxRouteHolds entries
// in configured order are kept: the plane refuses the whole report beyond its
// cap, and the first routes are the ones the phase will try first.
func wireRoutes(st State) []wireRouteHold {
	if st.Kind != StateLimited && st.Kind != StateDraining {
		return nil
	}
	if len(st.Routes) == 0 {
		return nil
	}
	routes := st.Routes
	if len(routes) > maxRouteHolds {
		log.Printf("fleet: %d route holds exceed the plane's cap of %d — reporting the first %d in configured order and dropping %d, or the plane would refuse the whole report",
			len(routes), maxRouteHolds, maxRouteHolds, len(routes)-maxRouteHolds)
		routes = routes[:maxRouteHolds]
	}
	out := make([]wireRouteHold, 0, len(routes))
	for _, h := range routes {
		route := strings.TrimSpace(h.Route)
		if route == "" || len(route) > maxRouteName {
			log.Printf("fleet: dropping a route hold: route %q is empty or over the plane's %d-character bound", h.Route, maxRouteName)
			continue
		}
		reason := strings.TrimSpace(h.Reason)
		if reason != RouteHoldUsageLimit && reason != RouteHoldWindow {
			log.Printf("fleet: dropping a route hold: route %q names reason %q, not one of the two the plane knows", route, h.Reason)
			continue
		}
		w := wireRouteHold{Route: route, Reason: reason}
		if reason == RouteHoldWindow {
			if sched := strings.TrimSpace(h.Schedule); sched != "" {
				if len(sched) > maxRouteSchedule {
					log.Printf("fleet: route hold on %q names a %d-character schedule, over the plane's %d bound — reporting the window without its name", route, len(sched), maxRouteSchedule)
				} else {
					w.Schedule = sched
				}
			}
		}
		if !h.Until.IsZero() {
			w.Until = h.Until.UTC().Format(time.RFC3339)
		}
		out = append(out, w)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ── Wire shape ────────────────────────────────────────────────────────────────
// Exactly what apps/web/src/server/fleet-report.ts parseFleetReport reads
// (CLA-465). Kept as a separate marshalling step with its own tags so the Go
// field names can say what they mean without leaking into the JSON.

type wireState struct {
	Kind    string `json:"kind"`
	N       int    `json:"n,omitempty"`
	TaskRef string `json:"taskRef,omitempty"`
	Phase   string `json:"phase,omitempty"`
	// Model is the running iteration's live route (CLA-1026): the plane reads it
	// on `iteration` only and stores it bounded to MAX_ROUTE_NAME. A string so
	// `omitempty` can actually omit it, like ResetAt.
	Model string `json:"model,omitempty"`
	// Routes is the per-route hold list (CLA-1026): read on `limited` and
	// `draining` only, bounded to the plane's MAX_ROUTE_HOLDS. Omitted when
	// nothing is held, so an older plane reads exactly the bytes it always did.
	Routes []wireRouteHold `json:"routes,omitempty"`
	// ResetAt is the `limited` state's payload (CLA-728), RFC 3339 with an
	// explicit zone — the only shape the plane's parser accepts. Omitted when
	// the harness stated no reset, so the plane reads "probing", never a
	// guessed instant. A string rather than a time.Time so `omitempty` can
	// actually omit it: time.Time is a struct, and the encoder never treats
	// its zero value as empty.
	ResetAt string `json:"resetAt,omitempty"`
	// PauseReason/PauseUntil are the `draining` state's payload (CLA-908), in the
	// shape the plane's parseFleetReport accepts: a bounded reason token
	// (PauseReason*) and, for the schedule alone, the window's end as RFC 3339
	// with an explicit zone. Both are omitted on every other kind, exactly like
	// resetAt, so an old plane sees the bytes it always did.
	PauseReason string `json:"pauseReason,omitempty"`
	PauseUntil  string `json:"pauseUntil,omitempty"`
}

// wireRouteHold is one `state.routes` entry. `route` and `reason` are always
// present (the plane refuses an entry without them); `schedule` rides only a
// window hold and `until` only when the hold states an end.
type wireRouteHold struct {
	Route    string `json:"route"`
	Reason   string `json:"reason"`
	Schedule string `json:"schedule,omitempty"`
	Until    string `json:"until,omitempty"`
}

// wireUsage is the normalised breakdown on the wire. Each key is omitted when
// zero, so a session that reported usage with an empty bucket stays a compact
// object rather than five zeroes.
type wireUsage struct {
	Input      int `json:"input,omitempty"`
	Output     int `json:"output,omitempty"`
	Reasoning  int `json:"reasoning,omitempty"`
	CacheRead  int `json:"cacheRead,omitempty"`
	CacheWrite int `json:"cacheWrite,omitempty"`
}

type wireSession struct {
	Phase   string `json:"phase"`
	Harness string `json:"harness"`
	// Model is deliberately not omitempty: the plane stores the empty string as
	// null ("not resolved"), and sending it explicitly is CLA-630's contract.
	Model string `json:"model"`
	// UsageReported, Usage and CostUSD are the CLA-691 additions, and each is
	// omitted when there is nothing to say: an older CLI's entry — and a session
	// that died before its harness reported anything — stays the pre-CLA-691
	// shape byte for byte, so the plane's "no usage" case is representable.
	UsageReported bool       `json:"usageReported,omitempty"`
	Usage         *wireUsage `json:"usage,omitempty"`
	// CostUSD is a pointer so a reported ZERO survives marshalling
	// (omitempty drops a nil pointer, never a pointer to 0).
	CostUSD *float64 `json:"costUsd,omitempty"`
	// RunID omits itself when the session held no claim, like the rest.
	RunID string `json:"runId,omitempty"`
	// Cause is omitted for an ordinary session, so an older plane sees the same
	// bytes it always did. The current plane ignores unknown session keys; the
	// classification is carried for the durable phase_session record to read
	// once its parser stores it (CLA-703 follow-up).
	Cause string `json:"cause,omitempty"`
}

type wireIteration struct {
	TaskID          string        `json:"taskId,omitempty"`
	TaskRef         string        `json:"taskRef,omitempty"`
	Phases          []string      `json:"phases,omitempty"`
	Sessions        []wireSession `json:"sessions,omitempty"`
	Outcome         string        `json:"outcome"`
	DurationSeconds float64       `json:"durationSeconds,omitempty"`
	Tokens          int           `json:"tokens,omitempty"`
	// CostUSD is the iteration's dollar spend; always present — UNLIKE Tokens
	// (omitempty) — so a zero (nothing reported) stays distinguishable from a
	// missing key (an older CLI). CLA-677.
	CostUSD float64 `json:"costUsd"`
}

type wireReport struct {
	Instance       string          `json:"instance"`
	Host           string          `json:"host,omitempty"`
	Version        string          `json:"version,omitempty"`
	ConfigIdentity string          `json:"configIdentity,omitempty"`
	Role           string          `json:"role,omitempty"`
	State          *wireState      `json:"state,omitempty"`
	Iterations     []wireIteration `json:"iterations,omitempty"`
}

func marshalReport(rep Report) ([]byte, error) {
	// An unnamed instance would upsert a garbage row keyed on "" — refuse here,
	// once, rather than teach the plane to defend against it.
	if rep.Instance == "" {
		return nil, errors.New("fleet report has no instance name")
	}
	w := wireReport{
		Instance:       rep.Instance,
		Host:           rep.Host,
		Version:        rep.Version,
		ConfigIdentity: rep.ConfigIdentity,
		Role:           rep.Role,
		State: &wireState{
			Kind:        rep.State.Kind,
			N:           rep.State.N,
			TaskRef:     rep.State.TaskRef,
			Phase:       rep.State.Phase,
			Model:       wireModel(rep.State),
			Routes:      wireRoutes(rep.State),
			ResetAt:     wireResetAt(rep.State),
			PauseReason: wirePauseReason(rep.State),
			PauseUntil:  wirePauseUntil(rep.State),
		},
	}
	if w.State.Kind == "" {
		w.State.Kind = StateIdle
	}
	for _, it := range rep.Iterations {
		switch it.Outcome {
		case OutcomeCheckpoint, OutcomeReleased, OutcomeParked, OutcomeDead:
		default:
			// A typo'd outcome would 400 the WHOLE report plane-side
			// (all-or-nothing batch), losing the presence refresh too. Drop the
			// row here and log it directly: returning an error would lose the
			// presence half locally, and deliver's failStreak could silence it.
			log.Printf("fleet: dropping iteration %q: outcome %q is not one of the four the plane knows", it.TaskRef, it.Outcome)
			continue
		}
		w.Iterations = append(w.Iterations, wireIteration{
			TaskID:          it.TaskID,
			TaskRef:         it.TaskRef,
			Phases:          it.Phases,
			Sessions:        wireSessions(it.Sessions),
			Outcome:         it.Outcome,
			DurationSeconds: it.DurationSeconds,
			Tokens:          it.Tokens,
			CostUSD:         it.CostUSD,
		})
	}
	return json.Marshal(w)
}

// maxSessionsPerIteration mirrors the plane's MAX_SESSIONS_PER_ITERATION
// (apps/web/src/server/fleet-report.ts). The plane refuses the WHOLE report —
// all-or-nothing, presence refresh included — when an iteration carries more,
// so the cap is an interface this side has to honour rather than a bound the
// plane silently sheds. The shipped phase sequences cannot reach it (handoff
// respawns are capped per phase and dead retries per task), but a many-phase
// config plus respawns on each could; clamping the tail keeps the report — and
// with it the iteration record and presence — valid, at the cost of the
// least-important entries.
const maxSessionsPerIteration = 20

// wireSessions converts one iteration's sessions to the wire shape, clamping at
// the plane's per-iteration cap. The FIRST entries in spawn order are kept: the
// configured phases lead the list, and it is the tail of respawns that a run
// long enough to cross the cap has accumulated. The clamp is logged because it
// means the reporter overran a contract the plane will not forgive — a loop bug
// to see, not a normal path.
func wireSessions(sessions []Session) []wireSession {
	if len(sessions) == 0 {
		return nil // no key on the wire: the pre-CLA-630 shape, which stays valid
	}
	if len(sessions) > maxSessionsPerIteration {
		log.Printf("fleet: iteration spawned %d sessions, over the plane's per-iteration cap of %d — reporting the first %d in spawn order and dropping %d, or the plane would refuse the whole report",
			len(sessions), maxSessionsPerIteration, maxSessionsPerIteration, len(sessions)-maxSessionsPerIteration)
		sessions = sessions[:maxSessionsPerIteration]
	}
	out := make([]wireSession, 0, len(sessions))
	for _, s := range sessions {
		w := wireSession{Phase: s.Phase, Harness: s.Harness, Model: s.Model, RunID: s.RunID, Cause: s.Cause}
		if s.UsageReported {
			w.UsageReported = true
			w.Usage = &wireUsage{
				Input:      s.Usage.Input,
				Output:     s.Usage.Output,
				Reasoning:  s.Usage.Reasoning,
				CacheRead:  s.Usage.CacheRead,
				CacheWrite: s.Usage.CacheWrite,
			}
		}
		// CLA-737: the plane refuses a session carrying costUsd without
		// usageReported: true ("session.costUsd requires session.usageReported"),
		// and it refuses the WHOLE report with it. sessionSpend already withholds
		// the figure at the aggregated source, but Session is constructible by any
		// caller and the wire is where the contract lives — so the guarantee is
		// made here too, rather than trusting every future producer to remember a
		// rule whose violation costs unrelated iterations and presence as well.
		if s.UsageReported && s.CostUSD != nil {
			w.CostUSD = s.CostUSD
		}
		// Dropping a figure the caller DID report is a producer bug, and a silent
		// one would let the session sums stop tying up to the iteration-level
		// costUsd with nothing to explain the gap. Every other contract stumble on
		// this wire is logged (the clamp above, endIteration's BUG line,
		// marshalReport's refusal); the guard's job is protecting the batch, not
		// hiding the case it was written to catch (review 1.review-2.3).
		if s.CostUSD != nil && !s.UsageReported {
			log.Printf("fleet: session %q (%s/%s) reported costUsd %v with no usage report — dropping the figure (BUG: the producer bypassed sessionSpend, whose contract is that a cost is known only when the attempt reported usage)",
				s.Phase, s.Harness, s.Model, *s.CostUSD)
		}
		out = append(out, w)
	}
	return out
}
