package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

func init() { Register(opencode2{}) }

// opencode2 drives the OpenCode 2.0 preview CLI (`opencode2`), the line the
// fleet deliberately does NOT run on (docs/opencode-build.md). The adapter
// exists so the preview can be evaluated and driven explicitly by name without
// endangering the stable line: the stable adapter hardcodes the binary name
// `opencode`, and this one hardcodes `opencode2`, so the two can never
// silently swap.
//
// THIS IS A THIN ADAPTER, and deliberately so. Verified against the installed
// `opencode2 v0.0.0-beta-18314` (the @opencode-ai/cli preview line) via the
// hermetic conformance suite (docs/opencode2.md):
//
//   - Headless entry: `opencode2 run --standalone --format json [--model m] --
//     <prompt>`. `--standalone` is LOAD-BEARING: opencode2 is client/server and
//     `opencode2 run` otherwise attaches to the shared background server (the
//     `serve --service` daemon) that an interactive session may be using. A
//     standalone run spawns a private server, so a loop's session can never
//     couple itself to an operator's live TUI (the session record shows the
//     private `serve --stdio --port 0` child it spawns).
//   - What `--format json` actually prints is DIFFERENT from opencode 1.x, and
//     the usable surface is NARROWER than the raw event stream suggests. The
//     beta emits `text`, `step_start`, `tool_use`, `step_finish` and typed
//     `error` events — but a plain text answer (the common drain case) emits
//     ONLY the `text` event: the provider's usage block is NOT surfaced
//     (verified: the fake's 18511+2 usage never appeared on the stream), so
//     `UsageReported` stays false and budget accounting is inert
//     (ReportsCost false). `tool_use`/`step_finish` appear on tool-call
//     turns, but the adapter never gets to use tools: see permissions below.
//   - Config dialect differs from 1.x too: `model` is an OBJECT
//     (`{"providerID": "...", "model": "..."}`) and MCP servers live under
//     `mcp.servers` (the operator's own v2 config and `opencode2 debug config`
//     corroborate it). `OPENCODE_CONFIG` points at the config file, a custom
//     `npm: @ai-sdk/openai-compatible` provider block is accepted, and the
//     provider-qualified `--model` form works. `OPENCODE_CONFIG_DIR` is NOT
//     set by this adapter: it steers the PLUGIN dir (verified: a plugin under
//     `$OPENCODE_CONFIG_DIR/plugins` loads, the operator's wrapper relies on
//     this), but config-file discovery is HARDCODED (~/.claude, ~/.agents,
//     ~/.config/opencode2, ~/.opencode — XDG_CONFIG_HOME does not move them),
//     so mapping `config_dir` to it would redirect plugins without pinning any
//     config. The fleet's config file travels via `OPENCODE_CONFIG`, exactly
//     as the stable adapter hands it.
//   - Permissions: the adapter exports the SAME fail-closed OPENCODE_PERMISSION
//     policy the stable adapter exports (one policy to maintain, one posture
//     for both lines). VERIFIED caveat: beta-18314 does NOT honor the env var —
//     with `--auto` + `{"*": "deny"}` the write tool still executed, and with
//     no env var at all the same write was declined; the `permission` CONFIG
//     block is not honored either. The fail-closed property of an unattended
//     opencode2 run comes from the HEADLESS DEFAULT: without `--auto`, every
//     tool call is declined ("The user declined this tool call"), and this
//     adapter never passes `--auto`. The exported policy is belt-and-braces
//     for a future build that honors it; doctor says exactly this.
//
// The provider ecosystem is shared with the stable opencode, so the
// classification word-patterns (opencodeBudgetRe / opencodeFatalRe /
// opencodeTransientRe) are reused rather than duplicated. They were authored
// against the 1.18 family whose surface changes without notice; against the
// beta they are best-effort, and the failure direction is SAFE: an
// unrecognised failure reads as non-retryable and stops loudly rather than
// re-spawning paid sessions forever.
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
// adapter does: opencode2 takes no turn flag, so elapsed time is the backstop
// a phase can have against a runaway session (CLA-368). Never on a probe.
func (o opencode2) Invoke(ctx context.Context, in Invocation) (Result, error) {
	args := opencode2Args(in)

	sctx := ctx
	cancel := context.CancelFunc(func() {})
	if in.MaxSessionWallClock > 0 && !in.Probe {
		sctx, cancel = context.WithTimeout(ctx, in.MaxSessionWallClock)
	}
	defer cancel()

	cmd := exec.CommandContext(sctx, "opencode2", args...)
	setupProcessGroup(cmd)
	if in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	cmd.Env = o.env(in)
	// A cap that can hang is not a cap. The capture points Stdout/Stderr at
	// io.MultiWriter values, so os/exec makes its own pipes and cmd.Run waits
	// for every WRITER of them to close — and CommandContext kills the direct
	// child only. A runaway session is exactly the case with a bash-tool
	// grandchild (a build, a test run) or an MCP server holding the inherited
	// fd, which survives the SIGKILL and keeps the pipe open. WaitDelay
	// force-closes the pipes and lets Wait return. Set whenever the exec
	// context carries ANY deadline — one this function created, or one a caller
	// handed in: an fd holder would otherwise keep the pipe open past the kill
	// and hang Run past its own box. An ordinary uncapped session's I/O is
	// never cut short by it (CLA-374, ported from the stable adapter).
	if _, hasDeadline := sctx.Deadline(); hasDeadline {
		cmd.WaitDelay = 5 * time.Second
	}

	// Kill the whole process group on ANY cancellation of sctx — the wall-clock
	// cap, or the caller's own cancellation. That group kill is opencode2's
	// ONLY backstop: the standalone server child (and any bash-tool
	// grandchild) survives CommandContext's direct-child kill, and this is the
	// same killProcessGroup the stable adapter uses (CLA-374). The direct
	// child is killed too (the trailing Kill mirrors CommandContext's own
	// default Cancel), and a Kill on an already-dead child reads as
	// os.ErrProcessDone, which os/exec treats as nothing to report.
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return cmd.Process.Kill()
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
	} else if errors.Is(runErr, exec.ErrWaitDelay) && !timedOut {
		// The process exited cleanly on its own — ExitCode 0 — but a grandchild
		// (the standalone server, a backgrounded build) inherited the output
		// pipes and held them open past cmd.WaitDelay, so os/exec force-closed
		// them and Run returned exec.ErrWaitDelay instead of an *exec.ExitError.
		// p.finish has already run, and every event the session emitted was
		// consumed by the live parse as it arrived — only grandchild bytes
		// written after the parent's exit are lost, and those were never this
		// session's output. This is a clean end, not a failure: returning it as
		// an error failed a completed attempt with no retry classification
		// (CLA-414). The same holds for a run-wide cancellation landing inside
		// the window: sctx reads Canceled, not DeadlineExceeded, so !timedOut is
		// true and the attempt ends "clean" during teardown. Nothing durable
		// turns on it.
		return res, nil
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
	// Fail-closed permission policy, the SAME shape the stable adapter exports
	// (one policy to maintain, one posture for both lines). Set before in.Env
	// so an explicit caller OPENCODE_PERMISSION in in.Env still wins (exec
	// takes the last of a dup key), but the ambient environment never silently
	// loosens an unattended run. VERIFIED caveat (beta-18314): the env var is
	// NOT honored by this build — the headless default without --auto declines
	// every tool call, and the adapter never passes --auto, which is the real
	// fail-closed enforcement. The export is belt-and-braces for a future build
	// that reads it; doctor says exactly this (see the type doc).
	env = append(env, "OPENCODE_PERMISSION="+opencodePermission(in.Probe, in.WorkDir, in.ExtraDirs))
	// OPENCODE_CONFIG_DIR is deliberately NOT set: beta-18314's config-file
	// discovery is hardcoded (~/.claude, ~/.agents, ~/.config/opencode2,
	// ~/.opencode — XDG_CONFIG_HOME does not move them), so `config_dir` maps
	// to nothing. The variable that IS named that way steers the PLUGIN dir
	// (verified), and redirecting plugins to the fleet's config dir would
	// silently detach the operator's plugin setup. The config file the fleet
	// hands over travels via OPENCODE_CONFIG, like the stable adapter.
	if in.MCPConfigPath != "" {
		env = append(env, "OPENCODE_CONFIG="+in.MCPConfigPath)
	}
	env = append(env, in.Env...)
	return env
}

// opencode2Parse consumes the `--format json` stream. On the verified
// beta-18314 build the stream carries `text`, `step_start`, `tool_use`,
// `step_finish` and typed `error` events — but the COMMON case, a plain text
// answer, emits ONLY the `text` event (verified: a provider usage block in the
// API response is not surfaced; no step_finish follows a plain answer). The
// observation is therefore the last non-empty text (the final answer) plus —
// when a tool-call turn actually emits step_finish — whatever usage it carries
// (the beta reports per-step input/output/reasoning, no `total` sibling).
// Typed error events are NOT parsed here: the shared opencodeErrorText already
// collects {"type":"error"} stdout lines for classification.
type opencode2Parse struct {
	lastText string
	// lastReason is the reason of the MOST RECENT step_finish — the session's
	// final step, once the stream ends. Recorded for diagnostics; the adapter
	// does not classify on it (no quiet-death marker — ZeroUsageUnknown stays
	// false, see docs/opencode2.md).
	lastReason string
	// tokens and cost are SUMMED across every step_finish: when the beta
	// emits them it reports usage per step (one step = one LLM turn), not
	// cumulatively, so a multi-turn drain emits several step-finish parts that
	// each cover their own turn. sawUsage is the presence of the REPORT, not
	// its size — a step_finish carrying zeros still reported (CLA-288).
	tokens   int
	cost     float64
	sawUsage bool
}

func (p *opencode2Parse) line(line []byte) {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "{") {
		return
	}
	var ev struct {
		Type string `json:"type"`
		Part *struct {
			Type   string  `json:"type"`
			Text   string  `json:"text"`
			Reason string  `json:"reason"`
			Tokens *struct {
				Total     int `json:"total"`
				Input     int `json:"input"`
				Output    int `json:"output"`
				Reasoning int `json:"reasoning"`
			} `json:"tokens"`
			Cost *float64 `json:"cost"`
		} `json:"part"`
	}
	if json.Unmarshal([]byte(trimmed), &ev) != nil {
		return
	}
	switch ev.Type {
	case "text":
		// Both the event AND the part must be text: on the verified surface every
		// text event wraps a text part, but gating on the part type too keeps a
		// build that reuses the "text" event for a different part shape from
		// being captured as the final answer. Parts REPLACE, they do not append
		// (verified: one part per answer; a build streaming token deltas would
		// need concatenation, and the conformance test turns red on it).
		if ev.Part != nil && ev.Part.Type == "text" {
			if t := strings.TrimSpace(ev.Part.Text); t != "" {
				p.lastText = t
			}
		}
	case "step_finish":
		// Emitted on tool-call turns (and possibly future builds' plain turns);
		// NOT emitted after a plain text answer on beta-18314 (verified). The
		// parse is defensive: sum whatever a step_finish reports, and let the
		// conformance suite pin whether a given build surfaces it.
		if ev.Part == nil {
			return
		}
		if ev.Part.Reason != "" {
			p.lastReason = ev.Part.Reason
		}
		if tk := ev.Part.Tokens; tk != nil {
			// The beta omits `total` (verified: only input/output/reasoning and
			// the cache siblings), so the honest spend figure is the sum of the
			// parts; a future build that emits total wins over the fallback.
			total := tk.Total
			if total == 0 {
				total = tk.Input + tk.Output + tk.Reasoning
			}
			p.tokens += total
			p.sawUsage = true
		}
		if c := ev.Part.Cost; c != nil {
			p.cost += *c
			p.sawUsage = true
		}
	}
}

func (p *opencode2Parse) finish(res *Result) {
	res.FinalMessage = p.lastText
	// Recorded whatever the figures come to: the driver bounds attempts that
	// reported NOTHING, and a step_finish carrying zeros still reported
	// (CLA-288). On beta-18314 the common text-only path never reports usage,
	// so UsageReported stays false there — the conformance suite pins it.
	res.UsageReported = p.sawUsage
	if p.sawUsage {
		res.Tokens = p.tokens
		res.CostUSD = p.cost
	}
	// The finish reason is recorded for diagnostics even when the step carried
	// no usage, as the same FinishReasonKey the stable adapter writes. There is
	// deliberately NO quiet-death marker here: beta-18314 does not exhibit the
	// silent exit-0 signature — the #43622 shape (no finish_reason) exits 1
	// with a typed provider.invalid-output error event (verified), which the
	// error classification already reads as a loud failure. Inventing a marker
	// for a shape this build does not produce would mislead the driver.
	if p.lastReason != "" {
		if res.Raw == nil {
			res.Raw = map[string]any{}
		}
		res.Raw[FinishReasonKey] = p.lastReason
	}
}

// opencode2ErrorText is the classification evidence: the SAME scoped text the
// stable adapter reads (stderr plus typed error stdout lines, never assistant
// narration), shared rather than duplicated so the memoized scan and any future
// fix to that scoping discipline apply here too. Each Result only ever comes
// from one adapter, so sharing the scan cache cannot collide.
func opencode2ErrorText(res Result) string { return opencodeErrorText(res) }

// The provider ecosystem is the same one the stable opencode talks to, so the
// budget/fatal/transient word-classes are shared with that adapter rather than
// duplicated. They were authored against the 1.18 family; against a beta whose
// surface changes without notice (CLA-381) they are best-effort. The failure
// direction stays safe: a failure BEFORE usage (no step_finish yet) is never
// "unclassified-transient", so a config/auth refusal stops loudly instead of
// retrying forever (see the type doc).

func (opencode2) DetectLimit(res Result) Limit {
	if opencodeBudgetRe.MatchString(opencode2ErrorText(res)) {
		return Limit{Limited: true, Stop: true, Reason: "budget_exhausted"}
	}
	return Limit{}
}

func (o opencode2) IsTransient(res Result) bool {
	text := opencode2ErrorText(res)
	if opencodeBudgetRe.MatchString(text) || opencodeFatalRe.MatchString(text) {
		return false
	}
	if opencodeTransientRe.MatchString(text) {
		return true
	}
	// The same fall-through as the stable adapter: a post-usage failure no
	// pattern names leans retryable (CLA-381). The fall-through is what keeps
	// IsUnclassifiedTransient a SUBSET of IsTransient — the loop's bound
	// invariant (TestUnclassifiedTransientImpliesTransient).
	return o.IsUnclassifiedTransient(res)
}

func (opencode2) IsUnclassifiedTransient(res Result) bool {
	// The same CLA-381 heuristic as the stable adapter: an exit 1 whose cause
	// no pattern has seen yet, on a session that REPORTED usage, leans
	// retryable — usage means the session authenticated and started doing paid
	// work, so the failure came after the startup checks. The no-usage case
	// stays a stop. beta-18314 does NOT report usage for a plain text answer
	// (verified — see the type doc), so on the common path this stays false
	// and an unrecognised failure stops loudly rather than re-spawning paid
	// sessions forever. The step_finish arm keeps the heuristic live for a
	// future build that does surface usage, without changing today's direction.
	if res.ExitCode != 1 || !res.UsageReported {
		return false
	}
	text := opencode2ErrorText(res)
	if opencodeBudgetRe.MatchString(text) || opencodeFatalRe.MatchString(text) {
		return false
	}
	return !opencodeTransientRe.MatchString(text)
}

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

// ZeroUsageUnknown is always false on this build — the honest answer, stated
// rather than faked. The marker's signature is a silent exit-0 with reason
// "unknown" and all-zero usage (CLA-398), and beta-18314 does not produce it:
// the #43622 shape (a stream with no finish_reason) makes the beta emit typed
// `provider.invalid-output` error events, RETRY internally, and exit 1
// (verified by the conformance suite) — a loud failure the error
// classification already reads. Inventing a marker for a shape this build does
// not exhibit would mislead the driver (see docs/opencode2.md).
func (opencode2) ZeroUsageUnknown(Result) bool { return false }

func (opencode2) Capabilities() Capabilities {
	return Capabilities{
		TracksClaims:            false, // tool_use events exist on tool-call turns, but the adapter does not consume them for claim tracking; a phased run is refused by config.Validate
		HonoursMaxTurns:         false,
		HonoursSessionWallClock: true, // the process-group kill in Invoke is the phase backstop
		ReportsCost:             false, // a plain text answer (the common case) emits NO step_finish and the provider's usage block is not surfaced — verified against beta-18314, so budget.max_cost_usd is inert
		HasSessionTokenCeiling:  false,
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
