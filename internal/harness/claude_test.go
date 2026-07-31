package harness

import (
	"bytes"
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
			name:    "session later today, explicit zone",
			msg:     "You've hit your session limit · resets 9:40pm (Europe/Madrid)",
			wantH:   21, wantM: 40, wantDay: 16, wantWD: -1,
		},
		{
			name:    "session already passed rolls to tomorrow",
			msg:     "You've hit your session limit · resets 6:00am (Europe/Madrid)",
			wantH:   6, wantM: 0, wantDay: 17, wantWD: -1,
		},
		{
			name:    "weekly names a weekday",
			msg:     "You've hit your weekly limit · resets Sunday 12:00am (Europe/Madrid)",
			wantH:   0, wantM: 0, wantDay: 19, wantWD: time.Sunday, // next Sunday after Thu 16th
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
