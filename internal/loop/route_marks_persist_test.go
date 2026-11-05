package loop

import (
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// A mark written by one Driver is honoured by a new Driver over the same state
// dir: its routable list drops the marked route.
func TestRouteMarks_SurviveRestart(t *testing.T) {
	first := routedDriver(t, &fakeAdapter{}, nil, routeFreeAlias, routePaidAlias)
	first.markRouteLimited(routeFreeAlias, time.Now().Add(time.Hour))

	second := routedDriver(t, &fakeAdapter{}, nil, routeFreeAlias, routePaidAlias)
	second.state = first.state
	second.loadRouteMarks()

	got := second.routableRoutes([]string{routeFreeAlias, routePaidAlias})
	if len(got) != 1 || got[0] != routePaidAlias {
		t.Errorf("after restart routes = %v, want only the paid route", got)
	}

	prev := checkpointed(1, 0)
	ph := config.Phase{Name: config.ReviewPhaseName, Prompt: config.ReviewOnlyBrief()}
	phases := []config.Phase{{Name: "implement"}, ph, {Name: "fix"}}
	inv, err := second.invocationFor(second.targets[0], 1, ph, &prev, "", "", phases)
	if err != nil {
		t.Fatalf("invocationFor: %v", err)
	}
	if inv.Model != routePaidAlias {
		t.Errorf("restarted driver spawns on %q, want the fallback %q", inv.Model, routePaidAlias)
	}
}

// A mark past its expiry is not loaded.
func TestRouteMarks_ExpiredNotLoaded(t *testing.T) {
	st, err := statedir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if err := st.WriteFileAtomic(statedir.RouteMarksName, []byte(`{"`+routeFreeAlias+`":"`+past+`"}`)); err != nil {
		t.Fatal(err)
	}
	d := routedDriver(t, &fakeAdapter{}, nil, routeFreeAlias, routePaidAlias)
	d.state = st
	d.loadRouteMarks()
	if _, ok := d.routeMarkExpiry(routeFreeAlias); ok {
		t.Error("an expired mark was loaded")
	}
}

// A corrupt marks file is logged and ignored.
func TestRouteMarks_CorruptFileIgnored(t *testing.T) {
	st, err := statedir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.WriteFileAtomic(statedir.RouteMarksName, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	d := routedDriver(t, &fakeAdapter{}, nil, routeFreeAlias, routePaidAlias)
	d.state = st
	logs := captureLogs(t)
	d.loadRouteMarks()
	if _, ok := d.routeMarkExpiry(routeFreeAlias); ok {
		t.Error("a corrupt file produced a mark")
	}
	if !strings.Contains(logs.String(), "corrupt saved route marks") {
		t.Errorf("corruption not logged: %q", logs.String())
	}
}

// A missing marks file is the normal first start: no marks, no log noise.
func TestRouteMarks_MissingFileIsQuiet(t *testing.T) {
	d := routedDriver(t, &fakeAdapter{}, nil, routeFreeAlias, routePaidAlias)
	logs := captureLogs(t)
	d.loadRouteMarks()
	if logs.String() != "" {
		t.Errorf("unexpected log: %q", logs.String())
	}
}
