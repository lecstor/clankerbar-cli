package harness

// turns.go — the multi-turn invocation contract.

import (
	"errors"
	"fmt"
	"strings"
)

// Turn is one follow-up message in a multi-turn invocation.
type Turn struct {
	// Prompt is the message this turn sends.
	Prompt string

	// Model optionally names the model THIS turn runs on, overriding Invocation.Model for the turn
	// alone. Blank (or whitespace) means the invocation's model.
	Model string
}

// OnTurn returns a copy of the invocation pinned to one follow-up turn: the turn's own model when
// it names one, otherwise the invocation's current one.
func (in Invocation) OnTurn(t Turn) Invocation {
	if m := strings.TrimSpace(t.Model); m != "" {
		in.Model = m
		in.Routes = nil
	}
	return in
}

// OnFinishedRoute pins the invocation to the route a turn finished on, dropping the routes already
// abandoned. A later turn's own limit must not walk the list back onto a route this session has
// already exhausted.
func (in Invocation) OnFinishedRoute(route string) Invocation {
	route = strings.TrimSpace(route)
	if route == "" {
		return in
	}
	if routes := in.RouteList(); len(routes) > 0 {
		for i, r := range routes {
			if r == route {
				in.Model = route
				in.Routes = append([]string(nil), routes[i:]...)
				return in
			}
		}
	}
	in.Model = route
	in.Routes = nil
	return in
}

// ErrTurnsUnsupported is what an adapter that cannot continue a live session returns for an
// invocation carrying Invocation.Turns.
var ErrTurnsUnsupported = errors.New("multi-turn invocations (Invocation.Turns) are not supported by this harness")

// refuseTurns is the check an adapter without session continuation runs before anything else, so a
// planned sequence is refused up front rather than run as its first turn. The adapter name prefixes
// the error: the caller picked the harness, and the message has to say which one cannot do this.
func refuseTurns(name string, in Invocation) error {
	if len(in.Turns) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %w", name, ErrTurnsUnsupported)
}
