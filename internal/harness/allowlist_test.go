package harness

// The inherited-env allowlist (CLA-714), asserted END TO END for every adapter:
// the claim is about the environment the spawned harness process actually gets,
// so a unit test of inheritEnv alone would still pass if an adapter forgot to
// apply it.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envDumpStub installs an executable named `name` on PATH that writes its whole
// environment to out and exits cleanly.
func envDumpStub(t *testing.T, name, out string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nenv > " + out + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("writing %s stub: %v", name, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// dumpedEnv reads env(1)'s output into a name -> value map.
func dumpedEnv(t *testing.T, out string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the stub recorded nothing (%v) - it never ran, so nothing below is being tested", err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	return got
}

// Default (no allowlist): byte-identical to today — a parent variable the
// session never declared still reaches it. This is the half a later "filter
// everything" would break silently, so it is pinned for every adapter.
func TestNoAllowlistStillInheritsTheWholeEnvironment(t *testing.T) {
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
			out := filepath.Join(t.TempDir(), "env.txt")
			envDumpStub(t, a.bin, out)
			t.Setenv("CLA714_SENTINEL", "inherited")
			t.Setenv("CLA714_KEPT", "kept")

			_, _ = a.invoke(context.Background(), Invocation{Prompt: "work", WorkDir: t.TempDir(), Console: io.Discard})

			got := dumpedEnv(t, out)
			if got["CLA714_SENTINEL"] != "inherited" {
				t.Errorf("CLA714_SENTINEL = %q, want the daemon's whole environment inherited when no allowlist is declared", got["CLA714_SENTINEL"])
			}
		})
	}
}

// With an allowlist: the sentinel is gone, the listed variable survives, and the
// config's env block is still added — it is an instruction for the session, not
// inherited state.
func TestAllowlistDropsUnlistedInheritedVariablesOnEveryAdapter(t *testing.T) {
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
			out := filepath.Join(t.TempDir(), "env.txt")
			envDumpStub(t, a.bin, out)
			t.Setenv("CLA714_SENTINEL", "leaked")
			t.Setenv("CLA714_KEPT", "kept")

			in := Invocation{
				Prompt:       "work",
				WorkDir:      t.TempDir(),
				Console:      io.Discard,
				EnvAllowlist: []string{"PATH", "HOME", "CLA714_KEPT", "CLANKERBAR_API_KEY"},
				Env:          []string{"GH_TOKEN=declared-token"},
			}
			_, _ = a.invoke(context.Background(), in)

			got := dumpedEnv(t, out)
			if v, ok := got["CLA714_SENTINEL"]; ok {
				t.Errorf("CLA714_SENTINEL = %q reached the session - it is neither on the allowlist nor in the env block", v)
			}
			if got["CLA714_KEPT"] != "kept" {
				t.Errorf("CLA714_KEPT = %q, want the allowlisted value to survive", got["CLA714_KEPT"])
			}
			if got["GH_TOKEN"] != "declared-token" {
				t.Errorf("GH_TOKEN = %q, want the config env block added on top of the allowlist", got["GH_TOKEN"])
			}
			if _, ok := got["PWD"]; !ok {
				t.Error("PWD is missing - the adapter's own pin must survive the filter")
			}
		})
	}
}

// The filter itself: exact names, no values touched, nil/empty = unchanged.
func TestInheritEnv(t *testing.T) {
	env := []string{"HOME=/home/x", "PATH=/bin", "HOME_DIRECTORY=/nope", "NOEQUALS"}

	if got := inheritEnv(env, nil); len(got) != len(env) {
		t.Fatalf("inheritEnv(nil allow) = %v, want the environment untouched", got)
	}
	if got := inheritEnv(env, []string{}); len(got) != len(env) {
		t.Fatalf("inheritEnv(empty allow) = %v, want the environment untouched (full inheritance is the default)", got)
	}

	got := inheritEnv(env, []string{"HOME"})
	if len(got) != 1 || got[0] != "HOME=/home/x" {
		t.Fatalf("inheritEnv(allow HOME) = %v, want only HOME (no prefix match on HOME_DIRECTORY, no entry without '=')", got)
	}
}
