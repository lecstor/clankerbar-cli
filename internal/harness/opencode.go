package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

func init() { Register(opencode{}) }

// opencode drives the open, provider-agnostic opencode CLI (`opencode run`). It is
// the adapter that proves the loop isn't Claude-shaped: opencode points at
// OpenRouter / opencode Zen / opencode Go / any provider, and the model choice
// lives in opencode's own config (invisible here). Divergences this adapter
// absorbs, verified against opencode 1.18.2:
//
//   - Headless entry point is `opencode run <message> --format json`, which emits
//     one JSON event per line. There is NO per-run --mcp-config flag: MCP servers,
//     the model, providers and auth all come from opencode's config dir, so
//     ConfigDir → OPENCODE_CONFIG_DIR is the parity of claude's CLAUDE_CONFIG_DIR.
//   - Permissions are set via the OPENCODE_PERMISSION env var (a JSON allow/ask/deny
//     policy), not a run flag. We fail closed: a drain allows edit+bash within the
//     working set but denies the exfil tools (webfetch/websearch/external_directory);
//     a probe / read-only run denies edits and shell too. (We never pass --auto,
//     which blanket-approves everything not explicitly denied.)
//   - Billing depends on the configured backend, not opencode: metered pay-per-token
//     (OpenRouter / Zen) or a monthly-limit subscription (opencode Go). NONE impose a
//     short rolling-window cap, so there is no supervised-wait/early-reset case here.
//     DetectLimit instead recognizes a HARD budget/credit-exhaustion stop (402 /
//     "out of credits" / "monthly limit reached") and flags Limit.Stop so the loop
//     stops cleanly rather than waiting for a reset that never comes. The
//     self-accounted budget breaker is the primary control; ReadUsage is unsupported.
type opencode struct{}

func (opencode) Name() string { return "opencode" }

func (o opencode) Invoke(ctx context.Context, in Invocation) (Result, error) {
	args := opencodeArgs(in)

	cmd := exec.CommandContext(ctx, "opencode", args...)
	if in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	cmd.Env = o.env(in)

	// Capture for parsing, and tee live to the console when one is set (the JSON
	// event stream — a readable renderer is a TODO; raw is honest live output).
	var stdout, stderr bytes.Buffer
	if in.Console != nil && !in.Probe {
		cmd.Stdout = io.MultiWriter(&stdout, in.Console)
		cmd.Stderr = io.MultiWriter(&stderr, in.Console)
	} else {
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
	}
	runErr := cmd.Run()

	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if ee, ok := runErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if runErr != nil {
		return res, runErr // couldn't launch opencode at all
	}
	o.parse(&res)
	return res, nil
}

// opencodeArgs maps an Invocation to `opencode run`'s CLI dialect. Split out so the
// probe's read-only shape is unit-testable without executing anything.
func opencodeArgs(in Invocation) []string {
	prompt := in.Prompt
	if in.Probe {
		// The cheapest possible request: a trivial prompt, tools all denied by the
		// read-only permission policy (see env) — just enough to see whether the
		// provider still answers or is budget-exhausted.
		prompt = "."
	}
	args := []string{"run", prompt, "--format", "json"}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	return args
}

// opencodePermission is the fail-closed OPENCODE_PERMISSION policy. A real drain
// allows edits and shell inside the working set (opencode confines writes to the
// run directory) but denies the network/exfil tools; a read-only run (probe, or
// the connectivity smoke) additionally denies edits and shell — zero writes, just
// enough to reach the clankerbar MCP.
func opencodePermission(readOnly bool) string {
	perm := map[string]string{
		"webfetch":           "deny",
		"websearch":          "deny",
		"external_directory": "deny",
	}
	if readOnly {
		perm["edit"] = "deny"
		perm["bash"] = "deny"
	} else {
		perm["edit"] = "allow"
		perm["bash"] = "allow"
	}
	b, _ := json.Marshal(perm)
	return string(b)
}

func (opencode) env(in Invocation) []string {
	env := os.Environ()
	// Fail-closed permission policy. Set before in.Env so an explicit caller
	// OPENCODE_PERMISSION in in.Env still wins (exec takes the last of a dup key),
	// but the ambient environment never silently loosens an unattended run.
	env = append(env, "OPENCODE_PERMISSION="+opencodePermission(in.Probe))
	// Pin the config dir so a headless session loads the SAME MCP servers,
	// providers, model and auth as the interactive one (the claude/CODEX parity).
	if in.ConfigDir != "" {
		env = append(env, "OPENCODE_CONFIG_DIR="+in.ConfigDir)
	}
	// opencode has no per-run MCP flag; when a caller supplies a config file path
	// (in opencode's own schema, carrying the mcp block) point OPENCODE_CONFIG at
	// it. The Claude-shaped .mcp.json is a different schema and is ignored here.
	if in.MCPConfigPath != "" {
		env = append(env, "OPENCODE_CONFIG="+in.MCPConfigPath)
	}
	env = append(env, in.Env...)
	return env
}

// opencodeEvent is one line of `opencode run --format json`. The stream is a
// sequence of typed events, each wrapping a message part; this captures only the
// fields the loop needs and tolerates the rest.
//
// Success shape (verified against opencode 1.18.2):
//
//	{"type":"step_start","part":{"type":"step-start",...}}
//	{"type":"text","part":{"type":"text","text":"OK",...}}
//	{"type":"step_finish","part":{"type":"step-finish","reason":"stop",
//	   "tokens":{"total":18909,"input":3,"output":4,"reasoning":0,
//	             "cache":{"write":18902,"read":0}},"cost":0.0236505}}
//
// Error shape:
//
//	{"type":"error","error":{"name":"APIError","data":{
//	   "message":"...","statusCode":401,"isRetryable":false,...}}}
type opencodeEvent struct {
	Type string        `json:"type"`
	Part *opencodePart `json:"part"`
}

type opencodePart struct {
	Type   string          `json:"type"`
	Text   string          `json:"text"`
	Reason string          `json:"reason"`
	Tokens *opencodeTokens `json:"tokens"`
	Cost   float64         `json:"cost"`
}

type opencodeTokens struct {
	Total     int `json:"total"`
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Write int `json:"write"`
		Read  int `json:"read"`
	} `json:"cache"`
}

// parse walks the event stream to fill FinalMessage, Tokens and CostUSD.
//
// Tokens/cost are SUMMED across every step-finish part: opencode reports usage
// per step (one step = one LLM turn), not cumulatively, so a multi-turn drain
// emits several step-finish parts that each cover their own turn. FinalMessage is
// the last non-empty text part — the final assistant answer, after any
// intermediate per-step messages.
func (opencode) parse(res *Result) {
	var (
		lastText                              string
		total, in, out, reason, cWrite, cRead int
		cost                                  float64
		sawUsage                              bool
	)
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev opencodeEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue // partial/non-JSON line — skip
		}
		if ev.Part == nil {
			continue
		}
		switch ev.Type {
		case "text":
			if t := strings.TrimSpace(ev.Part.Text); t != "" {
				lastText = t
			}
		case "step_finish":
			// tokens and cost are siblings on the part; count each independently so
			// a step that reports one without the other still lands in the budget.
			if tk := ev.Part.Tokens; tk != nil {
				total += tk.Total
				in += tk.Input
				out += tk.Output
				reason += tk.Reasoning
				cWrite += tk.Cache.Write
				cRead += tk.Cache.Read
				sawUsage = true
			}
			if ev.Part.Cost != 0 {
				cost += ev.Part.Cost
				sawUsage = true
			}
		}
	}
	res.FinalMessage = lastText
	if sawUsage {
		// opencode's `total` already folds in input+output+reasoning+cache, so it
		// is the honest spend figure for the budget.
		res.Tokens = total
		res.CostUSD = cost
		res.Raw = map[string]any{
			"input_tokens": in, "output_tokens": out, "reasoning_tokens": reason,
			"cache_write_tokens": cWrite, "cache_read_tokens": cRead,
		}
	}
}

// opencodeBudgetRe recognizes a HARD budget/credit-exhaustion stop from the
// configured provider: out of credits, spend-cap or monthly-limit reached, or an
// HTTP 402 Payment Required. This is a STOP (Limit.Stop), NOT the rolling-window
// wait case — opencode's backends have no short reset to poll for. A plain 429
// "rate limit" is a transient blip (IsTransient), not this.
var opencodeBudgetRe = regexp.MustCompile(`(?i)"status(code)?": ?402` +
	`|payment required` +
	`|out of (credit|credits|balance)` +
	`|insufficient (credit|credits|fund|funds|balance|quota)` +
	`|(credit|token) balance (is )?(too low|depleted)` +
	`|spend[ -]?(cap|limit)|spending limit` +
	`|monthly (usage )?limit|billing (hard )?limit|usage limit reached`)

func (opencode) DetectLimit(res Result) Limit {
	// Scan only the harness's own diagnostics, NOT the agent's narration: with
	// --format json the assistant's text is a {"type":"text"} part on stdout, so a
	// task that merely discusses billing ("we're out of credits") must not trip a
	// terminal stop. Budget errors arrive as a {"type":"error"} event (or plain
	// text on stderr), which is what opencodeErrorText gathers.
	if opencodeBudgetRe.MatchString(opencodeErrorText(res)) {
		return Limit{Limited: true, Stop: true, Reason: "budget_exhausted"}
	}
	return Limit{}
}

// opencodeErrorText collects the harness-level diagnostic text — all of stderr,
// plus the stdout lines that decode to a {"type":"error"} event — so a limit scan
// sees provider/transport errors but never the agent's own assistant text.
func opencodeErrorText(res Result) string {
	var b strings.Builder
	b.WriteString(res.Stderr)
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type == "error" {
			b.WriteByte('\n')
			b.WriteString(line)
		}
	}
	return b.String()
}

// opencodeTransientRe: retryable server/network blips and API-level rate limits.
// Honours opencode's own "isRetryable":true signal, plus HTTP 408/429/5xx and the
// usual connection strings. The budget-exhaustion stop (DetectLimit) is checked
// first by the loop and carries isRetryable:false, so it never reaches here.
var opencodeTransientRe = regexp.MustCompile(`(?i)"status(code)?": ?(408|429|5\d\d)` +
	`|"isretryable": ?true` +
	`|overloaded|too many requests|rate ?limit` +
	`|connection error|fetch failed|econnreset|econnrefused|etimedout|eai_again|socket hang up|network (error|timeout)`)

func (opencode) IsTransient(res Result) bool {
	// Scope the scan to opencodeErrorText (stderr + {"type":"error"} events), NOT raw
	// Stdout+Stderr: the latter includes {"type":"text"} assistant narration, so a
	// session that merely *discusses* a rate limit / connection error before exiting
	// non-zero would be retried as transient instead of surfacing the real failure —
	// the same reason DetectLimit scopes its scan this way.
	return opencodeTransientRe.MatchString(opencodeErrorText(res))
}

// Diagnostic returns the same scoped text IsTransient judged. See the claude
// adapter's Diagnostic for why the scope must match exactly.
func (opencode) Diagnostic(res Result) string { return opencodeErrorText(res) }

func (o opencode) Probe(ctx context.Context, in Invocation) (ProbeResult, error) {
	in.Probe = true
	res, err := o.Invoke(ctx, in)
	// Spend is taken off the Result on every path — see claude.Probe for why.
	out := ProbeResult{Tokens: res.Tokens, CostUSD: res.CostUSD}
	if err != nil {
		return out, err
	}
	out.Limit = o.DetectLimit(res)
	return out, nil
}

func (opencode) ReadUsage(context.Context, Invocation) (Usage, error) {
	// opencode surfaces no remaining-quota/balance figure to a headless caller;
	// billing lives with the configured provider. The loop falls back to the
	// self-accounted Budget breaker.
	return Usage{}, ErrUsageUnsupported
}
