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

const claimOK = `{"task":{"id":"t-1","ref":"CLA-1"},"run":{"id":"r-1"},"branch":"clanker/x"}`

func TestClaudeClaimTracking(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  Claim
	}{
		{
			name: "claimed and never settled — the lease the driver must hand back",
			lines: []string{
				toolUse("u1", claimTaskTool, `{"taskId":"t-1"}`),
				toolResult("u1", claimOK),
			},
			want: Claim{TaskID: "t-1", RunID: "r-1"},
		},
		{
			name: "settled at in_review — the plane already released it",
			lines: []string{
				toolUse("u1", claimTaskTool, `{"taskId":"t-1"}`),
				toolResult("u1", claimOK),
				toolUse("u2", updateTaskTool, `{"taskId":"t-1","runId":"r-1","status":"in_review"}`),
			},
			want: Claim{TaskID: "t-1", RunID: "r-1", Settled: true},
		},
		{
			name: "a status-less revision of the held task leaves the claim standing",
			lines: []string{
				toolUse("u1", claimTaskTool, `{"taskId":"t-1"}`),
				toolResult("u1", claimOK),
				toolUse("u2", updateTaskTool, `{"taskId":"t-1","outcome":"still going"}`),
			},
			want: Claim{TaskID: "t-1", RunID: "r-1"},
		},
		{
			name: "recording a branch marks the claim as carrying pushed work",
			lines: []string{
				toolUse("u1", claimTaskTool, `{"taskId":"t-1"}`),
				toolResult("u1", claimOK),
				toolUse("u2", updateTaskTool, `{"taskId":"t-1","branch":"clanker/x"}`),
			},
			want: Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true},
		},
		{
			name: "a predecessor's WIP arrives with the claim",
			lines: []string{
				toolUse("u1", claimTaskTool, `{"taskId":"t-1","takeover":true}`),
				toolResult("u1", `{"task":{"id":"t-1"},"run":{"id":"r-1"},"hasWip":true}`),
			},
			want: Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true},
		},
		{
			name: "another task's branch is not my WIP",
			lines: []string{
				toolUse("u1", claimTaskTool, `{"taskId":"t-1"}`),
				toolResult("u1", claimOK),
				toolUse("u2", updateTaskTool, `{"taskId":"t-99","branch":"clanker/other"}`),
			},
			want: Claim{TaskID: "t-1", RunID: "r-1"},
		},
		{
			name: "closing a DIFFERENT task must not look like letting go of mine",
			lines: []string{
				toolUse("u1", claimTaskTool, `{"taskId":"t-1"}`),
				toolResult("u1", claimOK),
				toolUse("u2", updateTaskTool, `{"taskId":"t-99","status":"done"}`),
			},
			want: Claim{TaskID: "t-1", RunID: "r-1"},
		},
		{
			name: "a claim that lost the race records nothing to release",
			lines: []string{
				toolUse("u1", claimTaskTool, `{"taskId":"t-1"}`),
				toolResult("u1", `{"error":{"code":"already_claimed"}}`),
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
			lines: []string{
				toolUse("u1", claimTaskTool, `{"taskId":"t-1"}`),
				toolResult("u1", claimOK),
				toolUse("u2", updateTaskTool, `{"taskId":"t-1","status":"in_review"}`),
				toolUse("u3", claimTaskTool, `{"taskId":"t-2"}`),
				toolResult("u3", `{"task":{"id":"t-2"},"run":{"id":"r-2"}}`),
			},
			want: Claim{TaskID: "t-2", RunID: "r-2"},
		},
		{
			name: "an unrelated tool_result must not be read as a claim",
			lines: []string{
				toolUse("u1", claimTaskTool, `{"taskId":"t-1"}`),
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
	if want := (Claim{TaskID: "t-1", RunID: "r-1"}); res.Claim != want {
		t.Errorf("Claim = %+v, want %+v", res.Claim, want)
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
		// Anchored: a task log mentioning an HTTP 500 without the API Error prefix
		// is NOT a dead session.
		{"task log mentions 500", "the endpoint returned HTTP 500 to the user", false},
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
