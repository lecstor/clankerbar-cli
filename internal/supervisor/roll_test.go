package supervisor

// Phase 5b (daemon-supervisor): the one-at-a-time fleet roll. A running
// supervisor serves the fleet from the fake daemon + fake plane harness; the
// roll then walks the SAME options (binary, roster, cache dir) the supervisor
// runs on. The binary swap is simulated the way phase 5a built the seam for:
// the VersionOf hook reports the version each spawn was launched as — "1.0.0"
// before the swap, version.Current after it — and the fake daemon echoes that
// version back in its local beacon (CLANKERBAR_CHILD_VERSION rides the spawn).
// The roll's target is its own build (version.Current), so a post-swap child
// satisfies the verify gate and a pre-swap one never does.

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/loop"
	"github.com/lecstor/clankerbar-cli/internal/version"
)

// The whole fleet moves, ONE CHILD AT A TIME: the roll writes RESTART to the
// first child, waits for ITS next beacon to report the new version, and only
// then touches the second — the ordering is pinned in the log, and both
// children end on the target.
func TestRollMovesTheFleetOneChildAtATime(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", ""), runEntry("daemon-two", "")})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := testOptions(t, cacheDir, srv)
	calls := 0
	o.VersionOf = func() string {
		calls++
		if calls <= 2 {
			return "1.0.0" // both initial spawns: the pre-swap binary
		}
		return version.Current // respawns: the swapped-in binary
	}
	o.LaunchVersion = func() (string, error) { return version.Current, nil }
	o.VerifyTimeout = 5 * time.Second
	done := runSupervise(ctx, o)

	a := entryStateDir(cacheDir, "daemon-one")
	b := entryStateDir(cacheDir, "daemon-two")
	waitFor(t, 5*time.Second, "both children to come up on 1.0.0", func() bool {
		ba := readLocalBeacon(a)
		return ba != nil && ba.Version == "1.0.0" &&
			readLocalBeacon(b) != nil && readLocalBeacon(b).Version == "1.0.0"
	})

	if err := Roll(context.Background(), o); err != nil {
		t.Fatalf("Roll returned %v, want nil", err)
	}

	// Both children now report the target version.
	ba, bb := readLocalBeacon(a), readLocalBeacon(b)
	if ba == nil || ba.Version != version.Current {
		t.Fatalf("daemon-one beacon = %+v, want version %s", ba, version.Current)
	}
	if bb == nil || bb.Version != version.Current {
		t.Fatalf("daemon-two beacon = %+v, want version %s", bb, version.Current)
	}

	// The ordering is the point: daemon-one is rolled — its next beacon seen —
	// BEFORE daemon-two is so much as asked.
	text := buf.String()
	iOne := strings.Index(text, "roll: rolling daemon-one")
	iOneDone := strings.Index(text, "roll: daemon-one: the next beacon reports")
	iTwo := strings.Index(text, "roll: rolling daemon-two")
	if iOne < 0 || iOneDone < 0 || iTwo < 0 || !(iOne < iOneDone && iOneDone < iTwo) {
		t.Fatalf("roll did not proceed one child at a time:\n%s", text)
	}
	if !strings.Contains(text, "roll complete: 2 child(ren) on "+version.Current) {
		t.Fatalf("no completion summary:\n%s", text)
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

// A child that fails to come back on the new version HALTS the roll: the
// first child never satisfies the gate, and the roll stops — naming the stuck
// child and how many had rolled — WITHOUT touching the rest of the fleet.
func TestRollHaltsWhenAChildFailsToComeBackOnTheNewVersion(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", ""), runEntry("daemon-two", "")})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := testOptions(t, cacheDir, srv)
	o.VersionOf = func() string { return "1.0.0" } // the swap never took: every spawn stays old
	o.LaunchVersion = func() (string, error) { return version.Current, nil }
	o.VerifyTimeout = 500 * time.Millisecond
	done := runSupervise(ctx, o)

	a := entryStateDir(cacheDir, "daemon-one")
	b := entryStateDir(cacheDir, "daemon-two")
	waitFor(t, 5*time.Second, "both children to come up on 1.0.0", func() bool {
		ba := readLocalBeacon(a)
		return ba != nil && ba.Version == "1.0.0" &&
			readLocalBeacon(b) != nil && readLocalBeacon(b).Version == "1.0.0"
	})

	err := Roll(context.Background(), o)
	if err == nil {
		t.Fatal("Roll returned nil, want a halt error naming the stuck child")
	}
	for _, want := range []string{"daemon-one", "did not come back on", "halting", "0 of 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("halt error %q does not contain %q", err, want)
		}
	}
	// The failing child never satisfied the gate: it came back on the OLD
	// binary, so its beacon still says 1.0.0.
	if ba := readLocalBeacon(a); ba == nil || ba.Version != "1.0.0" {
		t.Errorf("daemon-one beacon = %+v, want 1.0.0 (it failed to come back on the new version)", ba)
	}
	// The roll did NOT continue to the second child: daemon-two was never
	// asked to restart, so it still runs its original spawn.
	if _, err := os.Lstat(filepath.Join(b, loop.MarkerRestart)); !os.IsNotExist(err) {
		t.Errorf("daemon-two received a RESTART marker despite the halt; stat err = %v", err)
	}
	if got := countRuns(t, b); got != 1 {
		t.Errorf("daemon-two was respawned %d time(s), want 1 - the roll must not continue past a failed child", got)
	}
	// No completion summary: the roll halted.
	if text := buf.String(); strings.Contains(text, "roll complete:") {
		t.Fatalf("the roll printed a completion summary despite halting:\n%s", text)
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

// Children already on the target are skipped: no RESTART, no respawn, and a
// line saying so — re-running a roll after a halt is safe.
func TestRollSkipsChildrenAlreadyOnTheTarget(t *testing.T) {
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
	o := testOptions(t, cacheDir, srv)
	o.VersionOf = func() string { return version.Current } // fleet already on the target
	o.LaunchVersion = func() (string, error) { return version.Current, nil }
	o.VerifyTimeout = 5 * time.Second
	done := runSupervise(ctx, o)

	a := entryStateDir(cacheDir, "daemon-one")
	waitFor(t, 5*time.Second, "the child to come up on the target", func() bool {
		ba := readLocalBeacon(a)
		return ba != nil && ba.Version == version.Current
	})

	if err := Roll(context.Background(), o); err != nil {
		t.Fatalf("Roll returned %v, want nil", err)
	}
	if text := buf.String(); !strings.Contains(text, "daemon-one already runs "+version.Current+" - skipping") {
		t.Fatalf("the roll did not say the child was already on the target:\n%s", text)
	}
	if _, err := os.Lstat(entryStateDir(cacheDir, "daemon-one")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(entryStateDir(cacheDir, "daemon-one") + "/RESTART"); !os.IsNotExist(err) {
		t.Errorf("a skipped child must not receive a RESTART marker; stat err = %v", err)
	}
	if got := countRuns(t, a); got != 1 {
		t.Errorf("a skipped child was respawned %d time(s), want 1 (no restart at all)", got)
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

// The roll is scoped to local running entries (Decision 7): remote and
// stopped entries — and entries the supervisor itself refuses — are skipped,
// never restarted.
func TestRollSkipsRemoteStoppedAndRefusedEntries(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{
		runEntry("local", ""),
		stopEntry("stopped"),
		{Name: "remote", DesiredState: RosterDesiredRunning, Placement: RosterPlacementRemote, Projects: []RosterProject{{Slug: "acme"}}},
		{Name: "refused", DesiredState: "sideways", Placement: RosterPlacementLocal, Projects: []RosterProject{{Slug: "acme"}}},
	})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := testOptions(t, cacheDir, srv)
	o.VersionOf = func() string { return version.Current } // local is already on the target
	o.LaunchVersion = func() (string, error) { return version.Current, nil }
	o.VerifyTimeout = 5 * time.Second
	done := runSupervise(ctx, o)

	waitFor(t, 5*time.Second, "the local child to come up", func() bool {
		return readLocalBeacon(entryStateDir(cacheDir, "local")) != nil
	})

	if err := Roll(context.Background(), o); err != nil {
		t.Fatalf("Roll returned %v, want nil", err)
	}
	text := buf.String()
	for _, want := range []string{
		`roll: skipping entry "remote" - placement remote is not implemented (Decision 7)`,
		`roll: skipping entry "stopped" - desired state is "stopped", not running`,
		`roll: skipping entry "refused" - desired state is "sideways", not running`,
		"roll complete: 0 child(ren) on " + version.Current + " (1 already on it, 0 not running, 3 not touched)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("roll output does not contain %q:\n%s", want, text)
		}
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

// The pre-flight refuses a roll whose launch path still carries another
// version: the operator forgot to install the new build (or is running the
// roll from a copy), and touching any child would roll nothing.
func TestRollRefusesWhenTheLaunchBinaryIsNotTheTarget(t *testing.T) {
	o := Options{
		Binary: os.Args[0],
		LaunchVersion: func() (string, error) {
			return "0.0.0-stale", nil
		},
	}
	err := Roll(context.Background(), o)
	if err == nil {
		t.Fatal("Roll returned nil, want a refusal naming the launch binary version")
	}
	for _, want := range []string{"launch binary", "0.0.0-stale", "install the new build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}
}

// A roll with no roster at all — plane unreachable AND no cached one — is
// refused: rolling nothing because the fleet could not be seen would read as
// success.
func TestRollRefusesWhenTheFleetCannotBeSeen(t *testing.T) {
	cacheDir := t.TempDir()
	plane := &fakePlane{}
	plane.setDown(true)
	srv := plane.serve(t)

	o := testOptions(t, cacheDir, srv)
	o.LaunchVersion = func() (string, error) { return version.Current, nil }
	err := Roll(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "no cached roster") {
		t.Fatalf("Roll returned %v, want a refusal naming the missing roster", err)
	}
}

func TestParseVersionLine(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		err  bool
	}{
		{"clankerbar 1.2.3\n", "1.2.3", false},
		{"clankerbar 0.13.3-8-gabc123\n", "0.13.3-8-gabc123", false},
		{"clankerbar 0.0.0-dev\nnext line\n", "0.0.0-dev", false},
		{"something else\n", "", true},
		{"clankerbar \n", "", true},
		{"", "", true},
	} {
		got, err := parseVersionLine(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("parseVersionLine(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseVersionLine(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}
