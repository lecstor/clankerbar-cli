package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The chain, in one table: the phase's own cap wins, then the run-wide one,
// then nothing at all. The last row is the shipped default and the one that
// would be easiest to break by copying the turn cap's "never resolves to zero"
// rule — the wall-clock cap deliberately DOES, because zero is how it stays off
// (CLA-368).
func TestEffectivePhases_ResolvesTheSessionWallClockChain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runWide Duration
		phase   Duration
		want    time.Duration
	}{
		{name: "the phase's own cap wins", runWide: Duration(time.Hour), phase: Duration(20 * time.Minute), want: 20 * time.Minute},
		{name: "a phase setting none inherits the run-wide cap", runWide: Duration(time.Hour), want: time.Hour},
		{name: "neither set: off", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := defaults()
			c.Prompt = ""
			c.MaxSessionWallClock = tc.runWide
			c.Phases = []Phase{{Name: "implement", MaxWallClock: tc.phase}}

			got := c.EffectivePhases()
			if got[0].MaxWallClock.Duration() != tc.want {
				t.Errorf("resolved to %s, want %s", got[0].MaxWallClock.Duration(), tc.want)
			}
			// Resolution must not mutate the config it read.
			if c.Phases[0].MaxWallClock != tc.phase {
				t.Error("EffectivePhases wrote a resolved cap back into the config")
			}
		})
	}
}

// An unphased run is the case the task named beside the phased one: the
// run-wide dial has to reach a config that declares no phases at all, since
// that is what most opencode operators are running today.
func TestEffectivePhases_TheRunWideCapReachesAnUnphasedRun(t *testing.T) {
	c := defaults()
	c.Prompt = "Work the next backlog item."
	c.MaxSessionWallClock = Duration(45 * time.Minute)

	got := c.EffectivePhases()
	if got[0].MaxWallClock.Duration() != 45*time.Minute {
		t.Errorf("an unphased run resolved to %s, want the run-wide 45m", got[0].MaxWallClock.Duration())
	}
}

// Off by DEFAULT, pinned on the shipped defaults rather than on a hand-built
// Config: the dial arriving switched on would cut real sessions off mid-work
// for every operator who never asked for it.
func TestDefaultsShipTheSessionWallClockCapOff(t *testing.T) {
	c := defaults()
	if c.MaxSessionWallClock != 0 {
		t.Errorf("defaults ship max_session_wall_clock = %s, want it unset (off)", c.MaxSessionWallClock.Duration())
	}
	if got := c.EffectivePhases()[0].MaxWallClock; got != 0 {
		t.Errorf("an untouched config resolved to a cap of %s, want none", got.Duration())
	}
}

// The dial takes the friendly duration strings the rest of the config takes, so
// "30m" in a config file is not silently read as 30 nanoseconds.
func TestSessionWallClockParsesAFriendlyDuration(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"max_session_wall_clock":"30m","phases":[{"name":"implement","max_wall_clock":"90s"}]}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.MaxSessionWallClock.Duration() != 30*time.Minute {
		t.Errorf("max_session_wall_clock = %s, want 30m", c.MaxSessionWallClock.Duration())
	}
	if c.Phases[0].MaxWallClock.Duration() != 90*time.Second {
		t.Errorf("phases[0].max_wall_clock = %s, want 90s", c.Phases[0].MaxWallClock.Duration())
	}
}

// A negative cap would reach exec as an already-expired deadline and kill every
// session the instant it started — a config typo that reads like a broken
// harness. Refused at load, where it reads as the typo it is.
func TestValidate_RefusesANegativeSessionWallClock(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{
			name: "run-wide",
			mut:  func(c *Config) { c.MaxSessionWallClock = Duration(-time.Minute) },
			want: "max_session_wall_clock is negative",
		},
		{
			name: "per phase",
			mut: func(c *Config) {
				c.Prompt = ""
				c.Phases = []Phase{{Name: "implement", MaxWallClock: Duration(-time.Second)}}
			},
			want: "max_wall_clock is negative",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := defaults()
			c.Prompt = "Work the next backlog item."
			tc.mut(c)

			err := c.Validate()
			if err == nil {
				t.Fatal("a negative wall-clock cap was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the dial (%q)", err, tc.want)
			}
		})
	}
}
