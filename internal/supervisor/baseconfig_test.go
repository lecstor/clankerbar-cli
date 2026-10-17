package supervisor

// CLA-632: the roster owns WHICH projects an instance drives; the base config
// owns HOW this machine reaches them. buildConfig used to REPLACE cfg.Projects
// with bare {Slug, PrimaryRepo} rows, so a roster entry naming projects that
// match the base config's projects[] entries dropped each entry's machine
// wiring — workdir, repos, mcp_config_path(s), pr_base and the rest — and a
// multi-project run on opencode was refused outright for want of the
// per-project mcp_config_paths the README requires ("Multi-project runs need
// one MCP file per project per harness").

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// writeMCPFile writes a harness-shaped MCP config naming /mcp/<slug> — the
// file shape Validate accepts for a project's own or per-harness path.
func writeMCPFile(t *testing.T, slug, flavor string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("%s-%s.json", slug, flavor))
	data := fmt.Sprintf(`{"mcpServers": {"clankerbar": {"type": "http", "url": "https://clankerbar.com/mcp/%s", "headers": {"Authorization": "Bearer ${CLANKERBAR_API_KEY}"}}}}`, slug)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// baseWithProject loads the default machine layer (HOME is isolated by
// materializeOptions) and adds one projects[] entry the roster can match.
func baseWithProject(t *testing.T, p config.Project) *config.Config {
	t.Helper()
	base, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base.Projects = []config.Project{p}
	return base
}

// The core of CLA-632: a roster project whose slug matches a base-config
// projects[] entry keeps that entry's machine-layer fields — and the
// materialized file carries them — with only Slug and PrimaryRepo from the
// roster.
func TestBuildConfigKeepsBaseProjectMachineWiring(t *testing.T) {
	cacheDir := t.TempDir()
	baseWorkDir := t.TempDir()
	mcp := writeMCPFile(t, "acme", "project")
	checkout := filepath.Join(t.TempDir(), "widgets")

	o := materializeOptions(t, cacheDir, "")
	o.BaseCfg = baseWithProject(t, config.Project{
		Slug:             "acme",
		WorkDir:          baseWorkDir,
		MCPConfigPath:    mcp,
		Repos:            map[string]string{"acme/widgets": checkout},
		PRBase:           map[string]string{"acme/widgets": "develop"},
		PrimaryRepo:      "base/legacy",
		AllowUncheckedPR: true,
	})
	d := &Supervisor{o: o, hostname: "testhost", cacheDir: cacheDir}

	entry := RosterEntry{
		Name:         "fleet-one",
		DesiredState: RosterDesiredRunning,
		Placement:    RosterPlacementLocal,
		Projects:     []RosterProject{{Slug: "acme", PrimaryRepo: "plane/repo"}},
	}
	cfg, stateDir, err := d.buildConfig(&entry)
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}

	// The materialized file is what the child actually runs on: pin the
	// wiring in the round-tripped config, not only in memory.
	inst := &Instance{name: entry.Name, entry: &entry, cfg: cfg, stateDir: stateDir}
	path, err := d.materializeConfig(inst)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatalf("generated config does not validate: %v", err)
	}
	if len(loaded.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(loaded.Projects))
	}
	p := loaded.Projects[0]
	if p.Slug != "acme" {
		t.Errorf("slug = %q, want acme (from the roster)", p.Slug)
	}
	if p.PrimaryRepo != "plane/repo" {
		t.Errorf("primary_repo = %q, want plane/repo (from the roster)", p.PrimaryRepo)
	}
	if p.WorkDir != baseWorkDir {
		t.Errorf("workdir = %q, want the base entry's %q", p.WorkDir, baseWorkDir)
	}
	if p.MCPConfigPath != mcp {
		t.Errorf("mcp_config_path = %q, want the base entry's %q", p.MCPConfigPath, mcp)
	}
	if p.Repos["acme/widgets"] != checkout {
		t.Errorf("repos = %v, want the base entry's map with acme/widgets -> %q", p.Repos, checkout)
	}
	if p.PRBase["acme/widgets"] != "develop" {
		t.Errorf("pr_base = %v, want the base entry's map with acme/widgets -> develop", p.PRBase)
	}
	if !p.AllowUncheckedPR {
		t.Error("allow_unchecked_pr = false, want the base entry's true")
	}
}

// A roster entry with no primaryRepo of its own is not an instruction to clear
// the base entry's: the empty value is the config's own "no opinion" marker
// (PrimaryRepoFor falls back), so the base wiring survives.
func TestBuildConfigRosterWithoutPrimaryRepoKeepsBaseWiring(t *testing.T) {
	cacheDir := t.TempDir()
	o := materializeOptions(t, cacheDir, "")
	o.BaseCfg = baseWithProject(t, config.Project{
		Slug:        "acme",
		PrimaryRepo: "base/legacy",
	})
	d := &Supervisor{o: o, hostname: "testhost", cacheDir: cacheDir}

	entry := RosterEntry{
		Name:         "fleet-one",
		DesiredState: RosterDesiredRunning,
		Placement:    RosterPlacementLocal,
		Projects:     []RosterProject{{Slug: "acme"}},
	}
	cfg, _, err := d.buildConfig(&entry)
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	if got := cfg.Projects[0].PrimaryRepo; got != "base/legacy" {
		t.Errorf("primary_repo = %q, want the base entry's base/legacy when the roster names none", got)
	}
}

// With CLANKERBAR_WORKDIR_ROOT stated, derivation overrides WORKDIR ONLY: the
// base entry's repos, mcp_config_path(s), pr_base and the rest survive the
// derived checkout.
func TestBuildConfigWorkdirDerivationOverridesOnlyWorkdir(t *testing.T) {
	root := t.TempDir()
	makeCheckout(t, filepath.Join(root, "widgets"), "acme/widgets")

	cacheDir := t.TempDir()
	baseWorkDir := t.TempDir()
	mcp := writeMCPFile(t, "acme", "project")
	checkout := filepath.Join(t.TempDir(), "elsewhere")

	o := materializeOptions(t, cacheDir, root)
	o.BaseCfg = baseWithProject(t, config.Project{
		Slug:          "acme",
		WorkDir:       baseWorkDir,
		MCPConfigPath: mcp,
		Repos:         map[string]string{"acme/widgets": checkout},
		PRBase:        map[string]string{"acme/widgets": "develop"},
	})
	d := &Supervisor{o: o, hostname: "testhost", cacheDir: cacheDir}

	entry := RosterEntry{
		Name:         "fleet-one",
		DesiredState: RosterDesiredRunning,
		Placement:    RosterPlacementLocal,
		Projects:     []RosterProject{{Slug: "acme", PrimaryRepo: "acme/widgets"}},
	}
	cfg, _, err := d.buildConfig(&entry)
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	p := cfg.Projects[0]
	if want := filepath.Join(root, "widgets"); p.WorkDir != want {
		t.Errorf("workdir = %q, want the derived %q", p.WorkDir, want)
	}
	if p.MCPConfigPath != mcp {
		t.Errorf("mcp_config_path = %q, want the base entry's %q (derivation overrides workdir only)", p.MCPConfigPath, mcp)
	}
	if p.Repos["acme/widgets"] != checkout {
		t.Errorf("repos = %v, want the base entry's map (derivation overrides workdir only)", p.Repos)
	}
	if p.PRBase["acme/widgets"] != "develop" {
		t.Errorf("pr_base = %v, want the base entry's map (derivation overrides workdir only)", p.PRBase)
	}
}

// A roster project with no matching base entry behaves exactly as before:
// bare {Slug, PrimaryRepo}, no machine wiring invented.
func TestBuildConfigRosterProjectWithoutBaseEntryUnchanged(t *testing.T) {
	cacheDir := t.TempDir()
	o := materializeOptions(t, cacheDir, "")
	// A base entry exists, for a DIFFERENT slug: matching is by slug, so it
	// must not leak into the roster's project.
	o.BaseCfg = baseWithProject(t, config.Project{
		Slug:          "other",
		WorkDir:       t.TempDir(),
		MCPConfigPath: writeMCPFile(t, "other", "project"),
		Repos:         map[string]string{"other/thing": t.TempDir()},
	})
	d := &Supervisor{o: o, hostname: "testhost", cacheDir: cacheDir}

	entry := RosterEntry{
		Name:         "fleet-one",
		DesiredState: RosterDesiredRunning,
		Placement:    RosterPlacementLocal,
		Projects:     []RosterProject{{Slug: "acme", PrimaryRepo: "acme/widgets"}},
	}
	cfg, _, err := d.buildConfig(&entry)
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("projects = %d, want 1 (the roster owns the list)", len(cfg.Projects))
	}
	p := cfg.Projects[0]
	if p.Slug != "acme" || p.PrimaryRepo != "acme/widgets" {
		t.Errorf("project = {%q, %q}, want {acme, acme/widgets} from the roster", p.Slug, p.PrimaryRepo)
	}
	if p.WorkDir != "" || p.MCPConfigPath != "" || len(p.Repos) != 0 || len(p.PRBase) != 0 || len(p.MCPConfigPaths) != 0 {
		t.Errorf("project carries machine wiring with no base entry to source it from: %+v", p)
	}
}

// The inherited MCPConfigPaths map must be a COPY: Validate normalizes each
// per-harness path IN PLACE, and Options.BaseCfg may be shared across builds
// (no source file to re-read), so writing through would let one build resolve
// paths into the base the next build — possibly under another instance's
// derived workdir — reads.
func TestBuildConfigDoesNotWriteThroughToTheSharedBaseProject(t *testing.T) {
	cacheDir := t.TempDir()
	workdir := t.TempDir()
	o := materializeOptions(t, cacheDir, "")
	o.BaseCfg = baseWithProject(t, config.Project{
		Slug:    "acme",
		WorkDir: workdir,
		MCPConfigPaths: map[string]string{
			"opencode": "mcp/oc.json",
			"claude":   "mcp/cl.json",
		},
	})
	d := &Supervisor{o: o, hostname: "testhost", cacheDir: cacheDir}

	entry := RosterEntry{
		Name:         "fleet-one",
		DesiredState: RosterDesiredRunning,
		Placement:    RosterPlacementLocal,
		Projects:     []RosterProject{{Slug: "acme"}},
	}
	cfg, _, err := d.buildConfig(&entry)
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	if got := cfg.Projects[0].MCPConfigPaths["opencode"]; got != filepath.Join(workdir, "mcp/oc.json") {
		t.Errorf("resolved mcp_config_paths.opencode = %q, want %q", got, filepath.Join(workdir, "mcp/oc.json"))
	}
	if got := o.BaseCfg.Projects[0].MCPConfigPaths["opencode"]; got != "mcp/oc.json" {
		t.Errorf("base config was written through: mcp_config_paths.opencode = %q, want the declared \"mcp/oc.json\"", got)
	}
}

// The operator's shape (CLA-633): a three-project roster entry over a base
// config with implement on opencode, review on claude and one MCP file per
// project per harness. Before CLA-632 the replacement projects carried no
// mcp_config_paths, so Validate refused the whole entry — the operator's fleet
// could not run under the supervisor at all.
func TestBuildConfigOperatorThreeProjectShapeValidates(t *testing.T) {
	cacheDir := t.TempDir()
	o := materializeOptions(t, cacheDir, "")
	base, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base.Harness = "opencode"
	base.Phases = []config.Phase{
		{Name: "implement", Harness: "opencode"},
		{Name: "review", Harness: "claude"},
	}
	base.Harnesses = map[string]config.HarnessConfig{
		"claude": {Model: "opus"},
	}

	slugs := []string{"clankerbar", "ezyapp", "makespdf"}
	var projects []config.Project
	for _, slug := range slugs {
		projects = append(projects, config.Project{
			Slug:    slug,
			WorkDir: filepath.Join(t.TempDir(), slug),
			MCPConfigPaths: map[string]string{
				"opencode": writeMCPFile(t, slug, "opencode"),
				"claude":   writeMCPFile(t, slug, "claude"),
			},
			PrimaryRepo: "lecstor/" + slug,
		})
	}
	base.Projects = projects
	o.BaseCfg = base
	d := &Supervisor{o: o, hostname: "testhost", cacheDir: cacheDir}

	entry := RosterEntry{
		Name:         "clanker1",
		DesiredState: RosterDesiredRunning,
		Placement:    RosterPlacementLocal,
		Projects: []RosterProject{
			{Slug: "clankerbar", PrimaryRepo: "lecstor/clankerbar"},
			{Slug: "ezyapp", PrimaryRepo: "lecstor/ezyapp"},
			{Slug: "makespdf", PrimaryRepo: "lecstor/makespdf"},
		},
	}
	cfg, stateDir, err := d.buildConfig(&entry)
	if err != nil {
		t.Fatalf("buildConfig refused the operator-shaped entry: %v", err)
	}
	if len(cfg.Projects) != 3 {
		t.Fatalf("projects = %d, want 3", len(cfg.Projects))
	}
	for _, p := range cfg.Projects {
		for harness := range map[string]bool{"opencode": true, "claude": true} {
			if p.MCPConfigPaths[harness] == "" {
				t.Errorf("project %q: mcp_config_paths.%s missing — the review-on-claude phase would be refused", p.Slug, harness)
			}
		}
	}

	// The generated file is the child's real config: it must round-trip.
	inst := &Instance{name: entry.Name, entry: &entry, cfg: cfg, stateDir: stateDir}
	path, err := d.materializeConfig(inst)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatalf("generated config does not validate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, statedir.MaterializedConfigName)); err != nil {
		t.Errorf("materialized config missing: %v", err)
	}
}
