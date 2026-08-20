package harness

import (
	"context"
	"encoding/json"
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
//     policy), not a run flag. We fail closed: a drain allows read/edit/bash within
//     the run's workdir subtree (path-scoped PermissionConfig — the session's
//     worktrees live under it) but denies the exfil tools (webfetch/websearch) and
//     everything outside the subtree; a probe / read-only run denies edits and
//     shell too. (We never pass --auto, which blanket-approves everything not
//     explicitly denied.)
//   - Billing depends on the configured backend, not opencode: metered pay-per-token
//     (OpenRouter / Zen) or a monthly-limit subscription (opencode Go). NONE impose a
//     short rolling-window cap, so there is no supervised-wait/early-reset case here.
//     DetectLimit instead recognizes a HARD budget/credit-exhaustion stop (402 /
//     "out of credits" / "monthly limit reached") and flags Limit.Stop so the loop
//     stops cleanly rather than waiting for a reset that never comes. The
//     self-accounted budget breaker is the primary control; ReadUsage is unsupported.
type opencode struct{}

func (opencode) Name() string { return "opencode" }

// opencode DOES receive mcp_config_path - as OPENCODE_CONFIG (see env below) -
// and that is exactly why "does the adapter hand it over" was the wrong question
// to gate a preflight on. What arrives has to be an opencode config, whose
// servers live under `mcp`; the Claude-shaped `.mcp.json` that config.Validate
// auto-discovers from the workdir is a different schema, and opencode does not
// ignore the difference. Verified against opencode 1.18.2:
//
//	$ OPENCODE_CONFIG=<dir>/.mcp.json opencode run --format json ... -- hi
//	Error: Configuration is invalid at <dir>/.mcp.json
//	  Unrecognized key: mcpServers
//
// So every session dies at startup, which is why doctor treats that combination
// as a FAIL rather than a missing-tools WARN (CLA-263).
func (opencode) MCPConfigUse() MCPConfigUse {
	return MCPConfigUse{
		Schema: MCPConfigNative,
		Note: "the opencode adapter passes mcp_config_path as OPENCODE_CONFIG, which must be an opencode " +
			"config (servers under `mcp`, not Claude's `mcpServers`) - opencode refuses to start on a file it cannot parse",
	}
}

func (o opencode) Invoke(ctx context.Context, in Invocation) (Result, error) {
	args := opencodeArgs(in)

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
	if in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	cmd.Env = o.env(in)
	// A cap that can hang is not a cap. The capture points Stdout/Stderr at
	// io.MultiWriter values, so os/exec makes its own pipes and cmd.Run waits for
	// every WRITER of them to close — and CommandContext kills the direct child
	// only. A runaway session is exactly the case with a bash-tool grandchild (a
	// build, a test run) or an MCP server holding the inherited fd, which survives
	// the SIGKILL and keeps the pipe open: without a delay, Invoke blocks past its
	// own deadline in the one scenario the cap exists for. WaitDelay force-closes
	// the pipes and lets Wait return.
	//
	// It does NOT kill the orphan itself — that needs a process group, which is
	// platform-specific and is filed rather than smuggled in here. Set only with
	// a cap in play, so an ordinary session's I/O is never cut short by it.
	if sctx != ctx {
		cmd.WaitDelay = 5 * time.Second
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
	} else if runErr != nil && !timedOut {
		return res, runErr // couldn't launch opencode at all
	} else if runErr != nil {
		// Our own kill: the child died on the cancel, so its error is the kill's
		// and not a verdict. The marker above is the verdict.
		res.ExitCode = -1
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
	prompt := in.Prompt
	if in.Probe {
		// The cheapest possible request: a trivial prompt, tools all denied by the
		// read-only permission policy (see env) — just enough to see whether the
		// provider still answers or is budget-exhausted.
		prompt = "."
	}
	args := []string{"run", "--format", "json"}
	if m := in.ModelArg(); m != "" {
		args = append(args, "--model", m)
	}
	return append(args, "--", prompt)
}

// opencodeMCPResourcePattern is the pattern shape opencode's MCP resource tools
// ask with under the `read` permission: "mcp:<server>:<uri>" for a read and
// "mcp:<server>:*" for a listing. opencode's matcher (util/wildcard.ts) lets `*`
// span "/" and ":", so one pattern covers every server and every URI — including
// the URL-shaped resource URIs the clankerbar plane serves its protocol at.
const opencodeMCPResourcePattern = "mcp:*"

// opencodePermission is the fail-closed OPENCODE_PERMISSION policy: a path-scoped
// PermissionConfig, not a flat tool map. The run's workdir subtree is the working
// set — `read`/`edit` are allowed there and `external_directory` (opencode's gate
// for any path-taking tool whose target is outside the project directory) allows
// the same subtree, so the session's own worktrees, which live under the workdir
// and are separate git roots, work normally. Everything else fails closed via the
// `*` catch-all, and the network/exfil tools (webfetch/websearch) are denied in
// both shapes. A read-only run (probe, or the connectivity smoke) additionally
// denies edits and shell — zero writes, just enough to reach the clankerbar MCP.
//
// `bash` stays TOOL-LEVEL: its permission patterns match parsed COMMANDS ("git
// status --porcelain"), not paths, so a path rule can never express it. Commands
// whose file arguments fall outside the project boundary are gated separately via
// external_directory, which IS scoped above — that is the carve-out that fixes
// the old heuristic denials ("cp is denied but sed -i works").
//
// Five opencode quirks the emitted shape is fitted to (verified against opencode
// 1.18.16 source, and the MCP-resource one re-verified against the shipped 1.18.18
// bundle: permission/index.ts, session/tools.ts, tool/read.ts, tool/edit.ts,
// tool/external-directory.ts):
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
//   - read/edit asks carry the path RELATIVE TO THE GIT WORKTREE
//     (path.relative(worktree, file)); a session whose cwd is not inside a git
//     repo — the multi-repo-parent case, workdir=~/dev — gets worktree "/", so
//     its patterns are the absolute path minus the leading slash
//     ("Users/jason/dev/..."). The read/edit patterns are therefore emitted in
//     that same root-relative form. external_directory patterns are absolute
//     globs (path.join(dir, "*")), so that rule uses the absolute form.
func opencodePermission(readOnly bool, workdir string) string {
	rootRel, abs := opencodeWorkdirPatterns(workdir)
	// The exact (non-wildcard) workdir pattern, so reading the workdir root
	// itself — a directory listing of ~/dev, pattern "Users/jason/dev" — is
	// allowed along with everything under it.
	exact := strings.TrimSuffix(rootRel, "/**")
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
		"read":               map[string]string{rootRel: "allow", exact: "allow", opencodeMCPResourcePattern: "allow"},
		"edit":               map[string]string{rootRel: "allow", exact: "allow"},
		"external_directory": map[string]string{"*": "deny", abs: "allow"},
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
	if readOnly {
		perm["edit"] = "deny"
		perm["bash"] = "deny"
	} else {
		perm["bash"] = "allow"
	}
	// The catch-all: anything not named above — lsp/task/skill, reads or edits
	// outside the workdir — is denied rather than asked. Sorts first (see the
	// doc), so every specific rule wins the last-match-wins evaluation.
	perm["*"] = "deny"
	b, _ := json.Marshal(perm)
	return string(b)
}

// opencodeWorkdirPatterns derives the two pattern forms the policy needs from
// the run's workdir. rootRel is the pattern for read/edit asks — opencode's
// path.relative(worktree, file) form, i.e. the absolute path minus its leading
// separator (and drive letter on Windows) — so "Users/jason/dev/**" matches the
// patterns a session with worktree "/" asks for files under /Users/jason/dev.
// abs is the pattern for external_directory asks, which are absolute globs. An
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

func (opencode) env(in Invocation) []string {
	env := os.Environ()
	// Fail-closed permission policy. Set before in.Env so an explicit caller
	// OPENCODE_PERMISSION in in.Env still wins (exec takes the last of a dup key),
	// but the ambient environment never silently loosens an unattended run.
	env = append(env, "OPENCODE_PERMISSION="+opencodePermission(in.Probe, in.WorkDir))
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
	// A POINTER for the same reason Tokens is one: a step reporting cost 0 has
	// REPORTED, and the driver's zero-spend bound counts silence, not zeroes
	// (CLA-288).
	Cost *float64 `json:"cost"`

	// tool parts, which arrive on a "tool_use" event.
	Tool   string             `json:"tool"`
	CallID string             `json:"callID"`
	State  *opencodeToolState `json:"state"`
}

// opencodeToolState is a tool call's lifecycle slot on its part. Where claude
// splits a call across an `assistant` tool_use block and a later `user`
// tool_result block, opencode re-emits the same part as its status advances and
// carries the arguments and the result TOGETHER on the terminal one (recorded
// against opencode 1.18.16, CLA-365):
//
//	{"type":"tool_use","part":{"type":"tool","tool":"clankerbar_claim_task",
//	  "callID":"call_00_9Mb...","state":{"status":"completed",
//	    "input":{"taskId":"e1d01dae-..."},
//	    "output":"{\"task\":{\"id\":\"e1d01dae-...\",\"ref\":\"CLA-371\"},\"run\":{...}}"}}}
//
// A refusal takes the other terminal status and puts the plane's error where the
// output would have been. There is no `is_error` flag to read — the STATUS is the
// flag, which is why a status this adapter does not recognise must never be
// treated as success:
//
//	{"state":{"status":"error","input":{...},
//	  "error":"{\"error\":{\"code\":\"not_ready\",\"message\":\"...\"}}"}}
//
// `output` is a STRING, not an object: an MCP tool's result arrives flattened to
// its text content, which is the shape noteClaimed already parses.
//
// No spill envelope has been seen here. The `claim_task` result in the recording
// was ~21 KB and arrived INLINE (the committed fixture is ~3 KB, because most of
// that was a decisions block redacted out of it), where Claude Code substitutes a pointer
// to a file above its own threshold and broke the phase seam doing it (CLA-330,
// docs/large-tool-results.md). noteClaimed's rehydration still runs on this path,
// so an opencode that grows the same behaviour with the same envelope is handled;
// one with a DIFFERENT envelope would be a new bug, and the diagnostic noteClaimed
// prints on an unparseable result is what would surface it.
type opencodeToolState struct {
	Status string          `json:"status"`
	Input  json.RawMessage `json:"input"`
	Output string          `json:"output"`
	Error  string          `json:"error"`
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
	lastStepZeroUsage                     bool
	total, in, out, reason, cWrite, cRead int
	cost                                  float64
	sawUsage                              bool

	// observed carries ONLY the claim/report observation state, which finish
	// copies onto the real Result. It cannot be the real Result: that one is built
	// by capture.result() after the process exits, long after the claim event went
	// past, so anything written into it during the stream would be discarded.
	observed Result
	console  io.Writer
	// settledCalls is the callIDs already acted on, so a re-delivered terminal
	// event cannot rebuild a claim that a later call has since settled.
	settledCalls map[string]bool
}

// newOpencodeParse builds the parser a session streams into. It exists so the
// CLAIM SEED is exercised by tests: a resumed phase never calls claim_task, so
// without newSessionResult here its Claim would be the zero value and
// Claim.Held() — the gate on the handback, the salvage and the delivery check —
// would be false for the phase that pushes the branch and opens the PR. Assigned
// inline in Invoke, deleting it would leave the whole suite green.
func newOpencodeParse(in Invocation, console io.Writer) opencodeParse {
	return opencodeParse{observed: newSessionResult(in), console: console}
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
	if ev.Part == nil {
		return
	}
	switch ev.Type {
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
// part only once it has settled (cli/cmd/run.ts gates the emit on status
// completed-or-error), which is why the recorded fixture carries exactly one
// event per callID. The check is here because the status is the ONLY error flag
// opencode gives — there is no is_error sibling — so a version that started
// forwarding in-flight parts would otherwise have us read a call that had not
// returned as a success. An unrecognised status is ignored for the same reason:
// the cost of being wrong that way is an unobserved claim (the driver hands the
// task back — one expiring lease), against a claim recorded for a call that
// failed, which is a task the driver believes it holds and does not.
//
// A callID is acted on ONCE. noteClaimed rebuilds Result.Claim wholesale, so a
// re-delivered terminal event for a claim already settled by a later update_task
// would resurrect it — and the driver would post `ready` over a task that is in
// review. Upstream transitions a part to terminal only from `running`
// (session/processor.ts), so this is insurance, not an observed bug.
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
	// callID and a state, so the fields below are the discriminator.
	if st == nil || part.CallID == "" {
		return
	}
	if st.Status != "completed" && st.Status != "error" {
		return
	}
	name := opencodeClankerbarTool(part.Tool)
	if name == "" {
		return
	}
	if p.settledCalls[part.CallID] {
		return
	}
	if p.settledCalls == nil {
		p.settledCalls = map[string]bool{}
	}
	p.settledCalls[part.CallID] = true

	noteToolUse(name, part.CallID, st.Input, &p.observed)
	// The result text is handed over as JSON so it reaches noteClaimed by the same
	// road claude's string-shaped tool_result content does. Marshalling a string
	// cannot fail (invalid UTF-8 is replaced, not rejected), and the error is
	// swallowed rather than returned on purpose: bailing here would strand the
	// delivery Report noteToolUse just armed, unresolved and silently dropped.
	content, _ := json.Marshal(st.Output)
	noteToolResult(&p.observed, part.CallID, st.Status == "error", content, p.diag())
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

var opencodeFatalRe = regexp.MustCompile(`(?i)"status(code)?": ?40[13]` +
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
var opencodeTransientRe = regexp.MustCompile(`(?i)"status(code)?": ?(408|429|5\d\d)` +
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

// No mid-stream token ceiling: opencode sums per-step usage but the adapter
// never kills the process, so no exit of its can be attributed to one.
func (opencode) TokenCeilingHit(Result) bool { return false }

// TracksClaims is true as of CLA-365: opencode's `--format json` stream carries
// each tool call's arguments AND its result on one terminal `tool_use` event, so
// the adapter reads the claim off the session the same way claude does and
// populates Result.Claim. That is what config.Validate gates `phases` on, so this
// flag is what lets a mixed-harness queue run implement on opencode.
//
// HonoursMaxTurns stays false — Invocation.MaxTurns still never reaches the CLI.
//
// ReportsCost is true: opencode's step_finish parts carry a `cost` sibling to the
// token counts, and opencodeParse sums it into Result.CostUSD, so
// `budget.max_cost_usd` is a live ceiling here (CLA-288).
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
