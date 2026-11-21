package loop

// Console -> agent message delivery in the driver.

import (
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// SetInboxChannel wires the plane origin and bearer the inbox poller uses. Unset (or an adapter
// with no delivery) leaves every session without one.
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
