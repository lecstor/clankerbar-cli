package harness

import (
	"io"
	"math"
	"os"
	"path/filepath"
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

// The verified beta-18314 `--format json` surface: a plain text answer (the
// common case) emits ONLY a text event — the provider's usage block is not
// surfaced, no step_finish follows. The parser keeps the LAST non-empty text
// and reports nothing.
func TestOpencode2Parse(t *testing.T) {
	const stream = `{"type":"text","timestamp":1,"sessionID":"s1","part":{"type":"text","text":"Working...","messageID":"m1"}}
not json, skipped
{"type":"text","timestamp":2,"sessionID":"s1","part":{"type":"text","text":"Done.","messageID":"m2"}}`
	res := opencode2Parsed(stream)
	if res.FinalMessage != "Done." {
		t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, "Done.")
	}
	if res.UsageReported {
		t.Error("UsageReported = true, want false — no step_finish in this stream")
	}
}

// The beta's step_finish events (verified on TOOL-CALL turns: reason/cost/
// tokens, no `total` sibling — only input/output/reasoning plus the cache
// pair). A plain text answer emits no step_finish (see TestOpencode2Parse),
// so the parse is defensive: when a step_finish IS present, sum usage across
// steps and record the last reason.
func TestOpencode2Parse_stepFinish(t *testing.T) {
	const stream = `{"type":"step_start","timestamp":1,"sessionID":"s1","part":{"type":"step-start"}}
{"type":"text","timestamp":2,"sessionID":"s1","part":{"type":"text","text":"Working...","messageID":"m1"}}
{"type":"step_finish","timestamp":3,"sessionID":"s1","part":{"type":"step-finish","reason":"stop","cost":0.0001,"tokens":{"input":10,"output":2,"reasoning":0,"cache":{"read":0,"write":0}}}}
{"type":"text","timestamp":4,"sessionID":"s1","part":{"type":"text","text":"Done.","messageID":"m2"}}
{"type":"step_finish","timestamp":5,"sessionID":"s1","part":{"type":"step-finish","reason":"stop","cost":0.0002,"tokens":{"input":5,"output":1,"reasoning":0,"cache":{"read":0,"write":0}}}}`
	res := opencode2Parsed(stream)
	if res.FinalMessage != "Done." {
		t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, "Done.")
	}
	if !res.UsageReported {
		t.Error("UsageReported = false, want true — the beta's step_finish events report usage")
	}
	// total is absent on the beta: input+output+reasoning per step.
	if res.Tokens != 10+2+5+1 {
		t.Errorf("Tokens = %d, want %d (sum of input+output across both steps)", res.Tokens, 18)
	}
	if math.Abs(res.CostUSD-0.0003) > 1e-9 {
		t.Errorf("CostUSD = %v, want %v", res.CostUSD, 0.0003)
	}
	if got := res.Raw[FinishReasonKey]; got != "stop" {
		t.Errorf("Raw[FinishReasonKey] = %v, want %q (the LAST step's reason)", got, "stop")
	}
}

// A step_finish that DOES carry `total` (a future build) wins over the
// input+output+reasoning fallback.
func TestOpencode2Parse_stepFinishTotal(t *testing.T) {
	const stream = `{"type":"step_finish","part":{"type":"step-finish","reason":"stop","tokens":{"total":99,"input":10,"output":2,"reasoning":0}}}`
	res := opencode2Parsed(stream)
	if !res.UsageReported || res.Tokens != 99 {
		t.Errorf("UsageReported=%v Tokens=%d, want true/99 — an explicit total must win", res.UsageReported, res.Tokens)
	}
}

// The #43622 shape (a stream with no finish_reason) does NOT produce a
// quiet-death step_finish on beta-18314: the build emits typed
// provider.invalid-output error events, retries internally, and exits 1
// (verified by the conformance suite). The parse records the events it sees —
// text and any step_finish — and never invents a quiet-death marker: a stream
// that only ever carried text reports no usage and no terminal reason.
func TestOpencode2Parse_quietShapeIsLoud(t *testing.T) {
	const stream = `{"type":"step_start","part":{"type":"step-start"}}
{"type":"text","part":{"type":"text","text":"OK"}}
{"type":"error","error":{"type":"provider.invalid-output","message":"OpenAI Chat stream ended without finish_reason"}}`
	res := opencode2Parsed(stream)
	if res.FinalMessage != "OK" {
		t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, "OK")
	}
	if res.UsageReported {
		t.Error("UsageReported = true — the quiet shape carries no usage and none may be invented")
	}
	if res.Raw[TerminalReasonKey] != nil {
		t.Errorf("Raw[TerminalReasonKey] = %v, want absent — beta-18314 fails loudly, there is no quiet-death marker", res.Raw[TerminalReasonKey])
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
	cfgPath := filepath.Join(t.TempDir(), "opencode.json")
	if err := os.WriteFile(cfgPath, []byte(`{"mcp": "pin"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	in := Invocation{MCPConfigPath: cfgPath, ConfigDir: "/tmp/v2cfg", Env: []string{"XDG_DATA_HOME=/tmp/d"}}
	env, err := (opencode2{}).env(in)
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if !containsEnv(env, "OPENCODE_CONFIG="+cfgPath) {
		t.Errorf("env missing OPENCODE_CONFIG=%s: %v", cfgPath, env)
	}
	// The fail-closed permission policy is exported, the same shape the stable
	// adapter exports. beta-18314 does NOT honor the env var (verified: the
	// headless default without --auto declines every tool call regardless), but
	// the export is belt-and-braces for a build that reads it — and doctor
	// reports exactly this (docs/opencode2.md).
	if !containsEnvPrefix(env, "OPENCODE_PERMISSION=") {
		t.Errorf("env missing OPENCODE_PERMISSION: %v", env)
	}
	if !containsEnv(env, "XDG_DATA_HOME=/tmp/d") {
		t.Errorf("env missing caller XDG var: %v", env)
	}
	// The content pin: the same bytes the v1 adapter pins, because beta-18314
	// merges OPENCODE_CONFIG_CONTENT after every other layer (verified — see
	// the adapter doc), so the driver's file must be the last word. The pin
	// reads the config file and FAILS CLOSED when it cannot.
	if got := envValue(env, "OPENCODE_CONFIG_CONTENT"); got != `{"mcp": "pin"}` {
		t.Errorf("OPENCODE_CONFIG_CONTENT = %q, want the config file's bytes", got)
	}
	// A caller's OPENCODE_PERMISSION still wins (exec takes the last dup key):
	// the same override ordering as the stable adapter.
	in.Env = append(in.Env, "OPENCODE_PERMISSION={\"*\":\"allow\"}")
	env, err = (opencode2{}).env(in)
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if got := envValue(env, "OPENCODE_PERMISSION"); got != `{"*":"allow"}` {
		t.Errorf("caller OPENCODE_PERMISSION must win, got %q", got)
	}
	// config_dir maps to NOTHING: beta-18314's config discovery is hardcoded
	// (~/.claude, <cwd>/.claude, ~/.agents, ~/.config/opencode2, ~/.opencode)
	// and the OPENCODE_CONFIG_DIR-named variable steers only the plugin dir, so
	// the adapter must not pretend to pin config with it (see the adapter doc).
	// Asserted on the DELTA: the adapter inherits whatever the ambient
	// environment carries (an operator's wrapper legitimately sets it), it just
	// never adds it.
	if added := addedEnv(env, os.Environ()); addedEnvHasPrefix(added, "OPENCODE_CONFIG_DIR=") {
		t.Errorf("env sets OPENCODE_CONFIG_DIR — must not: config discovery is hardcoded and config_dir maps to nothing: %v", added)
	}
	// With an empty Invocation the adapter appends nothing of its own beyond
	// what the ambient environment already carries.
	added := addedEnv(env2(t, Invocation{}), os.Environ())
	for _, e := range added {
		if strings.HasPrefix(e, "OPENCODE_CONFIG=") || strings.HasPrefix(e, "OPENCODE_CONFIG_DIR=") || strings.HasPrefix(e, "OPENCODE_CONFIG_CONTENT=") {
			t.Errorf("empty Invocation must not append %q", e)
		}
	}
}

func TestOpencode2EnvFailClosed(t *testing.T) {
	// The content pin reads the config file the driver named; a file that
	// cannot be read must refuse to spawn rather than launch a session whose
	// last config word is ambient (the CLA-441 defect class). Mirror of the
	// stable adapter's fail-closed test.
	_, err := (opencode2{}).env(Invocation{MCPConfigPath: "/nonexistent/opencode2.json"})
	if err == nil {
		t.Fatal("env with an unreadable MCPConfigPath must fail closed")
	}
}

func env2(t *testing.T, in Invocation) []string {
	t.Helper()
	env, err := (opencode2{}).env(in)
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	return env
}

func addedEnvHasPrefix(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// envValue returns the LAST occurrence of key in env — the one exec actually
// uses (a later duplicate key wins).
func envValue(env []string, key string) string {
	prefix := key + "="
	out := ""
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			out = strings.TrimPrefix(e, prefix)
		}
	}
	return out
}

// addedEnv returns the entries of env whose key is absent from baseline — the
// entries the adapter appended rather than inherited.
func addedEnv(env, baseline []string) []string {
	base := map[string]bool{}
	for _, e := range baseline {
		if i := strings.IndexByte(e, '='); i > 0 {
			base[e[:i]] = true
		}
	}
	var out []string
	for _, e := range env {
		key := e
		if i := strings.IndexByte(e, '='); i > 0 {
			key = e[:i]
		}
		if !base[key] {
			out = append(out, e)
		}
	}
	return out
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
	// The CLA-381 heuristic shares the stable adapter's shape: an exit 1 whose
	// cause no pattern names, on a session that REPORTED usage, leans
	// retryable. beta-18314 does NOT report usage for a plain text answer
	// (verified — no step_finish follows it), so the common path stays a stop;
	// the arm is live for a future build that surfaces usage via step_finish.
	cases := []struct {
		name string
		res  Result
		want bool
	}{
		{"exit 0 never transient", Result{ExitCode: 0, UsageReported: true}, false},
		{"exit 1 with no usage stays a stop", Result{ExitCode: 1}, false},
		{"exit 1 with usage leans retryable", Result{ExitCode: 1, UsageReported: true}, true},
		{"recognized budget stop vetoes", Result{ExitCode: 1, UsageReported: true, Stderr: "payment required"}, false},
		{"recognized fatal vetoes", Result{ExitCode: 1, UsageReported: true, Stderr: "error: authentication failed"}, false},
		{"recognized transient pattern wins", Result{ExitCode: 1, UsageReported: true, Stderr: "transport error"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (opencode2{}).IsUnclassifiedTransient(tc.res); got != tc.want {
				t.Errorf("IsUnclassifiedTransient = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOpencode2Capabilities(t *testing.T) {
	c := (opencode2{}).Capabilities()
	if c.TracksClaims {
		t.Error("TracksClaims must be false — tool_use events exist on tool-call turns but the adapter does not consume them for claim tracking, so a phased run must be refused")
	}
	if !c.HonoursSessionWallClock {
		t.Error("HonoursSessionWallClock must be true — Invoke's process-group kill is the phase backstop (no turn flag)")
	}
	if c.HonoursMaxTurns {
		t.Error("HonoursMaxTurns must be false")
	}
	if c.ReportsCost {
		t.Error("ReportsCost must be false — a plain text answer (the common case) emits NO step_finish and the provider's usage block is not surfaced (verified against beta-18314), so budget.max_cost_usd is inert")
	}
	if c.HasSessionTokenCeiling {
		t.Error("HasSessionTokenCeiling must be false")
	}
}

func TestOpencode2ZeroUsageUnknown(t *testing.T) {
	// beta-18314 does not exhibit the quiet-death signature: the #43622 shape
	// exits 1 with a typed provider.invalid-output error event (a LOUD failure,
	// verified by the conformance suite), never a silent exit-0. False is the
	// honest answer — inventing a marker for a shape this build does not
	// produce would mislead the driver (docs/opencode2.md).
	if (opencode2{}).ZeroUsageUnknown(Result{}) {
		t.Error("ZeroUsageUnknown must be false for opencode2")
	}
	if (opencode2{}).ZeroUsageUnknown(opencode2Parsed(`{"type":"step_finish","part":{"type":"step-finish","reason":"unknown"}}`)) {
		t.Error("ZeroUsageUnknown must be false even for an unknown-reason step — the marker is not implemented on this build")
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
