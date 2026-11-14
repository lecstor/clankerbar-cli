package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
//     (OpenRouter / Zen), a monthly-limit subscription (opencode Go), or the Command
//     Code plan's rolling usage windows — 5-hour, daily and weekly. The first two
//     are hard caps; a rolling window names the cap and usually states its reset,
//     so DetectLimit reports it as a WAITABLE limit (Limit.Stop unset, ResetAt
//     parsed when stated, Limit.Window labelled) and the loop supervises the wait.
//     The window is read from the provider's structured `rateLimit.window` in the
//     response body opencode passes through verbatim, falling back to the message
//     wording. The hard cases (402 / "out of credits" / "monthly limit reached")
//     still set Limit.Stop so the loop stops cleanly rather than waiting for a
//     reset that never comes. The self-accounted budget breaker is the primary
//     control; ReadUsage is unsupported.
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
// dialect, still accepted) or `mcp.servers` (2.x dialect). A Claude-shaped
// file is a different schema: 1.18.2 refused to start on it, and 1.18.18 and
// 2.0.32 silently skip its `mcpServers` key, so the session gets no clankerbar
// tools. config.ResolveMCPConfig never hands opencode a .mcp.json; a
// Claude-shaped file under another name is a doctor FAIL (CLA-263).
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
	if len(in.Turns) > 0 {
		// A planned multi-turn session (CLA-867): the follow-up turns run in
		// this same session before any recovery pass, because a turn that ends
		// badly must stop the sequence rather than be resumed behind the
		// caller's back. A completed sequence hands back the FINAL turn's
		// invocation: everything below judges the turn that actually ended the
		// session, never the first one's model or route list.
		var completed bool
		in, completed, err = o.runTurns(ctx, in, &res, deadline)
		if err != nil {
			return res, err
		}
		if !completed {
			return res, nil
		}
	}
	if res.ToolLoop != "" || res.IdleKilled != "" {
		// CLA-739 / the idle watchdog: killed for a cause of the adapter's own.
		// Neither recovery below applies - a stall resume or a resurrection
		// continues the same transcript, and the session was not silently dead,
		// it was stopped. Resuming would re-send all of it at full price.
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
	// A silent 130 gets one resume before the route switch, so a quota error
	// the resume surfaces is switched on evidence rather than a guess.
	o.resumeInterrupted(ctx, in, &res, deadline)
	// CLA-782: a usage limit on one model route is not the end of the session
	// when the config named a fallback. Runs BEFORE the resurrection loop (a
	// switch resumes the same session, and a later quiet death then resurrects on
	// the route that was working), never on a probe, and leaves a limit on the
	// last route untouched for the driver's existing limit branch.
	if !in.Probe {
		if err := o.switchRoutesOnLimit(ctx, in, in.RouteList(), &res, deadline); err != nil {
			return res, err
		}
		// Keep later resumes on the route that actually ran: the resurrection
		// probe and continuation below read in.ModelArg(), and resuming on the
		// route that was just denied would only re-hit its limit.
		if finished := RouteFinished(res); finished != "" {
			in = in.OnRoute(finished)
		}
	}
	// An UNTRUSTED stream is never resurrected: CLA-262 forbids reading claim
	// state off an over-run capture, and both the coherence ref and the
	// continuation's seed come from exactly that state - while the driver's
	// own untrusted path declines to count further spend, so probing would
	// enlarge the accounting blind spot endUntrustedDrain exists to bound.
	// The conjunct mirrors the driver's own gate (!capped && !ceiling &&
	// !wallclock && untrusted) so adapter and tallyDead agree on what is dead.
	for attempt := 1; attempt <= opencodeMaxResurrections && o.ZeroUsageUnknown(res) && res.Untrusted == "" && res.ToolLoop == ""; attempt++ {
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
		res.Usage.Add(probeRes.Usage)
		res.UsageReported = res.UsageReported || probeRes.UsageReported
		res.CostReported = res.CostReported || probeRes.CostReported
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
		// the new turn, so without the seed every progress write the continuation
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
			res.Usage.Add(contRes.Usage)
			res.UsageReported = res.UsageReported || contRes.UsageReported
			res.CostReported = res.CostReported || contRes.CostReported
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
	// Last, so it judges the merged result of every recovery above and resumes
	// on the route that finished. One resume; a second refusal is the runner's.
	o.resumeBadReport(ctx, in, &res, deadline)
	if len(res.TurnMessages) > 0 {
		// The recovery passes above resume the FINAL turn, so its message is
		// whatever the session said last: the per-turn record may not keep the
		// pre-recovery text beside a corrected FinalMessage.
		res.TurnMessages[len(res.TurnMessages)-1] = res.FinalMessage
	}
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

// opencodePrompt is the message a working (non-probe) run sends. It is the
// caller's prompt unchanged.
//
// It USED to prepend an MCP warm-up note ("MCP servers connect in the
// background; call the tool again if it is missing"). CLA-703 removed the race
// that note described: the adapter now starts its own server, waits until the
// clankerbar server reports connected, and only then attaches the run, so the
// first model turn already has the tools. Keeping the note would tell every
// session to expect a failure that no longer happens, and would spend prompt
// bytes on a workaround the code no longer needs.
//
// The function is kept (rather than inlined at its two call sites) because the
// prompt shape is a contract this adapter owns, and the tests assert the
// absence of the note through it.
func opencodePrompt(prompt string) string {
	return prompt
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
	// opencode 2.2 built-ins are named like MCP tools, so `*_*` would allow them,
	// and worktree_create wrote outside the workdir. Deny the family; keep the MCP
	// resource reads (see the function doc). Shadows an MCP server named "opencode".
	perm["opencode_*"] = "deny"
	perm["opencode_list_mcp_resources"] = "allow"
	perm["opencode_read_mcp_resource"] = "allow"
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

// withoutAmbientOpencodeConfig drops an inherited OPENCODE_CONFIG: the driver
// decides which file a session is handed, and an ambient value would hand one
// over even when config resolved none (CLA-318). An explicit in.Env value still
// applies, since it is appended later.
func withoutAmbientOpencodeConfig(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "OPENCODE_CONFIG=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (opencode) env(in Invocation) ([]string, error) {
	// The session's INSTANCE directory. opencode reads $PWD, not just the real
	// cwd, so this is what decides which project-level opencode.json it merges
	// and which git worktree its read/edit asks are relative to - see pinPWD.
	env := pinPWD(inheritEnv(withoutAmbientOpencodeConfig(os.Environ()), in.EnvAllowlist), in.WorkDir)
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
	// it. A Claude-shaped .mcp.json is a different schema and is gated out by
	// config.ResolveMCPConfig before it gets here (CLA-318), so empty exports nothing.
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
	// CLA-877: a hung provider stream must ERROR, not stay silent until the
	// phase idle watchdog kills the whole session. See opencode_timeout.go.
	opencodeProviderTimeouts(in, doc)
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
	// CLA-703: how many clankerbar calls got a successful result, copied for the
	// same reason as the claim — the parser's observation is stream-side state
	// until the process ends.
	res.ClankerbarCalls = p.observed.ClankerbarCalls
	// Recorded whatever the figures come to: the driver bounds attempts that
	// reported NOTHING, and a step_finish carrying zeros still reported (CLA-288).
	res.UsageReported = p.sawUsage
	// The cost figure is its own question (CLA-691): a stream whose steps carried
	// tokens but no cost reported no money, and the durable record must say so
	// rather than reporting 0.
	res.CostReported = p.sawCost
	if p.sawUsage {
		// opencode's `total` already folds in input+output+reasoning+cache, so it
		// is the honest spend figure for the budget.
		res.Tokens = p.total
		res.CostUSD = p.cost
		// The normalised breakdown (CLA-691): opencode's step tokens are already
		// disjoint across exactly these five buckets, so this is the report
		// itself, summed over the session's steps.
		res.Usage = TokenUsage{
			Input:      p.in,
			Output:     p.out,
			Reasoning:  p.reason,
			CacheRead:  p.cRead,
			CacheWrite: p.cWrite,
		}
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
	// CLA-700: the events this session RECOVERED from, with no error event ending
	// the stream. Recorded as a positive count so the driver can tell "nothing
	// happened" from "provider blips were ridden out" — the difference between an
	// exit status that means the session failed and one that is upstream's own
	// bug (2.x exits non-zero after recovering, pinned by the conformance suite).
	if p.recoveredErrors > 0 && !p.lastEventWasError {
		if res.Raw == nil {
			res.Raw = map[string]any{}
		}
		res.Raw[RecoveredErrorsKey] = p.recoveredErrors
	}
}

// opencodeBudgetRe recognizes a HARD budget/credit-exhaustion stop from the
// configured provider: out of credits, spend-cap or monthly/billing-limit
// reached, or an HTTP 402 Payment Required. This is a STOP (Limit.Stop) — there
// is no window to wait out.
//
// It carries only phrasings that NAME a hard condition. The generic wordings
// that name an exhausted quota without saying which window it is — "usage limit
// reached", "insufficient quota" — live in opencodeGenericLimitRe, because the
// SAME words arrive on a rolling subscription window that a wait can outlast: a
// provider.quota event whose text is "Usage limit reached ... resets at ..." was
// a Stop here and threw the stated reset away (CLA-736 review). A rolling
// subscription quota (an N-hour/N-minute/session/daily usage limit, or
// error.type "provider.quota") is the separate, WAITABLE case, matched by
// opencodeRollingQuotaRe; a plain 429 "rate limit" with no quota wording is a
// transient blip (IsTransient), not this.
var opencodeBudgetRe = regexp.MustCompile(`(?i)"status(code)?": ?402` +
	`|payment required` +
	`|out of (credit|credits|balance)` +
	`|insufficient (credit|credits|fund|funds|balance)` +
	`|(credit|token) balance (is )?(too low|depleted)` +
	`|spend[ -]?(cap|limit)|spending limit` +
	`|monthly (usage )?limit|billing (hard )?limit`)

// opencodeGenericLimitRe names an exhausted quota WITHOUT naming a window or a
// hard condition: the legacy "usage limit reached", and "insufficient
// [provider] quota". On its own that is still a Stop — nothing in it says the
// window lifts — but DetectLimit lets the rolling classification win when the
// same event is typed provider.quota or states a reset instant, which is the
// whole difference between a top-up and a wait (CLA-736 review).
var opencodeGenericLimitRe = regexp.MustCompile(`(?i)usage limit reached` +
	`|insufficient (?:[\w-]+ )?quota`)

// opencodeRollingQuotaRe recognizes the OTHER kind of provider limit: a rolling
// subscription window that a wait can outlast (CLA-736). The Command Code plan's
// 5-hour quota arrives as an HTTP 429 typed `provider.quota` and states the
// reset instant in its message; the loop supervises that wait and re-runs the
// session once the window lifts, so this must be reported as Limited with Stop
// UNSET rather than falling through to the transient ladder.
//
// Three arms, all read from opencodeErrorText (stderr + a TERMINAL error event),
// never the agent's narration:
//   - 2.x types it: error.type == "provider.quota". This alone is enough — the
//     type is the provider's own statement that the cap is a quota it will lift,
//     so a message carrying no duration/window wording at all is still waitable.
//   - the message names the window BEFORE the limit word: "5-hour usage limit",
//     "Your 5-hour limit has been reached", "30-minute usage limit", "session
//     usage limit", "daily usage limit", "weekly usage limit". The word "usage"
//     is not required — an untyped provider that calls it a "5-hour limit" is
//     the same window. The weekly arm is not optional: it is the window this
//     task adds, and a prose weekly cap must classify exactly like its sibling
//     windows instead of reading as no limit at all (review 1.review-1.1).
//   - the message names the window AFTER the limit word: "Your usage limit has
//     been reached (5-hour window)". Same statement with the clause order
//     swapped; without this arm it matches neither the window-first arm (the
//     limit must follow the window) nor the generic quota arm ("usage limit
//     reached" wants those words adjacent), so a waitable window rode the
//     transient ladder instead of a supervised wait (review 1.review-2.2).
//
// Deliberately narrow. A bare 429 "rate limit" names no window and stays
// transient, and the hard budget arms above are checked FIRST — so a monthly cap
// typed as provider.quota is still a Stop.
var opencodeRollingQuotaRe = regexp.MustCompile(`(?i)"type": ?"provider\.quota"` +
	`|\b\d+\s*[- ]?\s*(hour|hr|minute|min)s?\b[^.\n"]{0,40}\blimit\b` +
	`|\b(?:limit|quota)\b[^.\n"]{0,40}\b\d+\s*[- ]?\s*(hour|hr|minute|min)s?\b` +
	`|\b(session|daily|weekly)\s+usage\s+limit`)

// opencodeResetRe captures the reset instant a rolling quota STATES as RFC3339,
// e.g. "Your limit resets at 2026-10-20T15:56:48.251Z." When the message does not
// state one the parse returns the zero time and the supervised wait falls back to
// interval probing — a missing reset must not demote the limit to "no limit".
var opencodeResetRe = regexp.MustCompile(`(?i)resets?\s+at\s+` +
	`(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}))`)

// opencodeResetInRe captures a RELATIVE reset a rolling quota states by duration,
// e.g. "Usage limit reached. Resets in 4 hours." or "reset in 45m". The same
// provider family emits either form, and a relative window is the same statement
// of when the limit lifts — the classifier's own rule is that "a message that
// says when it lifts is a wait" (review 1.review-2.2), and reading only the
// absolute form turned the wait into an engineer-visible stop.
var opencodeResetInRe = regexp.MustCompile(`(?i)resets?\s+in\s+(\d+)\s*(hours?|hrs?|minutes?|mins?|h|m)\b`)

func parseOpencodeReset(s string) time.Time { return parseOpencodeResetAt(s, time.Now()) }

// parseOpencodeResetAt is parseOpencodeReset with an injectable "now" for
// testing — the same split parseClaudeResetAt uses. "now" is the drain's now: a
// relative window is resolved at the moment the session's diagnostics are
// classified.
func parseOpencodeResetAt(s string, now time.Time) time.Time {
	if m := opencodeResetRe.FindStringSubmatch(s); m != nil {
		if t, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
			return t
		}
	}
	m := opencodeResetInRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return time.Time{}
	}
	switch strings.ToLower(m[2])[0] {
	case 'h':
		return now.Add(time.Duration(n) * time.Hour)
	case 'm':
		return now.Add(time.Duration(n) * time.Minute)
	}
	return time.Time{}
}

func (opencode) DetectLimit(res Result) Limit {
	// Scan only the harness's own diagnostics, NOT the agent's narration: with
	// --format json the assistant's text is a {"type":"text"} part on stdout, so a
	// task that merely discusses billing ("we're out of credits") must not trip a
	// terminal stop. Budget errors arrive as a {"type":"error"} event (or plain
	// text on stderr), which is what opencodeErrorText gathers.
	text := opencodeErrorText(res)
	// Hard budget stops keep winning (CLA-736 settled 2): a monthly cap or an
	// exhausted balance has no window to wait out, however the event is typed.
	if opencodeBudgetRe.MatchString(text) {
		return Limit{Limited: true, Stop: true, Reason: "budget_exhausted"}
	}
	// CLA-752: the provider's own STRUCTURED statement comes before its wording.
	// The terminal error event carries the provider's response body verbatim
	// (opencode 2.0.32 stores it under error.response.body), so a 429 that names
	// its window there is classified from the window and the code, not from
	// prose that may be reworded.
	facts := opencodeErrorFactsOf(res)
	if window, planLimit := opencodeWindowLimit(facts, text); planLimit {
		return Limit{Limited: true, ResetAt: parseOpencodeReset(text), Reason: "usage_limit", Window: window}
	}
	// A rolling subscription quota is a LIMIT the loop waits out, so Stop stays
	// false. ResetAt is the provider's own stated instant when present.
	if opencodeRollingQuotaRe.MatchString(text) {
		return Limit{Limited: true, ResetAt: parseOpencodeReset(text), Reason: "usage_limit", Window: opencodeNamedWindow(text)}
	}
	// The generic exhausted-quota phrasings are a Stop only when nothing makes
	// the limit waitable. A provider.quota type or a rolling window is the case
	// above; here the one remaining signal is a STATED reset — absolute
	// ("resets at <RFC3339>") or relative ("resets in 4 hours") — a message that
	// says when it lifts is a wait, not a top-up, so it must not be turned into a
	// hard stop by wording that names no hard condition (CLA-736 review,
	// 1.review-2.2).
	if opencodeGenericLimitRe.MatchString(text) {
		if reset := parseOpencodeReset(text); !reset.IsZero() {
			return Limit{Limited: true, ResetAt: reset, Reason: "usage_limit"}
		}
		return Limit{Limited: true, Stop: true, Reason: "budget_exhausted"}
	}
	return Limit{}
}

// opencodeRateLimitWindows maps the provider's structured `rateLimit.window`
// token onto the label the loop reports. The tokens are Command Code's own —
// `fiveHour`, `weekly`, `daily` — not opencode's. Keys are normalised
// (lower-case, separators stripped) because the provider's spelling is not
// stable: `FiveHour` and `five-hour` are the documented token too, and an
// exact-match map made those variants invisible, which is how a plan cap rode
// the transient ladder (review 1.review-1.4). The mapping stays deliberately
// closed: an unrecognised token names no window this adapter understands, and
// guessing one is how a hard stop becomes an unbounded wait.
var opencodeRateLimitWindows = map[string]string{
	"fivehour": "5-hour",
	"weekly":   "weekly",
	"daily":    "daily",
}

// opencodeWindowToken normalises a structured window token before the closed
// map lookup: case- and separator-insensitive, so `fiveHour`, `five_hour` and
// `five-hour` are one window rather than one known token and two unknown ones.
func opencodeWindowToken(s string) string {
	return strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(strings.TrimSpace(s)))
}

// opencodeLimitWindowPhrase is the plan-limit wording a provider uses when it
// names no window in its body. It is the message-only fallback (CLA-752): a
// weekly statement labels weekly, and the plan's default cap is the 5-hour one.
const opencodeLimitWindowPhrase = "usage limit for your plan"

// opencodeWindowLimit resolves the plan limit a failure states: the LABEL of
// the usage window it names (empty when it names none this adapter knows), and
// whether the failure is a plan limit at all. Two sources, in order:
//
//  1. The STRUCTURED `rateLimit.window` from the provider's response body —
//     top level or nested under the body's `error` object. Its token is
//     authoritative: when it names a window the closed map does not know, the
//     window is UNKNOWN — the wording is not consulted over it, because a
//     window the provider named is not something prose may relabel, and the
//     plan's default is a guess the map's own contract forbids (reviews
//     1.review-1.4, 1.review-2.2).
//  2. The message's "usage limit for your plan" phrase, which is the same
//     statement in prose: weekly when it says weekly, otherwise the plan's
//     default 5-hour window.
//
// Only a rate limit can be a PLAN limit: the structured path requires a 429
// status or the RATE_LIMITED code, and the wording path requires the same. A
// window seen on a 5xx is not a plan cap, and a 429 that names no window stays
// the transient burst limit IsTransient already handles — that is the difference
// between waiting out a plan cap and backing off a burst.
func opencodeWindowLimit(f opencodeErrorFacts, text string) (window string, planLimit bool) {
	rateLimited := f.Status == 429 || strings.EqualFold(f.Code, "RATE_LIMITED")
	if !rateLimited {
		return "", false
	}
	if f.Window != "" {
		// The body NAMED a window. The closed map decides the label — including
		// "no label" for a token it does not know. The failure is still a plan
		// limit (a rate-limited failure that names a window is one by the task's
		// own rule); what it is NOT is the 5-hour default, which would label a
		// cap the provider never described as 5-hour and turn an unknown window
		// into an unbounded wait (reviews 1.review-1.4, 1.review-2.2).
		return opencodeRateLimitWindows[opencodeWindowToken(f.Window)], true
	}
	lower := strings.ToLower(text)
	if !strings.Contains(lower, opencodeLimitWindowPhrase) {
		return "", false
	}
	if window := opencodeNamedWindow(text); window != "" {
		return window, true
	}
	return "5-hour", true
}

// opencodeNamedWindow labels a window the text NAMES, when it names one. It is
// the wording arm's label, used where a rolling window is recognised by its
// shape rather than by the structured token: an untyped "daily usage limit" is
// the daily window. A bare "30-minute usage limit" names a duration, not one of
// the plan's windows, and stays unlabelled rather than being called 5-hour.
//
// The window word only counts inside the limit STATEMENT — the same clause as a
// limit/quota word (either side of it, since the provider swaps the order). The
// scan reads the whole diagnostic text, which includes the terminal error event
// and the provider's raw body, so a stray "weekly" in a session id or a body
// field must not relabel the cap (review 1.review-1.6).
var opencodeNamedWindowRe = regexp.MustCompile(`(?i)\b(weekly|daily|5[- ]hour)\b[^.\n"]{0,40}\b(?:usage\s+)?(?:limit|quota)\b` +
	`|\b(?:usage\s+)?(?:limit|quota)\b[^.\n"]{0,40}\b(weekly|daily|5[- ]hour)\b`)

func opencodeNamedWindow(text string) string {
	m := opencodeNamedWindowRe.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	token := m[1]
	if token == "" {
		token = m[2]
	}
	switch strings.ToLower(token) {
	case "weekly":
		return "weekly"
	case "daily":
		return "daily"
	}
	return "5-hour"
}

// opencodeErrorFacts is what the terminal error event states STRUCTURALLY:
// fields a caller can trust without reading prose. The zero value means the
// stream carried no terminal error event, or one whose JSON did not parse.
type opencodeErrorFacts struct {
	// Status is the HTTP status the wrapper reported (error.status, or the older
	// APIError shape's error.data.statusCode).
	Status int
	// Code is the provider body's machine-readable error code, e.g.
	// RATE_LIMITED. It is the body's, not opencode's error.type.
	Code string
	// Window is the raw `rateLimit.window` token, when the body carried one.
	Window string
}

// opencodeErrorFactsOf reads the structured facts out of a finished Result's
// terminal error event.
//
// What survives opencode's wrapping is verified against the 2.0.32 binary and
// the 2026-10-20 capture in testdata/opencode-quota-five-hour.jsonl: the
// provider's message and status are lifted onto `error`, and its raw response
// body is passed through as a JSON STRING under `error.response.body` (the
// binary's provider error is built with `response:{body:…}`). The older
// APIError shape puts the same body under `error.data.responseBody`. Neither is
// re-parsed by opencode, so `code` and `rateLimit` are only reachable by
// decoding that string here.
//
// Wrapper first, body second: the transport's own `error.status` is the status
// the request actually failed with, while a provider body may carry an envelope
// `status: 200` (or any other non-HTTP value) that says nothing about the
// failure. Filling the status from the body first masked a wrapper 429 and sent
// a plan cap down the transient ladder (review 1.review-1.5). Code and window
// live only in the body, so the wrapper merge leaves those holes for it.
func opencodeErrorFactsOf(res Result) opencodeErrorFacts {
	line := opencodeTerminalErrorLine(res)
	if line == "" {
		return opencodeErrorFacts{}
	}
	var ev map[string]any
	if json.Unmarshal([]byte(line), &ev) != nil {
		return opencodeErrorFacts{}
	}
	errObj := opencodeJSONObject(ev["error"])
	dataObj := opencodeJSONObject(errObj["data"])

	f := opencodeMergeFacts(opencodeFactsFromObject(errObj), opencodeFactsFromObject(dataObj))
	for _, raw := range []any{
		opencodeJSONObject(errObj["response"])["body"],
		dataObj["responseBody"],
		ev["responseBody"],
		errObj["body"],
	} {
		body, ok := raw.(string)
		if !ok || body == "" {
			continue
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(body), &decoded) != nil {
			continue
		}
		f = opencodeMergeFacts(f, opencodeFactsFromObject(decoded))
		// The provider's body is normally an {error:{…}} envelope itself.
		f = opencodeMergeFacts(f, opencodeFactsFromObject(opencodeJSONObject(decoded["error"])))
	}
	return f
}

// opencodeFactsFromObject reads the three structured fields out of one decoded
// JSON object. Absent or wrongly-typed fields stay zero, and a `status` outside
// the HTTP range is not a status at all (review 1.review-1.5).
func opencodeFactsFromObject(o map[string]any) opencodeErrorFacts {
	var f opencodeErrorFacts
	if n, ok := opencodeJSONNumber(o["status"]); ok && n >= 100 && n <= 599 {
		f.Status = n
	} else if n, ok := opencodeJSONNumber(o["statusCode"]); ok && n >= 100 && n <= 599 {
		f.Status = n
	}
	if s, ok := o["code"].(string); ok {
		f.Code = s
	}
	if rl := opencodeJSONObject(o["rateLimit"]); rl != nil {
		if w, ok := rl["window"].(string); ok {
			f.Window = w
		}
	}
	return f
}

// opencodeMergeFacts keeps base's facts and fills only its holes from next.
func opencodeMergeFacts(base, next opencodeErrorFacts) opencodeErrorFacts {
	if base.Status == 0 {
		base.Status = next.Status
	}
	if base.Code == "" {
		base.Code = next.Code
	}
	if base.Window == "" {
		base.Window = next.Window
	}
	return base
}

// opencodeJSONObject is a decoded JSON object, or nil for anything else.
func opencodeJSONObject(v any) map[string]any {
	o, _ := v.(map[string]any)
	return o
}

// opencodeJSONNumber is a decoded JSON number as an int, or false.
func opencodeJSONNumber(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// opencodeErrorText collects the harness-level diagnostic text — all of stderr,
// plus a TERMINAL {"type":"error"} event on stdout — so a limit scan sees
// provider/transport errors but never the agent's own assistant text.
func opencodeErrorText(res Result) string {
	return res.scan(scanErrorText, func() string { return buildOpencodeErrorText(res) })
}

// buildOpencodeErrorText appends the stream's error event ONLY when it is how
// the session ENDED — the last typed event on stdout (CLA-700).
//
// 2.x emits a provider error mid-session, retries the request itself and carries
// on: a stream whose error event is followed by a step_start / tool_use / text
// part is a session that RECOVERED, and calling that session failed or
// retryable is how a delivered phase got classified as a failed attempt (both
// 2026-10-19 incidents). So an error event with any typed event after it is
// recovery and contributes nothing here; only a stream that stops on the error
// is a failure. The conformance suite's recovered-provider-error case pins the
// recovery against the real binary (2.0.x error event; 2.2.x retry event).
//
// stderr is scanned unconditionally: it is the harness's own diagnostic channel,
// written as the process winds down, and the fallback for the untyped failures
// the typed-event scan cannot see.
//
// Note this is deliberately stricter than opencodeErrorType (the Raw
// diagnostic key), which records the LAST error event whatever followed it —
// that key is testimony about the stream, not a classification.
func buildOpencodeErrorText(res Result) string {
	var b strings.Builder
	b.WriteString(res.Stderr)
	// The error event is appended ONLY when the stream ENDED on it — the rule
	// opencodeTerminalErrorLine owns, shared with the structured fact scan so
	// the two can never disagree about which event is the failure.
	if line := opencodeTerminalErrorLine(res); line != "" {
		b.WriteByte('\n')
		b.WriteString(line)
	}
	return b.String()
}

// opencodeTerminalErrorLine returns the LAST typed event on the stream when it
// is a {"type":"error"} event, verbatim; "" otherwise.
//
// 2.x emits provider errors mid-session, retries the request itself and carries
// on: a stream whose error event is followed by a step_start / tool_use / text
// part is a session that RECOVERED, and calling that session failed or retryable
// is how a delivered phase got classified as a failed attempt (both 2026-10-19
// incidents). So an error event with any typed event after it contributes
// nothing.
func opencodeTerminalErrorLine(res Result) string {
	last := ""
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Type == "" {
			continue
		}
		last = line
	}
	if last == "" {
		return ""
	}
	var ev struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(last), &ev) != nil || ev.Type != "error" {
		return ""
	}
	return last
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
// it is still a rate limit and still rides the loop's backoff, and a budget stop
// or a rolling quota window worded inside it is caught first by opencodeBudgetRe
// / opencodeRollingQuotaRe.
var opencodeTransientRe = regexp.MustCompile(`(?i)"type": ?"provider\.rate-limit"` +
	`|"status(code)?": ?(408|429|5\d\d)` +
	`|"isretryable": ?true` +
	`|overloaded|too many requests|rate ?limit` +
	`|unsupported_tool_schema|tool_count_limit` +
	`|"message": ?"transport` +
	`|transport (error|failure|reset|dropped)|transport (is )?clos(ed|ing)|transport-level` +
	`|stream (error|failed|closed|reset|ended|disconnected|interrupted)` +
	`|timed out waiting for response data|sse read timed out` +
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
	// an exhausted account just because it happens to be asked out of that order,
	// and a rolling quota is the same shape: it is WAITED, not retried. The
	// generic quota phrasings belong here even though DetectLimit may make them
	// waitable on a stated reset: either way there is nothing to RETRY, and a 429
	// saying "usage limit reached" must never ride the transient ladder. The
	// braces are why the loop's ordering has no test of its own: with both
	// classifiers agreeing, no order produces a retry, so there is nothing left
	// for a test to distinguish (CLA-381).
	if opencodeBudgetRe.MatchString(text) || opencodeRollingQuotaRe.MatchString(text) ||
		opencodeGenericLimitRe.MatchString(text) || opencodeFatalRe.MatchString(text) {
		return false
	}
	// CLA-752: the same veto for a plan cap stated STRUCTURALLY. A rate-limited
	// failure whose body names a usage window — or that says "usage limit for
	// your plan" — is a limit the loop WAITS out; DetectLimit says so from the
	// same function, and the two classifiers agreeing is what makes the loop's
	// DetectLimit-first ordering safe (CLA-381, review 1.review-1.2). Without
	// this, the body-only shape was "limited" to one classifier and "retryable"
	// to the other, so a caller that asked this one alone retried a plan cap as
	// if it were a burst.
	if _, planLimit := opencodeWindowLimit(opencodeErrorFactsOf(res), text); planLimit {
		return false
	}
	// CLA-700: a session that RECOVERED from its provider errors is not retryable,
	// however its exit status reads. 2.x emits the error, retries the request
	// itself and finishes the work, then exits 1 anyway (pinned by the conformance
	// suite), so there is no failure here for a retry to ride out — the question
	// this exit raises is whether the session DELIVERED, and that is the driver's
	// to answer, not a classifier's. Both the pattern scan below and the
	// usage-gated heuristic are skipped: the heuristic exists to catch failures no
	// pattern names, and this one is named (and it is not a failure).
	if RecoveredErrors(res) > 0 {
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
	if opencodeBudgetRe.MatchString(text) || opencodeRollingQuotaRe.MatchString(text) ||
		opencodeGenericLimitRe.MatchString(text) || opencodeFatalRe.MatchString(text) {
		return false
	}
	// CLA-752: IsTransient's structural veto, for the same reason — the
	// heuristic must not launder a body-named plan cap into a bounded guess
	// (review 1.review-1.2).
	if _, planLimit := opencodeWindowLimit(opencodeErrorFactsOf(res), text); planLimit {
		return false
	}
	// CLA-700: the same rule as IsTransient's — a session that recovered from its
	// error events has no unrecognised failure to guess at, so the heuristic must
	// not launder its exit status into a retry.
	if RecoveredErrors(res) > 0 {
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
