package harness

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestClaudeRenderAndParse(t *testing.T) {
	var res Result
	var console bytes.Buffer
	lines := []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Draining."},{"type":"tool_use","name":"Bash"}]}}`,
		`{"type":"result","subtype":"success","result":"Backlog drained.","total_cost_usd":0.12,"usage":{"input_tokens":100,"output_tokens":20}}`,
		`not json — must be tolerated`,
	}
	for _, l := range lines {
		(claude{}).renderAndParse([]byte(l), &console, &res)
	}
	if res.FinalMessage != "Backlog drained." {
		t.Errorf("FinalMessage = %q", res.FinalMessage)
	}
	if res.Tokens != 120 {
		t.Errorf("Tokens = %d, want 120", res.Tokens)
	}
	if res.CostUSD != 0.12 {
		t.Errorf("CostUSD = %v, want 0.12", res.CostUSD)
	}
	if out := console.String(); !strings.Contains(out, "Draining.") || !strings.Contains(out, "Bash") {
		t.Errorf("console missing rendered content:\n%s", out)
	}
}

// claimStream renders the events a session emits around the clankerbar tools, so
// each test states only the calls it cares about.
func claimStream(lines ...string) Result {
	var res Result
	var console bytes.Buffer
	for _, l := range lines {
		(claude{}).renderAndParse([]byte(l), &console, &res)
	}
	return res
}

func toolUse(id, name, input string) string {
	return `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id +
		`","name":"` + name + `","input":` + input + `}]}}`
}

func toolResult(id, text string) string {
	b, _ := json.Marshal(text)
	return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id +
		`","content":[{"type":"text","text":` + string(b) + `}]}]}}`
}

// A refused MCP call. The plane changed nothing, so neither may we.
func toolResultErr(id, text string) string {
	b, _ := json.Marshal(text)
	return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id +
		`","is_error":true,"content":[{"type":"text","text":` + string(b) + `}]}]}}`
}

// The plane returns BOTH a UUID and a qualified ref, and accepts either as a
// later `taskId`. Real-shaped ids here on purpose: synthetic "t-1" everywhere is
// what let a ref/UUID mismatch hide.
const (
	claimUUID = "99f0647b-cef2-45cd-b2bc-23e8a7f97b0d"
	claimRef  = "CLA-242"
	claimOK   = `{"task":{"id":"` + claimUUID + `","ref":"` + claimRef + `"},"run":{"id":"r-1"},"branch":"clanker/x"}`
)

// heldClaim is the claim the fixture stream establishes.
func heldClaim() Claim {
	return Claim{TaskID: claimUUID, Ref: claimRef, RunID: "r-1", Branch: "clanker/x"}
}

// withClaim prefixes the two events that claim the task, so each case states
// only what it is actually about.
func withClaim(more ...string) []string {
	return append([]string{
		toolUse("u1", claimTaskTool, `{"taskId":"`+claimUUID+`"}`),
		toolResult("u1", claimOK),
	}, more...)
}

func settled() Claim { c := heldClaim(); c.Settled = true; return c }
func withWIP() Claim { c := heldClaim(); c.HasWIP = true; return c }

func TestClaudeClaimTracking(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  Claim
	}{
		{
			name:  "claimed and never settled - the lease the driver must hand back",
			lines: withClaim(),
			want:  heldClaim(),
		},
		{
			name: "settled at in_review by UUID",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"`+claimUUID+`","status":"in_review"}`),
				toolResult("u2", `{"task":{"id":"`+claimUUID+`"}}`),
			),
			want: settled(),
		},
		{
			// The plane resolves a taskId as a UUID *or* a ref. Miss this and the
			// driver posts `ready` over a task already handed to review.
			name: "settled at in_review by REF",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"`+claimRef+`","status":"in_review"}`),
				toolResult("u2", `{"task":{"id":"`+claimUUID+`"}}`),
			),
			want: settled(),
		},
		{
			name: "ref matching is case-insensitive, as the plane's own parse is",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"cla-242","status":"done"}`),
				toolResult("u2", `{}`),
			),
			want: settled(),
		},
		{
			// updateStatus clears the holder for every status but in_progress, so
			// an allowlist of "terminal" ones would read this as still held and
			// promote an explicitly de-scoped task back to ready.
			name: "demoting the task to backlog also lets go of it",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"`+claimUUID+`","status":"backlog"}`),
				toolResult("u2", `{}`),
			),
			want: settled(),
		},
		{
			// evidence_required / delivery_required land here. The task is still
			// held, and this is exactly the session whose claim wants handing back.
			name: "a REFUSED terminal update leaves the claim standing",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"`+claimUUID+`","status":"in_review"}`),
				toolResultErr("u2", "evidence_required"),
			),
			want: heldClaim(),
		},
		{
			name: "a status-less revision of the held task leaves the claim standing",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"`+claimUUID+`","outcome":"still going"}`),
				toolResult("u2", `{}`),
			),
			want: heldClaim(),
		},
		{
			// A blocking question ends the run and sets the task `blocked` with no
			// update_task at all. Releasing it would drop a task awaiting the
			// operator back into the claimable queue, question unanswered.
			name: "a BLOCKING question is a handback",
			lines: withClaim(
				toolUse("u2", askQuestionTool, `{"taskId":"`+claimUUID+`","question":"which?","blocking":true}`),
				toolResult("u2", `{"taskBlocked":true}`),
			),
			want: settled(),
		},
		{
			name: "a blocking question named by ref is also a handback",
			lines: withClaim(
				toolUse("u2", askQuestionTool, `{"taskId":"`+claimRef+`","question":"which?","blocking":true}`),
				toolResult("u2", `{"taskBlocked":true}`),
			),
			want: settled(),
		},
		{
			name: "a NON-blocking question holds the task",
			lines: withClaim(
				toolUse("u2", askQuestionTool, `{"taskId":"`+claimUUID+`","question":"fyi","blocking":false}`),
				toolResult("u2", `{"taskBlocked":false}`),
			),
			want: heldClaim(),
		},
		{
			name: "a blocking question about someone else's task is not my handback",
			lines: withClaim(
				toolUse("u2", askQuestionTool, `{"taskId":"other-uuid","question":"q","blocking":true}`),
				toolResult("u2", `{"taskBlocked":true}`),
			),
			want: heldClaim(),
		},
		{
			name: "escalating a question is a handback",
			lines: withClaim(
				toolUse("u2", escalateQuestionTool, `{"questionId":"q-1"}`),
				toolResult("u2", `{"taskBlocked":true}`),
			),
			want: settled(),
		},
		{
			name: "recording a branch marks the claim as carrying pushed work",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"`+claimUUID+`","branch":"clanker/x"}`),
			),
			want: withWIP(),
		},
		{
			name: "a branch recorded by REF is still my pushed work",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"`+claimRef+`","branch":"clanker/x"}`),
			),
			want: withWIP(),
		},
		{
			name: "a predecessor's WIP arrives with the claim",
			lines: []string{
				toolUse("u1", claimTaskTool, `{"taskId":"`+claimUUID+`","takeover":true}`),
				toolResult("u1", `{"task":{"id":"`+claimUUID+`","ref":"`+claimRef+`"},"run":{"id":"r-1"},"hasWip":true}`),
			},
			want: Claim{TaskID: claimUUID, Ref: claimRef, RunID: "r-1", HasWIP: true},
		},
		{
			name: "another task's branch is not my WIP",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"t-99","branch":"clanker/other"}`),
			),
			want: heldClaim(),
		},
		{
			name: "closing a DIFFERENT task must not look like letting go of mine",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"t-99","status":"done"}`),
				toolResult("u2", `{}`),
			),
			want: heldClaim(),
		},
		{
			name: "a claim that lost the race records nothing to release",
			lines: []string{
				toolUse("u1", claimTaskTool, `{"taskId":"`+claimUUID+`"}`),
				toolResult("u1", `{"error":{"code":"already_claimed"}}`),
			},
			want: Claim{},
		},
		{
			name: "a REFUSED claim records nothing to release",
			lines: []string{
				toolUse("u1", claimTaskTool, `{"taskId":"`+claimUUID+`"}`),
				toolResultErr("u1", "human_only"),
			},
			want: Claim{},
		},
		{
			name:  "a session that claimed nothing",
			lines: []string{toolUse("u1", "Bash", `{"command":"ls"}`)},
			want:  Claim{},
		},
		{
			name: "the latest claim supersedes a settled earlier one",
			lines: withClaim(
				toolUse("u2", updateTaskTool, `{"taskId":"`+claimUUID+`","status":"in_review"}`),
				toolResult("u2", `{}`),
				toolUse("u3", claimTaskTool, `{"taskId":"t-2"}`),
				toolResult("u3", `{"task":{"id":"t-2","ref":"CLA-2"},"run":{"id":"r-2"}}`),
			),
			want: Claim{TaskID: "t-2", Ref: "CLA-2", RunID: "r-2"},
		},
		{
			name: "an unrelated tool_result must not be read as a claim",
			lines: []string{
				toolUse("u1", claimTaskTool, `{"taskId":"`+claimUUID+`"}`),
				toolResult("u2", claimOK),
			},
			want: Claim{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := claimStream(tt.lines...).Claim
			if got != tt.want {
				t.Errorf("Claim = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestClaimHeld(t *testing.T) {
	tests := []struct {
		name  string
		claim Claim
		want  bool
	}{
		{"unsettled claim is held", Claim{TaskID: "t-1", RunID: "r-1"}, true},
		{"settled claim is not", Claim{TaskID: "t-1", RunID: "r-1", Settled: true}, false},
		{"no claim at all", Claim{}, false},
		{"WIP does not stop it being HELD", Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.claim.Held(); got != tt.want {
				t.Errorf("Held() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClaimReleasable(t *testing.T) {
	tests := []struct {
		name  string
		claim Claim
		want  bool
	}{
		{"held with no WIP is the one releasable case", Claim{TaskID: "t-1", RunID: "r-1"}, true},
		// Releasing this to `ready` would discard requiresTakeover and strand the
		// pushed branch — worse than letting the lease expire.
		{"held WITH WIP must be left to expire", Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true}, false},
		{"settled: the plane already released it", Claim{TaskID: "t-1", RunID: "r-1", Settled: true}, false},
		{"nothing claimed", Claim{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.claim.Releasable(); got != tt.want {
				t.Errorf("Releasable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A tool_result whose content is a bare string, rather than an array of blocks —
// both shapes appear in the stream depending on the tool.
func TestClaudeClaimTracking_stringContent(t *testing.T) {
	b, _ := json.Marshal(claimOK)
	res := claimStream(
		toolUse("u1", claimTaskTool, `{"taskId":"t-1"}`),
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"u1","content":`+string(b)+`}]}}`,
	)
	if want := heldClaim(); res.Claim != want {
		t.Errorf("Claim = %+v, want %+v", res.Claim, want)
	}
}

func TestClaimNames(t *testing.T) {
	c := heldClaim()
	for _, tt := range []struct {
		id   string
		want bool
	}{
		{claimUUID, true},
		{claimRef, true},
		{"cla-242", true},
		{"CLA-243", false},
		{"other-uuid", false},
		{"", false},
	} {
		if got := c.Names(tt.id); got != tt.want {
			t.Errorf("Names(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
	// An empty claim names nothing — otherwise a session that never claimed would
	// match every id it touched.
	if (Claim{}).Names("anything") {
		t.Error("an empty claim must name nothing")
	}
}

func TestSettlesTask(t *testing.T) {
	// The plane's rule is "every status but in_progress clears the holder", so
	// this must not be an allowlist that a new status silently falls outside.
	for _, s := range []string{"done", "in_review", "parked", "blocked", "ready", "backlog", "some_future_status"} {
		if !settlesTask(s) {
			t.Errorf("settlesTask(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"in_progress", ""} {
		if settlesTask(s) {
			t.Errorf("settlesTask(%q) = true, want false", s)
		}
	}
}

// `input_tokens` is UNCACHED input only. Summing it with output alone misses the
// cache reads and writes that dominate a long agentic session, which is how a
// real run reported 140,387 tokens against $147.98 of spend — about $1.05 per
// thousand, no model's price. A max_tokens ceiling built on that number would
// silently pass roughly ten times what the operator set.
func TestClaudeCountsCacheTokens(t *testing.T) {
	line := `{"type":"result","subtype":"success","total_cost_usd":33.92,"usage":` +
		`{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":5000,"cache_read_input_tokens":900000}}`

	var res Result
	var console bytes.Buffer
	(claude{}).renderAndParse([]byte(line), &console, &res)

	if want := 905120; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d (cache reads and writes are billed input)", res.Tokens, want)
	}
}

// The non-streaming path parses the same envelope and must agree — it is the one
// used for probes and any non-stream invocation.
func TestClaudeParseCountsCacheTokens(t *testing.T) {
	res := Result{Stdout: `{"result":"done","total_cost_usd":1.5,"usage":` +
		`{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":1000,"cache_read_input_tokens":20000}}`}

	(claude{}).parse(&res)

	if want := 21015; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d", res.Tokens, want)
	}
}

func TestParseClaudeResetAt(t *testing.T) {
	madrid, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		t.Fatalf("load Madrid: %v", err)
	}
	// A fixed "now": 2026-07-16 08:00 in Madrid.
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, madrid)

	tests := []struct {
		name    string
		msg     string
		wantH   int
		wantM   int
		wantDay int          // day-of-month expected in Madrid
		wantWD  time.Weekday // only checked when non-negative
	}{
		{
			name:  "session later today, explicit zone",
			msg:   "You've hit your session limit · resets 9:40pm (Europe/Madrid)",
			wantH: 21, wantM: 40, wantDay: 16, wantWD: -1,
		},
		{
			name:  "session already passed rolls to tomorrow",
			msg:   "You've hit your session limit · resets 6:00am (Europe/Madrid)",
			wantH: 6, wantM: 0, wantDay: 17, wantWD: -1,
		},
		{
			name:  "weekly names a weekday",
			msg:   "You've hit your weekly limit · resets Sunday 12:00am (Europe/Madrid)",
			wantH: 0, wantM: 0, wantDay: 19, wantWD: time.Sunday, // next Sunday after Thu 16th
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseClaudeResetAt(tc.msg, now)
			if got.IsZero() {
				t.Fatalf("got zero time, want a parse")
			}
			if !got.After(now) {
				t.Errorf("reset %v is not after now %v", got, now)
			}
			g := got.In(madrid)
			if g.Hour() != tc.wantH || g.Minute() != tc.wantM {
				t.Errorf("clock = %02d:%02d, want %02d:%02d", g.Hour(), g.Minute(), tc.wantH, tc.wantM)
			}
			if g.Day() != tc.wantDay {
				t.Errorf("day = %d, want %d (%v)", g.Day(), tc.wantDay, g)
			}
			if tc.wantWD >= 0 && g.Weekday() != tc.wantWD {
				t.Errorf("weekday = %v, want %v", g.Weekday(), tc.wantWD)
			}
		})
	}
}

func TestClaudeIsTransient(t *testing.T) {
	cases := []struct {
		name string
		blob string
		want bool
	}{
		{"API 500", "API Error: 500 Internal Server Error", true},
		{"API 529 overloaded", `API Error: 529 {"type":"overloaded_error"}`, true},
		{"API 429", "API Error: 429 Too Many Requests", true},
		{"connection error", "Connection error.", true},
		{"econnreset", "read ECONNRESET", true},

		// The three documented "the response above may be incomplete" variants
		// (code.claude.com/docs/en/errors). Every one of them missed every arm
		// before CLA-268: "Connection closed" is not "connection error", "Server
		// error" is not "internal server", and a stalled stream names no status
		// code. A miss here stops the daemon rather than costing an iteration.
		{"connection closed mid-response", "API Error: Connection closed mid-response. The response above may be incomplete.", true},
		{"server error mid-response", "API Error: Server error mid-response. The response above may be incomplete.", true},
		{"response stalled mid-stream", "API Error: Response stalled mid-stream. The response above may be incomplete.", true},

		// Anchored: a task log mentioning an HTTP 500 without the API Error prefix
		// is NOT a dead session.
		{"task log mentions 500", "the endpoint returned HTTP 500 to the user", false},
		// The same anchoring has to hold for the new arm, or CLA-268's own body —
		// which quotes the wording verbatim — becomes a retry trigger.
		{"prose mentions mid-response without the prefix", "the stream was closed mid-response, per the bug report", false},
		// A 400 bad-request is a real failure — retrying won't help.
		{"API 400 stops", "API Error: 400 invalid request", false},
		// The subscription cap is handled by DetectLimit, not here.
		{"usage cap not transient", "You've hit your session limit · resets 9:40pm", false},
		{"clean", "done", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (claude{}).IsTransient(Result{Stdout: tc.blob}); got != tc.want {
				t.Errorf("IsTransient(%q) = %v, want %v", tc.blob, got, tc.want)
			}
		})
	}
}

// Where the mid-response notice ACTUALLY arrives in headless mode, and the gate
// that decides whether it is read at all.
//
// The bare-string cases above prove the regex; this proves the path. Claude
// Code's error reference says that with `--output-format json` or `stream-json`
// the notice is reported in the `result` field — which claudeText reads only
// when the CLI itself marked the session failed (claudeDiagnostic + ev.failed).
//
// Both halves matter and they are not the same claim:
//   - a FAILED result event carrying the notice must be retryable, or the daemon
//     stops on a network blip;
//   - a SUCCESSFUL one must not be, because on a clean finish `result` is the
//     agent's own closing summary, and reading it would hand a task body the
//     power to fake a transient failure all over again (CLA-258, decision
//     28b13387). A session that kept its partial output and exited zero is not
//     classified at all — the loop never asks.
func TestClaudeMidResponseNoticeInTheResultField(t *testing.T) {
	const notice = "API Error: Connection closed mid-response. The response above may be incomplete."

	failed := Result{ExitCode: 1, Stdout: `{"type":"result","subtype":"error_during_execution","is_error":true,` +
		`"result":` + mustJSON(t, notice) + `}`}
	if !(claude{}).IsTransient(failed) {
		t.Error("the documented mid-response notice, in the field the CLI actually puts it in, was judged non-retryable — the daemon would stop on a connection drop")
	}

	clean := Result{ExitCode: 0, Stdout: `{"type":"result","subtype":"success","is_error":false,` +
		`"result":` + mustJSON(t, "I finished. Note: "+notice) + `}`}
	if (claude{}).IsTransient(clean) {
		t.Error("a clean session's closing summary was read as a transient failure — the agent's narration is back inside the scan")
	}
}

// Diagnostic must report exactly the text IsTransient judged — no wider and no
// NARROWER. Both directions are regressions with teeth:
//
//   - wider (e.g. raw Stdout) prints a quoted task body to the operator's
//     terminal, which is CLA-258 arriving by a new door;
//   - narrower (e.g. claudeTyped) drops the `result` field of a failed session,
//     which is exactly where the CLI puts the mid-response notice — so the stop
//     message would come back EMPTY on the very failure this exists to explain.
//
// The narrowing case is the one to be careful about in a test: a fixture built
// from a CLEAN result event is dropped at both widths, so it cannot tell the two
// apart and would pass green against `claudeTyped`. Both cases below therefore
// use a FAILED event, which is the only shape the two widths disagree on.
func TestClaudeDiagnosticMatchesWhatIsTransientReads(t *testing.T) {
	const notice = "API Error: Connection closed mid-response. The response above may be incomplete."

	failed := Result{
		Stderr: "some stderr",
		Stdout: `{"type":"result","subtype":"error_during_execution","is_error":true,` +
			`"result":` + mustJSON(t, notice) + `}`,
	}
	got := (claude{}).Diagnostic(failed)
	if !strings.Contains(got, "some stderr") {
		t.Errorf("Diagnostic dropped stderr, which IsTransient reads: %q", got)
	}
	// The load-bearing assertion: this is the text the operator is shown when the
	// run stops, and it is only present at the diagnostic width.
	if !strings.Contains(got, notice) {
		t.Errorf("Diagnostic narrowed past the failed session's `result` — the stop message would name nothing: %q", got)
	}
	if got != claudeText(failed, claudeDiagnostic) {
		t.Errorf("Diagnostic scope drifted from the one IsTransient reads:\n got %q\nwant %q", got, claudeText(failed, claudeDiagnostic))
	}

	// A session the CLI did NOT call failed keeps its narration out, at every
	// width — the agent's closing summary is not the operator's diagnostic.
	clean := Result{
		Stderr: "some stderr",
		Stdout: `{"type":"result","subtype":"success","is_error":false,` +
			`"result":` + mustJSON(t, "the agent's closing summary") + `}`,
	}
	if g := (claude{}).Diagnostic(clean); strings.Contains(g, "closing summary") {
		t.Errorf("Diagnostic exposed the agent's narration, which IsTransient does not read: %q", g)
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal %q: %v", s, err)
	}
	return string(b)
}

func TestParseClaudeResetAt_unparseable(t *testing.T) {
	now := time.Now()
	for _, msg := range []string{
		"", "some unrelated output", "resets soon", "You've hit your limit",
	} {
		if got := parseClaudeResetAt(msg, now); !got.IsZero() {
			t.Errorf("%q: got %v, want zero", msg, got)
		}
	}
}
