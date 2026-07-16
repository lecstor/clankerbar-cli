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

func TestCodexParse_empty(t *testing.T) {
	res := Result{Stdout: ""}
	codex{}.parse(&res)
	if res.FinalMessage != "" || res.Tokens != 0 {
		t.Errorf("empty stream: got msg=%q tokens=%d, want empty/0", res.FinalMessage, res.Tokens)
	}
}

func TestCodexDetectLimit(t *testing.T) {
	cases := []struct {
		name string
		blob string
		want bool
	}{
		{"429 statusCode", `{"type":"turn.failed","error":{"statusCode":429,"message":"rate limited"}}`, true},
		{"usage limit prose", "You've hit your usage limit, try again in 4 hours", true},
		{"too many requests", "Error: 429 Too Many Requests", true},
		{"clean run", `{"type":"turn.completed","usage":{"input_tokens":10}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codex{}.DetectLimit(Result{Stdout: tc.blob}).Limited
			if got != tc.want {
				t.Errorf("DetectLimit = %v, want %v", got, tc.want)
			}
		})
	}
}
