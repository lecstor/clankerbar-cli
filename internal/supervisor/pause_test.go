package supervisor

// CLA-908: a live child stopped by an operator pause window says so in the fleet
// listing — the reason, the window's end in the operator's own zone, and any task
// held at a phase boundary — read from the same local BEACON the plane POST
// carries, so the listing and the console's pause banner agree.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/loop"
)

// brisbaneZone is the zone fixture: the schedule names it, so the listing must
// render the end there rather than in whatever zone the test process runs in.
const brisbaneZone = "Australia/Brisbane"

func brisbaneLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(brisbaneZone)
	if err != nil {
		t.Fatalf("load %s: %v", brisbaneZone, err)
	}
	return loc
}

func atPtr(t time.Time) *time.Time { return &t }

// writeBeacon plants one local BEACON file, marshalled from the type
// internal/loop writes it with — so this fixture cannot state a field or a tag
// the reader does not know, and a rename cannot pass both suites (CLA-908).
func writeBeacon(t *testing.T, dir string, b loop.LocalBeacon) {
	t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal beacon: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, loop.LocalBeaconName), data, 0o644); err != nil {
		t.Fatalf("write beacon: %v", err)
	}
}

func TestSchedulePauseText(t *testing.T) {
	loc := brisbaneLoc(t)
	now := time.Date(2026, time.November, 9, 12, 30, 0, 0, loc)
	until := time.Date(2026, time.November, 9, 14, 0, 0, 0, loc)

	cases := []struct {
		name   string
		beacon loop.LocalBeacon
		want   string
	}{
		{
			name: "schedule with an end and a held task",
			beacon: loop.LocalBeacon{
				Version: "1.2.3", State: "draining", At: now,
				PauseReason: "schedule", PauseUntil: atPtr(until.UTC()), PauseZone: brisbaneZone,
				TaskRef: "CLA-1", Phase: "review",
			},
			want: "paused by schedule until 14:00 AEST (holding CLA-1 at the review phase)",
		},
		{
			name: "schedule with no held task",
			beacon: loop.LocalBeacon{
				Version: "1.2.3", State: "draining", At: now,
				PauseReason: "schedule", PauseUntil: atPtr(until.UTC()), PauseZone: brisbaneZone,
			},
			want: "paused by schedule until 14:00 AEST",
		},
		{
			name: "overnight window names the day",
			beacon: loop.LocalBeacon{
				Version: "1.2.3", State: "draining", At: now,
				PauseReason: "schedule",
				PauseUntil:  atPtr(time.Date(2026, time.November, 10, 6, 0, 0, 0, loc).UTC()),
				PauseZone:   brisbaneZone,
			},
			want: "paused by schedule until Tue 06:00 AEST",
		},
		{
			name: "schedule with an unstated end keeps the reason alone",
			beacon: loop.LocalBeacon{
				Version: "1.2.3", State: "draining", At: now,
				PauseReason: "schedule",
			},
			want: "paused by schedule",
		},
		{
			// The plane's instance view treats a passed end as a window that
			// ended while the beacon has not caught up; the listing agrees.
			name: "a window whose stated end has passed is over",
			beacon: loop.LocalBeacon{
				Version: "1.2.3", State: "draining", At: now,
				PauseReason: "schedule",
				PauseUntil:  atPtr(now.Add(-time.Minute).UTC()),
				PauseZone:   brisbaneZone,
			},
			want: "",
		},
		{
			// The console/fleet/startup/budget causes are not the schedule: the
			// listing's line is the schedule's, so none of them invents one.
			name: "a console pause is not the schedule",
			beacon: loop.LocalBeacon{
				Version: "1.2.3", State: "draining", At: now,
				PauseReason: "console",
			},
			want: "",
		},
		{
			name: "an idle child has no pause",
			beacon: loop.LocalBeacon{
				Version: "1.2.3", State: "idle", At: now,
			},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeBeacon(t, dir, tc.beacon)
			if got := schedulePauseText(dir, now); got != tc.want {
				t.Errorf("schedulePauseText = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("no beacon yet", func(t *testing.T) {
		if got := schedulePauseText(t.TempDir(), now); got != "" {
			t.Errorf("schedulePauseText with no beacon = %q, want none", got)
		}
	})
}

// The listing an operator reads: the child is alive (its process is up) and its
// loop is holding spawns closed, so the line says why and until when — the same
// sentence the console's instance row shows.
func TestFleetStatusNamesASchedulePause(t *testing.T) {
	loc := brisbaneLoc(t)
	dir := t.TempDir()
	writeBeacon(t, dir, loop.LocalBeacon{
		Version: "0.25.6", State: "draining", At: time.Now(),
		PauseReason: "schedule",
		PauseUntil:  atPtr(time.Now().Add(90 * time.Minute).UTC()),
		PauseZone:   brisbaneZone,
		TaskRef:     "CLA-1", Phase: "review",
	})

	inst := &Instance{
		name:         "clanker1",
		desired:      RosterDesiredRunning,
		cmd:          &exec.Cmd{Process: &os.Process{Pid: 4242}},
		aliveSince:   time.Now(),
		childVersion: "0.25.6",
		stateDir:     dir,
	}
	d := &Supervisor{hostname: "headless", startedAt: time.Now(), instances: []*Instance{inst}}

	text := d.fleetStatus()
	if !strings.Contains(text, "clanker1: running (pid 4242, version 0.25.6) - paused by schedule until ") {
		t.Fatalf("fleet listing does not name the schedule pause:\n%s", text)
	}
	if !strings.Contains(text, "(holding CLA-1 at the review phase)") {
		t.Errorf("fleet listing does not name the held task:\n%s", text)
	}
	// The end is rendered in the schedule's zone, whatever this process's zone is.
	want := time.Now().Add(90 * time.Minute).In(loc).Format("15:04 MST")
	if !strings.Contains(text, want) {
		t.Errorf("fleet listing does not render the end in %s (want %q):\n%s", brisbaneZone, want, text)
	}

	// No pause line when the child's beacon says idle: the listing is unchanged.
	writeBeacon(t, dir, loop.LocalBeacon{
		Version: "0.25.6", State: "idle", At: time.Now(),
	})
	if text := d.fleetStatus(); strings.Contains(text, "paused by") {
		t.Errorf("an idle child grew a pause line:\n%s", text)
	}
}
