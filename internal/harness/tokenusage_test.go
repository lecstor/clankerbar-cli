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

// A session's TOKENS and its DOLLARS are reported by different events on claude:
// per-turn assistant usage arrives on the stream, the session's own accounting
// (total_cost_usd) only on the terminal result event. CostReported is what tells
// those apart, and it is what lets the durable record carry "unknown" instead of
// a $0 nobody measured (CLA-691).
func TestClaudeReportsWhetherItSawACostFigure(t *testing.T) {
	t.Run("a stream killed before its result event reported tokens and no cost", func(t *testing.T) {
		stream := `{"type":"assistant","message":{"content":[{"type":"text","text":"a"}],` +
			`"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":900000}}}`

		var res Result
		(claude{}).consume(strings.NewReader(stream), &bytes.Buffer{}, newTail(), &res, 0, func() {})

		if !res.UsageReported {
			t.Error("UsageReported = false; the per-turn usage event is a report")
		}
		if res.CostReported {
			t.Error("CostReported = true, but no event carried a cost figure — a 0 from this session would claim a spend nobody measured")
		}
	})

	t.Run("a result event carrying zero cost IS a figure", func(t *testing.T) {
		line := `{"type":"result","subtype":"success","total_cost_usd":0,"usage":` +
			`{"input_tokens":10,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`

		var res Result
		(claude{}).renderAndParse([]byte(line), &bytes.Buffer{}, &res)

		if !res.CostReported || res.CostUSD != 0 {
			t.Errorf("CostReported = %v (cost %v), want a reported zero — a free session is a figure, not silence", res.CostReported, res.CostUSD)
		}
	})

	t.Run("the probe path agrees", func(t *testing.T) {
		var withCost Result
		withCost.Stdout = `{"result":"done","total_cost_usd":0.5,"usage":` +
			`{"input_tokens":10,"output_tokens":2,"cache_creation_input_tokens":1000,"cache_read_input_tokens":20000}}`
		(claude{}).parse(&withCost)
		if !withCost.CostReported {
			t.Error("the non-streaming envelope carries a cost figure; CostReported must be true")
		}
		if want := (TokenUsage{Input: 10, Output: 2, CacheRead: 20000, CacheWrite: 1000}); withCost.Usage != want {
			t.Errorf("Usage = %+v, want %+v — the probe path must normalise like the stream path", withCost.Usage, want)
		}

		var noCost Result
		noCost.Stdout = `{"is_error":true,"result":"claude: command failed"}`
		(claude{}).parse(&noCost)
		if noCost.CostReported || noCost.Usage != (TokenUsage{}) {
			t.Errorf("an envelope with no accounting must report neither usage nor cost: %v / %+v", noCost.CostReported, noCost.Usage)
		}
	})
}

func TestOpencodeReportsWhetherItSawACostFigure(t *testing.T) {
	// opencode reports a step's tokens and its cost as independent siblings, so a
	// stream can account for tokens and never for money.
	const noCost = `{"type":"step_finish","part":{"type":"step-finish","reason":"stop","tokens":{"total":100,"input":40,"output":10,"reasoning":0,"cache":{"write":50,"read":0}}}}`
	res := opencodeParsed(noCost)
	if !res.UsageReported || res.CostReported {
		t.Errorf("usageReported=%v costReported=%v, want tokens reported and no cost figure", res.UsageReported, res.CostReported)
	}

	const withCost = `{"type":"step_finish","part":{"type":"step-finish","reason":"stop","tokens":{"total":100,"input":40,"output":10,"reasoning":0,"cache":{"write":50,"read":0}},"cost":0.02}}`
	res = opencodeParsed(withCost)
	if !res.CostReported || res.CostUSD != 0.02 {
		t.Errorf("costReported=%v cost=%v, want the reported figure", res.CostReported, res.CostUSD)
	}
}

// A codex session that reports cached > input or reasoning > output violates the
// inclusive-counting premise the subtraction rests on. The clamp keeps a nonsense
// NEGATIVE out of the durable record; this pins that it does, so a future change
// to the mapping is a deliberate one.
func TestCodexClampsASubsetLargerThanItsTotal(t *testing.T) {
	const stream = `{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":800,"output_tokens":50,"reasoning_output_tokens":90}}`
	p := &codexParse{}
	p.line([]byte(stream))
	var res Result
	p.finish(&res)

	if res.Usage.Input != 0 || res.Usage.Output != 0 {
		t.Errorf("Usage = %+v, want the impossible buckets clamped at 0 rather than going negative", res.Usage)
	}
	if res.Usage.CacheRead != 800 || res.Usage.Reasoning != 90 {
		t.Errorf("Usage = %+v, want the reported cached and reasoning counts intact", res.Usage)
	}
}

// codex's BUDGET figure still adds reasoning to output, which double counts it
// (reasoning_output_tokens is a subset of output_tokens). The normalised buckets
// do NOT — they are the disjoint breakdown — so the two figures legitimately
// differ for codex alone, and CLA-692 carries the budget-side fix. Pinned here so
// the divergence is visible and deliberate rather than an unnoticed drift.
func TestCodexBudgetFigureAndBucketsDifferUntilCLA692(t *testing.T) {
	const stream = `{"type":"turn.completed","usage":{"input_tokens":1200,"cached_input_tokens":800,"output_tokens":300,"reasoning_output_tokens":50}}`
	p := &codexParse{}
	p.line([]byte(stream))
	var res Result
	p.finish(&res)

	if want := 1200 + 300; res.Usage.Total() != want {
		t.Errorf("Usage.Total() = %d, want %d (codex's own total_tokens = input + output)", res.Usage.Total(), want)
	}
	if want := 1200 + 300 + 50; res.Tokens != want {
		t.Errorf("Tokens = %d, want %d — the budget figure's historical composition", res.Tokens, want)
	}
	if res.Tokens-res.Usage.Total() != 50 {
		t.Errorf("the gap between the two figures is %d, want exactly reasoning_output_tokens (50)", res.Tokens-res.Usage.Total())
	}
	if !res.UsageReported || res.CostReported {
		t.Errorf("usageReported=%v costReported=%v, want tokens reported and no cost (codex reports no money)", res.UsageReported, res.CostReported)
	}
}
