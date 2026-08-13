package harness

import (
	"bufio"
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

// claude drives Claude Code (`claude -p`). Notable, per the capability spike:
//   - `.mcp.json` is NOT auto-discovered in -p mode; pass --mcp-config explicitly.
//   - --output-format stream-json (+ --verbose) streams events live, so we can
//     render progress to the console AND parse the final result/usage/limit.
//   - CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0 keeps the session alive while
//     delegated subagents are still running (the drain dispatches to subagents).
//   - CLAUDE_CONFIG_DIR pins the config dir so a headless run loads the same
//     skills, plugins, and auth as the interactive one.
type claude struct{}

func (claude) Name() string { return "claude" }

// claude is the one harness for which `mcp_config_path` means what its name
// suggests: the file is passed as --mcp-config (with --strict-mcp-config) and
// read as Claude's own `.mcp.json`, so a present one really does hand the
// session its clankerbar tools. No note - there is nothing surprising to say.
func (claude) MCPConfigUse() MCPConfigUse { return MCPConfigUse{Schema: MCPConfigClaudeJSON} }

// newSessionResult is the Result a session starts from, before a byte of its
// stream is parsed.
//
// It exists so the SEED is testable. A resumed phase never calls claim_task — it
// was told to continue a run, not start one — so without this its claim would be
// the zero value, and Result.Claim.Held() is what gates the driver's handback,
// the CLA-314 salvage and the CLA-253 delivery check. Seeding leaves the stream
// to supply Settled/HasWIP on top: noteToolUse matches `update_task` against this
// TaskID, which is precisely the arm that would otherwise never fire for the
// phase that pushes the branch and opens the PR.
//
// Extracted rather than assigned inline because a test that builds its own
// Result does not exercise an assignment inside Invoke — which is exactly how the
// first cut of this shipped with the seed untested and a mutation of it surviving
// the whole suite.
func newSessionResult(in Invocation) Result {
	return Result{Claim: in.ResumeClaim}
}

// claudeArgs builds the session's argv. Extracted from Invoke so it can be
// asserted directly: a flag that silently stops being emitted is invisible to
// every other test, and one of these is a dependency on an UNDOCUMENTED flag
// (see claudeMaxTurnsReason), where a version bump is exactly how it would go.
func claudeArgs(in Invocation) []string {
	args := []string{"-p", in.Prompt, "--output-format", "stream-json", "--verbose", "--permission-mode", "acceptEdits"}
	if m := in.ModelArg(); m != "" {
		args = append(args, "--model", m)
	}
	if in.MCPConfigPath != "" {
		args = append(args, "--mcp-config", in.MCPConfigPath, "--strict-mcp-config")
	}
	// The headless permission policy: with no human to prompt, an unattended run
	// is gated entirely by this file's allow/deny rules (deny wins over the
	// config-dir's ambient allowlist).
	if in.SettingsPath != "" {
		args = append(args, "--settings", in.SettingsPath)
	}
	// The phase backstop. Claude ends the session at the cap; whatever the tree
	// holds is then the salvage's problem, which is exactly what it is for.
	if in.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(in.MaxTurns))
	}
	return args
}

func (c claude) Invoke(ctx context.Context, in Invocation) (Result, error) {
	if in.Probe {
		return c.probe(ctx, in)
	}

	// A derived context, not the caller's: the per-session token ceiling kills
	// THIS process without cancelling the driver (CLA-343). exec.CommandContext
	// kills the child when the context is cancelled, which is exactly the kill
	// switch the ceiling needs — see consume.
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(sctx, "claude", claudeArgs(in)...)
	if in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	cmd.Env = c.env(in)

	console := in.Console
	if console == nil {
		console = os.Stderr
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	stderrTail := newTail()
	cmd.Stderr = io.MultiWriter(stderrTail, console) // surface errors live too

	if err := cmd.Start(); err != nil {
		return Result{}, err
	}

	// Stream stdout line-by-line: parse each event as it arrives, render readable
	// progress to the console, and retain a bounded tail for the text scans. The
	// ceiling is passed in so a session that crosses it is killed right here,
	// mid-stream, instead of running to the end and being judged after.
	stdoutTail := newTail()
	res := newSessionResult(in)
	c.consume(stdoutPipe, console, stdoutTail, &res, in.MaxSessionTokens, cancel)
	waitErr := cmd.Wait()

	res.Stdout = stdoutTail.String()
	res.Stderr = stderrTail.String()
	res.OutputDropped = stdoutTail.Dropped() + stderrTail.Dropped()
	// The memo is created only now that the text it memoizes is final: keyed on
	// scope alone, a scan taken any earlier would freeze an answer built from an
	// empty Stdout for the whole session.
	res.scans = newScanCache()
	if ee, ok := waitErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if waitErr != nil && !c.TokenCeilingHit(res) {
		return res, waitErr // couldn't run claude at all
	} else if waitErr != nil {
		// Our own ceiling kill: the child died on the cancel, and its exit code
		// is the kill's, not a verdict. The marker below is the verdict.
		res.ExitCode = -1
	}
	return res, nil
}

// consume reads the session's stream-json stdout to the end: parsing and
// rendering every line as it arrives, and retaining a bounded tail in keep.
//
// ceiling is the per-session token ceiling (Invocation.MaxSessionTokens, 0 =
// none) and kill cancels the session's context. When the session's accumulated
// usage crosses the ceiling, the session is marked and killed ON THE SPOT —
// mid-stream, while the runaway is still spending — rather than read to the end
// and judged after. The kill closes our end of the pipe, so the scanner ends on
// a clean EOF: no partial events, no untrusted stream, and the spend parsed so
// far is the figure the budget sees (CLA-343).
//
// # A line too long is a wrong answer, not a missing one
//
// bufio.Scanner ends its loop on a line above maxStreamLine with
// bufio.ErrTooLong, and for years nothing here looked. Every consequence is a
// silent wrong decision by the supervisor, which is the one class this loop is
// built to avoid (CLA-262):
//
//   - The `result` event never arrives, so Tokens and CostUSD stay ZERO and a
//     session that may have spent hundreds of dollars reports nothing to the
//     budget breaker.
//   - A claim's settle is never observed, so the driver reads a task the session
//     handed to review as one it abandoned, and posts `ready` over it.
//   - Abandoning the pipe with the child still writing means cmd.Wait closes our
//     end, claude dies on EPIPE, and the exit code reads as a genuine
//     non-retryable failure — which stops the whole run.
//
// So the error is said out loud on the console (and so into the iteration log),
// the Result is marked untrusted for the driver, and the pipe is DRAINED to
// io.Discard so the child finishes its own write and exits on its own terms.
// Draining is not free — the bytes still arrive — but it is a fixed cost with
// nothing retained, and it buys an honest exit code.
func (c claude) consume(r io.Reader, console io.Writer, keep *tail, res *Result, ceiling int, kill context.CancelFunc) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxStreamLine) // a result event can be large
	for sc.Scan() {
		line := sc.Bytes()
		keep.Write(line)
		keep.Write(newline)
		c.renderAndParse(line, console, res)
		// The kill fires only before the session's own end: a result event that
		// merely REPORTS a total above the ceiling is a finished session, and
		// there is nothing left to stop (see Result.gotResult).
		if ceiling > 0 && !res.gotResult && res.Tokens > ceiling && !c.TokenCeilingHit(*res) {
			fmt.Fprintf(console, "\n!! session crossed its per-session token ceiling (%d tokens ≥ %d) — killing it here, mid-stream, instead of letting the runaway keep spending\n", res.Tokens, ceiling)
			res.markCeilingHit()
			kill()
			// The kill is the end of this stream: the child is dying, and whatever
			// bytes are still buffered in the pipe are not events we may read — the
			// `result` event among them would overwrite the kill marker and turn an
			// orderly ceiling stop into an unclassified failure.
			//
			// The cost of a killed session is therefore UNMEASURED, not zero: the
			// result event is the only carrier of total_cost_usd, and we deliberately
			// never see it. The driver says so on the ceiling branch rather than
			// printing $0.0000 as if it were real (CLA-343 review).
			break
		}
	}
	err := sc.Err()
	if err == nil {
		return
	}
	res.markUntrusted(fmt.Sprintf("claude's stdout could not be read to the end (%v)"+
		": an unknown number of events never reached the parser, so this session's spend, "+
		"its final message and any claim it settled are all incomplete", err))
	fmt.Fprintf(console, "\n!! stream read failed: %v — draining the rest so the session is not killed mid-write; "+
		"this run's parsed figures are NOT trustworthy\n", err)
	// Read the remainder so the child is not killed by EPIPE. It has nowhere to
	// go: nothing can be parsed out of a stream whose framing we have already
	// lost.
	if n, cerr := io.Copy(io.Discard, r); cerr != nil {
		fmt.Fprintf(console, "!! discarded %d further bytes, then: %v\n", n, cerr)
	} else {
		fmt.Fprintf(console, "!! discarded %d further bytes of stdout\n", n)
	}
}

// renderAndParse renders one stream-json event to the console (assistant text and
// tool-use markers) and captures the final result/usage/limit into res.
//
// Token accounting spans both event shapes, verified against claude 2.1.229
// (CLA-343): each `assistant` event carries the API's per-turn usage object
// under `message.usage` — summed here as the session's running total — and the
// final `result` event carries the CUMULATIVE session total, which overwrites
// the sum on arrival. The running total is what the mid-stream ceiling kill
// reads; the result total is what the budget sees.
func (claude) renderAndParse(line []byte, console io.Writer, res *Result) {
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				ID    string          `json:"id"`
				Input json.RawMessage `json:"input"`
				// tool_result blocks, which arrive on a "user" event.
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
				IsError   bool            `json:"is_error"`
			} `json:"content"`
			Usage usage `json:"usage"`
		} `json:"message"`
		Result         string  `json:"result"`
		TerminalReason string  `json:"terminal_reason"`
		TotalCostUSD   float64 `json:"total_cost_usd"`
		Usage          usage   `json:"usage"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	switch ev.Type {
	case "assistant":
		for _, b := range ev.Message.Content {
			switch b.Type {
			case "text":
				if s := strings.TrimSpace(b.Text); s != "" {
					fmt.Fprintln(console, s)
				}
			case "tool_use":
				fmt.Fprintf(console, "  → %s\n", b.Name)
				noteToolUse(b.Name, b.ID, b.Input, res)
			}
		}
		// The API's per-turn usage rides on the assistant event's message. A
		// zero object (a turn that spent nothing, or a shape change) adds
		// nothing — the result event remains authoritative for the total.
		if tot := ev.Message.Usage.total(); tot > 0 {
			res.Tokens += tot
		}
	case "user":
		// Tool results come back on a synthetic user turn.
		for _, b := range ev.Message.Content {
			if b.Type != "tool_result" || b.ToolUseID == "" {
				continue
			}
			// A delivery claim is kept only if the plane ACCEPTED the call that made
			// it — same rule as everything else here, and for the same reason: a
			// refused `update_task` recorded no branch and declared no delivery, so
			// there is nothing to check and nothing to complain about.
			res.settleReport(b.ToolUseID, !b.IsError)

			kind, waiting := res.pending[b.ToolUseID]
			if !waiting {
				continue
			}
			delete(res.pending, b.ToolUseID)
			// A refused call changed nothing on the plane, so it must not change
			// anything here either. This is the whole reason these are judged on
			// the result: an `in_review` rejected for a missing Tests header
			// leaves the task held, and that session is exactly the one whose
			// claim needs handing back.
			if b.IsError {
				continue
			}
			switch kind {
			case pendingClaim:
				noteClaimed(b.Content, b.ToolUseID, res, console)
			case pendingSettle:
				res.Claim.Settled = true
			}
		}
	case "result":
		res.FinalMessage = ev.Result
		res.CostUSD = ev.TotalCostUSD
		// The result event's usage is the session's CUMULATIVE total, so it
		// overwrites the running sum rather than adding to it. On a stream that
		// reached its end this is the number the budget sees; on a stream we
		// killed for the ceiling it never arrives, and the sum stands.
		res.Tokens = ev.Usage.total()
		res.Raw = map[string]any{"terminal_reason": ev.TerminalReason}
		res.gotResult = true
	}
}

// The clankerbar MCP tools the driver watches for. Namespaced exactly as the
// harness reports them, so an unrelated tool called "claim_task" cannot match.
const (
	claimTaskTool        = "mcp__clankerbar__claim_task"
	updateTaskTool       = "mcp__clankerbar__update_task"
	askQuestionTool      = "mcp__clankerbar__ask_question"
	escalateQuestionTool = "mcp__clankerbar__escalate_question"
)

// noteToolUse records what a clankerbar call will mean once its result lands.
//
// `update_task` is not the only way a session lets go. A BLOCKING question ends
// the run and sets the task `blocked` without any `update_task` at all — it is
// the documented one-call handback, and the protocol explicitly calls
// `update_task(status: "blocked")` a trap and steers clankers here instead.
// Missing it is not a missed handback but a wrong one: the driver would post
// `ready` over a task that is waiting on the operator, dropping it back into the
// claimable queue with the question unanswered.
func noteToolUse(name, toolUseID string, input json.RawMessage, res *Result) {
	switch name {
	case claimTaskTool:
		// The ids are in the RESULT, not the arguments. A claim that loses the
		// race carries none and so leaves the tracked claim untouched.
		res.expect(toolUseID, pendingClaim)
		// The REQUESTED task, kept only so a failure can name what it was about.
		// Deliberately never promoted into a Claim: the run id exists only in the
		// result, and a half-claim is the one thing the driver must not be handed.
		var args struct {
			TaskID string `json:"taskId"`
		}
		if json.Unmarshal(input, &args) == nil && args.TaskID != "" {
			res.noteClaimRequest(toolUseID, args.TaskID)
		}

	case updateTaskTool:
		var args struct {
			TaskID   string `json:"taskId"`
			Status   string `json:"status"`
			Branch   string `json:"branch"`
			Delivery struct {
				Commit            string `json:"commit"`
				IntegrationBranch string `json:"integrationBranch"`
				PR                string `json:"pr"`
			} `json:"delivery"`
		}
		if json.Unmarshal(input, &args) != nil || !res.Claim.Names(args.TaskID) {
			return
		}
		if settlesTask(args.Status) {
			res.expect(toolUseID, pendingSettle)
		}
		// The two claims the plane cannot check for itself. Armed here, kept only if
		// the call is accepted, and verified against local git once the session ends
		// (CLA-253).
		res.expectReport(toolUseID, Report{
			TaskID:            res.Claim.TaskID,
			Ref:               res.Claim.Ref,
			RunID:             res.Claim.RunID,
			Status:            args.Status,
			Branch:            args.Branch,
			Commit:            args.Delivery.Commit,
			IntegrationBranch: args.Delivery.IntegrationBranch,
			PR:                args.Delivery.PR,
		})
		// Recording a branch declares pushed work worth handing over, which is
		// exactly what makes the task unsafe to release. Applied on the REQUEST,
		// unlike settling: erring towards "there is WIP" only ever costs the
		// reclaim an expiring lease already costs, while erring the other way
		// strands the branch.
		if args.Branch != "" {
			res.Claim.HasWIP = true
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
		// This one takes a questionId, so the held task cannot be confirmed from
		// the arguments. Escalating blocks the question's task and ends that run,
		// and a session escalating a question about some OTHER task while holding
		// its own is not a thing the loop does. Treat it as settling: the cost of
		// being wrong is one expiring lease, which is the behaviour this whole
		// change replaces — versus un-blocking a task awaiting the operator.
		res.expect(toolUseID, pendingSettle)
	}
}

// noteClaimed records the task/run a successful claim_task returned. A payload
// missing either id (a lost race, a refusal, a shape we do not recognise) leaves
// the tracked claim alone — the driver must never be handed a half-claim it would
// then try to release.
//
// Both ways of giving up SAY SO. They used to return in silence, and the silence
// cost a whole live phased run to localise (CLA-330): everything downstream reads
// `Claim.Held()`, which is false on a zero Claim, so a claim that was never
// recorded is indistinguishable from a session that never claimed — no handback,
// no checkpoint, no complaint. A diagnostic here is the only place the difference
// is still visible.
func noteClaimed(content json.RawMessage, toolUseID string, res *Result, console io.Writer) {
	// Which task the session ASKED for, read off the tool_use arguments. Not
	// enough to record a claim - the run id exists only in the result, and a claim
	// that lost the race would otherwise be tracked as won - but exactly what a
	// diagnostic needs. Without it the operator gets "something went wrong"
	// followed by "ended without holding the task"; with it they get the id of the
	// lease now ticking down unattended.
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
			ID     string `json:"id"`
			Ref    string `json:"ref"`
			Branch string `json:"branch"`
		} `json:"task"`
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
		HasWip bool `json:"hasWip"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		fmt.Fprintf(console, "  · claim NOT tracked%s: its result is not JSON (%v) - %s\n", about, err, snippet(text))
		return
	}
	if payload.Task.ID == "" || payload.Run.ID == "" {
		// Benign when the claim simply lost the race or was refused, which is why
		// this is a note and not a warning. It is worth saying anyway: the same
		// line is what a SHAPE change looks like, and those two are otherwise
		// impossible to tell apart from the outside.
		fmt.Fprintf(console, "  · claim NOT tracked%s: its result carried task id %q and run id %q, and both are needed - %s\n",
			about, payload.Task.ID, payload.Run.ID, snippet(text))
		return
	}
	// A predecessor's pushed work arrives with the claim. Carry it: the task is no
	// safer to release just because THIS session has not written anything yet.
	res.Claim = Claim{
		TaskID: payload.Task.ID,
		Ref:    payload.Task.Ref,
		RunID:  payload.Run.ID,
		HasWIP: payload.HasWip || payload.Task.Branch != "",
	}
	// Say it out loud. Everything downstream is silent by design — a claim that is
	// never observed produces no handback and no complaint, so without this line
	// the feature could quietly stop working (a stream-shape change under a
	// harness upgrade) and look exactly like a run that never claimed anything.
	fmt.Fprintf(console, "  · holding %s (release on exit if nothing is pushed)\n", claimLabel(res.Claim))
}

func claimLabel(c Claim) string {
	if c.Ref != "" {
		return c.Ref
	}
	return c.TaskID
}

// The envelope Claude Code substitutes for a tool_result too large to inline: a
// pointer to the file holding the real thing, plus a truncated preview of it. It
// carries no `is_error` and nothing upstream flags it — the result simply stops
// being the JSON a parser expects.
//
// This is what broke the phase seam (CLA-330), and it is neither of the two
// causes that bug was first attributed to. A `claim_task` result reached 66KB and
// was spilled; `noteClaimed` could not parse the pointer it got instead, so
// `res.Claim` stayed at its zero value. The driver then read `Claim.Held()` as
// false and ended the drain - "the implement phase ended without holding the
// task" - while the plane had that task claimed the whole time, and phase 2 never
// ran.
//
// Where the 66KB came from is worth stating exactly, because the obvious guess is
// wrong: the decision list is ALREADY capped at 20, of 98. Those 20 were 48KB
// between them, on top of a 13KB task detail. So the payload grows with the
// LENGTH of decisions and details, not with how many exist - a count cap is not a
// size cap, which is the same lesson clankerbar's own docs/token-budget.md draws
// from the other side. Any agent-facing payload bounded only by a count will
// eventually cross this threshold.
const persistedOutputMarker = "<persisted-output>"

// Deliberately NOT anchored to the start of a line: the pointer shares its line
// with the size that caused the spill ("Output too large (66.4KB). Full output
// saved to: /..."), and anchoring it was worth one red test to find out.
var persistedOutputRe = regexp.MustCompile(`Full output saved to:[ \t]*(\S[^\n]*?)[ \t]*(?:\n|$)`)

// persistedOutputPath returns the file a spilled tool_result was written to, if
// this result is one.
//
// The path is honoured only when its base name is THIS tool call's id, and that
// guard is the point rather than a nicety. Everything inside a tool_result is
// potentially backlog text quoted back at the driver — the whole subject of
// injection_test.go — so an unguarded "read the path you find in here" would let a
// task body point the driver at a file of its choosing and have the contents
// parsed as a claim, which is a claim on a task nobody asked for and a handback
// aimed at it. A tool_use id is minted by the harness for THIS call in THIS
// stream; text written before the call cannot name it.
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

// readSpilled reads a spilled tool_result under the same bound every other read
// in this package obeys.
//
// output.go states the invariant plainly - "a stream of any size costs a fixed
// amount of memory", and no dial, because a dial is a way to reintroduce the OOM.
// A whole-file read is bounded by nothing but what the plane chose to return,
// which is precisely the thing that grows without warning: clankerbar's own
// list_tasks reached ~253,000 tokens before anyone capped it. Today's spill is
// 66KB, so this is a latent cost rather than a live one, and the cheap time to
// bound it is while the bound is obviously generous.
//
// A file AT the limit is treated as unreadable rather than truncated, following
// maxStreamLine's own rule that a payload above the bound is a lost event and not
// a shortened one: half a JSON document parses as nothing anyway, and saying so is
// worth more than a parse error that names the wrong cause.
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

// snippetMax is how much of an unreadable payload a diagnostic quotes. Enough to
// recognise the shape - an envelope, an MCP error, a lost race - and no more: the
// thing being quoted is routinely the reason the payload was unreadable in the
// first place, and a driver log nobody can scroll is its own kind of silent.
const snippetMax = 160

// snippet bounds and sanitises a diagnostic that quotes a tool_result.
//
// Both of those follow failureDetail in the loop package, for the same two
// reasons, because this is the same kind of string: text from outside rendered to
// a terminal an operator then copies out of.
//
// Non-printables are STRIPPED because this changes what the text is for. Result
// content has until now only ever been fed to a JSON parser, where a control byte
// is inert; here it is printed, where ESC is executed - and this is backlog text,
// quoted verbatim into the stream, which is the threat model injection_test.go
// exists for.
//
// The cut is in RUNES, not bytes. The payloads quoted here carry "·" and "→" from
// the harness's own rendering, so a byte slice would cut a rune in half and emit
// U+FFFD at the seam - mojibake in the one string whose whole purpose is to be
// pasted into a bug report. "..." rather than U+2026 for the same reason.
func snippet(s string) string {
	printable := strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			return r
		}
		return -1
	}, s)
	// Cut first, THEN collapse whitespace: strings.Fields over a 66KB payload
	// allocates a slice header per token to keep 160 bytes of it. The generous
	// pre-cut leaves room for whitespace that is about to collapse.
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

// toolResultText flattens a tool_result's `content`, which the stream renders
// either as a bare string or as an array of typed blocks.
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

// usage is claude's reported token accounting for a session.
//
// The cache fields are the whole point of counting them. `input_tokens` is
// UNCACHED input only, so summing it with output misses the cache reads and
// writes that dominate a long agentic session — one real run reported 140,387
// tokens against $147.98 of actual spend, about $1.05 per thousand, which is no
// model's price. A budget dial that undercounts by an order of magnitude is worse
// than no dial: max_tokens would silently allow roughly ten times what an
// operator set it to.
type usage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
}

func (u usage) total() int {
	return u.InputTokens + u.OutputTokens + u.CacheCreationTokens + u.CacheReadTokens
}

// probe runs the cheapest possible request (tiny prompt, no tools, plain json) to
// answer "am I still limited?" — no streaming, no console.
func (c claude) probe(ctx context.Context, in Invocation) (Result, error) {
	args := []string{"-p", ".", "--output-format", "json", "--permission-mode", "dontAsk", "--allowedTools", ""}
	cmd := exec.CommandContext(ctx, "claude", args...)
	if in.WorkDir != "" {
		cmd.Dir = in.WorkDir
	}
	cmd.Env = c.env(in)

	// Bounded like the drain path, though a probe's answer is a few hundred bytes:
	// the cap is what makes "how much can one session hold" answerable without
	// reasoning about which path produced it.
	stdout, stderr := newTail(), newTail()
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()

	dropped := stdout.Dropped() + stderr.Dropped()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String(), OutputDropped: dropped, scans: newScanCache()}
	// `--output-format json` is ONE object, so a trimmed tail does not merely lose
	// context here — it loses the answer, and parse would fail into a Result that
	// reads exactly like "no limit found". A few hundred bytes never reaches the
	// window, so this is the improbable path; it is checked because the whole point
	// of this change is that a silent zero is worse than a loud stop.
	//
	// Either stream, not just stdout: a limit notice reaches a probe as CLI text on
	// stderr when the JSON never starts, which is half of why this path exists.
	if dropped > 0 {
		res.markUntrusted(fmt.Sprintf("the probe's output overran the retained window (%d bytes dropped), so its verdict cannot be read", dropped))
	}
	if ee, ok := runErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if runErr != nil {
		return res, runErr
	}
	c.parse(&res)
	return res, nil
}

func (claude) env(in Invocation) []string {
	env := append(os.Environ(), in.Env...)
	// Never truncate the session while subagents/background work run.
	env = append(env, "CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0")
	if in.ConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+in.ConfigDir)
	}
	return env
}

// parse reads `claude --output-format json` — a single JSON object (probe path).
func (claude) parse(res *Result) {
	var p struct {
		IsError        bool    `json:"is_error"`
		Result         string  `json:"result"`
		TerminalReason string  `json:"terminal_reason"`
		TotalCostUSD   float64 `json:"total_cost_usd"`
		Usage          usage   `json:"usage"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &p); err != nil {
		return
	}
	res.FinalMessage = p.Result
	res.CostUSD = p.TotalCostUSD
	res.Tokens = p.Usage.total()
	res.Raw = map[string]any{"terminal_reason": p.TerminalReason, "is_error": p.IsError}
}

// claudeScope is how much of claude's output a scan is allowed to read.
//
// Both scans used to run over the whole of Stdout+Stderr, and Stdout is the entire
// raw NDJSON stream: every assistant message, and every `tool_result` block, which
// carries the verbatim MCP response to `claim_task`. So a backlog task whose body
// merely said "hit your" reported a usage limit that never happened — and the loop
// then slept, re-spawned the same paid session, re-claimed the same task and did it
// again, with every budget ceiling sitting inert inside the drain (CLA-258). Same
// defect, same fix, as opencodeErrorText.
type claudeScope int

const (
	// claudeTyped is the narrow width: only what the CLI itself authored outside
	// the event stream, or set in a machine-written field — stderr, stdout lines
	// that are not events at all, and `terminal_reason`. Nothing here can carry a
	// word the agent chose.
	claudeTyped claudeScope = iota
	// claudeDiagnostic adds the free text of a session the CLI says ended BADLY:
	// the `result` field of a failed result event, and typed error events whole.
	claudeDiagnostic
)

// claudeText collects claude's own output at the given width, so a scan never
// reads the agent's narration.
//
// Non-event stdout lines are kept deliberately: under `--output-format stream-json`
// every word the agent says arrives inside a JSON event, so a bare line is the CLI
// talking — which is how a usage-limit notice arrives when the stream never starts.
// A line that starts like an event but does not parse is dropped rather than read
// raw; a half-written event is not something to classify a run on.
func claudeText(res Result, scope claudeScope) string {
	return res.scan(int(scope), func() string { return buildClaudeText(res, scope) })
}

func buildClaudeText(res Result, scope claudeScope) string {
	var b strings.Builder
	b.WriteString(res.Stderr)

	// The probe path uses `--output-format json` — ONE object, with no
	// one-event-per-line contract to lean on. Take it whole. Walking a
	// pretty-printed object line by line would fail to parse the opening `{` and
	// then read every remaining line as CLI text, which is precisely how the
	// agent's own `result` would get back into the scan.
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

// claudeEvent is the sliver of a stream event that says whether the CLI is
// reporting a failure, and what it called it.
type claudeEvent struct {
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	IsError        bool   `json:"is_error"`
	Result         string `json:"result"`
	TerminalReason string `json:"terminal_reason"`
}

// failed reports whether the CLI itself said this session ended badly — the
// condition under which its `result` text is the CLI's own words rather than the
// agent's closing summary.
func (ev claudeEvent) failed() bool {
	return ev.IsError || ev.TerminalReason != "" || (ev.Subtype != "" && ev.Subtype != "success")
}

func writeClaudeEvent(b *strings.Builder, ev claudeEvent, line string, scope claudeScope) {
	switch {
	case ev.Type == "result":
		// terminal_reason is a typed field the CLI sets; never the agent's words,
		// so it is read at both widths.
		if ev.TerminalReason != "" {
			b.WriteByte('\n')
			b.WriteString(ev.TerminalReason)
		}
		// `result` is free text. On a clean finish it is the agent's own closing
		// summary — narration by another name.
		if scope == claudeDiagnostic && ev.failed() {
			b.WriteByte('\n')
			b.WriteString(ev.Result)
		}
	case scope == claudeDiagnostic && strings.Contains(ev.Type, "error"):
		// A typed error event, read whole — the parity codex and opencode already
		// have. Dropping these would turn a retryable blip the CLI announced this
		// way into a "non-retryable, stopping".
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
		// The reset comes from the NARROW width, unlike the detection above. It is
		// not just how long to nap: waitPastBudget abandons the run outright when a
		// reset lands past the wall-clock ceiling, so a reset read out of free text
		// would let backlog text end the run. Losing it costs only the upper bound
		// — the supervised wait polls for an early lift regardless.
		ResetAt: parseClaudeReset(claudeText(res, claudeTyped)),
		Reason:  "usage_limit",
	}
}

// claudeTransientRe anchors on the "API Error:" prefix (and bare connection
// errors) so a task log that legitimately mentions an HTTP 500 can't be mistaken
// for a dead session. A 400 bad-request is NOT here — retrying won't help it.
// Ported from loop.sh's TRANSIENT_RE.
//
// The anchoring is the second line of defence, not the first: it is applied to
// claudeText, so the agent's narration is not scanned at all.
//
// The mid-response arm is not a guess. Claude Code's error reference documents
// exactly three "the response above may be incomplete" variants, emitted when a
// stream fails after Claude has completed a block of text or a tool call:
//
//	API Error: Server error mid-response. The response above may be incomplete.
//	API Error: Connection closed mid-response. The response above may be incomplete.
//	API Error: Response stalled mid-stream. The response above may be incomplete.
//
// All three missed every arm above. "Connection closed" is not "connection
// error"; "Server error" is not "internal server"; a stalled stream names no
// status code — so the 5\d\d arm has nothing to catch either. Every one of them
// is a network/server fault that a fresh session is exactly the right answer to,
// and the cost of missing one is not a lost iteration but a stopped daemon
// (loop.go treats an unrecognised non-zero exit as fatal).
//
// Matching on "mid-response|mid-stream" rather than on "connection|closed" keeps
// the arm as narrow as the documented wording: those two tokens are the CLI's
// own, they cover all three variants including ones with no connection word in
// them, and they do not fire on the ordinary English an error string quotes.
//
// The same doc says that in headless mode ("--output-format json" or
// "stream-json") this message is reported in the `result` field. claudeText only
// reads `result` when the CLI marked the session failed, which is the correct
// gate rather than a gap: a session that keeps its partial output and exits zero
// is never classified at all, and one that reports the turn as an error is
// exactly the one whose exit code brings the loop here.
//
// See https://code.claude.com/docs/en/errors#the-response-above-may-be-incomplete
var claudeTransientRe = regexp.MustCompile(`(?i)api error: (408|429|5\d\d)` +
	`|api error:.*(overloaded|internal server|bad gateway|service unavailable|gateway time|too many requests)` +
	`|api error:.*(mid-response|mid-stream)` +
	`|connection error|fetch failed|econnreset|econnrefused|etimedout|eai_again|socket hang up|network (error|timeout)`)

func (claude) IsTransient(res Result) bool {
	return claudeTransientRe.MatchString(claudeText(res, claudeDiagnostic))
}

// claudeMaxTurnsReason is what `claude -p --max-turns N` reports when the cap
// fires, verified against claude 2.1.226:
//
//	exit 1, {"type":"result","subtype":"error_max_turns","is_error":true,
//	         "result":null,"terminal_reason":"max_turns", ...}
//
// `result` is null and the text matches neither the usage-limit scan nor the
// transient one, so WITHOUT this classification the loop reaches its
// non-retryable branch and ends the whole run. That is the phase backstop
// killing the daemon it was added to protect.
//
// --max-turns is not in `claude --help` for that version; it is accepted (an
// unknown flag exits 1 with "error: unknown option"), so this is a dependency on
// an undocumented flag, pinned here against a named version deliberately.
const claudeMaxTurnsReason = "max_turns"

func (claude) TurnCapped(res Result) bool {
	if res.Raw == nil {
		return false
	}
	r, _ := res.Raw["terminal_reason"].(string)
	return r == claudeMaxTurnsReason
}

// tokenCeilingReason is the terminal_reason the ADAPTER writes into a Result
// when it killed the session for crossing Invocation.MaxSessionTokens
// (CLA-343). It is deliberately not a reason the CLI ever emits: the marker
// exists so the driver can tell an orderly ceiling kill from a genuine failure,
// and a marker the CLI itself could produce would be indistinguishable from
// one — same reasoning as claudeMaxTurnsReason, except that one is the CLI's
// own word and this one is ours.
const tokenCeilingReason = "token_ceiling_hit"

func (claude) TokenCeilingHit(res Result) bool {
	if res.Raw == nil {
		return false
	}
	r, _ := res.Raw["terminal_reason"].(string)
	return r == tokenCeilingReason
}

// markCeilingHit writes the adapter's own kill marker onto a Result.
func (r *Result) markCeilingHit() {
	r.Raw = map[string]any{"terminal_reason": tokenCeilingReason}
}

// Claude is the one adapter that watches the session's clankerbar tool calls, so
// it is the only one phases can run on today. See Capabilities.TracksClaims.
func (claude) Capabilities() Capabilities {
	return Capabilities{TracksClaims: true, HonoursMaxTurns: true}
}

// Diagnostic returns the same CLI-authored text IsTransient judged, so a caller
// that stops on a non-retryable exit can say WHICH message it stopped on. It is
// deliberately the identical scope: showing an operator text the classifier
// never read would send them chasing a string that could not have mattered.
func (claude) Diagnostic(res Result) string {
	return claudeText(res, claudeDiagnostic)
}

func (c claude) Probe(ctx context.Context, in Invocation) (ProbeResult, error) {
	in.Probe = true
	res, err := c.Invoke(ctx, in)
	// Spend first, and off the Result on every path: a probe that exited non-zero
	// still cost what it cost, and under-counting is the one direction a budget
	// breaker must not err in. On the error return this is whatever got parsed,
	// which today is nothing — see ProbeResult and CLA-299.
	out := ProbeResult{Tokens: res.Tokens, CostUSD: res.CostUSD}
	if err != nil {
		return out, err
	}
	return probeVerdict(out, res, c.DetectLimit)
}

func (claude) ReadUsage(context.Context, Invocation) (Usage, error) {
	// /usage is TTY-only; --output-format json carries no remaining quota; no
	// local file persists it (see the memo, and anthropics/claude-code#32796).
	return Usage{}, ErrUsageUnsupported
}

// parseClaudeReset extracts the reset time from a message like
//
//	You've hit your session limit · resets 9:40pm (Europe/Madrid)
//	You've hit your weekly limit · resets Sunday 12:00am
//
// The reset is only an upper bound for the supervised wait (the loop still polls
// for an early reset), so this is deliberately best-effort: on any doubt it
// returns the zero time and the loop falls back to interval polling.
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
