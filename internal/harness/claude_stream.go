package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

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
	// The no-output watchdog. Armed here because this is where the session's kill
	// switch is in hand, and the stream below is what feeds it. It is always
	// non-nil so the deferred stop can be unconditional; a zero bound leaves it
	// disabled.
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
		// CLA-739: the same tool returned the same error on enough consecutive
		// calls that the session is looping, and every further retry re-sends
		// the whole context. Killed here for the ceiling's reasons, and the
		// stream ends here for the same one: what is still in the pipe arrived
		// after the verdict.
		if cause := res.toolLoop.tripped(); cause != "" && res.ToolLoop == "" {
			res.ToolLoop = cause
			fmt.Fprintf(console, "\n!! %s - killing the session here instead of letting it retry the same failing call\n", cause)
			kill()
			break
		}
		// The idle watchdog fires on its own goroutine, with the same reasons for
		// ending the read here: the child is dying and whatever is still buffered
		// arrived after the verdict.
		if res.idle.tripped() != "" {
			break
		}
	}
	// Disarmed BEFORE the verdict is read: a timer that lands as the stream ends
	// must not report a kill for a session that had already finished.
	res.idle.stop()
	res.IdleKilled = res.idle.tripped()
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
			// A POINTER, so a per-turn usage object that is present but all
			// zeros is distinguishable from one that never came: the first is a
			// report, the second is the silence the driver's bound counts
			// (CLA-288).
			Usage *usage `json:"usage"`
		} `json:"message"`
		Result         string `json:"result"`
		TerminalReason string `json:"terminal_reason"`
		// A POINTER, so a result event that carries no cost at all is
		// distinguishable from one that reports 0 (CLA-691): only the latter is a
		// figure the durable record may carry.
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
			case "tool_use":
				fmt.Fprintf(console, "  → %s\n", b.Name)
				noteToolUse(b.Name, b.ID, b.Input, res)
				res.noteToolName(b.ID, b.Name)
				res.idle.toolStarted(b.ID)
			}
		}
		// The API's per-turn usage rides on the assistant event's message, and it
		// is a usage-bearing event in its own right: a session killed mid-stream
		// never reaches the result event, and it still told us what it had spent
		// by the time it died (CLA-288). A zero object (a turn that spent
		// nothing, or a shape change) adds nothing to the SUM - the result event
		// remains authoritative for the total - but the report is recorded either
		// way, because a report of zero is not silence.
		if u := ev.Message.Usage; u != nil {
			res.UsageReported = true
			if tot := u.total(); tot > 0 {
				res.Tokens += tot
			}
			// The normalised breakdown (CLA-691) is summed on the same terms as
			// Tokens above: the API reports each turn's own usage, and the
			// result event below overwrites both if the stream reaches it.
			res.Usage.Add(u.normalised())
		}
	case "user":
		// Tool results come back on a synthetic user turn.
		for _, b := range ev.Message.Content {
			if b.Type != "tool_result" || b.ToolUseID == "" {
				continue
			}
			noteToolResult(res, b.ToolUseID, b.IsError, b.Content, console)
			res.idle.toolFinished(b.ToolUseID)
			// CLA-739: every tool, not only clankerbar's - a loop on any tool
			// re-sends the context just the same. The error text is only read
			// for a failed call; a success resets the streak whatever it says.
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
		// The session's DOLLARS arrive here and nowhere else, so the flag that says
		// "a cost figure was seen" (CLA-691) is set here too: a session killed
		// before this event reported tokens but no money, and the durable record
		// must carry the absence rather than a zero nobody measured.
		if ev.TotalCostUSD != nil {
			res.CostUSD = *ev.TotalCostUSD
			res.CostReported = true
		}
		// The result event's usage is the session's CUMULATIVE total, so it
		// overwrites the running sum rather than adding to it. On a stream that
		// reached its end this is the number the budget sees; on a stream we
		// killed for the ceiling it never arrives, and the sum stands.
		res.Tokens = ev.Usage.total()
		// The CUMULATIVE usage, so the breakdown is overwritten rather than added
		// to, exactly as Tokens is above (CLA-691).
		res.Usage = ev.Usage.normalised()
		res.Raw = map[string]any{"terminal_reason": ev.TerminalReason}
		res.gotResult = true
		// The session's own accounting arrived, whatever it adds up to - a
		// zero-token result event is a report, not a silence (CLA-288).
		res.UsageReported = true
	}
}
