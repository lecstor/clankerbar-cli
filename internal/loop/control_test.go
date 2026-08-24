package loop

// CLA-461: the restart/reload control markers. The graceful pair (RESTART,
// RESTART_NOW with nothing in flight) and RELOAD are honoured at the iteration
// boundary — where STOP is checked; RESTART_NOW with a session in flight is
// caught by the watcher, which cancels the run context and rides the same
// kill/salvage/release machinery a Ctrl-C rides. The re-exec itself lives in
// cli.Run (it must not run under go test), so these tests pin everything up to
// it: the flag the CLI reads, the consumed marker, and the claim hygiene.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// hookedAdapter adds an Invoke hook to fakeAdapter so a test can act "while the
// session is in flight" — planting a marker exactly when a session is running.
type hookedAdapter struct {
	fakeAdapter
	onInvoke func(n int, ctx context.Context)
}

func (h *hookedAdapter) Invoke(ctx context.Context, in harness.Invocation) (harness.Result, error) {
	if h.onInvoke != nil {
		h.onInvoke(h.invokeCalls, ctx)
	}
	return h.fakeAdapter.Invoke(ctx, in)
}

// plantMarker writes a marker without t.Fatalf, for use off the test goroutine
// (the RESTART_NOW watcher test plants from a goroutine).
func plantMarker(dir, name, body string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
}

func markerAbsent(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s marker should have been consumed once acted on; stat err = %v", name, err)
	}
}

// A RESTART dropped while a session runs is honoured only at the next iteration
// boundary: the session runs to COMPLETION first, then the run unwinds toward a
// re-exec. No claim is held across the boundary — the drain handed its task
// back before the check ever ran — so the releaser sees nothing.
func TestRun_RestartWaitsForTheSessionThenReExecs(t *testing.T) {
	cfg := fastCfg()
	dir := t.TempDir()
	cfg.StateDir = dir
	cfg.MaxIterations = 5 // generous: the restart, not the ceiling, ends this run
	h := &hookedAdapter{fakeAdapter: fakeAdapter{steps: []invokeStep{{res: okResult(0, 0)}}}}
	h.onInvoke = func(n int, _ context.Context) {
		if n == 0 {
			// Dropped mid-session. The session itself still completes cleanly.
			if err := plantMarker(dir, MarkerRestart, "new build installed"); err != nil {
				t.Errorf("planting RESTART: %v", err)
			}
		}
	}
	p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
	rel := &fakeReleaser{}
	d := NewMulti(cfg, h, []Target{{Poller: p, Releaser: rel}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 1 {
		t.Errorf("the in-flight session must run to completion before the restart; got %d sessions", h.invokeCalls)
	}
	if !d.RestartRequested() {
		t.Error("Run ended on a consumed RESTART marker; RestartRequested must be true so the caller re-execs")
	}
	markerAbsent(t, dir, MarkerRestart)
	if len(rel.calls) != 0 {
		t.Errorf("a clean session owes no handback; releaser saw %v", rel.calls)
	}
}

// --now is the point of the whole feature: a RESTART_NOW planted mid-session
// kills that session within the poll bound (via ctx cancellation — the same
// process-group kill a Ctrl-C rides), the held claim is RELEASED (recorded as
// released, not failed), and the run ends with RestartRequested set. Exactly
// one session is spawned: the kill must not be retried.
func TestRun_RestartNowKillsInFlightSessionAndReleasesItsClaim(t *testing.T) {
	cfg := fastCfg()
	dir := t.TempDir()
	cfg.StateDir = dir
	started := make(chan struct{})
	var once sync.Once
	h := &killAdapter{started: started, once: &once}
	p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
	rel := &fakeReleaser{}
	d := NewMulti(cfg, h, []Target{{Poller: p, Releaser: rel}})
	d.restartPoll = time.Millisecond
	go func() {
		<-started
		if err := plantMarker(dir, MarkerRestartNow, "session known-doomed"); err != nil {
			t.Errorf("planting RESTART_NOW: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 1 {
		t.Errorf("a killed session must not be respawned; got %d Invoke calls", h.invokeCalls)
	}
	if !d.RestartRequested() {
		t.Error("RestartRequested must be set after a --now kill")
	}
	markerAbsent(t, dir, MarkerRestartNow)
	if len(rel.calls) != 1 || rel.calls[0] != (releaseCall{"t-1", "r-1"}) {
		t.Errorf("the killed session's held claim must be released (released, not failed); releaser saw %v", rel.calls)
	}
}

// killAdapter blocks until its context is cancelled — an in-flight session being
// killed by --now — and reports the claim it was holding, the way a real adapter
// surfaces what its stream showed before the group kill landed.
type killAdapter struct {
	fakeAdapter
	started     chan struct{}
	once        *sync.Once
	invokeCalls int
}

func (k *killAdapter) Invoke(ctx context.Context, in harness.Invocation) (harness.Result, error) {
	k.invokeCalls++
	k.once.Do(func() { close(k.started) })
	<-ctx.Done()
	return harness.Result{
		ExitCode: 137,
		Claim:    harness.Claim{TaskID: "t-1", RunID: "r-1"},
	}, ctx.Err()
}

// A RELOAD re-reads the config at the boundary WITHOUT exec: the NEXT session
// runs on the reloaded config, the process carries on, and no restart is
// requested. STOP (planted by the second session here) still ends the run the
// old way — the regression guard for the unchanged stop behaviour.
func TestRun_ReloadAppliesToTheNextIterationWithoutExec(t *testing.T) {
	cfg := fastCfg()
	dir := t.TempDir()
	cfg.StateDir = dir
	cfg.Prompt = "original brief"
	cfg.MaxIterations = 10
	h := &hookedAdapter{fakeAdapter: fakeAdapter{steps: []invokeStep{{res: okResult(0, 0)}, {res: okResult(0, 0)}}}}
	h.onInvoke = func(n int, _ context.Context) {
		switch n {
		case 0:
			if err := plantMarker(dir, MarkerReload, ""); err != nil {
				t.Errorf("planting RELOAD: %v", err)
			}
		case 1:
			// End the run the ordinary way so the test terminates deterministically.
			if err := plantMarker(dir, "STOP", ""); err != nil {
				t.Errorf("planting STOP: %v", err)
			}
		}
	}
	p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
	d := New(cfg, h, p)
	reloaded := false
	d.SetReloader(func() (*config.Config, error) {
		fresh := fastCfg()
		fresh.StateDir = cfg.StateDir
		fresh.Prompt = "reloaded brief"
		reloaded = true
		return fresh, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("want two sessions (one before the reload, one after); got %d", h.invokeCalls)
	}
	if !reloaded {
		t.Error("the reload closure never ran")
	}
	if got := h.invocations[1].Prompt; !strings.Contains(got, "reloaded brief") {
		t.Errorf("the second session must run on the reloaded prompt; got %q", got)
	}
	if d.RestartRequested() {
		t.Error("a reload must not request a restart/re-exec")
	}
	markerAbsent(t, dir, MarkerReload)
}

// A broken edit must not kill a daemon that was running fine: a reload whose
// closure fails keeps the current config and carries on.
func TestRun_FailedReloadKeepsTheCurrentConfig(t *testing.T) {
	cfg := fastCfg()
	dir := t.TempDir()
	cfg.StateDir = dir
	cfg.Prompt = "original brief"
	cfg.MaxIterations = 10
	h := &hookedAdapter{fakeAdapter: fakeAdapter{steps: []invokeStep{{res: okResult(0, 0)}, {res: okResult(0, 0)}}}}
	h.onInvoke = func(n int, _ context.Context) {
		switch n {
		case 0:
			if err := plantMarker(dir, MarkerReload, ""); err != nil {
				t.Errorf("planting RELOAD: %v", err)
			}
		case 1:
			if err := plantMarker(dir, "STOP", ""); err != nil {
				t.Errorf("planting STOP: %v", err)
			}
		}
	}
	p := &fakePoller{sum: backlog.Summary{Claimable: 1}}
	d := New(cfg, h, p)
	d.SetReloader(func() (*config.Config, error) { return nil, errors.New("config.json: not valid JSON") })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.invokeCalls != 2 {
		t.Fatalf("a failed reload must not end the run; got %d sessions then stop", h.invokeCalls)
	}
	if d.cfg.Prompt != "original brief" {
		t.Errorf("the current config must survive a failed reload; prompt now %q", d.cfg.Prompt)
	}
}

// The waits are control-responsive too: each marker is honoured from inside
// waitOrStop, which backs both idle polls and supervised limit waits.
func TestWaitOrStopHonoursTheControlMarkers(t *testing.T) {
	t.Run("RESTART stops toward a re-exec", func(t *testing.T) {
		d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
		openTestStateDir(t, d)
		writeMarker(t, d.state.Path(), MarkerRestart, "")
		start := time.Now()
		if stop := d.waitOrStop(context.Background(), time.Minute); !stop {
			t.Fatal("a RESTART during a wait must stop the wait")
		}
		if !d.RestartRequested() {
			t.Error("waitOrStop must flag the restart for the caller")
		}
		markerAbsent(t, d.state.Path(), MarkerRestart)
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("the wait ran %s; the marker should be seen on the first chunk", elapsed)
		}
	})

	t.Run("RESTART_NOW stops toward a re-exec", func(t *testing.T) {
		d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
		openTestStateDir(t, d)
		writeMarker(t, d.state.Path(), MarkerRestartNow, "")
		if stop := d.waitOrStop(context.Background(), time.Minute); !stop {
			t.Fatal("a RESTART_NOW during a wait must stop the wait")
		}
		if !d.RestartRequested() {
			t.Error("waitOrStop must flag the restart for the caller")
		}
		markerAbsent(t, d.state.Path(), MarkerRestartNow)
	})

	t.Run("RELOAD applies in place and lets the wait finish", func(t *testing.T) {
		d := New(fastCfg(), &fakeAdapter{}, &fakePoller{})
		openTestStateDir(t, d)
		writeMarker(t, d.state.Path(), MarkerReload, "")
		d.SetReloader(func() (*config.Config, error) {
			fresh := fastCfg()
			fresh.StateDir = d.cfg.StateDir
			fresh.Prompt = "reloaded brief"
			return fresh, nil
		})
		const dur = 30 * time.Millisecond
		start := time.Now()
		if stop := d.waitOrStop(context.Background(), dur); stop {
			t.Fatal("a RELOAD during a wait applies in place; the wait is not a stop")
		}
		if elapsed := time.Since(start); elapsed < dur {
			t.Errorf("a reload must not cut the wait short (an immediate probe against a usage limit would cost a paid session); waited %s", elapsed)
		}
		if d.RestartRequested() {
			t.Error("a reload must not request a restart")
		}
		if d.cfg.Prompt != "reloaded brief" {
			t.Errorf("the config was not swapped by the wait-side reload; prompt %q", d.cfg.Prompt)
		}
		markerAbsent(t, d.state.Path(), MarkerReload)
	})
}
