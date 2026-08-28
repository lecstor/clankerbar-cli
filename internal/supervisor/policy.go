package supervisor

// Permission-policy gate (phase 2c of docs/proposals/daemon-supervisor.md in
// the clankerbar repo): the supervisor never starts a daemon whose permission
// policy file is absent.
//
// `settings_path` names the fail-closed headless permission policy — the
// allow/deny rules an unattended session is gated by — and it is deliberately
// read-only to agents: a plane that could set it could hand a daemon a
// permissive policy, which is the escalation the local/cloud boundary exists
// to prevent (the proposal's "what can never come from the cloud"). The
// supervisor enforces the fail-closed half at the child-start gate: the file
// the config names must exist, or the daemon is refused — it would otherwise
// start with no policy at all, exactly the state the boundary forbids.
//
// The gate checks the EFFECTIVE settings path of every harness the config
// will spawn sessions on — SessionFor's fallback from the per-harness block
// to the top-level settings_path applied — after Validate has resolved it,
// because that is the path the materialized config carries and the child
// loads. A config that names NO settings_path is untouched: it runs on the
// ambient allowlist, which is doctor's warning to make, not the supervisor's
// refusal.

import (
	"errors"
	"fmt"
	"os"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// ErrPolicyRefused is the sentinel for a permission policy that cannot be
// verified at the child-start gate: the file a spawned harness's settings_path
// names is absent. The wrapped text names the path that was tried.
var ErrPolicyRefused = errors.New("permission_policy_refused")

// checkPermissionPolicy verifies that the permission-policy file every harness
// the config spawns sessions on will load exists: SessionFor(h).SettingsPath
// for each spawned harness. An absent file — or a path that is not a file —
// refuses with ErrPolicyRefused naming the path; an empty settings_path (no
// policy named) passes.
func checkPermissionPolicy(cfg *config.Config) error {
	for _, h := range cfg.SpawnedHarnesses() {
		path := cfg.SessionFor(h).SettingsPath
		if path == "" {
			continue
		}
		if fi, err := os.Stat(path); err != nil || fi.IsDir() {
			return fmt.Errorf("%w: %s (the permission policy harness %q will load) is absent - refusing to start the daemon without it", ErrPolicyRefused, path, h)
		}
	}
	return nil
}
