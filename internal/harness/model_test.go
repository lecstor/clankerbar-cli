package harness

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// No path lets a blank model reach a harness's model flag.
//
// Every adapter guarded on `in.Model != ""`, which is the check a whitespace
// string passes: a `"model": " "` in a config file, or a tier mapped to a space,
// then reaches the child as an alias no provider has and every session of an
// unattended run dies on it. The flag has to be ABSENT for a blank, not present
// with a blank argument, which is why each case below asserts the flag is gone
// rather than that its value is empty.
func TestArgs_ABlankModelEmitsNoModelFlag(t *testing.T) {
	adapters := []struct {
		name string
		args func(Invocation) []string
		flag string
	}{
		{"claude", claudeArgs, "--model"},
		{"codex", codexArgs, "-m"},
		{"opencode", opencodeArgs, "--model"},
	}

	for _, a := range adapters {
		for _, blank := range []string{"", " ", "\t", "  \n "} {
			got := a.args(Invocation{Prompt: "work", Model: blank})
			if i := indexOf(got, a.flag); i >= 0 {
				t.Errorf("%s built %q for model %q: %s must be absent, not carry a blank alias",
					a.name, got, blank, a.flag)
			}
		}
	}
}

// The other half: a real alias still reaches the flag, trimmed. Without this the
// guard above is satisfied by an adapter that never emits the flag at all.
func TestArgs_ARealModelReachesTheFlagTrimmed(t *testing.T) {
	adapters := []struct {
		name string
		args func(Invocation) []string
		flag string
	}{
		{"claude", claudeArgs, "--model"},
		{"codex", codexArgs, "-m"},
		{"opencode", opencodeArgs, "--model"},
	}

	for _, a := range adapters {
		got := a.args(Invocation{Prompt: "work", Model: "  some/model  "})
		i := indexOf(got, a.flag)
		if i < 0 {
			t.Errorf("%s built %q, want it to carry %s", a.name, got, a.flag)
			continue
		}
		if i+1 >= len(got) {
			t.Errorf("%s built %q: %s is last, so it has no value", a.name, got, a.flag)
			continue
		}
		if got[i+1] != "some/model" {
			t.Errorf("%s built %q: %s = %q, want the trimmed alias", a.name, got, a.flag, got[i+1])
		}
	}
}

// The ratchet. The failure to fear is not this file's three adapters, which are
// fixed now, but the FOURTH one: a new harness written by copying an old one from
// before ModelArg existed, reading Model directly and reintroducing the blank
// flag with nothing failing. Derived from the directory rather than a hand-kept
// list, for the same reason.
func TestNoAdapterReadsTheRawModelField(t *testing.T) {
	// `in.Model` but not `in.ModelArg` — the word boundary is what separates them.
	raw := regexp.MustCompile(`\bin\.Model\b`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	swept := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// harness.go declares the field and IS the accessor; everything else in the
		// package must go through it.
		if name == "harness.go" {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		swept++
		if hit := raw.Find(src); hit != nil {
			t.Errorf("%s reads %s directly instead of Invocation.ModelArg() — a blank-but-not-empty alias would reach the child's model flag",
				name, hit)
		}
	}
	// Guard the guard: a sweep that read nothing would pass silently forever.
	if swept < 3 {
		t.Fatalf("the sweep read %d adapter files, which is too few to be reading the package at all", swept)
	}
}
