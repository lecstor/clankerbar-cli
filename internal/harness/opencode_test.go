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
	return opencodeParsedFrom(Invocation{}, io.Discard, stdout)
}

// opencodeParsedFrom is the same, for the cases that need the Invocation the
// session started from (a resumed phase's seeded claim) or the console a claim
// diagnostic is written to. It builds the parser through the SAME constructor
// Invoke uses, so deleting the seed there turns these red rather than leaving the
// suite green.
func opencodeParsedFrom(in Invocation, console io.Writer, stdout string) Result {
	p := newOpencodeParse(in, console)
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
	if p["read"] != "Users/jason/dev/**:allow Users/jason/dev:allow mcp:*:allow" || p["external_directory"] != "*:deny /Users/jason/dev/**:allow" {
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
		"read":               "Users/jason/dev/**:allow Users/jason/dev:allow mcp:*:allow",
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
		// MCP RESOURCE reads ask under `read`, not under their tool name.
		{"read", "mcp:clankerbar:https://clankerbar.com/skills/clankerbar.md", "allow"},
		{"read", "mcp:clankerbar:*", "allow"},
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

// The served protocol has to be REACHABLE, or an unattended session never learns
// the loop it is supposed to run. CLA-382: every opencode session's
// read_mcp_resource call was denied, so no session read the skill that carries
// the heartbeat cadence, and leases expired mid-work while the session kept
// editing. The mechanism is that opencode's MCP RESOURCE tools do not ask under
// their own names — list_mcp_resources, list_mcp_resource_templates and
// read_mcp_resource all ask under permission `read` with `mcp:<server>:<uri>`
// patterns (1.18.x, session/tools.ts) — so the `*_*` MCP allow never saw them and
// the `*` catch-all denied them. This pins that they resolve to allow in BOTH run
// shapes, while the fail-closed posture the policy exists for is unchanged.
func TestOpencodePermissionAllowsMCPResourceReads(t *testing.T) {
	for _, shape := range []struct {
		name     string
		readOnly bool
	}{{"drain", false}, {"probe", true}} {
		t.Run(shape.name, func(t *testing.T) {
			perm := opencodePermission(shape.readOnly, "/Users/jason/dev")

			// The three asks the resource tools actually make. The read's URI
			// is a URL, so the pattern has to span both "/" and ":".
			allowed := []string{
				"mcp:clankerbar:https://clankerbar.com/skills/clankerbar.md",
				"mcp:clankerbar:https://clankerbar.com/skills/clankerbar/finishing.md",
				"mcp:clankerbar:*", // list_mcp_resources(server: "clankerbar")
				"mcp:context7:*",   // ...and the same listing for any other server
				"mcp:chrome-devtools:*",
			}
			for _, pattern := range allowed {
				if got := opencodeEvaluate(t, perm, "read", pattern); got != "allow" {
					t.Errorf("ask(permission=read, pattern=%q) = %s, want allow — the session cannot read the served protocol", pattern, got)
				}
			}

			// Fail-closed elsewhere: the carve-out is one pattern on `read`,
			// and it moves nothing else. A filesystem read outside the workdir
			// is still denied, and the `mcp:` pattern cannot be reached by a
			// filesystem ask (those carry worktree-relative paths).
			denied := []struct{ permission, pattern string }{
				{"read", "etc/hosts"},
				{"read", "Users/jason/.ssh/id_ed25519"},
				{"read", "Users/jason/.config/clankerbar/opencode.json"},
				{"edit", "mcp:clankerbar:https://clankerbar.com/skills/clankerbar.md"},
				{"external_directory", "/etc/*"},
				{"webfetch", "*"},
				{"websearch", "*"},
				{"glob", "**/*.go"},
				{"grep", "TODO"},
			}
			for _, tc := range denied {
				if got := opencodeEvaluate(t, perm, tc.permission, tc.pattern); got != "deny" {
					t.Errorf("ask(permission=%s, pattern=%q) = %s, want deny — the MCP carve-out must not loosen anything else", tc.permission, tc.pattern, got)
				}
			}

			// The carve-out must not hide the tools either: opencode drops a
			// tool whose mapped permission's LAST matching rule is a `*`-pattern
			// deny (read_mcp_resource maps onto `read`), so the emitted `read`
			// entry must keep ending in a specific pattern, not a bare deny.
			if got := opencodeEvaluate(t, perm, "read", "Users/jason/dev/x.go"); got != "allow" {
				t.Errorf("workdir read = %s, want allow", got)
			}
		})
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
// matches any run of characters (including "/" and ":"), `?` matches exactly one,
// everything else is literal, anchored at both ends, backslashes normalized. The
// one special case: a pattern ending in " *" also matches the input without the
// trailing argument at all ("git status *" matches "git status"), which opencode
// gets by rewriting that suffix to "( .*)?". No pattern this file emits ends that
// way today, but a `bash` command pattern would, and the replica is only worth
// having while it is faithful.
func wildcardMatch(input, pattern string) bool {
	input = strings.ReplaceAll(input, `\`, `/`)
	pattern = strings.ReplaceAll(pattern, `\`, `/`)
	if strings.HasSuffix(pattern, " *") && wildcardMatch(input, strings.TrimSuffix(pattern, " *")) {
		return true
	}
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

// --- Claim observation (CLA-365) -------------------------------------------
//
// The fixture is a RECORDING, not a hand-built stream: two real `opencode run
// --format json` sessions against opencode 1.18.16 and the live clankerbar MCP,
// concatenated (nothing here is session-scoped, and the pair reads as one
// session's traffic). Recording it was step 1 of the task — the question "does
// this stream even carry tool-call arguments and results?" is answered by the
// file, and if a future opencode stops carrying them these tests are where it
// shows. The sessions claimed a throwaway task (CLA-371), tried to claim it a
// second time to capture a REFUSAL verbatim, then parked it with a branch.
//
// Redactions, so a public repo does not carry a backlog dump: the claim result's
// `task.detail`, its `decisions` array and its `decisionsNote` are replaced. The
// envelope — event type, part, state, status, input, output, error — is byte-for-
// byte what opencode emitted, and it is the envelope every assertion here is
// about.
const (
	opencodeFixtureTask   = "e1d01dae-ba13-42ef-9ccc-580a9c6cdc70"
	opencodeFixtureRef    = "CLA-371"
	opencodeFixtureRun    = "6190e2fa-9d24-49e5-a6aa-e402e46a1289"
	opencodeFixtureBranch = "clanker/e1d01dae-temp-fixture-recording"
)

// opencodeRecording returns the recorded session, or the first n of its lines.
// n <= 0 means all of it.
func opencodeRecording(t *testing.T, n int) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "opencode-claim-session.jsonl"))
	if err != nil {
		t.Fatalf("reading the recorded opencode session: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if n > 0 && n < len(lines) {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n") + "\n"
}

// The whole point of the change: a real recorded session's claim reaches
// Result.Claim. Without it Claim.Held() is false for every opencode session, so a
// phase seam hands nothing to the next phase and the driver reads a task it holds
// as one it abandoned.
func TestOpencodeObservesAClaimInARecordedSession(t *testing.T) {
	var console strings.Builder
	res := opencodeParsedFrom(Invocation{}, &console, opencodeRecording(t, 3))

	want := Claim{TaskID: opencodeFixtureTask, Ref: opencodeFixtureRef, RunID: opencodeFixtureRun}
	if res.Claim != want {
		t.Errorf("Claim = %+v, want %+v", res.Claim, want)
	}
	if !res.Claim.Held() {
		t.Error("Claim.Held() is false after a recorded claim_task — every gate downstream (handback, salvage, delivery check) reads this")
	}
	// A claim that silently stops being tracked is indistinguishable from a session
	// that never claimed, so the observation says so out loud.
	if !strings.Contains(console.String(), opencodeFixtureRef) {
		t.Errorf("the console never named the held task; got %q", console.String())
	}
}

// A refused claim carries no ids and must leave the tracked claim exactly as it
// was — which the recording covers, since opencode puts a refusal's payload in
// `state.error` and leaves `state.output` empty.
//
// That alone does NOT pin the status reading, and saying so is the point: with
// the output empty, even a status-blind read hands noteClaimed nothing to parse
// and gives up anyway. TestOpencodeRefusedSettleChangesNothing below is the one
// that fails when the status stops being read as the error flag.
func TestOpencodeRefusedClaimChangesNothing(t *testing.T) {
	afterFirst := opencodeParsedFrom(Invocation{}, io.Discard, opencodeRecording(t, 3)).Claim
	afterRefusal := opencodeParsedFrom(Invocation{}, io.Discard, opencodeRecording(t, 6)).Claim

	if afterRefusal != afterFirst {
		t.Errorf("the refused second claim_task changed the tracked claim: %+v, want it left at %+v", afterRefusal, afterFirst)
	}
}

// opencode has no `is_error` flag: the terminal STATUS is the only thing saying
// whether the plane accepted the call. A refused `update_task` is where reading it
// wrong actually costs something — an `in_review` rejected for a missing Tests
// header leaves the task HELD, and that session is exactly the one whose claim
// needs handing back. Read blind, the driver would instead record the task as let
// go (no handback, no salvage) and keep a delivery Report for a branch the plane
// never recorded.
func TestOpencodeRefusedSettleChangesNothing(t *testing.T) {
	stream := opencodeRecording(t, 3) +
		opencodeToolLine("call-refused", "clankerbar_update_task",
			`{"taskId":"`+opencodeFixtureTask+`","status":"in_review","branch":"`+opencodeFixtureBranch+`"}`,
			"error", `{"error":{"code":"evidence_required","message":"outcome needs a **Tests** section"}}`) + "\n"

	res := opencodeParsedFrom(Invocation{}, io.Discard, stream)

	if res.Claim.Settled {
		t.Error("a REFUSED update_task settled the claim — the driver would read a task the session still holds as one it let go of, and hand nothing back")
	}
	if !res.Claim.Held() {
		t.Errorf("Claim.Held() = false after a refused settle; Claim = %+v", res.Claim)
	}
	if len(res.Reports) != 0 {
		t.Errorf("Reports = %+v, want none: the plane refused the call, so it recorded no branch and there is nothing to check", res.Reports)
	}
}

// A terminal event re-delivered for a callID already acted on must not rebuild a
// claim a later call has since settled — noteClaimed assigns Result.Claim
// wholesale, so a replay would clear Settled and the driver would post `ready`
// over a task in review. Insurance against a stream shape, not an observed bug.
func TestOpencodeIgnoresAReplayedTerminalEvent(t *testing.T) {
	recording := opencodeRecording(t, 0)
	claimEvent := strings.Split(strings.TrimRight(recording, "\n"), "\n")[1]

	res := opencodeParsedFrom(Invocation{}, io.Discard, recording+claimEvent+"\n")

	if !res.Claim.Settled {
		t.Error("a replayed claim_task event resurrected a settled claim; the driver would post `ready` over a task already in review")
	}
}

// The rest of the observation, over the same recording: recording a branch marks
// the claim as carrying work worth handing over, a settling status releases it,
// and the accepted call leaves a delivery Report for the driver to check against
// local git.
func TestOpencodeObservesTheSettleAndTheDeliveryReport(t *testing.T) {
	res := opencodeParsedFrom(Invocation{}, io.Discard, opencodeRecording(t, 0))

	want := Claim{
		TaskID: opencodeFixtureTask, Ref: opencodeFixtureRef, RunID: opencodeFixtureRun,
		HasWIP: true, Settled: true,
	}
	if res.Claim != want {
		t.Errorf("Claim = %+v, want %+v", res.Claim, want)
	}
	if res.Claim.Held() {
		t.Error("Claim.Held() is still true after update_task settled the task — the driver would post `ready` over a task the session had already let go of")
	}
	wantReports := []Report{{
		TaskID: opencodeFixtureTask, Ref: opencodeFixtureRef, RunID: opencodeFixtureRun,
		Status: "parked", Branch: opencodeFixtureBranch,
	}}
	if len(res.Reports) != 1 || res.Reports[0] != wantReports[0] {
		t.Errorf("Reports = %+v, want %+v", res.Reports, wantReports)
	}
	// The recording is also an ordinary session: the usage sum and final message
	// must survive the tool events being parsed alongside them.
	if res.FinalMessage != "DONE" {
		t.Errorf("FinalMessage = %q, want the last text part of the recording", res.FinalMessage)
	}
	if !res.UsageReported || res.Tokens == 0 {
		t.Errorf("the recording's step_finish usage did not reach the Result (UsageReported=%v, Tokens=%d)", res.UsageReported, res.Tokens)
	}
}

// A resumed phase never calls claim_task, so its claim can only come from the
// Invocation. The behavioural payload of the seed is that noteToolUse's
// update_task arm matches — see the claude twin in resume_test.go.
func TestOpencodeSeedsAResumedPhasesClaim(t *testing.T) {
	stream := opencodeToolLine("call-1", "clankerbar_update_task", `{"taskId":"task-abc","branch":"clanker/task-abc-work"}`, "completed", `{}`) + "\n" +
		opencodeToolLine("call-2", "clankerbar_update_task", `{"taskId":"task-abc","status":"in_review","branch":"clanker/task-abc-work","delivery":{"pr":"#42"}}`, "completed", `{}`) + "\n"

	res := opencodeParsedFrom(Invocation{ResumeClaim: Claim{TaskID: "task-abc", RunID: "run-xyz"}}, io.Discard, stream)

	if !res.Claim.HasWIP {
		t.Error("update_task(branch:) did not set HasWIP on the seeded claim — the driver would read the task as safe to release and post it back to the queue over pushed work")
	}
	if !res.Claim.Settled {
		t.Error("update_task(status: in_review) did not settle the seeded claim")
	}
	// Two updates restating the same branch collapse to one report, and the later
	// status wins — the same dedup claude's stream gets.
	want := Report{TaskID: "task-abc", RunID: "run-xyz", Status: "in_review", Branch: "clanker/task-abc-work", PR: "#42"}
	if len(res.Reports) != 1 || res.Reports[0] != want {
		t.Errorf("Reports = %+v, want exactly %+v", res.Reports, want)
	}
}

// opencodeToolLine builds one `tool_use` event in opencode's shape. Hand-built
// only for the cases the recording does not contain — the claim path itself is
// asserted against the recording, never against this.
func opencodeToolLine(callID, tool, input, status, output string) string {
	state := map[string]any{"status": status, "input": json.RawMessage(input)}
	if status == "error" {
		state["error"] = output
	} else {
		state["output"] = output
	}
	b, err := json.Marshal(map[string]any{
		"type": "tool_use",
		"part": map[string]any{"type": "tool", "tool": tool, "callID": callID, "state": state},
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// A call that has not finished carries no result, and its status is the only
// error flag there is — so acting on a non-terminal state would record a claim
// from a call that might yet be refused.
func TestOpencodeIgnoresNonTerminalAndForeignToolEvents(t *testing.T) {
	claimOutput := `{"task":{"id":"t-1","ref":"CLA-1"},"run":{"id":"r-1"}}`
	for _, tt := range []struct{ name, line string }{
		{"still running", opencodeToolLine("c1", "clankerbar_claim_task", `{"taskId":"t-1"}`, "running", claimOutput)},
		{"pending", opencodeToolLine("c1", "clankerbar_claim_task", `{"taskId":"t-1"}`, "pending", claimOutput)},
		{"a status we do not know", opencodeToolLine("c1", "clankerbar_claim_task", `{"taskId":"t-1"}`, "cancelled", claimOutput)},
		{"another server's tool", opencodeToolLine("c1", "context7_claim_task", `{"taskId":"t-1"}`, "completed", claimOutput)},
		{"a built-in of the same name", opencodeToolLine("c1", "claim_task", `{"taskId":"t-1"}`, "completed", claimOutput)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if res := opencodeParsed(tt.line + "\n"); res.Claim != (Claim{}) {
				t.Errorf("Claim = %+v, want none recorded", res.Claim)
			}
		})
	}
}

// One list of watched tools, two namespacings. Adding a tool to the constants in
// claude.go must reach opencode too, which it only does while this mapping holds.
func TestOpencodeClankerbarToolMapsOntoTheWatchedNames(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"clankerbar_claim_task", claimTaskTool},
		{"clankerbar_update_task", updateTaskTool},
		{"clankerbar_ask_question", askQuestionTool},
		{"clankerbar_escalate_question", escalateQuestionTool},
		{"clankerbar_", "mcp__clankerbar__"},
		{"context7_query-docs", ""},
		{"bash", ""},
		{"", ""},
	} {
		if got := opencodeClankerbarTool(tt.in); got != tt.want {
			t.Errorf("opencodeClankerbarTool(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
