package harness

// The ONE resume a silent exit 130 earns: opencode can exit 130 with no error
// event when its provider route is cut off, and only resuming the same session
// makes the missed error (a quota) reach the classifier.

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

const (
	interruptText  = `{"type":"text","sessionID":"ses_int","part":{"type":"text","text":"working before the interrupt"}}`
	interruptNoSID = `{"type":"text","part":{"type":"text","text":"working before the interrupt"}}`

	interruptCleanText = `{"type":"text","sessionID":"ses_int","part":{"type":"text","text":"continued after the interrupt"}}`
	interruptCleanStep = `{"type":"step_finish","sessionID":"ses_int","part":{"type":"step-finish","reason":"stop","tokens":{"total":90,"input":70,"output":20,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.002}}`

	// interruptQuota is the error the resume surfaces: the stream of the 130
	// itself never carried it, which is the whole reason the resume exists.
	interruptQuota = `{"type":"error","sessionID":"ses_int","error":{"type":"provider.quota","message":"Usage limit reached. Resets at 2026-10-23T00:00:00Z"}}`
)

// interruptClaim is the live claim the interrupted session held. A runner phase
// is seeded with it (Invocation.ResumeClaim), which is also the gate the resume
// reads before spending anything.
var interruptClaim = Claim{TaskID: "task-int", Ref: "CLA-130", RunID: "run-int"}

// interruptSession builds a 130 end the way the parse helper does for the other
// skip tables: a real stream through the real parser, with only the process
// outcome written by hand.
func interruptSession(stdout string) Result {
	res := opencodeParsedFrom(Invocation{ResumeClaim: interruptClaim}, io.Discard, stdout)
	res.ExitCode = opencodeInterruptExit
	return res
}

// A silent 130 resumes the same session exactly once, carrying the interrupt
// prompt, and the continuation's clean end is what Invoke returns.
func TestInterruptResume_ResumesOnceOnASilent130(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "log")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
case "$*" in
  *"exited 130"*)
    echo '`+interruptCleanText+`'
    echo '`+interruptCleanStep+`'
    ;;
  *)
    echo '`+interruptText+`'
    exit 130
    ;;
esac
`)

	console := &bytes.Buffer{}
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "Work.",
		Console:     console,
		ResumeClaim: interruptClaim,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	runs := stubCalls(t, logPath)
	if len(runs) != 2 {
		t.Fatalf("the stub ran %d times, want exactly 2 (the session + ONE interrupt resume):\n%s", len(runs), strings.Join(runs, "\n"))
	}
	if !strings.Contains(runs[1], "-s ses_int") {
		t.Errorf("the resume did not target the interrupted session: %s", runs[1])
	}
	if !strings.Contains(runs[1], "exited 130") {
		t.Errorf("the resume did not tell the session it was interrupted: %s", runs[1])
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want the clean continuation's 0", res.ExitCode)
	}
	// Charged exactly once: the interrupted run reported no usage, so the total
	// is the continuation's own step.
	if res.Tokens != 90 || res.CostUSD != 0.002 {
		t.Errorf("Tokens/CostUSD = %d/%v, want the continuation's 90/0.002 counted once", res.Tokens, res.CostUSD)
	}
	if out := console.String(); !strings.Contains(out, "interrupt resume done") {
		t.Errorf("console did not report the resume; got:\n%s", out)
	}
}

// A resume that surfaces the quota error the silent stream missed hands the
// route switch exactly the evidence it needs: the next call is the paid route,
// resuming the same session.
func TestInterruptResume_ResumeSurfacingALimitSwitchesRoute(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "log")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
case "$*" in
  *"--model `+routePaid+`"*)
    echo '`+interruptCleanText+`'
    echo '`+interruptCleanStep+`'
    ;;
  *"-s ses_int"*)
    echo '`+interruptQuota+`'
    exit 1
    ;;
  *)
    echo '`+interruptText+`'
    exit 130
    ;;
esac
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "Work.",
		Model:       routeFree,
		Routes:      []string{routeFree, routePaid},
		ResumeClaim: interruptClaim,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	calls := stubCalls(t, logPath)
	if len(calls) != 3 {
		t.Fatalf("the stub ran %d times, want 3 (the 130 + one resume + one route switch):\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if !strings.Contains(calls[2], "--model "+routePaid) {
		t.Errorf("the third call did not run on the paid route: %s", calls[2])
	}
	if !strings.Contains(calls[2], "-s ses_int") {
		t.Errorf("the route switch did not resume the same session: %s", calls[2])
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want the paid route's clean 0", res.ExitCode)
	}
}

// Every end below is one an interrupt resume must not talk over: nothing is
// spawned, and the Result keeps the end it arrived with.
func TestInterruptResume_Skips(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	unheld := interruptSession(interruptText)
	unheld.Claim = Claim{}
	nonZero := interruptSession(interruptText)
	nonZero.ExitCode = 1
	clean := interruptSession(interruptText)
	clean.ExitCode = 0
	signalled := interruptSession(interruptText)
	signalled.ExitSignal = 2
	untrusted := interruptSession(interruptText)
	untrusted.Untrusted = "line overran the capture"
	toolLoop := interruptSession(interruptText)
	toolLoop.ToolLoop = "repeated failing tool call"
	idleKilled := interruptSession(interruptText)
	idleKilled.IdleKilled = "no output for 5m"
	// DetectLimit reads the raw stream, which the parse helper does not keep.
	limited := interruptSession(interruptText + "\n" + interruptQuota)
	limited.Stdout = interruptText + "\n" + interruptQuota

	in := Invocation{Prompt: "Work.", ResumeClaim: interruptClaim}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		in   Invocation
		res  Result
		skip bool
	}{
		{
			name: "probe",
			ctx:  context.Background(),
			in:   Invocation{Prompt: "Work.", Probe: true, ResumeClaim: interruptClaim},
			res:  interruptSession(interruptText),
			skip: true,
		},
		{
			name: "cancelled run",
			ctx:  cancelled,
			in:   in,
			res:  interruptSession(interruptText),
			skip: true,
		},
		{
			name: "exit 1",
			ctx:  context.Background(),
			in:   in,
			res:  nonZero,
			skip: true,
		},
		{
			name: "exit 0",
			ctx:  context.Background(),
			in:   in,
			res:  clean,
			skip: true,
		},
		{
			name: "signalled 130",
			ctx:  context.Background(),
			in:   in,
			res:  signalled,
			skip: true,
		},
		{
			name: "untrusted stream",
			ctx:  context.Background(),
			in:   in,
			res:  untrusted,
			skip: true,
		},
		{
			name: "tool loop kill",
			ctx:  context.Background(),
			in:   in,
			res:  toolLoop,
			skip: true,
		},
		{
			name: "idle kill",
			ctx:  context.Background(),
			in:   in,
			res:  idleKilled,
			skip: true,
		},
		{
			name: "claim not held",
			ctx:  context.Background(),
			in:   in,
			res:  unheld,
			skip: true,
		},
		{
			// A limit on the stream is the route switch's evidence, not a silent end.
			name: "130 with a usage limit on the stream",
			ctx:  context.Background(),
			in:   in,
			res:  limited,
			skip: true,
		},
		{
			name: "no session id",
			ctx:  context.Background(),
			in:   in,
			res:  interruptSession(interruptNoSID),
			skip: true,
		},
		{
			name: "qualifying 130",
			ctx:  context.Background(),
			in:   in,
			res:  interruptSession(interruptText),
			skip: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.res
			if got := (opencode{}).interruptResumeSkipped(tc.ctx, tc.in, &res); got != tc.skip {
				t.Errorf("interruptResumeSkipped = %v, want %v", got, tc.skip)
			}
		})
	}
}

// A second silent 130 is not resumed again: one resume, never a loop.
func TestInterruptResume_SecondInterruptIsNotResumedAgain(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "log")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
echo '`+interruptText+`'
exit 130
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "Work.",
		Console:     &bytes.Buffer{},
		ResumeClaim: interruptClaim,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if runs := stubCalls(t, logPath); len(runs) != 2 {
		t.Errorf("the stub ran %d times, want exactly 2 — one resume, never a loop:\n%s", len(runs), strings.Join(runs, "\n"))
	}
	if res.ExitCode != opencodeInterruptExit {
		t.Errorf("ExitCode = %d, want the second interrupt's %d", res.ExitCode, opencodeInterruptExit)
	}
}
