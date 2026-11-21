package harness

// opencode_timeout.go — the provider stream timeout the adapter pins.

import (
	"strings"
	"time"
)

// opencodeProviderChunkTimeout is the longest silence between streamed chunks the adapter
// tolerates. Five minutes is well inside the idle watchdog's 15m, so a hung stream fails as a
// provider error before the phase is killed, and is far above the gap between chunks of a healthy
// streamed answer (including one that thinks for a while: reasoning itself streams).
var opencodeProviderChunkTimeout = 5 * time.Minute

// opencodeProviderTimeouts pins chunkTimeout on every provider this invocation can NAME, in the 2.x
// native dialect. A route is `provider/model`, which is the only provider identity an Invocation
// carries; a model with no `/` (the harness's own configured default) names no provider, and
// opencode's default applies there.
func opencodeProviderTimeouts(in Invocation, doc map[string]any) {
	ids := map[string]bool{}
	for _, route := range in.RouteList() {
		if id, _, ok := strings.Cut(route, "/"); ok && id != "" {
			ids[id] = true
		}
	}
	if len(ids) == 0 {
		return
	}
	providers, _ := doc["providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
		doc["providers"] = providers
	}
	for id := range ids {
		entry, _ := providers[id].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
			providers[id] = entry
		}
		settings, _ := entry["settings"].(map[string]any)
		if settings == nil {
			settings = map[string]any{}
			entry["settings"] = settings
		}
		settings["chunkTimeout"] = opencodeProviderChunkTimeout.Milliseconds()
	}
}
