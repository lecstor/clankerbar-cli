package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func init() { Register(opencode{}) }

// opencode drives the open, provider-agnostic opencode CLI, 2.x line (`opencode
// run`). It is the adapter that proves the loop isn't Claude-shaped: opencode
// points at OpenRouter / opencode Zen / opencode Go / any provider, and the model
// choice lives in opencode's own config (invisible here). The 1.x line is no
// longer supported upstream and this adapter no longer speaks its dialect
// (CLA-675). Divergences it absorbs, verified against opencode 2.0.32 with a
// hermetic fake provider and fake MCP server (docs/opencode-build.md):
//
//   - Headless entry point is `opencode run --standalone --format json [--model
//     m] -- <message>`, one JSON event per line. `--standalone` is load-bearing:
//     2.x is client/server and `run` otherwise attaches to the managed background
//     service, whose own config then wins over everything this adapter sets.
//     Stdin is left nil: a non-TTY stdin left open hangs `run`, and piped stdin
//     is appended to the prompt.
//   - Permissions come from a config `permission` block. 2.x ignores the
//     OPENCODE_PERMISSION env var entirely, and its default with no policy is
//     fail-OPEN (write, shell and webfetch all execute, inside the workdir or
//     not). The adapter therefore writes its fail-closed policy into the
//     OPENCODE_CONFIG_CONTENT layer it pins on every spawn, which 2.x merges
//     last. We never pass --auto, which approves every `ask`.
//   - MCP servers default to Code Mode in 2.x: their tools are not top-level but
//     callable only from inside the `execute` tool, whose stream shows no
//     per-call output (so no claim can be observed) and whose sandbox reached the
//     network with webfetch denied. The adapter sets `codemode: false` on every
//     server it hands over and denies `execute`.
//   - Billing depends on the configured backend, not opencode: metered pay-per-token
//     (OpenRouter / Zen) or a monthly-limit subscription (opencode Go). NONE impose a
//     short rolling-window cap, so there is no supervised-wait/early-reset case here.
//     DetectLimit instead recognizes a HARD budget/credit-exhaustion stop (402 /
//     "out of credits" / "monthly limit reached") and flags Limit.Stop so the loop
//     stops cleanly rather than waiting for a reset that never comes. The
//     self-accounted budget breaker is the primary control; ReadUsage is unsupported.
//   - Usage: 2.x emits step_finish only for tool-call steps, never for the final
//     answer, so the stream under-counts. After each process exits the adapter
//     reads `opencode session export --standalone <sid>` for the authoritative
//     figures (see opencode_export.go).
type opencode struct{}

func (opencode) Name() string { return "opencode" }

// opencode DOES receive mcp_config_path - as OPENCODE_CONFIG and, rewritten, as
// OPENCODE_CONFIG_CONTENT (see env below) - and that is exactly why "does the
// adapter hand it over" was the wrong question to gate a preflight on. What
// arrives has to be an opencode config, whose servers live under `mcp` (1.x
// dialect, still accepted) or `mcp.servers` (2.x dialect). The Claude-shaped
// `.mcp.json` that config.Validate auto-discovers from the workdir is a
// different schema: 1.18.2 refused to start on it, and 1.18.18 and 2.0.32
// silently skip its `mcpServers` key, so the session runs with no clankerbar
// tools at all. Either way that combination is a doctor FAIL rather than a
// missing-tools WARN (CLA-263).
func (opencode) MCPConfigUse() MCPConfigUse {
	return MCPConfigUse{
		Schema: MCPConfigNative,
		Note: "the opencode adapter passes mcp_config_path as OPENCODE_CONFIG (plus a rewritten copy as OPENCODE_CONFIG_CONTENT), which must be an opencode " +
			"config (servers under `mcp` or `mcp.servers`, not Claude's `mcpServers`) - opencode skips a key it does not know, so a Claude-shaped file gives sessions no clankerbar tools",
	}
}

// Invoke runs one opencode session for the driver, and — CLA-406 — when that
// session dies the quiet death (stream dropped mid-generation, no terminal
// chunk, no usage), tries to RESURRECT it in place rather than hand the driver
// a corpse: back off, probe the session with a coherence question, and on a
// mechanical match continue it where it left off. See opencode_resume.go for
// the rules and their rationale; the loop here is only their orchestration.
//
// Every round re-checks ZeroUsageUnknown on the MERGED result, so a
// continuation that itself dies quietly takes another round (a fresh probe per
// death), while a clean continuation clears the marker and exits the loop. The
// cap bounds the whole loop; after it, the last Result falls through to the
// driver's existing dead-phase path unchanged.
func (o opencode) Invoke(ctx context.Context, in Invocation) (Result, error) {
	// The wall-clock cap bounds the WHOLE Invoke, not each process: the
	// deadline is computed BEFORE the first run so the original run's time
	// counts against the budget the continuations inherit. Computing it after
	// would hand every resurrection a fresh full cap — worst case 2x the
	// configured dial before any probe overhead.
	var deadline time.Time
	if in.MaxSessionWallClock > 0 {
		deadline = time.Now().Add(in.MaxSessionWallClock)
	}
	res, err := o.runSession(ctx, in, opencodeArgs(in))
	if err != nil {
		return res, err
	}
	if in.Probe {
		// Never resurrect a probe: a probe IS the liveness check.
		return res, nil
	}
	console := in.Console
	// CLA-584: the output-cap stall. A final step that hit the output token
	// limit with zero output is NOT the quiet death the loop below resumes —
	// the session is alive and its transcript is intact; the step failed
	// because the model spent its whole output budget thinking. It gets ONE
	// resume carrying a steer (opencode_stall.go), and this runs BEFORE the
	// quiet-death loop so a stall resume that then dies quietly still gets
	// CLA-406's probe on its own terms.
	if o.OutputCapNoOutput(res) {
		o.resumeOutputCapStall(ctx, in, &res, deadline)
	}
	// An UNTRUSTED stream is never resurrected: CLA-262 forbids reading claim
	// state off an over-run capture, and both the coherence ref and the
	// continuation's seed come from exactly that state - while the driver's
	// own untrusted path declines to count further spend, so probing would
	// enlarge the accounting blind spot endUntrustedDrain exists to bound.
	// The conjunct mirrors the driver's own gate (!capped && !ceiling &&
	// !wallclock && untrusted) so adapter and tallyDead agree on what is dead.
	for attempt := 1; attempt <= opencodeMaxResurrections && o.ZeroUsageUnknown(res) && res.Untrusted == ""; attempt++ {
		sid, ref := resumeTargets(res)
		if sid == "" || ref == "" {
			// No session to resume into (died before its first event) or no
			// claim to verify against (died before claiming — no work at
			// stake). The quiet death stands.
			break
		}
		// Budget check BEFORE the round spends anything: a round's backoff
		// plus probe box come off the clock before any continuation could,
		// so a slice under the round floor can only buy a probe whose answer
		// is never acted on.
		if !deadline.IsZero() && time.Until(deadline) < opencodeResumeRoundFloor {
			if console != nil {
				fmt.Fprint(console, opencodeWallClockExhaustedMsg)
			}
			break
		}
		if console != nil {
			fmt.Fprintf(console, "\n!! quiet death detected — resurrecting session %s (attempt %d of %d): backing off %s, then probing for coherence against %s\n",
				sid, attempt, opencodeMaxResurrections, opencodeResumeBackoff, ref)
		}
		select {
		case <-ctx.Done():
			// Run-wide cancellation (Ctrl-C / supervised wait): not this
			// phase's verdict. Hand back what we have; the driver reads the
			// parent context itself.
			return res, nil
		case <-time.After(opencodeResumeBackoff):
		}

		// The probe: same session, informed of the convention, asked to prove
		// it is intact by naming its task ref. Time-boxed — silence past the
		// window is a death, not a pause. (A partially-streamed reply killed by
		// the box is judged on whatever text landed: if it already named the
		// ref, the session genuinely resumed and continuing is correct.)
		pctx, pcancel := context.WithTimeout(ctx, opencodeProbeTimeout)
		probeIn := in
		probeIn.Probe = true // no wall-clock double-cap, no console tee
		// Seed the probe's parser with the dead run's live claim, so a probe
		// turn that touches the backlog is observed on the same terms as the
		// session it belongs to - and folded back in below, so the observation
		// actually reaches the driver.
		probeIn.ResumeClaim = res.Claim
		probeRes, probeErr := o.runSession(pctx, probeIn,
			opencodeResumeArgs(in, sid, resurrectionProbePrompt))
		pcancel()
		// Charge the probe's spend UNCONDITIONALLY, before any verdict: a
		// resume re-sends the whole transcript, so this is real money even when
		// the answer is garbage. Under-counting is the one direction a budget
		// breaker must not err in. Deliberately NOT via mergeResume — a probe
		// must never donate raw/terminal state or a process outcome; its CLAIM
		// observation folds separately, just as unconditionally.
		res.Tokens += probeRes.Tokens
		res.CostUSD += probeRes.CostUSD
		res.UsageReported = res.UsageReported || probeRes.UsageReported
		// The claim observation folds ONLY when it is still about THIS task
		// and arrived on a capture we may read: noteClaimed replaces a parser's
		// claim WHOLESALE, so a decohered probe that re-claimed a different
		// task would otherwise redirect the merged Result's identity — the
		// real lease expiring while release/salvage/tally all fire against a
		// task the dead session never held. An untrusted probe capture is
		// skipped for the CLA-262 reason the loop conjunct below spells out.
		// A probe that kept its seed (did nothing, or settled/advanced ITS OWN
		// task) always passes both gates — that is finding 6's fix standing.
		if probeRes.Untrusted == "" && probeRes.Claim.Names(res.Claim.TaskID) {
			mergeObservation(&res, probeRes)
		}
		// An UNTRUSTED probe capture proves nothing: its text survived an
		// over-cap event discard (output.go), so a ref found in FinalMessage
		// may be debris rather than recall. Counting it coherent would send
		// Continue into a session whose state was just misread - the same
		// CLA-262 reasoning that gates the fold above and the loop conjunct.
		// An UNTRUSTED probe capture proves nothing: its text survived an
		// over-cap event discard (output.go), so a ref found in FinalMessage
		// may be debris rather than recall. Today the discard already empties
		// the parse, so this conjunct and probeCoherent agree by accident -
		// the conjunct makes "untrusted reply is not evidence" hold BY RULE,
		// so a future parser that survives a bad line cannot quietly start
		// continuations on debris. Same CLA-262 reasoning as the fold above.
		coherent := probeErr == nil && probeRes.Untrusted == "" && probeCoherent(probeRes.FinalMessage, ref)
		if !coherent {
			if console != nil {
				fmt.Fprintf(console, "!! resurrection probe FAILED (err=%v, reply=%.200q) — counting the death\n",
					probeErr, probeRes.FinalMessage)
			}
			break
		}

		// Coherent: continue the session for real. The continuation's parser is
		// seeded with the DEAD RUN'S live claim (not in.ResumeClaim, which on a
		// phase 2+ holds the staler predecessor): `opencode run -s` streams only
		// the new turn, so without the seed every update_task the continuation
		// makes would go unobserved — no settle, no HasWIP, no delivery report —
		// and releaseHeldClaim would post ready over work the session pushed.
		contIn := in
		contIn.ResumeClaim = res.Claim
		if !deadline.IsZero() {
			// Re-checked here, not just at the top of the round: the probe
			// just consumed up to its own time-box, and a continuation that
			// could only be cap-killed on arrival must not be started.
			remaining := time.Until(deadline)
			if remaining < opencodeResumeWallClockFloor {
				if console != nil {
					fmt.Fprint(console, opencodeWallClockExhaustedMsg)
				}
				break
			}
			contIn.MaxSessionWallClock = remaining
		}
		contRes, contErr := o.runSession(ctx, contIn,
			opencodeResumeArgs(in, sid, continuePrompt))
		if contErr != nil {
			// Spend first, even on the error path.
			res.Tokens += contRes.Tokens
			res.CostUSD += contRes.CostUSD
			res.UsageReported = res.UsageReported || contRes.UsageReported
			if console != nil {
				fmt.Fprintf(console, "!! resurrection continuation errored (%v) — counting the death\n", contErr)
			}
			break
		}
		n := 0
		if nRaw, ok := res.Raw[opencodeResurrectionsKey].(int); ok {
			n = nRaw
		}
		mergeResume(&res, contRes)
		if res.Raw == nil {
			res.Raw = map[string]any{}
		}
		res.Raw[opencodeResurrectionsKey] = n + 1
		if console != nil {
			fmt.Fprintf(console, "!! resurrection %d succeeded — session continued in place\n", n+1)
		}
	}
	return res, nil
}

// runSession executes ONE `opencode run` process end-to-end and parses its
// stream. Split out of Invoke so the CLA-406 resurrection loop can run the
// probe and the continuation through exactly the same exec/parse machinery as
// the original session — same env, same permission policy, same capture — and
// merge the results, rather than maintaining a second code path that would
// drift the first time either half changed.
func (o opencode) runSession(ctx context.Context, in Invocation, args []string) (Result, error) {

	// The per-session wall-clock cap, enforced on the exec context so the kill
	// needs nothing from the stream: opencode reports no turn count and takes no
	// turn flag, so elapsed time is the only backstop this adapter can offer a
	// phase (CLA-368). The derived context is ALWAYS created, cap or no cap, so
	// there is one cancellation path rather than two shapes of ctx below.
	//
	// Never on a PROBE: a probe is a one-word liveness check, not a session doing
	// work, and a capped probe would report a phase-shaped cap for something no
	// phase ran. The driver never sets the field on a probe either — this is the
	// belt to that braces, so a future caller reusing an Invocation cannot turn a
	// probe into a capped session by accident.
	sctx := ctx
	cancel := context.CancelFunc(func() {})
	if in.MaxSessionWallClock > 0 && !in.Probe {
		sctx, cancel = context.WithTimeout(ctx, in.MaxSessionWallClock)
	}
	defer cancel()

	cmd := exec.CommandContext(sctx, "opencode", args...)
	setupProcessGroup(cmd)
	if in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	env, err := o.env(in)
	if err != nil {
		// Nothing has been spawned yet, so this is a clean refusal to start
		// rather than a session to classify: an empty Result, and the error the
		// caller reports. See env's fail-closed note.
		return Result{}, err
	}
	cmd.Env = env
	// A cap that can hang is not a cap. The capture points Stdout/Stderr at
	// io.MultiWriter values, so os/exec makes its own pipes and cmd.Run waits for
	// every WRITER of them to close — and CommandContext kills the direct child
	// only. A runaway session is exactly the case with a bash-tool grandchild (a
	// build, a test run) or an MCP server holding the inherited fd, which survives
	// the SIGKILL and keeps the pipe open: without a delay, Invoke blocks past its
	// own deadline in the one scenario the cap exists for. WaitDelay force-closes
	// the pipes and lets Wait return.
	// The orphan itself is killed by the process-group Cancel below; this is
	// the backstop BEHIND that kill, for a descendant that escaped the group -
	// a daemonised child (setsid) lands in a group of its own that no signal
	// of ours reaches (the same escapee claude's session path has no backstop
	// for at all, CLA-423). Set whenever the exec context carries ANY deadline -
	// one this function created, or one a caller handed in (the CLA-406
	// probe's 30s box arrives that way, and the old `sctx != ctx` test missed
	// exactly that case): an fd holder would otherwise keep the pipe open past
	// the kill and hang Run past its own box. An ordinary uncapped session's
	// I/O is never cut short by it.
	if _, hasDeadline := sctx.Deadline(); hasDeadline {
		cmd.WaitDelay = 5 * time.Second
	}

	// Kill the whole process group on ANY cancellation of sctx - the
	// wall-clock cap, or the caller's own cancellation (a Ctrl-C that stopped
	// reaching the session subtree through the tty the moment Setpgid moved
	// it out of the foreground group). Through exec.Cmd.Cancel, not a monitor
	// goroutine: os/exec calls Cancel only while it still considers the
	// process live, so the post-reap window shrinks from "every capped
	// session" to the instant between Process.Wait returning and the
	// watchdog's handshake, where os/exec's own Kill reads os.ErrProcessDone.
	// killProcessGroup's raw syscall.Kill narrows the recycled-pid hazard by
	// orders of magnitude without eliminating it - unlike Process.Kill it
	// enjoys no pidfd/reaped-status protection, so a group id reused inside
	// that window would be signalled in error. Accepted rather than hardened:
	// skipping the group kill whenever the direct child looks already-reaped
	// could strand exactly the in-group descendants this file exists to kill,
	// trading the bar away for a microseconds-wide hazard. The direct child
	// is killed too (the trailing Kill mirrors CommandContext's own default
	// Cancel), and a Kill on an already-dead child reads as os.ErrProcessDone,
	// which os/exec treats as nothing to report.
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return cmd.Process.Kill()
	}

	// Parse as the stream arrives, retain only a bounded tail of it for the text
	// scans, and tee live to the console when one is set (the JSON event stream —
	// a readable renderer is a TODO; raw is honest live output).
	//
	// Live parsing is load-bearing here rather than merely tidy: this adapter SUMS
	// per-step usage across the whole session, so a capped copy of stdout walked
	// afterwards would silently drop the early steps and under-count the budget by
	// however much of the stream had scrolled away (CLA-262).
	console := in.Console
	if in.Probe {
		console = nil
	}
	// Through the constructor, not a struct literal: it carries the resumed-phase
	// claim seed, and the tests build the parser the same way so that dropping it
	// here turns them red.
	p := newOpencodeParse(in, console)
	captured := newCapture(p.line)
	captured.attach(cmd, console)
	// Taken before the spawn, so every message this process adds to the session
	// is stamped after it: applyExport attributes export figures to THIS run by
	// that window (a resumed session's export also carries the earlier runs).
	started := time.Now()
	runErr := cmd.Run()

	res := captured.result("opencode")
	p.finish(&res)

	// Our own deadline, not the caller's: ctx.Err() distinguishes a session we
	// cut off from a run-wide Ctrl-C / SIGTERM / supervised-wait cancellation,
	// which cancels the parent too and is NOT this phase reaching its backstop.
	// Marked after p.finish, which rewrites Raw wholesale when it saw usage.
	//
	// runErr is the third conjunct because the deadline is read AFTER Run
	// returned: a session that exited cleanly a hair before it, with the deadline
	// passing in the window while the output was parsed, would otherwise be
	// marked as capped. Nothing would misbehave today — the driver takes its
	// exit-0 branch first — but the Result would be lying about how the session
	// ended, and the next reader of this marker inherits the lie.
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
		// (a backgrounded build, a test run) inherited the output pipes and held
		// them open past cmd.WaitDelay, so os/exec force-closed them and Run
		// returned exec.ErrWaitDelay instead of an *exec.ExitError. p.finish has
		// already run (the CLA-299 ordering: parse whatever arrived, THEN
		// classify), and every event the session emitted was consumed by the live
		// parse as it arrived — only grandchild bytes written after the parent's
		// exit are lost, and those were never this session's output. This is a
		// clean end, not a failure: returning it as an error failed a completed
		// attempt in drainPhase with no retry classification (CLA-414). Residual
		// risk, accepted: force-closed pipes mean this cannot be distinguished
		// from a parent killed while pretending to succeed — ExitCode 0 plus a
		// fully parsed live stream is the strongest evidence available either way.
		// The same holds for a run-wide cancellation landing inside the window:
		// sctx reads Canceled, not DeadlineExceeded, so !timedOut is true and the
		// attempt ends "clean" during teardown. Nothing durable turns on it —
		// salvage, handback and spend are computed from res before any error
		// check either way, and the cancelled driver ctx stops the run one spawn
		// later — so the noise is left standing rather than guarded against.
		o.settleUsage(ctx, in, &res, started, isResumeArgs(args))
		return res, nil
	} else if runErr != nil && !timedOut {
		// A launch failure (bad PATH, unreadable config): nothing was emitted,
		// so res is an honest zero, and the failure is real.
		return res, runErr
	} else if runErr != nil {
		// Our own kill: the child died on the cancel, so its error is the kill's
		// and not a verdict. The marker above is the verdict.
		res.ExitCode = -1
	}
	o.settleUsage(ctx, in, &res, started, isResumeArgs(args))
	return res, nil
}

// opencodeArgs maps an Invocation to `opencode run`'s CLI dialect. Split out so the
// probe's read-only shape is unit-testable without executing anything.
//
// The prompt goes LAST, behind a `--` terminator, for the reason spelled out in
// codexArgs: it is a bare positional, and a prompt that is itself a flag token
// would otherwise be parsed as a flag rather than as the message.
func opencodeArgs(in Invocation) []string {
	prompt := opencodePrompt(in.Prompt)
	if in.Probe {
		// The cheapest possible request: a trivial prompt, tools all denied by the
		// read-only permission policy (see env) — just enough to see whether the
		// provider still answers or is budget-exhausted.
		prompt = "."
	}
	args := []string{"run", "--standalone", "--format", "json", "--agent", opencodeAgent}
	if m := in.ModelArg(); m != "" {
		args = append(args, "--model", m)
	}
	return append(args, "--", prompt)
}

// opencodeMCPWarmupNote is prepended to every prompt that does work. 2.x
// connects MCP servers asynchronously: verified against 2.0.32 with a fake MCP
// server, the clankerbar tools were absent from the session's first LLM request
// and arrived on the second, so a first-turn `claim_task` comes back as the
// tool error "No tool named ... is currently available". The model sees that
// error and decides what to do next; without being told, it can read it as a
// missing integration and give up on the backlog. A retry inside the adapter
// was the alternative and was not chosen: the failed call is an ordinary turn
// the model already gets to react to, and re-spawning the session would pay for
// the whole prompt again to fix something one sentence of brief fixes.
const opencodeMCPWarmupNote = "Note: this session's MCP servers connect in the background. " +
	"If an MCP tool (for example clankerbar_claim_task) reports that no tool by that name is currently available, " +
	"it is still connecting: call it again on your next step rather than concluding it is missing. "

// opencodePrompt is the message a working (non-probe) run sends. It carries the
// MCP warm-up note ahead of the caller's prompt; see opencodeMCPWarmupNote. The
// note is one line and ends in a space, so the prompt keeps its own first line.
// An empty prompt stays empty so the CLI's own handling of it is unchanged.
func opencodePrompt(prompt string) string {
	if prompt == "" {
		return prompt
	}
	return opencodeMCPWarmupNote + prompt
}

// opencodeMCPResourcePattern is the pattern shape opencode's MCP resource tools
// ask with under the `read` permission: "mcp:<server>:<uri>" for a read and
// "mcp:<server>:*" for a listing. opencode's matcher (util/wildcard.ts) lets `*`
// span "/" and ":", so one pattern covers every server and every URI — including
// the URL-shaped resource URIs the clankerbar plane serves its protocol at.
const opencodeMCPResourcePattern = "mcp:*"

// opencodePermission is the fail-closed policy the adapter writes into the
// `permission` key of the config it pins via OPENCODE_CONFIG_CONTENT (see
// opencodeSessionConfig): a path-scoped PermissionConfig, not a flat tool map. The run's workdir subtree is the working
// set — `read`/`edit` are allowed there and `external_directory` (opencode's gate
// for any path-taking tool whose target is outside the project directory) allows
// the same subtree, so the session's own worktrees, which live under the workdir
// and are separate git roots, work normally. Everything else fails closed via the
// `*` catch-all, and the network/exfil tools (webfetch/websearch) are denied in
// both shapes. A read-only run (probe, or the connectivity smoke) additionally
// denies edits and shell — zero writes, just enough to reach the clankerbar MCP.
//
// EXTRA DIRECTORIES (CLA-437, agent-rule-scoping piece 2): each entry in
// extraDirs — the project's other declared repos, plus any conventional worktree
// area beside the spawn checkout — is granted the same reach the workdir has:
// an absolute external_directory allow, and a read/edit allow in the
// instance-relative "../<dir>/**" form, which sorts after the blanket "../**"
// deny and so wins for exactly the declared trees. See scopeExtraDirs.
//
// `shell` (1.x called it `bash`; 2.x accepts both names) stays TOOL-LEVEL: its
// permission patterns match parsed COMMANDS ("git status --porcelain"), not
// paths, so a path rule can never express it. On 1.x, commands whose file
// arguments fell outside the project boundary were gated separately via
// external_directory. 2.x DROPPED THAT: verified against 2.0.32, under this policy
// `touch /abs/outside` and `cat /etc/hosts` both ran, and only the shell tool's
// own `workdir` argument reaches external_directory. The operator accepted that
// gap on 2026-10-18 (CLA-675): an unattended session can already run arbitrary
// commands inside the workdir, so no sandbox and no absolute-path shell deny
// patterns were added (docs/opencode-build.md, "Shell is not path-gated").
//
// `execute` is denied by name. It is 2.x's Code Mode tool: MCP tools left in Code
// Mode are callable only through it, its stream carries no per-call output (no
// claim could be observed), and code run inside it reached the network with
// webfetch denied. The catch-all already denies it; naming it keeps that from
// depending on the catch-all. Every MCP server the adapter hands over is moved
// out of Code Mode instead (opencodeSessionConfig), so `*_*` below is what
// reaches them.
//
// Five quirks the emitted shape is fitted to. They were read out of opencode
// 1.18.16 source (permission/index.ts, session/tools.ts, tool/read.ts,
// tool/edit.ts, tool/external-directory.ts). Against 2.0.32 the first three were
// re-checked behaviourally (last-match-wins, `<server>_<tool>` MCP names,
// external_directory for out-of-tree file tools); the MCP-resource one no
// longer applies the same way, because 2.x registers the resource tools in Code
// Mode only, so with `execute` denied a session has no resource tools at all:
//
//   - Rules are evaluated LAST-MATCH-WINS on a flattened ruleset, so the specific
//     rules must sort AFTER the `*` catch-all or the catch-all overrides them.
//     Go marshals map keys sorted, and "*" sorts before every letter, so the
//     order holds by construction; opencode_test.go pins it.
//   - `*` also matches every MCP tool ask — the MCP adapter asks with the full
//     tool name — so the catch-all would deny the session the plane itself. MCP
//     tool names are `<sanitized-server>_<sanitized-tool>` (opencode 1.18.x,
//     McpCatalog.toolName: "clankerbar_get_backlog_summary", "context7_query-docs",
//     "chrome-devtools_click"), so the policy allows `*_*`. That rule also matches
//     underscore-named built-ins (external_directory, list_mcp_resources,
//     apply_patch, doom_loop, plan_enter/exit). The one that matters is
//     external_directory: its own entry carries an inner `*` deny, which sorts
//     after the `*_*` allow in the flattened ruleset, so an out-of-workdir ask
//     still resolves to deny (the old flat `external_directory: deny`, restored);
//     only an in-workdir ask reaches the allow. The other collisions are benign:
//     apply_patch asks under "edit" (so the probe's edit:deny covers it), and
//     doom_loop / plan_enter / plan_exit are loop/UI guards with no file side
//     effects.
//   - MCP RESOURCE reads do NOT ask under their own tool name, so `*_*` never
//     reaches them: list_mcp_resources, list_mcp_resource_templates and
//     read_mcp_resource all ask under permission **`read`**, with `mcp:`-prefixed
//     patterns — `mcp:<server>:<uri>` for a read, `mcp:<server>:*` for a listing
//     (opencode 1.18.x, session/tools.ts). Against a `read` entry scoped to
//     workdir paths alone, no rule matched them and the `*` catch-all denied every
//     one — which is why a session could call the plane's TOOLS but could not read
//     the served skill that carries the heartbeat cadence (CLA-382). The `read`
//     entry therefore also allows `mcp:*`. That is not a loosening of the
//     fail-closed posture: it grants exactly what `*_*` already grants for the
//     same servers — read-only data from an MCP server the operator configured,
//     which is strictly narrower than the arbitrary TOOL calls `*_*` allows those
//     same servers — and it cannot widen filesystem reach. Not because a path
//     could never look like one of these patterns (a file named `mcp:x` directly
//     under the worktree root would ask with the pattern "mcp:x"), but because the
//     read tool runs its external_directory gate BEFORE the read ask: a path
//     outside the workdir is refused before `mcp:*` is ever consulted, and a
//     relative path that did start with "mcp:" is inside the working set already.
//   - grep/glob (and list, in newer opencode) are read-only SEARCH tools, and
//     their asks are NOT path-scoped the way read/edit are. grep and glob are
//     verified against opencode 1.18.18 source (tool/grep.ts, tool/glob.ts):
//     grep asks with the REGEX itself (`patterns: [pattern]`), glob asks with
//     the GLOB itself. `list` is NOT a builtin in 1.18.18 - the run path's
//     registry carries no list tool, so there is no tool/list.ts to verify
//     against - its ask shape (the resolved absolute directory) is inferred by
//     analogy with grep/glob/read, not source-confirmed. A path rule could
//     never match any of these asks, so for these tools the workdir boundary
//     cannot live in the ask; it lives in `external_directory`, which every one
//     of them is claimed to run BEFORE searching - containsPath returns
//     without asking for an in-workdir target, and an out-of-workdir target
//     asks external_directory, denied here. That claim about grep.ts/glob.ts's
//     internal call order is verified against source and is NOT re-checked by
//     any test in this repo (a future opencode release changing it would not
//     go red here); confirm it holds after an opencode upgrade by checking
//     `~/.local/share/opencode/log/opencode.log` for an `external_directory`
//     deny on an out-of-workdir grep/glob/list call in a live session (see the
//     README's "Verifying the policy against a live session"). Allowing the
//     pattern `**` therefore grants no reach `read` does not already have: the
//     ask governs the search pattern, the reach is still the workdir subtree
//     (CLA-390). `list` is allowed for the same read-only reason so a future
//     opencode upgrade does not re-break directory listing the way grep/glob
//     broke.
//   - read/edit asks carry the path RELATIVE TO THE INSTANCE DIRECTORY on 2.x
//     (verified against 2.0.32: in a git checkout's subdirectory a write of
//     "w1.txt" asked "w1.txt", not "sub/w1.txt", and outside any repo it asked
//     "w1.txt", not the root-relative form 1.x used). An in-tree ask is a plain
//     relative path, so "**" allows the working set, and an escape is
//     "../"-prefixed, so "../**" denies it. The deny sorts first ("*" 0x2a
//     before "." 0x2e), so a declared extra dir's own "../<dir>/**" allow still
//     wins on last-match-wins. external_directory patterns are absolute globs,
//     and external_directory is judged against the instance directory too (a
//     read of "../top.txt" from a checkout's subdirectory was refused there, git
//     worktree or not), so that rule uses the absolute workdir form.
//
// 1.x made those asks relative to the git WORKTREE (or to "/" outside a repo),
// which is why this policy used to carry a root-relative layer and an
// in-checkout layer chosen by walking up for a .git entry (CLA-441). On 2.x the
// instance directory alone decides the shape, so there is one layer, and the
// PWD pin (pinPWD) is what makes the instance directory Invocation.WorkDir -
// which the driver sets to the task's repo checkout (loop sessionDir), not the
// project workdir.
func opencodePermission(readOnly bool, workdir string, extraDirs []string) string {
	b, _ := json.Marshal(opencodePermissionPolicy(readOnly, workdir, extraDirs))
	return string(b)
}

// opencodePermissionPolicy is opencodePermission before marshalling, so the
// session config can embed it as an object rather than a string.
func opencodePermissionPolicy(readOnly bool, workdir string, extraDirs []string) map[string]any {
	_, abs := opencodeWorkdirPatterns(workdir)
	// "**" is everything under the instance directory, the root included;
	// "../**" is every escape from it. See the function doc for why these are
	// the only two shapes a 2.x ask can take.
	read := map[string]string{"**": "allow", "../**": "deny", opencodeMCPResourcePattern: "allow"}
	edit := map[string]string{"**": "allow", "../**": "deny"}
	ext := map[string]string{"*": "deny", abs: "allow"}
	scopeExtraDirs(read, edit, ext, opencodeInstanceDir(workdir), extraDirs)
	perm := map[string]any{
		// The working set: read/edit scoped to the workdir subtree (root
		// included), plus the external_directory carve-out for the same
		// subtree. The carve-out carries an inner `*` deny so an ask for a
		// path OUTSIDE the subtree still resolves to deny — the old flat
		// `external_directory: deny`, restored — while an in-subtree ask wins
		// on the later absolute allow.
		// `mcp:*` rides on "read" because that is the permission the MCP
		// resource tools ask under — see the function doc. A read-only run
		// keeps it: reaching the served protocol IS the point of the probe.
		"read":               read,
		"edit":               edit,
		"external_directory": ext,
		// MCP tools must survive the `*` catch-all — see the function doc.
		"*_*": "allow",
		// The read-only search tools: grep/glob/list. Their asks carry the
		// search pattern (a regex, a glob, or the resolved directory), never a
		// workdir-relative path, so a path-scoped rule could not match them and
		// the catch-all would leave the tools denied — the CLA-390 bug. The
		// pattern `**` is a flat allow of the ASK; the filesystem boundary
		// stays in `external_directory`, which each tool runs before searching
		// and which is scoped to the workdir subtree above. See the function
		// doc's grep/glob/list bullet for the verification.
		"grep": map[string]string{"**": "allow"},
		"glob": map[string]string{"**": "allow"},
		"list": map[string]string{"**": "allow"},
		// The exfil guards, explicit in both shapes.
		"webfetch":  "deny",
		"websearch": "deny",
	}
	// Code Mode, denied by name: see the function doc.
	perm["execute"] = "deny"
	if readOnly {
		perm["edit"] = "deny"
		perm["shell"] = "deny"
	} else {
		perm["shell"] = "allow"
	}
	// The catch-all: anything not named above — lsp/task/skill, reads or edits
	// outside the workdir — is denied rather than asked. Sorts first (see the
	// doc), so every specific rule wins the last-match-wins evaluation.
	perm["*"] = "deny"
	return perm
}

// scopeExtraDirs widens the policy's three path-scoped rules to cover extraDirs:
// the absolute external_directory allow, and the instance-relative read/edit
// form. It mutates the maps in place so the base construction stays the single
// description of the working set.
//
// A tree INSIDE the instance directory needs no read/edit rule ("**" covers it).
// A tree that CONTAINS the instance directory is skipped, and that skip is
// load-bearing rather than tidy. filepath.Rel returns a pure dot-dot chain for
// an ancestor ("..", "../.."), and opencode's matcher reads `*` as any run of
// characters including "/", so "../**" is not "that directory" - it is
// everything outside the instance directory. Emitting it would either overwrite
// the "../**" deny with an allow or sort after it and beat it ("../../**"),
// turning the policy's only stated escape boundary into an allow of the whole
// filesystem. There is no bounded glob to give it ("../*" spans "/" exactly as
// "../**" does), so the choice is that or an escape, and fail-closed wins
// (CLA-441 reviews). The cost is a WITHDRAWN grant: a declared tree containing
// the workdir is unreadable outside the workdir itself.
func scopeExtraDirs(read, edit, ext map[string]string, base string, extraDirs []string) {
	for _, dir := range extraDirs {
		if dir == "" {
			continue
		}
		_, abs := opencodeWorkdirPatterns(dir)
		ext[abs] = "allow"
		rel, err := filepath.Rel(base, absOrSelf(dir))
		if err != nil || rel == "." || isDotDotChain(rel) || !strings.HasPrefix(filepath.ToSlash(rel), "../") {
			continue
		}
		relGlob := filepath.ToSlash(rel)
		read[relGlob+"/**"] = "allow"
		edit[relGlob+"/**"] = "allow"
		read[relGlob] = "allow"
		edit[relGlob] = "allow"
	}
}

// isDotDotChain reports whether a slash-separated relative path is nothing but
// ".." components - what filepath.Rel returns for a directory that CONTAINS the
// base. Such a path has no bounded glob form in opencode's instance-relative ask
// shape, which is why scopeExtraDirs refuses to emit one.
func isDotDotChain(rel string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg != ".." {
			return false
		}
	}
	return true
}

// opencodeInstanceDir is the directory a session's asks are relative to: the
// workdir, or the process cwd when the workdir is empty (where the session
// would actually run).
func opencodeInstanceDir(workdir string) string {
	if workdir == "" {
		if cwd, err := os.Getwd(); err == nil {
			return cwd
		}
	}
	return absOrSelf(workdir)
}

func absOrSelf(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// opencodeWorkdirPatterns derives two pattern forms from a directory. abs is the
// pattern for external_directory asks, which are absolute globs. rootRel is the
// root-relative form 1.x read/edit asks used (the absolute path minus its
// leading separator); 2.x never asks in it, and the policy no longer emits it,
// but the helper keeps returning it for the tests that pin its derivation. An
// empty workdir (the config default: run in the driver's cwd) falls back to the
// resolved cwd so the policy still scopes the session to where it actually runs.
func opencodeWorkdirPatterns(workdir string) (rootRel, abs string) {
	if workdir == "" {
		var err error
		if workdir, err = os.Getwd(); err != nil {
			// Unresolvable cwd: match NOTHING rather than everything — the
			// policy's stated posture is fail-closed. Never seen live.
			return "/**", "\x00/**"
		}
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return "/**", "\x00/**"
	}
	abs = filepath.Clean(abs)
	vol := filepath.VolumeName(abs)
	rel := strings.TrimPrefix(abs, vol)
	rel = strings.TrimLeft(rel, `/\`)
	return filepath.ToSlash(rel) + "/**", filepath.ToSlash(abs) + "/**"
}

func (opencode) env(in Invocation) ([]string, error) {
	// The session's INSTANCE directory. opencode reads $PWD, not just the real
	// cwd, so this is what decides which project-level opencode.json it merges
	// and which git worktree its read/edit asks are relative to - see pinPWD.
	env := pinPWD(os.Environ(), in.WorkDir)
	// Pin the config dir so a headless session loads the SAME providers, model
	// and plugins as the interactive one (the claude/CODEX parity). On 2.x
	// OPENCODE_CONFIG_DIR REPLACES the global config dir (~/.config/opencode)
	// rather than adding a layer after it. Auth is not here on 2.x: credentials
	// live in the data dir's opencode.db, which this does not move.
	if in.ConfigDir != "" {
		env = append(env, "OPENCODE_CONFIG_DIR="+in.ConfigDir)
	}
	// opencode has no per-run MCP flag; when a caller supplies a config file path
	// (in opencode's own schema, carrying the mcp block) point OPENCODE_CONFIG at
	// it. The Claude-shaped .mcp.json is a different schema and is refused by
	// doctor, not handed over.
	if in.MCPConfigPath != "" {
		env = append(env, "OPENCODE_CONFIG="+in.MCPConfigPath)
	}
	// ...and hand the session config over again as OPENCODE_CONFIG_CONTENT,
	// which opencode merges after every layer this driver or a checkout can
	// write (CLA-441; re-verified on 2.0.32, where the content layer's
	// `permission` beat a project-level opencode.json). OPENCODE_CONFIG is an
	// early layer: the global dir, then that file, then every project-level
	// opencode.json and .opencode/opencode.json discovered from the instance
	// directory, THEN this. Later layers have been observed redirecting
	// `mcp.clankerbar` at ANOTHER project, so naming the file does not pin which
	// backlog an unattended session works. The content layer does.
	//
	// On 2.x it is also the ONLY place the permission policy can live: the
	// OPENCODE_PERMISSION variable 1.x read is ignored (the string is absent from
	// the 2.0.32 binary), and with no policy 2.x runs every tool. So the content
	// is ALWAYS set, with or without an mcp config - see opencodeSessionConfig.
	//
	// This does not make the PWD pin redundant, nor it this: the content layer
	// settles which MCP server the session talks to and what it may do, the
	// instance directory settles which repo it reads, edits and asks permission
	// for.
	//
	// Two costs of moving a file's bytes into the environment, neither large
	// enough to change the call: a credential written LITERALLY into the mcp
	// config (rather than as `{env:CLANKERBAR_API_KEY}`) is now in the child's
	// environment, where a process listing or a crash dump reaches it and a 0600
	// file did not; and environment plus argv is capped around 1 MB on macOS, so
	// an enormous config would fail the spawn with E2BIG rather than merely being
	// ignored.
	content, err := opencodeSessionConfig(in)
	if err != nil {
		// FAIL CLOSED. The alternative - spawn without the content layer - is a
		// session with no permission policy at all on 2.x, claiming from whichever
		// project the ambient config names. A session that never starts is a loud,
		// correct outage.
		return nil, err
	}
	env = append(env, "OPENCODE_CONFIG_CONTENT="+string(content))
	// Set last so an explicit caller value in in.Env still wins (exec takes the
	// last of a duplicate key). doctor warns when a config declares
	// OPENCODE_CONFIG_CONTENT, because that replaces the policy wholesale.
	env = append(env, in.Env...)
	return env, nil
}

// opencodeSessionConfig builds the document pinned as OPENCODE_CONFIG_CONTENT:
// the mcp config file's own keys, with every MCP server moved out of Code Mode
// and the fail-closed permission policy under `permission`.
//
// The file is parsed as strict JSON, the same rule config applies to every file
// the driver names (readMCPFile): those feed fail-closed gates, and a lenient
// parse here would let the adapter accept what config refused. Numbers are kept
// as written (UseNumber) so a re-marshalled timeout or port is byte-for-byte the
// operator's value.
//
// The file's own `permission` key, if any, is replaced here rather than merged.
// That does not neutralise it: the same file is also loaded as OPENCODE_CONFIG,
// an earlier layer, and opencode merges permission blocks key by key in
// insertion order, so a key the file declares sorts ahead of this policy's `*`
// catch-all. doctor WARNs on a named file carrying one (and config refuses one
// in a DISCOVERED file, checkDiscoveredMCPConfig).
func opencodeSessionConfig(in Invocation) ([]byte, error) {
	doc := map[string]any{}
	if in.MCPConfigPath != "" {
		data, err := os.ReadFile(in.MCPConfigPath)
		if err != nil {
			return nil, fmt.Errorf("mcp config %s: %w", in.MCPConfigPath, err)
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return nil, fmt.Errorf("mcp config %s: not a JSON object: %w", in.MCPConfigPath, err)
		}
		if doc == nil {
			doc = map[string]any{}
		}
		if err := opencodeMCPOutOfCodeMode(doc); err != nil {
			return nil, fmt.Errorf("mcp config %s: %w", in.MCPConfigPath, err)
		}
	}
	policy := opencodePermissionPolicy(in.Probe, in.WorkDir, in.ExtraDirs)
	doc["permission"] = policy
	// Pin the same policy on every built-in agent. Verified against 2.0.32: an
	// agent-level permission block in ANY layer (1.x `agent.<name>.permission`
	// or 2.x `agents.<name>.permissions`) overrides the top-level one for that
	// agent. With `{"agent":{"build":{"permission":{"external_directory":
	// "allow",...}}}}` in a project opencode.json - a file a session can write
	// into its own checkout for the next spawn - out-of-tree reads and writes
	// succeeded and webfetch was offered. With this pin in the content layer
	// they were refused again, in both dialects, and in-tree writes still
	// landed. The file's own `agent` key is replaced, not merged.
	agents := map[string]any{}
	for _, name := range opencodePinnedAgents {
		agents[name] = map[string]any{"permission": policy}
	}
	doc["agent"] = agents
	delete(doc, "agents")
	delete(doc, "default_agent")
	return json.Marshal(doc)
}

// OpencodeSessionConfig is the document the opencode adapter pins as
// OPENCODE_CONFIG_CONTENT for a session with these inputs. Exported so doctor
// reports the posture the adapter will actually hand over, from the same code,
// rather than re-deriving it.
func OpencodeSessionConfig(mcpConfigPath, workdir string, readOnly bool) ([]byte, error) {
	return opencodeSessionConfig(Invocation{MCPConfigPath: mcpConfigPath, WorkDir: workdir, Probe: readOnly})
}

// opencodePinnedAgents are the agents the session config pins the policy on:
// opencodeAgent, which every run selects explicitly, and the other agents
// 2.0.32 defines. The session's own policy denies the subagent tool, so the
// others are pinned as a second line, not because a session can reach them.
var opencodePinnedAgents = []string{opencodeAgent, "plan", "general", "explore"}

// opencodeAgent is the agent every run selects with --agent. Selecting it
// explicitly means an ambient `default_agent` cannot route the session to an
// agent whose permissions this adapter never pinned.
const opencodeAgent = "build"

// opencodeMCPOutOfCodeMode rewrites a config's `mcp` block in place so every
// server it declares is in the 2.x dialect (`mcp.servers.<name>`) with
// `codemode: false`.
//
// Why both halves are needed, verified against 2.0.32:
//
//   - With Code Mode on (the 2.x default) a server's tools are reachable only
//     from inside the `execute` tool, which this adapter denies, and whose
//     stream would not show the claim anyway. `codemode: false` puts them back
//     at the top level as `<server>_<tool>`, with input and output on the
//     tool_use event as on 1.x.
//   - `codemode` is only honoured in the 2.x dialect. A 1.x-dialect entry
//     (`mcp.<name>` with a `type`) is still accepted by 2.x, but a `codemode`
//     key on it is ignored and the tools stay in Code Mode. So 1.x entries are
//     moved under `servers`, translating the one key whose spelling changed
//     (1.x `enabled: false` is 2.x `disabled: true`).
//
// A server declared in both dialects under one name is refused rather than
// guessed at. A key under `mcp` that is not a server entry is left alone for
// opencode to judge.
func opencodeMCPOutOfCodeMode(doc map[string]any) error {
	raw, ok := doc["mcp"]
	if !ok || raw == nil {
		return nil
	}
	mcp, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("`mcp` is not an object")
	}
	servers := map[string]any{}
	if rs, present := mcp["servers"]; present {
		s, ok := rs.(map[string]any)
		if !ok {
			return fmt.Errorf("`mcp.servers` is not an object")
		}
		servers = s
	}
	for name, v := range mcp {
		if name == "servers" {
			continue
		}
		entry, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if _, typed := entry["type"].(string); !typed {
			continue
		}
		if _, dup := servers[name]; dup {
			return fmt.Errorf("MCP server %q is declared both as `mcp.%s` and `mcp.servers.%s`", name, name, name)
		}
		if enabled, isBool := entry["enabled"].(bool); isBool {
			delete(entry, "enabled")
			if !enabled {
				entry["disabled"] = true
			}
		}
		servers[name] = entry
		delete(mcp, name)
	}
	for name, v := range servers {
		entry, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("`mcp.servers.%s` is not an object", name)
		}
		entry["codemode"] = false
	}
	mcp["servers"] = servers
	return nil
}

// opencodeEvent is one line of `opencode run --format json`. The stream is a
// sequence of typed events, each wrapping a message part; this captures only the
// fields the loop needs and tolerates the rest.
//
// Shapes, recorded against opencode 2.0.32 (every event also carries a
// top-level `timestamp`):
//
//	{"type":"step_start","sessionID":"ses_...","part":{"id":"prt_...","messageID":"msg_...","type":"step-start"}}
//	{"type":"text","sessionID":"ses_...","part":{"id":"prt_...","type":"text","text":"DONE",...}}
//	{"type":"step_finish","sessionID":"ses_...","part":{"type":"step-finish","reason":"tool-calls",
//	   "cost":0.055563,"tokens":{"input":18511,"output":2,"reasoning":0,"cache":{"read":0,"write":0}}}}
//	{"type":"error","sessionID":"ses_...","error":{"type":"provider.auth","message":"...","status":401,...}}
//
// step_finish is emitted for TOOL-CALL steps only: the step that ends the
// session with a plain answer emits none, so neither the final reason nor the
// final step's usage is on the stream. That is what the session export after
// exit is for (opencode_export.go); the stream figures are the fallback.
type opencodeEvent struct {
	Type  string              `json:"type"`
	Part  *opencodePart       `json:"part"`
	Error *opencodeErrorEvent `json:"error"`

	// SessionID is the opencode session every event of one run belongs to. It
	// rides at the TOP level of each event, not on the part, and is constant for
	// a run — captured once and used as the handle a follow-up `opencode run -s
	// <id>` continues and `opencode session export` reads.
	SessionID string `json:"sessionID"`
}

// opencodeErrorEvent is the typed error 2.x puts on a {"type":"error"} event.
// The type is a stable dotted name ("provider.auth", "provider.rate-limit",
// "provider.no-route", "provider.invalid-output"), which classification keys on
// before it falls back to the word classes.
type opencodeErrorEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

type opencodePart struct {
	Type   string          `json:"type"`
	Text   string          `json:"text"`
	Reason string          `json:"reason"`
	Tokens *opencodeTokens `json:"tokens"`
	// A POINTER for the same reason Tokens is one: a step reporting cost 0 has
	// REPORTED, and the driver's zero-spend bound counts silence, not zeroes
	// (CLA-288).
	Cost *float64 `json:"cost"`

	// Tool parts, which arrive on a "tool_use" event. On 2.x the call id is
	// `id` (1.x called it `callID`) and the part's own id moves to `partID`.
	// The call id is the PROVIDER's, so it is not unique across steps - the
	// fake provider used in the conformance suite numbers every step's calls
	// from call_0 - which is why replays are recognised by partID.
	Tool   string             `json:"tool"`
	ID     string             `json:"id"`
	PartID string             `json:"partID"`
	State  *opencodeToolState `json:"state"`
}

// opencodeToolState is a tool call's lifecycle slot on its part. Where claude
// splits a call across an `assistant` tool_use block and a later `user`
// tool_result block, opencode emits the part once it has settled and carries
// the arguments and the result TOGETHER (recorded against opencode 2.0.32 with
// the MCP server out of Code Mode):
//
//	{"type":"tool_use","part":{"partID":"prt_...","type":"tool","id":"call_0",
//	  "tool":"clankerbar_claim_task","state":{"status":"completed",
//	    "input":{"taskId":"T-9"},
//	    "output":"{\"task\": {\"id\": \"T-9\", \"ref\": \"CLA-1\"}, \"run\": {\"id\": \"run-1\"}}",
//	    "title":"clankerbar_claim_task","metadata":{...}}}}
//
// A refusal takes the other terminal status and puts the error where the output
// would have been. There is no `is_error` flag to read — the STATUS is the flag,
// which is why a status this adapter does not recognise must never be treated as
// success:
//
//	{"state":{"status":"error","input":{...},
//	  "error":"No tool named \"clankerbar_claim_task\" is currently available. ..."}}
//
// `output` is a STRING, not an object: an MCP tool's result arrives flattened to
// its text content, which is the shape noteClaimed already parses.
//
// A large result is TRUNCATED on 2.x, the same failure Claude Code's spill
// caused on the phase seam (CLA-330, docs/large-tool-results.md). Verified
// against 2.0.32: an MCP result above 51,200 bytes arrives as
//
//	"output":"[showing 0 lines of 1; full output saved to <data>/opencode/tool-output/tool_...]",
//	"metadata":{"metadata":{"truncated":true,"outputPath":"<the same path>"},...}
//
// and the full text is in that file. Plane payloads reach that size (a
// claim_task carrying decisions, a get_task at 56 KB). opencodeTruncatedOutput
// rehydrates it from the file, under the guards that function states.
type opencodeToolState struct {
	Status   string          `json:"status"`
	Input    json.RawMessage `json:"input"`
	Output   string          `json:"output"`
	Error    string          `json:"error"`
	Metadata *struct {
		Metadata *struct {
			Truncated  bool   `json:"truncated"`
			OutputPath string `json:"outputPath"`
		} `json:"metadata"`
	} `json:"metadata"`
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

// opencodeParse accumulates FinalMessage, Tokens and CostUSD from the event
// stream, ONE LINE AT A TIME as it arrives.
//
// Tokens/cost are SUMMED across every step-finish part: opencode reports usage
// per step (one step = one LLM turn), not cumulatively, so a multi-turn drain
// emits several step-finish parts that each cover their own turn. FinalMessage is
// the last non-empty text part — the final assistant answer, after any
// intermediate per-step messages.
//
// Because it sums, it has to see EVERY step — which is exactly why it runs on the
// live stream instead of over the retained tail (see Invoke).
type opencodeParse struct {
	lastText string
	// lastReason is the reason of the MOST RECENT step_finish — the session's
	// final step, once the stream ends. opencode writes "stop" when the session
	// produced a final answer and "unknown" when it died without one (the
	// CLA-386 dead-phase marker); the driver reads it off Result.Raw via
	// FinishReasonKey.
	lastReason string
	// lastStepZeroUsage reports that the MOST RECENT step_finish's own usage
	// summed to zero — no tokens block, an all-zero tokens block, or no cost
	// are all the same quiet end. Reset for every step: the CLA-398
	// quiet-death signature is a FINAL step whose OWN usage is all-zero — a
	// session can do hours of real paid work and then die with an "unknown"
	// final step that reported nothing, so the session total (p.total/p.cost)
	// is NOT the discriminator. Only the final step's own usage is.
	lastStepZeroUsage bool
	// lastStepOut and lastStepReason are the MOST RECENT step_finish's OWN
	// output and reasoning token counts (CLA-584). lastStepOut is the
	// discriminator for the output-cap stall — a final "length" step with ZERO
	// output — and it is deliberately not the all-zero test above: the recorded
	// MAK-123 stall carried reasoning=32000 and a nonzero total, so only the
	// output figure sees it. lastStepReason rides onto the Result for the
	// daemon's named log line. Both move only with a step_finish that carried a
	// reason, so they always describe the same event as lastReason (deadscan's
	// rule, kept in step so the two classifiers cannot disagree).
	lastStepOut, lastStepReason           int
	total, in, out, reason, cWrite, cRead int
	cost                                  float64
	sawUsage                              bool

	// sessionID is the opencode session this stream belongs to, taken from the
	// top-level sessionID field every event carries. Constant for a run; the
	// CLA-406 resurrection path needs it as the handle a follow-up
	// `opencode run --session <id>` continues into.
	sessionID string

	// observed carries ONLY the claim/report observation state, which finish
	// copies onto the real Result. It cannot be the real Result: that one is built
	// by capture.result() after the process exits, long after the claim event went
	// past, so anything written into it during the stream would be discarded.
	observed Result
	console  io.Writer
	// settledCalls is the tool parts already acted on (by partID), so a
	// re-delivered terminal event cannot rebuild a claim that a later call has
	// since settled.
	settledCalls map[string]bool

	// toolOutputDir is where this session's opencode saves truncated tool
	// output: <data home>/opencode/tool-output, resolved from the session's own
	// environment. The only directory a truncated result is read back from.
	toolOutputDir string

	// errorType is the type of the last typed {"type":"error"} event
	// ("provider.auth", ...), recorded on Result.Raw for diagnostics and the
	// conformance suite. Classification itself reads the event text.
	errorType string
}

// newOpencodeParse builds the parser a session streams into. It exists so the
// CLAIM SEED is exercised by tests: a resumed phase never calls claim_task, so
// without newSessionResult here its Claim would be the zero value and
// Claim.Held() — the gate on the handback, the salvage and the delivery check —
// would be false for the phase that pushes the branch and opens the PR. Assigned
// inline in Invoke, deleting it would leave the whole suite green.
func newOpencodeParse(in Invocation, console io.Writer) opencodeParse {
	return opencodeParse{observed: newSessionResult(in), console: console, toolOutputDir: opencodeToolOutputDir(in)}
}

// diag is where a claim diagnostic goes. The zero opencodeParse has no console (a
// probe passes none, and the parser tests build one directly), and noteClaimed
// writes unconditionally — by design, since a claim that silently stops being
// tracked is indistinguishable from a session that never claimed.
func (p *opencodeParse) diag() io.Writer {
	if p.console == nil {
		return io.Discard
	}
	return p.console
}

func (p *opencodeParse) line(line []byte) {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "{") {
		return
	}
	var ev opencodeEvent
	if json.Unmarshal([]byte(trimmed), &ev) != nil {
		return // partial/non-JSON line — skip
	}
	// Every event names its session; the first one seen wins (they all agree
	// within a run). Taken before the Part check, because an error event carries
	// no part and can be the only event a failed run emits.
	if p.sessionID == "" && ev.SessionID != "" {
		p.sessionID = ev.SessionID
	}
	if ev.Type == "error" && ev.Error != nil && ev.Error.Type != "" {
		p.errorType = ev.Error.Type
	}
	if ev.Part == nil {
		return
	}
	switch ev.Type {
	case "step_start":
		// A new step has begun, so the previous step is no longer the final one.
		// The final step of a 2.x session (the plain answer) emits no
		// step_finish, so without this reset the last TOOL step's "tool-calls"
		// would be reported as how the session ended. With it, a session whose
		// last step never finished on the stream reports no streamed reason, and
		// the export supplies the real one.
		p.lastReason = ""
		p.lastStepOut, p.lastStepReason = 0, 0
		p.lastStepZeroUsage = false
	case "text":
		if t := strings.TrimSpace(ev.Part.Text); t != "" {
			p.lastText = t
		}
	case "step_finish":
		// The reason rides on the same part as the usage. The LAST one wins: it
		// is the step the session's stream ended on, which is the step that says
		// whether a final answer was produced.
		if ev.Part.Reason != "" {
			p.lastReason = ev.Part.Reason
			// CLA-584: the stalled step's own figures move WITH the reason, so
			// the pair always describes ONE event. A reason-less step_finish is
			// skipped for this pair exactly as deadscan skips it for its own —
			// otherwise the previous step's "length" would be paired with THIS
			// event's zero output (or its output paired with the previous
			// reason), and the live adapter would resume a session the
			// retrospective scan reads as a mere long answer.
			p.lastStepOut, p.lastStepReason = 0, 0
		}
		// tokens and cost are siblings on the part; count each independently so
		// a step that reports one without the other still lands in the budget.
		//
		// The LAST step's OWN usage is tracked separately from the session sum:
		// the CLA-398 quiet-death signature is a final step that reported
		// NOTHING (all-zero), whatever the earlier steps cost — so the sums that
		// feed the budget are not the discriminator, the last step's own block
		// is. Reset per step; the final value is the one that survives.
		p.lastStepZeroUsage = true
		if tk := ev.Part.Tokens; tk != nil {
			p.total += tk.Total
			p.in += tk.Input
			p.out += tk.Output
			p.reason += tk.Reasoning
			p.cWrite += tk.Cache.Write
			p.cRead += tk.Cache.Read
			if ev.Part.Reason != "" {
				p.lastStepOut = tk.Output
				p.lastStepReason = tk.Reasoning
			}
			if tk.Total != 0 {
				p.lastStepZeroUsage = false
			}
			p.sawUsage = true
		}
		if c := ev.Part.Cost; c != nil {
			p.cost += *c
			if *c != 0 {
				p.lastStepZeroUsage = false
			}
			p.sawUsage = true
		}
	case "tool_use":
		p.noteTool(ev.Part)
	}
}

// noteTool feeds one tool event into the shared clankerbar observation, which is
// what populates Result.Claim and the delivery Reports.
//
// Only a TERMINAL state is acted on, and that is FUTURE-PROOFING rather than a
// filter this stream needs today: `opencode run --format json` forwards a tool
// part only once it has settled (true of 1.x source, cli/cmd/run.ts, and of
// every 2.0.32 stream recorded for this adapter), which is why the recorded
// fixtures carry exactly one event per tool part. The check is here because the
// status is the ONLY error flag
// opencode gives — there is no is_error sibling — so a version that started
// forwarding in-flight parts would otherwise have us read a call that had not
// returned as a success. An unrecognised status is ignored for the same reason:
// the cost of being wrong that way is an unobserved claim (the driver hands the
// task back — one expiring lease), against a claim recorded for a call that
// failed, which is a task the driver believes it holds and does not.
//
// A tool part is acted on ONCE. noteClaimed rebuilds Result.Claim wholesale, so
// a re-delivered terminal event for a claim already settled by a later
// update_task would resurrect it — and the driver would post `ready` over a task
// that is in review. This is insurance, not an observed bug.
//
// # What this shape cannot do, and what it costs
//
// claude sets Claim.HasWIP on the REQUEST (see noteToolUse), deliberately: a
// session killed between issuing `update_task(branch: …)` and its result has
// still pushed work, and erring towards "there is WIP" only costs the reclaim an
// expiring lease already costs. opencode gives no request-side event at all — the
// arguments arrive fused to the result — so that arm cannot fire early here. A
// session killed mid-call (SIGTERM, a cancelled context) therefore records no
// WIP, and releaseHeldClaim posts `ready` over a branch that was pushed. The
// truncated-stream variant of this is caught by the capture's untrusted mark; the
// killed-mid-call one is not, and it is a property of opencode's stream rather
// than something this adapter can fix.
func (p *opencodeParse) noteTool(part *opencodePart) {
	st := part.State
	// part.Type ("tool") is deliberately not checked: only a tool part carries a
	// call id and a state, so the fields below are the discriminator.
	if st == nil || part.ID == "" {
		return
	}
	if st.Status != "completed" && st.Status != "error" {
		return
	}
	name := opencodeClankerbarTool(part.Tool)
	if name == "" {
		return
	}
	// Replays are recognised by the part id, not the call id: the call id is
	// the provider's and repeats across steps (see opencodePart), so keying on
	// it would drop a later, genuine call.
	key := part.PartID
	if key == "" {
		key = part.ID
	}
	if p.settledCalls[key] {
		return
	}
	if p.settledCalls == nil {
		p.settledCalls = map[string]bool{}
	}
	p.settledCalls[key] = true

	noteToolUse(name, part.ID, st.Input, &p.observed)
	// The result text is handed over as JSON so it reaches noteClaimed by the same
	// road claude's string-shaped tool_result content does. Marshalling a string
	// cannot fail (invalid UTF-8 is replaced, not rejected), and the error is
	// swallowed rather than returned on purpose: bailing here would strand the
	// delivery Report noteToolUse just armed, unresolved and silently dropped.
	output := st.Output
	if full, ok, err := opencodeTruncatedOutput(st, p.toolOutputDir); err != nil {
		// Fail safe: the envelope goes on as the result, which no consumer can
		// parse, so a claim is NOT tracked - the same outcome as any unreadable
		// claim result, with the reason said first.
		fmt.Fprintf(p.diag(), "  · %s result was truncated by opencode and could not be read back: %v\n", part.Tool, err)
	} else if ok {
		output = full
	}
	content, _ := json.Marshal(output)
	noteToolResult(&p.observed, part.ID, st.Status == "error", content, p.diag())
}

// opencodeTruncatedOutput returns the full text of a tool result 2.x truncated
// (see opencodeToolState), ok=false when the result was not truncated.
//
// The path comes from the event's own metadata, which opencode writes, not
// from the output text a tool or the backlog controls. It is still checked
// before anything is read, because a path that is merely read and parsed as a
// claim is a way to hand the driver a claim on a task nobody asked for (the
// threat injection_test.go covers):
//
//   - it must be a file directly inside dir, the session's own
//     <data home>/opencode/tool-output, compared after resolving symlinks;
//   - its name must start with "tool_", opencode's naming;
//   - it must be a regular file, not a symlink;
//   - the read is bounded by readSpilled, the same cap as Claude's spill.
//
// Any failure is returned as an error; the caller fails safe.
func opencodeTruncatedOutput(st *opencodeToolState, dir string) (string, bool, error) {
	if st.Metadata == nil || st.Metadata.Metadata == nil || !st.Metadata.Metadata.Truncated {
		return "", false, nil
	}
	path := st.Metadata.Metadata.OutputPath
	if path == "" || !filepath.IsAbs(path) {
		return "", false, fmt.Errorf("no absolute outputPath in the truncation metadata (%q)", path)
	}
	if dir == "" {
		return "", false, fmt.Errorf("the session's tool-output directory is unknown")
	}
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false, fmt.Errorf("tool-output directory %s: %w", dir, err)
	}
	gotDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil || gotDir != wantDir {
		return "", false, fmt.Errorf("%s is not in this session's tool-output directory %s", path, dir)
	}
	if !strings.HasPrefix(filepath.Base(path), "tool_") {
		return "", false, fmt.Errorf("%s is not an opencode tool-output file", path)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "", false, err
	}
	if !fi.Mode().IsRegular() {
		return "", false, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := readSpilled(path)
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

// opencodeToolOutputDir is <data home>/opencode/tool-output for the session in:
// XDG_DATA_HOME, else HOME/.local/share, each read from the session's own
// environment (in.Env wins over the inherited one, as it does on exec).
func opencodeToolOutputDir(in Invocation) string {
	get := func(k string) string {
		v := os.Getenv(k)
		for _, kv := range in.Env {
			if name, val, ok := strings.Cut(kv, "="); ok && name == k {
				v = val
			}
		}
		return v
	}
	data := get("XDG_DATA_HOME")
	if data == "" {
		home := get("HOME")
		if home == "" {
			return ""
		}
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(data, "opencode", "tool-output")
}

// opencodeClankerbarTool maps an opencode tool name onto the namespaced name the
// shared observer switches on, and returns "" for anything that is not a
// clankerbar tool.
//
// opencode names an MCP tool `<server>_<tool>`, where the server is the key under
// `mcp` in its config; claude names the same tool `mcp__<server>__<tool>`.
// Translating rather than keeping a second set of constants leaves ONE list of
// watched tools: add a tool there and both harnesses see it.
//
// The server name `clankerbar` is HARD-CODED, here as in claude's constants, and
// that is an assumption the driver does not enforce: it never writes an MCP config
// (the operator hand-writes opencode's, per the README), and config.Validate
// deliberately accepts a clankerbar server under any key so long as it is handed
// CLANKERBAR_API_KEY. So `mcp: {"cb": …}` validates, opencode names the tools
// `cb_claim_task`, nothing here matches, and a phased run stops after phase 1 on
// every task looking like an ordinary early finish — the exact silent half-run the
// TracksClaims gate exists to prevent. Filed rather than fixed inside this
// adapter, because the check belongs where the config is read (doctor, or a
// Validate refusal under phases), not where the tool name is parsed.
func opencodeClankerbarTool(name string) string {
	const prefix = "clankerbar_"
	if !strings.HasPrefix(name, prefix) {
		return ""
	}
	return "mcp__clankerbar__" + strings.TrimPrefix(name, prefix)
}

// finish writes what the stream added up to onto res.
func (p *opencodeParse) finish(res *Result) {
	res.FinalMessage = p.lastText
	// What the session did with the backlog. Copied rather than accumulated in
	// place because res does not exist until the process has exited; see the
	// `observed` field.
	res.Claim = p.observed.Claim
	res.Reports = p.observed.Reports
	// Recorded whatever the figures come to: the driver bounds attempts that
	// reported NOTHING, and a step_finish carrying zeros still reported (CLA-288).
	res.UsageReported = p.sawUsage
	if p.sawUsage {
		// opencode's `total` already folds in input+output+reasoning+cache, so it
		// is the honest spend figure for the budget.
		res.Tokens = p.total
		res.CostUSD = p.cost
		res.Raw = map[string]any{
			"input_tokens": p.in, "output_tokens": p.out, "reasoning_tokens": p.reason,
			"cache_write_tokens": p.cWrite, "cache_read_tokens": p.cRead,
		}
	}
	// The finish reason is recorded even when the step carried no usage: a dead
	// session's final step can report zero tokens, and that reason is exactly the
	// CLA-386 signal the driver needs to refuse to hand the phase on.
	if p.lastReason != "" {
		if res.Raw == nil {
			res.Raw = map[string]any{}
		}
		res.Raw[FinishReasonKey] = p.lastReason
		// The CLA-398 quiet-death signature: a FINAL "unknown" step whose OWN
		// usage was all-zero. The reason alone is the CLA-386 dead-phase marker,
		// but a session can end "unknown" after doing real paid work; the final
		// step reporting nothing is what makes this the shape that dies with NO
		// error event, NO stderr, and nothing produced — indistinguishable from a
		// cheap clean run until the adapter names it. The marker is the adapter's
		// own (see ZeroUsageUnknown), so the driver can log the end by name.
		//
		// The step's OWN usage, never the session total: the 2026-08-20 dead
		// sessions had done hours of paid work across earlier steps before the
		// final "unknown" step that reported nothing.
		if p.lastReason == FinishReasonUnknown && p.lastStepZeroUsage {
			res.Raw[TerminalReasonKey] = ZeroUsageReason
		}
		// CLA-584: the output-cap stall — a FINAL step that hit the output token
		// limit with ZERO output. Distinct from the quiet death above in the two
		// ways that matter: the step reported real usage (reasoning and cost),
		// and opencode ended the turn itself rather than the stream being
		// dropped, so the session is alive and resumable. The reasoning count
		// rides along because it is what makes the stall legible — thousands of
		// tokens of thinking, nothing emitted — and it is the step's OWN count,
		// like every other final-step figure here.
		if p.lastReason == FinishReasonLength && p.lastStepOut == 0 {
			res.Raw[TerminalReasonKey] = OutputCapReason
			res.Raw[OutputCapReasoningKey] = p.lastStepReason
		}
	}
	// The opencode session id, when the stream named one: the CLA-406
	// resurrection path resumes INTO this session via `opencode run --session`,
	// and needs the handle to survive onto the Result. Absent from a stream that
	// died before its first parseable event — nothing there to resume.
	if p.sessionID != "" {
		if res.Raw == nil {
			res.Raw = map[string]any{}
		}
		res.Raw[opencodeSessionIDKey] = p.sessionID
	}
	if p.errorType != "" {
		if res.Raw == nil {
			res.Raw = map[string]any{}
		}
		res.Raw[OpencodeErrorTypeKey] = p.errorType
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
	return res.scan(scanErrorText, func() string { return buildOpencodeErrorText(res) })
}

func buildOpencodeErrorText(res Result) string {
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

// opencodeFatalRe: failures that are NEVER a blip, scanned before the transient
// patterns and before the heuristic below so neither can overrule them.
//
// It exists because the transient scan reads all of stderr, which carries text
// this adapter did not author — third-party stack traces, a provider SDK's own
// error prose — and one word from that text used to be enough to call a stop a
// blip. A revoked key or a dead MCP server must end the run loudly at the first
// attempt: those are the failures an operator has to act on, and a daemon that
// retries them at the backoff ceiling instead reports "transient failure (exit
// 1) — retry 47" all night and never says what is actually wrong.
//
// Deliberately narrow: an auth/permission refusal or a hard 401/403. Anything
// vaguer belongs in neither list, where the heuristic can weigh it under a
// bound.
//
// ANCHORED for the same reason the transient arms are, and it is the reason the
// words below are never matched on their own. This scan runs FIRST, so a match
// vetoes even a recognised blip — which means a bare word here fails in the
// opposite direction to the `transport` bug: a WARN line mentioning "403
// Forbidden" beside a real "Error: 429 Too Many Requests" would stop the daemon
// for the night on a rate limit it should have ridden out. So a refusal counts
// only where the failure is actually being REPORTED: in a typed error event's
// status or message field, or after an `error:` prefix on the same line.
//
// The `authentication( \w+)?` shape is not decoration: gRPC's phrasing is
// "transport: authentication handshake failed", which is both the word that used
// to trip the transient scan and a failure that must stop.
const opencodeFatalWords = `(authentication( \w+)? (failed|error|rejected)` +
	`|unauthorized|forbidden|permission denied` +
	`|invalid api key|api key (is )?(invalid|missing|not found|expired))`

// 2.x types its errors, and two of the types are fatal by definition, verified
// against 2.0.32: `provider.auth` (a 401, exit 1, no retry) and
// `provider.no-route` (an unknown provider or model, exit 1, no request sent).
// Neither can be fixed by trying again, and a no-route carries no status code
// for the arms below to read.
var opencodeFatalRe = regexp.MustCompile(`(?i)"type": ?"provider\.(auth|no-route)"` +
	`|"status(code)?": ?40[13]` +
	`|"message": ?"[^"]*` + opencodeFatalWords +
	`|error: ?[^\n]*` + opencodeFatalWords)

// opencodeTransientRe: retryable server/network blips and API-level rate limits.
// Honours opencode's own "isRetryable":true signal, plus HTTP 408/429/5xx and the
// usual connection strings. The transport and stream arms are the CLA-381 shape:
// a stream drop arrives as a bare error event like {"type":"error",
// "error":{"type":"unknown","message":"Transport"}} — a provider-side failure, not
// a config/auth refusal, and retrying it under the existing backoff is exactly what
// riding the blip out means.
//
// The Console Go tool-schema rejection family (`unsupported_tool_schema` /
// `tool_count_limit`, anomalyco/opencode#43374 and #43378) is classified HERE,
// deliberately, rather than as a dedicated class with its own retry ladder: the
// gateway's enforcement is INCONSISTENT — #43374 records the same catalog
// accepted and refused seconds apart — so the identical retry often succeeds,
// which is exactly what the transient path does. The two codes are opaque
// gateway error names, not words a Node stack trace or agent narration would
// emit, so unlike the `transport` arm they need no word-pair anchor, and the
// assistant-text scoping below keeps a task body that merely QUOTES them out of
// the scan entirely. A persistent run of rejections is bounded separately: each
// rejected session reports no usage (the 400 fires at request time), so the
// driver's zero-spend bound stops the run after a few attempts rather than
// retrying forever.
//
// Every arm here is ANCHORED — on a JSON field, or on a word pair that only a
// failure description produces. A bare `transport` alternation was tried and
// reverted: scoping the scan to opencodeErrorText keeps the agent's narration
// out, but stderr still carries library text the adapter did not write, and the
// MCP SDK names its classes StdioClientTransport / SSEClientTransport /
// StreamableHTTPClientTransport — so any Node stack trace from a misconfigured
// MCP server (a config failure, and one this harness can now produce since
// CLA-382 wired opencode to one) matched, and a stop became an unbounded retry.
// The claude adapter anchors its provider arms on `api error:` for the same
// reason.
//
// 2.x's `provider.rate-limit` is the typed form of the 429 arm. 2.0.32 retries a
// 429 itself (about ten attempts over ~82s) before it exits 1 with that type,
// so by the time the adapter sees it the burst has lasted a minute and a half;
// it is still a rate limit and still rides the loop's backoff, and a budget
// stop worded inside it is caught first by opencodeBudgetRe.
var opencodeTransientRe = regexp.MustCompile(`(?i)"type": ?"provider\.rate-limit"` +
	`|"status(code)?": ?(408|429|5\d\d)` +
	`|"isretryable": ?true` +
	`|overloaded|too many requests|rate ?limit` +
	`|unsupported_tool_schema|tool_count_limit` +
	`|"message": ?"transport` +
	`|transport (error|failure|reset|dropped)|transport (is )?clos(ed|ing)|transport-level` +
	`|stream (error|failed|closed|reset|ended|disconnected|interrupted)` +
	`|connection error|fetch failed|econnreset|econnrefused|etimedout|eai_again|socket hang up|network (error|timeout)`)

// No turn cap: Invocation.MaxTurns never reaches the CLI, so no exit can be
// attributed to one. The wall-clock cap below is this adapter's phase backstop
// instead — a TIME budget rather than a turn one, but classified separately so
// this stays literally true and no log line calls an elapsed cap a turn cap.
func (opencode) TurnCapped(Result) bool { return false }

// wallClockReason is the terminal_reason the ADAPTER writes into a Result when
// it ended the session for outliving Invocation.MaxSessionWallClock (CLA-368).
// Like tokenCeilingReason it is deliberately not something the CLI emits: the
// marker exists so the driver can tell an orderly cap from a genuine failure,
// and opencode's stream carries no reason field a task could write into anyway.
const wallClockReason = "wall_clock_capped"

func (opencode) WallClockCapped(res Result) bool {
	if res.Raw == nil {
		return false
	}
	r, _ := res.Raw["terminal_reason"].(string)
	return r == wallClockReason
}

// markWallClockCapped writes the adapter's own cap marker onto a Result,
// PRESERVING the parsed usage already there: unlike the claude token-ceiling
// kill, this session's spend was summed from step_finish events all the way to
// the kill, and replacing Raw would throw the per-step breakdown away while the
// budget still has to see what the session cost.
func (r *Result) markWallClockCapped() {
	if r.Raw == nil {
		r.Raw = map[string]any{}
	}
	r.Raw["terminal_reason"] = wallClockReason
}

// ZeroUsageUnknown reports whether this session ended with the CLA-398
// quiet-death signature: a FINAL step_finish carrying reason "unknown" whose
// OWN usage was all-zero — whatever the earlier steps cost (the 2026-08-20 dead
// sessions had done real paid work before the silent final step). The marker is
// the adapter's own (a terminal_reason it writes, never text the CLI or an
// agent could emit), and it exists so the driver can NAME the end instead of
// logging "iteration done (tokens=0 cost=$0.00)" over a session that produced
// nothing.
func (opencode) ZeroUsageUnknown(res Result) bool {
	if res.Raw == nil {
		return false
	}
	r, _ := res.Raw[TerminalReasonKey].(string)
	return r == ZeroUsageReason
}

// OutputCapNoOutput reports whether this session ended on the CLA-584 output-cap
// stall: a FINAL step_finish carrying reason "length" whose OWN output tokens
// were zero — the model spent the step's whole output budget thinking and
// emitted nothing. Read back from the adapter's own terminal_reason marker, so
// unlike the CLA-398 quiet death this is not a question about usage being
// absent: the MAK-123 stall reported reasoning=32000 and real cost, and only
// the output figure was zero.
//
// It is the resume gate in Invoke (one resume with a steer, see
// opencode_stall.go) and the driver's marker for naming the end instead of its
// generic "never moved the task on" line.
func (opencode) OutputCapNoOutput(res Result) bool {
	if res.Raw == nil {
		return false
	}
	r, _ := res.Raw[TerminalReasonKey].(string)
	return r == OutputCapReason
}

// No mid-stream token ceiling: opencode sums per-step usage but the adapter
// never kills the process, so no exit of its can be attributed to one.
func (opencode) TokenCeilingHit(Result) bool { return false }

// TracksClaims is true as of CLA-365, and on 2.x only because the adapter takes
// every MCP server out of Code Mode (opencodeMCPOutOfCodeMode): opencode's
// `--format json` stream then carries
// each tool call's arguments AND its result on one terminal `tool_use` event, so
// the adapter reads the claim off the session the same way claude does and
// populates Result.Claim. That is what config.Validate gates `phases` on, so this
// flag is what lets a mixed-harness queue run implement on opencode.
//
// HonoursMaxTurns stays false — Invocation.MaxTurns still never reaches the CLI.
//
// ReportsCost is true: on 2.x the authoritative figure is the session export
// read after each process exits (opencode_export.go), with the step_finish
// `cost` siblings summed from the stream as the fallback when the export fails,
// so `budget.max_cost_usd` is a live ceiling here (CLA-288). The cost is only
// non-zero when the model's provider config declares a `cost` block; a model
// without one reports tokens and $0.
//
// HonoursSessionWallClock is true and HonoursMaxTurns is false, which is the
// whole point of the pair: this is the adapter whose CLI takes no turn flag, so
// its phase backstop is the elapsed-time cap Invoke enforces (CLA-368).
func (opencode) Capabilities() Capabilities {
	return Capabilities{TracksClaims: true, ReportsCost: true, HonoursSessionWallClock: true}
}

func (o opencode) IsTransient(res Result) bool {
	// Scope the scan to opencodeErrorText (stderr + {"type":"error"} events), NOT raw
	// Stdout+Stderr: the latter includes {"type":"text"} assistant narration, so a
	// session that merely *discusses* a rate limit / connection error before exiting
	// non-zero would be retried as transient instead of surfacing the real failure —
	// the same reason DetectLimit scopes its scan this way.
	text := opencodeErrorText(res)
	// A named stop wins over every retry path below, including the heuristic. The
	// loop already runs DetectLimit before IsTransient, so the budget arm is
	// belt-and-braces there — but this function must not answer "retryable" about
	// an exhausted account just because it happens to be asked out of that order.
	// The braces are why the loop's ordering has no test of its own: with both
	// classifiers agreeing, no order produces a retry, so there is nothing left for
	// a test to distinguish (CLA-381).
	if opencodeBudgetRe.MatchString(text) || opencodeFatalRe.MatchString(text) {
		return false
	}
	if opencodeTransientRe.MatchString(text) {
		return true
	}
	return o.IsUnclassifiedTransient(res)
}

// IsUnclassifiedTransient is the CLA-381 heuristic, and it is separate from the
// pattern scan because a caller has to be able to bound it — see
// harness.Adapter.IsUnclassifiedTransient for why that is a spending requirement and not a
// tidiness one.
//
// The judgement: an exit 1 whose cause no pattern has seen yet, on a session that
// REPORTED usage, leans retryable. A usage event means the session authenticated
// and started doing paid work, so the failure came after the startup checks and
// is more likely a version-shaped stream drop this adapter has no pattern for
// than a config/auth refusal — that kind fails BEFORE any usage arrives, which is
// why the no-usage case stays a stop. The opencode error surface moves between
// versions (the CLA-381 incident was a nightly emitting an untyped event), so the
// heuristic is what covers the shapes the patterns above have not caught up with.
//
// What it must NOT be read as is a bounded retry in its own right. It names no
// failure, so a DETERMINISTIC post-usage crash satisfies it on every attempt, and
// the caller's ordinary dials do not stop that: max_retries and budget are both
// off by default, and max_zero_spend_attempts cannot fire at all here, since its
// counter resets on exactly the reported usage this requires. The loop bounds it
// separately.
func (opencode) IsUnclassifiedTransient(res Result) bool {
	if res.ExitCode != 1 || !res.UsageReported {
		return false
	}
	text := opencodeErrorText(res)
	if opencodeBudgetRe.MatchString(text) || opencodeFatalRe.MatchString(text) {
		return false
	}
	return !opencodeTransientRe.MatchString(text)
}

// Diagnostic returns the scoped text IsTransient's PATTERN scans judged. On the
// heuristic path there is no matching text to point at — the verdict came from
// the exit code and whether usage was reported — so this is the text that was
// searched, not in every case the evidence for the answer. See the claude
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
	return probeVerdict(out, res, o.DetectLimit)
}

func (opencode) ReadUsage(context.Context, Invocation) (Usage, error) {
	// opencode surfaces no remaining-quota/balance figure to a headless caller;
	// billing lives with the configured provider. The loop falls back to the
	// self-accounted Budget breaker.
	return Usage{}, ErrUsageUnsupported
}
