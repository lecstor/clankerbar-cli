package harness

// CLA-867: planned multi-turn invocations in one opencode session, with an
// optional model per turn.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// turnStubTurn is one prompt-selected arm of the stub script.
type turnStubTurn struct {
	marker string // substring of the prompt that selects this arm
	body   string // shell lines that emit the turn's stream
}

// turnScript answers each turn by the marker in its prompt and logs every run's
// argv. opencodeStub adds the export guard, so the stream's figures stand.
func turnScript(logPath string, turns ...turnStubTurn) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&b, "echo \"$*\" >> '%s'\n", logPath)
	b.WriteString("case \"$*\" in\n")
	for _, turn := range turns {
		// The marker is quoted (it may contain spaces) but the surrounding
		// asterisks are not: a fully-quoted pattern makes `*` literal and the
		// arm never matches.
		fmt.Fprintf(&b, "  *\"%s\"*)\n%s\n    ;;\n", turn.marker, turn.body)
	}
	fmt.Fprintf(&b, "  *)\n    echo '{\"type\":\"text\",\"sessionID\":\"ses_live\",\"part\":{\"type\":\"text\",\"text\":\"unexpected prompt\"}}'\n    ;;\n")
	b.WriteString("esac\n")
	return b.String()
}

// turnAnswer is a clean turn: a text part and a step_finish carrying usage.
func turnAnswer(text string, tokens int, cost string) string {
	return fmt.Sprintf(
		`    echo '{"type":"text","sessionID":"ses_live","part":{"type":"text","text":%q}}'
    echo '{"type":"step_finish","sessionID":"ses_live","part":{"type":"step-finish","reason":"stop","tokens":{"total":%d,"input":%d,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":%s}}'`,
		text, tokens, tokens, cost)
}

// turnLimit is a turn that hit a provider usage limit: a typed quota event and
// a non-zero exit, with no answer.
const turnLimit = `    echo '{"type":"error","sessionID":"ses_live","error":{"type":"provider.quota","message":"Usage limit reached. Resets at 2026-11-15T00:00:00Z"}}'
    exit 1`

// turnStall is the CLA-584 output-cap stall: a FINAL step that hit "length"
// with zero output tokens (and real reasoning spend, so it is not the CLA-398
// quiet death).
const turnStall = `    echo '{"type":"text","sessionID":"ses_live","part":{"type":"text","text":"thinking out loud"}}'
    echo '{"type":"step_finish","sessionID":"ses_live","part":{"type":"step-finish","reason":"length","tokens":{"total":306592,"input":5280,"output":0,"reasoning":32000,"cache":{"write":0,"read":269312}},"cost":0.020799936}}'`

// turnClaim is the claim a runner phase is seeded with: without it every turn
// guard's claim check would (correctly) stop at turn 1.
var turnClaim = Claim{TaskID: "uuid-1", Ref: "CLA-867", RunID: "run-1"}

// Three turns run in order in ONE session id, each on the right model: the
// first on the invocation's, the second on its own, the third back on the
// invocation's (a per-turn model does not leak into a later turn that names
// none). Usage sums and FinalMessage is the last turn's.
func TestOpencodeInvokeRunsTurnsInOneSession(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, turnScript(logPath,
		turnStubTurn{"turn-one", turnAnswer("one", 10, "0.001")},
		turnStubTurn{"turn-two", turnAnswer("two", 20, "0.002")},
		turnStubTurn{"turn-three", turnAnswer("three", 30, "0.003")},
	))

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "turn-one",
		Model:       routeFree,
		ResumeClaim: turnClaim,
		Turns: []Turn{
			{Prompt: "turn-two", Model: routePaid},
			{Prompt: "turn-three"},
		},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	calls := stubCalls(t, logPath)
	if len(calls) != 3 {
		t.Fatalf("the stub ran %d times, want 3 (first turn + two follow-ups):\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if strings.Contains(calls[0], " -s ") {
		t.Errorf("the first turn was a resume: %s", calls[0])
	}
	for i, want := range []struct{ prompt, model string }{
		{"turn-one", routeFree},
		{"turn-two", routePaid},
		{"turn-three", routeFree},
	} {
		if !strings.Contains(calls[i], want.prompt) {
			t.Errorf("turn %d did not send its prompt %q: %s", i+1, want.prompt, calls[i])
		}
		if !strings.Contains(calls[i], "--model "+want.model) {
			t.Errorf("turn %d did not run on %s: %s", i+1, want.model, calls[i])
		}
		if i > 0 && !strings.Contains(calls[i], "-s ses_live") {
			t.Errorf("turn %d did not continue the first turn's session: %s", i+1, calls[i])
		}
	}

	if want := []string{"one", "two", "three"}; !reflect.DeepEqual(res.TurnMessages, want) {
		t.Errorf("TurnMessages = %q, want %q", res.TurnMessages, want)
	}
	if res.FinalMessage != "three" {
		t.Errorf("FinalMessage = %q, want the last turn's %q", res.FinalMessage, "three")
	}
	if want := 10 + 20 + 30; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d summed across turns", res.Tokens, want)
	}
	if res.CostUSD < 0.0059 || res.CostUSD > 0.0061 {
		t.Errorf("CostUSD = %v, want 0.006 summed across turns", res.CostUSD)
	}
	if res.TurnStopped != 0 || res.TurnStopReason != "" {
		t.Errorf("TurnStopped = %d (%q), want 0: every turn ran", res.TurnStopped, res.TurnStopReason)
	}
}

// A usage limit on turn 2 stops the sequence before turn 3, and the Result says
// which turn ended it. The invocation has no other route, so there is nothing
// to switch to; the limit stands for the driver, exactly as a single-turn
// session's would.
func TestOpencodeInvokeStopsTurnsOnALimit(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, turnScript(logPath,
		turnStubTurn{"turn-one", turnAnswer("one", 10, "0.001")},
		turnStubTurn{"turn-two", turnLimit},
		turnStubTurn{"turn-three", turnAnswer("three", 30, "0.003")},
	))

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "turn-one",
		Model:       routeFree,
		ResumeClaim: turnClaim,
		Turns:       []Turn{{Prompt: "turn-two"}, {Prompt: "turn-three"}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	calls := stubCalls(t, logPath)
	if len(calls) != 2 {
		t.Fatalf("the stub ran %d times, want 2 — turn 3 must not run after turn 2's limit:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if res.TurnStopped != 2 || res.TurnStopReason != "usage_limit" {
		t.Errorf("TurnStopped = %d (%q), want turn 2 stopped on usage_limit", res.TurnStopped, res.TurnStopReason)
	}
	if !(opencode{}).DetectLimit(res).Limited {
		t.Error("the Result no longer reads as limited, so the driver cannot act on it")
	}
	if want := []string{"one", ""}; !reflect.DeepEqual(res.TurnMessages, want) {
		t.Errorf("TurnMessages = %q, want %q (turn 2 produced no answer)", res.TurnMessages, want)
	}
}

// An output-cap stall on turn 2 is the other bad end: the sequence stops, the
// turn is named, and the stall is NOT resumed behind the caller's back (a later
// turn would then run on a session whose last step failed).
func TestOpencodeInvokeStopsTurnsOnAStall(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, turnScript(logPath,
		turnStubTurn{"turn-one", turnAnswer("one", 10, "0.001")},
		turnStubTurn{"turn-two", turnStall},
		turnStubTurn{"turn-three", turnAnswer("three", 30, "0.003")},
	))

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "turn-one",
		Model:       routeFree,
		ResumeClaim: turnClaim,
		Turns:       []Turn{{Prompt: "turn-two"}, {Prompt: "turn-three"}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	calls := stubCalls(t, logPath)
	if len(calls) != 2 {
		t.Fatalf("the stub ran %d times, want 2 — a turn-2 stall must stop before turn 3 and must not be auto-resumed:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if res.TurnStopped != 2 || res.TurnStopReason != "output_cap_stall" {
		t.Errorf("TurnStopped = %d (%q), want turn 2 stopped on output_cap_stall", res.TurnStopped, res.TurnStopReason)
	}
	if !(opencode{}).OutputCapNoOutput(res) {
		t.Error("the stall marker did not survive the merge, so the driver cannot name the end")
	}
}

// A usage limit on a turn that NAMES its model ends the sequence; it is never
// silently swapped onto the invocation's fallback route. The invocation has a
// route list precisely so a swap would be visible.
func TestOpencodeInvokeDoesNotSwapAPerTurnModel(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, turnScript(logPath,
		turnStubTurn{"turn-one", turnAnswer("one", 10, "0.001")},
		turnStubTurn{"turn-two", turnLimit},
		turnStubTurn{"route hit a usage limit", turnAnswer("switched", 40, "0.004")},
	))

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "turn-one",
		Model:       routeFree,
		Routes:      []string{routeFree, routePaid},
		ResumeClaim: turnClaim,
		Turns:       []Turn{{Prompt: "turn-two", Model: "other/model"}, {Prompt: "turn-three"}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	calls := stubCalls(t, logPath)
	if len(calls) != 2 {
		t.Fatalf("the stub ran %d times, want 2 — no route switch may follow a per-turn model:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	for _, call := range calls {
		if strings.Contains(call, routePaid) {
			t.Errorf("the per-turn model was silently swapped onto the invocation's route: %s", call)
		}
	}
	if res.TurnStopped != 2 || res.TurnStopReason != "usage_limit" {
		t.Errorf("TurnStopped = %d (%q), want turn 2 stopped on usage_limit", res.TurnStopped, res.TurnStopReason)
	}
}

// A limit on a turn running the INVOCATION's route list still follows the
// existing fallback rules: the same session resumes on the next route, and the
// sequence then continues on that route. This is the stage's configured route,
// not a per-turn model, so the swap is the one the caller asked for.
func TestOpencodeInvokeSwitchesRouteBetweenTurns(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, turnScript(logPath,
		turnStubTurn{"turn-one", turnLimit},
		turnStubTurn{"previous model route hit a usage limit", turnAnswer("switched", 40, "0.004")},
		turnStubTurn{"turn-two", turnAnswer("two", 20, "0.002")},
	))

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "turn-one",
		Model:       routeFree,
		Routes:      []string{routeFree, routePaid},
		ResumeClaim: turnClaim,
		Turns:       []Turn{{Prompt: "turn-two"}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	calls := stubCalls(t, logPath)
	if len(calls) != 3 {
		t.Fatalf("the stub ran %d times, want 3 (turn 1, its route switch, turn 2); stopped=%d reason=%q:\n%s",
			len(calls), res.TurnStopped, res.TurnStopReason, strings.Join(calls, "\n"))
	}
	if !strings.Contains(calls[1], "--model "+routePaid) || !strings.Contains(calls[1], "-s ses_live") {
		t.Errorf("turn 1's limit did not switch the session to the paid route: %s", calls[1])
	}
	if !strings.Contains(calls[2], "--model "+routePaid) || !strings.Contains(calls[2], "turn-two") {
		t.Errorf("turn 2 did not continue on the route that finished turn 1: %s", calls[2])
	}
	if want := []string{"switched", "two"}; !reflect.DeepEqual(res.TurnMessages, want) {
		t.Errorf("TurnMessages = %q, want %q (turn 1's message is the switched continuation's)", res.TurnMessages, want)
	}
	if res.TurnStopped != 0 || res.TurnStopReason != "" {
		t.Errorf("TurnStopped = %d (%q), want 0 after a successful route switch", res.TurnStopped, res.TurnStopReason)
	}
}

// The done-report check runs on the FINAL turn only. A notes turn's answer must
// never be judged as a done report — the check here fails anything that is not
// the last turn's text, and the stub would serve an extra resume if it fired.
func TestOpencodeCheckReportOnlyOnTheFinalTurn(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, turnScript(logPath,
		turnStubTurn{"turn-one", turnAnswer("notes for the reviewer", 10, "0.001")},
		turnStubTurn{"turn-two", turnAnswer("FINAL REPORT", 20, "0.002")},
	))

	var checked []string
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "turn-one",
		Model:       routeFree,
		ResumeClaim: turnClaim,
		Turns:       []Turn{{Prompt: "turn-two"}},
		CheckReport: func(final string) error {
			checked = append(checked, final)
			if final != "FINAL REPORT" {
				return fmt.Errorf("not the final turn's report: %q", final)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if want := []string{"FINAL REPORT"}; !reflect.DeepEqual(checked, want) {
		t.Errorf("CheckReport saw %q, want only the final turn's message %q", checked, want)
	}
	if calls := stubCalls(t, logPath); len(calls) != 2 {
		t.Errorf("the stub ran %d times, want 2 — a notes turn was treated as a bad done report:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if res.FinalMessage != "FINAL REPORT" {
		t.Errorf("FinalMessage = %q, want the final turn's", res.FinalMessage)
	}
	if want := []string{"notes for the reviewer", "FINAL REPORT"}; !reflect.DeepEqual(res.TurnMessages, want) {
		t.Errorf("TurnMessages = %q, want %q", res.TurnMessages, want)
	}
}

// A claim the session no longer holds stops the sequence: the next turn would
// be work done under no lease, which the plane cannot account for.
func TestOpencodeInvokeStopsTurnsWhenTheClaimIsGone(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, turnScript(logPath,
		turnStubTurn{"turn-one", turnAnswer("one", 10, "0.001")},
		turnStubTurn{"turn-two", turnAnswer("two", 20, "0.002")},
	))

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt: "turn-one",
		Model:  routeFree,
		Turns:  []Turn{{Prompt: "turn-two"}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if calls := stubCalls(t, logPath); len(calls) != 1 {
		t.Fatalf("the stub ran %d times, want 1 — an unclaimed session must not take a later turn:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if res.TurnStopped != 1 || res.TurnStopReason != "claim_not_held" {
		t.Errorf("TurnStopped = %d (%q), want turn 1 stopped on claim_not_held", res.TurnStopped, res.TurnStopReason)
	}
	if want := []string{"one"}; !reflect.DeepEqual(res.TurnMessages, want) {
		t.Errorf("TurnMessages = %q, want %q", res.TurnMessages, want)
	}
}

// A turn whose model produces no answer records an empty message rather than
// inheriting the previous turn's text (mergeResume keeps the base when a
// continuation emits none, which is why runTurns blanks it first).
func TestOpencodeTurnMessagesDoNotRepeatThePreviousMessage(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, turnScript(logPath,
		turnStubTurn{"turn-one", turnAnswer("one", 10, "0.001")},
		turnStubTurn{"turn-two", `    echo '{"type":"step_finish","sessionID":"ses_live","part":{"type":"step-finish","reason":"stop","tokens":{"total":5,"input":5,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.0005}}'`},
	))

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "turn-one",
		Model:       routeFree,
		ResumeClaim: turnClaim,
		Turns:       []Turn{{Prompt: "turn-two"}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if want := []string{"one", ""}; !reflect.DeepEqual(res.TurnMessages, want) {
		t.Errorf("TurnMessages = %q, want %q — a silent last turn must not repeat turn 1's text", res.TurnMessages, want)
	}
	if res.FinalMessage != "" {
		t.Errorf("FinalMessage = %q, want empty (the last turn said nothing)", res.FinalMessage)
	}
}

// The turn's parser is seeded with the claim the PREVIOUS turn left, exactly
// as every other continuation is: a `run -s` streams only the new turn, so
// without the seed a branch record the NOTES turn makes is matched against the
// staler invocation seed and dropped. The first turn re-claims under a new id,
// which is the case the seed exists for.
func TestOpencodeTurnsSeedTheLiveClaim(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	claim := opencodeToolLine("c0", "clankerbar_claim_task", `{"taskId":"uuid-new"}`,
		"completed", `{"task":{"id":"uuid-new","ref":"CLA-2"},"run":{"id":"run-new"}}`)
	progress := opencodeToolLine("c1", "clankerbar_record_progress",
		`{"taskId":"uuid-new","branch":"clanker/turns-work"}`, "completed", `{}`)
	opencodeStub(t, turnScript(logPath,
		turnStubTurn{"turn-one", "    echo '" + claim + "'\n" + turnAnswer("one", 10, "0.001")},
		turnStubTurn{"turn-two", "    echo '" + progress + "'\n" + turnAnswer("two", 20, "0.002")},
	))

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "turn-one",
		Model:       routeFree,
		ResumeClaim: turnClaim, // uuid-1 / CLA-867
		Turns:       []Turn{{Prompt: "turn-two"}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Claim.TaskID != "uuid-new" || res.Claim.RunID != "run-new" {
		t.Fatalf("Claim = %+v, want the second task the first turn claimed", res.Claim)
	}
	if !res.Claim.HasWIP || res.Claim.Branch != "clanker/turns-work" {
		t.Errorf("Claim = %+v, want the branch the SECOND turn recorded - its parser was not seeded with the live claim", res.Claim)
	}
	if len(res.Reports) != 1 || res.Reports[0].Branch != "clanker/turns-work" || res.Reports[0].RunID != "run-new" {
		t.Errorf("Reports = %+v, want the second turn's branch record signed by the live run", res.Reports)
	}
}

// A single-turn invocation is untouched by the feature: no TurnMessages, no
// turn stop state, and the Result is the one it always was.
func TestOpencodeSingleTurnHasNoTurnRecord(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, turnScript(logPath,
		turnStubTurn{"turn-one", turnAnswer("one", 10, "0.001")},
	))

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "turn-one",
		Model:       routeFree,
		ResumeClaim: turnClaim,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if calls := stubCalls(t, logPath); len(calls) != 1 {
		t.Errorf("the stub ran %d times, want 1", len(calls))
	}
	if res.TurnMessages != nil || res.TurnStopped != 0 || res.TurnStopReason != "" {
		t.Errorf("single turn recorded turn state: messages=%q stopped=%d reason=%q",
			res.TurnMessages, res.TurnStopped, res.TurnStopReason)
	}
	if res.FinalMessage != "one" {
		t.Errorf("FinalMessage = %q, want one", res.FinalMessage)
	}
}

// An adapter that cannot continue a live session REFUSES a turn sequence up
// front, with an error naming the harness and the field. Silently dropping the
// later turns would hand the caller a half-finished answer shaped like a
// complete one.
func TestClaudeAndCodexRefuseTurns(t *testing.T) {
	for _, c := range []struct {
		name string
		ad   Adapter
	}{
		{"claude", claude{}},
		{"codex", codex{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// PATH is emptied so a refusal that failed to fire would fail this
			// test on the spawn rather than quietly running one turn.
			t.Setenv("PATH", t.TempDir())
			res, err := c.ad.Invoke(context.Background(), Invocation{
				Prompt: "turn-one",
				Turns:  []Turn{{Prompt: "turn-two"}},
			})
			if !errors.Is(err, ErrTurnsUnsupported) {
				t.Fatalf("err = %v, want ErrTurnsUnsupported", err)
			}
			if !strings.Contains(err.Error(), c.name) {
				t.Errorf("error %q does not name the harness that refused", err)
			}
			if res.TurnMessages != nil || res.FinalMessage != "" || res.ExitCode != 0 {
				t.Errorf("a refused invocation returned a non-empty Result: %+v", res)
			}
		})
	}
}

// A probe is not a planned sequence: it returns its liveness verdict before the
// turns are considered. Pinned so a future caller cannot accidentally turn a
// probe into a paid N-turn session.
func TestOpencodeProbeIgnoresTurns(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, turnScript(logPath,
		turnStubTurn{"turn-one", turnAnswer("one", 10, "0.001")},
		turnStubTurn{"turn-two", turnAnswer("two", 20, "0.002")},
	))

	_, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "turn-one",
		Model:       routeFree,
		Probe:       true,
		ResumeClaim: turnClaim,
		Turns:       []Turn{{Prompt: "turn-two"}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	calls := stubCalls(t, logPath)
	if len(calls) != 1 {
		t.Fatalf("a probe ran %d times, want 1 — a probe is a liveness check, not a sequence:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if strings.Contains(calls[0], "turn-one") {
		t.Errorf("a probe sent the invocation prompt instead of its one-word check: %s", calls[0])
	}
}

// The wall-clock budget is shared across the whole sequence: once less than the
// continuation floor is left, the next turn is not started (it could only be
// cap-killed on arrival) and the sequence says which turn it withheld.
func TestOpencodeRunTurnsStopsWhenTheWallClockIsSpent(t *testing.T) {
	res := opencodeParsed(opencodeStream)
	// The claim a runner phase is seeded with; the turn sequence refuses to
	// continue without one, which would mask the wall-clock gate.
	res.Claim = turnClaim
	in := Invocation{
		Prompt:      "turn-one",
		Model:       routeFree,
		ResumeClaim: turnClaim,
		Turns:       []Turn{{Prompt: "turn-two"}},
	}
	console := &strings.Builder{}
	in.Console = console
	_, completed, err := (opencode{}).runTurns(context.Background(), in, &res, time.Now())
	if err != nil {
		t.Fatalf("runTurns: %v", err)
	}
	if completed {
		t.Error("the sequence completed with no wall-clock budget left")
	}
	if res.TurnStopped != 2 || res.TurnStopReason != "wall_clock" {
		t.Errorf("TurnStopped = %d (%q), want turn 2 stopped on wall_clock", res.TurnStopped, res.TurnStopReason)
	}
	if !strings.Contains(console.String(), "wall-clock budget exhausted") {
		t.Errorf("console does not name the wall-clock stop:\n%s", console.String())
	}
	if want := []string{res.TurnMessages[0]}; !reflect.DeepEqual(res.TurnMessages, want) || len(res.TurnMessages) != 1 {
		t.Errorf("TurnMessages = %q, want only the first turn's message", res.TurnMessages)
	}
}

// The same rule on the FINAL turn: its limit reaches the caller's recovery
// passes with the per-turn invocation, which has no route list, so the existing
// switchRoutesOnLimit cannot swap the stage onto the invocation's route.
func TestOpencodeFinalPerTurnModelLimitIsNotSwapped(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, turnScript(logPath,
		turnStubTurn{"turn-one", turnAnswer("one", 10, "0.001")},
		turnStubTurn{"turn-two", turnLimit},
		turnStubTurn{"previous model route hit a usage limit", turnAnswer("switched", 40, "0.004")},
	))

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "turn-one",
		Model:       routeFree,
		Routes:      []string{routeFree, routePaid},
		ResumeClaim: turnClaim,
		Turns:       []Turn{{Prompt: "turn-two", Model: "other/model"}},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	calls := stubCalls(t, logPath)
	if len(calls) != 2 {
		t.Fatalf("the stub ran %d times, want 2 — the final turn's limit must not cause a route switch:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	for _, call := range calls {
		if strings.Contains(call, routePaid) {
			t.Errorf("the final per-turn model was silently swapped onto the invocation's route: %s", call)
		}
	}
	if !(opencode{}).DetectLimit(res).Limited {
		t.Error("the final turn's limit did not survive to the Result")
	}
	if res.TurnStopped != 0 {
		t.Errorf("TurnStopped = %d (%q), want 0: every planned turn ran", res.TurnStopped, res.TurnStopReason)
	}
}

// A refused done report is re-emitted by the ONE report resume, on the FINAL
// turn: the per-turn record is updated to the corrected report, and no notes
// turn is ever re-steered.
func TestOpencodeReportResumeAppliesToTheFinalTurn(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	opencodeStub(t, turnScript(logPath,
		turnStubTurn{"turn-one", turnAnswer("notes", 10, "0.001")},
		turnStubTurn{"turn-two", turnAnswer("BAD REPORT", 20, "0.002")},
		turnStubTurn{"done report was not accepted", turnAnswer("GOOD REPORT", 5, "0.0005")},
	))

	var checked []string
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "turn-one",
		Model:       routeFree,
		ResumeClaim: turnClaim,
		Turns:       []Turn{{Prompt: "turn-two"}},
		CheckReport: func(final string) error {
			checked = append(checked, final)
			if final != "GOOD REPORT" {
				return fmt.Errorf("the report must be the run's JSON block, got %q", final)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if want := []string{"BAD REPORT", "GOOD REPORT"}; !reflect.DeepEqual(checked, want) {
		t.Errorf("CheckReport saw %q, want %q", checked, want)
	}
	calls := stubCalls(t, logPath)
	if len(calls) != 3 {
		t.Fatalf("the stub ran %d times, want 3 (two turns + one report resume):\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if !strings.Contains(calls[2], "done report was not accepted") {
		t.Errorf("the extra run is not the report steer: %s", calls[2])
	}
	if res.FinalMessage != "GOOD REPORT" {
		t.Errorf("FinalMessage = %q, want the corrected report", res.FinalMessage)
	}
	if want := []string{"notes", "GOOD REPORT"}; !reflect.DeepEqual(res.TurnMessages, want) {
		t.Errorf("TurnMessages = %q, want %q (the final entry follows the corrected report)", res.TurnMessages, want)
	}
}

// The shared guard names a wall-clock kill as the cap it is, not as the exit
// status the kill leaves behind: both stop a sequence, but only one of them
// tells the operator the budget ran out.
func TestResumeSkippedReasonNamesAWallClockKill(t *testing.T) {
	res := opencodeParsed(opencodeStream)
	res.Claim = turnClaim
	res.ExitCode = -1
	res.Raw[TerminalReasonKey] = wallClockReason
	got := (opencode{}).resumeSkippedReason(context.Background(), Invocation{}, &res)
	if got != "wall_clock" {
		t.Errorf("resumeSkippedReason = %q, want wall_clock", got)
	}
}

// Invocation.OnTurn is the model/route rule in one place: a named model wins
// and drops the route list, a blank one leaves both alone.
func TestOnTurn(t *testing.T) {
	in := Invocation{Model: routeFree, Routes: []string{routeFree, routePaid}}
	got := in.OnTurn(Turn{Prompt: "x", Model: " "})
	if got.Model != routeFree || len(got.Routes) != 2 {
		t.Errorf("a blank turn model changed the invocation: model=%q routes=%v", got.Model, got.Routes)
	}
	got = in.OnTurn(Turn{Prompt: "x", Model: routePaid})
	if got.Model != routePaid {
		t.Errorf("Model = %q, want %q", got.Model, routePaid)
	}
	if len(got.Routes) != 0 {
		t.Errorf("Routes = %v, want none — a per-turn model must not inherit a fallback list", got.Routes)
	}
	if in.Model != routeFree || len(in.Routes) != 2 {
		t.Errorf("OnTurn mutated the receiver: model=%q routes=%v", in.Model, in.Routes)
	}
}

// OnFinishedRoute drops the routes already abandoned, so a LATER turn's limit
// cannot walk back onto one of them.
func TestOnFinishedRoute(t *testing.T) {
	in := Invocation{Model: routeFree, Routes: []string{routeFree, routePaid, "third/model"}}
	got := in.OnFinishedRoute(routePaid)
	if want := []string{routePaid, "third/model"}; !reflect.DeepEqual(got.Routes, want) {
		t.Errorf("Routes = %v, want %v", got.Routes, want)
	}
	if got.Model != routePaid {
		t.Errorf("Model = %q, want %q", got.Model, routePaid)
	}
	if got := in.OnFinishedRoute(""); got.Model != routeFree || len(got.Routes) != 3 {
		t.Errorf("an empty route changed the invocation: model=%q routes=%v", got.Model, got.Routes)
	}
}
