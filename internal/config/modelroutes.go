package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ModelRoutes is an ordered list of model route aliases for ONE model field: the free route first,
// then the paid route of the same model, and so on. The fallback exists because a provider's free
// tier can rate-limit or withdraw a route at any time, and the same model is available paid.
type ModelRoutes []string

// UnmarshalJSON accepts either the historical plain string or an ordered JSON array of strings. A
// non-string element is refused with a message naming the field's shape rather than the decoder's,
// because a mistyped route list must be visible in the config load that refused it.
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
	// An explicit `"model": ""` is the historical "no model", so it decodes to no routes rather than
	// one blank route.
	if alias == "" {
		*m = nil
		return nil
	}
	*m = ModelRoutes{alias}
	return nil
}

// MarshalJSON emits a plain string for zero or one route and only uses the array form when there is
// a choice to make. The zero case is load-bearing for Config.Identity(): these fields marshal
// without `omitempty` (the historical tags), so an unset field used to write `"model":""` and must
// keep writing exactly that.
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

// First returns the route the session starts on: the first non-blank entry, or "" for no routes
// (the harness's own default).
func (m ModelRoutes) First() string {
	for _, route := range m {
		if r := strings.TrimSpace(route); r != "" {
			return r
		}
	}
	return ""
}

// List returns every non-blank route in configured order, trimmed, with exact duplicates dropped
// and nothing else reordered: the order IS the fallback order, and a route listed twice has no more
// value the second time.
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

// Empty reports whether the field names no model at all — the same nothing the historical empty
// string meant.
func (m ModelRoutes) Empty() bool { return len(m.List()) == 0 }

// cloneRoutesMap copies a route-valued tier map one level deep, so a cloned config's overlay cannot
// write through to the base it was cloned from.
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
