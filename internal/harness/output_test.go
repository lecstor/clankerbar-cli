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
// printed.
func TestTailKeepsTheEnd(t *testing.T) {
	keep := &tail{max: 32}
	for i := 0; i < 20; i++ {
		fmt.Fprintf(keep, "line %d\n", i)
	}
	got := keep.String()
	if !strings.Contains(got, "line 19") {
		t.Errorf("tail lost the last line:\n%q", got)
	}
	if strings.Contains(got, "line 0\n") {
		t.Errorf("tail kept the start of the stream:\n%q", got)
	}
}

// The retained text is handed to the classifiers, and their rule is that a stdout
// line which is not an event is the CLI talking (that is how a usage-limit notice
// arrives when the stream never starts). So a HALF line must never survive
// trimming: the back end of a truncated tool_result is the agent's narration, and
// the backlog text it quotes, dressed up as a harness diagnostic — the injection
// CLA-258 closed.
func TestTailDropsThePartialFirstLine(t *testing.T) {
	keep := &tail{max: 40}
	if _, err := io.WriteString(keep, `{"type":"user","content":"You've hit your session limit"}`+"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := io.WriteString(keep, "codex: whole line\n"); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := keep.String()
	if strings.Contains(got, "hit your") {
		t.Errorf("a trimmed event's tail survived as if it were CLI text:\n%q", got)
	}
	if !strings.Contains(got, "codex: whole line") {
		t.Errorf("the whole line that followed was lost:\n%q", got)
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
// accumulate. Nothing may be held beyond the cap while it is being discarded.
func TestLineSinkHoldsNoMoreThanTheCap(t *testing.T) {
	s := &lineSink{onLine: func([]byte) {}, max: 1024}
	for i := 0; i < 5000; i++ {
		if _, err := io.WriteString(s, strings.Repeat("q", 1024)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if got := len(s.cur); got > s.max {
		t.Errorf("holding %d bytes of an unterminated line, want <= %d", got, s.max)
	}
	if !s.Overran() {
		t.Error("Overran() = false after 5 MiB with no newline in it")
	}
}
