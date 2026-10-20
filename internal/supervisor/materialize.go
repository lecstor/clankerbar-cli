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
// or each project's own. Since CLA-678 the derived value is the PROJECT ROOT —
// <root>/<project slug>, holding that project's clones and its worktrees
// directory — which is the directory a session's permission scope is bounded
// to; the session itself still starts in the task's checkout INSIDE it, which
// the config's own repo resolution finds at <project root>/<repo name>.
//
// Applying a project root also REWRITES that project's `repos` path overrides
// to the fleet's own clones under it (managedCheckouts). The derivation
// just cloned them; leaving the operator's path in place would make
// config.ResolveCheckout prefer the developer's checkout over the clone the
// fleet created, and DeclaredCheckouts would hand that outside path to the
// session's permission policy — the managed clone would sit unused while the
// session ran (and could edit) outside its project root.
//
// The top-level workdir of a MULTI-project config stays as declared — it is
// the multi-repo parent, which the derivation does not derive (sessions run in
// the per-project workdirs).
//
// A single-project config is the instance itself, not a multi-repo parent, so
// its top-level workdir follows the derived project root too (the phase-2b
// shape the roster entry replaces, whose derivation landed top-level). Without
// the pin, the materialized file's top-level `workdir` would be the
// supervisor's launch cwd — implicit, machine-context-dependent — and the
// top-level conventions resolved against it (mcp_config_path discovery,
// relative settings_path, Config.Identity) would vary with wherever the
// supervisor happened to start rather than the project root the entry names.
func applyDerivedWorkdirs(cfg *config.Config, derived map[string]string) error {
	if len(cfg.Projects) == 0 {
		// The single-project config's derived root is keyed by the slug its MCP
		// endpoint names (deriveInstanceWorkdirs), not by "". Only a root that
		// was actually derived is applied — and only then is the scope flag
		// asserted, because the flag claims the layout is in place.
		if dir, ok := derived[cfg.Slug()]; ok {
			cfg.WorkDir = dir
			cfg.ProjectRoot = true
			repos, err := managedCheckouts(cfg.Repos, dir)
			if err != nil {
				return fmt.Errorf("project %s: %w", cfg.Slug(), err)
			}
			cfg.Repos = repos
		}
		return nil
	}
	for i := range cfg.Projects {
		dir, ok := derived[cfg.Projects[i].Slug]
		if !ok {
			continue
		}
		cfg.Projects[i].WorkDir = dir
		cfg.ProjectRoot = true
		// Rewrite this project's EFFECTIVE map — its own when it declares one,
		// else the top-level fallback — into a FRESH map carrying the same
		// identity keys and this project's managed clones. A fresh map (rather
		// than an in-place rewrite) is load-bearing twice over: the effective
		// map may be the top-level one shared by every project (whose roots
		// differ), and Config.Clone shares these maps with the base config
		// that the supervisor re-reads on every build.
		repos, err := managedCheckouts(cfg.ReposFor(cfg.Projects[i].Slug), dir)
		if err != nil {
			return fmt.Errorf("project %s: %w", cfg.Projects[i].Slug, err)
		}
		if repos != nil {
			cfg.Projects[i].Repos = repos
		}
	}
	if len(cfg.Projects) == 1 {
		if dir, ok := derived[cfg.Projects[0].Slug]; ok {
			cfg.WorkDir = dir
		}
	}
	// Every workdir this call just wrote is a fleet project root by
	// construction (CLA-678), so the derived config SAYS so: it is the flag a
	// session's permission scope keys on, and a child that had to infer it
	// would be guessing where a generated file can simply state the fact. Set
	// only when a derived root was actually applied — the flag asserts the
	// layout, so it must not claim one that was never written.
	return nil
}

// managedCheckouts returns a COPY of a project's `repos` map whose path
// OVERRIDES all resolve to the fleet's own clones under the derived project
// root: <project root>/<repo name>. The identity KEYS are kept — they are what
// the project declares, what a task's `repo` field names, and what the
// derivation cloned — and only the local path value is replaced. An empty map
// yields nil, so a config that declares no path overrides keeps its shape.
//
// This is the materialization half of the project-root containment (CLA-678
// review round 2). config.ResolveCheckout prefers an explicit `repos` entry
// over every convention, and DeclaredCheckouts hands every explicit entry to
// the session's permission policy. Without the rewrite, a config carrying
// `repos: {"acme/widgets": "/Users/me/dev/widgets"}` would start the session in
// that developer checkout and grant it, while the fleet clone the derivation
// just made sat unused — the session would read and edit outside the project
// root the layout promises.
//
// An identity whose last segment is empty, "." or ".." names no manageable
// checkout and is refused rather than joined, because ".." would escape the
// project root.
func managedCheckouts(repos map[string]string, projectRoot string) (map[string]string, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(repos))
	for identity := range repos {
		name := basenameOfRepo(identity)
		if name == "" || name == "." || name == ".." {
			return nil, fmt.Errorf("repo identity %q does not name a checkout directory", identity)
		}
		out[identity] = filepath.Join(projectRoot, name)
	}
	return out, nil
}

// materializedBytes is the ONE serializer for a child's generated config: the
// effective config with the two values the child must not derive for itself
// pinned (see the package comment). Both the spawn path and the reconcile-time
// refresh go through it, so "the file an edit produces" and "the file a spawn
// produces" are the same bytes — which is what makes change detection
// byte-exact. encoding/json sorts map keys, and every input here is a struct,
// slice or sorted map, so the same config always serializes to the same bytes.
func (d *Supervisor) materializedBytes(cfg *config.Config, dir string) ([]byte, error) {
	eff := *cfg
	eff.InstanceName = cfg.ResolvedInstanceName(d.hostname)
	eff.StateDir = dir
	data, err := json.MarshalIndent(&eff, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", statedir.MaterializedConfigName, err)
	}
	return data, nil
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

	data, err := d.materializedBytes(cfg, dir)
	if err != nil {
		return "", err
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
