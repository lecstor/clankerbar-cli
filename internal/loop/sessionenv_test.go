package loop

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// --- CLA-462: the daemon composes each session's environment itself ----------

// The composed env must reach the ADAPTER invocation - the thing every adapter
// appends to cmd.Env - with the overlay resolved and sorted. This is the
// end-to-end proof behind "a daemon launched without the wrapper still spawns
// sessions that can push": the stub harness stands where claude/opencode stand,
// and its recorded Invocation.Env is what the child would receive.
func TestInvocationCarriesComposedSessionEnv(t *testing.T) {
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(1, 0)}}}
	cfg := fastCfg()
	cfg.Env = config.EnvMap{
		"GH_TOKEN": config.CommandEnv("token-cmd"),
		"PLAIN":    {Literal: "value"},
	}
	d := New(cfg, h, &fakePoller{})
	openTestStateDir(t, d)

	prev := config.RunEnvCommand
	t.Cleanup(func() { config.RunEnvCommand = prev })
	config.RunEnvCommand = func(command string) (string, error) {
		if command != "token-cmd" {
			t.Errorf("ran %q, want the declared token-cmd", command)
		}
		return "tok-123", nil
	}

	if _, _, stop, err := d.drainWithRetries(context.Background(), 1, d.targets[0], spend{start: time.Now()}); err != nil || stop {
		t.Fatalf("drainWithRetries: stop=%v err=%v", stop, err)
	}
	if len(h.invocations) == 0 {
		t.Fatal("no invocation reached the adapter")
	}
	got := h.invocations[0].Env
	want := []string{"GH_TOKEN=tok-123", "PLAIN=value"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("child env:\n got %v\nwant %v", got, want)
	}
}

// A failing fromCommand REFUSES THE SPAWN: no session starts, and the error
// names the variable. Fail closed - a session without its declared env is the
// incident, not a degraded mode.
func TestFailingFromCommandRefusesTheSpawn(t *testing.T) {
	h := &fakeAdapter{}
	d, _ := phaseDriver(t, h, []config.Phase{{Name: "implement"}})

	prev := config.RunEnvCommand
	t.Cleanup(func() { config.RunEnvCommand = prev })
	config.RunEnvCommand = func(string) (string, error) {
		return "", errors.New("account not logged in")
	}
	d.cfg.Env = config.EnvMap{"GH_TOKEN": config.CommandEnv("gh auth token")}

	_, _, _, _, err := d.drainPhase(context.Background(), 1, 0, "", d.cfg.EffectivePhases()[0], true, nil, d.targets[0], spend{start: time.Now()})
	if err == nil {
		t.Fatal("the phase spawned a session despite a failing declared command")
	}
	for _, want := range []string{"refusing to spawn", "GH_TOKEN", "gh auth token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q: %v", want, err)
		}
	}
	if len(h.invocations) != 0 {
		t.Errorf("%d invocations reached the adapter; want none", len(h.invocations))
	}
}
