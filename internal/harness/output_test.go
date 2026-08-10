package harness

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

// The whole point of the tail: a stream of any size costs a FIXED amount of
// memory. A session that emits far more than the cap must leave the supervisor
// holding no more than the cap — the supervisor being the process whose entire job
// is to outlive the child (CLA-262).
func TestTailHoldsNoMoreThanTheCap(t *testing.T) {
	keep := newTail()
	line := strings.Repeat("x", 4096) + "\n"

	// 40 MiB through a 2 MiB window.
	const written = 10240
	for i := 0; i < written; i++ {
		if _, err := io.WriteString(keep, line); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// CAPACITY, not length: the allocation is what the supervisor's memory bill is
	// made of, and append's growth factor would overshoot a length-only bound.
	if got := cap(keep.buf); got > retainedTail {
		t.Errorf("retained buffer capacity %d, want <= the %d-byte cap", got, retainedTail)
	}
	if got := len(keep.buf); got > retainedTail {
		t.Errorf("retained %d bytes, want <= the %d-byte cap", got, retainedTail)
	}
	total := int64(written * len(line))
	if keep.Dropped() == 0 {
		t.Error("Dropped() = 0, but 40 MiB went through a 2 MiB window")
	}
	if held := int64(len(keep.String())); held+keep.Dropped() > total {
		t.Errorf("held %d + dropped %d exceeds the %d written", held, keep.Dropped(), total)
	}
}

// A tail keeps the END of the stream, which is where every answer it is read for
// lives: the usage-limit notice, the terminal_reason, the last error the CLI
// printed. And a trim that happens to land ON a newline leaves a whole line at
// the front, which must be KEPT — dropping it would be a silent loss for nothing.
func TestTailKeepsTheEnd(t *testing.T) {
	keep := &tail{max: 32} // exactly four "line NN\n" lines
	for i := 10; i < 30; i++ {
		fmt.Fprintf(keep, "line %d\n", i)
	}
	if want := "line 26\nline 27\nline 28\nline 29\n"; keep.String() != want {
		t.Errorf("String() = %q, want %q", keep.String(), want)
	}
	if keep.partial {
		t.Error("partial = true after a trim that landed exactly on a newline")
	}
}

// The retained text is handed to the classifiers, and their rule is that a stdout
// line which is not an event is the CLI talking (that is how a usage-limit notice
// arrives when the stream never starts). So a HALF line must never survive
// trimming: the back end of a truncated tool_result is the agent's narration, and
// the backlog text it quotes, dressed up as a harness diagnostic — the injection
// CLA-258 closed.
func TestTailDropsThePartialFirstLine(t *testing.T) {
	const event = `{"type":"user","content":"You've hit your session limit"}`
	const after = "codex: whole line\n"

	// Sized so the byte trim leaves the poisoned phrase INSIDE the retained
	// bytes — otherwise the arithmetic does the work and the alignment is never
	// exercised. The guard below fails if a later edit breaks that.
	keep := &tail{max: len("hit your session limit\"}\n") + len(after)}
	if _, err := io.WriteString(keep, event+"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := io.WriteString(keep, after); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(string(keep.buf), "hit your") {
		t.Fatalf("fixture no longer exercises the line alignment: the byte trim already removed the phrase from %q", string(keep.buf))
	}
	if !keep.partial {
		t.Fatal("partial = false, but the retained bytes start mid-line")
	}

	got := keep.String()
	if got != after {
		t.Errorf("String() = %q, want exactly %q — the partial line must go whole", got, after)
	}
	if strings.Contains(got, "hit your") {
		t.Errorf("a trimmed event's tail survived as if it were CLI text:\n%q", got)
	}
}

// Everything held is the middle of one enormous line: there is no whole line to
// report, and a fragment would be read as CLI text.
func TestTailWithNoLineBoundaryReportsNothing(t *testing.T) {
	keep := &tail{max: 16}
	if _, err := io.WriteString(keep, strings.Repeat("a", 64)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := keep.String(); got != "" {
		t.Errorf("String() = %q, want empty — no line boundary survived", got)
	}
}

// Nothing dropped means the retained text IS the stream, byte for byte. The
// line-alignment above must not touch a stream that fit.
func TestTailUntrimmedIsExact(t *testing.T) {
	keep := newTail()
	const s = "first\nsecond\nthird"
	if _, err := io.WriteString(keep, s); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := keep.String(); got != s {
		t.Errorf("String() = %q, want %q", got, s)
	}
	if keep.Dropped() != 0 {
		t.Errorf("Dropped() = %d, want 0", keep.Dropped())
	}
}

func TestLineSinkSplitsAcrossWrites(t *testing.T) {
	var got []string
	s := newLineSink(func(b []byte) { got = append(got, string(b)) })

	// A stream arrives in whatever chunks the pipe hands over — routinely cutting
	// an event in half.
	for _, chunk := range []string{`{"a":1}` + "\n" + `{"b`, `":2}` + "\n", "trailing, no newline"} {
		if _, err := io.WriteString(s, chunk); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	s.Flush()

	want := []string{`{"a":1}`, `{"b":2}`, "trailing, no newline"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("lines = %q, want %q", got, want)
	}
	if s.Overran() {
		t.Error("Overran() = true for lines well under the cap")
	}
}

// A line above the cap is DISCARDED, not shortened — half a JSON object parses as
// no object, and it is indistinguishable from a line the CLI wrote itself. The
// lines around it must still arrive, and the caller must be told an event was
// lost so it can mark the Result untrusted.
func TestLineSinkDiscardsAnOversizedLine(t *testing.T) {
	var got []string
	s := &lineSink{onLine: func(b []byte) { got = append(got, string(b)) }, max: 64}

	if _, err := io.WriteString(s, "before\n"+strings.Repeat("z", 500)+"\nafter\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	s.Flush()

	if strings.Join(got, "|") != "before|after" {
		t.Errorf("lines = %q, want the oversized one dropped and its neighbours kept", got)
	}
	if !s.Overran() {
		t.Error("Overran() = false after a line above the cap")
	}
}

// The line cap is a bound on MEMORY too: a single line that never ends must not
// accumulate, and the array behind an abandoned one must not be held for the rest
// of the session either.
func TestLineSinkHoldsNoMoreThanTheCap(t *testing.T) {
	s := &lineSink{onLine: func([]byte) {}, max: 1024}
	for i := 0; i < 5000; i++ {
		if _, err := io.WriteString(s, strings.Repeat("q", 1024)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if got := cap(s.cur); got > s.max {
		t.Errorf("holding a %d-byte array for an abandoned line, want <= %d", got, s.max)
	}
	if !s.Overran() {
		t.Error("Overran() = false after 5 MiB with no newline in it")
	}

	// The interesting case the loop above does not reach: a long but LEGAL line,
	// held right up against the cap and then delivered whole.
	var got []string
	s = &lineSink{onLine: func(b []byte) { got = append(got, string(b)) }, max: 1024}
	legal := strings.Repeat("k", 1024)
	for _, chunk := range []string{legal[:1000], legal[1000:], "\n"} {
		if _, err := io.WriteString(s, chunk); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if len(got) != 1 || got[0] != legal {
		t.Errorf("a line of exactly the cap was not delivered whole: got %d line(s), first %d bytes", len(got), len(got[0]))
	}
	if s.Overran() {
		t.Error("Overran() = true for a line of exactly the cap")
	}
	if cap(s.cur) > s.max {
		t.Errorf("holding %d bytes after emitting, want <= %d", cap(s.cur), s.max)
	}
}
