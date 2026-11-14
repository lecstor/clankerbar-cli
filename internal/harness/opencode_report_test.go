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
	"sync"
	"testing"
	"time"
)

// safeBuffer is a console writer the idle guard can use from its timer
// goroutine while the capture writes from the exec pipe. A plain bytes.Buffer
// races between the two: the watchdog prints its kill line the instant it
// fires, which is exactly while the stalled stub's last bytes are arriving.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

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

	// A stalled resume's evidence: the stream opens (so it is not a launch
	// failure), reports the step it already spent, and then says nothing — the
	// CLA-868 shape before the process was killed at the phase's 15m bound.
	reportStallStart = `{"type":"step_start","sessionID":"ses_report","part":{"type":"step-start"}}`
	reportStallStep  = `{"type":"step_finish","sessionID":"ses_report","part":{"type":"step-finish","reason":"tool-calls","tokens":{"total":300,"input":100,"output":200,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.02}}`
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

// A STANDALONE invocation holds no claim and applies the report itself rather
// than a runner writing records, so the one re-emit must still be available.
func TestReportResume_UnclaimedSessionResumesWhenAllowed(t *testing.T) {
	logPath := reportStub(t)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:         "Work.",
		Console:        &bytes.Buffer{},
		AllowUnclaimed: true,
		CheckReport:    reportRefusesLong,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if runs := stubRuns(t, logPath); len(runs) != 2 {
		t.Fatalf("stub ran %d times, want exactly 2 (the session + ONE report re-emit):\n%v", len(runs), runs)
	}
	if !strings.Contains(res.FinalMessage, "short") {
		t.Errorf("FinalMessage = %q, want the continuation's corrected report", res.FinalMessage)
	}
}

// A second refusal is the runner's to fail closed on: one resume, never a loop.
// The refused reply leaves the report as it was, and the resume-only marker
// tells the driver to retry the phase under the claim it still holds.
func TestReportResume_SecondRefusalIsNotResumedAgain(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "log")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
echo '`+reportBadText+`'
echo '`+reportBadStep+`'
`)
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
	if runs := stubRuns(t, logPath); len(runs) != 2 {
		t.Errorf("stub ran %d times, want exactly 2 — one resume, never a loop:\n%v", len(runs), runs)
	}
	if res.ReportResumeFailed == "" {
		t.Error("ReportResumeFailed is empty; a resume that still cannot produce an applicable report must fail closed")
	}
	if !strings.Contains(res.FinalMessage, "too long") {
		t.Errorf("FinalMessage = %q, want the session's own report preserved", res.FinalMessage)
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

// A cap kill is now named by the shared resume guard, which runs before the
// report resume's own budget check. A refused report must still say why it was
// left alone rather than reading like one that passed; a report that PASSED owes
// no resume and so earns no line.
func TestReportResume_WallClockSkipSaysWhy(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "log")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
`)

	res := reportSession(reportBadStream)
	res.ExitCode = -1
	res.Raw[TerminalReasonKey] = wallClockReason

	console := &bytes.Buffer{}
	in := Invocation{Prompt: "Work.", Console: console, ResumeClaim: reportClaim, CheckReport: reportRefusesLong}
	(opencode{}).resumeBadReport(context.Background(), in, &res, time.Time{})

	if runs := stubRuns(t, logPath); len(runs) != 0 {
		t.Errorf("stub ran %d times, want none — a cap-killed session cannot be resumed:\n%v", len(runs), runs)
	}
	if !strings.Contains(console.String(), "wall-clock budget exhausted") {
		t.Errorf("console did not say why the refused report was left alone; got:\n%s", console.String())
	}

	// The same end with a report that passes: nothing was owed, so no line.
	passing := &bytes.Buffer{}
	in.Console = passing
	in.CheckReport = func(string) error { return nil }
	(opencode{}).resumeBadReport(context.Background(), in, &res, time.Time{})
	if out := passing.String(); out != "" {
		t.Errorf("a passing report on a cap-killed session printed a resume line; got:\n%s", out)
	}
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
	// CLA-877: the error is a resume-only failure too, so the runner is told to
	// retry the phase inside the claim it holds instead of advancing on a report
	// that never reached it.
	if !strings.Contains(res.ReportResumeFailed, "errored") {
		t.Errorf("ReportResumeFailed = %q, want the resume's error", res.ReportResumeFailed)
	}
}

// CLA-877: a report resume that produces NOTHING is killed at the resume's own
// short idle bound, not the phase's, and retried once with the same steer. The
// second attempt's corrected report is what the runner reads, and the stalled
// attempt's spend is still charged.
func TestReportResume_StalledResumeIsKilledAtTheShortBoundAndRetriedOnce(t *testing.T) {
	old := opencodeReportResumeIdle
	opencodeReportResumeIdle = 200 * time.Millisecond
	t.Cleanup(func() { opencodeReportResumeIdle = old })

	logPath := filepath.Join(t.TempDir(), "log")
	countPath := filepath.Join(t.TempDir(), "resumes")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
case "$*" in
  *"not accepted by the runner"*)
    n=$(cat "`+countPath+`" 2>/dev/null || echo 0)
    n=$((n+1))
    echo $n > "`+countPath+`"
    if [ "$n" -eq 1 ]; then
      echo '`+reportStallStart+`'
      echo '`+reportStallStep+`'
      exec sleep 30
    fi
    echo '`+reportGoodText+`'
    echo '`+reportGoodStep+`'
    ;;
  *)
    echo '`+reportBadText+`'
    echo '`+reportBadStep+`'
    ;;
esac
`)
	console := &safeBuffer{}
	start := time.Now()
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "Work.",
		Console:     console,
		ResumeClaim: reportClaim,
		CheckReport: reportRefusesLong,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Invoke took %s — the stalled resume was not killed at the report resume's short idle bound", elapsed)
	}
	runs := stubRuns(t, logPath)
	if len(runs) != 3 {
		t.Fatalf("stub ran %d times, want 3 (the session + a stalled resume + its retry):\n%v", len(runs), runs)
	}
	if !strings.Contains(runs[2], "-s ses_report") || !strings.Contains(runs[2], errReportTooLong.Error()) {
		t.Errorf("the retry did not resume the same session with the same steer: %q", runs[2])
	}
	if !strings.Contains(res.FinalMessage, "short") {
		t.Errorf("FinalMessage = %q, want the retry's corrected report", res.FinalMessage)
	}
	if res.ReportResumeFailed != "" {
		t.Errorf("ReportResumeFailed = %q after a successful retry, want none", res.ReportResumeFailed)
	}
	if want := 500 + 300 + 900; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d (the stalled attempt's spend is charged too)", res.Tokens, want)
	}
	out := console.String()
	if !strings.Contains(out, "report resume stalled") || !strings.Contains(out, "retrying it with the same steer") {
		t.Errorf("console did not announce the stall and its retry; got:\n%s", out)
	}
	if !strings.Contains(out, "corrected report passes the check") {
		t.Errorf("console did not report the corrected check; got:\n%s", out)
	}
}

// CLA-877: a SECOND stall fails closed. The report stays refused, but the
// session's own end is preserved — no IdleKilled, the original exit code and
// final message — and the resume-only marker tells the runner what really
// failed, so the phase can be retried inside the claim it still holds.
func TestReportResume_SecondStallFailsClosedAndKeepsTheSessionsOwnEnd(t *testing.T) {
	old := opencodeReportResumeIdle
	opencodeReportResumeIdle = 200 * time.Millisecond
	t.Cleanup(func() { opencodeReportResumeIdle = old })

	logPath := filepath.Join(t.TempDir(), "log")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
case "$*" in
  *"not accepted by the runner"*)
    echo '`+reportStallStart+`'
    echo '`+reportStallStep+`'
    exec sleep 30
    ;;
  *)
    echo '`+reportBadText+`'
    echo '`+reportBadStep+`'
    ;;
esac
`)
	console := &safeBuffer{}
	start := time.Now()
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "Work.",
		Console:     console,
		ResumeClaim: reportClaim,
		CheckReport: reportRefusesLong,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Invoke took %s — a second stalled resume must be bounded too", elapsed)
	}
	if runs := stubRuns(t, logPath); len(runs) != 3 {
		t.Fatalf("stub ran %d times, want 3 — at most two resume attempts, never a loop:\n%v", len(runs), runs)
	}
	if res.ReportResumeFailed == "" {
		t.Error("ReportResumeFailed is empty; the runner cannot tell a resume-only failure from a dead session")
	}
	if !strings.Contains(res.ReportResumeFailed, "no output for") {
		t.Errorf("ReportResumeFailed = %q, want the idle cause", res.ReportResumeFailed)
	}
	if res.IdleKilled != "" {
		t.Errorf("IdleKilled = %q; the SESSION did not go silent, only its report resume did", res.IdleKilled)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want the session's own clean exit preserved", res.ExitCode)
	}
	if !strings.Contains(res.FinalMessage, "too long") {
		t.Errorf("FinalMessage = %q, want the refused report unchanged", res.FinalMessage)
	}
	if want := 500 + 300 + 300; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d (both stalled attempts' spend is charged)", res.Tokens, want)
	}
	out := console.String()
	if !strings.Contains(out, "report resume stalled 2 times") || !strings.Contains(out, "session's own end stands") {
		t.Errorf("console did not state the fail-closed outcome; got:\n%s", out)
	}
}

// CLA-877 review 1.review-1.1: the stall is STICKY. Attempt 1 is idle-killed at
// the short bound and the phase wall clock then leaves less than the resume floor
// for attempt 2, so no second process runs — but the result must still carry the
// resume-only marker. Without it the session's own clean exit would be the whole
// verdict: an implement phase would checkpoint on its pushed branch and advance
// to review with no delivery record.
func TestReportResume_StallThenWallClockSkipStillFailsClosed(t *testing.T) {
	old := opencodeReportResumeIdle
	// A 2s stall against a 1s surplus over the floor: the window is wide enough
	// that process-start jitter cannot flip which gate refuses.
	opencodeReportResumeIdle = 2 * time.Second
	t.Cleanup(func() { opencodeReportResumeIdle = old })

	logPath := filepath.Join(t.TempDir(), "log")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
case "$*" in
  *"not accepted by the runner"*)
    echo '`+reportStallStart+`'
    echo '`+reportStallStep+`'
    exec sleep 30
    ;;
  *)
    echo '`+reportBadText+`'
    echo '`+reportBadStep+`'
    ;;
esac
`)
	console := &safeBuffer{}
	start := time.Now()
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "Work.",
		Console:     console,
		ResumeClaim: reportClaim,
		CheckReport: reportRefusesLong,
		// Above the floor when Invoke starts (so attempt 1 is allowed), below it
		// once the 2s stall has been killed: ~59s remain, under the 1m floor.
		MaxSessionWallClock: opencodeResumeWallClockFloor + time.Second,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("Invoke took %s — the stalled attempt was not killed at the short bound", elapsed)
	}
	if runs := stubRuns(t, logPath); len(runs) != 2 {
		t.Fatalf("stub ran %d times, want 2 (the session + the one stalled attempt the budget allowed):\n%v", len(runs), runs)
	}
	if res.ReportResumeFailed == "" {
		t.Fatal("ReportResumeFailed is empty; a stall with no room for the retry must fail closed, not read as the session's clean end")
	}
	if !strings.Contains(res.ReportResumeFailed, "no output for") {
		t.Errorf("ReportResumeFailed = %q, want the stall to be the cause, not the wall clock", res.ReportResumeFailed)
	}
	if res.IdleKilled != "" {
		t.Errorf("IdleKilled = %q; the SESSION did not go silent, only its report resume did", res.IdleKilled)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want the session's own clean exit preserved", res.ExitCode)
	}
	if !strings.Contains(res.FinalMessage, "too long") {
		t.Errorf("FinalMessage = %q, want the session's own refused report unchanged", res.FinalMessage)
	}
	out := console.String()
	if !strings.Contains(out, "failing closed on the stall") {
		t.Errorf("console did not say the stall fails closed when the budget ran out; got:\n%s", out)
	}
}

// CLA-877 review 1.review-1.1: a second attempt that ends non-zero with no
// applicable report is a resume-only failure too. Its exit must NOT be merged
// (that would brand the phase with the resume's death) and its unreadable reply
// must not replace the refused one; the marker names the cause.
func TestReportResume_NonZeroRetryIsAResumeOnlyFailure(t *testing.T) {
	old := opencodeReportResumeIdle
	opencodeReportResumeIdle = 200 * time.Millisecond
	t.Cleanup(func() { opencodeReportResumeIdle = old })

	logPath := filepath.Join(t.TempDir(), "log")
	countPath := filepath.Join(t.TempDir(), "resumes")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
case "$*" in
  *"not accepted by the runner"*)
    n=$(cat "`+countPath+`" 2>/dev/null || echo 0)
    n=$((n+1))
    echo $n > "`+countPath+`"
    if [ "$n" -eq 1 ]; then
      echo '`+reportStallStart+`'
      echo '`+reportStallStep+`'
      exec sleep 30
    fi
    echo '`+reportGoodText+`'
    echo '`+reportGoodStep+`'
    exit 1
    ;;
  *)
    echo '`+reportBadText+`'
    echo '`+reportBadStep+`'
    ;;
esac
`)
	console := &safeBuffer{}
	res, err := (opencode{}).Invoke(context.Background(), Invocation{
		Prompt:      "Work.",
		Console:     console,
		ResumeClaim: reportClaim,
		CheckReport: reportRefusesLong,
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if runs := stubRuns(t, logPath); len(runs) != 3 {
		t.Fatalf("stub ran %d times, want 3 (the session + the stall + its retry):\n%v", len(runs), runs)
	}
	if res.ReportResumeFailed == "" {
		t.Fatal("ReportResumeFailed is empty; a non-zero retry produced no report the runner can apply")
	}
	if !strings.Contains(res.ReportResumeFailed, "exit 1") {
		t.Errorf("ReportResumeFailed = %q, want it to name the retry's end", res.ReportResumeFailed)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want the session's own clean exit — the failed retry's exit must not be merged", res.ExitCode)
	}
	if strings.Contains(res.FinalMessage, "short") {
		t.Errorf("FinalMessage = %q, want the failed retry's text NOT merged", res.FinalMessage)
	}
	if !strings.Contains(res.FinalMessage, "too long") {
		t.Errorf("FinalMessage = %q, want the session's own refused report preserved", res.FinalMessage)
	}
}

// A resume the wall clock leaves no room to START is a resume-only failure as
// well: no process runs, but the report is still refused, so the marker must be
// set or the phase advances on a report the runner never accepted.
func TestReportResume_WallClockSkipBeforeAResumeFailsClosed(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "log")
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+logPath+`"
echo '`+reportGoodText+`'
echo '`+reportGoodStep+`'
`)
	console := &bytes.Buffer{}
	res := reportSession(reportBadStream)
	in := Invocation{Prompt: "Work.", Console: console, ResumeClaim: reportClaim, CheckReport: reportRefusesLong}
	(opencode{}).resumeBadReport(context.Background(), in, &res, time.Now().Add(opencodeResumeWallClockFloor/2))

	if runs := stubRuns(t, logPath); len(runs) != 0 {
		t.Errorf("stub ran %d times, want none — no budget for a resume:\n%v", len(runs), runs)
	}
	if res.ReportResumeFailed == "" {
		t.Fatal("ReportResumeFailed is empty; a refused report that cannot be resumed must fail closed")
	}
	if !strings.Contains(res.ReportResumeFailed, "wall-clock") {
		t.Errorf("ReportResumeFailed = %q, want the wall clock named as the cause", res.ReportResumeFailed)
	}
	if !strings.Contains(res.FinalMessage, "too long") {
		t.Errorf("FinalMessage = %q, want the session's own refused report unchanged", res.FinalMessage)
	}
	if out := console.String(); !strings.Contains(out, "wall-clock budget exhausted") {
		t.Errorf("console did not say why no resume ran; got:\n%s", out)
	}
}

// The resume's bound is its own, but a SHORTER phase dial still wins, and a
// disabled phase watchdog does not disable it.
func TestReportResumeIdle(t *testing.T) {
	if got := reportResumeIdle(30 * time.Second); got != 30*time.Second {
		t.Errorf("reportResumeIdle(30s) = %s, want the phase's stricter dial", got)
	}
	if got := reportResumeIdle(0); got != opencodeReportResumeIdle {
		t.Errorf("reportResumeIdle(0) = %s, want the resume's own bound", got)
	}
	if got := reportResumeIdle(time.Hour); got != opencodeReportResumeIdle {
		t.Errorf("reportResumeIdle(1h) = %s, want the resume's own bound", got)
	}
}

// A corrected report the check accepts is still wasted when the resumed session
// ended in a way the driver never applies a report on. The failed resume's exit
// and text must NOT be merged — that would brand the phase with the resume's
// death — and the resume-only marker must say what happened so the phase is
// retried under the claim instead of advancing on the report.
func TestReportResume_UnusableEndFailsClosedWithoutMerging(t *testing.T) {
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

	if res.ReportResumeFailed == "" {
		t.Fatal("ReportResumeFailed is empty; a resume whose end no report is read on must fail closed")
	}
	if strings.Contains(res.FinalMessage, "short") {
		t.Errorf("FinalMessage = %q, want the unusable resume's text NOT merged", res.FinalMessage)
	}
	if !strings.Contains(res.FinalMessage, "too long") {
		t.Errorf("FinalMessage = %q, want the session's own refused report preserved", res.FinalMessage)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want the session's own clean exit — the cap-killed resume's exit must not be merged", res.ExitCode)
	}
	out := console.String()
	if !strings.Contains(out, "leaving the report as it was") {
		t.Errorf("console did not say the refused report stands; got:\n%s", out)
	}
	if strings.Contains(out, "corrected report passes the check\n") {
		t.Errorf("console reported the corrected report as a plain pass; got:\n%s", out)
	}
}
