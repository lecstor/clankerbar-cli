package harness

import (
	"io"
	"strings"
	"testing"
)

// opencode2Parsed runs a captured `opencode2 run --format json` stdout stream
// through the same incremental machinery Invoke uses (lineSink + parser).
func opencode2Parsed(stdout string) Result {
	p := opencode2Parse{}
	sink := newLineSink(p.line)
	_, _ = io.WriteString(sink, stdout)
	sink.Flush()
	var res Result
	p.finish(&res)
	return res
}

func TestOpencode2Args(t *testing.T) {
	cases := []struct {
		name string
		in   Invocation
		want []string
	}{
		{"plain", Invocation{Prompt: "hi"}, []string{"run", "--standalone", "--format", "json", "--", "hi"}},
		{"model", Invocation{Prompt: "hi", Model: "fake/fake-model"}, []string{"run", "--standalone", "--format", "json", "--model", "fake/fake-model", "--", "hi"}},
		{"probe stays read-only sized", Invocation{Prompt: "hi", Probe: true}, []string{"run", "--standalone", "--format", "json", "--", "."}},
		{"flag-looking prompt behind the terminator", Invocation{Prompt: "--help"}, []string{"run", "--standalone", "--format", "json", "--", "--help"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := opencode2Args(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("args = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("args = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// The verified `--format json` surface emits ONE JSON per assistant text part —
// no step_finish, no tool_use. The parser keeps the LAST non-empty text.
func TestOpencode2Parse(t *testing.T) {
	const stream = `{"type":"text","timestamp":1,"sessionID":"s1","part":{"type":"text","text":"Working...","messageID":"m1"}}
not json, skipped
{"type":"text","timestamp":2,"sessionID":"s1","part":{"type":"text","text":"Done.","messageID":"m2"}}`
	res := opencode2Parsed(stream)
	if res.FinalMessage != "Done." {
		t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, "Done.")
	}
	if res.UsageReported {
		t.Error("UsageReported = true, want false — opencode2's stream never reports usage")
	}
}

func TestOpencode2Parse_empty(t *testing.T) {
	res := opencode2Parsed("")
	if res.FinalMessage != "" || res.UsageReported {
		t.Errorf("empty stream: msg=%q usageReported=%v", res.FinalMessage, res.UsageReported)
	}
}

// Parts REPLACE, they do not append: the last non-empty text part is the final
// answer. Pinned so a nightly that starts streaming token-delta parts (several
// per answer) is caught as a deliberate decision rather than silently returning
// a partial word — the conformance test would also go red on that day.
func TestOpencode2Parse_partsReplace(t *testing.T) {
	const stream = `{"type":"text","part":{"type":"text","text":"Work","messageID":"m1"}}
{"type":"text","part":{"type":"text","text":"ing...","messageID":"m2"}}
{"type":"text","part":{"type":"text","text":"ing...","messageID":"m3"}}`
	res := opencode2Parsed(stream)
	if res.FinalMessage != "ing..." {
		t.Errorf("FinalMessage = %q, want %q (parts replace: the LAST non-empty text wins)", res.FinalMessage, "ing...")
	}
}

// A "text" event whose part is NOT a text part must not be captured as the
// final answer — the part-type gate (opencode2Parse.line).
func TestOpencode2Parse_ignoresNonTextParts(t *testing.T) {
	const stream = `{"type":"text","part":{"type":"file","text":"peek-a-boo","messageID":"m1"}}
{"type":"text","part":{"type":"text","text":"OK","messageID":"m2"}}`
	res := opencode2Parsed(stream)
	if res.FinalMessage != "OK" {
		t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, "OK")
	}
}

func TestOpencode2Env(t *testing.T) {
	in := Invocation{MCPConfigPath: "/tmp/v2.json", Env: []string{"XDG_DATA_HOME=/tmp/d"}}
	env := (opencode2{}).env(in)
	if !containsEnv(env, "OPENCODE_CONFIG=/tmp/v2.json") {
		t.Errorf("env missing OPENCODE_CONFIG=/tmp/v2.json: %v", env)
	}
	if !containsEnv(env, "XDG_DATA_HOME=/tmp/d") {
		t.Errorf("env missing caller XDG var: %v", env)
	}
	// No ConfigDir mapping: opencode2's config discovery is hardcoded, so it
	// must NOT invent an OPENCODE_CONFIG_DIR knob (see the adapter doc).
	if containsEnvPrefix(env, "OPENCODE_CONFIG_DIR=") {
		t.Errorf("env sets OPENCODE_CONFIG_DIR — must not: %v", env)
	}
	if got := (opencode2{}).env(Invocation{}); containsEnvPrefix(got, "OPENCODE_CONFIG=") {
		t.Errorf("env sets OPENCODE_CONFIG with no MCPConfigPath: %v", got)
	}
}

func TestOpencode2DetectLimit(t *testing.T) {
	// Same budget word-classes as the shared provider ecosystem (opencodeBudgetRe),
	// but evidence scoped to stderr and typed error events — never the assistant
	// narration, which on this surface is the only thing on stdout.
	cases := []struct {
		name string
		res  Result
		want bool
	}{
		{"402 on stderr", Result{Stderr: "payment required"}, true},
		{"out of credits on stderr", Result{Stderr: "Your account is out of credits"}, true},
		{"monthly limit on stderr", Result{Stderr: "monthly limit reached"}, true},
		{"429 rate limit is transient not the cap", Result{Stderr: `"statusCode":429`}, false},
		{"assistant text mentioning credits is NOT a limit", Result{Stdout: `{"type":"text","part":{"type":"text","text":"the account is out of credits"}}`}, false},
		{"clean", Result{Stdout: `{"type":"text","part":{"type":"text","text":"OK"}}`}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := (opencode2{}).DetectLimit(tc.res)
			if got.Limited != tc.want {
				t.Errorf("DetectLimit.Limited = %v, want %v", got.Limited, tc.want)
			}
			if tc.want && !got.Stop {
				t.Error("a recognized budget stop must flag Stop")
			}
		})
	}
}

func TestOpencode2IsTransient(t *testing.T) {
	cases := []struct {
		name string
		res  Result
		want bool
	}{
		{"transport drop on stderr", Result{Stderr: "transport error"}, true},
		{"429 on stderr", Result{Stderr: `"statusCode":429`}, true},
		{"fatal auth vetoes the transient scan", Result{Stderr: "error: authentication failed: transport error"}, false},
		{"assistant text mentioning a rate limit is NOT retryable", Result{Stdout: `{"type":"text","part":{"type":"text","text":"we hit a rate limit"}}`}, false},
		{"clean", Result{Stdout: `{"type":"text","part":{"type":"text","text":"OK"}}`}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (opencode2{}).IsTransient(tc.res); got != tc.want {
				t.Errorf("IsTransient = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOpencode2IsUnclassifiedTransient(t *testing.T) {
	// opencode's heuristic keys on reported usage, and opencode2 never reports
	// usage — so there is no heuristic arm here. An unrecognised non-retryable
	// failure stops loudly instead of re-spawning paid sessions (CLA-381).
	if (opencode2{}).IsUnclassifiedTransient(Result{}) {
		t.Error("IsUnclassifiedTransient must be false for opencode2")
	}
}

func TestOpencode2Capabilities(t *testing.T) {
	c := (opencode2{}).Capabilities()
	if c.TracksClaims {
		t.Error("TracksClaims must be false — the stream carries no tool events, so a phased run must be refused")
	}
	if !c.HonoursSessionWallClock {
		t.Error("HonoursSessionWallClock must be true — Invoke's process kill is the phase backstop (no turn flag, no usage)")
	}
	if c.HonoursMaxTurns {
		t.Error("HonoursMaxTurns must be false")
	}
	if c.ReportsCost {
		t.Error("ReportsCost must be false — no usage/cost event reaches this surface")
	}
	if c.HasSessionTokenCeiling {
		t.Error("HasSessionTokenCeiling must be false")
	}
}

func TestOpencode2ZeroUsageUnknown(t *testing.T) {
	// The quiet-death marker is read off a step_finish event, and this surface
	// has none. False is the HONEST answer: inventing a marker for a shape we
	// cannot observe would mislead the driver (see docs/opencode2.md).
	if (opencode2{}).ZeroUsageUnknown(Result{}) {
		t.Error("ZeroUsageUnknown must be false for opencode2")
	}
}

func TestOpencode2Registered(t *testing.T) {
	// The config validates `harness` against the registry (harness.Known), so
	// `--harness=opencode2` and `harnesses.opencode2` only exist if the adapter
	// registered itself. Dropping the init() registration must turn this red.
	if !Known("opencode2") {
		t.Error("opencode2 is not registered — config will refuse it as a harness")
	}
	a, err := Get("opencode2")
	if err != nil || a.Name() != "opencode2" {
		t.Errorf("Get(opencode2) = %v, %v", a, err)
	}
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

func containsEnvPrefix(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}
