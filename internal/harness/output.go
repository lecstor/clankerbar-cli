package harness

import (
	"bytes"
	"io"
	"os/exec"
)

// This file holds the two bounds every adapter puts on a child's output, and the reason they exist
// at all.

const (
	// maxStreamLine is the largest single line an adapter will assemble from a child's stdout. One
	// stream-json event carrying a large file read can be megabytes, so this is generous; it is a
	// bound, not a budget.
	maxStreamLine = 16 << 20 // 16 MiB

	// retainedTail is how much of each stream (stdout, stderr) a session KEEPS, for the classifiers
	// and for the operator-facing diagnostic. Accepted, because the alternative is unbounded retention
	// in a process built to outlive its child, and because what the CLI says LAST about how a session
	// ended is the better evidence anyway.
	retainedTail = 2 << 20 // 2 MiB
)

// newline is the separator a line-oriented reader has to put back when it writes scanned lines to a
// tail (bufio.Scanner strips it).
var newline = []byte("\n")

// tail is a bounded io.Writer that keeps the LAST retainedTail bytes written to it and discards the
// rest as it goes, so a stream of any size costs a fixed amount of memory. When anything has been
// dropped, the leading partial line goes with it: a half line does not parse as an event, and the
// classifiers deliberately read a non-event stdout line as the CLI's own words (that is how a
// usage-limit notice arrives when the stream never starts), so handing them the back half of a
// truncated tool_result would re-open exactly the injection CLA-258 closed — the agent's
// narration, and the backlog text it quotes, read as a harness diagnostic.
type tail struct {
	max int
	buf []byte
	// partial reports that buf now STARTS mid-line, recomputed on every trim from the byte that fell
	// off. A trim that happens to land on a newline boundary leaves a whole line at the front, and
	// throwing it away would be a small silent loss for nothing.
	partial bool
	dropped int64
}

func newTail() *tail { return &tail{max: retainedTail} }

func (t *tail) Write(p []byte) (int, error) {
	n := len(p)
	if t.max < 0 {
		// Retain nothing. Not constructed anywhere here; kept total rather than letting a negative window
		// reach make() and panic.
		t.dropped += int64(n)
		return n, nil
	}
	if t.max == 0 {
		// A zero-value tail must not silently swallow the stream.
		t.max = retainedTail
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
		t.buf = t.buf[:0]
		t.reserve(t.max)
		t.buf = append(t.buf, p[cut:]...)
		return n, nil
	}
	if over := len(t.buf) + n - t.max; over > 0 {
		t.partial = t.buf[over-1] != '\n'
		copy(t.buf, t.buf[over:])
		t.buf = t.buf[:len(t.buf)-over]
		t.dropped += int64(over)
	}
	t.reserve(n)
	t.buf = append(t.buf, p...)
	return n, nil
}

// reserve makes room for need more bytes, growing by doubling and CLAMPING at max so the buffer's
// capacity is bounded and not merely its length. Callers must have trimmed first: this never
// allocates past max, so it relies on len(buf)+need <= max, which is what the trims in Write
// establish.
func (t *tail) reserve(need int) {
	want := len(t.buf) + need
	if want > t.max {
		want = t.max
	}
	if cap(t.buf) >= want {
		return
	}
	grow := cap(t.buf)
	const floor = 8 << 10 // one comfortable event, so a short stream stops growing at once
	if grow < floor {
		grow = floor
	}
	for grow < want {
		grow *= 2
	}
	if grow > t.max {
		grow = t.max
	}
	grown := make([]byte, len(t.buf), grow)
	copy(grown, t.buf)
	t.buf = grown
}

// String returns the retained tail, with a leading PARTIAL line dropped — see the type comment
// for why a half line must never be handed to a classifier.
func (t *tail) String() string {
	b := t.buf
	if t.partial {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		} else {
			// Everything held is the middle of one line. There is no whole line to report, and a fragment is
			// worse than nothing.
			b = nil
		}
	}
	return string(b)
}

// Dropped is how many bytes this stream produced that are no longer held. Zero means the retained
// text IS the whole stream.
func (t *tail) Dropped() int64 { return t.dropped }

// lineSink turns a byte stream into whole lines for a live parser, with an explicit cap on how long
// one line may get. bufio.Scanner is the equivalent on the reader side, and claude uses it because
// it reads the pipe itself; maxStreamLine is the cap for both, so neither path holds an order of
// magnitude more than the other (they may disagree by a byte at the exact boundary, which nothing
// depends on).
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
		// Drop what we have and everything up to this line's newline. Keeping the head would hand the
		// parser a truncated JSON object, which is indistinguishable from a line the CLI wrote itself.
		s.cur, s.skipping, s.overran = nil, true, true
		return
	}
	s.cur = append(s.cur, p...)
}

func (s *lineSink) emit() {
	if s.skipping {
		// cur was already released by take; this only clears the skip.
		s.cur, s.skipping = nil, false
		return
	}
	if len(s.cur) > 0 {
		s.onLine(s.cur)
	}
	s.cur = s.recycle()
}

// keepLineBuffer is how large an assembly buffer may be and still be REUSED for the next line.
// Above it the array is released instead.
const keepLineBuffer = 64 << 10

func (s *lineSink) recycle() []byte {
	if cap(s.cur) > keepLineBuffer {
		return nil
	}
	return s.cur[:0]
}

// Flush emits a trailing line that arrived without a newline. Call it once the child has exited and
// every write has landed.
func (s *lineSink) Flush() { s.emit() }

// Overran reports that some line was longer than max and was therefore never parsed.
func (s *lineSink) Overran() bool { return s.overran }

// capture is one child's output, wired the way an adapter that lets os/exec do the copying needs
// it: a bounded tail per stream, a live line parser fed from stdout, and the console tee. Shared by
// codex and opencode because the wiring is the whole of what they had in common here, and because a
// helper is drivable by a test without executing a binary — the alternative left the untrusted
// arm of both adapters unpinned.
type capture struct {
	stdout *tail
	stderr *tail
	sink   *lineSink
}

func newCapture(onLine func([]byte)) *capture {
	return &capture{stdout: newTail(), stderr: newTail(), sink: newLineSink(onLine)}
}

// attach points cmd's streams at this capture, teeing to console when there is one (a probe passes
// nil: nobody is watching, and its output is a few hundred bytes).
func (c *capture) attach(cmd *exec.Cmd, console io.Writer) {
	out := []io.Writer{c.stdout, c.sink}
	errs := []io.Writer{c.stderr}
	if console != nil {
		out = append(out, console)
		errs = append(errs, console)
	}
	cmd.Stdout, cmd.Stderr = io.MultiWriter(out...), io.MultiWriter(errs...)
}

// result closes the capture and returns the Result skeleton for the adapter to fill: the retained
// tails, a scan memo, and — if a line overran the cap — the reason the figures that follow
// cannot be believed.
func (c *capture) result(harness string) Result {
	var res Result
	c.finishInto(&res, harness)
	return res
}

// finishInto is result for an adapter that already holds a Result — copilot's parser writes the
// shared observer's claim state into one as the stream arrives — so the two paths cannot drift on
// the untrusted message or the scan memo.
func (c *capture) finishInto(res *Result, harness string) {
	c.sink.Flush() // a final line that arrived without a newline
	res.Stdout = c.stdout.String()
	res.Stderr = c.stderr.String()
	res.OutputDropped = c.stdout.Dropped() + c.stderr.Dropped()
	res.scans = newScanCache()
	if c.sink.Overran() {
		res.markUntrusted("a " + harness + " event was longer than the line cap and was discarded, " +
			"so an unknown number of events never reached the parser: this session's token and cost " +
			"figures are incomplete")
	}
}
