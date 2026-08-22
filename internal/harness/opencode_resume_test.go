package harness

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A quiet-death stream that also names its session — the shape the CLA-406
// resurrection path has to work from: the CLA-398 marker AND a session handle
// to resume into.
const quietDeathWithSession = `{"type":"step_start","sessionID":"ses_dead","part":{"type":"step-start","messageID":"msg_1"}}
{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"unknown","tokens":{"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}`

func TestOpencodeParseCapturesSessionID(t *testing.T) {
	res := opencodeParsed(quietDeathWithSession)
	if got := res.Raw[opencodeSessionIDKey]; got != "ses_dead" {
		t.Errorf("Raw[%q] = %v, want ses_dead", opencodeSessionIDKey, got)
	}

	// A stream with no session id (died before its first parseable event)
	// must NOT fabricate one — resumeTargets treats its absence as "nothing
	// to resume into".
	res = opencodeParsed(`{"type":"step_finish","part":{"type":"step-finish","reason":"unknown","tokens":{"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}`)
	if _, ok := res.Raw[opencodeSessionIDKey]; ok {
		t.Errorf("Raw[%q] set on a stream that never named a session; want absent", opencodeSessionIDKey)
	}
}

func TestResumeTargets(t *testing.T) {
	dead := opencodeParsed(quietDeathWithSession)
	dead.Claim = Claim{TaskID: "uuid-1", Ref: "EZY-196"}

	sid, ref := resumeTargets(dead)
	if sid != "ses_dead" {
		t.Errorf("sid = %q, want ses_dead", sid)
	}
	if ref != "EZY-196" {
		t.Errorf("ref = %q, want EZY-196", ref)
	}

	// No held claim (died before claiming): no ref to verify against, so no
	// resurrection — even though a session handle exists.
	noClaim := dead
	noClaim.Claim = Claim{}
	sid, ref = resumeTargets(noClaim)
	if sid != "ses_dead" || ref != "" {
		t.Errorf("unclaimed: sid=%q ref=%q; want sid kept, ref empty", sid, ref)
	}

	// No session id (died before any event): nothing to resume into.
	noSid := dead
	noSid.Raw = map[string]any{}
	sid, _ = resumeTargets(noSid)
	if sid != "" {
		t.Errorf("sid = %q on a Result with no session id; want empty", sid)
	}
}

func TestResurrectionProbePrompt(t *testing.T) {
	p := resurrectionProbePrompt
	// The real ref must NOT appear in the question: the probe exists to prove
	// the reply comes from the session's memory of its own claim_task call,
	// and naming the answer inside the question would let any model that can
	// parrot its prompt pass with no surviving transcript.
	if strings.Contains(p, "EZY-196") {
		t.Error("probe prompt names the real task ref - the coherence check is testing parroting, not recall")
	}
	// NO example of ANY kind, fixed placeholder included: an example is a
	// string a parroting model can echo back, and the instruction already
	// says exactly where the answer lives ("the ref your claim_task call
	// returned"). A ref-shaped token like ABC-123 in the question would let
	// a model fake the SHAPE of recall it cannot have.
	for _, leak := range []string{"ABC-123", "EZY-", "CLA-", "for example"} {
		if strings.Contains(p, leak) {
			t.Errorf("probe prompt carries an example (%q) - it must describe only the answer's shape", leak)
		}
	}
	if !strings.Contains(p, "claim_task") {
		t.Error("probe prompt must point at where the ref lives (the session's own claim_task call) now that no example shows its shape")
	}
	// The informing half is load-bearing (operator decision 2026-08-21): the
	// agent is told WHAT happened and THAT it resumes in place, mirroring the
	// operator's own interactive convention.
	for _, want := range []string{"interrupted", "transcript", "resumed"} {
		if !strings.Contains(strings.ToLower(p), want) {
			t.Errorf("probe prompt missing %q — an uninformed agent cannot resume confidently", want)
		}
	}
}

func TestProbeCoherent(t *testing.T) {
	if !probeCoherent("EZY-196 — I had just pushed the branch.", "EZY-196") {
		t.Error("a reply naming the claimed ref must count as coherent")
	}
	if !probeCoherent("i'm working on ezy-196, last done: tests", "EZY-196") {
		t.Error("coherence match should be case-insensitive")
	}
	for _, reply := range []string{
		"",
		"I don't know what you mean.",
		"EZY-260 — continuing that other task.",
		"Sure, what would you like me to do?",
	} {
		if probeCoherent(reply, "EZY-196") {
			t.Errorf("reply %q must NOT count as coherent against EZY-196", reply)
		}
	}
}

func TestOpencodeResumeArgs(t *testing.T) {
	in := Invocation{Model: "opencode/x-preview-f-free"}
	got := opencodeResumeArgs(in, "ses_dead", "Continue.")
	want := []string{"run", "--format", "json", "-s", "ses_dead", "--model", "opencode/x-preview-f-free", "--", "Continue."}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %v, want %v", got, want)
		}
	}

	got = opencodeResumeArgs(Invocation{}, "ses_dead", "Continue.")
	if strings.Join(got, " ") != "run --format json -s ses_dead -- Continue." {
		t.Errorf("no-model args = %v", got)
	}
}

func quietDeathResult(sid string) Result {
	res := opencodeParsed(quietDeathWithSession)
	if sid != "" {
		res.Raw[opencodeSessionIDKey] = sid
	}
	return res
}

func TestMergeResumeCleanContinuationClearsQuietDeath(t *testing.T) {
	base := quietDeathResult("ses_dead")
	base.Claim = Claim{TaskID: "uuid-1", Ref: "EZY-196"}
	base.Tokens = 100000

	cont := opencodeParsed(`{"type":"text","sessionID":"ses_dead","part":{"type":"text","text":"Continuing."}}
{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"stop","tokens":{"total":5000,"input":4000,"output":1000,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.01}}`)

	mergeResume(&base, cont)

	if (opencode{}).ZeroUsageUnknown(base) {
		t.Error("a clean continuation must CLEAR the quiet-death mark — otherwise the loop re-probes a live session and the driver counts a dead phase")
	}
	if base.Tokens != 105000 {
		t.Errorf("Tokens = %d, want 105000 (usage sums across the hole)", base.Tokens)
	}
	if base.FinalMessage != "Continuing." {
		t.Errorf("FinalMessage = %q, want the continuation's latest text", base.FinalMessage)
	}
	if !base.Claim.Held() || base.Claim.Ref != "EZY-196" {
		t.Errorf("Claim = %+v, want the held claim preserved", base.Claim)
	}
	if base.CostUSD < 0.01 {
		t.Errorf("CostUSD = %v, want the continuation's cost folded in", base.CostUSD)
	}
}

func TestMergeResumeQuietContinuationKeepsMark(t *testing.T) {
	base := quietDeathResult("ses_dead")

	// The continuation itself died quietly: the merged result must still carry
	// the mark so the caller's loop takes another round instead of declaring
	// victory over a second corpse.
	cont := opencodeParsed(`{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"unknown","tokens":{"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}`)
	mergeResume(&base, cont)

	if !(opencode{}).ZeroUsageUnknown(base) {
		t.Error("a continuation that died quietly must KEEP the quiet-death mark")
	}
}

func TestMergeResumeUnobservedContinuationKeepsClaim(t *testing.T) {
	// A pure mergeResume unit test: Invoke no longer produces UNSEEDED
	// continuations (it seeds from the dead run's live claim), but an
	// unobserved one - seeded, yet seeing no clankerbar tool events in its
	// turn - is still the shape this guard exists for.
	base := quietDeathResult("ses_dead")
	base.Claim = Claim{TaskID: "uuid-1", Ref: "EZY-196"}

	// A probe-style continuation that produced text but observed no clankerbar
	// tool events must not wipe the dead run's claim with a zero value.
	cont := opencodeParsed(`{"type":"text","sessionID":"ses_dead","part":{"type":"text","text":"EZY-196 — last action: ran go test."}}
{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"stop","tokens":{"total":900,"input":800,"output":100,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}`)
	mergeResume(&base, cont)

	if !base.Claim.Held() || base.Claim.Ref != "EZY-196" {
		t.Errorf("Claim = %+v, want the original claim kept when the continuation observed nothing", base.Claim)
	}
}

// Review WARN (finish_reason asymmetry): a continuation that names NO finish
// reason must clear the corpse's, symmetrically with terminal_reason. The
// parser writes finish_reason only when the stream carried a step_finish
// reason, so blind inheritance would leave the merged Result asserting the
// dead run's "unknown" while the mark cleared - ZeroUsageUnknown says
// recovered, deadPhase/tallyDead (which read THIS key) say dead, and the phase
// parks despite a successful resurrection.
func TestMergeResumeReasonlessContinuationClearsFinishReason(t *testing.T) {
	base := quietDeathResult("ses_dead")
	if base.Raw[FinishReasonKey] != FinishReasonUnknown {
		t.Fatalf("test precondition: the quiet-death result carries finish_reason %v", base.Raw[FinishReasonKey])
	}

	cont := opencodeParsed(`{"type":"text","sessionID":"ses_dead","part":{"type":"text","text":"killed mid-turn, no step_finish ever landed"}}`)
	mergeResume(&base, cont)

	if _, ok := base.Raw[FinishReasonKey]; ok {
		t.Errorf("Raw[finish_reason] = %v survived a continuation that named none - a recovered phase would still classify as dead", base.Raw[FinishReasonKey])
	}
	if (opencode{}).ZeroUsageUnknown(base) {
		t.Error("the quiet-death mark must be cleared alongside it")
	}
}

// ---- Invoke-level tests (CLA-406 review WARN 6) ----
//
// These exec a stub `opencode` through the REAL Invoke, so the loop's
// orchestration — backoff, probe, coherence gate, continuation, merge — is
// exercised end-to-end, not just its helpers.

const (
	// The first call: the session claims EZY-196, then dies the quiet death.
	resumeClaimEvent = `{"type":"tool_use","sessionID":"ses_dead","part":{"type":"tool","tool":"clankerbar_claim_task","callID":"c0","state":{"status":"completed","input":{"taskId":"uuid-1"},"output":"{\"task\":{\"id\":\"uuid-1\",\"ref\":\"EZY-196\"},\"run\":{\"id\":\"run-1\"}}"}}}` + "\n" + `{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"unknown","tokens":{"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}`
	// A coherent probe reply: names the claimed ref.
	resumeProbeOK = `{"type":"text","sessionID":"ses_dead","part":{"type":"text","text":"EZY-196 — last action: ran go test."}}
{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"stop","tokens":{"total":900,"input":800,"output":100,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.001}}`
	// An incoherent probe reply: names some OTHER task.
	resumeProbeWrong = `{"type":"text","sessionID":"ses_dead","part":{"type":"text","text":"EZY-260 — continuing that one."}}
{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"stop","tokens":{"total":900,"input":800,"output":100,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.001}}`
	// The continuation: does the work (update_task settle + branch), ends clean.
	resumeContinuation = `{"type":"tool_use","sessionID":"ses_dead","part":{"type":"tool","tool":"clankerbar_update_task","callID":"c1","state":{"status":"completed","input":{"taskId":"uuid-1","status":"in_review","branch":"clanker/x"},"output":"{\"ok\":true}"}}}
{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"stop","tokens":{"total":5000,"input":4000,"output":1000,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.01}}`
)

// resumeStub branches on the prompt the way the real loop drives it: no `-s` is
// the original session; the probe and the continuation are told apart by their
// prompts. Each test below writes its own stub script inline via opencodeStub,
// because the assertions differ per case.

func TestResurrectionInvokeHappyPath(t *testing.T) {
	old := opencodeResumeBackoff
	opencodeResumeBackoff = time.Millisecond
	defer func() { opencodeResumeBackoff = old }()

	console := &bytes.Buffer{}
	opencodeStub(t, `#!/bin/sh
case "$*" in
  *"Confirm you are intact"*)
    echo '{"type":"text","sessionID":"ses_dead","part":{"type":"text","text":"EZY-196 - last action: ran go test."}}'
    echo '{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"stop","tokens":{"total":900,"input":800,"output":100,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.001}}'
    ;;
  *"Continue exactly where you left off"*)
    echo '{"type":"tool_use","sessionID":"ses_dead","part":{"type":"tool","tool":"clankerbar_update_task","callID":"c1","state":{"status":"completed","input":{"taskId":"uuid-1","status":"in_review","branch":"clanker/x"},"output":"{\"ok\":true}"}}}'
    echo '{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"stop","tokens":{"total":5000,"input":4000,"output":1000,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.01}}'
    ;;
  *)
    echo '{"type":"tool_use","sessionID":"ses_dead","part":{"type":"tool","tool":"clankerbar_claim_task","callID":"c0","state":{"status":"completed","input":{"taskId":"uuid-1"},"output":"{\"task\":{\"id\":\"uuid-1\",\"ref\":\"EZY-196\"},\"run\":{\"id\":\"run-1\"}}"}}}'
    echo '{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"unknown","tokens":{"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}'
    ;;
esac
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{Prompt: "Work.", Console: console})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if (opencode{}).ZeroUsageUnknown(res) {
		t.Error("a resurrected-and-completed session must not carry the quiet-death mark")
	}
	// Adjudication 1 (review): nothing else pins that a RECOVERED result stops
	// looking like a dead phase to the DRIVER, which classifies on
	// finish_reason - not on the terminal_reason marker the merge is careful
	// with. Inheriting the corpse's "unknown" here would park a recovered
	// phase despite the clean mark.
	if res.Raw[FinishReasonKey] != "stop" {
		t.Errorf("Raw[finish_reason] = %v, want the continuation's \"stop\" - a recovered result must not classify as a dead phase", res.Raw[FinishReasonKey])
	}
	if n := res.Raw[opencodeResurrectionsKey]; n != 1 {
		t.Errorf("resurrections = %v, want 1", n)
	}
	// ERROR 1 regression: the settle observed by the CONTINUATION must survive
	// into the merged Claim. Unseeded, the continuation's parser starts with a
	// zero claim, update_task goes unobserved, Settled stays false, and
	// releaseHeldClaim posts ready over an in_review task.
	if !res.Claim.Settled {
		t.Errorf("Claim.Settled = false; the continuation's update_task was not observed — ResumeClaim seed missing")
	}
	if !res.Claim.HasWIP {
		t.Errorf("Claim.HasWIP = false; the continuation's branch recording was not observed")
	}
	// Spend sums across all three runs (dead + probe + continuation). The dead
	// run itself reports ZERO — that is the quiet death — so the honest sum is
	// the probe's 900 plus the continuation's 5000.
	if res.Tokens != 5900 {
		t.Errorf("Tokens = %d, want 5900 summed across probe and continuation (the dead run reports nothing)", res.Tokens)
	}
	if !strings.Contains(console.String(), "resurrection 1 succeeded") {
		t.Errorf("console missing success line; got:\n%s", console.String())
	}
}

func TestResurrectionInvokeWrongRefBreaksAfterOneProbe(t *testing.T) {
	old := opencodeResumeBackoff
	opencodeResumeBackoff = time.Millisecond
	defer func() { opencodeResumeBackoff = old }()

	dir := t.TempDir()
	console := &bytes.Buffer{}
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+dir+`/log"
case "$*" in
  *"Confirm you are intact"*)
    echo '{"type":"text","sessionID":"ses_dead","part":{"type":"text","text":"EZY-260 - continuing that one."}}'
    echo '{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"stop","tokens":{"total":900,"input":800,"output":100,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.001}}'
    ;;
  *)
    echo '{"type":"tool_use","sessionID":"ses_dead","part":{"type":"tool","tool":"clankerbar_claim_task","callID":"c0","state":{"status":"completed","input":{"taskId":"uuid-1"},"output":"{\"task\":{\"id\":\"uuid-1\",\"ref\":\"EZY-196\"},\"run\":{\"id\":\"run-1\"}}"}}}'
    echo '{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"unknown","tokens":{"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}'
    ;;
esac
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{Prompt: "Work.", Console: console})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// ONE probe per death: the original session plus the failed probe, and no
	// continuation - the incoherent answer ends it there.
	log, err := os.ReadFile(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatalf("stub log: %v", err)
	}
	if n := strings.Count(string(log), "\n"); n != 2 {
		t.Errorf("stub ran %d times, want exactly 2 (original session + one probe, no continuation)", n)
	}
	if _, ok := res.Raw[opencodeResurrectionsKey]; ok {
		t.Error("a failed probe must not record a resurrection")
	}
	if !(opencode{}).ZeroUsageUnknown(res) {
		t.Error("after a failed probe the death must stand (quiet-death mark kept)")
	}
	if !strings.Contains(console.String(), "resurrection probe FAILED") {
		t.Errorf("console missing failure line; got:\n%s", console.String())
	}
	if !strings.Contains(console.String(), "EZY-260") {
		t.Error("failure line should include the probe's actual reply (NIT 9)")
	}
}

func TestResurrectionInvokeCapStopsTheLoop(t *testing.T) {
	old := opencodeResumeBackoff
	opencodeResumeBackoff = time.Millisecond
	defer func() { opencodeResumeBackoff = old }()

	opencodeStub(t, `#!/bin/sh
case "$*" in
  *"Confirm you are intact"*)
    echo '{"type":"text","sessionID":"ses_dead","part":{"type":"text","text":"EZY-196 - still here."}}'
    echo '{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"stop","tokens":{"total":10,"input":10,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}'
    ;;
  *)
    echo '{"type":"tool_use","sessionID":"ses_dead","part":{"type":"tool","tool":"clankerbar_claim_task","callID":"c0","state":{"status":"completed","input":{"taskId":"uuid-1"},"output":"{\"task\":{\"id\":\"uuid-1\",\"ref\":\"EZY-196\"},\"run\":{\"id\":\"run-1\"}}"}}}'
    echo '{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"unknown","tokens":{"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}'
    ;;
esac
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{Prompt: "Work."})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if n := res.Raw[opencodeResurrectionsKey]; n != opencodeMaxResurrections {
		t.Errorf("resurrections = %v, want %d (the cap stops the loop)", n, opencodeMaxResurrections)
	}
	if !(opencode{}).ZeroUsageUnknown(res) {
		t.Error("past the cap the last quiet death stands")
	}
}

// doneWhen clause 4, proven at Invoke level: a final step that is "unknown"
// but PAID is not a quiet death - the ZeroUsageUnknown predicate is false, so
// the existing path takes over untouched: exactly ONE process runs, no probe,
// no continuation, no resurrection recorded.
func TestResurrectionInvokePaidUnknownNotResurrected(t *testing.T) {
	dir := t.TempDir()
	console := &bytes.Buffer{}
	opencodeStub(t, `#!/bin/sh
echo "$*" >> "`+dir+`/log"
echo '{"type":"tool_use","sessionID":"ses_dead","part":{"type":"tool","tool":"clankerbar_claim_task","callID":"c0","state":{"status":"completed","input":{"taskId":"uuid-1"},"output":"{\"task\":{\"id\":\"uuid-1\",\"ref\":\"EZY-196\"},\"run\":{\"id\":\"run-1\"}}"}}}'
echo '{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"unknown","tokens":{"input":4000,"output":1000,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.01}}'
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{Prompt: "Work.", Console: console})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	log, err := os.ReadFile(filepath.Join(dir, "log"))
	if err != nil {
		t.Fatalf("stub log: %v", err)
	}
	if n := strings.Count(string(log), "\n"); n != 1 {
		t.Errorf("stub ran %d times, want exactly 1 - a paid unknown is not a quiet death and must not be resurrected", n)
	}
	if _, ok := res.Raw[opencodeResurrectionsKey]; ok {
		t.Error("a paid unknown must not record a resurrection")
	}
	if strings.Contains(console.String(), "quiet death detected") {
		t.Error("console announced a resurrection for a paid unknown - the existing path must take over untouched")
	}
}

// Review WARN (probe observations discarded): the probe turn runs with MCP
// tools available, so it can settle or advance the claim exactly as the
// continuation could. A probe that moved the task must not be invisible to
// the driver - that is ERROR 1 one turn earlier: without the fold, a probe
// that declared in_review followed by an unobserved continuation merges into
// Held()+unsettled, and releaseHeldClaim posts ready over the in_review task.
func TestResurrectionInvokeProbeObservationSurvives(t *testing.T) {
	old := opencodeResumeBackoff
	opencodeResumeBackoff = time.Millisecond
	defer func() { opencodeResumeBackoff = old }()

	opencodeStub(t, `#!/bin/sh
case "$*" in
  *"Confirm you are intact"*)
    echo '{"type":"tool_use","sessionID":"ses_dead","part":{"type":"tool","tool":"clankerbar_update_task","callID":"c1","state":{"status":"completed","input":{"taskId":"uuid-1","status":"in_review","branch":"clanker/x"},"output":"{\"ok\":true}"}}}'
    echo '{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"stop","tokens":{"total":900,"input":800,"output":100,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.001}}'
    ;;
  *"Continue exactly where you left off"*)
    echo '{"type":"text","sessionID":"ses_dead","part":{"type":"text","text":"Done."}}'
    echo '{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"stop","tokens":{"total":10,"input":10,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}'
    ;;
  *)
    echo '{"type":"tool_use","sessionID":"ses_dead","part":{"type":"tool","tool":"clankerbar_claim_task","callID":"c0","state":{"status":"completed","input":{"taskId":"uuid-1"},"output":"{\"task\":{\"id\":\"uuid-1\",\"ref\":\"EZY-196\"},\"run\":{\"id\":\"run-1\"}}"}}}'
    echo '{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"unknown","tokens":{"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}'
    ;;
esac
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{Prompt: "Work."})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Claim.Settled {
		t.Error("Claim.Settled = false; the PROBE's update_task was dropped - probe observations must fold into the merged result")
	}
	if !res.Claim.HasWIP {
		t.Error("Claim.HasWIP = false; the probe's branch recording was dropped")
	}
}

// An UNTRUSTED probe capture cannot prove coherence: its text survived an
// over-cap event discard, so a ref in FinalMessage may be debris. The verdict
// must be death - not a continuation sent on evidence the adapter itself says
// is unreadable (the same CLA-262 reasoning as the fold guard and the loop
// conjunct). Today the discard empties the whole parse, so the bare coherence
// check already fails; this test pins the OUTCOME either way, so a future
// parser that keeps good lines after a bad one cannot quietly resurrect on a
// capture marked unreadable.
func TestResurrectionInvokeUntrustedProbeCountsDeath(t *testing.T) {
	old := opencodeResumeBackoff
	opencodeResumeBackoff = time.Millisecond
	defer func() { opencodeResumeBackoff = old }()

	console := &bytes.Buffer{}
	opencodeStub(t, `#!/bin/sh
case "$*" in
  *"Confirm you are intact"*)
    # One event over the 16 MiB line cap: the capture is discarded and the
    # Result marked untrusted, even though a coherent reply follows it.
    head -c 17000000 /dev/zero | tr '\0' 'x'
    echo '{"type":"text","sessionID":"ses_dead","part":{"type":"text","text":"EZY-196 - last action: ran go test."}}'
    echo '{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"stop","tokens":{"total":900,"input":800,"output":100,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0.001}}'
    ;;
  *)
    echo '{"type":"tool_use","sessionID":"ses_dead","part":{"type":"tool","tool":"clankerbar_claim_task","callID":"c0","state":{"status":"completed","input":{"taskId":"uuid-1"},"output":"{\"task\":{\"id\":\"uuid-1\",\"ref\":\"EZY-196\"},\"run\":{\"id\":\"run-1\"}}"}}}'
    echo '{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"unknown","tokens":{"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}'
    ;;
esac
`)

	res, err := (opencode{}).Invoke(context.Background(), Invocation{Prompt: "Work.", Console: console})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// The quiet death STANDS: an untrusted probe is not evidence of a live
	// session, so no continuation may run and the dead-phase path takes over.
	if !(opencode{}).ZeroUsageUnknown(res) {
		t.Error("an untrusted probe reply must count as a death - ZeroUsageUnknown mark lost")
	}
	if n := res.Raw[opencodeResurrectionsKey]; n != nil {
		t.Errorf("resurrections = %v, want none recorded - the round never earned a continuation", n)
	}
	if !strings.Contains(console.String(), "resurrection probe FAILED") {
		t.Errorf("console missing probe-failure line; got:\n%s", console.String())
	}
	// Spend first: the probe ran and its figures are charged even though its
	// answer was rejected.
	if res.Tokens != 900 {
		t.Errorf("Tokens = %d, want 900 charged from the rejected probe", res.Tokens)
	}
}

// mergeResume table rows for the review's ERROR 2 / ERROR 3 cases.

func TestMergeResumeCappedContinuationKeepsCapMarker(t *testing.T) {
	base := quietDeathResult("ses_dead")
	cont := Result{
		ExitCode: -1,
		Raw:      map[string]any{TerminalReasonKey: wallClockReason, FinishReasonKey: "tool-calls"},
	}
	mergeResume(&base, cont)
	if !(opencode{}).WallClockCapped(base) {
		t.Error("a wall-clock-capped continuation must keep the cap marker on the merged result")
	}
	if (opencode{}).ZeroUsageUnknown(base) {
		t.Error("the cap marker must replace the quiet-death mark, not stack beside it")
	}
}

func TestMergeResumeFailedContinuationCarriesOutcome(t *testing.T) {
	base := quietDeathResult("ses_dead")
	cont := Result{
		ExitCode: 1,
		Stderr:   `{"type":"error"} Error: 402 payment required, out of credits`,
		Tokens:   777,
	}
	mergeResume(&base, cont)
	if base.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want the continuation's non-zero exit", base.ExitCode)
	}
	if lim := (opencode{}).DetectLimit(base); !lim.Limited || !lim.Stop {
		t.Errorf("DetectLimit = %+v; a budget-exhausted continuation must trip the hard stop through the merged streams", lim)
	}
	if base.Tokens != 777 {
		t.Errorf("Tokens = %d, want the continuation's spend charged even on the error path", base.Tokens)
	}
}

func TestMergeResumeUntrustedContinuationMarksMerged(t *testing.T) {
	base := quietDeathResult("ses_dead")
	cont := Result{Tokens: 5}
	cont.markUntrusted("tool_result line exceeded the retained tail")
	mergeResume(&base, cont)
	if base.Untrusted == "" {
		t.Error("an untrusted continuation must make the merged result untrusted (CLA-262 gates read this)")
	}
}

// Second-review finding 2 regression: escalate_question arms a settle WITHOUT a
// Names() check, so even a bare parse ends with Claim{Settled: true}. On an
// UNSEEDED continuation that zero-identity settled claim used to pass
// mergeResume's guard and wipe the dead run's held claim (TaskID/Ref/RunID all
// gone — no handback, no salvage, no attribution). With the ResumeClaim seed
// the Invoke now applies, Settled lands on the REAL claim instead: identity
// preserved, held correctly false.
func TestMergeResumeEscalatedContinuationKeepsClaimIdentity(t *testing.T) {
	base := quietDeathResult("ses_dead")
	base.Claim = Claim{TaskID: "uuid-1", Ref: "EZY-196", RunID: "run-1"}

	seeded := Invocation{Prompt: "Work."}
	seeded.ResumeClaim = base.Claim
	cont := opencodeParsedFrom(seeded, nil, `{"type":"tool_use","sessionID":"ses_dead","part":{"type":"tool","tool":"clankerbar_escalate_question","callID":"c2","state":{"status":"completed","input":{"questionId":"q1"},"output":"{\"ok\":true}"}}}
{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"stop","tokens":{"total":10,"input":10,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}`)

	if !cont.Claim.Settled {
		t.Fatal("test precondition: the escalated continuation must observe Settled")
	}
	mergeResume(&base, cont)

	if base.Claim.TaskID != "uuid-1" || base.Claim.Ref != "EZY-196" || base.Claim.RunID != "run-1" {
		t.Errorf("Claim identity wiped by the escalated continuation: %+v", base.Claim)
	}
	if !base.Claim.Settled {
		t.Error("Settled must survive the merge — escalation deliberately ends the run")
	}
	if base.Claim.Held() {
		t.Error("an escalated claim is settled, not held")
	}
}

// Re-review finding 1: sameClaim ignores Status, and Status decides
// ClaimsMerge(). A dead run that declared in_review, then a continuation that
// re-declares done on the identical delivery, is a status UPGRADE — keeping the
// earlier report would leave ClaimsMerge() false and skip merge attestation.
// Later wins, matching settleReport's own rule.
func TestMergeResumeLaterReportWinsStatusUpgrade(t *testing.T) {
	base := quietDeathResult("ses_dead")
	base.Reports = []Report{{
		TaskID: "uuid-1", Ref: "EZY-196", RunID: "run-1",
		Status: "in_review", Branch: "clanker/x",
		Commit: "abc123", IntegrationBranch: "staging",
	}}

	// The continuation runs SEEDED (as Invoke now seeds it) — without the seed
	// its update_task would be unobserved and there would be nothing to dedupe.
	seeded := Invocation{Prompt: "Work."}
	seeded.ResumeClaim = Claim{TaskID: "uuid-1", Ref: "EZY-196", RunID: "run-1"}
	cont := opencodeParsedFrom(seeded, nil, `{"type":"tool_use","sessionID":"ses_dead","part":{"type":"tool","tool":"clankerbar_update_task","callID":"c1","state":{"status":"completed","input":{"taskId":"uuid-1","status":"done","branch":"clanker/x","delivery":{"commit":"abc123","integrationBranch":"staging"}},"output":"{\"ok\":true}"}}}
{"type":"step_finish","sessionID":"ses_dead","part":{"type":"step-finish","reason":"stop","tokens":{"total":10,"input":10,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}`)
	mergeResume(&base, cont)

	if len(base.Reports) != 1 {
		t.Fatalf("Reports = %d entries, want 1 deduped", len(base.Reports))
	}
	if got := base.Reports[0].Status; got != "done" {
		t.Errorf("Report.Status = %q, want the later 'done' (the upgrade must not be dropped)", got)
	}
	if !base.Reports[0].ClaimsMerge() {
		t.Error("the upgraded report must claim the merge landed - attestation depends on it")
	}
}
