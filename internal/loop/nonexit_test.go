package loop

import (
	"errors"
	"testing"
)

// CLA-299: an Invoke whose run failed with something other than an exit status
// still returns a fully parsed Result — a session that spent and died in Wait.
// The drain must honour that on both counts the fix names: releaseHeldClaim
// (which runs above the error check) sees the claim the stream carried, and the
// budget accumulator counts the spend even though the attempt ends as an error
// rather than a classified verdict.
func TestDrainHandsBackAndCountsSpendWhenInvokeDiesInWait(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 1

	// waitDeath stands in for exec.ErrDelay-class failures: NOT an
	// *exec.ExitError, so res-alongside-err is the driver's only signal.
	waitDeath := errors.New("exec: WaitDelay expired before I/O complete")
	h := &fakeAdapter{steps: []invokeStep{{
		res: held(okResult(1200, 0.5), openClaim()),
		err: waitDeath,
	}}}
	rel := &fakeReleaser{}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)

	tokens, cost, _, err := drainOnce(t, d)
	if !errors.Is(err, waitDeath) {
		t.Fatalf("drain error = %v — an Invoke run failure must end the iteration loudly, wrapping the failure", err)
	}
	if len(rel.calls) != 1 || rel.calls[0] != (releaseCall{"t-1", "r-1"}) {
		t.Fatalf("release calls = %+v, want exactly [{t-1 r-1}] — a claim observed on a stream that died untidily is real and must be handed back", rel.calls)
	}
	if tokens != 1200 || cost != 0.5 {
		t.Errorf("drain returned tokens=%d cost=$%.2f, want 1200/$0.50 — the budget accumulator must see a session that spent and died in Wait, not lose it to the error return", tokens, cost)
	}
}

// The boundary case of the same arm: a LAUNCH failure also arrives as
// res-alongside-err, but its Result is an honest zero, so counting it changes
// nothing and releasing it releases nothing.
func TestDrainLaunchFailureSpendsNothingAndReleasesNothing(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 1

	launchFailure := errors.New("exec: no such file or directory")
	h := &fakeAdapter{steps: []invokeStep{{
		res: okResult(0, 0), // what every adapter's launch-failure path actually builds
		err: launchFailure,
	}}}
	rel := &fakeReleaser{}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)

	tokens, cost, _, err := drainOnce(t, d)
	if !errors.Is(err, launchFailure) {
		t.Fatalf("drain error = %v, want the launch failure wrapped", err)
	}
	if len(rel.calls) != 0 {
		t.Fatalf("release calls = %+v — a zero Result releases nothing; reporting a release would erase a predecessor's live claim", rel.calls)
	}
	if tokens != 0 || cost != 0 {
		t.Errorf("drain returned tokens=%d cost=$%.2f for a session that never ran", tokens, cost)
	}
}

// The CLA-262 boundary of the same arm: a run failure whose stream ALSO came
// back untrusted carries figures that are a floor, not a total, so they count
// nowhere - the same refusal the untrusted branch and endUntrustedDrain make -
// and releaseHeldClaim leaves the lease to expire rather than acting on claim
// state read through a hole. The error itself still propagates: an attempt that
// failed is an attempt that failed, whatever its stream looked like.
func TestDrainCountsNothingWhenAFailedStreamIsAlsoUntrusted(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 1

	waitDeath := errors.New("exec: WaitDelay expired before I/O complete")
	truncated := held(okResult(1200, 0.5), openClaim())
	truncated.Untrusted = "claude's stdout could not be read to the end"
	h := &fakeAdapter{steps: []invokeStep{{
		res: truncated,
		err: waitDeath,
	}}}
	rel := &fakeReleaser{}
	d := NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: rel}})
	openTestStateDir(t, d)

	tokens, cost, _, err := drainOnce(t, d)
	if !errors.Is(err, waitDeath) {
		t.Fatalf("drain error = %v - an untrusted stream must not soften a real invoke failure into silence", err)
	}
	if len(rel.calls) != 0 {
		t.Fatalf("release calls = %+v - claim state read from an unread-whole stream must not be acted on: the settle we never saw may sit in the bytes that never arrived (CLA-262)", rel.calls)
	}
	if tokens != 0 || cost != 0 {
		t.Errorf("drain returned tokens=%d cost=$%.2f - figures parsed from an unread-whole stream are a floor, not a total, and must never reach the accumulators", tokens, cost)
	}
}
