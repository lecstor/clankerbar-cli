package harness

// opencode_timeout.go — the provider stream timeout the adapter pins (CLA-877).
//
// opencode 2.0.32 takes per-provider settings under `providers.<id>.settings`
// (the 2.x native dialect; the deprecated `provider.<id>.options` dialect is
// normalized onto it). `chunkTimeout` is the web-request SSE read bound: when no
// chunk arrives within the window opencode aborts the request with "SSE read
// timed out" and the run ends with an error event and a non-zero exit. Its own
// default is 900000 ms (15 minutes) — the very bound of the phase idle watchdog,
// so a hung stream stayed silent until the watchdog killed the session, and the
// kill read as a dead phase rather than as a provider timeout. A shorter bound
// turns that silence into an error the driver can classify and retry.

import (
	"strings"
	"time"
)

// opencodeProviderChunkTimeout is the longest silence between streamed chunks
// the adapter tolerates. Five minutes is well inside the idle watchdog's 15m,
// so a hung stream fails as a provider error before the phase is killed, and is
// far above the gap between chunks of a healthy streamed answer (including one
// that thinks for a while: reasoning itself streams). Deliberately NOT the
// full-request `timeout`: a long generation that keeps streaming must not be
// cut off.
//
// A var purely so the conformance test can shrink it rather than wait five
// minutes; the production value is what the unit tests pin.
var opencodeProviderChunkTimeout = 5 * time.Minute

// opencodeProviderTimeouts pins chunkTimeout on every provider this invocation
// can NAME, in the 2.x native dialect. A route is `provider/model`, which is the
// only provider identity an Invocation carries; a model with no `/` (the
// harness's own configured default) names no provider, and opencode's default
// applies there. An existing entry is merged, never replaced: the MCP config may
// itself carry `providers.<id>` (or the deprecated `provider.<id>`), and wiping
// its settings would lose the operator's apiKey/baseURL/models.
//
// Verified against 2.0.32: the content layer is merged last, the setting reaches
// the request wrapper, and a stalled stream then aborts with "SSE read timed
// out" instead of going silent (docs/harness-conformance.md, `_chunkTimeout`).
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
