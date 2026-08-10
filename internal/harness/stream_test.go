package harness

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"
)

// filler yields n bytes of the same character, so a test can feed a 16 MiB line to
// a scanner without allocating one.
type filler struct {
	c    byte
	left int
}

func (f *filler) Read(p []byte) (int, error) {
	if f.left <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > f.left {
		n = f.left
	}
	for i := 0; i < n; i++ {
		p[i] = f.c
	}
	f.left -= n
	return n, nil
}

// drainedReader records whether it was read all the way to EOF — which is the
// whole question behind the EPIPE half of CLA-262.
type drainedReader struct {
	r      io.Reader
	atEOF  bool
	nRead  int64
	closed bool
}

func (d *drainedReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	d.nRead += int64(n)
	if err == io.EOF {
		d.atEOF = true
	}
	return n, err
}

// A stream-json line above the scanner's cap used to end the read loop with
// bufio.ErrTooLong and nothing looked at it. Every consequence was a confident
// WRONG decision by the supervisor rather than a missing one (CLA-262):
//
//   - the `result` event, which is the only place a claude session's total tokens
//     and cost appear, is after the break — so the session reports ZERO SPEND;
//   - a claim's settle may be after the break too, so the driver reads a task in
//     review as one that was abandoned;
//   - and abandoning the pipe means cmd.Wait closes our end, the child dies on
//     EPIPE, and its exit code reads as a genuine non-retryable failure.
//
// So: the error is checked, said out loud, recorded on the Result, and the pipe is
// drained to the end.
func TestClaudeOversizedLineIsReportedNotSwallowed(t *testing.T) {
	// A real session: it claims a task, then one tool_result carries a file read
	// bigger than the line cap, and the CLI carries on writing afterwards —
	// including the result event with the whole session's spend on it.
	before := strings.Join(withClaim(), "\n") + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"u9","name":"Read","input":{}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"u9","content":"`
	after := "\n" + toolUse("u2", updateTaskTool, `{"taskId":"`+claimUUID+`","status":"in_review"}`) + "\n" +
		toolResult("u2", `{"task":{"id":"`+claimUUID+`"}}`) + "\n" +
		`{"type":"result","subtype":"success","total_cost_usd":312.50,"usage":{"input_tokens":900000,"output_tokens":40000}}` + "\n"

	stream := &drainedReader{r: io.MultiReader(
		strings.NewReader(before),
		&filler{c: 'x', left: maxStreamLine + 1},
		strings.NewReader(after),
	)}

	var console bytes.Buffer
	keep := newTail()
	res := Result{scans: newScanCache()}
	(claude{}).consume(stream, &console, keep, &res)

	if res.Untrusted == "" {
		t.Fatal("Untrusted is empty after a line above the cap — the truncation is silent, which is the whole defect")
	}
	if !strings.Contains(res.Untrusted, "could not be read to the end") {
		t.Errorf("Untrusted = %q, want it to say the stream was not read whole", res.Untrusted)
	}

	// Drained, not abandoned: everything after the oversized line was consumed to
	// EOF, so the child finishes its own write instead of dying on EPIPE.
	if !stream.atEOF {
		t.Errorf("the stream was abandoned after %d bytes — cmd.Wait would close the pipe and the child would die on EPIPE", stream.nRead)
	}

	// And the figures really are incomplete, which is what Untrusted is claiming:
	// the result event is on the far side of the break.
	if res.CostUSD != 0 || res.Tokens != 0 {
		t.Errorf("spend = %d tokens / $%v: the fixture puts the result event AFTER the break, so a non-zero figure here means the test no longer covers what it says", res.Tokens, res.CostUSD)
	}
	// The claim was seen; its settle was not. This is exactly the reading the
	// driver must refuse to act on.
	if !res.Claim.Held() {
		t.Error("the claim made BEFORE the break should still have been parsed")
	}

	// Said out loud, on the console — which is also the iteration logfile.
	out := console.String()
	for _, want := range []string{"stream read failed", "draining", "NOT trustworthy"} {
		if !strings.Contains(out, want) {
			t.Errorf("console missing %q:\n%s", want, lastBytes(out, 400))
		}
	}
	if !strings.Contains(out, "discarded") {
		t.Errorf("console does not say how much was discarded:\n%s", lastBytes(out, 400))
	}
}

// The ordinary path must be untouched: a whole stream parses as it always did, is
// retained in full, and is trusted.
func TestClaudeConsumeCleanStream(t *testing.T) {
	stream := strings.Join(withClaim(
		`{"type":"result","subtype":"success","result":"done","total_cost_usd":0.5,"usage":{"input_tokens":10,"output_tokens":2}}`,
	), "\n") + "\n"

	var console bytes.Buffer
	keep := newTail()
	res := Result{scans: newScanCache()}
	(claude{}).consume(strings.NewReader(stream), &console, keep, &res)

	if res.Untrusted != "" {
		t.Errorf("Untrusted = %q on a clean stream", res.Untrusted)
	}
	if res.Tokens != 12 || res.CostUSD != 0.5 {
		t.Errorf("spend = %d tokens / $%v, want 12 / $0.5", res.Tokens, res.CostUSD)
	}
	if got := keep.String(); got != stream {
		t.Errorf("retained text is not the stream:\n%q", got)
	}
	if keep.Dropped() != 0 {
		t.Errorf("Dropped() = %d on a stream well under the cap", keep.Dropped())
	}
}

// The memory half of CLA-262: a session that emits far more than the cap must
// leave the supervisor holding no more than the cap, and the spend parsed off the
// stream must not depend on how much of it was retained — the events are read as
// they pass.
func TestClaudeRetainedOutputIsBounded(t *testing.T) {
	// ~16 MiB of narration — eight times the cap — then the result event.
	const narrationLines = 2048
	narration := `{"type":"assistant","message":{"content":[{"type":"text","text":"` +
		strings.Repeat("filling the buffer. ", 400) + `"}]}}` + "\n"
	var stream strings.Builder
	for i := 0; i < narrationLines; i++ {
		stream.WriteString(narration)
	}
	stream.WriteString(`{"type":"result","subtype":"success","total_cost_usd":7.25,"usage":{"input_tokens":1000,"output_tokens":100}}` + "\n")

	keep := newTail()
	res := Result{scans: newScanCache()}
	(claude{}).consume(strings.NewReader(stream.String()), io.Discard, keep, &res)

	if res.Untrusted != "" {
		t.Errorf("Untrusted = %q: every line here is well under the line cap, so trimming the RETAINED text must not mark the run untrusted", res.Untrusted)
	}
	if len(keep.buf) > retainedTail {
		t.Errorf("retained %d bytes of a %d-byte stream, want <= %d", len(keep.buf), stream.Len(), retainedTail)
	}
	if keep.Dropped() == 0 {
		t.Fatalf("nothing dropped from a %d-byte stream through a %d-byte window — the fixture is too small to test the cap", stream.Len(), retainedTail)
	}
	// Parsed live, so the spend survives the trimming that threw the narration away.
	if res.Tokens != 1100 || res.CostUSD != 7.25 {
		t.Errorf("spend = %d tokens / $%v, want 1100 / $7.25 — usage is parsed as the stream passes, not from the retained tail", res.Tokens, res.CostUSD)
	}
	// And what survived is the END, where every classification's answer lives.
	if !strings.Contains(keep.String(), `"type":"result"`) {
		t.Error("the retained tail does not include the end of the stream")
	}
}

// codex and opencode retain a bounded tail too, and their token accounting must
// survive it. opencode's is the load-bearing case: it SUMS per-step usage, so a
// parser reading a capped copy of stdout would silently lose the early steps.
func TestCodexAndOpencodeRetentionIsBoundedWithoutLosingSpend(t *testing.T) {
	t.Run("opencode sums every step, retained or not", func(t *testing.T) {
		var stream strings.Builder
		const steps = 400
		filler := `{"type":"text","part":{"type":"text","text":"` + strings.Repeat("padding ", 1000) + `"}}` + "\n"
		for i := 0; i < steps; i++ {
			stream.WriteString(filler)
			stream.WriteString(`{"type":"step_finish","part":{"type":"step-finish","tokens":{"total":1000,"input":10,"output":10,"reasoning":0,"cache":{"write":980,"read":0}},"cost":0.01}}` + "\n")
		}

		keep := &tail{max: 4096} // a deliberately tiny window, to make the point in a fast test
		var p opencodeParse
		sink := newLineSink(p.line)
		if _, err := io.Copy(io.MultiWriter(keep, sink), strings.NewReader(stream.String())); err != nil {
			t.Fatalf("copy: %v", err)
		}
		sink.Flush()
		var res Result
		p.finish(&res)

		if want := steps * 1000; res.Tokens != want {
			t.Errorf("Tokens = %d, want %d — every step must be counted, however little of the stream is kept", res.Tokens, want)
		}
		if len(keep.buf) > keep.max {
			t.Errorf("retained %d bytes of a %d-byte stream, want <= %d", len(keep.buf), stream.Len(), keep.max)
		}
	})

	t.Run("codex keeps the last cumulative total, retained or not", func(t *testing.T) {
		var stream strings.Builder
		filler := `{"type":"item.completed","item":{"type":"agent_message","text":"` + strings.Repeat("padding ", 1000) + `"}}` + "\n"
		for i := 1; i <= 50; i++ {
			stream.WriteString(filler)
			// Cumulative, so the LAST one is the session total.
			stream.WriteString(`{"type":"turn.completed","usage":{"input_tokens":` +
				strconv.Itoa(i*100) + `,"cached_input_tokens":0,"output_tokens":0,"reasoning_output_tokens":0}}` + "\n")
		}

		keep := &tail{max: 4096}
		var p codexParse
		sink := newLineSink(p.line)
		if _, err := io.Copy(io.MultiWriter(keep, sink), strings.NewReader(stream.String())); err != nil {
			t.Fatalf("copy: %v", err)
		}
		sink.Flush()
		var res Result
		p.finish(&res)

		if want := 50 * 100; res.Tokens != want {
			t.Errorf("Tokens = %d, want %d (last cumulative total)", res.Tokens, want)
		}
		if len(keep.buf) > keep.max {
			t.Errorf("retained %d bytes, want <= %d", len(keep.buf), keep.max)
		}
	})
}

// The classifiers are handed the Result by value and each used to rebuild the same
// scoped copy of it. With a memo they build it once.
func TestScanIsBuiltOncePerWidth(t *testing.T) {
	res := Result{Stderr: "API Error: 503 overloaded\n", scans: newScanCache()}

	builds := 0
	build := func() string { builds++; return res.Stderr }
	for i := 0; i < 3; i++ {
		if got := res.scan(int(claudeDiagnostic), build); got != res.Stderr {
			t.Fatalf("scan = %q", got)
		}
	}
	if builds != 1 {
		t.Errorf("built %d times, want 1", builds)
	}
	// A different width is a different answer and is built on its own.
	if got := res.scan(int(claudeTyped), build); got != res.Stderr || builds != 2 {
		t.Errorf("builds = %d after a second width, want 2", builds)
	}

	// A hand-built Result has no cache and must still work — just uncached.
	plain := Result{Stderr: "x"}
	builds = 0
	plain.scan(int(claudeTyped), build)
	plain.scan(int(claudeTyped), build)
	if builds != 2 {
		t.Errorf("uncached Result: builds = %d, want 2 (no memo, but a working answer)", builds)
	}
}

func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
