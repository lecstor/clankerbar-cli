package supervisor

// Phase 2b of docs/proposals/daemon-supervisor.md (in the clankerbar repo):
// the supervisor generates each child's effective config into the child's own
// state dir from machine conventions and the roster entry, and starts the
// child with `run -c` pointing at it. The generated file is a cache —
// regenerated on every reconcile, safe to delete — written to disk so it
// doubles as the offline last-known-good. In phase 3b the entry drives the
// build: the projects come from the roster row, identity is the entry's name,
// and the state dir is derived from the same name.

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// materializeOptions builds the options the direct (non-Supervise) materialize
// tests drive buildConfig with. HOME is isolated so the machine-layer config
// discovery in buildConfig returns defaults instead of the operator's real
// config.
func materializeOptions(t *testing.T, cacheDir, workdirRoot string) Options {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	o := Options{RosterCacheDir: cacheDir, WorkdirRoot: workdirRoot}
	return o.withDefaults()
}

// The generated config is a REAL config: it loads and validates through the
// existing loader — the exact path `run -c` takes — and carries the machine
// conventions: the phase-2a derived workdir, the identity pinned from the
// roster entry's name, the pinned state dir (derived from the same name), and
// the default plane origin (the credential itself lives only in
// CLANKERBAR_API_KEY, which never lands in a file).
func TestMaterializedConfigRoundTripsThroughTheLoader(t *testing.T) {
	root := t.TempDir()
	makeCheckout(t, filepath.Join(root, "acme", "widgets"), "acme/widgets")
	// A discovered <workdir>/.mcp.json must land in the materialized file via
	// the documented mcp_config_path default, resolved against the DERIVED
	// workdir — which is the PROJECT ROOT, the directory a session's reach is
	// scoped to (CLA-678).
	mcp := `{"mcpServers": {"clankerbar": {"type": "http", "url": "https://clankerbar.com/mcp/acme", "headers": {"Authorization": "Bearer ${CLANKERBAR_API_KEY}"}}}}`
	if err := os.WriteFile(filepath.Join(root, "acme", ".mcp.json"), []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}

	cacheDir := t.TempDir()
	entry := runEntry("daemon-one", "acme/widgets")

	o := materializeOptions(t, cacheDir, root)
	d := &Supervisor{o: o, hostname: "testhost", cacheDir: cacheDir}

	cfg, stateDir, err := d.buildConfig(&entry)
	if err != nil {
		t.Fatal(err)
	}
	inst := &Instance{name: entry.Name, entry: &entry, cfg: cfg, stateDir: stateDir}
	path, err := d.materializeConfig(inst)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(stateDir, statedir.MaterializedConfigName); path != want {
		t.Fatalf("materialized path = %q, want %q", path, want)
	}

	// The round trip: the generated file loads and validates through the
	// existing loader.
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatalf("generated config does not validate: %v", err)
	}

	// The machine conventions are IN the file.
	if loaded.WorkDir != filepath.Join(root, "acme") {
		t.Errorf("workdir = %q, want the derived project root %q", loaded.WorkDir, filepath.Join(root, "acme"))
	}
	if len(loaded.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(loaded.Projects))
	}
	if loaded.Projects[0].WorkDir != filepath.Join(root, "acme") {
		t.Errorf("projects[0].workdir = %q, want the derived project root %q", loaded.Projects[0].WorkDir, filepath.Join(root, "acme"))
	}
	if !loaded.ProjectRoot {
		// The scope flag has to survive the round trip: it is what the DRIVER
		// reads to widen a session's reach to the whole project (CLA-678), and
		// it is written here precisely so the child never has to infer the
		// layout from the filesystem.
		t.Error("project_root = false, want true — the derivation's layout must be stated in the generated config")
	}
	if loaded.InstanceName != "daemon-one" {
		t.Errorf("instance_name = %q, want the roster entry's name (daemon-one)", loaded.InstanceName)
	}
	if sd, err := loaded.ResolveStateDir(); err != nil || sd != stateDir {
		t.Errorf("ResolveStateDir = %q (%v), want the pinned %q", sd, err, stateDir)
	}
	if loaded.BacklogURL != "https://clankerbar.com" {
		t.Errorf("backlog_url = %q, want the default plane origin", loaded.BacklogURL)
	}
	if loaded.MCPConfigPath != filepath.Join(root, "acme", ".mcp.json") {
		t.Errorf("mcp_config_path = %q, want the discovered default resolved against the derived workdir", loaded.MCPConfigPath)
	}
}

// TestMaterializedConfigWithoutAPanelRoundTrips (CLA-639 fix pass 3): a
// supervised daemon's child config is materializeConfig's marshal, and before
// the fix that marshal wrote `"review_panel": null` for a config with no panel.
// Load treated the key's presence as "set", so the child refused the standard
// [implement, review, fix] shape with "review_panel is empty" and the daemon
// never started. The whole supervised path is exercised here: Load -> Validate
// -> materialize -> Load -> Validate.
func TestMaterializedConfigWithoutAPanelRoundTrips(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workdir := t.TempDir()
	src := filepath.Join(t.TempDir(), "source.json")
	body := `{
		"harness": "claude",
		"workdir": "` + workdir + `",
		"phases": [{"name": "implement"}, {"name": "review"}, {"name": "fix"}]
	}`
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	cfg, err := config.Load(src)
	if err != nil {
		t.Fatalf("Load(source): %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("source does not validate: %v", err)
	}

	cacheDir := t.TempDir()
	stateDir := t.TempDir()
	d := &Supervisor{o: materializeOptions(t, cacheDir, ""), hostname: "testhost", cacheDir: cacheDir}
	inst := &Instance{name: "no-panel", cfg: cfg, stateDir: stateDir}
	path, err := d.materializeConfig(inst)
	if err != nil {
		t.Fatalf("materializeConfig: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("the materialized config does not load: %v", err)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatalf("the materialized config does not validate, so a supervised daemon with no panel would not start: %v", err)
	}
	if len(loaded.Phases) != 3 {
		t.Fatalf("materialized phases = %d, want 3", len(loaded.Phases))
	}
	if got := loaded.EffectivePanel(); len(got) != 0 {
		t.Fatalf("the materialized config invented a panel of %d members", len(got))
	}
}

// A multi-project instance derives one workdir per project, and the generated
// config carries each project's derived workdir — the phase-2a value consumed
// at materialization time. The projects come from the roster entry.
func TestMaterializedConfigConsumesPerProjectDerivedWorkdirs(t *testing.T) {
	root := t.TempDir()
	makeCheckout(t, filepath.Join(root, "one", "widgets"), "acme/widgets")
	makeCheckout(t, filepath.Join(root, "two", "gadgets"), "acme/gadgets")

	cacheDir := t.TempDir()
	entry := RosterEntry{
		Name:         "multi",
		DesiredState: RosterDesiredRunning,
		Placement:    RosterPlacementLocal,
		Projects: []RosterProject{
			{Slug: "one", PrimaryRepo: "acme/widgets"},
			{Slug: "two", PrimaryRepo: "acme/gadgets"},
		},
	}

	o := materializeOptions(t, cacheDir, root)
	d := &Supervisor{o: o, hostname: "testhost", cacheDir: cacheDir}

	cfg, stateDir, err := d.buildConfig(&entry)
	if err != nil {
		t.Fatal(err)
	}
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
	got := map[string]string{}
	for _, p := range loaded.Projects {
		got[p.Slug] = p.WorkDir
	}
	if got["one"] != filepath.Join(root, "one") || got["two"] != filepath.Join(root, "two") {
		t.Fatalf("project workdirs = %v, want one -> %q, two -> %q", got, filepath.Join(root, "one"), filepath.Join(root, "two"))
	}
}

// The supervisor starts every child on the GENERATED config: the spawn log
// names the materialized path inside the child's state dir (derived from the
// entry name), and the child — the fake daemon, whose only reading of its
// config is state_dir — runs at all only because the generated file carried
// it. Deleting the generated file while the child runs is harmless: the child
// loaded it at startup and the supervisor does not touch it until the next
// spawn.
func TestChildrenSpawnOnTheMaterializedConfig(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", "")})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	state := entryStateDir(cacheDir, "daemon-one")
	want := filepath.Join(state, statedir.MaterializedConfigName)
	// Wait for the child to both spawn on the generated config AND write its
	// first iteration log: the spawn log line lands the moment cmd.Start()
	// returns, but the child is a separate process that takes a beat to exec,
	// read the config and drop its iteration log. Polling only the log line
	// raced that window - the assertion below would read 0 spawns for a child
	// that was already writing. The two conditions together are the event.
	waitFor(t, 5*time.Second, "the child to spawn on the generated config and write its first iteration log", func() bool {
		return strings.Contains(buf.String(), want) && countRuns(t, state) >= 1
	})
	if got := countRuns(t, state); got != 1 {
		t.Fatalf("spawns = %d, want 1", got)
	}

	// Deleting the generated config must not disturb the running child.
	if err := os.Remove(want); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := countRuns(t, state); got != 1 {
		t.Fatalf("spawns after deleting the generated config = %d, want 1 — deletion must not disturb the running child", got)
	}

	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
}

// The generated config is a CACHE: deleting it is harmless, and the next
// reconcile (a respawn) regenerates it. A child that cannot start because its
// generated config was deleted is exactly the shape the supervisor's restart
// loop exists for — the regeneration happens BEFORE the respawn, so the
// respawned child starts on a fresh file.
func TestMaterializedConfigIsACache(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "crash")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", "")})
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	state := entryStateDir(cacheDir, "daemon-one")
	path := filepath.Join(state, statedir.MaterializedConfigName)
	waitFor(t, 5*time.Second, "the generated config to exist", func() bool {
		_, err := os.Lstat(path)
		return err == nil
	})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "a later reconcile to regenerate it", func() bool {
		_, err := os.Lstat(path)
		return err == nil
	})
	if _, err := config.Load(path); err != nil {
		t.Fatalf("the regenerated config does not load: %v", err)
	}

	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
}
