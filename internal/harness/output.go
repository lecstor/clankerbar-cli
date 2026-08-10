package harness

import (
	"bytes"
	"io"
	"os/exec"
)

// This file holds the two bounds every adapter puts on a child's output, and the
// reason they exist at all.
//
// A session is an agentic run that may last hours, and its stdout is the whole
// event stream: every assistant message, every tool_use, and every tool_result —
// which includes the verbatim content of any file the agent read. Hundreds of
// megabytes is an ordinary night's work. The adapters used to accumulate all of
// it, both streams, for the life of the drain, so a runaway child could OOM the
// SUPERVISOR — the process whose entire job is to outlive the child (CLA-262).
//
// Nothing needs the whole of it. The parsed figures (tokens, cost, the claim, the
// delivery reports) are taken line by line as the stream arrives, and the only
// consumer of the retained bytes is the limit/transient classification, which is
// a regex scan over the harness's OWN output and whose answer is always at the
// END: a usage-limit notice, a terminal_reason, the last error the CLI printed.

const (
	// maxStreamLine is the largest single line an adapter will assemble from a
	// child's stdout. One stream-json event carrying a large file read can be
	// megabytes, so this is generous; it is a bound, not a budget.
	//
	// A line ABOVE it is not silently shortened. The event it carried is lost to
	// the parser, so the Result's figures are incomplete in ways nothing
	// downstream can see — which is why crossing this marks the Result untrusted
	// rather than trimming and carrying on. See Result.Untrusted.
	maxStreamLine = 16 << 20 // 16 MiB

	// retainedTail is how much of each stream (stdout, stderr) a session KEEPS,
	// for the classifiers and for the operator-facing diagnostic.
	//
	// Two MiB per stream, so a session RETAINS at most 4 MiB whatever the child
	// does. (The process's peak is higher: assembling one line costs up to
	// maxStreamLine on top, in the scanner's buffer or a lineSink's. Those are
	// transient per-line working space; this is what is held for the session.)
	// Two MiB is far more than any classification needs — the widest pattern here
	// matches inside one line, and failureDetail renders 400 runes of it — and it
	// keeps enough context that an operator reading the tail can see what led up
	// to the failure.
	//
	// The cost, stated rather than implied: a signal emitted MORE than a window
	// ago is no longer read. A session that announced a retryable API error in its
	// first minute, then produced 6 MiB of stream and exited non-zero with nothing
	// retryable near the end, is now classified non-retryable and stops the run
	// where it used to back off and retry. Accepted, because the alternative is
	// unbounded retention in a process built to outlive its child, and because
	// what the CLI says LAST about how a session ended is the better evidence
	// anyway. Not configurable: a dial here is a way to reintroduce the OOM.
	retainedTail = 2 << 20 // 2 MiB
)

// newline is the separator a line-oriented reader has to put back when it writes
// scanned lines to a tail (bufio.Scanner strips it).
var newline = []byte("\n")

// tail is a bounded io.Writer that keeps the LAST retainedTail bytes written to
// it and discards the rest as it goes, so a stream of any size costs a fixed
// amount of memory.
//
// It hands back whole lines only. When anything has been dropped, the leading
// partial line goes with it: a half line does not parse as an event, and the
// classifiers deliberately read a non-event stdout line as the CLI's own words
// (that is how a usage-limit notice arrives when the stream never starts), so
// handing them the back half of a truncated tool_result would re-open exactly
// the injection CLA-258 closed — the agent's narration, and the backlog text it
// quotes, read as a harness diagnostic.
//
// Not safe for concurrent use: each stream gets its own tail, which is how
// os/exec already treats the writers it is given.
type tail struct {
	max int
	buf []byte
	// partial reports that buf now STARTS mid-line, recomputed on every trim from
	// the byte that fell off. A trim that happens to land on a newline boundary
	// leaves a whole line at the front, and throwing it away would be a small
	// silent loss for nothing.
	partial bool
	dropped int64
}

func newTail() *tail { return &tail{max: retainedTail} }

func (t *tail) Write(p []byte) (int, error) {
	n := len(p)
	if t.max == 0 {
		// A zero-value tail must not silently swallow the stream.
		t.max = retainedTail
	}
	// Allocated once, at exactly the window size, so the CAP is the bound too and
	// not just the length: append's growth factor would otherwise overshoot it by
	// a quarter for the life of the session.
	if t.buf == nil {
		t.buf = make([]byte, 0, t.max)
	}

	if n >= t.max {
		// This write alone fills the window: nothing held survives it.
		cut := n - t.max
		switch {
		case cut > 0:
			t.partial = p[cut-1] != '\n'
		case len(t.buf) > 0:
			t.partial = t.buf[len(t.buf)-1] != '\n'
		}
		t.dropped += int64(len(t.buf)) + int64(cut)
		t.buf = append(t.buf[:0], p[cut:]...)
		return n, nil
	}
	if over := len(t.buf) + n - t.max; over > 0 {
		t.partial = t.buf[over-1] != '\n'
		copy(t.buf, t.buf[over:])
		t.buf = t.buf[:len(t.buf)-over]
		t.dropped += int64(over)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

// String returns the retained tail, with a leading PARTIAL line dropped — see the
// type comment for why a half line must never be handed to a classifier.
func (t *tail) String() string {
	b := t.buf
	if t.partial {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		} else {
			// Everything held is the middle of one line. There is no whole line to
			// report, and a fragment is worse than nothing.
			b = nil
		}
	}
	return string(b)
}

// Dropped is how many bytes this stream produced that are no longer held. Zero
// means the retained text IS the whole stream.
func (t *tail) Dropped() int64 { return t.dropped }

// lineSink turns a byte stream into whole lines for a live parser, with an
// explicit cap on how long one line may get.
//
// It exists so an adapter can parse as the stream arrives — rather than
// accumulating the whole of stdout and walking it afterwards — while writing to
// the same os/exec-managed writer it always did. bufio.Scanner is the equivalent
// on the reader side, and claude uses it because it reads the pipe itself;
// maxStreamLine is the cap for both, so neither path holds an order of magnitude
// more than the other (they may disagree by a byte at the exact boundary, which
// nothing depends on).
//
// A line that overruns max is DISCARDED, not shortened, and Overran reports it:
// half an event parses as no event, and the caller marks its Result untrusted
// rather than reporting figures with a hole in them.
//
// onLine is handed a slice that is REUSED by the next line: copy anything you
// keep, as both parsers do.
type lineSink struct {
	onLine func([]byte)
	max    int

	cur      []byte
	skipping bool
	overran  bool
}

func newLineSink(onLine func([]byte)) *lineSink {
	return &lineSink{onLine: onLine, max: maxStreamLine}
}

func (s *lineSink) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.take(p)
			break
		}
		s.take(p[:i])
		s.emit()
		p = p[i+1:]
	}
	return n, nil
}

func (s *lineSink) take(p []byte) {
	if s.skipping {
		return
	}
	if len(s.cur)+len(p) > s.max {
		// Drop what we have and everything up to this line's newline. Keeping the
		// head would hand the parser a truncated JSON object, which is
		// indistinguishable from a line the CLI wrote itself.
		//
		// nil rather than cur[:0]: the array behind an overrun line is up to
		// maxStreamLine, and keeping it alive for the rest of the session is the
		// memory this file exists to bound.
		s.cur, s.skipping, s.overran = nil, true, true
		return
	}
	s.cur = append(s.cur, p...)
}

func (s *lineSink) emit() {
	if s.skipping {
		s.cur, s.skipping = nil, false
		return
	}
	if len(s.cur) > 0 {
		s.onLine(s.cur)
	}
	s.cur = s.cur[:0]
}

// Flush emits a trailing line that arrived without a newline. Call it once the
// child has exited and every write has landed.
func (s *lineSink) Flush() { s.emit() }

// Overran reports that some line was longer than max and was therefore never
// parsed.
func (s *lineSink) Overran() bool { return s.overran }

// capture is one child's output, wired the way an adapter that lets os/exec do
// the copying needs it: a bounded tail per stream, a live line parser fed from
// stdout, and the console tee.
//
// Shared by codex and opencode because the wiring is the whole of what they had
// in common here, and because a helper is drivable by a test without executing a
// binary — the alternative left the untrusted arm of both adapters unpinned.
// claude does its own reading (it needs the pipe to render progress and to drain
// after a failed scan), so it composes the same two pieces by hand.
type capture struct {
	stdout *tail
	stderr *tail
	sink   *lineSink
}

func newCapture(onLine func([]byte)) *capture {
	return &capture{stdout: newTail(), stderr: newTail(), sink: newLineSink(onLine)}
}

// attach points cmd's streams at this capture, teeing to console when there is
// one (a probe passes nil: nobody is watching, and its output is a few hundred
// bytes).
func (c *capture) attach(cmd *exec.Cmd, console io.Writer) {
	out := []io.Writer{c.stdout, c.sink}
	errs := []io.Writer{c.stderr}
	if console != nil {
		out = append(out, console)
		errs = append(errs, console)
	}
	cmd.Stdout, cmd.Stderr = io.MultiWriter(out...), io.MultiWriter(errs...)
}

// result closes the capture and returns the Result skeleton for the adapter to
// fill: the retained tails, a scan memo, and — if a line overran the cap — the
// reason the figures that follow cannot be believed. `harness` names the CLI in
// that message, since it is read by an operator.
//
// Call it after cmd.Run/Wait: that is what joins os/exec's copy goroutines, and
// so what makes everything written here safe to read.
func (c *capture) result(harness string) Result {
	c.sink.Flush() // a final line that arrived without a newline
	res := Result{Stdout: c.stdout.String(), Stderr: c.stderr.String(), scans: newScanCache()}
	if c.sink.Overran() {
		res.markUntrusted("a " + harness + " event was longer than the line cap and was discarded, " +
			"so an unknown number of events never reached the parser: this session's token and cost " +
			"figures are incomplete")
	}
	return res
}
