package config

// CLA-899: session_nice is the scheduling-priority dial the drain fleet uses so
// its sessions do not starve a CI runner sharing the machine. These pin the
// three things a silent misparse would cost: the default (unchanged behaviour),
// the accepted range, and the loud refusal where the platform cannot honour it.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/harness"
)

func TestSessionNiceDefaultsToUnchanged(t *testing.T) {
	cfg := defaults()
	if cfg.SessionNice != 0 {
		t.Fatalf("default session_nice = %d, want 0 (the daemon's own priority)", cfg.SessionNice)
	}
	// The config's marshalled form is its identity (see EchoSessions): the knob
	// must not appear for a config that never set it, or every existing daemon's
	// identity changes merely because the field exists.
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "session_nice") {
		t.Errorf("a default config marshals session_nice: %s", b)
	}
}

func TestValidateSessionNice(t *testing.T) {
	base := func() *Config {
		return &Config{Harness: "claude", Prompt: "work", BacklogURL: "https://clankerbar.com", WorkDir: t.TempDir()}
	}
	for _, nice := range []int{0, 1, 10, maxSessionNice} {
		cfg := base()
		cfg.SessionNice = nice
		if err := cfg.Validate(); err != nil {
			t.Errorf("session_nice %d was refused: %v", nice, err)
		}
	}
	for _, nice := range []int{-1, maxSessionNice + 1} {
		cfg := base()
		cfg.SessionNice = nice
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "session_nice") {
			t.Errorf("session_nice %d: error = %v, want a refusal naming session_nice", nice, err)
		}
	}
}

// A positive value on a platform with no nice(1) is REFUSED, not ignored: the
// operator set it because they believe CI is protected, and a silent no-op keeps
// them believing it. On a supported platform the refusal is unreachable, so the
// test pins whichever side it is running on.
func TestValidateSessionNiceOnThisPlatform(t *testing.T) {
	cfg := &Config{Harness: "claude", Prompt: "work", BacklogURL: "https://clankerbar.com", WorkDir: t.TempDir(), SessionNice: 10}
	err := cfg.Validate()
	if harness.SessionPrioritySupported {
		if err != nil {
			t.Fatalf("a supported platform refused session_nice 10: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("error = %v, want the documented refusal on a platform with no nice(1)", err)
	}
}
