package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

func init() { Register(opencode2{}) }

// opencode2 drives the OpenCode 2.0 preview CLI (`opencode2`), the nightly line
// that the fleet deliberately does NOT run on (docs/opencode-build.md). The
// adapter exists so the nightly can be evaluated and driven explicitly by name
// without endangering the stable line: the stable adapter hardcodes the binary
// name `opencode`, and this one hardcodes `opencode2`, so the two can never
// silently swap.
//
// THIS IS A THIN ADAPTER, and deliberately so. Verified against
// `opencode2 v0.0.0-dev-17653` (the @opencode-ai/cli nightly):
//
//   - Headless entry: `opencode2 run --standalone --format json [--model m] --
//     <prompt>`. `--standalone` is LOAD-BEARING: opencode2 is client/server and
//     `opencode2 run` otherwise attaches to the shared background server (the
//     `serve --service` daemon) that an interactive session may be using. A
//     standalone run spawns a private server, so a loop's session can never
//     couple itself to an operator's live TUI.
//   - What `--format json` actually prints is DIFFERENT from opencode 1.x: only
//     assistant `text` parts, one JSON per part. There is NO step_finish (no
//     reason/tokens/cost), NO tool_use (no way to observe a claim), NO error
//     events. So this adapter can offer FinalMessage and an exit/stderr
//     classification, and NOTHING else: no budget accounting, no quiet-death
//     detection, no claim tracking. That is a property of the CLI's headless
//     surface, not a missing feature here; a phased run is refused on it
//     (TracksClaims false, exactly like codex).
//   - Config dialect differs from 1.x too: `model` is an OBJECT
//     (`{"providerID": "...", "model": "..."}`) and MCP servers live under
//     `mcp.servers` (verified via `opencode2 debug config`). `OPENCODE_CONFIG`
//     still points at the config file, and a custom `npm:
//     @ai-sdk/openai-compatible` provider block is accepted (the
//     provider-qualified `--model` form works). `OPENCODE_CONFIG_DIR` is NOT
//     relied on: opencode2's config discovery reads hardcoded paths (~/.claude,
//     ~/.agents, ~/.config/opencode), so `config_dir` is deliberately not
//     mapped to an env var here.
//   - Permissions: the V2 permission model is unverified from this headless
//     surface. Nothing fail-closed is exported (no OPENCODE_PERMISSION claim),
//     and doctor reports "no permission-policy checks for opencode2" — honest,
//     because asserting a policy we never saw would be worse than saying so.
//
// The provider ecosystem is shared with the stable opencode, so the
// classification word-patterns (opencodeBudgetRe / opencodeFatalRe /
// opencodeTransientRe) are reused rather than duplicated. They were authored
// against the 1.18 family whose surface changes without notice; against a
// nightly they are best-effort, and the failure direction is SAFE: opencode2
// reports no usage, so IsUnclassifiedTransient stays false and an unrecognised
// failure reads as non-retryable and stops loudly rather than re-spawning paid
// sessions forever.
type opencode2 struct{}

func (opencode2) Name() string { return "opencode2" }

func (opencode2) MCPConfigUse() MCPConfigUse {
	return MCPConfigUse{
		Schema: MCPConfigNative,
		Note: "the opencode2 adapter passes mcp_config_path as OPENCODE_CONFIG, which must be an opencode 2.x " +
			"config (model as an object, servers under `mcp.servers`); its --format json stream does not surface " +
			"tool-use events, so a session's claims cannot be observed and phased runs are refused",
	}
}

// Invoke runs `opencode2 run --standalone --format json`. The per-session
// wall-clock cap is enforced on the exec context, exactly as the opencode
// adapter does: opencode2 takes no turn flag and reports no usage, so elapsed
// time is the only backstop a phase can have here (CLA-368). Never on a probe.
func (o opencode2) Invoke(ctx context.Context, in Invocation) (Result, error) {
	args := opencode2Args(in)

	sctx := ctx
	cancel := context.CancelFunc(func() {})
	if in.MaxSessionWallClock > 0 && !in.Probe {
		sctx, cancel = context.WithTimeout(ctx, in.MaxSessionWallClock)
	}
	defer cancel()

	cmd := exec.CommandContext(sctx, "opencode2", args...)
	if in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	cmd.Env = o.env(in)
	if sctx != ctx {
		// Force-close the capture pipes if a bash-tool grandchild or MCP server
		// keeps them open past the kill — see opencode.Invoke for the why.
		cmd.WaitDelay = 5 * time.Second
	}

	console := in.Console
	if in.Probe {
		console = nil
	}
	p := opencode2Parse{}
	captured := newCapture(p.line)
	captured.attach(cmd, console)
	runErr := cmd.Run()

	res := captured.result("opencode2")
	p.finish(&res)

	timedOut := runErr != nil && sctx.Err() == context.DeadlineExceeded && ctx.Err() == nil
	if timedOut {
		res.markWallClockCapped()
		if console != nil {
			fmt.Fprintf(console, "\n!! session outlived its wall-clock cap (%s) — ending it here; whatever it wrote is still in the worktree, uncommitted\n", in.MaxSessionWallClock)
		}
	}

	if ee, ok := runErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		res.ExitSignal = exitSignal(ee)
	} else if runErr != nil && !timedOut {
		return res, runErr // couldn't launch opencode2 at all
	} else if runErr != nil {
		res.ExitCode = -1
	}
	return res, nil
}

// opencode2Args maps an Invocation to `opencode2 run`'s flag dialect. Split out
// so the probe shape and the `--` terminator are unit-testable without
// executing anything.
func opencode2Args(in Invocation) []string {
	prompt := in.Prompt
	if in.Probe {
		// The cheapest possible request — a probe is a liveness check, not work.
		prompt = "."
	}
	args := []string{"run", "--standalone", "--format", "json"}
	if m := in.ModelArg(); m != "" {
		args = append(args, "--model", m)
	}
	return append(args, "--", prompt)
}

func (opencode2) env(in Invocation) []string {
	env := os.Environ()
	// opencode2's config discovery reads hardcoded paths; OPENCODE_CONFIG is the
	// one knob verified to steer it (the fleet hands the harness its config file
	// the same way). OPENCODE_CONFIG_DIR is deliberately not set: it is not
	// relied on by this adapter (see the type doc).
	if in.MCPConfigPath != "" {
		env = append(env, "OPENCODE_CONFIG="+in.MCPConfigPath)
	}
	env = append(env, in.Env...)
	return env
}

// opencode2Parse consumes the `--format json` stream, which on the verified
// build carries ONLY assistant text parts. The whole of the observation is the
// last non-empty text — the final answer. There is deliberately no usage, no
// reason, no tool tracking: the surface does not emit them.
type opencode2Parse struct {
	lastText string
}

func (p *opencode2Parse) line(line []byte) {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "{") {
		return
	}
	var ev struct {
		Type string `json:"type"`
		Part *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"part"`
	}
	if json.Unmarshal([]byte(trimmed), &ev) != nil {
		return
	}
	// Both the event AND the part must be text: on the verified surface every
	// text event wraps a text part, but gating on the part type too keeps a
	// nightly that reuses the "text" event for a different part shape from
	// being captured as the final answer. Parts REPLACE, they do not append
	// (verified: one part per answer; a nightly streaming token deltas would
	// need concatenation, and the conformance test turns red on it).
	if ev.Type == "text" && ev.Part != nil && ev.Part.Type == "text" {
		if t := strings.TrimSpace(ev.Part.Text); t != "" {
			p.lastText = t
		}
	}
}

func (p *opencode2Parse) finish(res *Result) {
	res.FinalMessage = p.lastText
	// No usage is ever reported on this surface, so UsageReported stays false;
	// the zero-spend bound and the budget cannot account for an opencode2
	// session. The driver's wall-clock cap is the backstop that bounds it.
}

// opencode2ErrorText is the classification evidence: the SAME scoped text the
// stable adapter reads (stderr plus typed error stdout lines, never assistant
// narration), shared rather than duplicated so the memoized scan and any future
// fix to that scoping discipline apply here too. Each Result only ever comes
// from one adapter, so sharing the scan cache cannot collide.
func opencode2ErrorText(res Result) string { return opencodeErrorText(res) }

// The provider ecosystem is the same one the stable opencode talks to, so the
// budget/fatal/transient word-classes are shared with that adapter rather than
// duplicated. They were authored against the 1.18 family; against a nightly
// whose surface changes without notice (CLA-381) they are best-effort. The
// failure direction is safe: with no usage ever reported, an unrecognised exit
// is never "unclassified-transient", so it stops loudly instead of retrying
// forever (see the type doc).

func (opencode2) DetectLimit(res Result) Limit {
	if opencodeBudgetRe.MatchString(opencode2ErrorText(res)) {
		return Limit{Limited: true, Stop: true, Reason: "budget_exhausted"}
	}
	return Limit{}
}

func (opencode2) IsTransient(res Result) bool {
	text := opencode2ErrorText(res)
	if opencodeBudgetRe.MatchString(text) || opencodeFatalRe.MatchString(text) {
		return false
	}
	return opencodeTransientRe.MatchString(text)
}

// IsUnclassifiedTransient is always false here: the opencode heuristic keys on
// reported usage, and opencode2 never reports usage on this surface, so there
// is nothing a heuristic could name. Unrecognised failures stop loudly.
func (opencode2) IsUnclassifiedTransient(Result) bool { return false }

// No turn cap (no turn flag reaches the nightly CLI) and no mid-stream token
// ceiling (no usage is reported to compare a ceiling against).
func (opencode2) TurnCapped(Result) bool      { return false }
func (opencode2) TokenCeilingHit(Result) bool { return false }

// WallClockCapped reuses the shared wall-clock marker (opencode.go): the cap
// is enforced in Invoke on the exec context, and the marker is the adapter's
// own, so the driver can tell a killed-session from a failure.
func (opencode2) WallClockCapped(res Result) bool {
	return res.Raw != nil && res.Raw["terminal_reason"] == wallClockReason
}

// ZeroUsageUnknown is always false: the signature is read off a step_finish
// event, and this surface carries none. A session that exits 0 having produced
// no text is therefore indistinguishable from one that legitimately said
// nothing — a blind spot to state, not to fake: inventing a marker for a shape
// we cannot observe would send the driver down a path for a session that may be
// fine. See docs/opencode2.md.
func (opencode2) ZeroUsageUnknown(Result) bool { return false }

func (opencode2) Capabilities() Capabilities {
	return Capabilities{
		TracksClaims:           false, // no tool events on the stream → a phased run is refused by config.Validate
		HonoursMaxTurns:        false,
		HonoursSessionWallClock: true, // the process kill in Invoke is the phase backstop
		ReportsCost:            false, // no usage/cost event on the stream → budget.max_cost_usd is inert
		HasSessionTokenCeiling: false,
	}
}

func (opencode2) Diagnostic(res Result) string { return opencode2ErrorText(res) }

func (o opencode2) Probe(ctx context.Context, in Invocation) (ProbeResult, error) {
	in.Probe = true
	res, err := o.Invoke(ctx, in)
	out := ProbeResult{Tokens: res.Tokens, CostUSD: res.CostUSD}
	if err != nil {
		return out, err
	}
	return probeVerdict(out, res, o.DetectLimit)
}

func (opencode2) ReadUsage(context.Context, Invocation) (Usage, error) {
	return Usage{}, ErrUsageUnsupported
}

