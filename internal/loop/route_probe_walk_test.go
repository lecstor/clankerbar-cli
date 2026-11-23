package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// CLA-984: route marks must cover every route a limit was seen on. A wait probe
// walks the route list instead of re-testing the first routable route, and a
// session offered one route still reports it so the driver marks it.

// persistedRouteMarks reads ROUTE_MARKS back from the driver's state dir.
func persistedRouteMarks(t *testing.T, d *Driver) map[string]time.Time {
	t.Helper()
	b, err := d.state.ReadFile(statedir.RouteMarksName)
	if err != nil {
		t.Fatalf("reading %s: %v", statedir.RouteMarksName, err)
	}
	marks := map[string]time.Time{}
	if err := json.Unmarshal(b, &marks); err != nil {
		t.Fatalf("parsing %s %q: %v", statedir.RouteMarksName, b, err)
	}
	return marks
}

// A route that keeps refusing with no stated reset must not mask an open route
// behind it: the wake marks it and probes the next route, and resumes there.
func TestSupervisedWait_ALimitedFirstRouteIsMarkedAndTheNextRouteProbedInTheSameWake(t *testing.T) {
	statedReset := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name    string
		resetAt time.Time
	}{
		{"no stated reset marks for the poll interval", time.Time{}},
		{"a stated reset is the mark", statedReset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const interval = 200 * time.Millisecond
			// Past the fourth probe every route answers open, so a wait that never leaves the first route
			// fails the assertions below instead of hanging the package.
			h := &fakeAdapter{probeTokens: 5, probeLimitFor: func(call int, model string) harness.Limit {
				if model == routeFreeAlias && call < 4 {
					return harness.Limit{Limited: true, ResetAt: tc.resetAt}
				}
				return harness.Limit{Limited: false}
			}}
			d := routedDriver(t, h, twoPhases(), routeFreeAlias, routePaidAlias)
			d.cfg.PollInterval = config.Duration(interval)

			logs := captureLogs(t)
			start := time.Now()
			tokens, _, stop := d.supervisedWait(context.Background(), harness.Limit{Limited: true},
				h, d.cfg.Phases[0], d.targets[0], spend{start: start})
			elapsed := time.Since(start)

			if stop {
				t.Fatalf("the paid route was open; the wait should resume, not stop\n%s", logs.String())
			}
			if len(h.probeModels) != 2 || h.probeModels[0] != routeFreeAlias || h.probeModels[1] != routePaidAlias {
				t.Fatalf("probes ran on %v, want [%s %s] in one wake\n%s", h.probeModels, routeFreeAlias, routePaidAlias, logs.String())
			}
			if elapsed >= 2*interval {
				t.Errorf("resumed after %s, want the first wake (interval %s)\n%s", elapsed.Round(time.Millisecond), interval, logs.String())
			}
			if tokens != 10 || d.targetSpendAt(0).tokens != 10 {
				t.Errorf("tokens returned=%d ledger=%d, want both probes (10) charged", tokens, d.targetSpendAt(0).tokens)
			}
			marks := persistedRouteMarks(t, d)
			until, ok := marks[routeFreeAlias]
			if !ok {
				t.Fatalf("ROUTE_MARKS = %v, want the refusing route %s marked", marks, routeFreeAlias)
			}
			if _, ok := marks[routePaidAlias]; ok {
				t.Errorf("ROUTE_MARKS = %v, the route that probed open must not be marked", marks)
			}
			if tc.resetAt.IsZero() {
				// Marked at probe time, one poll interval into the wait, for one poll interval.
				if until.Before(start.Add(2*interval)) || until.After(start.Add(elapsed+interval)) {
					t.Errorf("mark lifts %s after the wait began, want one poll interval (%s) past the probe", until.Sub(start), interval)
				}
			} else if !until.Equal(tc.resetAt) {
				t.Errorf("mark = %s, want the stated reset %s", until, tc.resetAt)
			}
		})
	}
}

// With every route refusing, each wake probes each route at most once and then
// sleeps the poll interval: walking the list must not become a tight loop, and
// every probe is still a paid session on the ledger.
func TestSupervisedWait_EveryRouteLimitedStillSleepsThePollIntervalAndChargesEveryProbe(t *testing.T) {
	const interval = 80 * time.Millisecond
	const limitedProbes = 6
	h := &fakeAdapter{probeTokens: 7, probeLimitFor: func(call int, _ string) harness.Limit {
		return harness.Limit{Limited: call < limitedProbes}
	}}
	d := routedDriver(t, h, twoPhases(), routeFreeAlias, routePaidAlias)
	d.cfg.PollInterval = config.Duration(interval)

	logs := captureLogs(t)
	start := time.Now()
	tokens, _, stop := d.supervisedWait(context.Background(), harness.Limit{Limited: true},
		h, d.cfg.Phases[0], d.targets[0], spend{start: start})
	if stop {
		t.Fatalf("the limit lifted on probe %d; the wait should resume\n%s", limitedProbes+1, logs.String())
	}
	if h.probeCalls != limitedProbes+1 {
		t.Fatalf("got %d probes, want %d\n%s", h.probeCalls, limitedProbes+1, logs.String())
	}

	// Group probes into wakes: probes in one wake run back to back, and wakes are
	// at least a poll interval apart.
	wakes := [][]time.Time{{h.probeStarts[0]}}
	for i := 1; i < len(h.probeStarts); i++ {
		prev := h.probeStarts[i-1]
		if gap := h.probeStarts[i].Sub(prev); gap < interval/2 {
			wakes[len(wakes)-1] = append(wakes[len(wakes)-1], h.probeStarts[i])
			continue
		}
		wakes = append(wakes, []time.Time{h.probeStarts[i]})
	}
	seen := 0
	for i, w := range wakes {
		if len(w) > 2 {
			t.Errorf("wake %d ran %d probes, want at most one per route (2)\n%s", i, len(w), logs.String())
		}
		if len(w) == 2 && h.probeModels[seen] == h.probeModels[seen+1] {
			t.Errorf("wake %d probed %s twice, want each route at most once per wake", i, h.probeModels[seen])
		}
		seen += len(w)
		if i == 0 {
			continue
		}
		if gap := w[0].Sub(wakes[i-1][len(wakes[i-1])-1]); gap < interval {
			t.Errorf("wake %d began %s after the previous wake's last probe, want at least the poll interval %s",
				i, gap.Round(time.Millisecond), interval)
		}
	}
	if len(wakes) < 3 {
		t.Errorf("%d probes ran in %d wakes, want at least 3 wakes for %d limited probes over 2 routes", h.probeCalls, len(wakes), limitedProbes)
	}
	want := 7 * h.probeCalls
	if tokens != want || d.targetSpendAt(0).tokens != want {
		t.Errorf("tokens returned=%d ledger=%d, want every probe charged (%d)", tokens, d.targetSpendAt(0).tokens, want)
	}
}

// A ceiling crossed by the first probe of a wake ends the wait before the walk
// spends another paid probe on the next route.
func TestSupervisedWait_ABudgetCrossedMidWalkStopsBeforeTheNextRoute(t *testing.T) {
	h := &fakeAdapter{probeTokens: 50, probeLimitFor: func(int, string) harness.Limit {
		return harness.Limit{Limited: true}
	}}
	d := routedDriver(t, h, twoPhases(), routeFreeAlias, routePaidAlias)
	d.cfg.PollInterval = config.Duration(30 * time.Millisecond)
	d.cfg.Budget = config.Budget{MaxTokens: 40}

	logs := captureLogs(t)
	_, _, stop := d.supervisedWait(context.Background(), harness.Limit{Limited: true},
		h, d.cfg.Phases[0], d.targets[0], spend{start: time.Now()})
	if !stop {
		t.Fatalf("the probe crossed the ceiling; the wait should stop\n%s", logs.String())
	}
	if h.probeCalls != 1 {
		t.Errorf("got %d probes, want 1: the walk must re-read the ceiling before probing the next route\n%s", h.probeCalls, logs.String())
	}
}

// opencodeLimitStub puts an `opencode` on PATH whose run ends on a usage-limit
// error event carrying msg. `session export` is refused so the adapter keeps
// the stream's figures.
func opencodeLimitStub(t *testing.T, msg string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = session ] && [ "$2" = export ]; then exit 1; fi
echo '{"type":"text","sessionID":"ses_one","part":{"type":"text","text":"working"}}'
echo '{"type":"error","sessionID":"ses_one","error":{"type":"provider.quota","message":"` + msg + `"}}'
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The scarce case: the paid route is marked, so the driver offers the session
// the free route alone. When that session ends on a usage limit, the free route
// is marked too, through the real opencode adapter's report.
func TestOneRouteOpencodeSessionMarksItsRouteOnALimit(t *testing.T) {
	const statedReset = "2030-01-02T03:04:05Z"
	for _, tc := range []struct {
		name string
		msg  string
	}{
		{"stated reset", "Usage limit reached. Resets at " + statedReset},
		{"no stated reset", "Usage limit reached."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opencodeLimitStub(t, tc.msg)
			d := routedDriver(t, &fakeAdapter{}, twoPhases(), routeFreeAlias, routePaidAlias)
			d.cfg.PollInterval = config.Duration(time.Hour)
			d.markRouteLimited(routePaidAlias, time.Now().Add(3*time.Hour))

			inv, err := d.invocationOn(d.targets[0], "opencode", false)
			if err != nil {
				t.Fatalf("invocationOn: %v", err)
			}
			choice := d.routeRoutesFor(d.cfg, d.cfg.Phases[0])
			if len(choice.Routes) != 1 || choice.Routes[0] != routeFreeAlias {
				t.Fatalf("routable = %v, want the free route alone", choice.Routes)
			}
			inv.Prompt, inv.Model, inv.Routes = "Work.", choice.Routes[0], choice.Routes

			oc := opencodeAdapter()
			start := time.Now()
			res, err := oc.Invoke(context.Background(), inv)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if !oc.DetectLimit(res).Limited {
				t.Fatalf("the stub's session did not classify as limited: %+v", oc.DetectLimit(res))
			}
			d.noteRouteFallbacks(d.targets[0], 1, inv, res)

			until, ok := persistedRouteMarks(t, d)[routeFreeAlias]
			if !ok {
				t.Fatalf("ROUTE_MARKS has no mark for %s; the next session would be sent straight back onto it", routeFreeAlias)
			}
			if tc.name == "stated reset" {
				if got := until.UTC().Format(time.RFC3339); got != statedReset {
					t.Errorf("mark = %s, want the stated reset %s", got, statedReset)
				}
			} else if left := until.Sub(start); left < 59*time.Minute || left > 61*time.Minute {
				t.Errorf("mark lifts %s after the session, want the poll interval (1h)", left)
			}
		})
	}
}

// A session with no route list (no route config) writes no route report, so it
// marks nothing.
func TestNoRouteListOpencodeSessionWritesNoRouteReport(t *testing.T) {
	opencodeLimitStub(t, "Usage limit reached.")
	oc := opencodeAdapter()
	res, err := oc.Invoke(context.Background(), harness.Invocation{Prompt: "Work.", Model: routeFreeAlias, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !oc.DetectLimit(res).Limited {
		t.Fatalf("the stub's session did not classify as limited")
	}
	if started, finished, fallbacks := harness.RouteFallbacks(res); started != "" || finished != "" || len(fallbacks) != 0 {
		t.Errorf("a session with no route list reported routes: %q -> %q %+v", started, finished, fallbacks)
	}
}
