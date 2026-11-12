package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// load unmarshals a JSON document over an empty Config, so a test pins the
// operator-facing shape rather than a hand-built struct.
func loadIdle(t *testing.T, doc string) *Config {
	t.Helper()
	var c Config
	if err := json.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatalf("unmarshal %s: %v", doc, err)
	}
	return &c
}

// The shipped watchdogs: opencode above its 10-minute bash-tool ceiling,
// claude a shorter idle bound plus a per-call cap, and nothing for a harness
// whose adapter does not enforce the fields.
func TestSessionIdleDefaults(t *testing.T) {
	c := &Config{}
	for _, tc := range []struct {
		harness  string
		idle     time.Duration
		toolCall time.Duration
	}{
		{"opencode", 15 * time.Minute, 0},
		{"claude", 10 * time.Minute, 30 * time.Minute},
		{"codex", 0, 0},
	} {
		idle, toolCall := c.SessionIdleFor(tc.harness)
		if idle != tc.idle || toolCall != tc.toolCall {
			t.Errorf("%s: bounds = %v/%v, want %v/%v", tc.harness, idle, toolCall, tc.idle, tc.toolCall)
		}
	}
	// opencode's idle bound must clear the 10-minute bash-tool ceiling, because
	// opencode emits nothing while a tool runs and the clock never pauses.
	if idle, _ := c.SessionIdleFor("opencode"); idle <= 10*time.Minute {
		t.Errorf("opencode max_idle = %v, want above the 10m bash-tool ceiling", idle)
	}
}

// A block that sets one bound leaves the other at the shipped default.
func TestSessionIdleOverrideKeepsTheOtherDefault(t *testing.T) {
	c := loadIdle(t, `{"session_idle":{"opencode":{"max_idle":"20m"}}}`)
	idle, toolCall := c.SessionIdleFor("opencode")
	if idle != 20*time.Minute {
		t.Errorf("opencode max_idle = %v, want the configured 20m", idle)
	}
	if toolCall != 0 {
		t.Errorf("opencode max_tool_call = %v, want the default 0", toolCall)
	}
}

// An explicit "0" is a real setting (off), not "use the default" — so it has to
// be carried by a pointer.
func TestSessionIdleExplicitZeroDisables(t *testing.T) {
	c := loadIdle(t, `{"session_idle":{"opencode":{"max_idle":"0"}}}`)
	if idle, _ := c.SessionIdleFor("opencode"); idle != 0 {
		t.Errorf("opencode max_idle = %v, want 0 (off)", idle)
	}
}

// A block keyed by a name no adapter answers to is an inert watchdog: refuse it
// against the registry, with the same rename hint as the other per-harness maps.
func TestValidateRefusesUnknownSessionIdleHarness(t *testing.T) {
	c := defaults()
	c.SessionIdle = map[string]IdleBounds{"opencode2": {}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), `session_idle["opencode2"]: unknown harness`) {
		t.Errorf("Validate() = %v, want a refusal naming session_idle[\"opencode2\"]", err)
	}
}

// A negative bound would reach the adapter as an expired timer and kill every
// session at birth, so it is refused rather than clamped.
func TestValidateRefusesNegativeSessionIdle(t *testing.T) {
	for _, doc := range []string{
		`{"session_idle":{"claude":{"max_idle":"-1m"}}}`,
		`{"session_idle":{"claude":{"max_tool_call":"-1m"}}}`,
	} {
		c := defaults()
		overlay := loadIdle(t, doc)
		c.SessionIdle = overlay.SessionIdle
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "is negative") {
			t.Errorf("%s: Validate() = %v, want a negative-bound refusal", doc, err)
		}
	}
}
