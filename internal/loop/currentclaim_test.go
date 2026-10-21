package loop

// CLA-708: CURRENT_CLAIM, the record a supervisor uses to hand a dead child's
// claim back, and the driver's expireLease handback for pushed work.
//
// The record's lifecycle is the point: it must exist while a claim is held,
// carry the branch once one is recorded, and be GONE on every path where the
// driver lets go. A stale record makes the supervisor hand back the wrong claim;
// a missing one sends it back to the expiring lease this task exists to avoid.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// --- the record itself -------------------------------------------------------

// CLA-697 x CLA-708: a review panel's members run concurrently and each starts
// its own lease renewer on the SAME run, so every member's observation reaches
// noteCurrentClaim at once. WriteFileAtomic uses a fixed `.tmp` name, so the
// writes have to be serialised or they race on it; the surviving record must
// still be one valid claim, not a torn one.
func TestCurrentClaim_ConcurrentMembersWriteOneValidRecord(t *testing.T) {
	d := &Driver{}
	openTestStateDir(t, d)
	claim := harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.noteCurrentClaim(claim, "proj")
		}()
	}
	wg.Wait()

	got, err := ReadCurrentClaim(d.state)
	if err != nil {
		t.Fatalf("ReadCurrentClaim after concurrent member writes: %v", err)
	}
	if got.TaskID != "t-1" || got.RunID != "r-1" || got.Branch != "clanker/x" || got.Project != "proj" {
		t.Errorf("record = %+v, want the one held claim", got)
	}
}

func TestCurrentClaim_WrittenUpdatedAndRemoved(t *testing.T) {
	d := &Driver{}
	openTestStateDir(t, d)

	d.noteCurrentClaim(harness.Claim{TaskID: "t-1", RunID: "r-1"}, "proj")
	first, err := ReadCurrentClaim(d.state)
	if err != nil {
		t.Fatalf("ReadCurrentClaim after a claim: %v", err)
	}
	if first.TaskID != "t-1" || first.RunID != "r-1" || first.Project != "proj" {
		t.Errorf("record = %+v, want t-1/r-1/proj", first)
	}
	if first.Branch != "" {
		t.Errorf("branch = %q, want empty until one is recorded", first.Branch)
	}
	if first.ClaimedAt.IsZero() {
		t.Error("claimedAt is zero; a supervisor's log line would have no age to report")
	}

	// The branch arrives after the claim (update_task(branch:)). The record must
	// follow it, and claimedAt must keep describing the CLAIM, not the rewrite.
	d.noteCurrentClaim(harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"}, "proj")
	second, err := ReadCurrentClaim(d.state)
	if err != nil {
		t.Fatalf("ReadCurrentClaim after a branch update: %v", err)
	}
	if second.Branch != "clanker/x" {
		t.Errorf("branch = %q, want clanker/x — a supervisor would choose release and post ready over the work", second.Branch)
	}
	if !second.ClaimedAt.Equal(first.ClaimedAt) {
		t.Errorf("claimedAt moved on a rewrite: %s -> %s; it must mean when the claim was TAKEN", first.ClaimedAt, second.ClaimedAt)
	}

	// A settled snapshot is both the hand-over (in_review/done) and the park or
	// block shape — all the session letting go. Nothing is owed afterwards.
	d.noteCurrentClaim(harness.Claim{TaskID: "t-1", RunID: "r-1", Settled: true}, "proj")
	if _, err := ReadCurrentClaim(d.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record survived a settle (err=%v); the next dead child would hand back a claim that was already handed over", err)
	}
}

// A claim for a DIFFERENT task replaces the record rather than inheriting the
// first claim's claimedAt: a drain claims tasks one after another, and the
// supervisor must report the age of the claim it is actually handing back.
func TestCurrentClaim_ANewClaimStartsAFreshHoldTime(t *testing.T) {
	d := &Driver{}
	openTestStateDir(t, d)

	d.noteCurrentClaim(harness.Claim{TaskID: "t-1", RunID: "r-1"}, "proj")
	first, err := ReadCurrentClaim(d.state)
	if err != nil {
		t.Fatal(err)
	}
	d.noteCurrentClaim(harness.Claim{TaskID: "t-2", RunID: "r-2"}, "proj")
	second, err := ReadCurrentClaim(d.state)
	if err != nil {
		t.Fatal(err)
	}
	if second.TaskID != "t-2" || second.RunID != "r-2" {
		t.Errorf("record = %+v, want t-2/r-2", second)
	}
	if second.ClaimedAt.Before(first.ClaimedAt) {
		t.Errorf("the second claim's claimedAt %s is before the first's %s", second.ClaimedAt, first.ClaimedAt)
	}
}

// The record is written from the SAME observation seam the lease renewer uses,
// while the session is still running — not only from what the Result carries
// after it exits, which is exactly what a killed daemon never gets to report.
func TestCurrentClaim_IsWrittenMidSessionAndClearedOnRelease(t *testing.T) {
	cfg := fastCfg()
	cfg.MaxIterations = 1
	rel := &fakeReleaser{}
	var d *Driver
	h := &fakeAdapter{steps: []invokeStep{{
		claims: []harness.Claim{{TaskID: "t-1", RunID: "r-1"}},
		afterClaims: func() {
			waitFor(t, "the claim record to be written mid-session", func() bool {
				c, err := ReadCurrentClaim(d.state)
				return err == nil && c.TaskID == "t-1"
			})
		},
		res: held(okResult(10, 0), openClaim()),
	}}}
	d = NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: renewingReleaser{fakeReleaser: rel, hb: &recordingHeartbeat{}}}})
	openTestStateDir(t, d)

	if _, _, _, err := drainOnce(t, d); err != nil {
		t.Fatalf("drainWithRetries: %v", err)
	}
	// The session held the claim with nothing pushed, so the driver released it:
	// the record must be gone with it.
	if len(rel.calls) != 1 {
		t.Fatalf("release calls = %+v, want exactly 1", rel.calls)
	}
	if _, err := ReadCurrentClaim(d.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record survived a release (err=%v); a supervisor would hand back a claim the driver already handed back", err)
	}
}

// --- the driver's expireLease handback --------------------------------------

func pushedWorkResult() harness.Result {
	return harness.Result{Claim: harness.Claim{
		TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/t-1-work",
	}}
}

func TestReleaseHeldClaim_EndsTheLeaseOnPushedWork(t *testing.T) {
	logs := captureLogs(t)
	rel := &fakeReleaser{}
	d := &Driver{}
	openTestStateDir(t, d)
	d.noteCurrentClaim(pushedWorkResult().Claim, "proj")

	released := d.releaseHeldClaim(context.Background(), Target{Name: "proj", Releaser: rel}, pushedWorkResult(), true)
	if released {
		t.Error("an expired lease is not a task back in the ready queue; a seam retry must not re-claim it as one")
	}
	if len(rel.expireCalls) != 1 || rel.expireCalls[0] != (releaseCall{"t-1", "r-1"}) {
		t.Fatalf("expireLease calls = %+v, want exactly t-1/r-1", rel.expireCalls)
	}
	if len(rel.calls) != 0 {
		t.Errorf("released %+v — a claim with pushed work must keep its takeover hand-off", rel.calls)
	}
	if d.undeclared != 1 {
		t.Errorf("undeclared = %d, want 1 — the phase still failed to hand the task over", d.undeclared)
	}
	out := logs.String()
	if !strings.Contains(out, "ended the lease on t-1 (pushed work kept for takeover)") {
		t.Errorf("the log must say the lease was ended and the work kept; got:\n%s", out)
	}
	if strings.Contains(out, "leaving the lease to expire") {
		t.Errorf("the old leave-to-expire line must not survive a successful handback:\n%s", out)
	}
	if _, err := ReadCurrentClaim(d.state); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("record survived the expire handback (err=%v)", err)
	}
}

// A plane that does not serve release_task (an older protocol) is not a failed
// handback — it is the old behaviour, announced once — and the old line stays,
// because it is still what happened.
func TestReleaseHeldClaim_FallsBackWhenThePlanePredatesExpireLease(t *testing.T) {
	logs := captureLogs(t)
	rel := &fakeReleaser{expireErr: fmt.Errorf("%w: MCP error -32602: Tool release_task not found", plane.ErrPlaneTooOld)}
	d := &Driver{}

	for i := 0; i < 2; i++ {
		d.releaseHeldClaim(context.Background(), Target{Name: "proj", Releaser: rel}, pushedWorkResult(), true)
	}
	if len(rel.expireCalls) != 2 {
		t.Errorf("expireLease attempted %d times, want 2 before the fallback", len(rel.expireCalls))
	}
	if len(rel.calls) != 0 {
		t.Errorf("fell back to Release (%+v) — that is the one call forbidden for pushed work", rel.calls)
	}
	out := logs.String()
	if !strings.Contains(out, "leaving the lease to expire so the takeover hand-off survives") {
		t.Errorf("the old behaviour line is missing:\n%s", out)
	}
	if got := strings.Count(out, "does not serve this build's task tools"); got != 1 {
		t.Errorf("the unsupported-plane line appeared %d times, want once per run:\n%s", got, out)
	}
}

func TestReleaseHeldClaim_ReportsAFailedExpire(t *testing.T) {
	logs := captureLogs(t)
	rel := &fakeReleaser{expireErr: errors.New("plane unavailable")}
	d := &Driver{}

	d.releaseHeldClaim(context.Background(), Target{Name: "proj", Releaser: rel}, pushedWorkResult(), false)

	out := logs.String()
	if !strings.Contains(out, "could not end the lease on t-1") {
		t.Errorf("a failed expire must be named:\n%s", out)
	}
	if !strings.Contains(out, "leaving the lease to expire so the takeover hand-off survives") {
		t.Errorf("the fallback behaviour line is missing:\n%s", out)
	}
}

// The untrusted path must not call expireLease: the output could not be read
// whole, so the driver may be wrong about whether the task is still held.
func TestReleaseHeldClaim_UntrustedNeverExpiresTheLease(t *testing.T) {
	rel := &fakeReleaser{}
	d := &Driver{}
	d.releaseHeldClaim(context.Background(), Target{Name: "proj", Releaser: rel}, held(untrustedResult(0, 0), openClaim()), true)
	if len(rel.expireCalls) != 0 {
		t.Errorf("expireLease called on a truncated stream: %+v", rel.expireCalls)
	}
	if len(rel.calls) != 0 {
		t.Errorf("release called on a truncated stream: %+v", rel.calls)
	}
}

// A settle observed mid-session — the session moving the task to in_review,
// done, parked or blocked — is the plan's "hand-over/park" let-go path. The
// record must be gone BEFORE the session's result is read, because a daemon
// that dies after the settle must not hand back a task it already handed over.
func TestCurrentClaim_SettledClaimIsRemoved(t *testing.T) {
	cfg := fastCfg()
	cfg.MaxIterations = 1
	rel := &fakeReleaser{}
	var d *Driver
	h := &fakeAdapter{steps: []invokeStep{{
		claims: []harness.Claim{{TaskID: "t-1", RunID: "r-1", Settled: true}},
		afterClaims: func() {
			waitFor(t, "the record to be removed on settle", func() bool {
				_, err := ReadCurrentClaim(d.state)
				return errors.Is(err, os.ErrNotExist)
			})
		},
		res: held(okResult(10, 0), harness.Claim{TaskID: "t-1", RunID: "r-1", Settled: true}),
	}}}
	d = NewMulti(cfg, h, []Target{{Poller: busyPoller(), Releaser: renewingReleaser{fakeReleaser: rel, hb: &recordingHeartbeat{}}}})
	openTestStateDir(t, d)
	// The session must have a record to remove.
	d.noteCurrentClaim(harness.Claim{TaskID: "t-1", RunID: "r-1"}, "")

	if _, _, _, err := drainOnce(t, d); err != nil {
		t.Fatalf("drainWithRetries: %v", err)
	}
	if len(rel.calls) != 0 || len(rel.expireCalls) != 0 {
		t.Errorf("a settled claim was handed back: releases=%+v expires=%+v", rel.calls, rel.expireCalls)
	}
	if _, err := ReadCurrentClaim(d.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record survived a settle (err=%v)", err)
	}
}

// A driver-side park moves the task out of the holding run just as a settle
// does, so the record must go with it. Otherwise a daemon that crashed after
// parking would leave a supervisor trying to expire a run that already ended.
func TestCurrentClaim_ParkClearsTheRecord(t *testing.T) {
	d := &Driver{}
	openTestStateDir(t, d)
	d.noteCurrentClaim(harness.Claim{TaskID: "t-1", RunID: "r-1"}, "proj")

	pk := &parkingReleaser{}
	res := held(okResult(1, 0), harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/x"})
	d.parkDeadPhase(context.Background(), Target{Name: "proj", Releaser: pk}, "implement", &res)

	if len(pk.parks) != 1 {
		t.Fatalf("park calls = %+v, want exactly 1", pk.parks)
	}
	if _, err := ReadCurrentClaim(d.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record survived a park (err=%v)", err)
	}
}

// The branch must be on disk before the observation callback returns, not when
// the renewer goroutine next reaches the snapshot (review 1.review-1.1). The
// renewer is parked INSIDE a Heartbeat here: before the fix the branch stayed
// queued in r.claims, and a daemon dying in that window left the old
// no-branch record for the supervisor to `release` over pushed work.
func TestCurrentClaim_BranchIsPersistedBeforeTheObservationReturns(t *testing.T) {
	hb := &recordingHeartbeat{stall: make(chan struct{}), entered: make(chan struct{})}
	ticks := make(chan time.Time, 1)
	d := &Driver{}
	openTestStateDir(t, d)
	r := &leaseRenewer{
		hb:        hb,
		interval:  time.Minute, // cadence is hand-driven via r.tick
		prefix:    "test: ",
		claims:    make(chan harness.Claim, 4),
		done:      make(chan struct{}),
		stopped:   make(chan struct{}),
		tick:      ticks,
		claimFile: func(c harness.Claim) { d.noteCurrentClaim(c, "proj") },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() {
		close(hb.stall)
		r.stop()
	}()
	go r.run(ctx, harness.Claim{TaskID: "t-1", RunID: "r-1"})

	ticks <- time.Now()
	heartbeatWaitFor(t, "the renewer to park inside the stalled heartbeat", func() bool {
		select {
		case <-hb.entered:
			return true
		default:
			return false
		}
	})

	// The record starts with no branch, as it would before update_task.
	r.observe(harness.Claim{TaskID: "t-1", RunID: "r-1"})
	c, err := ReadCurrentClaim(d.state)
	if err != nil || c.Branch != "" {
		t.Fatalf("precondition: record = %+v, err = %v; want a no-branch record", c, err)
	}

	// The branch observation arrives while the renewer is still parked. Read
	// the file WITHOUT releasing the heartbeat: the write must not wait for
	// the renewer goroutine to drain its queue.
	r.observe(harness.Claim{TaskID: "t-1", RunID: "r-1", HasWIP: true, Branch: "clanker/t-1-work"})
	c, err = ReadCurrentClaim(d.state)
	if err != nil {
		t.Fatalf("branch observation was not persisted while the renewer was parked: %v", err)
	}
	if c.Branch != "clanker/t-1-work" {
		t.Fatalf("branch on disk = %q, want clanker/t-1-work — a supervisor would choose release and post ready over the work", c.Branch)
	}
}
