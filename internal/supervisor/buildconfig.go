package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// buildConfig materializes one roster entry into an effective config: the
// machine-layer base (the operator's local config), with the entry's project
// list, its one permitted policy override (`harness` and the per-harness
// block's model/models half), and the pinned identity and state dir written
// over it — then the same resolution `run` performs: the phase-2a workdir
// derivation, Validate, the phase-2c permission-policy gate, and the state-dir
// resolution.
func (d *Supervisor) buildConfig(e *RosterEntry) (*config.Config, string, error) {
	if e == nil {
		// An adopted child has no roster row until the first successful fetch;
		// there is no configuration to build it from yet.
		return nil, "", errors.New("no roster entry yet - the plane has not been reached since this instance was adopted")
	}
	base := d.o.BaseCfg
	if base == nil {
		var err error
		base, err = config.Load("")
		if err != nil {
			return nil, "", err
		}
	} else if src := base.Source(); src != "" {
		// The machine layer is the operator's local config, and edits to it
		// apply at the next reconcile exactly as source-file edits did in
		// phase 2: re-read it on every build. A re-read that fails (a corrupt
		// edit, a deleted file) keeps the last good base — the same fallback
		// posture as every other failed re-read.
		if re, err := config.Load(src); err == nil {
			base = re
		} else {
			log.Printf("re-reading the supervisor's local config (%s) failed (%v) - keeping the last good one", src, err)
		}
	}
	cfg := base.Clone()
	// The roster owns WHICH projects an instance drives; the base config owns
	// HOW this machine reaches them (the machine-layer rule). A roster project
	// whose slug matches a `projects[]` entry in the base config starts from
	// that entry — its workdir, repos map, mcp_config_path(s), pr_base and
	// every other per-project key — with Slug and PrimaryRepo taken from the
	// roster. A roster project with no base entry is built from the roster
	// alone, exactly as before, and derivation (resolveInstance) still
	// overrides WORKDIR only. CLA-632: the replacement used to drop the
	// machine wiring, so a multi-project run whose phases span harnesses was
	// refused outright for want of the per-project mcp_config_paths.
	cfg.InstanceName = e.Name
	cfg.StateDir = rosterStateDir(d.cacheDir, e.Name)
	cfg.Projects = make([]config.Project, 0, len(e.Projects))
	for _, p := range e.Projects {
		slug := strings.TrimSpace(p.Slug)
		// No match yields the zero Project: the roster alone, as before.
		proj, _ := baseProjectFor(base, slug)
		proj.Slug = slug
		// The roster's primary repo is the plane's word and wins the workdir
		// derivation; an absent one is the config's own "no opinion" (see
		// PrimaryRepoFor), so the base entry's primary_repo survives it rather
		// than being cleared by omission.
		if primary := strings.TrimSpace(p.PrimaryRepo); primary != "" {
			proj.PrimaryRepo = primary
		}
		cfg.Projects = append(cfg.Projects, proj)
	}
	// Decision 1: harness and its per-harness block are the ONLY per-instance
	// policy overrides. checkEntry has already refused every other key; what
	// lands here is exactly the two allowed ones.
	//
	// The harness override is parsed before it is applied because re-pointing
	// the top-level rides the CLA-475 swap guard: over a machine layer that
	// wires the old harness run-wide, the new harness would silently inherit
	// that dialect through SessionFor/ResolveMCPConfig — the same re-homing a
	// stored run-config swap performs, through this door instead. The entry's
	// harnesses block carries only the policy half, exactly like the stored
	// document, so the synthetic document captures the override exactly, and
	// a refusal keeps the last-known-good via the caller's loud path.
	var overrideBlocks map[string]config.RunConfigHarnessBlock
	if raw, ok := e.Overrides["harnesses"]; ok {
		if err := json.Unmarshal(raw, &overrideBlocks); err != nil {
			return nil, "", fmt.Errorf("entry %q: harnesses override is not a per-harness block: %v", e.Name, err)
		}
	}
	if raw, ok := e.Overrides["harness"]; ok {
		var h string
		if err := json.Unmarshal(raw, &h); err != nil {
			return nil, "", fmt.Errorf("entry %q: harness override is not a string: %v", e.Name, err)
		}
		if err := cfg.CheckHarnessSwap(&config.RunConfigDoc{Harness: strings.TrimSpace(h), Harnesses: overrideBlocks}); err != nil {
			return nil, "", fmt.Errorf("entry %q: %v", e.Name, err)
		}
		cfg.Harness = strings.TrimSpace(h)
	}
	if len(overrideBlocks) > 0 {
		// The loop below writes into cfg.Harnesses, which Clone keeps nil when
		// the machine layer is the common single-harness config (no `harnesses:`
		// key, so the map never exists to copy). A roster entry carrying the
		// permitted `harnesses` override over that shape must allocate rather
		// than panic with "assignment to entry in nil map" - the same root
		// cause ApplyRunConfig guards (CLA-474).
		if len(overrideBlocks) > 0 && cfg.Harnesses == nil {
			cfg.Harnesses = make(map[string]config.HarnessConfig, len(overrideBlocks))
		}
		for name, b := range overrideBlocks {
			hc := cfg.Harnesses[name]
			// An explicit empty model still CLEARS the inherited one, exactly as
			// the plain-string field did before the route type existed.
			if m := strings.TrimSpace(b.Model); m != "" {
				hc.Model = config.ModelRoutes{m}
			} else {
				hc.Model = nil
			}
			if b.Models != nil {
				models := make(map[string]config.ModelRoutes, len(b.Models))
				for bucket, alias := range b.Models {
					models[bucket] = config.ModelRoutes{alias}
				}
				hc.Models = models
			}
			cfg.Harnesses[name] = hc
		}
	}
	return d.resolveInstance(cfg)
}

// baseProjectFor returns the base config's projects[] entry for one slug — the
// machine wiring a roster project inherits when it names the same project.
// The MCPConfigPaths map is cloned because Validate normalizes it IN PLACE
// (projects[i].mcp_config_paths.<harness> is rewritten with the resolved
// path), and the base config may be shared across builds (Options.BaseCfg
// without a source); the other map fields are treated read-only on the copy,
// exactly as Clone's contract does.
func baseProjectFor(base *config.Config, slug string) (config.Project, bool) {
	for _, p := range base.Projects {
		if p.Slug != slug {
			continue
		}
		if p.MCPConfigPaths != nil {
			paths := make(map[string]string, len(p.MCPConfigPaths))
			for k, v := range p.MCPConfigPaths {
				paths[k] = v
			}
			p.MCPConfigPaths = paths
		}
		return p, true
	}
	return config.Project{}, false
}

// loadMaterialized loads an instance's last-known-good materialized config
// from its state dir — the file phase 2b wrote and the child last ran on. It
// is the cold-start fallback: a config the roster entry can no longer produce
// (the workdir moved while the plane was unreachable) still has its last
// materialization on disk, and the daemon runs on that rather than not at all.
func (d *Supervisor) loadMaterialized(inst *Instance) *config.Config {
	inst.mu.Lock()
	dir := inst.stateDir
	inst.mu.Unlock()
	if dir == "" {
		return nil
	}
	cfg, err := config.Load(filepath.Join(dir, statedir.MaterializedConfigName))
	if err != nil {
		return nil
	}
	if err := cfg.Validate(); err != nil {
		return nil
	}
	return cfg
}

// resolveInstance resolves the machine conventions one instance runs under,
// in the order the materialized config is built from them: the phase-2a
// workdir derivation (when a machine root is stated) FIRST, so validation
// resolves every relative path and the mcp discovery against the DERIVED
// workdir rather than against wherever the supervisor happened to start; then
// the same Load + Validate + ResolveStateDir the daemon itself performs. The
// returned config is the one the materialized file is built from.
//
// The permission-policy gate (phase 2c) runs after Validate, on the RESOLVED
// settings_path: an absent policy file refuses the instance, so it is never
// spawned. Like the workdir gate, there is no fallback branch — a daemon whose
// policy cannot be verified is not started at all.
//
// The derivation applies only when WorkdirRoot is set — with no machine root
// stated, the base config's own workdir governs and the config validates
// exactly as `run` would validate it.
func (d *Supervisor) resolveInstance(cfg *config.Config) (*config.Config, string, error) {
	if d.o.WorkdirRoot != "" {
		derived, err := deriveInstanceWorkdirs(d.baseCtx(), cfg, d.o.WorkdirRoot, d.o.CloneURLBase)
		if err != nil {
			return nil, "", err
		}
		if err := applyDerivedWorkdirs(cfg, derived); err != nil {
			return nil, "", err
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, "", err
	}
	if err := checkPermissionPolicy(cfg); err != nil {
		return nil, "", err
	}
	stateDir, err := cfg.ResolveStateDir()
	if err != nil {
		return nil, "", err
	}
	return cfg, stateDir, nil
}
