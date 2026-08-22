package harness

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A stub that backgrounds a child which outlives its parent and writes a
// file after a delay. The kill mechanism must terminate both the parent
// and the descendant, so the file is never created.
//
// Two hardening choices, both earned by observed failures:
//
//   - The stub emits an over-ceiling assistant event every 200ms instead
//     of a single one. The ceiling kill fires from consume(), so it only
//     happens when that goroutine makes progress; under a loaded machine
//     (loadavg 10 was watched) a single early event can sit unread past
//     the parent's natural lifetime and the session dies of old age
//     instead of the kill. A steady stream means the kill lands the
//     moment the consumer is scheduled, however late.
//   - The descendant's marker is POLLED FOR after Invoke returns, not
//     checked once immediately. A direct-child-only kill regression
//     leaves the descendant alive holding an inherited fd; the old
//     immediate check ran while it was still asleep, saw no marker,
//     and passed — pinned by nothing. Waiting past the descendant's
//     lifetime makes survival observable: if the marker ever appears,
//     something escaped the kill.
func TestProcessGroupKillTerminatesAGrandchild(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "claude")
	marker := filepath.Join(dir, "grandchild_alive")
	event := `{"type":"assistant","message":{"content":[{"type":"text","text":"work"}],"usage":{"input_tokens":60000000,"output_tokens":0}}}`
	script := `#!/bin/sh
# Background a grandchild that outlives the parent session and marks the
# world if it survived the kill. First, so its 2s clock starts now.
( sleep 2; touch "` + marker + `" ) &
# Stream events that cross the token ceiling immediately, so the kill
# fires whenever the consumer first reads — never of natural old age.
i=0
while [ $i -lt 250 ]; do
  echo '` + event + `'
  sleep 0.2
  i=$((i+1))
done
# Outlive any plausible scheduling delay, then stop: if the kill has not
# fired by here the session died of old age and the elapsed bound fails.
exec sleep 120
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Invoke must return promptly, not hang on a descendant holding an
	// exec-owned pipe: cmd.Stderr is an io.MultiWriter, so os/exec makes its
	// own pipe and Wait waits on its writer past the kill. (claude's session
	// path sets no WaitDelay, so a surviving holder blocks Wait outright —
	// which is itself the failure being asserted against.) Running Invoke on
	// its own goroutine makes that bound REAL: checked after the fact, an
	// elapsed comparison cannot fire while Invoke is still blocked, and the
	// regression it guards would hang to the package timeout instead of
	// failing here. The stub streams for ~50s then sleeps 120s, so an
	// unfired kill blows the 30s bound long before that.
	res, err := invokeWithBound(t, 30*time.Second, func() (Result, error) {
		return (claude{}).Invoke(context.Background(), Invocation{
			Prompt:           "work",
			MaxSessionTokens: 10, // any positive ceiling kills quickly
			Console:          io.Discard,
		})
	})

	if err != nil {
		t.Fatalf("Invoke returned an error for the ceiling kill: %v — must be orderly", err)
	}
	if !(claude{}).TokenCeilingHit(res) {
		t.Fatal("the Result does not carry the ceiling marker")
	}
	if res.ExitCode == 0 {
		t.Error("ExitCode = 0 — the child was not killed; the group kill did not fire")
	}

	// The grandchild must be dead: poll past its 2s lifetime and fail the
	// moment the marker proves a survivor.
	waitForGrandchildMarker(t, marker)
}

// The same shape for the opencode adapter: a wall-clock cap kills the
// process group, not just the direct child, so descendants die.
func TestOpencodeProcessGroupKillTerminatesAGrandchild(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "grandchild_alive")
	opencodeStub(t, `#!/bin/sh
# Emit a step event that triggers the cap quickly.
echo '{"type":"step_start","sessionID":"s1","part":{"type":"step-start"}}'
echo '{"type":"text","sessionID":"s1","part":{"type":"text","text":"working"}}'
echo '{"type":"step_finish","sessionID":"s1","part":{"type":"step-finish","reason":"unknown","tokens":{"input":0,"output":0,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}'
# Background a grandchild that outlives the parent and marks the world
# if it survived the kill.
( sleep 2; touch "`+marker+`" ) &
# Parent continues so the adapter kills it mid-session.
exec sleep 60
`)

	// Same bound discipline as the claude test. Note that under a group-kill
	// regression this test's real pin is the MARKER POLL below, not the
	// bound: cmd.WaitDelay (set for any capped session) force-closes the
	// pipes and returns Wait in ~5s even with the descendant still alive.
	res, err := invokeWithBound(t, 30*time.Second, func() (Result, error) {
		return (opencode{}).Invoke(context.Background(), Invocation{
			Prompt:              "work",
			MaxSessionWallClock: 500 * time.Millisecond,
			Console:             io.Discard,
		})
	})

	if err != nil {
		t.Fatalf("Invoke returned an error for the cap: %v", err)
	}
	if !(opencode{}).WallClockCapped(res) {
		t.Error("the session was not reported as wall-clock capped")
	}

	waitForGrandchildMarker(t, marker)
}

// waitForGrandchildMarker polls past a 2s-lifetime descendant and fails as
// soon as the marker appears — i.e. the moment anything is proven to have
// survived the kill. Absent after the window, the group kill reached the
// whole tree.
func waitForGrandchildMarker(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(3500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("grandchild marker file %s appeared — the descendant survived the group kill", marker)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// invokeOutcome carries an Invoke result back from the goroutine that ran it.
type invokeOutcome struct {
	res Result
	err error
}

// invokeWithBound runs fn on its own goroutine and fails the test if it has
// not returned within bound. A promptness requirement checked AFTER Invoke
// returns is not a bound at all — while Invoke is blocked the check never
// runs — so the wait itself is what enforces it.
func invokeWithBound(t *testing.T, bound time.Duration, fn func() (Result, error)) (Result, error) {
	t.Helper()
	done := make(chan invokeOutcome, 1)
	go func() {
		res, err := fn()
		done <- invokeOutcome{res, err}
	}()
	select {
	case o := <-done:
		return o.res, o.err
	case <-time.After(bound):
		t.Fatalf("Invoke did not return within %s — something survived the kill and is holding the stream", bound)
		return Result{}, nil // unreachable; Fatalf exits the test
	}
}
