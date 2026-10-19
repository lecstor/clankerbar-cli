package harness

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// CLA-691: every adapter normalises its own usage report onto the CLI's five
// disjoint buckets (Result.Usage), because that is the durable, repriceable form
// of what Tokens only totals. The mapping is per-adapter by MEANING, never by
// field name, so these pin each adapter's own semantics:
//
//   - claude reports input exclusive of the cache fields, and folds reasoning
//     into output_tokens;
//   - opencode's step tokens are already disjoint;
//   - codex reports INCLUSIVE counters (cached ⊂ input, reasoning ⊂ output), so
//     copying them field-for-field would double count both.
//
// The buckets must also SUM to the figure the harness itself reported — that is
// the property that makes repricing possible — so each case asserts the total,
// not only the buckets.

func TestClaudeNormalisedUsage(t *testing.T) {
	t.Run("the cumulative result event overwrites the per-turn sums", func(t *testing.T) {
		// A two-turn stream: each assistant event carries its own turn's usage,
		// and the result event carries the session's cumulative total. Summing
		// the two would double the session; the result event wins exactly as it
		// does for Tokens.
		stream := `{"type":"assistant","message":{"content":[{"type":"text","text":"working"}],` +
			`"usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":5000,"cache_read_input_tokens":900000}}}` + "\n" +
			`{"type":"assistant","message":{"content":[{"type":"text","text":"still working"}],` +
			`"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":1000,"cache_read_input_tokens":20000}}}` + "\n" +
			`{"type":"result","subtype":"success","result":"done","total_cost_usd":0.5,` +
			`"usage":{"input_tokens":110,"output_tokens":25,"cache_creation_input_tokens":6000,"cache_read_input_tokens":920000}}`

		var res Result
		(claude{}).consume(strings.NewReader(stream), &bytes.Buffer{}, newTail(), &res, 0, func() {})

		want := TokenUsage{Input: 110, Output: 25, CacheRead: 920000, CacheWrite: 6000}
		if res.Usage != want {
			t.Errorf("Usage = %+v, want %+v — the result event is CUMULATIVE and replaces the per-turn sum, as it does for Tokens", res.Usage, want)
		}
		if res.Usage.Total() != res.Tokens {
			t.Errorf("Usage.Total() = %d, Tokens = %d — the buckets must sum to the figure the harness reported", res.Usage.Total(), res.Tokens)
		}
	})

	t.Run("a stream cut off before its result event keeps the per-turn sums", func(t *testing.T) {
		// The ceiling-kill shape: two turns reported, the session's own end never
		// arrived. The parts are what the session spent, and Tokens sums them the
		// same way.
		stream := `{"type":"assistant","message":{"content":[{"type":"text","text":"a"}],"usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":5000,"cache_read_input_tokens":900000}}}` + "\n" +
			`{"type":"assistant","message":{"content":[{"type":"text","text":"b"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":1000,"cache_read_input_tokens":20000}}}`

		var res Result
		(claude{}).consume(strings.NewReader(stream), &bytes.Buffer{}, newTail(), &res, 0, func() {})

		want := TokenUsage{Input: 110, Output: 25, CacheRead: 920000, CacheWrite: 6000}
		if res.Usage != want {
			t.Errorf("Usage = %+v, want %+v — a session killed mid-stream reports the parts it did account for", res.Usage, want)
		}
		if res.Usage.Total() != res.Tokens {
			t.Errorf("Usage.Total() = %d, Tokens = %d — the sum must match the figure the same events produced", res.Usage.Total(), res.Tokens)
		}
	})

	t.Run("no usage event leaves every bucket zero", func(t *testing.T) {
		stream := `{"type":"system","subtype":"init"}` + "\n"

		var res Result
		(claude{}).consume(strings.NewReader(stream), &bytes.Buffer{}, newTail(), &res, 0, func() {})

		if res.Usage != (TokenUsage{}) {
			t.Errorf("Usage = %+v, want the zero value when no usage-bearing event arrived", res.Usage)
		}
	})
}

func TestOpencodeNormalisedUsageSumsTheSteps(t *testing.T) {
	// opencode's per-step tokens are already disjoint, so the mapping is the
	// report itself summed over the session — including reasoning, which opencode
	// counts as its own bucket.
	const stream = `{"type":"step_finish","part":{"type":"step-finish","reason":"tool-calls","tokens":{"total":22588,"input":22259,"output":130,"reasoning":199,"cache":{"write":0,"read":0}},"cost":0.4}}
{"type":"step_finish","part":{"type":"step-finish","reason":"tool-calls","tokens":{"total":31119,"input":8498,"output":41,"reasoning":52,"cache":{"write":0,"read":22528}},"cost":0.5}}`
	res := opencodeParsed(stream)

	want := TokenUsage{Input: 22259 + 8498, Output: 130 + 41, Reasoning: 199 + 52, CacheRead: 22528, CacheWrite: 0}
	if res.Usage != want {
		t.Errorf("Usage = %+v, want %+v — one bucket per reported field, summed across steps", res.Usage, want)
	}
	if res.Usage.Total() != res.Tokens {
		t.Errorf("Usage.Total() = %d, Tokens = %d — opencode's own total is the sum of these buckets", res.Usage.Total(), res.Tokens)
	}
}

func TestOpencodeNormalisedUsageFromTheSessionExport(t *testing.T) {
	// 2.x's stream omits the final step, so settleUsage replaces the stream sums
	// with the session export's figures. The breakdown comes from exactly those
	// figures — the two must never disagree about what the session spent.
	doc := exportDoc(t, 3*18511, 0.3,
		assistantMsg(1100, "tool-calls", 18511, 2, 0, 0.1),
		assistantMsg(1200, "stop", 18511, 2, 0, 0.1))
	doc.Info.Tokens.Output = 6
	// The export's own totals must include the cached read the messages carry,
	// or the session-overhead delta cancels it back out.
	doc.Info.Tokens.Cache.Read = 500
	doc.Messages[0].Tokens.Cache.Read = 500

	var res Result
	applyOpencodeExport(&res, doc, time.UnixMilli(1000), false)

	want := TokenUsage{Input: 3 * 18511, Output: 6, CacheRead: 500}
	if res.Usage != want {
		t.Errorf("Usage = %+v, want %+v — the breakdown comes from the same export figures the budget totals were computed from", res.Usage, want)
	}
	if res.Usage.Total() != res.Tokens {
		t.Errorf("Usage.Total() = %d, Tokens = %d — the buckets must sum to the export's total", res.Usage.Total(), res.Tokens)
	}
}

func TestCodexNormalisedUsageSubtractsTheInclusiveCounters(t *testing.T) {
	// codex reports cached_input_tokens INSIDE input_tokens and
	// reasoning_output_tokens INSIDE output_tokens. A field-for-field copy would
	// double count both, so the subsets are peeled out: the five buckets then sum
	// to codex's own total_tokens (input + output), which is the repriceable
	// property this test exists for.
	const stream = `{"type":"turn.completed","usage":{"input_tokens":1200,"cached_input_tokens":800,"output_tokens":300,"reasoning_output_tokens":50}}`
	p := &codexParse{}
	p.line([]byte(stream))
	var res Result
	p.finish(&res)

	want := TokenUsage{Input: 400, Output: 250, Reasoning: 50, CacheRead: 800}
	if res.Usage != want {
		t.Errorf("Usage = %+v, want %+v — cached reads come OUT of input and reasoning OUT of output, so no token is counted twice", res.Usage, want)
	}
	if got, want := res.Usage.Total(), 1200+300; got != want {
		t.Errorf("Usage.Total() = %d, want %d (codex's own total_tokens = input + output)", got, want)
	}
}

func TestMergeResumeSumsTheContinuationsUsage(t *testing.T) {
	// A session that spanned a stall continuation is ONE session, so its parts
	// sum — the same rule Tokens and CostUSD already follow (CLA-584). The
	// buckets must not be replaced by the continuation's, or a resumed session
	// would report only its last turn.
	base := Result{
		Tokens: 100,
		Usage:  TokenUsage{Input: 60, Output: 20, CacheRead: 20},
	}
	add := Result{
		Tokens: 50,
		Usage:  TokenUsage{Input: 10, Output: 30, Reasoning: 10},
	}

	mergeResume(&base, add)

	want := TokenUsage{Input: 70, Output: 50, Reasoning: 10, CacheRead: 20}
	if base.Usage != want {
		t.Errorf("Usage = %+v, want %+v", base.Usage, want)
	}
	if base.Usage.Total() != base.Tokens {
		t.Errorf("Usage.Total() = %d, Tokens = %d — the buckets and the budget figure describe the same two runs", base.Usage.Total(), base.Tokens)
	}
}
