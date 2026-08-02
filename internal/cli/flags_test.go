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
			// It must name the spelling that works, not merely complain.
			want := "write --" + strings.TrimPrefix(arg, "-")
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not tell the user to %q", err, want)
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

// TestSingleDashTypoDoesNotSilentlySucceed is the regression test for the
// nastiest failure this migration could have introduced.
//
// pflag treats a lone dash as a bundle of SHORT flags, and if no `-h` is
// registered it reads ANY bundle starting with 'h' as a request for help:
// usage is printed, Parse returns ErrHelp, and the command exits 0 having done
// nothing. For `doctor` that means zero preflight checks ran and `doctor &&
// run` still fired. Go's stdlib `flag` errored on these, so this would have
// been a regression, not an inherited quirk. registerHelp closes it.
func TestSingleDashTypoDoesNotSilentlySucceed(t *testing.T) {
	// Near-misses of a real flag. None is a valid short bundle, so none may be
	// mistaken for --help. (`-hh` is genuinely `-h -h` and is left out: asking
	// for help twice is still asking for help.)
	typos := []string{"-harnes", "-hlp", "-harness2", "-help-me", "-hemp"}

	for _, arg := range typos {
		t.Run("run "+arg, func(t *testing.T) {
			_, _, err := parseRun([]string{arg, "claude"})
			if err == nil {
				t.Fatalf("parsing %q succeeded; a typo must not be accepted", arg)
			}
			if errors.Is(err, pflag.ErrHelp) {
				t.Fatalf("parsing %q was taken as --help, so the command would exit 0 without running", arg)
			}
		})

		t.Run("doctor "+arg, func(t *testing.T) {
			err := Doctor(t.Context(), []string{arg, "claude"})
			if err == nil {
				t.Fatalf("Doctor(%q) returned nil, so it exits 0 having run no checks", arg)
			}
		})
	}
}

// TestPflagAloneWouldMisreadLegacyFlags pins the upstream behaviour the two
// defences above exist for, by driving pflag directly.
//
// If these fail after a pflag upgrade, upstream has started rejecting them
// itself - which is good news: re-read registerHelp and
// rejectSingleDashLongFlags and decide whether they still earn their place.
func TestPflagAloneWouldMisreadLegacyFlags(t *testing.T) {
	t.Run("without -h registered, any -h bundle is taken as a help request", func(t *testing.T) {
		bare := pflag.NewFlagSet("bare", pflag.ContinueOnError)
		bare.SetOutput(&bytes.Buffer{})
		bare.String("harness", "", "")
		if err := bare.Parse([]string{"-harnes", "claude"}); !errors.Is(err, pflag.ErrHelp) {
			t.Errorf("pflag returned %v for -harnes, want ErrHelp (the 'h' is read as -h)", err)
		}
	})

	t.Run("-config is silently taken as -c with the value onfig", func(t *testing.T) {
		var f runFlags
		fs := newRunFlagSet(&f)
		fs.SetOutput(&bytes.Buffer{})
		if fs.ShorthandLookup("c") == nil {
			t.Skip("no -c alias registered; this upstream misread needs one")
		}
		if err := fs.Parse([]string{"-config", "/tmp/cb.json"}); err != nil {
			t.Errorf("pflag returned %v for -config, want it to parse (wrongly) as -c", err)
		}
		if f.cfgPath != "onfig" {
			t.Errorf("pflag parsed -config into cfgPath=%q, want %q", f.cfgPath, "onfig")
		}
	})
}

// A bare `-c` is a real short flag and must keep working, and a value that
// merely looks like a flag must reach the flag it belongs to - the guard is
// targeted, not a blanket ban on single-dash arguments.
func TestGuardDoesNotSwallowRealShortFlags(t *testing.T) {
	if _, _, err := parseRun([]string{"-c", "/tmp/cb.json"}); err != nil {
		t.Errorf("-c should still parse: %v", err)
	}
	if _, _, err := parseRun([]string{"-z"}); err == nil {
		t.Error("unknown short flag -z should fail")
	}

	// A directory genuinely named "-model" is the value of --workdir, not a
	// legacy flag spelling.
	f, _, err := parseRun([]string{"--workdir", "-model"})
	if err != nil {
		t.Errorf("--workdir -model should parse: %v", err)
	}
	if f.workdir != "-model" {
		t.Errorf("workdir = %q, want %q", f.workdir, "-model")
	}

	// Same via the short alias, where the value comes from the next argument.
	if f, _, err := parseRun([]string{"-c", "-harness"}); err != nil || f.cfgPath != "-harness" {
		t.Errorf("-c -harness: cfgPath = %q, err = %v; want the value to reach --config", f.cfgPath, err)
	}
}

// Neither subcommand takes positional arguments, so a leftover is a mistake and
// must be reported rather than dropped. This is the second line of defence
// behind `-config /path`, which pflag would otherwise split into `-c onfig`
// plus a discarded /path.
func TestPositionalArgumentsAreRejected(t *testing.T) {
	for _, args := range [][]string{
		{"stray"},
		{"--harness", "claude", "stray"},
		{"--", "-harness"},
	} {
		_, out, err := parseRun(args)
		if err == nil {
			t.Errorf("parseRun(%q) succeeded; leftover arguments must be reported", args)
			continue
		}
		if !strings.Contains(err.Error(), "unexpected argument") {
			t.Errorf("parseRun(%q) failed with %v, want an unexpected-argument error", args, err)
		}
		if !strings.Contains(out, "Usage: clankerbar run") {
			t.Errorf("parseRun(%q) did not print usage on rejection:\n%s", args, out)
		}
	}
}

// pflag prints nothing at all on a parse error under ContinueOnError, where
// stdlib `flag` printed the message and the whole flag list. Losing the flag
// list exactly when a user mistyped a flag is a real downgrade, so parseFlags
// prints usage itself - including for pflag's own errors, not just the guard's.
func TestUsageIsPrintedOnEveryRejection(t *testing.T) {
	for _, args := range [][]string{
		{"--harnes", "claude"},      // pflag's own unknown-flag error
		{"-harness", "claude"},      // the guard's error
		{"--max-iterations", "abc"}, // a value that will not parse
	} {
		_, out, err := parseRun(args)
		if err == nil {
			t.Errorf("parseRun(%q) unexpectedly succeeded", args)
			continue
		}
		if !strings.Contains(out, "--harness") {
			t.Errorf("parseRun(%q) rejected without showing the flag list:\n%s", args, out)
		}
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
