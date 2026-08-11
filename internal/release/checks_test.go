package release

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name string
		runs []CheckRun
		want Verdict
	}{
		{
			name: "green ci passes",
			runs: []CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}},
			want: VerdictPass,
		},
		{
			name: "red ci fails",
			runs: []CheckRun{{Name: "ci", Status: "completed", Conclusion: "failure"}},
			want: VerdictFail,
		},

		// THE clause this gate exists for. No run named `ci` at all is not a pass;
		// it is "no answer yet", which Await resolves to a failure at its deadline.
		{
			name: "absent is pending, never a pass",
			runs: nil,
			want: VerdictPending,
		},
		{
			name: "absent among other checks is still pending",
			runs: []CheckRun{{Name: "lint", Status: "completed", Conclusion: "success"}},
			want: VerdictPending,
		},
		{
			name: "a green check with a DIFFERENT name does not stand in for ci",
			runs: []CheckRun{
				{Name: "build", Status: "completed", Conclusion: "success"},
				{Name: "test", Status: "completed", Conclusion: "success"},
			},
			want: VerdictPending,
		},

		// Unfinished states.
		{
			name: "queued is pending",
			runs: []CheckRun{{Name: "ci", Status: "queued"}},
			want: VerdictPending,
		},
		{
			name: "in_progress is pending",
			runs: []CheckRun{{Name: "ci", Status: "in_progress"}},
			want: VerdictPending,
		},

		// Every non-success terminal conclusion is a failure. `skipped` and
		// `neutral` are the tempting ones: they are not red, but they tested
		// nothing, so they must not publish binaries.
		{
			name: "skipped is a failure, not a pass",
			runs: []CheckRun{{Name: "ci", Status: "completed", Conclusion: "skipped"}},
			want: VerdictFail,
		},
		{
			name: "neutral is a failure",
			runs: []CheckRun{{Name: "ci", Status: "completed", Conclusion: "neutral"}},
			want: VerdictFail,
		},
		{
			name: "cancelled is a failure",
			runs: []CheckRun{{Name: "ci", Status: "completed", Conclusion: "cancelled"}},
			want: VerdictFail,
		},
		{
			name: "timed_out is a failure",
			runs: []CheckRun{{Name: "ci", Status: "completed", Conclusion: "timed_out"}},
			want: VerdictFail,
		},
		{
			name: "action_required is a failure",
			runs: []CheckRun{{Name: "ci", Status: "completed", Conclusion: "action_required"}},
			want: VerdictFail,
		},
		{
			name: "completed with an empty conclusion is a failure",
			runs: []CheckRun{{Name: "ci", Status: "completed", Conclusion: ""}},
			want: VerdictFail,
		},

		// Several runs under one name (a re-run, or a matrixed job).
		{
			name: "all green passes",
			runs: []CheckRun{
				{Name: "ci", Status: "completed", Conclusion: "success"},
				{Name: "ci", Status: "completed", Conclusion: "success"},
			},
			want: VerdictPass,
		},
		{
			name: "one red among green fails",
			runs: []CheckRun{
				{Name: "ci", Status: "completed", Conclusion: "success"},
				{Name: "ci", Status: "completed", Conclusion: "failure"},
			},
			want: VerdictFail,
		},
		{
			name: "one still running holds the whole name pending",
			runs: []CheckRun{
				{Name: "ci", Status: "completed", Conclusion: "success"},
				{Name: "ci", Status: "in_progress"},
			},
			want: VerdictPending,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := Evaluate("ci", tt.runs)
			if got != tt.want {
				t.Errorf("Evaluate = %v (%s), want %v", got, reason, tt.want)
			}
			if strings.TrimSpace(reason) == "" {
				t.Error("Evaluate returned an empty reason; the reason is what makes a red gate diagnosable")
			}
		})
	}
}

// fakeClock is virtual time: Sleep advances the clock instead of waiting, so the
// deadline behaviour below is tested in microseconds.
type fakeClock struct {
	now   time.Time
	slept []time.Duration
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Sleep(d time.Duration) {
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 8, 10, 22, 0, 0, 0, time.UTC)}
}

func TestAwait(t *testing.T) {
	t.Run("returns pass once the check goes green", func(t *testing.T) {
		clock := newFakeClock()
		calls := 0
		fetch := func() ([]CheckRun, error) {
			calls++
			switch calls {
			case 1:
				return nil, nil // absent
			case 2:
				return []CheckRun{{Name: "ci", Status: "in_progress"}}, nil
			default:
				return []CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}}, nil
			}
		}

		got, reason := Await("ci", fetch, 10*time.Minute, 10*time.Second, clock)
		if got != VerdictPass {
			t.Fatalf("Await = %v (%s), want pass", got, reason)
		}
		if calls != 3 {
			t.Errorf("polled %d times, want 3", calls)
		}
	})

	t.Run("returns fail as soon as the check goes red, without waiting out the timeout", func(t *testing.T) {
		clock := newFakeClock()
		fetch := func() ([]CheckRun, error) {
			return []CheckRun{{Name: "ci", Status: "completed", Conclusion: "failure"}}, nil
		}

		got, reason := Await("ci", fetch, time.Hour, 10*time.Second, clock)
		if got != VerdictFail {
			t.Fatalf("Await = %v (%s), want fail", got, reason)
		}
		if len(clock.slept) != 0 {
			t.Errorf("slept %v; a terminal verdict should return immediately", clock.slept)
		}
	})

	// THE regression this task names explicitly: a check that never arrives must
	// produce NO release. The deadline resolves to fail, never to a pass.
	t.Run("a check that never arrives FAILS at the deadline", func(t *testing.T) {
		clock := newFakeClock()
		fetch := func() ([]CheckRun, error) { return nil, nil }

		got, reason := Await("ci", fetch, 30*time.Second, 10*time.Second, clock)
		if got != VerdictFail {
			t.Fatalf("Await = %v (%s), want fail - an absent check must never read as a pass", got, reason)
		}
		if !strings.Contains(reason, "timed out") {
			t.Errorf("reason = %q, want it to name the timeout", reason)
		}
	})

	t.Run("a check stuck in_progress FAILS at the deadline", func(t *testing.T) {
		clock := newFakeClock()
		fetch := func() ([]CheckRun, error) {
			return []CheckRun{{Name: "ci", Status: "in_progress"}}, nil
		}

		got, _ := Await("ci", fetch, 30*time.Second, 10*time.Second, clock)
		if got != VerdictFail {
			t.Fatalf("Await = %v, want fail", got)
		}
	})

	// A gate that can never READ the checks must also fail, and must say that is
	// why - "the API was down" and "the check was red" are both no-release, but
	// only one of them is worth paging about.
	t.Run("unreadable checks fail at the deadline and say so", func(t *testing.T) {
		clock := newFakeClock()
		fetch := func() ([]CheckRun, error) { return nil, errors.New("502 bad gateway") }

		got, reason := Await("ci", fetch, 30*time.Second, 10*time.Second, clock)
		if got != VerdictFail {
			t.Fatalf("Await = %v, want fail", got)
		}
		if !strings.Contains(reason, "502 bad gateway") {
			t.Errorf("reason = %q, want it to carry the last fetch error", reason)
		}
	})

	// A transient error must not end the wait: the deadline is the bound, not the
	// first flaky response.
	t.Run("recovers from a transient fetch error", func(t *testing.T) {
		clock := newFakeClock()
		calls := 0
		fetch := func() ([]CheckRun, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("connection reset")
			}
			return []CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}}, nil
		}

		got, reason := Await("ci", fetch, 10*time.Minute, 10*time.Second, clock)
		if got != VerdictPass {
			t.Fatalf("Await = %v (%s), want pass", got, reason)
		}
	})

	// A zero timeout still gets one real look - otherwise a green commit could be
	// failed by a gate that never examined it.
	t.Run("a zero timeout still polls once", func(t *testing.T) {
		clock := newFakeClock()
		calls := 0
		fetch := func() ([]CheckRun, error) {
			calls++
			return []CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}}, nil
		}

		got, _ := Await("ci", fetch, 0, 10*time.Second, clock)
		if got != VerdictPass {
			t.Fatalf("Await = %v, want pass", got)
		}
		if calls != 1 {
			t.Errorf("polled %d times, want exactly 1", calls)
		}
	})

	t.Run("a zero timeout fails an absent check", func(t *testing.T) {
		clock := newFakeClock()
		fetch := func() ([]CheckRun, error) { return nil, nil }

		got, _ := Await("ci", fetch, 0, 10*time.Second, clock)
		if got != VerdictFail {
			t.Fatalf("Await = %v, want fail", got)
		}
	})
}
