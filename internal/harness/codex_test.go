package harness

import "testing"

// A representative `codex exec --json` stream (documented shape; the parser is
// defensive so exact fidelity to the live schema is not required). Interleaves an
// unrelated event, a non-JSON line, an agent message item, and a final
// turn.completed carrying cumulative usage.
const codexStream = `{"type":"thread.started","thread_id":"t1"}
{"type":"turn.started"}
not json, should be skipped
{"type":"item.completed","item":{"type":"agent_message","text":"Backlog drained."}}
{"type":"token_count","input_tokens":10,"output_tokens":5}
{"type":"turn.completed","usage":{"input_tokens":1200,"cached_input_tokens":800,"output_tokens":300,"reasoning_output_tokens":50}}`

func TestCodexParse(t *testing.T) {
	res := Result{Stdout: codexStream}
	codex{}.parse(&res)

	if res.FinalMessage != "Backlog drained." {
		t.Errorf("FinalMessage = %q, want %q", res.FinalMessage, "Backlog drained.")
	}
	// Last usage-bearing event is turn.completed: input(1200)+output(300)+reasoning(50),
	// cached excluded.
	if want := 1200 + 300 + 50; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d", res.Tokens, want)
	}
}

// A multi-turn `codex exec --json` stream. Each `turn.completed.usage` is the
// CUMULATIVE session total (codex 0.144.6 / rust-v0.144.6), so the per-run total
// must be the LAST event's usage, NOT the sum of the two. This locks the parser
// against a regression to delta-summing / double-counting.
const codexCumulativeStream = `{"type":"thread.started","thread_id":"t1"}
{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":200,"output_tokens":100,"reasoning_output_tokens":50}}
{"type":"item.completed","item":{"type":"agent_message","text":"working"}}
{"type":"turn.completed","usage":{"input_tokens":3000,"cached_input_tokens":900,"output_tokens":400,"reasoning_output_tokens":120}}`

func TestCodexParse_cumulativeNotSummed(t *testing.T) {
	res := Result{Stdout: codexCumulativeStream}
	codex{}.parse(&res)

	// Correct: last turn.completed only — input(3000)+output(400)+reasoning(120),
	// cached excluded.
	last := 3000 + 400 + 120
	if res.Tokens != last {
		t.Errorf("Tokens = %d, want %d (last cumulative total, not the sum)", res.Tokens, last)
	}
	// Guard the specific regression: summing both turns would double-count.
	summed := (1000 + 100 + 50) + (3000 + 400 + 120)
	if res.Tokens == summed {
		t.Errorf("Tokens = %d equals the summed total; codex usage is cumulative and must not be summed", res.Tokens)
	}
}

func TestCodexParse_empty(t *testing.T) {
	res := Result{Stdout: ""}
	codex{}.parse(&res)
	if res.FinalMessage != "" || res.Tokens != 0 {
		t.Errorf("empty stream: got msg=%q tokens=%d, want empty/0", res.FinalMessage, res.Tokens)
	}
}

func TestCodexDetectLimit(t *testing.T) {
	// DetectLimit is the SUBSCRIPTION cap only; API 429s are transient (below).
	cases := []struct {
		name string
		blob string
		want bool
	}{
		{"usage limit prose", "You've hit your usage limit, try again in 4 hours", true},
		{"weekly limit", "you've hit your weekly limit", true},
		{"429 is transient not the cap", `{"type":"turn.failed","error":{"statusCode":429}}`, false},
		{"clean run", `{"type":"turn.completed","usage":{"input_tokens":10}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (codex{}).DetectLimit(Result{Stdout: tc.blob}).Limited; got != tc.want {
				t.Errorf("DetectLimit = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCodexIsTransient(t *testing.T) {
	cases := []struct {
		name string
		blob string
		want bool
	}{
		{"429 statusCode", `{"type":"turn.failed","error":{"statusCode":429}}`, true},
		{"5xx statusCode", `{"error":{"statusCode":503}}`, true},
		{"too many requests", "Error: 429 Too Many Requests", true},
		{"connection blip", "fetch failed", true},
		{"overloaded", "provider overloaded", true},
		{"subscription cap is not transient", "you've hit your usage limit", false},
		{"clean run", `{"type":"turn.completed","usage":{"input_tokens":10}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (codex{}).IsTransient(Result{Stdout: tc.blob}); got != tc.want {
				t.Errorf("IsTransient = %v, want %v", got, tc.want)
			}
		})
	}
}
