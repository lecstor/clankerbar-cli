package harness

import "bytes"

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
	// Two MiB per stream, so a session holds at most ~4 MiB whatever the child
	// does. That is far more than any classification needs — the widest pattern
	// here matches inside one line, and failureDetail renders 400 runes of it —
	// and it keeps enough context that an operator reading the tail can see what
	// led up to the failure.
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
	max     int
	buf     []byte
	dropped int64
}

func newTail() *tail { return &tail{max: retainedTail} }

func (t *tail) Write(p []byte) (int, error) {
	n := len(p)
	if t.max <= 0 {
		t.dropped += int64(n)
		return n, nil
	}
	if n >= t.max {
		// This write alone overruns the window: everything held, plus everything
		// but the last t.max bytes of p, is gone.
		t.dropped += int64(len(t.buf)) + int64(n-t.max)
		t.buf = append(t.buf[:0], p[n-t.max:]...)
		return n, nil
	}
	if over := len(t.buf) + n - t.max; over > 0 {
		copy(t.buf, t.buf[over:])
		t.buf = t.buf[:len(t.buf)-over]
		t.dropped += int64(over)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

// String returns the retained tail, aligned to a line boundary when bytes were
// dropped — see the type comment for why the partial first line must go.
func (t *tail) String() string {
	b := t.buf
	if t.dropped > 0 {
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
// maxStreamLine is deliberately the cap for both, so the two paths truncate at
// the same size.
//
// A line that overruns max is DISCARDED, not shortened, and Overran reports it:
// half an event parses as no event, and the caller marks its Result untrusted
// rather than reporting figures with a hole in them.
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
		s.cur, s.skipping, s.overran = s.cur[:0], true, true
		return
	}
	s.cur = append(s.cur, p...)
}

func (s *lineSink) emit() {
	if s.skipping {
		s.cur, s.skipping = s.cur[:0], false
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
