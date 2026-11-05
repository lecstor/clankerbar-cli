package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/loop"
)

// A child that ends through the Stop-limit path is not respawned, and the
// report names the limit reason; it is a stop, not a crash.
func TestLimitStopLeavesInstanceStopped(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "limit-stop")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("broke", "")})
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	state := entryStateDir(cacheDir, "broke")
	rep := waitForStatusState(t, plane, "broke", StatusStopped)
	inst := runningInstance(rep, "broke")
	if inst.Reason != "stopped: budget_exhausted" {
		t.Errorf("reason = %q, want %q", inst.Reason, "stopped: budget_exhausted")
	}
	if inst.NextRetryAt != nil {
		t.Errorf("nextRetryAt = %v, want null", *inst.NextRetryAt)
	}
	time.Sleep(300 * time.Millisecond) // six base backoffs
	if got := countRuns(t, state); got != 1 {
		t.Fatalf("spawns = %d, want 1 - a Stop-limit exit must not be restarted", got)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Supervise returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Supervise did not return")
	}
}

// Deleting the marker is the resume: the next poll respawns the child.
func TestLimitStopResumesWhenMarkerDeleted(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "limit-stop-once")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("broke", "")})
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	state := entryStateDir(cacheDir, "broke")
	waitForStatusState(t, plane, "broke", StatusStopped)
	marker := filepath.Join(state, loop.LimitStopName)
	if _, err := os.Lstat(marker); err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the resumed child to be spawned", func() bool { return countRuns(t, state) == 2 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Supervise did not return")
	}
}

func TestFleetStatusNamesTheLimitStop(t *testing.T) {
	d := &Supervisor{instances: []*Instance{{
		name: "broke", desired: RosterDesiredRunning, limitStopped: true, limitReason: "budget_exhausted",
	}}}
	got := d.fleetStatus()
	if !strings.Contains(got, "broke: stopped: budget_exhausted") || strings.Contains(got, "restarting") {
		t.Errorf("fleet status = %q", got)
	}
}

// The marker outlives a supervisor restart: a cold start finds it and spawns nothing.
func TestLimitStopHoldsAcrossSupervisorStart(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("broke", "")})
	srv := plane.serve(t)

	state := entryStateDir(cacheDir, "broke")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, loop.LimitStopName), []byte("budget_exhausted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))
	rep := waitForStatusState(t, plane, "broke", StatusStopped)
	if r := runningInstance(rep, "broke").Reason; r != "stopped: budget_exhausted" {
		t.Errorf("reason = %q", r)
	}
	time.Sleep(200 * time.Millisecond)
	if got := countRuns(t, state); got != 0 {
		t.Errorf("spawns = %d, want 0 while the marker is present", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Supervise did not return")
	}
}
