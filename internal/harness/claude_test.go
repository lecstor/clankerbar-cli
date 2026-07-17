package harness

import (
	"testing"
	"time"
)

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
