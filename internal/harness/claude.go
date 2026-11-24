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
	"strconv"
	"strings"
	"time"
	"unicode"
)

func init() { Register(claude{}) }

// claude drives Claude Code (`claude -p`). Notable, per the capability spike: - `.mcp.json` is NOT
// auto-discovered in -p mode; pass --mcp-config explicitly.
type claude struct{}

func (claude) Name() string { return "claude" }

// claude is the one harness for which `mcp_config_path` means what its name suggests: the file is
// passed as --mcp-config (with --strict-mcp-config) and read as Claude's own `.mcp.json`, so a
// present one really does hand the session its clankerbar tools.
func (claude) MCPConfigUse() MCPConfigUse { return MCPConfigUse{Schema: MCPConfigClaudeJSON} }

// newSessionResult is the Result a session starts from, before a byte of its stream is parsed. A
// resumed phase never calls claim_task — it was told to continue a run, not start one — so
// without this its claim would be the zero value, and Result.Claim.Held() is what gates the
// driver's handback, the CLA-314 salvage and the CLA-253 delivery check.
func newSessionResult(in Invocation) Result {
	res := Result{Claim: in.ResumeClaim, onClaim: in.OnClaim}
	if in.NewEventSink != nil {
		res.events = in.NewEventSink()
		// A resumed phase is seeded with the run it is continuing, so its events have somewhere to go
		// from the first call; a fresh phase learns the run id from noteClaimed when claim_task's result
		// arrives.
		if in.ResumeClaim.RunID != "" {
			res.events.SetRunID(in.ResumeClaim.RunID)
		}
	}
	// CLA-739: never on a probe. A probe is a one-word liveness check with its tools denied, so it has
	// no tool calls to repeat, and a probe killed for a "loop" would report a session-shaped verdict
	// for something no phase ran.
	if !in.Probe {
		res.toolLoop = newToolLoopGuard(in.MaxRepeatedToolFailures)
		res.maxIdle, res.maxToolCall = in.MaxSessionIdle, in.MaxToolCallDuration
	}
	return res
}

// claudeArgs builds the session's argv. Extracted from Invoke so it can be asserted directly: a
// flag that silently stops being emitted is invisible to every other test, and one of these is a
// dependency on an UNDOCUMENTED flag (see claudeMaxTurnsReason), where a version bump is exactly
// how it would go.
func claudeArgs(in Invocation) []string {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--permission-mode", "acceptEdits"}
	if m := in.ModelArg(); m != "" {
		args = append(args, "--model", m)
	}
	if in.MCPConfigPath != "" {
		args = append(args, "--mcp-config", in.MCPConfigPath, "--strict-mcp-config")
	}
	// The headless permission policy: with no human to prompt, an unattended run is gated entirely by
	// this file's allow/deny rules (deny wins over the config-dir's ambient allowlist).
	if in.SettingsPath != "" {
		args = append(args, "--settings", in.SettingsPath)
	}
	// The project's other declared repos: --add-dir is Claude's own mechanism for granting tool access
	// beyond the working directory, so a two-repo project's session can read and edit its sibling
	// whatever directory it starts in.
	if len(in.ExtraDirs) > 0 {
		args = append(args, "--add-dir")
		args = append(args, in.ExtraDirs...)
	}
	// The phase backstop. Claude ends the session at the cap; whatever the tree holds is then the
	// salvage's problem, which is exactly what it is for.
	if in.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(in.MaxTurns))
	}
	// Studio chat (studio-chat.md §3): one session per conversation, created under an id the CLI
	// chooses (`--session-id`) and continued on later turns (`--resume`).
	if ref := strings.TrimSpace(in.SessionRef); ref != "" {
		if in.ResumeSession {
			args = append(args, "--resume", ref)
		} else {
			args = append(args, "--session-id", ref)
		}
	}
	return args
}

func (c claude) Invoke(ctx context.Context, in Invocation) (Result, error) {
	if err := refuseTurns("claude", in); err != nil {
		return Result{}, err
	}
	if in.Probe {
		return c.probe(ctx, in)
	}

	// A derived context, not the caller's: the per-session token ceiling kills THIS process without
	// cancelling the driver.
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	deliveryCtx, stopDelivery := context.WithCancel(sctx)
	defer stopDelivery()

	// Console -> agent delivery. The watcher is wrapped, never replaced: the driver's own callback
	// still fires.
	var inboxRunIDs chan string
	if in.Inbox != nil {
		// A small buffer, not 1: a settle re-notifies the SAME run and a session may claim again
		// immediately, so the queue must hold the next run while the current poller finishes.
		inboxRunIDs = make(chan string, 8)
		if in.ResumeClaim.RunID != "" {
			inboxRunIDs <- in.ResumeClaim.RunID
		}
		lastRunID := in.ResumeClaim.RunID
		origOnClaim := in.OnClaim
		in.OnClaim = func(c Claim) {
			// A settle fires this callback with the run id unchanged; only a NEW run is a delivery target.
			if c.RunID != "" && c.RunID != lastRunID {
				lastRunID = c.RunID
				select {
				case inboxRunIDs <- c.RunID:
				default: // pathological churn; the newest claim will start the next poller
				}
			}
			if origOnClaim != nil {
				origOnClaim(c)
			}
		}
	}

	cmd, err := sessionCommand(sctx, in.Nice, "claude", claudeArgs(in)...)
	if err != nil {
		return Result{}, err
	}
	setupProcessGroup(cmd)
	// Kill the whole process group (descendants holding inherited pipes) rather than only the direct
	// child, on ANY cancellation of sctx - the token-ceiling kill from consume(), or the caller's own
	// cancellation (a Ctrl-C that stopped reaching the session subtree through the tty the moment
	// Setpgid moved it out of the foreground group).
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return cmd.Process.Kill()
	}
	if in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	cmd.Env = c.env(in)
	// A cap that can hang is not a cap: the same grandchild-holds-the-pipe scenario the opencode
	// adapter closes with WaitDelay (opencode.go:291-303).
	cmd.WaitDelay = 5 * time.Second

	console := in.Console
	if console == nil {
		console = os.Stderr
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, err
	}
	stderrTail := newTail()
	cmd.Stderr = io.MultiWriter(stderrTail, console) // surface errors live too

	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	input := newSessionInput(stdinPipe)
	defer input.close()
	// The prompt is the first user turn. A write failure here is reported, not fatal: the child may
	// have exited on a bad flag, and Wait's exit status is the better verdict.
	if err := input.writeUser(in.Prompt); err != nil {
		fmt.Fprintf(console, "!! could not write the prompt to claude's stream-json stdin: %v\n", err)
	}

	// Stream stdout line-by-line: parse each event as it arrives, render readable progress to the
	// console, and retain a bounded tail for the text scans.
	stdoutTail := newTail()
	res := newSessionResult(in)
	defer res.closeEvents()
	// A result retires one outstanding user turn; the LAST one closes stdin so the CLI exits.
	res.onTurnEnd = func() {
		if input.endTurn() {
			stopDelivery()
		}
	}
	deliveryDone := make(chan struct{})
	if inboxRunIDs != nil {
		go func() {
			defer close(deliveryDone)
			runDelivery(deliveryCtx, console, *in.Inbox, input, inboxRunIDs)
		}()
	} else {
		close(deliveryDone)
	}
	c.consume(stdoutPipe, console, stdoutTail, &res, in.MaxSessionTokens, cancel)
	// The stream is over for us (EOF, a kill, or an unreadable line): close stdin so a child still
	// waiting on it cannot hold Wait open past its own stream.
	input.close()
	stopDelivery()
	// Join delivery so the ACK of a message written just before the session ended reaches the plane
	// before we return.
	<-deliveryDone
	waitErr := cmd.Wait()

	res.Stdout = stdoutTail.String()
	res.Stderr = stderrTail.String()
	res.OutputDropped = stdoutTail.Dropped() + stderrTail.Dropped()
	// The memo is created only now that the text it memoizes is final: keyed on scope alone, a scan
	// taken any earlier would freeze an answer built from an empty Stdout for the whole session.
	res.scans = newScanCache()
	// Unlike opencode, consume stopped reading when the guard tripped, so a child that then exited 0
	// left events unparsed: keep the cause and say so.
	if waitErr == nil && res.IdleKilled != "" {
		res.markUntrusted("the idle watchdog fired (" + res.IdleKilled + ") and claude's stream was not read " +
			"to the end, so this session's spend, final message and claim settlement may be incomplete")
	}
	if err := c.classifyWait(waitErr, &res); err != nil {
		return res, err
	}
	// Studio chat (studio-chat.md §3): the handle this session ran under, and the one failure the
	// planner must recover from in-turn.
	res.SessionRef = strings.TrimSpace(in.SessionRef)
	if in.ResumeSession && res.SessionRef != "" && res.ExitCode != 0 &&
		strings.Contains(claudeText(res, claudeDiagnostic), claudeSessionMissingText) {
		res.SessionMissing = true
	}
	return res, nil
}

// claudeSessionMissingText is claude's own refusal to resume a session its local store no longer
// holds (verified against claude 2.1.322: exit 1, stderr "No conversation found with session ID:
// <id>").
const claudeSessionMissingText = "No conversation found with session ID"

// noteToolResult applies one clankerbar call's RESULT to res: it resolves the delivery claim
// noteToolUse armed on the request, and then whatever else that request was waiting to learn. A
// refused call changed nothing on the plane, so it must change nothing here either — which is the
// whole reason these are judged on the result rather than on the request.
func noteToolResult(res *Result, toolUseID string, isError bool, content json.RawMessage, console io.Writer) {
	if toolUseID == "" {
		return
	}
	// CLA-703: a clankerbar call's SUCCESS is the one fact that separates "the MCP server was not
	// there" from "the session reached the backlog and still chose not to review".
	if res.clankerbarCalls[toolUseID] {
		delete(res.clankerbarCalls, toolUseID)
		if !isError {
			res.ClankerbarCalls++
		}
	}
	// A delivery claim is kept only if the plane ACCEPTED the call that made it — same rule, same
	// reason: a refused progress or terminal call recorded no branch and declared no delivery, so
	// there is nothing to check and nothing to complain about.
	res.settleReport(toolUseID, !isError)

	kind, waiting := res.pending[toolUseID]
	if !waiting {
		return
	}
	delete(res.pending, toolUseID)
	if isError {
		return
	}
	switch kind {
	case pendingClaim:
		noteClaimed(content, toolUseID, res, console)
	case pendingSettle:
		res.Claim.Settled = true
		res.notifyClaim()
	}
}

// The clankerbar MCP tools the driver watches for.
const (
	claimTaskTool        = "mcp__clankerbar__claim_task"
	recordProgressTool   = "mcp__clankerbar__record_progress"
	submitForReviewTool  = "mcp__clankerbar__submit_for_review"
	completeTaskTool     = "mcp__clankerbar__complete_task"
	settleTaskTool       = "mcp__clankerbar__settle_task"
	setLaneTool          = "mcp__clankerbar__set_lane"
	askQuestionTool      = "mcp__clankerbar__ask_question"
	escalateQuestionTool = "mcp__clankerbar__escalate_question"

	// clankerbarToolPrefix is the namespace every clankerbar MCP tool carries, whichever adapter
	// reported it. CLA-703 asks "did this session reach the backlog at all?", which is true of any
	// successful call — get_task and heartbeat included — so the counter keys on the namespace,
	// not on the handful of calls that carry claim state.
	clankerbarToolPrefix = "mcp__clankerbar__"
)

// isClankerbarTool reports whether name is one of the clankerbar MCP tools.
func isClankerbarTool(name string) bool { return strings.HasPrefix(name, clankerbarToolPrefix) }

// noteToolUse records what a clankerbar call will mean once its result lands. A terminal tool is
// not the only way a session lets go.
func noteToolUse(name, toolUseID string, input json.RawMessage, res *Result) {
	// Remember the request so its RESULT can say whether the backlog was reachable.
	if toolUseID != "" && isClankerbarTool(name) {
		if res.clankerbarCalls == nil {
			res.clankerbarCalls = map[string]bool{}
		}
		res.clankerbarCalls[toolUseID] = true
	}
	switch name {
	case claimTaskTool:
		// The ids are in the RESULT, not the arguments. A claim that loses the race carries none and so
		// leaves the tracked claim untouched.
		res.expect(toolUseID, pendingClaim)
		// The REQUESTED task, kept only so a failure can name what it was about. Deliberately never
		// promoted into a Claim: the run id exists only in the result, and a half-claim is the one thing
		// the driver must not be handed.
		var args struct {
			TaskID string `json:"taskId"`
		}
		if json.Unmarshal(input, &args) == nil && args.TaskID != "" {
			res.noteClaimRequest(toolUseID, args.TaskID)
		}

	case recordProgressTool, submitForReviewTool, completeTaskTool, settleTaskTool, setLaneTool:
		var args struct {
			TaskID      string `json:"taskId"`
			Branch      string `json:"branch"`
			Disposition string `json:"disposition"`
			Lane        string `json:"lane"`
			Delivery    struct {
				Commit            string `json:"commit"`
				IntegrationBranch string `json:"integrationBranch"`
				PR                string `json:"pr"`
			} `json:"delivery"`
		}
		if json.Unmarshal(input, &args) != nil || !res.Claim.Names(args.TaskID) {
			return
		}
		// The tool set is one job per call, so what a call means is read off WHICH tool it is rather than
		// off a status field.
		status := ""
		switch name {
		case submitForReviewTool:
			status = "in_review"
			res.expect(toolUseID, pendingSettle)
		case completeTaskTool:
			status = "done"
			res.expect(toolUseID, pendingSettle)
		case settleTaskTool:
			// parked/archived: both clear the holder plane-side.
			status = args.Disposition
			res.expect(toolUseID, pendingSettle)
		case setLaneTool:
			// A lane move (ready/backlog) is a status write too: the plane clears the holder for every
			// status but in_progress.
			status = args.Lane
			res.expect(toolUseID, pendingSettle)
		}
		// The two claims the plane cannot check for itself. Armed here, kept only if the call is
		// accepted, and verified against local git once the session ends.
		res.expectReport(toolUseID, Report{
			TaskID:            res.Claim.TaskID,
			Ref:               res.Claim.Ref,
			RunID:             res.Claim.RunID,
			Status:            status,
			Branch:            args.Branch,
			Commit:            args.Delivery.Commit,
			IntegrationBranch: args.Delivery.IntegrationBranch,
			PR:                args.Delivery.PR,
		})
		// Recording a branch declares pushed work worth handing over, which is exactly what makes the
		// task unsafe to release.
		if args.Branch != "" {
			res.Claim.HasWIP = true
			// Carry the NAME too, not just the fact: CURRENT_CLAIM's branch field is what tells a supervisor
			// to expireLease rather than release — release would post `ready` over the branch.
			res.Claim.Branch = args.Branch
			res.notifyClaim()
		}

	case askQuestionTool:
		var args struct {
			TaskID   string `json:"taskId"`
			Blocking bool   `json:"blocking"`
		}
		if json.Unmarshal(input, &args) != nil {
			return
		}
		if args.Blocking && res.Claim.Names(args.TaskID) {
			res.expect(toolUseID, pendingSettle)
		}

	case escalateQuestionTool:
		// This one takes a questionId, so the held task cannot be confirmed from the arguments.
		res.expect(toolUseID, pendingSettle)
	}
}

// noteClaimed records the task/run a successful claim_task returned. A payload missing either id (a
// lost race, a refusal, a shape we do not recognise) leaves the tracked claim alone — the driver
// must never be handed a half-claim it would then try to release.
func noteClaimed(content json.RawMessage, toolUseID string, res *Result, console io.Writer) {
	// Which task the session ASKED for, read off the tool_use arguments.
	wanted := res.claimRequests[toolUseID]
	delete(res.claimRequests, toolUseID)
	about := ""
	if wanted != "" {
		about = fmt.Sprintf(" for %s", wanted)
	}

	text := toolResultText(content)
	if path, ok := persistedOutputPath(text, toolUseID); ok {
		b, err := readSpilled(path)
		if err != nil {
			fmt.Fprintf(console, "  · claim NOT tracked%s: the harness spilled this result to %s, which could not be read (%v)\n", about, path, err)
			return
		}
		text = toolResultText(json.RawMessage(b))
	}
	var payload struct {
		Task struct {
			ID       string `json:"id"`
			Ref      string `json:"ref"`
			Branch   string `json:"branch"`
			Category string `json:"category"`
		} `json:"task"`
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
		HasWip bool   `json:"hasWip"`
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		fmt.Fprintf(console, "  · claim NOT tracked%s: its result is not JSON (%v) - %s\n", about, err, snippet(text))
		return
	}
	if payload.Task.ID == "" || payload.Run.ID == "" {
		// Benign when the claim simply lost the race or was refused, which is why this is a note and not
		// a warning.
		fmt.Fprintf(console, "  · claim NOT tracked%s: its result carried task id %q and run id %q, and both are needed - %s\n",
			about, payload.Task.ID, payload.Run.ID, snippet(text))
		return
	}
	// A predecessor's pushed work arrives with the claim. Carry it: the task is no safer to release
	// just because THIS session has not written anything yet.
	claimBranch := payload.Branch
	if payload.Task.Branch != "" && claimBranch == "" {
		claimBranch = payload.Task.Branch
	}
	res.Claim = Claim{
		TaskID:   payload.Task.ID,
		Ref:      payload.Task.Ref,
		RunID:    payload.Run.ID,
		HasWIP:   payload.HasWip || payload.Task.Branch != "",
		Category: payload.Task.Category,
		Branch:   claimBranch,
	}
	// The agent event channel addresses the plane's per-run ingest endpoint with this id, so the run
	// becomes known exactly here: the claim's request and result were already buffered (the claim IS a
	// clankerbar_mcp_call event) and now get a destination.
	if res.events != nil {
		res.events.SetRunID(payload.Run.ID)
	}
	// Any OnClaim watcher - the driver's lease renewer - learns the claim the moment the stream
	// carries it, not when the process exits.
	res.notifyClaim()
	// Say it out loud. Everything downstream is silent by design — a claim that is never observed
	// produces no handback and no complaint, so without this line the feature could quietly stop
	// working (a stream-shape change under a harness upgrade) and look exactly like a run that never
	// claimed anything.
	fmt.Fprintf(console, "  · holding %s (release on exit if nothing is pushed)\n", claimLabel(res.Claim))
}

func claimLabel(c Claim) string {
	if c.Ref != "" {
		return c.Ref
	}
	return c.TaskID
}

// The envelope Claude Code substitutes for a tool_result too large to inline: a pointer to the file
// holding the real thing, plus a truncated preview of it. The driver then read `Claim.Held()` as
// false and ended the drain - "the implement phase ended without holding the task" - while the
// plane had that task claimed the whole time, and phase 2 never ran.
const persistedOutputMarker = "<persisted-output>"

// Deliberately NOT anchored to the start of a line: the pointer shares its line with the size that
// caused the spill ("Output too large (66.4KB).
var persistedOutputRe = regexp.MustCompile(`Full output saved to:[ \t]*(\S[^\n]*?)[ \t]*(?:\n|$)`)

// persistedOutputPath returns the file a spilled tool_result was written to, if this result is one.
// The path is honoured only when its base name is THIS tool call's id, and that guard is the point
// rather than a nicety.
func persistedOutputPath(text, toolUseID string) (string, bool) {
	if toolUseID == "" || !strings.HasPrefix(strings.TrimSpace(text), persistedOutputMarker) {
		return "", false
	}
	m := persistedOutputRe.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	path := m[1]
	if !filepath.IsAbs(path) || filepath.Base(path) != toolUseID+".json" {
		return "", false
	}
	return path, true
}

// readSpilled reads a spilled tool_result under the same bound every other read in this package
// obeys. output.go states the invariant plainly - "a stream of any size costs a fixed amount of
// memory", and no dial, because a dial is a way to reintroduce the OOM.
func readSpilled(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxStreamLine+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxStreamLine {
		return nil, fmt.Errorf("spilled result exceeds %d bytes", maxStreamLine)
	}
	return b, nil
}

// snippetMax is how much of an unreadable payload a diagnostic quotes. Enough to recognise the
// shape - an envelope, an MCP error, a lost race - and no more: the thing being quoted is routinely
// the reason the payload was unreadable in the first place, and a driver log nobody can scroll is
// its own kind of silent.
const snippetMax = 160

// snippet bounds and sanitises a diagnostic that quotes a tool_result. Both of those follow
// failureDetail in the loop package, for the same two reasons, because this is the same kind of
// string: text from outside rendered to a terminal an operator then copies out of.
func snippet(s string) string {
	printable := strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			return r
		}
		return -1
	}, s)
	// Cut first, THEN collapse whitespace: strings.Fields over a 66KB payload allocates a slice header
	// per token to keep 160 bytes of it.
	if r := []rune(printable); len(r) > snippetMax*4 {
		printable = string(r[:snippetMax*4])
	}
	out := strings.Join(strings.Fields(printable), " ")
	if out == "" {
		return "(empty)"
	}
	if r := []rune(out); len(r) > snippetMax {
		return string(r[:snippetMax]) + "..."
	}
	return out
}

// toolResultText flattens a tool_result's `content`, which the stream renders either as a bare
// string or as an array of typed blocks.
func toolResultText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// usage is claude's reported token accounting for a session. `input_tokens` is UNCACHED input only,
// so summing it with output misses the cache reads and writes that dominate a long agentic session
// — one real run reported 140,387 tokens against $147.98 of actual spend, about $1.05 per
// thousand, which is no model's price.
type usage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
}

func (u usage) total() int {
	return u.InputTokens + u.OutputTokens + u.CacheCreationTokens + u.CacheReadTokens
}

// normalised maps claude's report onto the CLI's own usage vocabulary. The mapping is the identity
// on three fields because claude's `input_tokens` is UNCACHED input only — the cache fields sit
// beside it, not inside it — and zero on Reasoning because Anthropic folds thinking tokens into
// `output_tokens` and reports no separate figure.
func (u usage) normalised() TokenUsage {
	return TokenUsage{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadTokens,
		CacheWrite: u.CacheCreationTokens,
	}
}

// probe runs the cheapest possible request (tiny prompt, no tools, plain json) to answer "am I
// still limited?" — no streaming, no console.
func (c claude) probe(ctx context.Context, in Invocation) (Result, error) {
	args := []string{"-p", ".", "--output-format", "json", "--permission-mode", "dontAsk", "--allowedTools", ""}
	cmd, err := sessionCommand(ctx, in.Nice, "claude", args...)
	if err != nil {
		return Result{}, err
	}
	if in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	cmd.Env = c.env(in)

	// Bounded like the drain path, though a probe's answer is a few hundred bytes: the cap is what
	// makes "how much can one session hold" answerable without reasoning about which path produced it.
	stdout, stderr := newTail(), newTail()
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	return c.probeResult(stdout, stderr, runErr)
}

// probeResult turns a finished probe run into its Result: parse whatever the CLI managed to emit,
// THEN classify how the run ended. Run can fail with something other than an exit status after the
// process has already printed its answer — the JSON object is then sitting in stdout, and only
// the wait fell over.
func (c claude) probeResult(stdout, stderr *tail, runErr error) (Result, error) {
	dropped := stdout.Dropped() + stderr.Dropped()
	res := Result{
		Stdout:        stdout.String(),
		Stderr:        stderr.String(),
		OutputDropped: dropped,
		scans:         newScanCache(),
	}
	// `--output-format json` is ONE object, so a trimmed tail does not merely lose context here — it
	// loses the answer, and parse would fail into a Result that reads exactly like "no limit found".
	if dropped > 0 {
		res.markUntrusted(fmt.Sprintf("the probe's output overran the retained window (%d bytes dropped), so its verdict cannot be read", dropped))
	}
	c.parse(&res)
	if ee, ok := runErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if runErr != nil {
		return res, runErr
	}
	return res, nil
}

func (claude) env(in Invocation) []string {
	// Pin the child's PWD to the directory Invoke sets as cmd.Dir, dropping the daemon's inherited
	// value - see pinPWD.
	env := append(pinPWD(inheritEnv(os.Environ(), in.EnvAllowlist), in.WorkDir), in.Env...)
	// Never truncate the session while subagents/background work run.
	env = append(env, "CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0")
	if in.ConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+in.ConfigDir)
	}
	return env
}

// parse reads `claude --output-format json` — a single JSON object (probe path).
func (claude) parse(res *Result) {
	// The accounting members are POINTERS so their absence is distinguishable from a zero: this
	// envelope is also how an error is reported, and one carrying no usage at all must not claim a
	// report of zero.
	var p struct {
		IsError        bool     `json:"is_error"`
		Result         string   `json:"result"`
		TerminalReason string   `json:"terminal_reason"`
		TotalCostUSD   *float64 `json:"total_cost_usd"`
		Usage          *usage   `json:"usage"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &p); err != nil {
		return
	}
	res.FinalMessage = p.Result
	if p.TotalCostUSD != nil {
		res.CostUSD = *p.TotalCostUSD
		res.CostReported = true
	}
	if p.Usage != nil {
		res.Tokens = p.Usage.total()
		res.Usage = p.Usage.normalised()
	}
	res.UsageReported = p.TotalCostUSD != nil || p.Usage != nil
	res.Raw = map[string]any{"terminal_reason": p.TerminalReason, "is_error": p.IsError}
}

// claudeScope is how much of claude's output a scan is allowed to read. Both scans used to run over
// the whole of Stdout+Stderr, and Stdout is the entire raw NDJSON stream: every assistant message,
// and every `tool_result` block, which carries the verbatim MCP response to `claim_task`.
type claudeScope int

const (
	// claudeTyped is the narrow width: only what the CLI itself authored outside the event stream, or
	// set in a machine-written field — stderr, stdout lines that are not events at all, and
	// `terminal_reason`.
	claudeTyped claudeScope = iota
	// claudeDiagnostic adds the free text of a session the CLI says ended BADLY: the `result` field of
	// a failed result event, and typed error events whole.
	claudeDiagnostic
)

// claudeText collects claude's own output at the given width, so a scan never reads the agent's
// narration. Non-event stdout lines are kept deliberately: under `--output-format stream-json`
// every word the agent says arrives inside a JSON event, so a bare line is the CLI talking —
// which is how a usage-limit notice arrives when the stream never starts.
func claudeText(res Result, scope claudeScope) string {
	return res.scan(int(scope), func() string { return buildClaudeText(res, scope) })
}

func buildClaudeText(res Result, scope claudeScope) string {
	var b strings.Builder
	b.WriteString(res.Stderr)

	// The probe path uses `--output-format json` — ONE object, with no one-event-per-line contract
	// to lean on.
	if whole := strings.TrimSpace(res.Stdout); strings.HasPrefix(whole, "{") {
		var ev claudeEvent
		if json.Unmarshal([]byte(whole), &ev) == nil {
			writeClaudeEvent(&b, ev, whole, scope)
			return b.String()
		}
	}

	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			b.WriteByte('\n')
			b.WriteString(line)
			continue
		}
		var ev claudeEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		writeClaudeEvent(&b, ev, line, scope)
	}
	return b.String()
}

// claudeEvent is the sliver of a stream event that says whether the CLI is reporting a failure, and
// what it called it.
type claudeEvent struct {
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	IsError        bool   `json:"is_error"`
	Result         string `json:"result"`
	TerminalReason string `json:"terminal_reason"`
}

// failed reports whether the CLI itself said this session ended badly — the condition under which
// its `result` text is the CLI's own words rather than the agent's closing summary.
func (ev claudeEvent) failed() bool {
	return ev.IsError || ev.TerminalReason != "" || (ev.Subtype != "" && ev.Subtype != "success")
}

func writeClaudeEvent(b *strings.Builder, ev claudeEvent, line string, scope claudeScope) {
	switch {
	case ev.Type == "result":
		// terminal_reason is a typed field the CLI sets; never the agent's words, so it is read at both
		// widths.
		if ev.TerminalReason != "" {
			b.WriteByte('\n')
			b.WriteString(ev.TerminalReason)
		}
		// `result` is free text. On a clean finish it is the agent's own closing summary — narration by
		// another name.
		if scope == claudeDiagnostic && ev.failed() {
			b.WriteByte('\n')
			b.WriteString(ev.Result)
		}
	case scope == claudeDiagnostic && strings.Contains(ev.Type, "error"):
		// A typed error event, read whole — the parity codex and opencode already have.
		b.WriteByte('\n')
		b.WriteString(line)
	}
}

func (claude) DetectLimit(res Result) Limit {
	reason, _ := res.Raw["terminal_reason"].(string)
	if reason != "usage_limit" && !strings.Contains(claudeText(res, claudeDiagnostic), "hit your") {
		return Limit{}
	}
	return Limit{
		Limited: true,
		// The reset comes from the NARROW width, unlike the detection above.
		ResetAt: parseClaudeReset(claudeText(res, claudeTyped)),
		Reason:  "usage_limit",
	}
}

// claudeTransientRe anchors on the "API Error:" prefix (and bare connection errors) so a task log
// that legitimately mentions an HTTP 500 can't be mistaken for a dead session. Claude Code's error
// reference documents exactly three "the response above may be incomplete" variants, emitted when a
// stream fails after Claude has completed a block of text or a tool call: API Error: Server error
// mid-response.
var claudeTransientRe = regexp.MustCompile(`(?i)api error: (408|429|5\d\d)` +
	`|api error:.*(overloaded|internal server|bad gateway|service unavailable|gateway time|too many requests)` +
	`|api error:.*(mid-response|mid-stream)` +
	`|connection error|fetch failed|econnreset|econnrefused|etimedout|eai_again|socket hang up|network (error|timeout)`)

func (claude) IsTransient(res Result) bool {
	return claudeTransientRe.MatchString(claudeText(res, claudeDiagnostic))
}

// Never a guess: every retry this adapter asks for is a pattern match anchored on the CLI's own
// "API Error:" prefix, so an unrecognised exit is a stop and the operator's own dials are the whole
// bound.
func (claude) IsUnclassifiedTransient(Result) bool { return false }

// claudeMaxTurnsReason is what `claude -p --max-turns N` reports when the cap fires, verified
// against claude 2.1.226: exit 1, {"type":"result","subtype":"error_max_turns","is_error":true,
// "result":null,"terminal_reason":"max_turns", ...} `result` is null and the text matches neither
// the usage-limit scan nor the transient one, so WITHOUT this classification the loop reaches its
// non-retryable branch and ends the whole run. --max-turns is not in `claude --help` for that
// version; it is accepted (an unknown flag exits 1 with "error: unknown option"), so this is a
// dependency on an undocumented flag, pinned here against a named version deliberately.
const claudeMaxTurnsReason = "max_turns"

func (claude) TurnCapped(res Result) bool {
	if res.Raw == nil {
		return false
	}
	r, _ := res.Raw["terminal_reason"].(string)
	return r == claudeMaxTurnsReason
}

// tokenCeilingReason is the terminal_reason the ADAPTER writes into a Result when it killed the
// session for crossing Invocation.MaxSessionTokens.
const tokenCeilingReason = "token_ceiling_hit"

func (claude) TokenCeilingHit(res Result) bool {
	if res.Raw == nil {
		return false
	}
	r, _ := res.Raw["terminal_reason"].(string)
	return r == tokenCeilingReason
}

// No wall-clock cap: claude's backstop is --max-turns, which reaches this CLI, so a time cap here
// would be a second answer to a question already answered.
func (claude) WallClockCapped(Result) bool { return false }

// No zero-usage-unknown marker: claude's terminal_reason is a typed CLI field, and the quiet-death
// signature is opencode's step_finish shape.
func (claude) ZeroUsageUnknown(Result) bool { return false }

// markCeilingHit writes the adapter's own kill marker onto a Result.
func (r *Result) markCeilingHit() {
	r.Raw = map[string]any{"terminal_reason": tokenCeilingReason}
}

// Claude watches the session's clankerbar tool calls, so phases can run on it. opencode does too as
// of CLA-365; codex still does not.
func (claude) Capabilities() Capabilities {
	return Capabilities{TracksClaims: true, HonoursMaxTurns: true, ReportsCost: true, HasSessionTokenCeiling: true, EmitsEvents: true, DeliversMessages: true, ResumesSessions: true}
}

// Diagnostic returns the same CLI-authored text IsTransient judged, so a caller that stops on a
// non-retryable exit can say WHICH message it stopped on.
func (claude) Diagnostic(res Result) string {
	return claudeText(res, claudeDiagnostic)
}

func (c claude) Probe(ctx context.Context, in Invocation) (ProbeResult, error) {
	in.Probe = true
	res, err := c.Invoke(ctx, in)
	// Spend first, and off the Result on every path: a probe that exited non-zero still cost what it
	// cost, and under-counting is the one direction a budget breaker must not err in.
	out := ProbeResult{Tokens: res.Tokens, CostUSD: res.CostUSD}
	if err != nil {
		return out, err
	}
	return probeVerdict(out, res, c.DetectLimit)
}

func (claude) ReadUsage(context.Context, Invocation) (Usage, error) {
	// /usage is TTY-only; --output-format json carries no remaining quota; no local file persists it
	// (see the memo, and anthropics/claude-code#32796).
	return Usage{}, ErrUsageUnsupported
}

// parseClaudeReset extracts the reset time from a message like You've hit your session limit ·
// resets 9:40pm (Europe/Madrid) You've hit your weekly limit · resets Sunday 12:00am The reset is
// only an upper bound for the supervised wait (the loop still polls for an early reset), so this is
// deliberately best-effort: on any doubt it returns the zero time and the loop falls back to
// interval polling.
func parseClaudeReset(s string) time.Time { return parseClaudeResetAt(s, time.Now()) }

// resetRe captures: [weekday] hour [:minute] [am/pm] [(timezone)].
var resetRe = regexp.MustCompile(`(?i)resets\s+(?:(mon|tue|wed|thu|fri|sat|sun)[a-z]*\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?(?:\s*\(([^)]+)\))?`)

// parseClaudeResetAt is parseClaudeReset with an injectable "now" for testing.
func parseClaudeResetAt(s string, now time.Time) time.Time {
	m := resetRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}
	}
	hour, err := strconv.Atoi(m[2])
	if err != nil {
		return time.Time{}
	}
	minute := 0
	if m[3] != "" {
		minute, _ = strconv.Atoi(m[3])
	}
	switch strings.ToLower(m[4]) {
	case "pm":
		if hour != 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	}
	if hour > 23 || minute > 59 {
		return time.Time{}
	}

	loc := time.Local
	if tz := strings.TrimSpace(m[5]); tz != "" {
		if l, lerr := time.LoadLocation(tz); lerr == nil {
			loc = l
		}
	}
	now = now.In(loc)
	target := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, loc)

	if wd := strings.ToLower(m[1]); wd != "" {
		want, ok := weekdayNum[wd]
		if !ok {
			return time.Time{}
		}
		for i := 0; i < 8; i++ {
			if target.Weekday() == want && target.After(now) {
				return target
			}
			target = target.AddDate(0, 0, 1)
		}
		return time.Time{}
	}
	if !target.After(now) {
		target = target.AddDate(0, 0, 1)
	}
	return target
}

var weekdayNum = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}
