package harness

import (
	"encoding/json"
	"fmt"
	"strings"
)

// toolloop.go - CLA-739: kill a session that keeps making the same failing tool call.

// toolLoopGuard counts consecutive failing calls of one tool with one error. A nil guard is
// disabled, and every method is safe on nil, so an adapter can hold one unconditionally and an
// Invocation that set no threshold costs nothing.
type toolLoopGuard struct {
	max int

	tool  string
	err   string
	count int

	// cause is set on the call that reaches max, and then never changes: the session is being killed,
	// and anything parsed after the kill is not evidence about why.
	cause string
}

// newToolLoopGuard returns a guard that trips on the max'th consecutive identical failure, or nil
// (disabled) when max is not positive.
func newToolLoopGuard(max int) *toolLoopGuard {
	if max <= 0 {
		return nil
	}
	return &toolLoopGuard{max: max}
}

// observe feeds one SETTLED tool call: the tool's name, whether it failed, and the failure text.
func (g *toolLoopGuard) observe(tool string, failed bool, errText string) bool {
	if g == nil || g.cause != "" {
		return false
	}
	if !failed {
		g.tool, g.err, g.count = "", "", 0
		return false
	}
	if g.count > 0 && tool == g.tool && errText == g.err {
		g.count++
	} else {
		g.tool, g.err, g.count = tool, errText, 1
	}
	if g.count < g.max {
		return false
	}
	g.cause = toolLoopCause(tool, g.count, errText)
	return true
}

// tripped returns the cause once the guard has fired, "" before.
func (g *toolLoopGuard) tripped() string {
	if g == nil {
		return ""
	}
	return g.cause
}

// ToolLoopCausePrefix opens every Result.ToolLoop cause, so a reader of the fleet record or the
// daemon log can match the class without parsing the rest.
const ToolLoopCausePrefix = "repeated failing tool call"

// toolLoopCause names the loop for an operator: the tool, how many consecutive calls failed, and
// the error, cut short.
func toolLoopCause(tool string, n int, errText string) string {
	name := snippet(tool)
	if strings.TrimSpace(tool) == "" {
		name = "(unnamed tool)"
	}
	return fmt.Sprintf("%s: %s x%d: %s", ToolLoopCausePrefix, name, n, snippet(toolErrorSummary(errText)))
}

// toolErrorSummary reduces a tool's error text to the part that identifies it. The clankerbar plane
// answers a refusal as {"error":{"code":..,"message":..}}, and the code is the stable half, so that
// shape renders as "code: message" - putting the code first, where a truncated line still shows it.
func toolErrorSummary(errText string) string {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(errText)), &env) == nil && env.Error.Code != "" {
		if env.Error.Message == "" {
			return env.Error.Code
		}
		return env.Error.Code + ": " + env.Error.Message
	}
	return errText
}

// noteToolName remembers a claude tool_use's name until its result arrives, so the guard can key on
// the tool. Only while a guard is armed: with the check off there is nothing to remember it for.
func (r *Result) noteToolName(id, name string) {
	if r.toolLoop == nil || id == "" {
		return
	}
	if r.toolNames == nil {
		r.toolNames = map[string]string{}
	}
	r.toolNames[id] = name
}

// takeToolName returns and forgets the name noteToolName recorded for id, "" when none was.
// Forgetting is what keeps the map bounded by the calls still in flight rather than by the
// session's length.
func (r *Result) takeToolName(id string) string {
	name := r.toolNames[id]
	delete(r.toolNames, id)
	return name
}
