package harness

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// CLA-288: the driver bounds consecutive attempts that reported NO usage, because
// those are the ones no spend ceiling can see. That makes "did this session report
// anything?" a question every adapter has to answer honestly, and it is NOT the
// same question as "did it spend anything?" - a session that reported zero has
// reported. Result.UsageReported is that answer; these pin it per adapter.

func TestClaudeMarksUsageReported(t *testing.T) {
	t.Run("the result event reports, even carrying zeros", func(t *testing.T) {
		line := `{"type":"result","subtype":"success","total_cost_usd":0,"usage":` +
			`{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`

		var res Result
		var console bytes.Buffer
		(claude{}).renderAndParse([]byte(line), &console, &res)

		if !res.UsageReported {
			t.Error("a result event carrying zero usage is still a REPORT of zero; counting it as silence would bound a session that legitimately did nothing")
		}
	})

	t.Run("a per-turn usage object reports before any result event arrives", func(t *testing.T) {
		// The killed-mid-stream shape: the session said what it had spent, and then
		// never reached its own end. It reported.
		line := `{"type":"assistant","message":{"content":[{"type":"text","text":"working"}],` +
			`"usage":{"input_tokens":100,"output_tokens":20}}}`

		var res Result
		var console bytes.Buffer
		(claude{}).renderAndParse([]byte(line), &console, &res)

		if !res.UsageReported {
			t.Error("a per-turn usage object is a usage-bearing event; a stream cut off after one must not look like silence")
		}
	})

	t.Run("a stream with no usage event at all reports nothing", func(t *testing.T) {
		// The bug's own shape: a session that died before the harness accounted for
		// anything. This is what the driver's bound counts.
		stream := `{"type":"system","subtype":"init"}` + "\n" +
			`{"type":"assistant","message":{"content":[{"type":"text","text":"starting"}]}}` + "\n"

		var res Result
		(claude{}).consume(strings.NewReader(stream), io.Discard, newTail(), &res, 0, func() {})

		if res.UsageReported {
			t.Error("no usage-bearing event arrived, so UsageReported must be false - this is exactly the attempt a spend ceiling cannot see")
		}
	})

	t.Run("a per-turn usage object of all zeros still reports", func(t *testing.T) {
		// A report of zero is a report. The token SUM ignores it (the result event
		// is authoritative for the total), which is why the flag has to be set on
		// the object's presence rather than inherited from that gate.
		line := `{"type":"assistant","message":{"content":[{"type":"text","text":"working"}],` +
			`"usage":{"input_tokens":0,"output_tokens":0}}}`

		var res Result
		var console bytes.Buffer
		(claude{}).renderAndParse([]byte(line), &console, &res)

		if !res.UsageReported {
			t.Error("a usage object carrying zeros is present, so it reported")
		}
		if res.Tokens != 0 {
			t.Errorf("Tokens = %d, want 0: a zero object must not change the sum", res.Tokens)
		}
	})

	t.Run("an envelope carrying no usage at all reports nothing", func(t *testing.T) {
		// The same envelope is how an ERROR comes back, and one with no accounting
		// in it must not claim a report of zero: this field is load-bearing now,
		// and a false report is a bound that can never fire.
		var res Result
		res.Stdout = `{"is_error":true,"result":"claude: command failed"}`
		(claude{}).parse(&res)

		if res.UsageReported {
			t.Error("no usage and no cost member is present; UsageReported must be false")
		}
	})

	t.Run("the non-streaming envelope reports", func(t *testing.T) {
		var res Result
		res.Stdout = `{"result":"done","total_cost_usd":0.5,"usage":{"input_tokens":10,"output_tokens":2}}`
		(claude{}).parse(&res)

		if !res.UsageReported {
			t.Error("the probe/json path parses the same accounting envelope and must agree")
		}
	})
}

func TestCodexMarksUsageReported(t *testing.T) {
	t.Run("a turn.completed reports", func(t *testing.T) {
		if res := codexParsed(codexStream); !res.UsageReported {
			t.Error("the stream ends on a turn.completed carrying usage; UsageReported must be true")
		}
	})

	t.Run("a stream that never reports usage says so", func(t *testing.T) {
		stream := `{"type":"thread.started","thread_id":"t1"}` + "\n" +
			`{"type":"item.completed","item":{"type":"agent_message","text":"died early"}}`
		if res := codexParsed(stream); res.UsageReported {
			t.Error("no usage-bearing event arrived; UsageReported must be false")
		}
	})
}

// The capability the doctor's cost warning reads. It is asserted per adapter
// rather than in a table, because the failure this guards against is a NEW
// adapter whose author copies a neighbour's line: false here is a statement that
// `budget.max_cost_usd` cannot fire, and getting it wrong the optimistic way
// silently reinstates the inert ceiling CLA-288 exists to surface.
func TestReportsCostPerAdapter(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
		why  string
	}{
		{"claude", true, "total_cost_usd rides on the result event"},
		{"opencode", true, "each step_finish part carries a cost sibling"},
		{"codex", false, "codex exec --json reports tokens, never money"},
	} {
		caps, ok := CapabilitiesOf(tc.name)
		if !ok {
			t.Fatalf("%s is not registered", tc.name)
		}
		if caps.ReportsCost != tc.want {
			t.Errorf("%s ReportsCost = %v, want %v (%s)", tc.name, caps.ReportsCost, tc.want, tc.why)
		}
	}
}
