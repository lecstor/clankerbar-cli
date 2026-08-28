package supervisor

// Phase 2b of docs/proposals/daemon-supervisor.md (in the clankerbar repo):
// the supervisor generates each child's effective config into the child's own
// state dir from machine conventions and the environment, and starts the
// child with `run -c` pointing at it. The generated file is a cache —
// regenerated on every reconcile, safe to delete — written to disk so it
// doubles as the offline last-known-good.

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

// The generated config is a REAL config: it loads and validates through the
// existing loader — the exact path `run -c` takes — and carries the machine
// conventions: the phase-2a derived workdir, the identity pinned from the
// source config path, the pinned state dir, and the default plane origin (the
// credential itself lives only in CLANKERBAR_API_KEY, which never lands in a
// file).
func TestMaterializedConfigRoundTripsThroughTheLoader(t *testing.T) {
	root := t.TempDir()
	makeCheckout(t, filepath.Join(root, "widgets"), "acme/widgets")
	// A discovered <workdir>/.mcp.json must land in the materialized file via
	// the documented mcp_config_path default, resolved against the DERIVED
	// workdir.
	mcp := `{"mcpServers": {"clankerbar": {"type": "http", "url": "https://clankerbar.com/mcp/acme", "headers": {"Authorization": "Bearer ${CLANKERBAR_API_KEY}"}}}}`
	if err := os.WriteFile(filepath.Join(root, "widgets", ".mcp.json"), []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "state")
	src := filepath.Join(dir, "daemon.json")
	body := `{"harness": "claude", "state_dir": "` + stateDir + `", "primary_repo": "acme/widgets"}`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	o := testOptions(t, dir)
	o.WorkdirRoot = root
	d := &Supervisor{o: o.withDefaults(), hostname: "testhost"}

	cfg, err := config.Load(src)
	if err != nil {
		t.Fatal(err)
	}
	resolved, stateDir, err := d.resolveInstance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	inst := &Instance{path: src, name: resolved.ResolvedInstanceName(d.hostname), cfg: resolved, stateDir: stateDir}
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
	if loaded.WorkDir != filepath.Join(root, "widgets") {
		t.Errorf("workdir = %q, want the derived %q", loaded.WorkDir, filepath.Join(root, "widgets"))
	}
	if loaded.InstanceName != "testhost/daemon" {
		t.Errorf("instance_name = %q, want the identity resolved from the source config path (testhost/daemon)", loaded.InstanceName)
	}
	if sd, err := loaded.ResolveStateDir(); err != nil || sd != stateDir {
		t.Errorf("ResolveStateDir = %q (%v), want the pinned %q", sd, err, stateDir)
	}
	if loaded.BacklogURL != "https://clankerbar.com" {
		t.Errorf("backlog_url = %q, want the default plane origin", loaded.BacklogURL)
	}
	if loaded.MCPConfigPath != filepath.Join(root, "widgets", ".mcp.json") {
		t.Errorf("mcp_config_path = %q, want the discovered default resolved against the derived workdir", loaded.MCPConfigPath)
	}
}

// A multi-project instance derives one workdir per project, and the generated
// config carries each project's derived workdir — the phase-2a value consumed
// at materialization time.
func TestMaterializedConfigConsumesPerProjectDerivedWorkdirs(t *testing.T) {
	root := t.TempDir()
	makeCheckout(t, filepath.Join(root, "widgets"), "acme/widgets")
	makeCheckout(t, filepath.Join(root, "gadgets"), "acme/gadgets")

	dir := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "state")
	src := filepath.Join(dir, "multi.json")
	body := `{"harness": "claude", "state_dir": "` + stateDir + `", "projects": [
		{"slug": "one", "primary_repo": "acme/widgets"},
		{"slug": "two", "primary_repo": "acme/gadgets"}]}`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	o := testOptions(t, dir)
	o.WorkdirRoot = root
	d := &Supervisor{o: o.withDefaults(), hostname: "testhost"}

	cfg, err := config.Load(src)
	if err != nil {
		t.Fatal(err)
	}
	resolved, stateDir, err := d.resolveInstance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	inst := &Instance{path: src, name: resolved.ResolvedInstanceName(d.hostname), cfg: resolved, stateDir: stateDir}
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
	if got["one"] != filepath.Join(root, "widgets") || got["two"] != filepath.Join(root, "gadgets") {
		t.Fatalf("project workdirs = %v, want one -> %q, two -> %q", got, filepath.Join(root, "widgets"), filepath.Join(root, "gadgets"))
	}
}

// The supervisor starts every child on the GENERATED config: the spawn log
// names the materialized path inside the child's state dir, and the child —
// the fake daemon, whose only reading of its config is state_dir — runs at
// all only because the generated file carried it. Deleting the generated file
// while the child runs is harmless: the child loaded it at startup and the
// supervisor does not touch it until the next spawn.
func TestChildrenSpawnOnTheMaterializedConfig(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	writeInstanceConfig(t, dir, "daemon.json", "sleep", state)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, dir))

	want := filepath.Join(state, statedir.MaterializedConfigName)
	waitFor(t, 5*time.Second, "the child to spawn on the generated config", func() bool {
		return strings.Contains(buf.String(), want)
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
	dir := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	writeInstanceConfig(t, dir, "daemon.json", "crash", state)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, dir))

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