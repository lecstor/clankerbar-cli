package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ModelRoutes is an ordered list of model route aliases for ONE model field
// (CLA-782): the free route first, then the paid route of the same model, and so
// on. A single route is the historical shape and stays byte-identical on the
// wire, which is what keeps every existing config's Identity() where it was.
//
// The fallback exists because a provider's free tier can rate-limit or withdraw
// a route at any time, and the same model is available paid. The adapter runs
// the first route, and when it hits a usage limit mid-session it resumes the
// SAME session on the next route; the driver remembers which routes were limited
// so the rest of the run stays on the route that worked until the free one's
// reset.
//
// This is deliberately NOT a second kind of model resolution: ModelForPhase and
// ModelForTier keep returning the FIRST route, so every existing caller, log line
// and brief placeholder is unchanged, and a config that names one route (or a
// plain string) behaves exactly as it did before this type existed.
type ModelRoutes []string

// UnmarshalJSON accepts either the historical plain string or an ordered JSON
// array of strings. A JSON `null` or an empty string, like an absent key, is no
// routes at all. A
// non-string element is refused with a message naming the field's shape rather
// than the decoder's, because a mistyped route list must be visible in the
// config load that refused it.
func (m *ModelRoutes) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*m = nil
		return nil
	}
	if trimmed[0] == '[' {
		var list []string
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return fmt.Errorf("a model route list must be an array of strings: %w", err)
		}
		*m = ModelRoutes(list)
		return nil
	}
	var alias string
	if err := json.Unmarshal(trimmed, &alias); err != nil {
		return fmt.Errorf("a model must be a string or an array of route strings: %w", err)
	}
	// An explicit `"model": ""` is the historical "no model", so it decodes to no
	// routes rather than one blank route. A one-element slice is non-empty to
	// `omitempty` (Phase.Model) and to len()-based checks (HarnessConfig.Empty),
	// so keeping it would emit a key the string field omitted, moving
	// Config.Identity(), and make a block holding only `"model": ""` read as
	// configured. Only the exact empty string: a whitespace alias always
	// marshalled as itself, and still does.
	if alias == "" {
		*m = nil
		return nil
	}
	*m = ModelRoutes{alias}
	return nil
}

// MarshalJSON emits a plain string for zero or one route and only uses the array
// form when there is a choice to make.
//
// The zero case is load-bearing for Config.Identity(): these fields marshal
// without `omitempty` (the historical tags), so an unset field used to write
// `"model":""` and must keep writing exactly that. A config that names one route
// likewise hashes the same bytes as the identical string config, so moving a
// config to the new shape is not by itself an identity change.
func (m ModelRoutes) MarshalJSON() ([]byte, error) {
	switch len(m) {
	case 0:
		return json.Marshal("")
	case 1:
		return json.Marshal(m[0])
	default:
		return json.Marshal([]string(m))
	}
}

// First returns the route the session starts on: the first non-blank entry, or
// "" for no routes (the harness's own default). Blank entries are skipped for
// the same reason the rest of this package trims every model: a whitespace alias
// that reaches a child's model flag kills every session of an unattended run.
func (m ModelRoutes) First() string {
	for _, route := range m {
		if r := strings.TrimSpace(route); r != "" {
			return r
		}
	}
	return ""
}

// List returns every non-blank route in configured order, trimmed, with exact
// duplicates dropped and nothing else reordered: the order IS the fallback
// order, and a route listed twice has no more value the second time.
func (m ModelRoutes) List() []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	seen := make(map[string]bool, len(m))
	for _, route := range m {
		r := strings.TrimSpace(route)
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Empty reports whether the field names no model at all — the same nothing the
// historical empty string meant.
func (m ModelRoutes) Empty() bool { return len(m.List()) == 0 }

// cloneRoutesMap copies a route-valued tier map one level deep, so a cloned
// config's overlay cannot write through to the base it was cloned from.
func cloneRoutesMap(m map[string]ModelRoutes) map[string]ModelRoutes {
	if m == nil {
		return nil
	}
	out := make(map[string]ModelRoutes, len(m))
	for k, v := range m {
		out[k] = append(ModelRoutes(nil), v...)
	}
	return out
}
