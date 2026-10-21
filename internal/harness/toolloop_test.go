package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The incident fixture (CLA-739): a trimmed excerpt of the 2026-10-20 Luna panel
// session that called clankerbar_submit_for_review 316 times, 315 of them refused with
// the same `invalid_release` error. Its shape, in order: a heartbeat that
// succeeded, two refused update_task calls, a get_task that succeeded, then
// seven more refused update_task calls. The run credential's secret half is
// redacted, the large get_task result and each event's rawInput are trimmed;
// nothing else is changed.
const toolLoopFixture = "testdata/opencode-repeated-tool-failure.jsonl"

// incidentError is the error every refused call in the fixture returned.
const incidentError = "{\"error\":{\"code\":\"invalid_release\",\"message\":\"`driverReleased` marks a run the DRIVER ended on a session's behalf - pass it only with `release: true` or `expireLease: true`, never on a status or revision call.\"}}"

func readFixtureLines(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	var lines [][]byte
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), maxStreamLine)
	for sc.Scan() {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning fixture: %v", err)
	}
	return lines
}

// feedOpencode streams lines through a real parser armed at threshold and
// reports the cause, and how many update_task events had been fed when the kill
// fired (0 = never fired).
func feedOpencode(t *testing.T, lines [][]byte, threshold int) (cause string, firedAt int, kills int) {
	t.Helper()
	p := newOpencodeParse(Invocation{MaxRepeatedToolFailures: threshold}, io.Discard)
	updates := 0
	p.kill = func() {
		kills++
		if firedAt == 0 {
			firedAt = updates
		}
	}
	for _, l := range lines {
		if bytes.Contains(l, []byte(`"tool":"clankerbar_submit_for_review"`)) {
			updates++
		}
		p.line(l)
	}
	return p.observed.toolLoop.tripped(), firedAt, kills
}

// The incident, replayed: the guard does NOT fire on the two refusals before the
// get_task (a different tool's success breaks the streak), and fires on the
// fifth consecutive refusal after it - the 7th update_task overall. The cause
// names the tool, the count and the error, code first.
func TestToolLoopIncidentFixtureKillsOnTheFifthConsecutiveRefusal(t *testing.T) {
	cause, firedAt, kills := feedOpencode(t, readFixtureLines(t, toolLoopFixture), 5)
	if kills != 1 {
		t.Fatalf("kill called %d times, want exactly 1 - the guard fires once and the session is gone", kills)
	}
	if firedAt != 7 {
		t.Errorf("fired on update_task #%d, want #7: 2 refusals, a get_task that resets the streak, then 5 more", firedAt)
	}
	want := "repeated failing tool call: clankerbar_submit_for_review x5: invalid_release: `driverReleased` marks a run the DRIVER ended"
	if !strings.HasPrefix(cause, want) {
		t.Errorf("cause = %q, want it to start %q", cause, want)
	}
	if !strings.HasSuffix(cause, "...") {
		t.Errorf("cause = %q, want the error cut short with \"...\"", cause)
	}
}

// Identical errors with VARYING inputs still count. In the fixture the first two
// refusals carry no `branch` and the last seven carry one; with the get_task
// between them taken out, the nine are one streak of the same error across two
// distinct inputs, and the guard fires on the fifth.
func TestToolLoopCountsIdenticalErrorsAcrossVaryingInputs(t *testing.T) {
	var lines [][]byte
	inputs := map[string]bool{}
	for _, l := range readFixtureLines(t, toolLoopFixture) {
		if bytes.Contains(l, []byte(`"tool":"clankerbar_get_task"`)) {
			continue
		}
		var ev opencodeEvent
		if json.Unmarshal(l, &ev) == nil && ev.Part != nil && ev.Part.Tool == "clankerbar_submit_for_review" {
			inputs[string(ev.Part.State.Input)] = true
		}
		lines = append(lines, l)
	}
	if len(inputs) < 2 {
		t.Fatalf("fixture carries %d distinct update_task inputs; this test needs at least 2", len(inputs))
	}
	_, firedAt, kills := feedOpencode(t, lines, 5)
	if kills != 1 || firedAt != 5 {
		t.Errorf("kills=%d firedAt=#%d, want one kill on update_task #5 - the inputs differ, the error does not", kills, firedAt)
	}
}

// Threshold 0 disables the check: the whole incident streams through untouched.
func TestToolLoopThresholdZeroDisablesTheCheck(t *testing.T) {
	cause, _, kills := feedOpencode(t, readFixtureLines(t, toolLoopFixture), 0)
	if kills != 0 || cause != "" {
		t.Errorf("kills=%d cause=%q with threshold 0, want no kill and no cause", kills, cause)
	}
	if g := newToolLoopGuard(0); g != nil {
		t.Errorf("newToolLoopGuard(0) = %+v, want nil (off)", g)
	}
	if g := newSessionResult(Invocation{MaxRepeatedToolFailures: 5, Probe: true}).toolLoop; g != nil {
		t.Error("a probe armed the guard; a probe has no tool calls to repeat and must never be killed for one")
	}
}

// The streak rules, on the guard itself.
func TestToolLoopGuardStreakRules(t *testing.T) {
	const tool, err = "clankerbar_submit_for_review", incidentError
	cases := []struct {
		name  string
		calls func(g *toolLoopGuard) bool // reports whether any call tripped
		trip  bool
	}{
		{"five identical failures trip", func(g *toolLoopGuard) bool {
			return g.observe(tool, true, err) || any4(g, tool, err)
		}, true},
		{"four failures then a different tool do not", func(g *toolLoopGuard) bool {
			tripped := any4(g, tool, err)
			tripped = g.observe("read", false, "") || tripped
			return g.observe(tool, true, err) || tripped
		}, false},
		{"four failures then a different tool's FAILURE do not", func(g *toolLoopGuard) bool {
			tripped := any4(g, tool, err)
			tripped = g.observe("shell", true, err) || tripped
			return g.observe(tool, true, err) || tripped
		}, false},
		{"four failures then a success of the same tool do not", func(g *toolLoopGuard) bool {
			tripped := any4(g, tool, err)
			tripped = g.observe(tool, false, "") || tripped
			return g.observe(tool, true, err) || tripped
		}, false},
		{"four failures then a different error do not", func(g *toolLoopGuard) bool {
			tripped := any4(g, tool, err)
			return g.observe(tool, true, err+" ") || tripped
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newToolLoopGuard(5)
			if got := c.calls(g); got != c.trip {
				t.Errorf("tripped = %v, want %v (cause %q)", got, c.trip, g.tripped())
			}
		})
	}
}

func any4(g *toolLoopGuard, tool, err string) bool {
	tripped := false
	for i := 0; i < 4; i++ {
		tripped = g.observe(tool, true, err) || tripped
	}
	return tripped
}

// A re-delivered terminal event for a call already seen is not a second
// failure: replays are keyed on the part id, for every tool now, not only
// clankerbar's.
func TestToolLoopIgnoresReplayedOpencodeEvents(t *testing.T) {
	p := newOpencodeParse(Invocation{MaxRepeatedToolFailures: 2}, io.Discard)
	kills := 0
	p.kill = func() { kills++ }
	ev := `{"type":"tool_use","part":{"partID":"prt_1","type":"tool","id":"call_0","tool":"shell","state":{"status":"error","input":{},"error":"Permission denied: external_directory"}}}`
	p.line([]byte(ev))
	p.line([]byte(ev))
	if kills != 0 {
		t.Fatal("a replayed event counted as a second failure and killed the session")
	}
	p.line([]byte(strings.Replace(ev, "prt_1", "prt_2", 1)))
	if kills != 1 {
		t.Fatalf("kills = %d after two distinct failing calls at threshold 2, want 1", kills)
	}
}

// The end to end seam under opencode: a real executable streams the incident
// fixture and then sleeps. The guard must kill it - not wait out the sleep -
// and Invoke must return an orderly Result carrying the cause, not an error.
func TestOpencodeInvokeKillsASessionRepeatingAFailingToolCall(t *testing.T) {
	fixture, err := filepath.Abs(toolLoopFixture)
	if err != nil {
		t.Fatal(err)
	}
	opencodeStub(t, fmt.Sprintf("#!/bin/sh\ncat '%s'\nexec sleep 30\n", fixture))

	start := time.Now()
	res, ierr := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:                  "work",
		MaxRepeatedToolFailures: 5,
		Console:                 io.Discard,
	})
	if ierr != nil {
		t.Fatalf("Invoke returned an error for our own kill: %v - it must read as a classified end, not a launch failure", ierr)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("Invoke took %s - the guard did not end the session; it ran to the stub's own sleep", elapsed)
	}
	if !strings.HasPrefix(res.ToolLoop, ToolLoopCausePrefix+": clankerbar_submit_for_review x5: invalid_release") {
		t.Errorf("ToolLoop = %q, want the cause naming the tool, the count and the error", res.ToolLoop)
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0 - the child was not killed")
	}
	if res.Untrusted != "" {
		t.Errorf("Untrusted = %q: our own kill is not an unreadable stream", res.Untrusted)
	}
	if !res.UsageReported || res.CostUSD == 0 {
		t.Errorf("UsageReported=%v CostUSD=%v - the spend summed before the kill must still reach the budget", res.UsageReported, res.CostUSD)
	}
}

// The same stream with the check off runs to the stub's own end.
func TestOpencodeInvokeLeavesTheLoopAloneWhenTheCheckIsOff(t *testing.T) {
	fixture, err := filepath.Abs(toolLoopFixture)
	if err != nil {
		t.Fatal(err)
	}
	opencodeStub(t, fmt.Sprintf("#!/bin/sh\ncat '%s'\n", fixture))
	res, ierr := (opencode{}).Invoke(context.Background(), Invocation{Prompt: "work", Console: io.Discard})
	if ierr != nil {
		t.Fatalf("Invoke: %v", ierr)
	}
	if res.ToolLoop != "" || res.ExitCode != 0 {
		t.Errorf("ToolLoop=%q ExitCode=%d with the check off, want neither", res.ToolLoop, res.ExitCode)
	}
}

// claude reports a call's result on a later `user` event that carries only the
// tool_use id, so the guard is fed the name its `assistant` event recorded.
func claudeToolCall(id, name string, isError bool, text string) string {
	use := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":%q,"name":%q,"input":{"n":%q}}]}}`, id, name, id)
	result := fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":%q,"is_error":%v,"content":%q}]}}`, id, isError, text)
	return use + "\n" + result + "\n"
}

func TestClaudeConsumeKillsOnTheFifthIdenticalFailure(t *testing.T) {
	var stream strings.Builder
	for i := 0; i < 4; i++ {
		stream.WriteString(claudeToolCall(fmt.Sprintf("a%d", i), "mcp__clankerbar__submit_for_review", true, incidentError))
	}
	stream.WriteString(claudeToolCall("ok", "mcp__clankerbar__get_task", false, `{"task":{}}`))
	for i := 0; i < 5; i++ {
		stream.WriteString(claudeToolCall(fmt.Sprintf("b%d", i), "mcp__clankerbar__submit_for_review", true, incidentError))
	}
	// Anything after the verdict is not read: the session is being killed.
	stream.WriteString(`{"type":"result","result":"done","usage":{"input_tokens":1}}` + "\n")

	res := newSessionResult(Invocation{MaxRepeatedToolFailures: 5})
	kills := 0
	(claude{}).consume(strings.NewReader(stream.String()), io.Discard, newTail(), &res, 0, func() { kills++ })
	if kills != 1 {
		t.Fatalf("kill called %d times, want 1", kills)
	}
	if !strings.HasPrefix(res.ToolLoop, "repeated failing tool call: mcp__clankerbar__submit_for_review x5: invalid_release") {
		t.Errorf("ToolLoop = %q", res.ToolLoop)
	}
	if res.FinalMessage != "" {
		t.Error("the result event after the kill was parsed; the stream must end at the verdict")
	}
	if len(res.toolNames) != 0 {
		t.Errorf("toolNames still holds %d ids; settled calls must be forgotten", len(res.toolNames))
	}
}

func TestClaudeConsumeLeavesFourFailuresAndADifferentCallAlone(t *testing.T) {
	var stream strings.Builder
	for i := 0; i < 4; i++ {
		stream.WriteString(claudeToolCall(fmt.Sprintf("a%d", i), "Bash", true, "Permission denied"))
	}
	stream.WriteString(claudeToolCall("r", "Read", false, "file"))
	stream.WriteString(claudeToolCall("b", "Bash", true, "Permission denied"))
	res := newSessionResult(Invocation{MaxRepeatedToolFailures: 5})
	kills := 0
	(claude{}).consume(strings.NewReader(stream.String()), io.Discard, newTail(), &res, 0, func() { kills++ })
	if kills != 0 || res.ToolLoop != "" {
		t.Errorf("kills=%d ToolLoop=%q, want neither: the Read broke the streak", kills, res.ToolLoop)
	}
}

// The end to end seam under claude: the kill reaches the real process and the
// cause survives Invoke as an orderly end.
func TestClaudeInvokeKillsASessionRepeatingAFailingToolCall(t *testing.T) {
	dir := t.TempDir()
	var stream strings.Builder
	for i := 0; i < 5; i++ {
		stream.WriteString(claudeToolCall(fmt.Sprintf("a%d", i), "mcp__clankerbar__submit_for_review", true, incidentError))
	}
	streamPath := filepath.Join(dir, "stream.jsonl")
	if err := os.WriteFile(streamPath, []byte(stream.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\ncat '%s'\nexec sleep 30\n", streamPath)
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	start := time.Now()
	res, err := (claude{}).Invoke(context.Background(), Invocation{
		Prompt:                  "work",
		MaxRepeatedToolFailures: 5,
		Console:                 io.Discard,
	})
	if err != nil {
		t.Fatalf("Invoke returned an error for our own kill: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("Invoke took %s - the kill did not land", elapsed)
	}
	if res.ToolLoop == "" || res.ExitCode == 0 {
		t.Errorf("ToolLoop=%q ExitCode=%d, want the cause and a killed child", res.ToolLoop, res.ExitCode)
	}
}

func TestToolLoopCauseIsBoundedAndPrintable(t *testing.T) {
	cause := toolLoopCause("evil\x1b[2Jtool", 5, strings.Repeat("x", 5000)+"\x07")
	if strings.ContainsAny(cause, "\x1b\x07") {
		t.Errorf("cause carries control bytes: %q", cause)
	}
	if n := len([]rune(cause)); n > 400 {
		t.Errorf("cause is %d runes; a log line must be bounded", n)
	}
	if got := toolLoopCause("", 5, "plain error"); !strings.Contains(got, "(unnamed tool) x5: plain error") {
		t.Errorf("cause = %q", got)
	}
}
