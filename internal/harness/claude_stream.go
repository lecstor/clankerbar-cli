package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// consume reads the session's stream-json stdout to the end: parsing and rendering every line as it
// arrives, and retaining a bounded tail in keep.
func (c claude) consume(r io.Reader, console io.Writer, keep *tail, res *Result, ceiling int, kill context.CancelFunc) {
	// The no-output watchdog. Armed here because this is where the session's kill switch is in hand,
	// and the stream below is what feeds it.
	res.idle = newIdleGuard(res.maxIdle, res.maxToolCall, func(cause string) {
		fmt.Fprintf(console, "\n!! %s - killing the session here instead of waiting on a silent child\n", cause)
		kill()
	})
	defer res.idle.stop()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxStreamLine) // a result event can be large
	for sc.Scan() {
		line := sc.Bytes()
		res.idle.seen()
		keep.Write(line)
		keep.Write(newline)
		c.renderAndParse(line, console, res)
		// The kill fires only before the session's own end: a result event that merely REPORTS a total
		// above the ceiling is a finished session, and there is nothing left to stop (see
		// Result.gotResult).
		if ceiling > 0 && !res.gotResult && res.Tokens > ceiling && !c.TokenCeilingHit(*res) {
			fmt.Fprintf(console, "\n!! session crossed its per-session token ceiling (%d tokens ≥ %d) — killing it here, mid-stream, instead of letting the runaway keep spending\n", res.Tokens, ceiling)
			res.markCeilingHit()
			kill()
			break
		}
		if cause := res.toolLoop.tripped(); cause != "" && res.ToolLoop == "" {
			res.ToolLoop = cause
			fmt.Fprintf(console, "\n!! %s - killing the session here instead of letting it retry the same failing call\n", cause)
			kill()
			break
		}
		if res.idle.tripped() != "" {
			break
		}
	}
	// Disarmed BEFORE the verdict is read: a timer that lands as the stream ends must not report a
	// kill for a session that had already finished.
	res.idle.stop()
	res.IdleKilled = res.idle.tripped()
	res.IdleToolCall = res.IdleKilled != "" && res.idle.trippedWithOpenCall()
	err := sc.Err()
	if err == nil {
		return
	}
	res.markUntrusted(fmt.Sprintf("claude's stdout could not be read to the end (%v)"+
		": an unknown number of events never reached the parser, so this session's spend, "+
		"its final message and any claim it settled are all incomplete", err))
	fmt.Fprintf(console, "\n!! stream read failed: %v — draining the rest so the session is not killed mid-write; "+
		"this run's parsed figures are NOT trustworthy\n", err)
	// Read the remainder so the child is not killed by EPIPE. It has nowhere to go: nothing can be
	// parsed out of a stream whose framing we have already lost.
	if n, cerr := io.Copy(io.Discard, r); cerr != nil {
		fmt.Fprintf(console, "!! discarded %d further bytes, then: %v\n", n, cerr)
	} else {
		fmt.Fprintf(console, "!! discarded %d further bytes of stdout\n", n)
	}
}

// renderAndParse renders one stream-json event to the console (assistant text and tool-use markers)
// and captures the final result/usage/limit into res.
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
			// A POINTER, so a per-turn usage object that is present but all zeros is distinguishable from
			// one that never came: the first is a report, the second is the silence the driver's bound
			// counts.
			Usage *usage `json:"usage"`
		} `json:"message"`
		Result         string `json:"result"`
		TerminalReason string `json:"terminal_reason"`
		// A POINTER, so a result event that carries no cost at all is distinguishable from one that
		// reports 0: only the latter is a figure the durable record may carry.
		TotalCostUSD *float64 `json:"total_cost_usd"`
		Usage        usage    `json:"usage"`
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
				// Transcript tier: one event per text block, unmerged and un-trimmed.
				noteAssistantText(res.events, b.Text)
			case "tool_use":
				fmt.Fprintf(console, "  → %s\n", b.Name)
				noteToolUse(b.Name, b.ID, b.Input, res)
				NoteClaudeToolEvent(res.events, b.Name, b.ID, b.Input)
				// The `.env*` rule needs the ARGUMENTS, and claude reports the result on a later event that
				// carries only the id — decide now.
				res.noteEnvRead(b.Name, b.ID, b.Input)
				res.noteToolName(b.ID, b.Name)
				res.idle.toolStarted(b.ID)
			}
		}
		// The API's per-turn usage rides on the assistant event's message, and it is a usage-bearing
		// event in its own right: a session killed mid-stream never reaches the result event, and it
		// still told us what it had spent by the time it died.
		if u := ev.Message.Usage; u != nil {
			res.UsageReported = true
			if tot := u.total(); tot > 0 {
				res.Tokens += tot
			}
			// The normalised breakdown is summed on the same terms as Tokens above: the API reports each
			// turn's own usage, and the result event below overwrites both if the stream reaches it.
			res.Usage.Add(u.normalised())
		}
	case "user":
		// Tool results come back on a synthetic user turn.
		for _, b := range ev.Message.Content {
			if b.Type != "tool_result" || b.ToolUseID == "" {
				continue
			}
			noteToolResult(res, b.ToolUseID, b.IsError, b.Content, console)
			// Transcript tier: the output itself, or its `.env*` redaction verdict, before the clankerbar
			// observation consumes it.
			noteClaudeToolResult(res.events, b.ToolUseID, b.IsError, b.Content, res.takeEnvRead(b.ToolUseID))
			res.idle.toolFinished(b.ToolUseID)
			// CLA-739: every tool, not only clankerbar's - a loop on any tool re-sends the context just the
			// same.
			if res.toolLoop != nil {
				var text string
				if b.IsError {
					text = toolResultText(b.Content)
				}
				res.toolLoop.observe(res.takeToolName(b.ToolUseID), b.IsError, text)
			}
		}
	case "result":
		res.FinalMessage = ev.Result
		// The turn this user message started is over. The claude adapter uses this to retire one
		// outstanding stdin turn; every other adapter leaves it nil.
		if res.onTurnEnd != nil {
			res.onTurnEnd()
		}
		// The session's DOLLARS arrive here and nowhere else, so the flag that says "a cost figure was
		// seen" is set here too: a session killed before this event reported tokens but no money, and the
		// durable record must carry the absence rather than a zero nobody measured.
		if ev.TotalCostUSD != nil {
			res.CostUSD = *ev.TotalCostUSD
			res.CostReported = true
		}
		// The result event's usage is the session's CUMULATIVE total, so it overwrites the running sum
		// rather than adding to it.
		res.Tokens = ev.Usage.total()
		// The CUMULATIVE usage, so the breakdown is overwritten rather than added to, exactly as Tokens
		// is above.
		res.Usage = ev.Usage.normalised()
		res.Raw = map[string]any{"terminal_reason": ev.TerminalReason}
		res.gotResult = true
		// The session's own accounting arrived, whatever it adds up to - a zero-token result event is a
		// report, not a silence.
		res.UsageReported = true
	}
}
