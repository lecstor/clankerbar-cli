package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// The tool-observer half of a harness session: what a clankerbar MCP call means
// once its result lands. Shared by every adapter (claude, opencode, copilot), so
// it lives here rather than in any one adapter's file.

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

	// A blocking ask has its own bookkeeping: the DRIVER needs the task it named even when the
	// session held no claim (a runner-claimed session asks on the runner's behalf), while a
	// success must still count as this session's handback when it DOES name the held claim.
	if ask, waiting := res.pendingAsks[toolUseID]; waiting {
		delete(res.pendingAsks, toolUseID)
		if isError {
			return
		}
		res.BlockingAsk = ask.taskID
		if ask.mine {
			res.Claim.Settled = true
			res.notifyClaim()
		}
		return
	}

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
		if json.Unmarshal(input, &args) != nil || !args.Blocking {
			return
		}
		// Armed for ANY task: the driver compares it with the task IT holds, which a
		// runner-claimed session never sees as its own claim. `mine` is the narrower
		// question the session itself needs answered on success.
		if res.pendingAsks == nil {
			res.pendingAsks = map[string]askRequest{}
		}
		res.pendingAsks[toolUseID] = askRequest{taskID: args.TaskID, mine: res.Claim.Names(args.TaskID)}

	case escalateQuestionTool:
		// This one takes a questionId, so the held task cannot be confirmed from the arguments. The
		// call promotes a question the session already asked, and a successful one blocks the task
		// it holds exactly like a blocking ask — so it is the claim's label the driver waits on.
		ref := res.Claim.Ref
		if ref == "" {
			ref = res.Claim.TaskID
		}
		if ref != "" {
			if res.pendingAsks == nil {
				res.pendingAsks = map[string]askRequest{}
			}
			res.pendingAsks[toolUseID] = askRequest{taskID: ref, mine: true}
			return
		}
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
