package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

// The CLI parses GNU-style options: `--word` long, `-x` short. These tests pin
// that down on BOTH subcommands, and pin down the one deliberate break it cost
// us - see TestLegacySingleDashFlagsAreRejected.

// parseRun runs args through the real `run` flag set, returning the populated
// flags, whatever the set wrote to its output, and the parse error.
func parseRun(args []string) (runFlags, string, error) {
	var f runFlags
	fs := newRunFlagSet(&f)
	var out bytes.Buffer
	fs.SetOutput(&out)
	err := parseFlags(fs, args)
	return f, out.String(), err
}

// parseDoctor is parseRun for the `doctor` subcommand.
func parseDoctor(args []string) (doctorFlags, string, error) {
	var f doctorFlags
	fs := newDoctorFlagSet(&f)
	var out bytes.Buffer
	fs.SetOutput(&out)
	err := parseFlags(fs, args)
	return f, out.String(), err
}

func TestRunParsesGNUStyleLongFlags(t *testing.T) {
	// Both spellings GNU accepts for a long option with a value.
	t.Run("--flag value", func(t *testing.T) {
		f, _, err := parseRun([]string{"--harness", "claude"})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if f.harness != "claude" {
			t.Errorf("harness = %q, want %q", f.harness, "claude")
		}
	})

	t.Run("--flag=value", func(t *testing.T) {
		args := []string{
			"--harness=claude",
			"--config=/tmp/cb.json",
			"--model=opus",
			"--workdir=/tmp/wd",
			"--config-dir=/tmp/cd",
			"--max-iterations=10",
			"--poll-interval=30m",
			"--idle-poll-interval=60s",
		}
		f, _, err := parseRun(args)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		want := runFlags{
			cfgPath:      "/tmp/cb.json",
			harness:      "claude",
			model:        "opus",
			workdir:      "/tmp/wd",
			configDir:    "/tmp/cd",
			maxIter:      10,
			pollInterval: 30 * time.Minute,
			idlePoll:     60 * time.Second,
		}
		if f != want {
			t.Errorf("parsed = %+v, want %+v", f, want)
		}
	})
}

func TestDoctorParsesGNUStyleLongFlags(t *testing.T) {
	f, _, err := parseDoctor([]string{
		"--config", "/tmp/cb.json",
		"--harness", "codex",
		"--workdir", "/tmp/wd",
		"--config-dir", "/tmp/cd",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := doctorFlags{cfgPath: "/tmp/cb.json", harness: "codex", workdir: "/tmp/wd", configDir: "/tmp/cd"}
	if f != want {
		t.Errorf("parsed = %+v, want %+v", f, want)
	}
}

// -c is the one short alias we added. Asserting it on both subcommands keeps
// them from drifting apart.
func TestShortAliasForConfig(t *testing.T) {
	t.Run("run", func(t *testing.T) {
		f, _, err := parseRun([]string{"-c", "/tmp/cb.json"})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if f.cfgPath != "/tmp/cb.json" {
			t.Errorf("cfgPath = %q, want %q", f.cfgPath, "/tmp/cb.json")
		}
	})

	t.Run("doctor", func(t *testing.T) {
		f, _, err := parseDoctor([]string{"-c", "/tmp/cb.json"})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if f.cfgPath != "/tmp/cb.json" {
			t.Errorf("cfgPath = %q, want %q", f.cfgPath, "/tmp/cb.json")
		}
	})
}

// TestLegacySingleDashFlagsAreRejected documents a DELIBERATE breaking change.
//
// Go's stdlib `flag` accepted `-harness` and `--harness` interchangeably. pflag
// does not: a single dash introduces a bundle of SHORT flags, so `-harness` is
// read as `-h -a -r -n -e -s -s` and fails on the first one that isn't defined.
// Taken pre-release (0.0.0-dev) and without a compatibility shim, because the
// error pflag emits says plainly enough what happened.
//
// If this test ever goes red, someone has restored single-dash long options -
// which is a UX decision to make on purpose, not a regression to paper over.
func TestLegacySingleDashFlagsAreRejected(t *testing.T) {
	runLegacy := []string{
		"-config", "-harness", "-model", "-workdir",
		"-config-dir", "-max-iterations", "-poll-interval", "-idle-poll-interval",
	}
	for _, arg := range runLegacy {
		t.Run("run "+arg, func(t *testing.T) {
			f, _, err := parseRun([]string{arg, "claude"})
			if err == nil {
				t.Fatalf("parsing %q succeeded (%+v); single-dash long options are meant to be rejected", arg, f)
			}
			// ErrHelp would mean the command exits 0 having done nothing - the
			// failure mode `doctor && run` cannot see. -harness starts with 'h',
			// so this is exactly what pflag alone would do here.
			if errors.Is(err, pflag.ErrHelp) {
				t.Fatalf("parsing %q returned ErrHelp, so the command would exit 0 instead of erroring", arg)
			}
			// It must name the flag and the spelling that works.
			if !strings.Contains(err.Error(), arg) || !strings.Contains(err.Error(), "-"+arg) {
				t.Errorf("error %q does not name both %s and --%s", err, arg, strings.TrimPrefix(arg, "-"))
			}
		})
	}

	for _, arg := range []string{"-config", "-harness", "-workdir", "-config-dir"} {
		t.Run("doctor "+arg, func(t *testing.T) {
			f, _, err := parseDoctor([]string{arg, "claude"})
			if err == nil {
				t.Fatalf("parsing %q succeeded (%+v); single-dash long options are meant to be rejected", arg, f)
			}
			if errors.Is(err, pflag.ErrHelp) {
				t.Fatalf("parsing %q returned ErrHelp, so the command would exit 0 instead of erroring", arg)
			}
		})
	}

	// `-config=/path` (the =-joined legacy spelling) must fail the same way.
	if _, _, err := parseRun([]string{"-config=/tmp/cb.json"}); err == nil {
		t.Error("parsing -config=/tmp/cb.json succeeded; it is meant to be rejected")
	}

	// The failure has to be legible on its own, since nothing accepts the old
	// form behind it, and usage must show the spelling that does work.
	_, out, err := parseRun([]string{"-harness", "claude"})
	if err == nil {
		t.Fatal("expected -harness to fail")
	}
	if !strings.Contains(out, "--harness") {
		t.Errorf("usage printed on the failure does not show the --harness form:\n%s", out)
	}
}

// TestPflagAloneWouldMisreadLegacyFlags pins the upstream behaviour that
// rejectSingleDashLongFlags exists to cover. It bypasses the guard and drives
// pflag directly.
//
// If this test fails after a pflag upgrade, upstream has started rejecting
// these itself - which is good news: re-read rejectSingleDashLongFlags and
// decide whether it is still earning its place.
func TestPflagAloneWouldMisreadLegacyFlags(t *testing.T) {
	t.Run("-harness is taken as a request for help, not an error", func(t *testing.T) {
		var f runFlags
		fs := newRunFlagSet(&f)
		fs.SetOutput(&bytes.Buffer{})
		err := fs.Parse([]string{"-harness", "claude"})
		if !errors.Is(err, pflag.ErrHelp) {
			t.Errorf("pflag returned %v for -harness, want ErrHelp (the 'h' is read as -h)", err)
		}
	})

	t.Run("-config is silently taken as -c with the value onfig", func(t *testing.T) {
		var f runFlags
		fs := newRunFlagSet(&f)
		fs.SetOutput(&bytes.Buffer{})
		err := fs.Parse([]string{"-config", "/tmp/cb.json"})
		if err != nil {
			t.Errorf("pflag returned %v for -config, want it to parse (wrongly) as -c", err)
		}
		if f.cfgPath != "onfig" {
			t.Errorf("pflag parsed -config into cfgPath=%q, want %q", f.cfgPath, "onfig")
		}
	})
}

// A bare `-c` is a real short flag and must keep working, and a genuinely
// unknown short must still fail - the guard is targeted, not a blanket ban on
// single-dash arguments.
func TestGuardDoesNotSwallowRealShortFlags(t *testing.T) {
	if _, _, err := parseRun([]string{"-c", "/tmp/cb.json"}); err != nil {
		t.Errorf("-c should still parse: %v", err)
	}
	if _, _, err := parseRun([]string{"-z"}); err == nil {
		t.Error("unknown short flag -z should fail")
	}
	// After `--`, arguments are positional and none of this applies.
	if _, _, err := parseRun([]string{"--", "-harness"}); err != nil {
		t.Errorf("arguments after -- should be left alone: %v", err)
	}
}

// TestHelpPrintsDoubleDashUsage covers `-h` and `--help` on both subcommands:
// the help a user reaches for must show the form that actually parses.
func TestHelpPrintsDoubleDashUsage(t *testing.T) {
	cases := []struct {
		name  string
		parse func([]string) (string, error)
		head  string
		flags []string
	}{
		{
			name:  "run",
			parse: func(a []string) (string, error) { _, out, err := parseRun(a); return out, err },
			head:  "Usage: clankerbar run [flags]",
			flags: []string{"--config", "--harness", "--model", "--workdir", "--config-dir", "--max-iterations", "--poll-interval", "--idle-poll-interval"},
		},
		{
			name:  "doctor",
			parse: func(a []string) (string, error) { _, out, err := parseDoctor(a); return out, err },
			head:  "Usage: clankerbar doctor [flags]",
			flags: []string{"--config", "--harness", "--workdir", "--config-dir"},
		},
	}

	for _, tc := range cases {
		for _, helpArg := range []string{"--help", "-h"} {
			t.Run(tc.name+" "+helpArg, func(t *testing.T) {
				out, err := tc.parse([]string{helpArg})
				if !errors.Is(err, pflag.ErrHelp) {
					t.Fatalf("err = %v, want pflag.ErrHelp", err)
				}
				if !strings.Contains(out, tc.head) {
					t.Errorf("usage lost its %q heading:\n%s", tc.head, out)
				}
				for _, f := range tc.flags {
					if !strings.Contains(out, f) {
						t.Errorf("usage does not list %s:\n%s", f, out)
					}
				}
				// The whole point: no flag is advertised in single-dash long form.
				for _, f := range tc.flags {
					if strings.Contains(out, " -"+strings.TrimPrefix(f, "--")+" ") {
						t.Errorf("usage advertises single-dash %s, which no longer parses:\n%s", f, out)
					}
				}
			})
		}
	}
}

// Run and Doctor must surface --help as "handled", not as an error the caller
// logs and exits non-zero on.
func TestHelpIsNotAnError(t *testing.T) {
	if err := Run(t.Context(), []string{"--help"}); err != nil {
		t.Errorf("Run --help returned %v, want nil", err)
	}
	if err := Doctor(t.Context(), []string{"--help"}); err != nil {
		t.Errorf("Doctor --help returned %v, want nil", err)
	}
}
