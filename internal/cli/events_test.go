package cli

import "testing"

// The machine-local agent-event opt-out (CLA-603). It is opt-OUT only: the
// project's `events.activity` dial is the only way on, and these tests pin the
// flag and the env spellings that turn it off.

func TestRunParsesNoEvents(t *testing.T) {
	f, _, err := parseRun([]string{"--no-events"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !f.noEvents {
		t.Fatal("--no-events did not parse")
	}

	f, _, err = parseRun(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.noEvents {
		t.Fatal("--no-events defaulted on")
	}
}

func TestEventsEnvOff(t *testing.T) {
	cases := []struct {
		value string
		off   bool
	}{
		{"off", true},
		{"OFF", true},
		{"  off ", true},
		{"0", true},
		{"false", true},
		{"False", true},
		{"on", false},
		{"1", false},
		{"true", false},
		{"", false},
		{"no", false},
		{"offf", false},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("CLANKERBAR_EVENTS", tc.value)
			if got := eventsEnvOff(); got != tc.off {
				t.Errorf("eventsEnvOff() with %q = %v, want %v", tc.value, got, tc.off)
			}
		})
	}
}
