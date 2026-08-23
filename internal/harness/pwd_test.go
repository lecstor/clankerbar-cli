package harness

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLA-441: every adapter sets cmd.Dir and inherits os.Environ(), and the
// inherited PWD names wherever the DAEMON was started. opencode honours $PWD
// over its real cwd - it creates a second instance there and runs the session in
// it - so every session every daemon spawned ran in the daemon's start
// directory, with the wrong project's MCP server, permission asks in a shape the
// policy cannot match, and the wrong repo's AGENTS.md.
//
// The pin is asserted END TO END rather than by calling env(): "the child's PWD
// equals cmd.Dir" is a claim about the process that actually starts, and a unit
// test of the env builder alone would still pass if a future Invoke stopped
// setting cmd.Dir - which is precisely the halves-disagree bug being fixed.

// envProbeStub installs an executable named `name` on PATH that records the two
// facts this is about - what $PWD says, and where the process really is - and
// exits cleanly.
func envProbeStub(t *testing.T, name, out string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"{ echo \"PWD=$PWD\"; echo \"OLDPWD=${OLDPWD-<unset>}\"; echo \"REAL=$(pwd -P)\"; } > " + out + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("writing %s stub: %v", name, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func probed(t *testing.T, out string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the stub recorded nothing (%v) - it never ran, so nothing below is being tested", err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			got[k] = v
		}
	}
	return got
}

// The headline invariant, one subtest per adapter: the child's PWD is the
// workdir the invocation names, which is the same directory cmd.Dir put it in.
func TestEveryAdapterPinsChildPWDToItsWorkDir(t *testing.T) {
	adapters := map[string]struct {
		bin    string
		invoke func(context.Context, Invocation) (Result, error)
	}{
		"opencode": {"opencode", func(ctx context.Context, in Invocation) (Result, error) { return opencode{}.Invoke(ctx, in) }},
		"claude":   {"claude", func(ctx context.Context, in Invocation) (Result, error) { return claude{}.Invoke(ctx, in) }},
		"codex":    {"codex", func(ctx context.Context, in Invocation) (Result, error) { return codex{}.Invoke(ctx, in) }},
	}
	for name, a := range adapters {
		t.Run(name, func(t *testing.T) {
			workdir := t.TempDir()
			out := filepath.Join(t.TempDir(), "env.txt")
			envProbeStub(t, a.bin, out)

			// The daemon's own inherited value: the exact thing that used to
			// travel through untouched and decide where the session ran.
			t.Setenv("PWD", "/somewhere/the/daemon/was/started")
			t.Setenv("OLDPWD", "/somewhere/else")

			// The error is not asserted: these stubs emit no stream, so an
			// adapter may well classify the session as empty. What is being
			// pinned is the environment the child was handed, which the stub
			// has already written by the time Invoke returns either way.
			_, _ = a.invoke(context.Background(), Invocation{Prompt: "work", WorkDir: workdir, Console: io.Discard})

			got := probed(t, out)
			// Resolved on both sides: a macOS temp dir is /var/... while the
			// physical path is /private/var/....
			//
			// A POSIX shell REPAIRS $PWD at startup when the inherited value
			// does not name its cwd, and these stubs are shell scripts, so this
			// half cannot by itself distinguish "the adapter pinned it" from
			// "sh cleaned up after the adapter". opencode is not a shell and
			// does no such repair - which is the whole defect - so the teeth are
			// in TestEachAdaptersEnvPinsPWDBeforeExec below, and this half is
			// what pins that cmd.Dir and the environment name ONE directory
			// end to end.
			if resolvePath(t, got["PWD"]) != resolvePath(t, workdir) {
				t.Errorf("child PWD = %q, want the invocation's workdir %q - opencode creates its instance at $PWD, so an inherited value runs the session in the daemon's directory (CLA-441)", got["PWD"], workdir)
			}
			// ...and it agrees with the real cwd, which is what makes "PWD
			// equals cmd.Dir" a single fact rather than two hopes. Compared
			// through EvalSymlinks because a macOS temp dir is /var/... while
			// `pwd -P` reports /private/var/...
			if got, want := resolvePath(t, got["REAL"]), resolvePath(t, workdir); got != want {
				t.Errorf("child real cwd = %q, want %q - PWD and cmd.Dir must name ONE directory", got, want)
			}
			if got["OLDPWD"] != "<unset>" {
				t.Errorf("OLDPWD = %q, want unset: it means the directory a shell cd'd FROM, and an inherited value describes a jump this child never made", got["OLDPWD"])
			}
		})
	}
}

// The assertion with teeth, one adapter at a time: the environment each adapter
// HANDS TO exec carries the workdir as PWD and no OLDPWD, whatever the daemon
// inherited. Nothing downstream can repair this one - it is read before any
// process starts - so a regression here fails even where a shell would have
// tidied up behind it.
func TestEachAdaptersEnvPinsPWDBeforeExec(t *testing.T) {
	workdir := t.TempDir()
	t.Setenv("PWD", "/somewhere/the/daemon/was/started")
	t.Setenv("OLDPWD", "/somewhere/else")
	in := Invocation{Prompt: "work", WorkDir: workdir}

	opencodeEnv, err := opencode{}.env(in)
	if err != nil {
		t.Fatalf("opencode env: %v", err)
	}
	for name, env := range map[string][]string{
		"opencode": opencodeEnv,
		"claude":   claude{}.env(in),
		"codex":    codex{}.env(in),
	} {
		if got := lastValue(env, "PWD"); got != workdir {
			t.Errorf("%s: PWD handed to exec = %q, want the invocation's workdir %q (CLA-441)", name, got, workdir)
		}
		for _, kv := range env {
			if strings.HasPrefix(kv, "OLDPWD=") {
				t.Errorf("%s: %q survived - OLDPWD means the directory a shell cd'd FROM, and the inherited value describes a jump this child never made", name, kv)
			}
		}
	}
}

func resolvePath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}

// An invocation with no workdir runs in the driver's own cwd, and the pin must
// not invent one: PWD is left exactly as inherited there.
func TestNoWorkDirLeavesPWDAlone(t *testing.T) {
	env := pinPWD([]string{"PWD=/inherited", "OLDPWD=/before", "HOME=/home/x"}, "")
	if len(env) != 3 || env[0] != "PWD=/inherited" || env[1] != "OLDPWD=/before" {
		t.Errorf("pinPWD(env, \"\") = %v, want the environment untouched", env)
	}
}

// A relative workdir must not travel as a relative PWD: whoever reads $PWD has
// no cwd to resolve it against except the child's own, which is the thing being
// pinned.
func TestPinnedPWDIsAbsolute(t *testing.T) {
	env := pinPWD([]string{"PWD=/inherited"}, "some/relative/dir")
	last := env[len(env)-1]
	if !strings.HasPrefix(last, "PWD=/") {
		t.Errorf("pinned %q, want an absolute PWD", last)
	}
	if !strings.HasSuffix(last, filepath.Join("some", "relative", "dir")) {
		t.Errorf("pinned %q, want it to end at the workdir it was given", last)
	}
}

// The pin filters the INHERITED environment only, so a caller that sets PWD
// explicitly still wins on the last-duplicate-wins rule os/exec applies - the
// same precedence every other variable the adapters set already has.
func TestExplicitCallerPWDStillWins(t *testing.T) {
	in := Invocation{WorkDir: t.TempDir(), Env: []string{"PWD=/explicitly/asked/for"}}
	for name, env := range map[string][]string{
		"claude": claude{}.env(in),
		"codex":  codex{}.env(in),
	} {
		if last := lastValue(env, "PWD"); last != "/explicitly/asked/for" {
			t.Errorf("%s: effective PWD = %q, want the caller's explicit value", name, last)
		}
	}
	env, err := opencode{}.env(in)
	if err != nil {
		t.Fatalf("opencode env: %v", err)
	}
	if last := lastValue(env, "PWD"); last != "/explicitly/asked/for" {
		t.Errorf("opencode: effective PWD = %q, want the caller's explicit value", last)
	}
}

// lastValue returns the value os/exec would give key: the LAST occurrence wins.
func lastValue(env []string, key string) string {
	out := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			out = v
		}
	}
	return out
}
