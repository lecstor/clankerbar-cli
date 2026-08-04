package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
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

func (c claude) Invoke(ctx context.Context, in Invocation) (Result, error) {
	if in.Probe {
		return c.probe(ctx, in)
	}

	args := []string{"-p", in.Prompt, "--output-format", "stream-json", "--verbose", "--permission-mode", "acceptEdits"}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
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

	cmd := exec.CommandContext(ctx, "claude", args...)
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
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(&stderrBuf, console) // surface errors live too

	if err := cmd.Start(); err != nil {
		return Result{}, err
	}

	// Stream stdout line-by-line: keep the raw NDJSON (for text scans and the
	// logfile) and render readable progress to the console as it arrives.
	var stdoutRaw bytes.Buffer
	res := Result{}
	sc := bufio.NewScanner(stdoutPipe)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // a result event can be large
	for sc.Scan() {
		line := sc.Bytes()
		stdoutRaw.Write(line)
		stdoutRaw.WriteByte('\n')
		c.renderAndParse(line, console, &res)
	}
	waitErr := cmd.Wait()

	res.Stdout = stdoutRaw.String()
	res.Stderr = stderrBuf.String()
	if ee, ok := waitErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if waitErr != nil {
		return res, waitErr // couldn't run claude at all
	}
	return res, nil
}

// renderAndParse renders one stream-json event to the console (assistant text and
// tool-use markers) and captures the final result/usage/limit into res.
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
	case "user":
		// Tool results come back on a synthetic user turn.
		for _, b := range ev.Message.Content {
			if b.Type != "tool_result" || b.ToolUseID == "" {
				continue
			}
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
				noteClaimed(b.Content, res, console)
			case pendingSettle:
				res.Claim.Settled = true
			}
		}
	case "result":
		res.FinalMessage = ev.Result
		res.CostUSD = ev.TotalCostUSD
		res.Tokens = ev.Usage.total()
		res.Raw = map[string]any{"terminal_reason": ev.TerminalReason}
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

	case updateTaskTool:
		var args struct {
			TaskID string `json:"taskId"`
			Status string `json:"status"`
			Branch string `json:"branch"`
		}
		if json.Unmarshal(input, &args) != nil || !res.Claim.Names(args.TaskID) {
			return
		}
		if settlesTask(args.Status) {
			res.expect(toolUseID, pendingSettle)
		}
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
func noteClaimed(content json.RawMessage, res *Result, console io.Writer) {
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
	if json.Unmarshal([]byte(toolResultText(content)), &payload) != nil {
		return
	}
	if payload.Task.ID == "" || payload.Run.ID == "" {
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

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
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

func (claude) DetectLimit(res Result) Limit {
	reason, _ := res.Raw["terminal_reason"].(string)
	if reason == "usage_limit" || strings.Contains(res.Stdout+res.Stderr, "hit your") {
		return Limit{
			Limited: true,
			ResetAt: parseClaudeReset(res.Stdout + res.Stderr),
			Reason:  "usage_limit",
		}
	}
	return Limit{}
}

// claudeTransientRe anchors on the "API Error:" prefix (and bare connection
// errors) so a task log that legitimately mentions an HTTP 500 can't be mistaken
// for a dead session. A 400 bad-request is NOT here — retrying won't help it.
// Ported from loop.sh's TRANSIENT_RE.
var claudeTransientRe = regexp.MustCompile(`(?i)api error: (408|429|5\d\d)` +
	`|api error:.*(overloaded|internal server|bad gateway|service unavailable|gateway time|too many requests)` +
	`|connection error|fetch failed|econnreset|econnrefused|etimedout|eai_again|socket hang up|network (error|timeout)`)

func (claude) IsTransient(res Result) bool {
	return claudeTransientRe.MatchString(res.Stdout + res.Stderr)
}

func (c claude) Probe(ctx context.Context, in Invocation) (Limit, error) {
	in.Probe = true
	res, err := c.Invoke(ctx, in)
	if err != nil {
		return Limit{}, err
	}
	return c.DetectLimit(res), nil
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
