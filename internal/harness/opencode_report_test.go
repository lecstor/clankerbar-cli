package harness

// The runner's done-report check, and the ONE resume it earns when the report
// is refused. The fixtures are the two shapes the check sees: a report it
// rejects and a corrected one it accepts.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reportBadText is a final message whose report the test's check refuses, and
// reportGoodText the corrected one the same check accepts.
const (
	reportBadText  = `{"type":"text","sessionID":"ses_report","part":{"type":"text","text":"=====CLANKERBAR DONE===== summary too long"}}`
	reportGoodText = `{"type":"text","sessionID":"ses_report","part":{"type":"text","text":"=====CLANKERBAR DONE===== summary short"}}`
	reportNoSID    = `{"type":"text","part":{"type":"text","text":"=====CLANKERBAR DONE===== summary too long"}}`
)

// The two steps' usage differs so the merge's sum is visible on the Result.
const (
	reportBadStep  = `{"type":"step_finish","sessionID":"ses_report","part":{"type":"step-finish","reason":"stop","tokens":{"total":500,"input":100,"output":400,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.01}}`
	reportGoodStep = `{"type":"step_finish","sessionID":"ses_report","part":{"type":"step-finish","reason":"stop","tokens":{"total":900,"input":800,"output":100,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.001}}`
)

const reportBadStream = reportBadText + "\n" + reportBadStep

// reportLimitEvent is a terminal provider-limit error: an end of the session's
// own that a report resume must not talk over. It comes LAST on the stream,
// because only an error the stream stopped on is a diagnostic (CLA-700).
const reportLimitEvent = `{"type":"error","sessionID":"ses_report","error":{"data":{"message":"Payment Required","statusCode":402,"isRetryable":false}}}`

// reportClaim is the claim a runner phase is seeded with. The report resume
// asks res.Claim.Held() before it spends anything, so every fixture that is
// meant to reach a resume starts from a held claim.
var reportClaim = Claim{TaskID: "task-report", Ref: "CLA-1", RunID: "run-report"}

// reportSession is a session that holds its claim, named itself, and emitted a
// report the check refuses: the shape each skip arm below starts from and then
// breaks in exactly one way.
func reportSession(stdout string) Result {
	return opencodeParsedFrom(Invocation{ResumeClaim: reportClaim}, io.Discard, stdout)
}

var errReportTooLong = errors.New("summary is 412 chars; the limit is 300")

// reportRefusesLong is the stand-in runner check: the summary wording is the
// only thing it judges, so a corrected report passes it.
func reportRefusesLong(final string) error {
	if strings.Contains(final, "too long") {
		return errReportTooLong
	}
	return nil
}

// stubRuns reads the argv lines an opencodeStub recorded, one per process.
func stubRuns(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("stub log: %v", err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// reportStub serves the bad report to a first run and the good one to a resume,
// distinguished by the steer the resume carries.
func reportStub(t *testing.T) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "log")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
case "$*" in
  *"not accepted by the runner"*)
    echo '`+reportGoodText+`'
    echo '`+reportGoodStep+`'
    ;;
  *)
    echo '`+reportBadText+`'
    echo '`+reportBadStep+`'
    ;;
esac
`)
	return logPath
}

// A phase that owes no report is not checked and not resumed.
func TestReportResume_NoCheckNoResume(t *testing.T) {
	logPath := reportStub(t)

	if _, err := (opencode{}).Invoke(context.Background(), Invocation{Prompt: "Work.", Console: &bytes.Buffer{}}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if runs := stubRuns(t, logPath); len(runs) != 1 {
		t.Errorf("stub ran %d times, want 1 — no CheckReport, no resume:\n%v", len(runs), runs)
	}
}

// A report the check accepts earns no resume.
func TestReportResume_PassingReportIsNotResumed(t *testing.T) {
	logPath := reportStub(t)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "Work.",
		Console:     &bytes.Buffer{},
		ResumeClaim: reportClaim,
		CheckReport: func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if runs := stubRuns(t, logPath); len(runs) != 1 {
		t.Errorf("stub ran %d times, want 1 — a passing report is not resumed:\n%v", len(runs), runs)
	}
	if !strings.Contains(res.FinalMessage, "too long") {
		t.Errorf("FinalMessage = %q, want the session's own report untouched", res.FinalMessage)
	}
}

// A refused report resumes the SAME session exactly once, carrying the check's
// error; the corrected report is what the merged Result hands the runner.
func TestReportResume_RefusedReportResumesOnceWithTheError(t *testing.T) {
	logPath := reportStub(t)
	console := &bytes.Buffer{}

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "Work.",
		Console:     console,
		ResumeClaim: reportClaim,
		CheckReport: reportRefusesLong,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	runs := stubRuns(t, logPath)
	if len(runs) != 2 {
		t.Fatalf("stub ran %d times, want exactly 2 (the session + ONE report resume):\n%v", len(runs), runs)
	}
	if !strings.Contains(runs[1], "-s ses_report") {
		t.Errorf("the resume did not target the existing session: %q", runs[1])
	}
	if !strings.Contains(runs[1], errReportTooLong.Error()) {
		t.Errorf("the resume does not carry the check's error: %q", runs[1])
	}
	if !strings.Contains(runs[1], "=====CLANKERBAR DONE=====") {
		t.Errorf("the steer does not name the report marker: %q", runs[1])
	}
	if !strings.Contains(res.FinalMessage, "short") {
		t.Errorf("FinalMessage = %q, want the continuation's corrected report", res.FinalMessage)
	}
	if want := 500 + 900; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d summed across the session and its resume", res.Tokens, want)
	}
	out := console.String()
	if !strings.Contains(out, "done report refused") {
		t.Errorf("console did not announce the refusal; got:\n%s", out)
	}
	if !strings.Contains(out, "corrected report passes the check") {
		t.Errorf("console did not report the corrected check; got:\n%s", out)
	}
}

// A second refusal is the runner's to fail closed on: one resume, never a loop.
func TestReportResume_SecondRefusalIsNotResumedAgain(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "log")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
echo '`+reportBadText+`'
echo '`+reportBadStep+`'
`)
	console := &bytes.Buffer{}

	if _, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "Work.",
		Console:     console,
		ResumeClaim: reportClaim,
		CheckReport: reportRefusesLong,
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if runs := stubRuns(t, logPath); len(runs) != 2 {
		t.Errorf("stub ran %d times, want exactly 2 — one resume, never a loop:\n%v", len(runs), runs)
	}
	out := console.String()
	if !strings.Contains(out, "still fails the check") {
		t.Errorf("console did not say the resumed report still fails; got:\n%s", out)
	}
	if !strings.Contains(out, errReportTooLong.Error()) {
		t.Errorf("console did not name the failure the runner will close on; got:\n%s", out)
	}
}

// Each of these ends is one a report resume must not talk over, so nothing is
// spawned and the report is left exactly as the session emitted it. Every row
// starts from the same held-claim session and breaks ONE thing, so a row can
// only pass on the arm it names.
func TestReportResume_Skips(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	unheld := reportSession(reportBadStream)
	unheld.Claim = Claim{}
	// DetectLimit reads the captured STDOUT, which the parse helper does not
	// build, so the terminal error event is appended to it here.
	limit := reportSession(reportBadStream)
	limit.Stdout = reportBadStream + "\n" + reportLimitEvent
	stall := reportSession(reportBadText + "\n" + outputCapStallLine)
	quiet := reportSession(reportBadText + "\n" + quietDeathWithSession)
	nonZero := reportSession(reportBadStream)
	nonZero.ExitCode = 1

	in := Invocation{Prompt: "Work.", ResumeClaim: reportClaim, CheckReport: reportRefusesLong}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		in   Invocation
		res  Result
	}{
		{
			name: "untrusted stream",
			ctx:  context.Background(),
			in:   in,
			res:  untrustedReport(),
		},
		{
			name: "cancelled run",
			ctx:  cancelled,
			in:   in,
			res:  reportSession(reportBadStream),
		},
		{
			name: "probe",
			ctx:  context.Background(),
			in:   Invocation{Prompt: "Work.", Probe: true, ResumeClaim: reportClaim, CheckReport: reportRefusesLong},
			res:  reportSession(reportBadStream),
		},
		{
			name: "no session id",
			ctx:  context.Background(),
			in:   in,
			res:  reportSession(reportNoSID),
		},
		{
			name: "claim not held",
			ctx:  context.Background(),
			in:   in,
			res:  unheld,
		},
		{
			name: "usage limit end",
			ctx:  context.Background(),
			in:   in,
			res:  limit,
		},
		{
			name: "standing output-cap stall",
			ctx:  context.Background(),
			in:   in,
			res:  stall,
		},
		{
			name: "unrecovered quiet death",
			ctx:  context.Background(),
			in:   in,
			res:  quiet,
		},
		{
			name: "non-zero exit with nothing recovered",
			ctx:  context.Background(),
			in:   in,
			res:  nonZero,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "log")
			opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
echo '`+reportGoodText+`'
echo '`+reportGoodStep+`'
`)
			res := tc.res
			(opencode{}).resumeBadReport(tc.ctx, tc.in, &res, time.Time{})
			if runs := stubRuns(t, logPath); len(runs) != 0 {
				t.Errorf("stub ran %d times, want none — this end cannot take a resume:\n%v", len(runs), runs)
			}
			if res.FinalMessage != tc.res.FinalMessage {
				t.Errorf("FinalMessage = %q, want it left as it was (%q)", res.FinalMessage, tc.res.FinalMessage)
			}
		})
	}
}

func untrustedReport() Result {
	res := reportSession(reportBadStream)
	res.Untrusted = "line overran the capture"
	return res
}

// An errored resume is real spend that changes nothing: the dollars and tokens
// are charged and the report stays the one the runner refused.
func TestReportResume_ErroredResumeChargesSpendAndKeepsTheReport(t *testing.T) {
	opencodeStub(t, `#!/bin/sh
printf '%s\n%s\n' '`+reportGoodText+`' '`+reportGoodStep+`'
`)

	rec := &failWriter{}
	res := reportSession(reportBadStream)
	(opencode{}).resumeBadReport(context.Background(),
		Invocation{Prompt: "Work.", Console: rec, ResumeClaim: reportClaim, CheckReport: reportRefusesLong}, &res, time.Time{})

	if !strings.Contains(rec.notes.String(), "report resume errored") {
		t.Fatalf("the resume did not error as the fixture requires; console got:\n%s", rec.notes.String())
	}
	if want := 500 + 900; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d — the errored resume's spend is this session's spend", res.Tokens, want)
	}
	if !strings.Contains(res.FinalMessage, "too long") {
		t.Errorf("FinalMessage = %q, want the refused report unchanged", res.FinalMessage)
	}
}

// A corrected report the check accepts is still wasted when the resumed session
// ended in a way the driver never applies a report on: the console must not
// claim a pass the runner will not read.
func TestReportResume_UnusableEndSaysTheRunnerWillNotApplyIt(t *testing.T) {
	// The continuation is cap-killed after it emits the corrected report, which
	// is the one nil-error non-zero end opencode's own kill produces.
	opencodeStub(t, `#!/bin/sh
echo '`+reportGoodText+`'
echo '`+reportGoodStep+`'
exec sleep 30
`)
	console := &bytes.Buffer{}

	res := reportSession(reportBadStream)
	in := Invocation{Prompt: "Work.", Console: console, ResumeClaim: reportClaim, CheckReport: reportRefusesLong}
	in.MaxSessionWallClock = time.Second
	(opencode{}).resumeBadReport(context.Background(), in, &res, time.Time{})

	out := console.String()
	if !strings.Contains(res.FinalMessage, "short") {
		t.Fatalf("test precondition: FinalMessage = %q, want the corrected report", res.FinalMessage)
	}
	if !strings.Contains(out, "will not apply it") {
		t.Fatalf("console did not say the runner will not apply the corrected report; got:\n%s", out)
	}
	if !strings.Contains(out, res.ExitString()) {
		t.Errorf("console does not name how the resumed session ended (%s); got:\n%s", res.ExitString(), out)
	}
	if strings.Contains(out, "corrected report passes the check\n") {
		t.Errorf("console reported the corrected report as a plain pass; got:\n%s", out)
	}
}
