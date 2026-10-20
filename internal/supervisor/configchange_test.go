package supervisor

// CLA-724: a base-config (or roster) edit must reach the children that are
// already running, not only the next spawn. Each reconcile rebuilds a running
// child's effective config the way the spawn path does and compares its bytes
// with materialized.json on disk; a difference is rewritten and one graceful
// RESTART is queued, so the daemon picks the new file up at its iteration
// boundary.
//
// The tests here drive that path two ways:
//
//   - a unit-level fixture (materializedInstance/runningFleet) builds the config and
//     materializes it exactly as a spawn would, then marks the instance alive
//     with a never-started *exec.Cmd — reconcile's liveness test reads only
//     "cmd != nil && !exited" — so a marker can be asserted before any process
//     eats it;
//   - one end-to-end test runs the real Supervise loop against the fake daemon
//     and the fake plane, edits the machine layer on disk, and watches the
//     child actually restart once on the new file.

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/loop"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// writeBaseFile writes one machine-layer config into a temp file and returns
// its path. A file (rather than an in-memory Options.BaseCfg) is what gives the
// supervisor a SOURCE to re-read on every build, which is also the only
// race-free way to change what a running supervisor reads.
func writeBaseFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write base config: %v", err)
	}
	return path
}

// materializedInstance builds a supervisor on basePath plus one instance whose
// config has been built and materialized the way a spawn would. cmd is set but
// never started, so the instance reads as running to reconcile/refresh without
// a real child consuming markers between assertions.
func materializedInstance(t *testing.T, basePath string, entry RosterEntry) (*Supervisor, *Instance, string) {
	t.Helper()
	d, insts := runningFleet(t, basePath, entry)
	return d, insts[0], entryStateDir(d.cacheDir, entry.Name)
}

// runningFleet is materializedInstance for several entries on ONE supervisor,
// sharing the machine layer — the shape the "only the changed child" test
// needs.
func runningFleet(t *testing.T, basePath string, entries ...RosterEntry) (*Supervisor, []*Instance) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cacheDir := t.TempDir()
	base, err := config.Load(basePath)
	if err != nil {
		t.Fatalf("load base config %s: %v", basePath, err)
	}
	d := &Supervisor{
		o:        Options{RosterCacheDir: cacheDir, BaseCfg: base}.withDefaults(),
		hostname: "testhost",
		cacheDir: cacheDir,
	}
	insts := make([]*Instance, 0, len(entries))
	for i := range entries {
		inst := &Instance{
			name:    entries[i].Name,
			entry:   &entries[i],
			desired: entries[i].DesiredState,
			cmd:     &exec.Cmd{},
		}
		d.refreshInstance(inst)
		inst.mu.Lock()
		built := inst.cfg
		inst.mu.Unlock()
		if built == nil {
			t.Fatalf("instance %q has no config after its first build", entries[i].Name)
		}
		if _, err := d.materializeConfig(inst); err != nil {
			t.Fatalf("materialize %q: %v", entries[i].Name, err)
		}
		d.instances = append(d.instances, inst)
		insts = append(insts, inst)
	}
	return d, insts
}

// reconcileStep is what one poll does to one instance's config: rebuild it from
// the current machine layer and roster entry, then apply the result to the
// running child.
func reconcileStep(d *Supervisor, inst *Instance) {
	d.refreshInstance(inst)
	d.refreshMaterialized(inst)
}

// materializedPath is the generated config's path in one instance's state dir.
func materializedPath(dir string) string {
	return filepath.Join(dir, statedir.MaterializedConfigName)
}

// readFileBytes reads a file the test expects to exist.
func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// An unchanged machine layer across several reconciles must write NOTHING: no
// RESTART (a marker here is a restart loop) and no change to the generated
// bytes. This is the determinism pin the whole mechanism rests on — the same
// config must serialize identically, or every poll would look like an edit.
func TestReconcileConfigEditUnchangedFleetWritesNothing(t *testing.T) {
	basePath := writeBaseFile(t, `{"model": "sonnet"}`)
	d, inst, dir := materializedInstance(t, basePath, runEntry("daemon-one", ""))
	path := materializedPath(dir)
	before := readFileBytes(t, path)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	for i := 0; i < 3; i++ {
		reconcileStep(d, inst)
	}

	if after := readFileBytes(t, path); !bytes.Equal(before, after) {
		t.Fatalf("materialized.json changed on an unchanged config:\nbefore: %s\nafter:  %s", before, after)
	}
	if markerPresent(dir, loop.MarkerRestart) {
		t.Fatal("an unchanged config wrote a RESTART - the fleet would restart-loop")
	}
	if strings.Contains(buf.String(), "effective config changed") {
		t.Fatalf("an unchanged config logged a change:\n%s", buf.String())
	}
}

// Materializing the same config twice produces byte-identical files. Pinned
// separately from the reconcile test because it is the property, not the
// consequence: if the serializer were ever order- or time-dependent, every
// reconcile would rewrite and restart every child.
func TestMaterializedBytesAreDeterministic(t *testing.T) {
	basePath := writeBaseFile(t, `{"model": "sonnet", "models": {"strong": "opus", "cheap": "haiku"}}`)
	d, inst, dir := materializedInstance(t, basePath, runEntry("daemon-one", ""))
	path := materializedPath(dir)
	first := readFileBytes(t, path)
	if _, err := d.materializeConfig(inst); err != nil {
		t.Fatalf("materialize again: %v", err)
	}
	second := readFileBytes(t, path)
	if !bytes.Equal(first, second) {
		t.Fatalf("materializing the same config twice produced different bytes:\nfirst:  %s\nsecond: %s", first, second)
	}
	d.refreshMaterialized(inst)
	if markerPresent(dir, loop.MarkerRestart) {
		t.Fatal("a byte-identical rewrite queued a RESTART")
	}
}

// A build failure falls back to the config loaded from materialized.json. That
// reloaded config must serialize back to the SAME bytes: otherwise every poll
// through a failed build would look like an edit and restart the fleet on a
// config that never changed.
func TestReloadedMaterializedConfigSerializesIdentically(t *testing.T) {
	basePath := writeBaseFile(t, `{"model": "sonnet"}`)
	d, _, dir := materializedInstance(t, basePath, runEntry("daemon-one", ""))
	path := materializedPath(dir)
	before := readFileBytes(t, path)

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("the generated config does not load: %v", err)
	}
	if err := reloaded.Validate(); err != nil {
		t.Fatalf("the generated config does not validate: %v", err)
	}
	again, err := d.materializedBytes(reloaded, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, again) {
		t.Fatalf("a reloaded materialized config serializes differently, so the fallback would queue a restart with no edit:\nfile:      %s\nreserialized: %s", before, again)
	}
}

// A base-config edit that changes the effective config rewrites the generated
// file and queues exactly ONE graceful RESTART. The next reconcile sees the
// bytes it just wrote and writes no second marker.
func TestReconcileConfigEditQueuesOneRestart(t *testing.T) {
	basePath := writeBaseFile(t, `{"model": "sonnet"}`)
	d, inst, dir := materializedInstance(t, basePath, runEntry("daemon-one", ""))
	path := materializedPath(dir)
	before := readFileBytes(t, path)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	if err := os.WriteFile(basePath, []byte(`{"model": "opus"}`), 0o600); err != nil {
		t.Fatalf("edit the base config: %v", err)
	}
	reconcileStep(d, inst)

	after := readFileBytes(t, path)
	if bytes.Equal(before, after) {
		t.Fatal("the edit did not reach the generated config")
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("the rewritten config does not load: %v", err)
	}
	if loaded.Model != "opus" {
		t.Errorf("materialized model = %q, want the edited opus", loaded.Model)
	}

	marker := filepath.Join(dir, loop.MarkerRestart)
	if !markerPresent(dir, loop.MarkerRestart) {
		t.Fatal("no RESTART was queued for the changed child")
	}
	first := readFileBytes(t, marker)
	// One line per child, naming it and saying a restart at the iteration
	// boundary was queued.
	logText := buf.String()
	if !strings.Contains(logText, "daemon-one: effective config changed") ||
		!strings.Contains(logText, "queued a restart at its iteration boundary") {
		t.Fatalf("the queued restart was not logged naming the child:\n%s", logText)
	}

	// A second reconcile: the bytes now match, so nothing more is written.
	reconcileStep(d, inst)
	if got := readFileBytes(t, path); !bytes.Equal(got, after) {
		t.Fatal("a second reconcile rewrote an already-current config")
	}
	if got := readFileBytes(t, marker); !bytes.Equal(got, first) {
		t.Fatal("a second reconcile wrote a SECOND restart marker")
	}
}

// A restart already pending from another source (a roll, a ctl) is not stacked
// on: the file is still rewritten — so the pending restart re-execs onto the
// edit — but the marker that was there is left exactly as it was.
func TestReconcileConfigEditDoesNotStackOnAPendingRestart(t *testing.T) {
	basePath := writeBaseFile(t, `{"model": "sonnet"}`)
	d, inst, dir := materializedInstance(t, basePath, runEntry("daemon-one", ""))
	path := materializedPath(dir)
	before := readFileBytes(t, path)

	marker := filepath.Join(dir, loop.MarkerRestart)
	if err := os.WriteFile(marker, []byte("ctl\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(basePath, []byte(`{"model": "opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reconcileStep(d, inst)

	if after := readFileBytes(t, path); bytes.Equal(before, after) {
		t.Fatal("the edit did not reach the generated config while a restart was pending")
	}
	if got := readFileBytes(t, marker); string(got) != "ctl\n" {
		t.Fatalf("the pending RESTART was replaced (%q) instead of left alone", got)
	}
}

// A base edit that only changes ONE child's effective config marks only that
// child: the other child's file is untouched and gets no marker.
func TestReconcileConfigEditMarksOnlyTheChangedChild(t *testing.T) {
	acmeWork := t.TempDir()
	betaWork := t.TempDir()
	basePath := writeBaseFile(t, fmt.Sprintf(
		`{"projects": [{"slug": "acme", "workdir": %q}, {"slug": "beta", "workdir": %q}]}`,
		acmeWork, betaWork))

	acmeEntry := RosterEntry{Name: "acme-one", DesiredState: RosterDesiredRunning, Placement: RosterPlacementLocal,
		Projects: []RosterProject{{Slug: "acme"}}}
	betaEntry := RosterEntry{Name: "beta-one", DesiredState: RosterDesiredRunning, Placement: RosterPlacementLocal,
		Projects: []RosterProject{{Slug: "beta"}}}
	d, insts := runningFleet(t, basePath, acmeEntry, betaEntry)

	acmeDir := entryStateDir(d.cacheDir, "acme-one")
	betaDir := entryStateDir(d.cacheDir, "beta-one")
	acmeBefore := readFileBytes(t, materializedPath(acmeDir))
	betaBefore := readFileBytes(t, materializedPath(betaDir))

	// Move only the acme project's workdir.
	moved := t.TempDir()
	if err := os.WriteFile(basePath, []byte(fmt.Sprintf(
		`{"projects": [{"slug": "acme", "workdir": %q}, {"slug": "beta", "workdir": %q}]}`,
		moved, betaWork)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, inst := range insts {
		reconcileStep(d, inst)
	}

	if after := readFileBytes(t, materializedPath(acmeDir)); bytes.Equal(acmeBefore, after) {
		t.Error("the edited project's child did not get the new config")
	}
	if !markerPresent(acmeDir, loop.MarkerRestart) {
		t.Error("the edited project's child got no RESTART")
	}
	if after := readFileBytes(t, materializedPath(betaDir)); !bytes.Equal(betaBefore, after) {
		t.Error("an unedited child's generated config was rewritten")
	}
	if markerPresent(betaDir, loop.MarkerRestart) {
		t.Error("an unedited child got a RESTART")
	}
}

// A roster change is a config change too: the plane editing an entry (projects,
// a permitted override) goes through the same rewrite + restart path as a base
// config file edit.
func TestReconcileConfigEditAppliesARosterChange(t *testing.T) {
	basePath := writeBaseFile(t, `{"model": "sonnet"}`)
	d, inst, dir := materializedInstance(t, basePath, runEntry("daemon-one", ""))
	before := readFileBytes(t, materializedPath(dir))

	// The console changed the entry to a two-project instance.
	edited := runEntry("daemon-one", "")
	edited.Projects = []RosterProject{{Slug: "acme"}, {Slug: "beta"}}
	inst.entry = &edited
	reconcileStep(d, inst)

	if after := readFileBytes(t, materializedPath(dir)); bytes.Equal(before, after) {
		t.Fatal("a roster change did not reach the running child's generated config")
	}
	if !markerPresent(dir, loop.MarkerRestart) {
		t.Fatal("a roster change queued no RESTART")
	}
}

// An edit that fails the permission-policy gate is refused at build time, so it
// never reaches the rewrite: the child's file is unchanged, no marker is
// written, and the refusal is logged naming the child and the missing policy.
func TestReconcileConfigEditRefusedByPolicyIsNotWritten(t *testing.T) {
	policy := filepath.Join(t.TempDir(), "headless.json")
	if err := os.WriteFile(policy, []byte(`{"permissions": {"allow": []}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	basePath := writeBaseFile(t, fmt.Sprintf(`{"settings_path": %q}`, policy))
	d, inst, dir := materializedInstance(t, basePath, runEntry("daemon-one", ""))
	before := readFileBytes(t, materializedPath(dir))

	missing := filepath.Join(t.TempDir(), "headless-missing.json")
	if err := os.WriteFile(basePath, []byte(fmt.Sprintf(`{"settings_path": %q}`, missing)), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	reconcileStep(d, inst)

	if after := readFileBytes(t, materializedPath(dir)); !bytes.Equal(before, after) {
		t.Fatal("a config refused by the permission-policy gate was written into the running child's file")
	}
	if markerPresent(dir, loop.MarkerRestart) {
		t.Fatal("a config refused by the permission-policy gate queued a RESTART")
	}
	if !inst.policyRefused {
		t.Error("the refused build did not mark the instance policy-refused")
	}
	logText := buf.String()
	if !strings.Contains(logText, "daemon-one") || !strings.Contains(logText, missing) {
		t.Fatalf("the refusal does not name the child and the policy path tried:\n%s", logText)
	}
}

// A child holding HALT or STOP is not ours to restart: the operator's stop
// switch keeps its meaning, and neither the generated file nor a marker is
// touched.
func TestReconcileConfigEditLeavesHaltedOrStoppingChildrenAlone(t *testing.T) {
	for _, marker := range []string{"HALT", "STOP"} {
		t.Run(marker, func(t *testing.T) {
			basePath := writeBaseFile(t, `{"model": "sonnet"}`)
			d, inst, dir := materializedInstance(t, basePath, runEntry("daemon-one", ""))
			before := readFileBytes(t, materializedPath(dir))
			if err := os.WriteFile(filepath.Join(dir, marker), []byte("test\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(basePath, []byte(`{"model": "opus"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			reconcileStep(d, inst)

			if after := readFileBytes(t, materializedPath(dir)); !bytes.Equal(before, after) {
				t.Fatalf("a child holding %s had its generated config rewritten", marker)
			}
			if markerPresent(dir, loop.MarkerRestart) {
				t.Fatalf("a child holding %s was given a RESTART", marker)
			}
		})
	}
}

// End to end through the real Supervise loop and the fake daemon: an unchanged
// base config restarts nothing across several polls; editing it makes the child
// drain at its boundary ONCE, come back on the new file, and stay there.
func TestBaseConfigEditReachesARunningChild(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", "")})
	srv := plane.serve(t)

	basePath := writeBaseFile(t, `{"model": "sonnet"}`)
	base, err := config.Load(basePath)
	if err != nil {
		t.Fatal(err)
	}

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	o := testOptions(t, cacheDir, srv)
	o.BaseCfg = base
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, o)

	state := entryStateDir(cacheDir, "daemon-one")
	path := materializedPath(state)
	waitFor(t, 5*time.Second, "the child to come up on its generated config", func() bool {
		_, err := os.Lstat(path)
		return err == nil && countRuns(t, state) == 1
	})
	before := readFileBytes(t, path)

	// Several polls with no edit: the cache is never touched and nothing
	// restarts.
	time.Sleep(300 * time.Millisecond)
	if got := countRuns(t, state); got != 1 {
		t.Fatalf("spawns with an unchanged base config = %d, want 1 - an unchanged fleet must not restart", got)
	}
	if after := readFileBytes(t, path); !bytes.Equal(before, after) {
		t.Fatal("materialized.json was rewritten with no config edit")
	}

	// The operator edits the machine layer.
	if err := os.WriteFile(basePath, []byte(`{"model": "opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the child to restart once on the new config", func() bool {
		return countRuns(t, state) >= 2
	})
	if logText := buf.String(); !strings.Contains(logText, "daemon-one: effective config changed") {
		t.Fatalf("the queued restart was not logged naming the child:\n%s", logText)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("the restarted child's config does not load: %v", err)
	}
	if loaded.Model != "opus" {
		t.Errorf("the child came back on model %q, want the edited opus", loaded.Model)
	}

	// No restart loop: it comes back once and then stays.
	time.Sleep(400 * time.Millisecond)
	if got := countRuns(t, state); got != 2 {
		t.Fatalf("spawns after the edit settled = %d, want 2 (initial + one restart)", got)
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
