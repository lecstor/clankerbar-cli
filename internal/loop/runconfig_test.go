package loop

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// fakeRCfg answers get_project_run_config from a version -> raw-document map,
// appending "fetch" to the shared event timeline on every call. A version with
// no document answers ErrNoConfig; errOn makes exactly that Nth fetch fail.
type fakeRCfg struct {
	docs    map[int]string
	fetches []int
	errOn   int
	errDone bool
	events  *[]string
}

func (f *fakeRCfg) note(s string) {
	if f.events != nil {
		*f.events = append(*f.events, s)
	}
}

func (f *fakeRCfg) RunConfig(context.Context) (*plane.RunConfigState, error) {
	f.note("fetch")
	// next counts VERSIONS independently of failures: a failed fetch of v1 does
	// not make the next fetch ask for v1 again.
	v := len(f.fetches) + 1
	if f.errOn == v && !f.errDone {
		f.errDone = true
		f.fetches = append(f.fetches, v)
		return nil, errors.New("plane unreachable")
	}
	f.fetches = append(f.fetches, v)
	raw, ok := f.docs[v]
	if !ok {
		return nil, plane.ErrNoConfig
	}
	return &plane.RunConfigState{Version: v, Config: []byte(raw)}, nil
}

// recordingAdapter logs each Invoke onto the SAME timeline as the config
// fetches, so their relative order is assertable.
type recordingAdapter struct {
	*fakeAdapter
	events *[]string
}

func (r *recordingAdapter) Invoke(ctx context.Context, in harness.Invocation) (harness.Result, error) {
	*r.events = append(*r.events, "invoke")
	return r.fakeAdapter.Invoke(ctx, in)
}

// rcRunDriver wires one target over a scripted summary and run-config reader,
// then runs the driver to its max-iterations stop.
func rcRunDriver(t *testing.T, cfg *config.Config, sums []backlog.Summary, rc *fakeRCfg, h harness.Adapter) *Driver {
	t.Helper()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = len(sums)
	d := NewMulti(cfg, h, []Target{{Poller: &fakePoller{sums: sums}, RCfg: rc}})
	openTestStateDir(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return d
}

// THE boundary rule: a moved runConfigVersion is honoured between iterations -
// each drain runs on the document current at ITS boundary.
func TestRun_RunConfigAppliesAtTheIterationBoundary(t *testing.T) {
	var events []string
	h := &recordingAdapter{fakeAdapter: &fakeAdapter{}, events: &events}
	rc := &fakeRCfg{
		docs: map[int]string{
			1: `{"model":"plane-1"}`,
			2: `{"model":"plane-2"}`,
		},
		events: &events,
	}

	d := rcRunDriver(t, fastCfg(), []backlog.Summary{
		{Ready: 1, Claimable: 1, RunConfigVersion: 1},
		{Ready: 1, Claimable: 1, RunConfigVersion: 2},
	}, rc, h)

	if len(h.invocations) != 2 {
		t.Fatalf("ran %d sessions, want 2", len(h.invocations))
	}
	if got := h.invocations[0].Model; got != "plane-1" {
		t.Errorf("drain 1 ran on model %q, want v1's plane-1", got)
	}
	if got := h.invocations[1].Model; got != "plane-2" {
		t.Errorf("drain 2 ran on model %q, want v2's plane-2 (applied at the boundary, not mid-run)", got)
	}
	if d.targets[0].Cfg == nil || d.rcVersions[0] != 2 {
		t.Errorf("target overlay in force = %t at version %d, want true/2", d.targets[0].Cfg != nil, d.rcVersions[0])
	}
}

// The reader is only ever consulted at an iteration boundary: every fetch
// lands before the first session or directly after a completed one - never
// inside a session. Invoke is atomic inside the fake, so any other order is a
// mid-session read.
func TestRun_TheConfigReaderIsNeverConsultedMidSession(t *testing.T) {
	var events []string
	h := &recordingAdapter{fakeAdapter: &fakeAdapter{}, events: &events}
	rc := &fakeRCfg{docs: map[int]string{1: `{"model":"m"}`}, events: &events}

	rcRunDriver(t, fastCfg(), []backlog.Summary{
		{Ready: 1, Claimable: 1, RunConfigVersion: 1},
		{Ready: 1, Claimable: 1, RunConfigVersion: 1},
	}, rc, h)

	fetches, invokes := 0, 0
	for i, e := range events {
		switch e {
		case "fetch":
			fetches++
			if i > 0 && events[i-1] != "invoke" {
				t.Fatalf("a config fetch followed %q, not a completed session: %v", events[i-1], events[:i+1])
			}
		case "invoke":
			invokes++
		}
	}
	if fetches == 0 || invokes == 0 {
		t.Fatalf("nothing was exercised: fetches=%d invokes=%d (%v)", fetches, invokes, events)
	}
}

// No stored config (version 0): byte-for-byte fallback - both drains run on
// the LOCAL model and no effective overlay comes to exist. Version 0 IS the
// plane's "never set" answer (CLA-481), so the loop latches it without a fetch:
// zero fetches, zero log lines, across any number of boundaries.
func TestRun_NoStoredConfigKeepsTheLocalRules(t *testing.T) {
	var events []string
	h := &recordingAdapter{fakeAdapter: &fakeAdapter{}, events: &events}
	rc := &fakeRCfg{events: &events} // no docs -> ErrNoConfig at any version
	cfg := fastCfg()
	cfg.Model = "local-model"
	buf := captureLogs(t)

	d := rcRunDriver(t, cfg, []backlog.Summary{
		{Ready: 1, Claimable: 1, RunConfigVersion: 0},
		{Ready: 1, Claimable: 1, RunConfigVersion: 0},
		{Ready: 1, Claimable: 1, RunConfigVersion: 0},
	}, rc, h)

	for i, inv := range h.invocations {
		if inv.Model != "local-model" {
			t.Errorf("drain %d ran on %q, want the local file's model", i+1, inv.Model)
		}
	}
	if len(rc.fetches) != 0 {
		t.Errorf("version-0 polls fetched %d time(s), want 0 (0 already means nothing stored)", len(rc.fetches))
	}
	if logs := buf.String(); strings.Contains(logs, "run-config") {
		t.Errorf("version-0 polls logged, want silence; log had: %s", logs)
	}
	if d.targets[0].Cfg != nil {
		t.Error("a project with nothing stored grew an effective config overlay")
	}
	if d.rcVersions[0] != 0 {
		t.Errorf("rcVersions = %d, want 0 latched without a fetch", d.rcVersions[0])
	}
}

// A failed fetch keeps the previous config for THAT boundary and still applies
// the next version when it arrives - a blip delays by one boundary rather than
// wedging the run on stale policy forever.
func TestRun_AFailedFetchKeepsLocalThenTheNextVersionApplies(t *testing.T) {
	var events []string
	h := &recordingAdapter{fakeAdapter: &fakeAdapter{}, events: &events}
	rc := &fakeRCfg{docs: map[int]string{2: `{"model":"plane-2"}`}, errOn: 1, events: &events}
	cfg := fastCfg()
	cfg.Model = "local-model"

	d := rcRunDriver(t, cfg, []backlog.Summary{
		{Ready: 1, Claimable: 1, RunConfigVersion: 1},
		{Ready: 1, Claimable: 1, RunConfigVersion: 2},
	}, rc, h)

	if h.invocations[0].Model != "local-model" {
		t.Errorf("drain 1 ran on %q after a failed fetch, want local rules kept", h.invocations[0].Model)
	}
	if h.invocations[1].Model != "plane-2" {
		t.Errorf("drain 2 ran on %q, want v2 applied once the fetch succeeded", h.invocations[1].Model)
	}
	if d.rcVersions[0] != 2 {
		t.Errorf("rcVersions = %d, want 2", d.rcVersions[0])
	}
}

// notWiredRCfg is the slug-less / project-scoped-key shape (CLA-481): every
// fetch answers plane.ErrNotWired, the way rcNotWired does when NewRunConfigAPI
// gets no endpoint or key.
type notWiredRCfg struct {
	fetches int
	events  *[]string
}

func (f *notWiredRCfg) RunConfig(context.Context) (*plane.RunConfigState, error) {
	f.fetches++
	if f.events != nil {
		*f.events = append(*f.events, "fetch")
	}
	return nil, plane.ErrNotWired
}

// A not-wired reader latches like nothing-stored (CLA-481): one fetch, silence
// thereafter, never retried across boundaries — not a 30s log for the daemon's
// lifetime.
func TestRun_ErrNotWiredLatchesSilently(t *testing.T) {
	var events []string
	h := &recordingAdapter{fakeAdapter: &fakeAdapter{}, events: &events}
	rc := &notWiredRCfg{events: &events}
	cfg := fastCfg()
	cfg.Model = "local-model"
	buf := captureLogs(t)

	d := NewMulti(cfg, h, []Target{{Poller: &fakePoller{sums: []backlog.Summary{
		{Ready: 1, Claimable: 1, RunConfigVersion: 1},
		{Ready: 1, Claimable: 1, RunConfigVersion: 1},
		{Ready: 1, Claimable: 1, RunConfigVersion: 1},
	}}, RCfg: rc}})
	cfg.StateDir = t.TempDir()
	// rcRunDriver sets StateDir/MaxIterations itself; replicate the few lines
	// here because that helper only takes *fakeRCfg.
	d.cfg.StateDir = cfg.StateDir
	d.cfg.MaxIterations = 3
	openTestStateDir(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for i, inv := range h.invocations {
		if inv.Model != "local-model" {
			t.Errorf("drain %d ran on %q, want the local file's model", i+1, inv.Model)
		}
	}
	if rc.fetches != 1 {
		t.Errorf("ErrNotWired fetched %d time(s), want exactly 1 (latch, never retry)", rc.fetches)
	}
	if logs := buf.String(); strings.Contains(logs, "run-config") {
		t.Errorf("ErrNotWired logged, want silence; log had: %s", logs)
	}
	if d.targets[0].Cfg != nil {
		t.Error("a not-wired target grew an effective config overlay")
	}
	if d.rcVersions[0] != 1 {
		t.Errorf("rcVersions = %d, want 1 latched", d.rcVersions[0])
	}
}

// The transient backoff widens instead of fixed 30s spam (CLA-481): 30s, 60s,
// 120s, 240s, then the 5m cap.
func TestRCFetchBackoff_Widens(t *testing.T) {
	want := map[int]time.Duration{
		1:  30 * time.Second,
		2:  60 * time.Second,
		3:  120 * time.Second,
		4:  240 * time.Second,
		5:  300 * time.Second,
		6:  300 * time.Second,
		10: 300 * time.Second,
	}
	for fails, w := range want {
		if got := rcFetchBackoff(fails); got != w {
			t.Errorf("rcFetchBackoff(%d) = %s, want %s", fails, got, w)
		}
	}
}

// alwaysFailRCfg fails every fetch transiently, so the test can drive the
// boundary directly and watch the window widen without waiting out real time.
type alwaysFailRCfg struct {
	fetches int
}

func (f *alwaysFailRCfg) RunConfig(context.Context) (*plane.RunConfigState, error) {
	f.fetches++
	return nil, errors.New("plane unreachable")
}

// Consecutive transient failures of the SAME version widen the retry window
// (CLA-481): the second retry waits ~60s, the third ~120s — not a fixed 30s.
// A different version still fetches immediately.
func TestCheckRunConfigVersion_TransientBackoffWidens(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	h := &fakeAdapter{}
	rc := &alwaysFailRCfg{}
	d := NewMulti(cfg, h, []Target{{Poller: &fakePoller{}, RCfg: rc}})
	openTestStateDir(t, d)
	ctx := context.Background()
	buf := captureLogs(t)

	d.checkRunConfigVersion(ctx, 0, &d.targets[0], 1)
	if rc.fetches != 1 || d.rcFails[0] != 1 {
		t.Fatalf("after 1st failure: fetches=%d fails=%d, want 1/1", rc.fetches, d.rcFails[0])
	}
	firstWait := time.Until(d.rcAttempt[0])

	// Within the window the same version does not refetch (and logs nothing new).
	d.checkRunConfigVersion(ctx, 0, &d.targets[0], 1)
	if rc.fetches != 1 {
		t.Fatalf("throttled retry fetched, want no fetch within the window (fetches=%d)", rc.fetches)
	}

	// Past the window it refetches, and the new window is wider.
	d.rcAttempt[0] = time.Now().Add(-time.Second)
	d.checkRunConfigVersion(ctx, 0, &d.targets[0], 1)
	if rc.fetches != 2 || d.rcFails[0] != 2 {
		t.Fatalf("after 2nd failure: fetches=%d fails=%d, want 2/2", rc.fetches, d.rcFails[0])
	}
	secondWait := time.Until(d.rcAttempt[0])
	if secondWait <= firstWait {
		t.Errorf("second window %s did not widen past first %s", secondWait, firstWait)
	}
	if secondWait < 55*time.Second || secondWait > 65*time.Second {
		t.Errorf("second window = %s, want ~60s", secondWait)
	}

	d.rcAttempt[0] = time.Now().Add(-time.Second)
	d.checkRunConfigVersion(ctx, 0, &d.targets[0], 1)
	if rc.fetches != 3 || d.rcFails[0] != 3 {
		t.Fatalf("after 3rd failure: fetches=%d fails=%d, want 3/3", rc.fetches, d.rcFails[0])
	}
	thirdWait := time.Until(d.rcAttempt[0])
	if thirdWait <= secondWait {
		t.Errorf("third window %s did not widen past second %s", thirdWait, secondWait)
	}
	if thirdWait < 115*time.Second || thirdWait > 125*time.Second {
		t.Errorf("third window = %s, want ~120s", thirdWait)
	}

	// Three fetches logged three times — the widening is in WHEN fetches
	// happen, not in suppressing the log for a fetch that did happen.
	if n := strings.Count(buf.String(), "fetch failed"); n != 3 {
		t.Errorf("logged %d fetch failures, want 3 (one per fetch)", n)
	}

	// A different version is never throttled by the old version's window.
	d.rcAttempt[0] = time.Now().Add(time.Hour) // would block version 1
	before := rc.fetches
	d.checkRunConfigVersion(ctx, 0, &d.targets[0], 2)
	if rc.fetches != before+1 {
		t.Errorf("a new version did not fetch immediately past the old window (fetches=%d, want %d)", rc.fetches, before+1)
	}
	if d.rcFails[0] != 1 {
		t.Errorf("a new version's first failure has fails=%d, want 1 (count restarts per version)", d.rcFails[0])
	}
}

// A stored document the LOCAL rules refuse keeps the previous config and says
// so loudly - a ratified-but-broken document must not become a half-running
// daemon, nor silently keep running as if nothing arrived.
func TestRun_AnOverlaidConfigValidateRefusesStaysPut(t *testing.T) {
	var events []string
	h := &recordingAdapter{fakeAdapter: &fakeAdapter{}, events: &events}
	// "not-a-harness" fails Validate against the registry, the same way a
	// mistyped local file value would.
	rc := &fakeRCfg{docs: map[int]string{1: `{"harness":"not-a-harness"}`}, events: &events}
	cfg := fastCfg()
	cfg.Model = "local-model"
	buf := captureLogs(t)

	rcRunDriver(t, cfg, []backlog.Summary{
		{Ready: 1, Claimable: 1, RunConfigVersion: 1},
		{Ready: 1, Claimable: 1, RunConfigVersion: 1},
	}, rc, h)

	if logs := buf.String(); !strings.Contains(logs, "REFUSED locally") {
		t.Errorf("the refusal was not logged loudly; log had: %s", logs)
	}
	for i, inv := range h.invocations {
		if inv.Model != "local-model" {
			t.Errorf("drain %d ran on %q despite a refused document", i+1, inv.Model)
		}
	}
}

// cli#118: the review-tier escalation evaluation reads PLANE-declared rules -
// they land in the target's effective config and resolve exactly as when they
// came from the local file.
func TestApplyRunConfig_StoredEscalationRulesReachTheEvaluation(t *testing.T) {
	cfg := fastCfg()
	doc := &config.RunConfigDoc{
		SchemaVersion: 1,
		Models:        map[string]string{"strong": "plane-strong"},
		Escalation:    &config.RunConfigEscalation{CategoryRules: map[string]string{"bug": "strong"}},
	}
	eff := cfg.Clone()
	eff.ApplyRunConfig(doc)

	tier, rule := eff.Escalation.Evaluate(nil, "bug")
	if tier != "strong" || !strings.Contains(rule, "bug") {
		t.Fatalf("stored category rule gave tier=%q rule=%q", tier, rule)
	}
	model, ok := eff.ModelForPhase(config.Phase{Tier: tier})
	if !ok || model != "plane-strong" {
		t.Errorf("escalated tier resolved to model=%q ok=%t, want plane-strong via the STORED map", model, ok)
	}
}

// stableRCfg returns the SAME document on every fetch — the shape the reload
// test needs, where the poll's runConfigVersion never moves and only a RELOAD
// invalidation can force a refetch. It counts fetches so the test can assert
// the rebuild actually happened.
type stableRCfg struct {
	doc     string
	fetches int
}

func (s *stableRCfg) RunConfig(context.Context) (*plane.RunConfigState, error) {
	s.fetches++
	return &plane.RunConfigState{Version: 1, Config: []byte(s.doc)}, nil
}

// A RELOAD swaps the local file; an in-force stored overlay must be rebuilt on
// the FRESH base, not keep running the pre-reload one. The overlay only carries
// the movable half, so the effective config's non-movable half (prompt, phases)
// comes from the base it was built on: a stale overlay would keep the pre-reload
// prompt while the reloaded base moves on. The poll's version never moves here,
// so the refetch can only be the RELOAD invalidation's doing — and the overlay
// surviving means the stored document was preserved across the rebuild, not
// dropped.
func TestRun_ReloadRebuildsAnInForceStoredOverlayOnTheFreshBase(t *testing.T) {
	dir := t.TempDir()
	base := fastCfg()
	base.StateDir = dir
	base.Prompt = "original brief"
	base.Model = "base-1"
	base.MaxIterations = 10
	h := &hookedAdapter{fakeAdapter: fakeAdapter{steps: []invokeStep{{res: okResult(0, 0)}, {res: okResult(0, 0)}}}}
	h.onInvoke = func(n int, _ context.Context) {
		switch n {
		case 0:
			if err := plantMarker(dir, MarkerReload, ""); err != nil {
				t.Errorf("planting RELOAD: %v", err)
			}
		case 1:
			if err := plantMarker(dir, "STOP", ""); err != nil {
				t.Errorf("planting STOP: %v", err)
			}
		}
	}
	rc := &stableRCfg{doc: `{"model":"plane-1"}`}
	p := &fakePoller{sum: backlog.Summary{Claimable: 1, RunConfigVersion: 1}}
	// No openTestStateDir here: the markers are planted at base.StateDir, so Run
	// must open the state dir from the config (as the control tests do) rather
	// than from a throwaway temp dir the markers would never reach.
	d := NewMulti(base, h, []Target{{Poller: p, RCfg: rc}})
	reloaded := false
	d.SetReloader(func() (*config.Config, error) {
		fresh := fastCfg()
		fresh.StateDir = dir
		fresh.Prompt = "reloaded brief"
		fresh.Model = "base-2"
		reloaded = true
		return fresh, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reloaded {
		t.Fatal("the reload closure never ran")
	}
	if len(h.invocations) != 2 {
		t.Fatalf("ran %d sessions, want 2", len(h.invocations))
	}
	if rc.fetches != 2 {
		t.Errorf("the stored config was fetched %d time(s), want 2 (once at start of run, once after the RELOAD rebuild)", rc.fetches)
	}
	// Both drains run on the stored document's model: the overlay survives the
	// reload (rebuilt), it is not dropped back to the local file's model.
	for i, inv := range h.invocations {
		if inv.Model != "plane-1" {
			t.Errorf("drain %d ran on model %q, want the stored plane-1 preserved across the reload", i+1, inv.Model)
		}
	}
	if got := h.invocations[0].Prompt; !strings.Contains(got, "original brief") {
		t.Errorf("the pre-reload session ran on prompt %q, want the original brief", got)
	}
	// The base's non-movable half follows the RELOAD: the second session must
	// run on the reloaded prompt (rebuilt overlay on the fresh base), not the
	// stale pre-reload one.
	if got := h.invocations[1].Prompt; !strings.Contains(got, "reloaded brief") {
		t.Errorf("the session after RELOAD ran on prompt %q, want the reloaded brief (the overlay must rebuild on the fresh base)", got)
	}
	if d.targets[0].Cfg == nil {
		t.Error("the stored overlay should still be in force after the reload")
	}
	markerAbsent(t, dir, MarkerReload)
}
