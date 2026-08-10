package harness

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
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

// codex and opencode assemble lines on the WRITER side, so their oversized-line
// case is the lineSink's rather than the scanner's — and it has the worse
// consequence, because for opencode the discarded event is the one carrying that
// step's tokens and cost. This drives the exact wiring Invoke uses (capture ->
// cmd.Stdout -> tail + lineSink -> parser) without executing a binary.
func TestCaptureMarksAnOversizedEventUntrusted(t *testing.T) {
	for _, tc := range []struct {
		harness string
		// wire builds the parser and hands back its two halves, so the capture is
		// constructed exactly as production constructs it: with the callback.
		wire   func() (func([]byte), func(*Result))
		before string
		after  string
		want   int    // tokens from the events that DID land
		msg    string // FinalMessage, when the fixture pins the BEFORE event that way
	}{
		{
			harness: "opencode",
			wire: func() (func([]byte), func(*Result)) {
				p := new(opencodeParse)
				return p.line, p.finish
			},
			// opencode SUMS, so the total alone proves both sides landed.
			before: `{"type":"step_finish","part":{"type":"step-finish","tokens":{"total":700,"input":1,"output":1,"reasoning":0,"cache":{"write":698,"read":0}}}}`,
			after:  `{"type":"step_finish","part":{"type":"step-finish","tokens":{"total":300,"input":1,"output":1,"reasoning":0,"cache":{"write":298,"read":0}}}}`,
			want:   1000,
		},
		{
			harness: "codex",
			wire: func() (func([]byte), func(*Result)) {
				p := new(codexParse)
				return p.line, p.finish
			},
			// codex keeps the LAST cumulative usage event, so a token total proves
			// nothing about the earlier one. The BEFORE event therefore leaves an
			// independent trace instead, and the AFTER event carries usage only.
			before: `{"type":"item.completed","item":{"type":"agent_message","text":"before the big one"}}`,
			after:  `{"type":"turn.completed","usage":{"input_tokens":900,"cached_input_tokens":0,"output_tokens":0,"reasoning_output_tokens":0}}`,
			want:   900,
			msg:    "before the big one",
		},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			onLine, finish := tc.wire()
			captured := newCapture(onLine)
			captured.sink.max = 4096 // so the oversized line is a fast fixture

			cmd := &exec.Cmd{}
			captured.attach(cmd, nil)

			// A tool_result carrying a large file read, between two events.
			oversized := `{"type":"text","part":{"type":"text","text":"` + strings.Repeat("y", 8192) + `"}}`
			if _, err := io.WriteString(cmd.Stdout, tc.before+"\n"+oversized+"\n"+tc.after+"\n"); err != nil {
				t.Fatalf("write: %v", err)
			}

			res := captured.result(tc.harness)
			finish(&res)

			if res.Untrusted == "" {
				t.Fatal("Untrusted is empty: an event was silently discarded and the figures below have a hole in them")
			}
			if !strings.Contains(res.Untrusted, tc.harness) {
				t.Errorf("Untrusted = %q, want it to name the harness", res.Untrusted)
			}
			// The events either side still landed — the overrun costs one line, not
			// the stream.
			if res.Tokens != tc.want {
				t.Errorf("Tokens = %d, want %d (the event after the oversized one)", res.Tokens, tc.want)
			}
			if tc.msg != "" && res.FinalMessage != tc.msg {
				t.Errorf("FinalMessage = %q, want %q — the event BEFORE the oversized one must still have been parsed", res.FinalMessage, tc.msg)
			}
		})
	}
}

// A clean stream through the same wiring is trusted, retained whole, and tees to
// the console — the behaviour the old io.MultiWriter(&buf, console) had.
func TestCaptureCleanStreamTeesAndTrusts(t *testing.T) {
	p := new(codexParse)
	captured := newCapture(p.line)

	var console bytes.Buffer
	cmd := &exec.Cmd{}
	captured.attach(cmd, &console)

	const stream = `{"type":"turn.completed","usage":{"input_tokens":42,"cached_input_tokens":0,"output_tokens":0,"reasoning_output_tokens":0}}` + "\n"
	if _, err := io.WriteString(cmd.Stdout, stream); err != nil {
		t.Fatalf("write stdout: %v", err)
	}
	if _, err := io.WriteString(cmd.Stderr, "codex: warming up\n"); err != nil {
		t.Fatalf("write stderr: %v", err)
	}

	res := captured.result("codex")
	p.finish(&res)

	if res.Untrusted != "" {
		t.Errorf("Untrusted = %q on a clean stream", res.Untrusted)
	}
	if res.Tokens != 42 {
		t.Errorf("Tokens = %d, want 42", res.Tokens)
	}
	if res.Stdout != stream {
		t.Errorf("Stdout = %q, want the stream verbatim", res.Stdout)
	}
	if res.Stderr != "codex: warming up\n" {
		t.Errorf("Stderr = %q", res.Stderr)
	}
	if res.OutputDropped != 0 {
		t.Errorf("OutputDropped = %d on a stream that fitted the window", res.OutputDropped)
	}
	if out := console.String(); !strings.Contains(out, "turn.completed") || !strings.Contains(out, "warming up") {
		t.Errorf("console lost the live tee of one or both streams:\n%q", out)
	}
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

// A probe answers one question, and the supervised wait resumes spending real
// sessions on a "no". So an untrusted probe must not answer it: an empty Limit
// read out of output the adapter could not read whole is indistinguishable from a
// lifted cap. It comes back as an error, which the loop already treats as "still
// do not know" and waits another interval on.
func TestProbeVerdictRefusesToReadAnUntrustedProbe(t *testing.T) {
	limited := func(Result) Limit { return Limit{Limited: true, Reason: "usage_limit"} }
	lifted := func(Result) Limit { return Limit{} }

	// The trustworthy case still answers, both ways round.
	if out, err := probeVerdict(ProbeResult{}, Result{}, lifted); err != nil || out.Limit.Limited {
		t.Errorf("a clean probe must report the lift: out=%+v err=%v", out, err)
	}
	if out, err := probeVerdict(ProbeResult{}, Result{}, limited); err != nil || !out.Limit.Limited {
		t.Errorf("a clean probe must report the cap: out=%+v err=%v", out, err)
	}

	// The untrusted case answers nothing — and still reports what it cost.
	out, err := probeVerdict(ProbeResult{Tokens: 12, CostUSD: 0.02}, Result{Untrusted: "stream cut short"}, lifted)
	if err == nil {
		t.Fatal("err = nil: an unreadable probe reported 'not limited', which resumes the run")
	}
	// Wrapped, so the wait can tell "cannot read this harness" (which will not clear
	// itself) from a network blip (which will).
	if !errors.Is(err, ErrUntrusted) {
		t.Errorf("err = %v, want it to wrap ErrUntrusted", err)
	}
	if out.Limit.Limited {
		t.Error("an untrusted probe must not report a limit either — it reports nothing")
	}
	if out.Tokens != 12 || out.CostUSD != 0.02 {
		t.Errorf("spend = %d / $%v, want it carried out through the error: a probe that could not be read still cost what it cost", out.Tokens, out.CostUSD)
	}
}

func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
