package harness

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// opencodeParsed runs a stdout stream through exactly the machinery Invoke uses —
// the lineSink that splits it into whole lines, and the incremental parser that
// sums usage as the steps arrive. Production parses live rather than walking a
// saved copy of stdout, because the retained copy is now capped (CLA-262).
func opencodeParsed(stdout string) Result {
	var p opencodeParse
	sink := newLineSink(p.line)
	_, _ = io.WriteString(sink, stdout)
	sink.Flush()

	var res Result
	p.finish(&res)
	return res
}

// A representative single-turn `opencode run --format json` stream, captured from
// opencode 1.18.2. One step: a step-start, the final text part, and a step-finish
// carrying that step's tokens/cost. The parser is defensive, so a non-JSON line is
// interleaved to prove it is skipped.
const opencodeStream = `{"type":"step_start","sessionID":"ses_1","part":{"type":"step-start","messageID":"msg_1"}}
not json, should be skipped
{"type":"text","sessionID":"ses_1","part":{"type":"text","text":"Backlog drained.","time":{"start":1,"end":2}}}
{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"stop","tokens":{"total":18909,"input":3,"output":4,"reasoning":0,"cache":{"write":18902,"read":0}},"cost":0.0236505}}`

func TestOpencodeParse(t *testing.T) {
	res := opencodeParsed(opencodeStream)

	if res.FinalMessage != "Backlog drained." {
		t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, "Backlog drained.")
	}
	// The single step-finish reports total=18909 (it already folds in cache).
	if want := 18909; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d", res.Tokens, want)
	}
	if res.CostUSD != 0.0236505 {
		t.Errorf("CostUSD = %v, want %v", res.CostUSD, 0.0236505)
	}
}

// A multi-turn drain emits several step-finish parts (one per turn) and an
// intermediate text part before the final one. Tokens SUM across turns; the final
// message is the LAST text part.
func TestOpencodeParse_multiStep(t *testing.T) {
	const stream = `{"type":"text","part":{"type":"text","text":"Working on it..."}}
{"type":"step_finish","part":{"type":"step-finish","tokens":{"total":100,"input":40,"output":10,"reasoning":0,"cache":{"write":50,"read":0}},"cost":0.01}}
{"type":"text","part":{"type":"text","text":"Done: opened PR #7."}}
{"type":"step_finish","part":{"type":"step-finish","tokens":{"total":200,"input":80,"output":20,"reasoning":0,"cache":{"write":100,"read":0}},"cost":0.02}}`
	res := opencodeParsed(stream)

	if res.FinalMessage != "Done: opened PR #7." {
		t.Errorf("FinalMessage = %q, want the last text part", res.FinalMessage)
	}
	if want := 100 + 200; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d (summed across steps)", res.Tokens, want)
	}
	if res.CostUSD != 0.03 {
		t.Errorf("CostUSD = %v, want 0.03 (summed across steps)", res.CostUSD)
	}
}

// A step-finish that carries cost but no tokens block must still contribute its
// cost to the budget total (the two fields are independent siblings).
func TestOpencodeParse_costOnlyStep(t *testing.T) {
	const stream = `{"type":"text","part":{"type":"text","text":"ok"}}
{"type":"step_finish","part":{"type":"step-finish","cost":0.05}}`
	res := opencodeParsed(stream)
	if res.CostUSD != 0.05 {
		t.Errorf("CostUSD = %v, want 0.05 (cost counted without a tokens block)", res.CostUSD)
	}
	if res.Tokens != 0 {
		t.Errorf("Tokens = %d, want 0", res.Tokens)
	}
}

func TestOpencodeParse_empty(t *testing.T) {
	res := opencodeParsed("")
	if res.FinalMessage != "" || res.Tokens != 0 {
		t.Errorf("empty stream: got msg=%q tokens=%d, want empty/0", res.FinalMessage, res.Tokens)
	}
}

func TestOpencodeDetectLimit(t *testing.T) {
	// DetectLimit is the HARD budget/credit-exhaustion STOP only, and only from the
	// harness's own diagnostics (an error event, or stderr) — never the agent's
	// narration. An API 429 rate limit is transient (below), not this.
	cases := []struct {
		name string
		res  Result
		want bool
	}{
		{"402 payment required (opencode error event)", Result{Stdout: `{"type":"error","error":{"data":{"message":"Payment Required","statusCode":402,"isRetryable":false}}}`}, true},
		{"out of credits (error event)", Result{Stdout: `{"type":"error","error":{"data":{"message":"Your account is out of credits"}}}`}, true},
		{"insufficient credits (error event)", Result{Stdout: `{"type":"error","error":{"data":{"message":"Insufficient credits to run this request"}}}`}, true},
		{"openrouter credit balance too low (error event)", Result{Stdout: `{"type":"error","error":{"data":{"message":"Your credit balance is too low to access this model"}}}`}, true},
		{"monthly limit reached (stderr diagnostic)", Result{Stderr: "opencode: you have reached your monthly limit"}, true},
		{"spend cap (stderr diagnostic)", Result{Stderr: "spend cap exceeded for this workspace"}, true},
		{"429 is transient not the cap", Result{Stdout: `{"type":"error","error":{"data":{"statusCode":429,"isRetryable":true}}}`}, false},
		{"agent narration about credits is NOT a stop", Result{Stdout: `{"type":"text","part":{"type":"text","text":"The billing note says the account is out of credits and hit its monthly limit."}}`}, false},
		{"clean run", Result{Stdout: opencodeStream}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := (opencode{}).DetectLimit(tc.res)
			if got.Limited != tc.want {
				t.Errorf("DetectLimit.Limited = %v, want %v", got.Limited, tc.want)
			}
			// A recognized budget stop must flag Stop so the loop stops rather than
			// waiting for a reset that never comes.
			if tc.want && !got.Stop {
				t.Errorf("DetectLimit recognized a budget limit but did not set Stop")
			}
		})
	}
}

// IsTransient must scan only the harness-level diagnostic text (opencodeErrorText:
// stderr + {"type":"error"} events), NOT the agent's own {"type":"text"} narration.
// So real provider/transport errors — whether they arrive as an error event or on
// stderr — trip it, but a session that merely *discusses* a rate limit before
// exiting non-zero does not. Cases mark where the signal lives (stdout error event
// vs. stderr) to match how opencode actually emits it.
func TestOpencodeIsTransient(t *testing.T) {
	cases := []struct {
		name string
		res  Result
		want bool
	}{
		{"429 error event", Result{Stdout: `{"type":"error","error":{"data":{"statusCode":429}}}`}, true},
		{"503 error event", Result{Stdout: `{"type":"error","error":{"data":{"statusCode":503}}}`}, true},
		{"isRetryable flag", Result{Stdout: `{"type":"error","error":{"data":{"statusCode":529,"isRetryable":true}}}`}, true},
		{"too many requests on stderr", Result{Stderr: "Error: 429 Too Many Requests"}, true},
		{"connection blip on stderr", Result{Stderr: "fetch failed"}, true},
		{"overloaded on stderr", Result{Stderr: "provider overloaded"}, true},
		{"402 payment required is NOT transient", Result{Stdout: `{"type":"error","error":{"data":{"statusCode":402,"isRetryable":false}}}`}, false},
		{"budget exhaustion is NOT transient", Result{Stderr: "Your account is out of credits"}, false},
		// Regression for finding #2: an assistant text part mentioning a rate limit
		// must NOT be read as a transient error — only opencodeErrorText is scanned.
		{"assistant narration mentioning a rate limit does NOT trip", Result{Stdout: `{"type":"text","text":"Earlier I hit a rate limit and connection error, so I paused before retrying."}`}, false},
		{"clean run", Result{Stdout: opencodeStream}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (opencode{}).IsTransient(tc.res); got != tc.want {
				t.Errorf("IsTransient = %v, want %v", got, tc.want)
			}
		})
	}
}

// Probe must build a READ-ONLY invocation: a trivial prompt and a permission
// policy that denies edits, shell and the exfil tools — zero writes.
func TestOpencodeProbeIsReadOnly(t *testing.T) {
	args := opencodeArgs(Invocation{Probe: true, Prompt: "Work the backlog.", Model: "opencode/claude-haiku-4-5"})
	joined := strings.Join(args, " ")
	// The prompt sits last, behind a `--` terminator (see args_test.go).
	if !strings.HasPrefix(joined, "run --format json") || !strings.HasSuffix(joined, " -- .") {
		t.Errorf("probe args = %q, want a trivial `run --format json ... -- .` request", joined)
	}
	for _, a := range args {
		if a == "Work the backlog." {
			t.Error("probe leaked the real drain prompt into its invocation")
		}
	}

	perm := opencodePermission(true, "/Users/jason/dev") // readOnly
	p := parsePolicy(t, perm)
	for _, tool := range []string{"edit", "bash"} {
		if p[tool] != "deny" {
			t.Errorf("read-only policy: %s = %q, want deny", tool, p[tool])
		}
	}
	assertNetworkDenied(t, p)
	// The path-scoped working set survives the probe shape: reads inside the
	// workdir are allowed, but nothing writes.
	if p["read"] != "Users/jason/dev/**:allow Users/jason/dev:allow" || p["external_directory"] != "*:deny /Users/jason/dev/**:allow" {
		t.Errorf("read-only policy must keep the path-scoped read/external_directory rules, got read=%q external_directory=%q",
			p["read"], p["external_directory"])
	}
}

// A real drain fails closed on exfil but must still allow edits and shell to do
// the work; the prompt is carried through verbatim.
func TestOpencodeDrainInvocation(t *testing.T) {
	args := opencodeArgs(Invocation{Prompt: "Work the backlog.", Model: "opencode/claude-sonnet-4-5"})
	joined := strings.Join(args, " ")
	if !strings.HasPrefix(joined, "run --format json") || !strings.HasSuffix(joined, " -- Work the backlog.") {
		t.Errorf("drain args = %q, want the prompt carried into `opencode run` behind a `--` terminator", joined)
	}
	if !strings.Contains(joined, "--model opencode/claude-sonnet-4-5") {
		t.Errorf("drain args = %q, want the model passed through", joined)
	}

	perm := opencodePermission(false, "/Users/jason/dev") // drain
	p := parsePolicy(t, perm)
	if p["edit"] != "Users/jason/dev/**:allow Users/jason/dev:allow" || p["bash"] != "allow" {
		t.Errorf("drain policy must allow edit (path-scoped) + bash (tool-level), got edit=%q bash=%q", p["edit"], p["bash"])
	}
	assertNetworkDenied(t, p)
}

// The drain policy is a path-scoped PermissionConfig: read/edit/external_directory
// carry pattern->action objects scoping the workdir subtree, `*` is the catch-all
// that denies everything else, and `*_*` keeps the session's MCP access alive
// under that catch-all. Key order is load-bearing (opencode evaluates rules
// last-match-wins), so the catch-all must sort first.
func TestOpencodePermissionPathScoping(t *testing.T) {
	perm := opencodePermission(false, "/Users/jason/dev")
	p := parsePolicy(t, perm)

	want := map[string]string{
		"read":               "Users/jason/dev/**:allow Users/jason/dev:allow",
		"edit":               "Users/jason/dev/**:allow Users/jason/dev:allow",
		"external_directory": "*:deny /Users/jason/dev/**:allow",
	}
	for tool, folded := range want {
		if got := p[tool]; got != folded {
			t.Errorf("%s = %q, want %q", tool, got, folded)
		}
	}
	if p["bash"] != "allow" {
		t.Errorf("bash must stay a tool-level allow (command patterns, not path-based), got %q", p["bash"])
	}
	if p["*_*"] != "allow" {
		t.Errorf("*_* = %q, want allow — MCP tool asks use the full tool name (<server>_<tool>, e.g. clankerbar_get_backlog_summary), which the `*` catch-all would otherwise deny", p["*_*"])
	}
	if p["*"] != "deny" {
		t.Errorf("* = %q, want deny (the catch-all that fails everything else closed)", p["*"])
	}

	// Last-match-wins: the catch-all must sort before every specific rule.
	star := strings.Index(perm, `"*"`)
	for _, key := range []string{"*_*", "bash", "edit", "external_directory", "read", "webfetch", "websearch"} {
		if i := strings.Index(perm, `"`+key+`"`); i == -1 {
			t.Errorf("policy JSON missing key %q", key)
		} else if i < star {
			t.Errorf("key %q sorts BEFORE the `*` catch-all (JSON key order is load-bearing: opencode's evaluator is last-match-wins)", key)
		}
	}
}

// The path patterns derive from the run's workdir: read/edit use opencode's
// root-relative form (path.relative("/", p) when the session's worktree is "/"),
// external_directory uses the absolute form, and a trailing slash / empty workdir
// resolve sensibly.
func TestOpencodeWorkdirPatterns(t *testing.T) {
	rel, abs := opencodeWorkdirPatterns("/Users/jason/dev")
	if rel != "Users/jason/dev/**" || abs != "/Users/jason/dev/**" {
		t.Errorf("patterns for /Users/jason/dev = (%q, %q), want (Users/jason/dev/**, /Users/jason/dev/**)", rel, abs)
	}
	rel, abs = opencodeWorkdirPatterns("/Users/jason/dev/")
	if rel != "Users/jason/dev/**" || abs != "/Users/jason/dev/**" {
		t.Errorf("trailing slash must clean: patterns = (%q, %q)", rel, abs)
	}
	// Empty workdir = the config default (run in the driver's cwd); the policy
	// must still scope to somewhere real.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	rel, abs = opencodeWorkdirPatterns("")
	wantRel := strings.TrimLeft(cwd, "/") + "/**"
	wantAbs := filepath.Clean(cwd) + "/**"
	if rel != wantRel || abs != wantAbs {
		t.Errorf("empty workdir must fall back to the cwd: patterns = (%q, %q), want (%q, %q)", rel, abs, wantRel, wantAbs)
	}
}

// The EFFECTIVE decisions of the emitted policy, evaluated the way opencode
// evaluates them (see opencodeEvaluate): this is the invariant the whole change
// rests on — the working set works, everything else is denied, MCP survives the
// catch-all, and the out-of-workdir external_directory gate stays closed even
// though the `*_*` MCP rule's permission wildcard also matches that name.
func TestOpencodePermissionEffective(t *testing.T) {
	perm := opencodePermission(false, "/Users/jason/dev")
	cases := []struct {
		permission, pattern, want string
	}{
		// The working set: files and worktrees under the workdir, and the
		// workdir root's own listing (exact pattern).
		{"read", "Users/jason/dev/clankerbar-cli-wt/643a681b/internal/harness/harness.go", "allow"},
		{"read", "Users/jason/dev", "allow"},
		{"edit", "Users/jason/dev/clankerbar-cli-wt/643a681b/scratch.txt", "allow"},
		{"external_directory", "/Users/jason/dev/clankerbar-cli-wt/643a681b/*", "allow"},
		// Everything outside the workdir fails closed.
		{"read", "Users/jason/.config/clankerbar/opencode.json", "deny"},
		{"read", "etc/hosts", "deny"},
		{"edit", "etc/passwd", "deny"},
		{"external_directory", "/etc/*", "deny"},
		{"external_directory", "/Users/jason/.config/clankerbar/*", "deny"},
		// bash stays tool-level: commands are allowed, whatever they touch
		// inside the workdir.
		{"bash", "git status --short", "allow"},
		{"bash", "sed -i s/x/y/ internal/harness/opencode.go", "allow"},
		// MCP tools (named <server>_<tool>) survive the catch-all.
		{"clankerbar_get_backlog_summary", "*", "allow"},
		{"context7_query-docs", "*", "allow"},
		{"chrome-devtools_navigate_page", "*", "allow"},
		// The exfil and hidden tools stay denied.
		{"webfetch", "*", "deny"},
		{"websearch", "*", "deny"},
		{"glob", "**/*.go", "deny"},
		{"grep", "TODO", "deny"},
	}
	for _, tc := range cases {
		if got := opencodeEvaluate(t, perm, tc.permission, tc.pattern); got != tc.want {
			t.Errorf("ask(permission=%s, pattern=%q) = %s, want %s", tc.permission, tc.pattern, got, tc.want)
		}
	}
}

// opencodeEvaluate replicates opencode's Permission.evaluate
// (permission/index.ts): flatten the policy document in JSON key order — Go
// marshals map keys sorted, so sorting the keys reproduces the document order —
// then take the LAST rule whose permission name AND pattern both wildcard-match
// the ask; when nothing matches, opencode defaults to "ask". The replica is
// what pins the effective semantics above, which the JSON-shape assertions
// cannot see.
func opencodeEvaluate(t *testing.T, perm, permission, pattern string) string {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(perm), &raw); err != nil {
		t.Fatalf("policy is not valid JSON: %v", err)
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	type rule struct{ permission, pattern, action string }
	var rules []rule
	for _, k := range keys {
		var s string
		if err := json.Unmarshal(raw[k], &s); err == nil {
			rules = append(rules, rule{k, "*", s})
			continue
		}
		var m map[string]string
		if err := json.Unmarshal(raw[k], &m); err != nil {
			t.Fatalf("permission %s is neither an action string nor a pattern map: %s", k, raw[k])
		}
		patterns := make([]string, 0, len(m))
		for p := range m {
			patterns = append(patterns, p)
		}
		sort.Strings(patterns)
		for _, p := range patterns {
			rules = append(rules, rule{k, p, m[p]})
		}
	}
	action := "ask" // opencode's default when no rule matches
	for _, r := range rules {
		if wildcardMatch(permission, r.permission) && wildcardMatch(pattern, r.pattern) {
			action = r.action
		}
	}
	return action
}

// wildcardMatch mirrors opencode's pattern matching (util/wildcard.ts): `*`
// matches any run of characters (including "/"), `?` matches exactly one,
// everything else is literal, anchored at both ends, backslashes normalized.
func wildcardMatch(input, pattern string) bool {
	input = strings.ReplaceAll(input, `\`, `/`)
	pattern = strings.ReplaceAll(pattern, `\`, `/`)
	i, j := 0, 0
	star, mark := -1, 0
	for i < len(input) {
		switch {
		case j < len(pattern) && pattern[j] != '*' && (pattern[j] == '?' || pattern[j] == input[i]):
			i++
			j++
		case j < len(pattern) && pattern[j] == '*':
			star = j
			mark = i
			j++
		case star != -1:
			j = star + 1
			mark++
			i = mark
		default:
			return false
		}
	}
	for j < len(pattern) && pattern[j] == '*' {
		j++
	}
	return j == len(pattern)
}

// parsePolicy folds the emitted policy into a flat map: a tool-level action
// string stays as-is; a pattern->action object folds each rule to
// "pattern:action" (sorted, so the fold is deterministic) and joins them, so
// the whole entry can be compared with ==.
func parsePolicy(t *testing.T, perm string) map[string]string {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(perm), &raw); err != nil {
		t.Fatalf("permission policy is not valid JSON: %v", err)
	}
	out := map[string]string{}
	for key, val := range raw {
		var s string
		if err := json.Unmarshal(val, &s); err == nil {
			out[key] = s
			continue
		}
		var rules map[string]string
		if err := json.Unmarshal(val, &rules); err != nil {
			t.Fatalf("permission %s is neither an action string nor a pattern map: %s", key, val)
		}
		folded := make([]string, 0, len(rules))
		for pattern, action := range rules {
			folded = append(folded, pattern+":"+action)
		}
		sort.Strings(folded)
		out[key] = strings.Join(folded, " ")
	}
	return out
}

func assertNetworkDenied(t *testing.T, p map[string]string) {
	t.Helper()
	for _, tool := range []string{"webfetch", "websearch"} {
		if p[tool] != "deny" {
			t.Errorf("policy must deny exfil tool %s, got %q", tool, p[tool])
		}
	}
}

func TestOpencodeReadUsageUnsupported(t *testing.T) {
	if _, err := (opencode{}).ReadUsage(nil, Invocation{}); err != ErrUsageUnsupported {
		t.Errorf("ReadUsage err = %v, want ErrUsageUnsupported", err)
	}
}

func TestOpencodeRegistered(t *testing.T) {
	a, err := Get("opencode")
	if err != nil {
		t.Fatalf("opencode not registered: %v", err)
	}
	if a.Name() != "opencode" {
		t.Errorf("Get(opencode).Name() = %q", a.Name())
	}
}
