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

// A line that never ends must not accumulate, and the array behind an ABANDONED
// one must be released rather than held for the rest of the session behind a
// `cur[:0]`.
//
// The contract asserted is `cur == nil`, not a size: a capacity bound would be
// satisfied by an implementation that keeps the array, since a size-class-aligned
// cap equals the cap it is compared against.
func TestLineSinkReleasesAnAbandonedLine(t *testing.T) {
	// Chunked so the buffer GROWS large before the overrun — the whole point is
	// what happens to an array that was actually allocated.
	s := &lineSink{onLine: func([]byte) {}, max: 100_000}
	for i := 0; i < 5000; i++ {
		if _, err := io.WriteString(s, strings.Repeat("q", 40_000)); err != nil {
			t.Fatalf("write: %v", err)
		}
		if i == 0 && len(s.cur) != 40_000 {
			t.Fatalf("fixture is wrong: after one chunk cur holds %d bytes, so the overrun below would find nothing to release", len(s.cur))
		}
	}
	if s.cur != nil {
		t.Errorf("still holding a %d-byte array for an abandoned line; it must be released, not reset", cap(s.cur))
	}
	if !s.Overran() {
		t.Error("Overran() = false after 200 MB with no newline in it")
	}

	// And once the abandoned line finally ends, the sink is usable again.
	if _, err := io.WriteString(s, "\n{\"type\":\"after\"}\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if s.skipping {
		t.Error("still skipping after the oversized line's newline arrived")
	}
}

// A LEGAL line can also be enormous — codex has no per-event size limit of its own
// — and one of those must not leave its array pinned for the rest of the session
// either. Above keepLineBuffer the buffer is released after delivery instead of
// being reused.
func TestLineSinkReleasesAnOversizedLegalLine(t *testing.T) {
	var got int
	s := newLineSink(func(b []byte) { got = len(b) })

	huge := strings.Repeat("h", keepLineBuffer*2)
	if _, err := io.WriteString(s, huge+"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got != len(huge) {
		t.Fatalf("delivered %d bytes, want %d — the line is legal and must arrive whole", got, len(huge))
	}
	if s.cur != nil {
		t.Errorf("kept a %d-byte array for reuse; above keepLineBuffer (%d) it must be released", cap(s.cur), keepLineBuffer)
	}
}

// A long but LEGAL line — held right up against the cap and then delivered whole —
// is the case the abandoning loop above never reaches. `max` is deliberately NOT a
// Go size class, so the capacity assertion is enforced by the code rather than by
// the allocator rounding up to exactly it.
func TestLineSinkDeliversALineAtTheCap(t *testing.T) {
	var got []string
	s := &lineSink{onLine: func(b []byte) { got = append(got, string(b)) }, max: 1000}
	legal := strings.Repeat("k", 1000)
	for _, chunk := range []string{legal[:997], legal[997:], "\n"} {
		if _, err := io.WriteString(s, chunk); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if len(got) != 1 || got[0] != legal {
		t.Fatalf("a line of exactly the cap was not delivered whole: got %d line(s), first %d bytes", len(got), len(got[0]))
	}
	if s.Overran() {
		t.Error("Overran() = true for a line of exactly the cap")
	}
	// Small enough to reuse, so the ordinary stream stays allocation-free.
	if s.cur == nil {
		t.Error("released a buffer well under keepLineBuffer; the common case should reuse it")
	}
}

// A tail's CAPACITY is bounded, not just its length — and it is not allocated at
// the full window up front either, because a probe writes a few hundred bytes and
// a week-long wait runs hundreds of them.
func TestTailCapacityGrowsAndIsClamped(t *testing.T) {
	small := &tail{max: 1 << 20}
	if _, err := io.WriteString(small, "codex: warming up\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if cap(small.buf) >= small.max {
		t.Errorf("a %d-byte write allocated %d bytes, the whole window", len(small.buf), cap(small.buf))
	}

	// max is NOT a power of two, so a clamp that leaned on the allocator rounding
	// down would fail here.
	full := &tail{max: 100_000}
	chunk := strings.Repeat("z", 4096) + "\n"
	for i := 0; i < 200; i++ {
		if _, err := io.WriteString(full, chunk); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if cap(full.buf) > full.max {
		t.Errorf("capacity %d exceeds the %d-byte window", cap(full.buf), full.max)
	}
	if len(full.buf) != full.max {
		t.Errorf("len = %d, want the window full at %d", len(full.buf), full.max)
	}
	// A single write larger than the whole window takes the other branch.
	if _, err := io.WriteString(full, strings.Repeat("y", 250_000)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if cap(full.buf) > full.max {
		t.Errorf("capacity %d exceeds the window after an oversized single write", cap(full.buf))
	}
}

// A negative window retains nothing rather than panicking in make().
func TestTailNegativeWindowRetainsNothing(t *testing.T) {
	keep := &tail{max: -1}
	if _, err := io.WriteString(keep, "anything\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := keep.String(); got != "" {
		t.Errorf("String() = %q, want empty", got)
	}
	if keep.Dropped() != int64(len("anything\n")) {
		t.Errorf("Dropped() = %d, want %d", keep.Dropped(), len("anything\n"))
	}
}
