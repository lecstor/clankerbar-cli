package cli

// CLA-461: `clankerbar ctl` — the operator-facing half of restart/reload
// control. These tests pin what ctl writes (the loop's own marker constants, so
// cli and loop cannot drift), that it refuses to conjure a state dir into
// existence, and that a pending request is idempotent rather than an error.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/loop"
)

// ctlCfg is a validated config whose state dir lives in a temp dir, mirroring
// validCfg but for ctl: the daemon-side directory ctl must find.
func ctlCfg(t *testing.T) (*config.Config, string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := &config.Config{
		Harness: "claude",
		Prompt:  "Work the backlog.",
		WorkDir: t.TempDir(),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fixture config does not validate: %v", err)
	}
	dir, err := cfg.ResolveStateDir()
	if err != nil {
		t.Fatalf("ResolveStateDir: %v", err)
	}
	return cfg, dir
}

// writeCtlConfig serialises cfg to a temp file, since Ctl loads config from a
// path exactly as run does.
func writeCtlConfig(t *testing.T, cfg *config.Config) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ctl-config.json")
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal fixture config: %v", err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatalf("write fixture config: %v", err)
	}
	return p
}

func TestCtlWritesTheMarkerTheLoopReads(t *testing.T) {
	t.Run("restart", func(t *testing.T) {
		cfg, stateDir := ctlCfg(t)
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		cfgPath := writeCtlConfig(t, cfg)
		if err := Ctl(t.Context(), []string{"restart", "-c", cfgPath}); err != nil {
			t.Fatalf("ctl restart: %v", err)
		}
		b, err := os.ReadFile(filepath.Join(stateDir, "RESTART"))
		if err != nil {
			t.Fatalf("RESTART not written where the daemon reads markers: %v", err)
		}
		if !strings.Contains(string(b), "clankerbar ctl restart") {
			t.Errorf("marker body should name its origin, got %q", string(b))
		}
	})

	t.Run("restart --now", func(t *testing.T) {
		cfg, stateDir := ctlCfg(t)
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		cfgPath := writeCtlConfig(t, cfg)
		if err := Ctl(t.Context(), []string{"restart", "--now", "-c", cfgPath}); err != nil {
			t.Fatalf("ctl restart --now: %v", err)
		}
		if _, err := os.Stat(filepath.Join(stateDir, loop.MarkerRestartNow)); err != nil {
			t.Fatalf("RESTART_NOW not written: %v", err)
		}
		if _, err := os.Stat(filepath.Join(stateDir, loop.MarkerRestart)); !os.IsNotExist(err) {
			t.Errorf("--now must write only RESTART_NOW, not both; RESTART stat err = %v", err)
		}
	})

	t.Run("reload", func(t *testing.T) {
		cfg, stateDir := ctlCfg(t)
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		cfgPath := writeCtlConfig(t, cfg)
		if err := Ctl(t.Context(), []string{"reload", "--config", cfgPath}); err != nil {
			t.Fatalf("ctl reload: %v", err)
		}
		if _, err := os.Stat(filepath.Join(stateDir, loop.MarkerReload)); err != nil {
			t.Fatalf("RELOAD not written: %v", err)
		}
	})
}

// A second identical request is a success with a note, not an error: the
// operator re-running a command out of doubt must get reassurance, and the
// marker format is create-only by design (O_EXCL guards against planted links).
func TestCtlRepeatedRequestIsIdempotent(t *testing.T) {
	cfg, stateDir := ctlCfg(t)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeCtlConfig(t, cfg)
	if err := Ctl(t.Context(), []string{"reload", "-c", cfgPath}); err != nil {
		t.Fatalf("first ctl reload: %v", err)
	}
	if err := Ctl(t.Context(), []string{"reload", "-c", cfgPath}); err != nil {
		t.Fatalf("second ctl reload must succeed: %v", err)
	}
}

// A missing state dir is REFUSED, never created: writing a marker into a fresh
// dir would report success against a daemon that either is not running or was
// launched from a different config - the surprise-restart-on-next-start class.
func TestCtlRefusesAMissingStateDir(t *testing.T) {
	cfg, stateDir := ctlCfg(t)
	cfgPath := writeCtlConfig(t, cfg)
	err := Ctl(t.Context(), []string{"restart", "-c", cfgPath})
	if err == nil {
		t.Fatalf("ctl restart with no state dir at %s must fail", stateDir)
	}
	if _, statErr := os.Lstat(stateDir); !os.IsNotExist(statErr) {
		t.Errorf("the refusal must not have created the state dir; lstat err = %v", statErr)
	}
}

// A daemon launched with `run -c cfg.json --workdir X` derives its state dir
// from X when the config file pins no explicit state_dir - so ctl must accept
// the same override and land the marker where THAT daemon reads it, not where
// the config file alone resolves. Writing there would report success against a
// restart nothing ever performs.
func TestCtlWorkdirOverrideResolvesWhereTheDaemonReads(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	daemonWork := t.TempDir() // the daemon's actual launch override

	fileCfg := &config.Config{
		Harness: "claude",
		Prompt:  "Work the backlog.",
		WorkDir: t.TempDir(), // what the config FILE pins
	}
	cfgPath := func() string {
		if err := fileCfg.Validate(); err != nil {
			t.Fatalf("fixture config does not validate: %v", err)
		}
		return writeCtlConfig(t, fileCfg)
	}()

	// Where a daemon launched with --workdir=daemonWork resolves its markers.
	expected := &config.Config{Harness: "claude", Prompt: "Work the backlog.", WorkDir: daemonWork}
	if err := expected.Validate(); err != nil {
		t.Fatalf("override config does not validate: %v", err)
	}
	daemonStateDir, err := expected.ResolveStateDir()
	if err != nil {
		t.Fatalf("ResolveStateDir: %v", err)
	}
	if err := os.MkdirAll(daemonStateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := Ctl(t.Context(), []string{"restart", "-c", cfgPath, "--workdir", daemonWork}); err != nil {
		t.Fatalf("ctl restart --workdir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(daemonStateDir, loop.MarkerRestart)); err != nil {
		t.Fatalf("the marker must land where the overridden daemon reads it (%s): %v", daemonStateDir, err)
	}
	fileStateDir, err := fileCfg.ResolveStateDir()
	if err != nil {
		t.Fatalf("ResolveStateDir for the un-overridden config: %v", err)
	}
	if fileStateDir == daemonStateDir {
		t.Fatal("fixture broken: both resolutions agree, so this test cannot discriminate")
	}
	if _, err := os.Stat(filepath.Join(fileStateDir, loop.MarkerRestart)); !os.IsNotExist(err) {
		t.Errorf("the config file's own resolution must stay untouched; stat err = %v", err)
	}
}

func TestCtlArgumentErrors(t *testing.T) {
	cfg, stateDir := ctlCfg(t)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeCtlConfig(t, cfg)

	for _, tc := range []struct {
		name, wantErr string
		args          []string
	}{
		{"unknown action", `unknown ctl action "pause"`, []string{"pause", "-c", cfgPath}},
		{"no action", "exactly one action", []string{"-c", cfgPath}},
		{"two actions", "exactly one action", []string{"restart", "reload", "-c", cfgPath}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Ctl(t.Context(), tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("args %v: got err %v, want it to contain %q", tc.args, err, tc.wantErr)
			}
		})
	}

	t.Run("--now with reload", func(t *testing.T) {
		err := Ctl(t.Context(), []string{"reload", "--now", "-c", cfgPath})
		if err == nil || !strings.Contains(err.Error(), "--now applies to restart only") {
			t.Errorf("--now reload: got %v, want the specific rejection", err)
		}
	})
}
