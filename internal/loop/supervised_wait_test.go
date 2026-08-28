package loop

// CLA-305: a supervised wait that holds a stated reset time must use it. The
// blind 30-minute grid threw the reset away - a cap observed lifting at 08:24
// left the loop idle until its 08:46 tick, 22 minutes of dead time with work
// queued and nothing wrong. These tests pin the aligned wait (probe shortly
// after the stated reset), the untouched fallbacks (unknown or already-past
// resets poll exactly as before), the honesty owed when the claim fails (a
// reset that passes while still limited keeps POLLING rather than resuming),
// and the STOP marker cutting any of it short.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

func TestProbeWait(t *testing.T) {
	const interval = 30 * time.Minute
	const grace = time.Minute
	now := time.Date(2026, 8, 24, 7, 16, 40, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		reset time.Time
		want  time.Duration
	}{
		{"unknown reset polls on the grid", time.Time{}, interval},
		{"past reset polls on the grid", now.Add(-time.Hour), interval},
		{"reset exactly now polls on the grid", now, interval},
		{"future reset inside the horizon wakes just past it", now.Add(20 * time.Minute), 21 * time.Minute},
		{"future reset beyond the horizon is capped at the interval", now.Add(2 * time.Hour), interval},
		{"reset+grace landing exactly on the horizon is capped too", now.Add(29 * time.Minute), interval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := probeWait(now, tc.reset, interval, grace)
			if got != tc.want {
				t.Errorf("probeWait(reset=%s) = %s, want %s", tc.reset.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

// The headline case: with a known future reset, the FIRST probe lands shortly
// after that reset instead of on the interval grid. The grid here is three
// seconds against a reset 120ms out; the blind loop of CLA-305 would have
// slept the full grid.
func TestSupervisedWait_FirstProbeLandsShortlyAfterTheStatedReset(t *testing.T) {
	const grid = 3 * time.Second
	cfg := fastCfg()
	cfg.PollInterval = config.Duration(grid)
	h := &fakeAdapter{probeResults: []harness.Limit{{Limited: false}}}
	d := New(cfg, h, &fakePoller{})
	openTestStateDir(t, d)
	d.waitGrace = 30 * time.Millisecond

	logs := captureLogs(t)
	start := time.Now()
	_, _, stop := d.supervisedWait(context.Background(),
		harness.Limit{Limited: true, ResetAt: start.Add(120 * time.Millisecond)},
		h, d.cfg.EffectivePhases()[0], d.targets[0], spend{start: start})
	elapsed := time.Since(start)

	if stop {
		t.Fatalf("the limit lifted at its stated reset; the wait should resume, not stop\n%s", logs.String())
	}
	if h.probeCalls != 1 {
		t.Errorf("got %d probes, want exactly the one confirmation past the reset", h.probeCalls)
	}
	if elapsed >= grid/2 {
		t.Fatalf("first probe took %s - the loop slept toward its %s grid while holding a reset %s out\n%s",
			elapsed.Round(time.Millisecond), grid, 120*time.Millisecond, logs.String())
	}
	if elapsed < 140*time.Millisecond {
		t.Errorf("first probe after only %s - sooner than the stated reset plus its grace the wait claims to honour\n%s",
			elapsed.Round(time.Millisecond), logs.String())
	}
	if !strings.Contains(logs.String(), "waiting out the stated reset") {
		t.Errorf("the paused log must say the wait is aimed at a stated reset, so an operator can tell it from blind polling:\n%s", logs.String())
	}
}

// A zero ResetAt (codex always; claude after CLA-258 whenever no typed source
// states one) keeps today's cadence byte for byte: sleep the interval, probe,
// repeat until a probe says lifted.
func TestSupervisedWait_ZeroResetKeepsBlindIntervalPolling(t *testing.T) {
	const interval = 50 * time.Millisecond
	cfg := fastCfg()
	cfg.PollInterval = config.Duration(interval)
	h := &fakeAdapter{probeResults: []harness.Limit{{Limited: true}, {Limited: true}, {Limited: false}}}
	d := New(cfg, h, &fakePoller{})
	openTestStateDir(t, d)

	logs := captureLogs(t)
	start := time.Now()
	_, _, stop := d.supervisedWait(context.Background(),
		harness.Limit{Limited: true},
		h, d.cfg.EffectivePhases()[0], d.targets[0], spend{start: start})
	elapsed := time.Since(start)

	if stop {
		t.Fatalf("an unknown reset must never stop the wait\n%s", logs.String())
	}
	if h.probeCalls != 3 {
		t.Fatalf("got %d probes, want 3 (two limited laps, then lifted)", h.probeCalls)
	}
	// Two full interval sleeps separate the three probes; a loop that skipped
	// the waits would finish in microseconds.
	if elapsed < 2*interval {
		t.Errorf("three probes completed in %s - the loop stopped sleeping a full interval between them\n%s",
			elapsed.Round(time.Millisecond), logs.String())
	}
	if !strings.Contains(logs.String(), "probing every") {
		t.Errorf("an unknown reset is blind polling and the log must say so:\n%s", logs.String())
	}
}

// The claim failed its confirmation: the wake lands past the stated reset, the
// probe says STILL limited, and the loop must keep polling on the grid rather
// than resume unprobed one interval later. Resuming would trust a provider
// claim that was just measured false.
func TestSupervisedWait_ResetPassingMidWaitFallsBackToPollingNotResuming(t *testing.T) {
	const interval = 50 * time.Millisecond
	cfg := fastCfg()
	cfg.PollInterval = config.Duration(interval)
	// Timeline with a 15ms grace and a reset 130ms out:
	//   lap 1 wakes ~50ms (before the reset)  -> probe: limited
	//   lap 2 wakes ~100ms (still before)     -> probe: limited
	//   lap 3 wakes ~145ms (past reset+grace) -> fallback logged, probe: LIMITED
	//   lap 4 wakes ~195ms on the blind grid  -> must PROBE again, not resume;
	//                                            this lap lifts and resumes.
	h := &fakeAdapter{
		probeResults: []harness.Limit{{Limited: true}, {Limited: true}, {Limited: true}, {Limited: false}},
		probeTokens:  1_000,
	}
	d := New(cfg, h, &fakePoller{})
	openTestStateDir(t, d)
	d.waitGrace = 15 * time.Millisecond

	logs := captureLogs(t)
	start := time.Now()
	tokens, _, stop := d.supervisedWait(context.Background(),
		harness.Limit{Limited: true, ResetAt: start.Add(130 * time.Millisecond)},
		h, d.cfg.EffectivePhases()[0], d.targets[0], spend{start: start})

	if stop {
		t.Fatalf("a failed reset claim falls back to polling; it must not stop the run\n%s", logs.String())
	}
	if h.probeCalls != 4 {
		t.Fatalf("got %d probes after the stated reset passed mid-wait; want 4 - the lap following the failed confirmation must poll again, not resume unprobed:\n%s", h.probeCalls, logs.String())
	}
	if tokens != 4000 {
		t.Errorf("tokens = %d, want 4000 (four probed laps, each a paid session counted)", tokens)
	}
	logged := logs.String()
	if !strings.Contains(logged, "falling back to interval polling") {
		t.Errorf("crossing the stated reset mid-wait must be said out loud:\n%s", logged)
	}
	if strings.Contains(logged, "already past") {
		t.Errorf("a reset the wait itself crossed must never take the entry-time resume shortcut:\n%s", logged)
	}
}

// A STOP marker ends the wait promptly however distant the reset - including
// the precise-wait shape, where the aligned sleep is uncapped by construction
// and would otherwise hold the loop hostage until the reset.
func TestSupervisedWait_StopMarkerEndsTheWaitPromptlyWhateverTheReset(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resetIn  time.Duration
		interval time.Duration
	}{
		// An uncapped precise wait (reset well inside the interval): the exact
		// shape where an uninterruptible multi-hour sleep makes the switch dead.
		{"an uncapped precise wait", 8 * time.Second, time.Hour},
		// And a capped wait of a distant reset - longer than the plant delay, so
		// the marker is found by waitOrStop's own check rather than a lift.
		{"a capped wait of a distant reset", 10 * time.Minute, 300 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fastCfg()
			cfg.PollInterval = config.Duration(tc.interval)
			h := &fakeAdapter{} // never probed: STOP lands first
			d := New(cfg, h, &fakePoller{})
			openTestStateDir(t, d)
			d.waitGrace = time.Second

			stopPath := filepath.Join(d.state.Path(), "STOP")
			go func() {
				time.Sleep(80 * time.Millisecond)
				if err := os.WriteFile(stopPath, []byte("enough"), 0o644); err != nil {
					t.Errorf("planting STOP: %v", err)
				}
			}()

			start := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, _, stop := d.supervisedWait(ctx,
				harness.Limit{Limited: true, ResetAt: start.Add(tc.resetIn)},
				h, d.cfg.EffectivePhases()[0], d.targets[0], spend{start: start})
			elapsed := time.Since(start)

			if !stop {
				t.Errorf("a planted STOP must end the paused wait")
			}
			if h.probeCalls != 0 {
				t.Errorf("STOP should end the wait before another paid probe; got %d", h.probeCalls)
			}
			// Both shapes should end within ~three STOP-check chunks of the plant;
			// five seconds keeps headroom for a loaded -race machine while still
			// proving the switch never waited anything like the full wait.
			if elapsed > 5*time.Second {
				t.Errorf("STOP took %s to end a wait aimed %s out - the stop switch went dead",
					elapsed.Round(time.Millisecond), tc.resetIn)
			}
			if _, err := os.Stat(stopPath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("STOP marker should be consumed by the graceful stop; stat err = %v", err)
			}
		})
	}
}
