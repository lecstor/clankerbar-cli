package harness

import (
	"strings"
	"testing"
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
	p := resurrectionProbePrompt("EZY-196")
	if !strings.Contains(p, "EZY-196") {
		t.Error("probe prompt does not name the expected task ref")
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
