package loop

// Console -> agent message delivery in the driver (CLA-606, CLA-911). The plane origin and
// key are the same trusted pair the event channel uses; what this file decides is which
// sessions may receive: only an adapter that declares DeliversMessages (claude and
// opencode), never a probe.

import (
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// SetInboxChannel wires the plane origin and bearer the inbox poller uses. Unset (or an
// adapter with no delivery) leaves every session without one.
func (d *Driver) SetInboxChannel(origin, apiKey string) {
	d.inboxOrigin = origin
	d.inboxKey = apiKey
}

// inboxOn reports whether a session on this harness may receive console messages.
func (d *Driver) inboxOn(harnessName string, probe bool) bool {
	if probe || d.inboxOrigin == "" || d.inboxKey == "" {
		return false
	}
	caps, ok := harness.CapabilitiesOf(harnessName)
	return ok && caps.DeliversMessages
}
