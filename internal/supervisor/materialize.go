package supervisor

// Materialization (phase 2b of docs/proposals/daemon-supervisor.md in the
// clankerbar repo): the supervisor generates each child's EFFECTIVE config
// into the child's own state dir and starts the child with `run -c` pointing
// at the generated file, so the config a daemon runs on is a generated
// artifact rather than something the operator maintains — the precondition
// for the roster driving it in phase 3b.
//
// The generated file is a CACHE: regenerated on every reconcile (each spawn
// re-reads the source config and rewrites it), never hand-edited, safe to
// delete while the child runs, and written to disk rather than held in memory
// so it doubles as the offline last-known-good (phase 3b: an unreachable plane
// means starting from cache, not starting nothing).
//
// What the generated file carries:
//
//   - the operator's declared intent, validated exactly as `run` validates it
//     (harness, project, prompt, budget, ...), and
//   - the machine conventions materialized from the environment and the
//     documented defaults, NOT from a per-daemon file:
//     - `workdir` from the phase-2a derivation (the derived value is consumed
//       here; with no machine root stated, the config's own workdir governs),
//     - `settings_path`, `config_dir` and `mcp_config_path` resolved through
//       their documented defaults (already applied by Validate),
//     - `backlog_url` at the default plane origin — the credential itself
//       lives only in CLANKERBAR_API_KEY in the environment, which the child
//       inherits from the supervisor.
//
// Two values are PINNED, because the file moved and the child must not derive
// them differently:
//
//   - `instance_name` is set to the identity resolved from the SOURCE config
//     path, so moving the file from the config dir to the state dir does not
//     change what the fleet beacon reports (ResolveInstanceName keys off the
//     file's basename), and
//   - `state_dir` is set to the directory this file lives in, so the child's
//     control markers land exactly where the supervisor writes STOP/HALT.
//
// A side effect the proposal records: Config.Identity() hashes the effective
// validated config, so the generated file makes the hash comparable across
// machines running the same entry.

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// applyDerivedWorkdirs writes the phase-2a derived workdirs into the config
// the materialized file is built from: the single-project config's workdir,
// or each project's own. The top-level workdir of a multi-project config
// stays as declared — it is the multi-repo parent, which the derivation does
// not derive (sessions run in the per-project workdirs).
func applyDerivedWorkdirs(cfg *config.Config, derived map[string]string) {
	if len(cfg.Projects) == 0 {
		if dir, ok := derived[""]; ok {
			cfg.WorkDir = dir
		}
		return
	}
	for i := range cfg.Projects {
		if dir, ok := derived[cfg.Projects[i].Slug]; ok {
			cfg.Projects[i].WorkDir = dir
		}
	}
}

// materializeConfig builds the instance's effective config and writes it into
// the instance's state dir, then returns the path the child is spawned with.
//
// The state dir is opened through statedir — created if needed, refused if it
// is somebody else's directory — with the same session-root anchors the child
// itself uses, so the supervisor and the daemon agree about what this
// directory is. Creating it here is deliberate: the child is spawned with a
// file INSIDE it, so the chicken-and-egg has to resolve on the supervisor's
// side. A created directory is not a planted marker: the child's statedir.Open
// adopts it (the sentinel is written, the materialized file is ours) and the
// STOP write is still gated by the settle window, never by directory absence.
//
// The write REPLACES the previous materialization — the cache is regenerated,
// never appended to. The replace is remove-then-create through the state-dir
// handle, so a symlink planted at the name is removed, never followed.
func (d *Supervisor) materializeConfig(inst *Instance) (string, error) {
	inst.mu.Lock()
	cfg := inst.cfg
	dir := inst.stateDir
	inst.mu.Unlock()

	st, err := statedir.Open(dir, cfg.SessionWorkDirs()...)
	if err != nil {
		return "", err
	}
	defer st.Close() //nolint:errcheck // read-side handle on the way out

	// The effective config: the validated instance config with the two values
	// the child must not derive for itself pinned (see the package comment).
	eff := *cfg
	eff.InstanceName = cfg.ResolvedInstanceName(d.hostname)
	eff.StateDir = dir
	data, err := json.MarshalIndent(&eff, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode %s: %w", statedir.MaterializedConfigName, err)
	}

	if st.Exists(statedir.MaterializedConfigName) {
		if err := st.Remove(statedir.MaterializedConfigName); err != nil {
			return "", fmt.Errorf("replace %s: %w", statedir.MaterializedConfigName, err)
		}
	}
	if err := st.WriteFile(statedir.MaterializedConfigName, data); err != nil {
		return "", fmt.Errorf("write %s: %w", statedir.MaterializedConfigName, err)
	}
	return filepath.Join(dir, statedir.MaterializedConfigName), nil
}