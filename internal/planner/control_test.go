package planner

import (
	"context"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// 1.review-1.1: `clankerbar ctl` writes the drain's control markers, and an
// operator running it against a planner config expects them to act. The planner
// honours them at the turn boundary, which is the top of each poll tick.

func controlFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, map[string]any{"harness": "claude"})
	dir, err := statedir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("statedir.Open: %v", err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	f.planner.state = dir
	return f
}

func TestControlStopEndsTheRunAndConsumesTheMarker(t *testing.T) {
	f := controlFixture(t)
	if err := f.planner.state.WriteFile("STOP", []byte("maintenance")); err != nil {
		t.Fatal(err)
	}
	if !f.planner.checkControl() {
		t.Fatal("STOP did not end the run")
	}
	if f.planner.state.Exists("STOP") {
		t.Error("STOP marker was left behind; the next start would consume it as a ghost")
	}
}

func TestControlHaltEndsTheRunAndLeavesTheMarker(t *testing.T) {
	f := controlFixture(t)
	if err := f.planner.state.WriteFile("HALT", []byte("wedged")); err != nil {
		t.Fatal(err)
	}
	if !f.planner.checkControl() {
		t.Fatal("HALT did not end the run")
	}
	if !f.planner.state.Exists("HALT") {
		t.Error("HALT marker was removed; it exists to keep the daemon down until a human resolves it")
	}
}

func TestControlRestartMarkersRequestAReexec(t *testing.T) {
	for _, marker := range []string{markerRestart, markerRestartNow} {
		t.Run(marker, func(t *testing.T) {
			f := controlFixture(t)
			if err := f.planner.state.WriteFile(marker, []byte("install done")); err != nil {
				t.Fatal(err)
			}
			if !f.planner.checkControl() {
				t.Fatalf("%s did not end the run", marker)
			}
			if !f.planner.RestartRequested() {
				t.Fatalf("%s did not request a re-exec", marker)
			}
			if f.planner.state.Exists(marker) {
				t.Error("marker was left behind; a crash before the re-exec would restart again")
			}
		})
	}
}

func TestControlReloadSwapsTheLocalConfig(t *testing.T) {
	f := controlFixture(t)
	f.planner.SetReloader(func() (*config.Config, error) {
		fresh := *f.base
		fresh.Role = config.RolePlanner
		fresh.Model = "reloaded-model"
		// The fixture's one target is named, so the fresh config must name the
		// same project set for the reload to be applicable.
		fresh.Projects = []config.Project{{Slug: "demo"}}
		return &fresh, nil
	})
	if err := f.planner.state.WriteFile(markerReload, nil); err != nil {
		t.Fatal(err)
	}
	if f.planner.checkControl() {
		t.Fatal("RELOAD ended the run; it must re-read and continue")
	}
	if f.planner.base.Model != "reloaded-model" {
		t.Errorf("base model = %q, want the reloaded config's", f.planner.base.Model)
	}
	if f.planner.state.Exists(markerReload) {
		t.Error("RELOAD marker was not consumed")
	}
}

// The loop checks markers, not just the helper: a RESTART dropped while the
// poller idles ends Run and leaves the re-exec decision to the caller.
func TestRunHonoursARestartMarker(t *testing.T) {
	f := controlFixture(t)
	f.planner.poll = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.planner.Run(ctx) }()

	// Run consumes a marker present at start-up only for STOP; plant RESTART
	// after the loop is live.
	deadline := time.After(2 * time.Second)
	for {
		f.poller.mu.Lock()
		calls := f.poller.calls
		f.poller.mu.Unlock()
		if calls >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Run never polled")
		case <-time.After(time.Millisecond):
		}
	}
	if err := f.planner.state.WriteFile(markerRestart, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after a RESTART marker")
	}
	if !f.planner.RestartRequested() {
		t.Error("Run ended without requesting the re-exec")
	}
}
