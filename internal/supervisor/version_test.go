package supervisor

// Phase 5a (daemon-supervisor): version discovery and the status surface.
//
// Discovery: every child is spawned from the supervisor's own binary, so the
// child's version is recorded at ITS spawn — no version query, no per-daemon
// file (the doneWhen's "derived without a hand-maintained per-daemon file").
// The Options.VersionOf hook lets a test simulate the phase-5b roll: children
// spawned before a binary swap stay on the old version while later spawns
// carry the new one, which is the one situation in which a single
// supervisor's children can differ.
//
// Reporting: the supervisor's status surface is its log. On every fleet
// change it prints a listing naming the version it itself runs and, per
// instance, the state and the child's version — so skew between children, and
// between a child and the supervisor, is observable on the machine. The
// listing is idempotent like the reconcile it follows: unchanged polls print
// nothing.

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/version"
)

// instanceLine returns one instance's line out of the LATEST fleet-status
// listing. The log buffer accumulates listings, so a plain first-match search
// would keep returning the startup listing forever; the current surface is
// the last "fleet status:" block. The per-child assertions (which version
// sits on which line) go through this, so they pin the mapping the test
// exists for rather than a global substring.
func instanceLine(t *testing.T, text, name string) string {
	t.Helper()
	idx := strings.LastIndex(text, "fleet status:")
	if idx < 0 {
		t.Fatalf("no fleet status listing yet:\n%s", text)
		return ""
	}
	tail := text[idx:]
	for _, line := range strings.Split(tail, "\n") {
		if strings.HasPrefix(line, "  "+name+": ") {
			return line
		}
	}
	t.Fatalf("no listing line for %q in the latest listing:\n%s", name, tail)
	return ""
}

// The discovery and reporting shape: with no hook, every child's version is
// the supervisor's own build (version.Current), and the listing carries the
// supervisor's version in the header and each child's version on its line.
func TestFleetStatusListsEachChildWithItsVersion(t *testing.T) {
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
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	a := entryStateDir(cacheDir, "daemon-one")
	b := entryStateDir(cacheDir, "daemon-two")
	waitFor(t, 5*time.Second, "both children to come up", func() bool {
		return countRuns(t, a) == 1 && countRuns(t, b) == 1
	})
	// The status listing appears as the fleet comes up, naming the
	// supervisor's own version and each child's.
	waitFor(t, 5*time.Second, "the fleet status listing to be logged", func() bool {
		text := buf.String()
		return strings.Contains(text, "fleet status: supervisor "+version.Current) &&
			strings.Contains(text, "daemon-one: running (pid ") &&
			strings.Contains(text, "daemon-two: running (pid ") &&
			strings.Contains(text, "version "+version.Current)
	})
	// The spawn lines carry the version too — the discovery is visible at the
	// moment it happens, not only in the listing.
	if text := buf.String(); !strings.Contains(text, "spawned (pid ") || !strings.Contains(text, ", version "+version.Current) {
		t.Fatalf("the spawn line does not report the child's version:\n%s", text)
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

// Skew between CHILDREN is observable: children spawned before a binary swap
// stay on the old version while later spawns carry the new one (the phase-5b
// roll), and the listing shows each child with its own version.
func TestFleetStatusShowsSkewBetweenChildren(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	// The roster order is the reconcile order: daemon-old spawns first (the
	// pre-swap binary), daemon-new second (the swapped-in binary).
	plane.set([]RosterEntry{runEntry("daemon-old", ""), runEntry("daemon-new", "")})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	calls := 0
	o := testOptions(t, cacheDir, srv)
	o.VersionOf = func() string {
		calls++
		if calls == 1 {
			return "1.0.0"
		}
		return "2.0.0"
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, o)

	oldState := entryStateDir(cacheDir, "daemon-old")
	newState := entryStateDir(cacheDir, "daemon-new")
	waitFor(t, 5*time.Second, "both children to come up", func() bool {
		return countRuns(t, oldState) == 1 && countRuns(t, newState) == 1
	})
	// The one listing carries both versions, EACH ON ITS OWN CHILD'S LINE —
	// the per-child mapping is the point of this test, so it is asserted
	// line-scoped, not as a global substring.
	waitFor(t, 5*time.Second, "the listing to show each child on its own version", func() bool {
		text := buf.String()
		return strings.Contains(instanceLine(t, text, "daemon-old"), "running (pid ") &&
			strings.Contains(instanceLine(t, text, "daemon-old"), "version 1.0.0") &&
			strings.Contains(instanceLine(t, text, "daemon-new"), "running (pid ") &&
			strings.Contains(instanceLine(t, text, "daemon-new"), "version 2.0.0")
	})

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

// Skew between a CHILD and the SUPERVISOR is observable: the header names the
// supervisor's own build while a child spawned from a different build carries
// its own version on its line.
func TestFleetStatusShowsSkewBetweenChildAndSupervisor(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", "")})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	o := testOptions(t, cacheDir, srv)
	o.VersionOf = func() string { return "2.0.0" } // the child's binary differs from the supervisor's
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, o)

	state := entryStateDir(cacheDir, "daemon-one")
	waitFor(t, 5*time.Second, "the child to come up", func() bool { return countRuns(t, state) == 1 })
	waitFor(t, 5*time.Second, "the listing to name both the supervisor and the child version", func() bool {
		text := buf.String()
		return strings.Contains(text, "fleet status: supervisor "+version.Current) &&
			strings.Contains(instanceLine(t, text, "daemon-one"), "running (pid ") &&
			strings.Contains(instanceLine(t, text, "daemon-one"), "version 2.0.0")
	})

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

// The status surface stays CURRENT: a child's unexpected exit flips its line
// from running to "restarting in", and the respawn flips it back — each
// transition printed exactly once.
func TestFleetStatusTracksExitsAndRespawns(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "crash")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("crashy", "")})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	// The first spawn is listed as running...
	waitFor(t, 5*time.Second, "the listing to show the child running", func() bool {
		return strings.Contains(buf.String(), "crashy: running (pid ")
	})
	// ...the crash schedules a restart and the listing follows...
	waitFor(t, 5*time.Second, "the listing to show the restart scheduled", func() bool {
		return strings.Contains(buf.String(), "crashy: restarting in ")
	})
	// ...and the respawn is listed as running again (a NEW child).
	waitFor(t, 5*time.Second, "the listing to show the respawned child running", func() bool {
		text := buf.String()
		i := strings.Index(text, "crashy: running (pid ")
		j := strings.LastIndex(text, "crashy: running (pid ")
		return i >= 0 && j > i
	})

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

// The status surface is as idempotent as the reconcile it follows: unchanged
// polls print nothing, so a settled fleet's listing appears exactly once.
func TestFleetStatusDoesNotRepeatOnUnchangedPolls(t *testing.T) {
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
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	a := entryStateDir(cacheDir, "daemon-one")
	b := entryStateDir(cacheDir, "daemon-two")
	waitFor(t, 5*time.Second, "both children to come up", func() bool {
		return countRuns(t, a) == 1 && countRuns(t, b) == 1
	})
	waitFor(t, 5*time.Second, "the listing to appear once", func() bool {
		return strings.Contains(buf.String(), "fleet status: supervisor ")
	})
	before := strings.Count(buf.String(), "fleet status:")
	// Six unchanged polls at the test interval: the listing must not repeat.
	time.Sleep(6 * 50 * time.Millisecond)
	if after := strings.Count(buf.String(), "fleet status:"); after != before {
		t.Fatalf("the fleet status listing repeated on unchanged polls: %d before, %d after — the status surface must be idempotent like the reconcile it follows", before, after)
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

// The status surface stays idempotent while a restart is PENDING too: the
// listing names the SCHEDULED delay, so its text does not drift between polls
// (a countdown would re-print the listing on every poll for the whole backoff
// window — the "at most one listing per transition" contract, broken).
func TestFleetStatusDoesNotRepeatWhileARestartIsPending(t *testing.T) {
	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	d := &Supervisor{
		instances: []*Instance{{
			name:        "crashy",
			desired:     RosterDesiredRunning,
			restartAt:   time.Now().Add(time.Second),
			lastBackoff: time.Second,
		}},
	}
	d.logFleetStatus()
	// The remaining time has since changed; the listing text must not have.
	time.Sleep(5 * time.Millisecond)
	d.logFleetStatus()
	if got := strings.Count(buf.String(), "fleet status:"); got != 1 {
		t.Fatalf("the pending-restart listing repeated while no fleet state changed: %d listings, want 1 — the listing must name the scheduled delay, not a drifting countdown", got)
	}
	if line := buf.String(); !strings.Contains(line, "crashy: restarting in 1s (last version ") {
		t.Fatalf("the pending-restart line does not name the scheduled delay:\n%s", line)
	}
}

// A removed instance is NAMED at its exit: the listing flips its line to
// "removed" the moment the child drains — the operator sees what happened to
// the entry instead of watching it silently disappear (without the onExit
// listing, the last listing would be "stopping" and the instance would then
// leave the fleet unnamed, as the next reconcile drops it).
func TestFleetStatusNamesARemovedInstanceAtItsExit(t *testing.T) {
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
	waitFor(t, 5*time.Second, "the child to come up", func() bool { return countRuns(t, state) == 1 })
	// Remove the entry: the child drains and the listing names the removal.
	plane.set([]RosterEntry{})
	waitFor(t, 5*time.Second, "the listing to name the removed instance at its exit", func() bool {
		return strings.Contains(instanceLine(t, buf.String(), "daemon-one"), "removed")
	})

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
