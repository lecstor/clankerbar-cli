package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// CLA-782: a model field accepts either the historical plain string or an
// ordered array of route aliases. The array form exists so a session can start
// on a free provider route and be resumed on the paid route of the same model
// when the free one is denied.

func TestModelRoutes_UnmarshalAcceptsAStringOrAnArray(t *testing.T) {
	var hc HarnessConfig
	if err := json.Unmarshal([]byte(`{"model": "opencode-go/one"}`), &hc); err != nil {
		t.Fatalf("a string model must keep parsing: %v", err)
	}
	if got := hc.Model.List(); len(got) != 1 || got[0] != "opencode-go/one" {
		t.Errorf("string model parsed to %v, want one route", got)
	}

	if err := json.Unmarshal([]byte(`{"model": ["free/one", "paid/one"]}`), &hc); err != nil {
		t.Fatalf("an array model must parse: %v", err)
	}
	if got := hc.Model.List(); len(got) != 2 || got[0] != "free/one" || got[1] != "paid/one" {
		t.Errorf("array model parsed to %v, want the two routes in order", got)
	}

	// The tier map's values carry routes too, and so do phases and panel members.
	var full struct {
		Models      map[string]ModelRoutes `json:"models"`
		Phases      []Phase                `json:"phases"`
		ReviewPanel []ReviewPanelMember    `json:"review_panel"`
	}
	if err := json.Unmarshal([]byte(`{
		"models": {"strong": ["free/strong", "paid/strong"], "cheap": "cheap/one"},
		"phases": [{"name": "implement", "model": ["free/impl", "paid/impl"]}],
		"review_panel": [{"model": ["free/rev", "paid/rev"]}]
	}`), &full); err != nil {
		t.Fatalf("unmarshal the route shapes: %v", err)
	}
	if got := full.Models["strong"].List(); len(got) != 2 {
		t.Errorf("tier array parsed to %v, want two routes", got)
	}
	if got := full.Models["cheap"].List(); len(got) != 1 || got[0] != "cheap/one" {
		t.Errorf("tier string parsed to %v, want one route", got)
	}
	if got := full.Phases[0].Model.List(); len(got) != 2 || got[1] != "paid/impl" {
		t.Errorf("phase route list parsed to %v", got)
	}
	if got := full.ReviewPanel[0].Model.List(); len(got) != 2 || got[1] != "paid/rev" {
		t.Errorf("panel member route list parsed to %v", got)
	}

	// A malformed list is refused rather than silently dropped.
	if err := json.Unmarshal([]byte(`{"model": [1, 2]}`), &hc); err == nil {
		t.Error("an array of numbers was accepted as a model route list")
	}
	// null is no model at all, exactly like an absent key.
	if err := json.Unmarshal([]byte(`{"model": null}`), &hc); err != nil {
		t.Fatalf("null model: %v", err)
	}
	if !hc.Model.Empty() {
		t.Errorf("null model parsed to %v, want no routes", hc.Model.List())
	}
}

// The wire shape is what keeps Config.Identity() stable: a single route marshals
// as the plain string it always was, and an unset field still writes `""` (these
// tags carry no omitempty except the phase's).
func TestModelRoutes_MarshalSingleRouteAsAPlainString(t *testing.T) {
	one, err := json.Marshal(HarnessConfig{Model: ModelRoutes{"opencode-go/one"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(one), `"model":"opencode-go/one"`) {
		t.Errorf("a one-route model marshalled to %s, want the plain string form", one)
	}
	if strings.Contains(string(one), `"model":["`) {
		t.Errorf("a one-route model marshalled as an array: %s", one)
	}

	two, err := json.Marshal(HarnessConfig{Model: ModelRoutes{"free/one", "paid/one"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(two), `"model":["free/one","paid/one"]`) {
		t.Errorf("a two-route model marshalled to %s, want the array form", two)
	}

	none, err := json.Marshal(HarnessConfig{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(none), `"model":""`) {
		t.Errorf("an unset model marshalled to %s, want the historical empty string (identity)", none)
	}
}

// The identity guarantee the change must not break: a config that names models
// as STRINGS hashes exactly what it hashed before ModelRoutes existed. Both
// goldens were computed at 5fc5ee1, before this type was introduced.
func TestModelRoutes_StringConfigKeepsTheConfigIdentity(t *testing.T) {
	strings := &Config{
		Harness: "opencode",
		Model:   "opus",
		Models:  map[string]string{"strong": "opus", "cheap": "haiku"},
		Harnesses: map[string]HarnessConfig{
			"opencode": {Model: ModelRoutes{"opencode-go/gpt-6-luna"}, Models: map[string]ModelRoutes{"strong": {"opencode-go/gpt-6-luna"}}},
		},
		Phases: []Phase{
			{Name: "implement", Tier: "strong", Model: ModelRoutes{"opencode-go/gpt-6-luna"}},
			{Name: "review", Tier: "cheap"},
		},
		ReviewPanel: []ReviewPanelMember{
			{Name: "one", Model: ModelRoutes{"opencode-go/gpt-6-luna"}},
			{Name: "two", Model: ModelRoutes{"opencode-go/glm-5.3-flash"}},
		},
	}
	const preRoutesIdentity = "0a42e4ce38f4f53d4d37299574b83d7090f78b32ba837ee158bd65abcec81c77"
	if got := strings.Identity(); got != preRoutesIdentity {
		t.Errorf("Identity() = %s, want the pre-route identity %s — a string config's identity must not move", got, preRoutesIdentity)
	}

	// And the same for a config that names no model anywhere: the empty fields
	// must still marshal as the empty strings they always did.
	empty := &Config{
		Harnesses: map[string]HarnessConfig{"opencode": {}},
		Phases:    []Phase{{Name: "implement"}},
	}
	const preRoutesEmptyIdentity = "e6129f7e624d40d519e0545713aaa7b0200b680582a5c0698024d5faabcbe26d"
	if got := empty.Identity(); got != preRoutesEmptyIdentity {
		t.Errorf("Identity() = %s, want the pre-route identity %s for an unset model", got, preRoutesEmptyIdentity)
	}
}

// ModelForPhase keeps returning the FIRST route — every existing caller, log line
// and brief placeholder is unchanged — while the sibling returns the whole list.
func TestModelRoutesForPhase_ReturnsTheWholeChainInResolutionOrder(t *testing.T) {
	c := &Config{
		Harness: "opencode",
		Harnesses: map[string]HarnessConfig{
			"opencode": {
				Model:  ModelRoutes{"free/default", "paid/default"},
				Models: map[string]ModelRoutes{"strong": {"free/strong", "paid/strong"}},
			},
		},
	}

	// A pinned route list wins outright.
	ph := Phase{Model: ModelRoutes{"free/pinned", "paid/pinned"}, Tier: "strong"}
	routes, ok := c.ModelRoutesForPhase(ph)
	if !ok || len(routes) != 2 || routes[0] != "free/pinned" || routes[1] != "paid/pinned" {
		t.Errorf("pinned routes = %v/%v, want the pinned list", routes, ok)
	}
	if got, _ := c.ModelForPhase(ph); got != "free/pinned" {
		t.Errorf("ModelForPhase = %q, want the first route", got)
	}

	// A tier resolves through the harness's own map, whole list.
	ph = Phase{Tier: "strong"}
	routes, ok = c.ModelRoutesForPhase(ph)
	if !ok || len(routes) != 2 || routes[0] != "free/strong" {
		t.Errorf("tier routes = %v/%v, want the bucket's two routes", routes, ok)
	}

	// No tier and no pin: the harness's default routes.
	routes, ok = c.ModelRoutesForPhase(Phase{})
	if !ok || len(routes) != 2 || routes[0] != "free/default" {
		t.Errorf("default routes = %v/%v, want the harness default's two routes", routes, ok)
	}

	// An unmapped tier still reports the typo case and falls back.
	routes, ok = c.ModelRoutesForPhase(Phase{Tier: "strng"})
	if ok || len(routes) != 2 || routes[0] != "free/default" {
		t.Errorf("unmapped tier = %v/%v, want the default routes with ok=false", routes, ok)
	}

	// No model anywhere is the harness's own default: no routes, not a typo.
	bare := &Config{}
	if routes, ok := bare.ModelRoutesForPhase(Phase{}); len(routes) != 0 || !ok {
		t.Errorf("bare config = %v/%v, want no routes and ok", routes, ok)
	}
}

// Doctor's credential check must see the paid fallback too: reading only the
// first route would certify a run that then asks a provider no credential covers.
func TestModelIDsFor_IncludesEveryRouteOfEveryPhase(t *testing.T) {
	c := &Config{
		Harness: "opencode",
		Harnesses: map[string]HarnessConfig{
			"opencode": {Model: ModelRoutes{"free/default", "paid/default"}},
		},
		Phases: []Phase{
			{Name: "implement"},
			{Name: "review", Model: ModelRoutes{"free/rev", "paid/rev"}},
		},
	}

	got := c.ModelIDsFor("opencode")
	want := []string{"free/default", "free/rev", "paid/default", "paid/rev"}
	if len(got) != len(want) {
		t.Fatalf("ModelIDsFor = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ModelIDsFor = %v, want %v (sorted, deduplicated, every route)", got, want)
		}
	}
}

// Blank and duplicate entries are dropped without reordering: the order IS the
// fallback order, and a blank alias would reach a child's model flag.
func TestModelRoutes_ListTrimsBlanksAndDuplicates(t *testing.T) {
	m := ModelRoutes{"  free/one  ", "", "free/one", "   ", "paid/one"}
	got := m.List()
	if len(got) != 2 || got[0] != "free/one" || got[1] != "paid/one" {
		t.Errorf("List = %v, want [free/one paid/one]", got)
	}
	if m.First() != "free/one" {
		t.Errorf("First = %q, want the trimmed first route", m.First())
	}
	if !(ModelRoutes{"", "  "}).Empty() {
		t.Error("a whitespace-only route list must be empty")
	}
}

// A panel seat inherits the route list, not just its first alias: the driver
// resolves each member through the same phase chain, so a seat configured with
// a fallback switches exactly as a phase does.
func TestReviewPanelMember_RouteListExpandsToThePhase(t *testing.T) {
	c := &Config{
		Harness: "opencode",
		Phases:  []Phase{{Name: ImplementPhaseName}, {Name: ReviewPhaseName}, {Name: FixPhaseName}},
		ReviewPanel: []ReviewPanelMember{
			{Model: ModelRoutes{"free/seat", "paid/seat"}},
		},
	}
	member := c.EffectivePhases()[1]
	routes, ok := c.ModelRoutesForPhase(member)
	if !ok || len(routes) != 2 || routes[0] != "free/seat" || routes[1] != "paid/seat" {
		t.Errorf("panel member routes = %v/%v, want the member's full list", routes, ok)
	}
	if got, _ := c.ModelForPhase(member); got != "free/seat" {
		t.Errorf("panel member ModelForPhase = %q, want the first route", got)
	}
}

// An explicit `"model": ""` on a phase or a harness block must hash exactly what
// the string field did, which is the same as leaving the key out (review
// findings 1.review-3.1 and 1.review-1.1): Phase.Model is `omitempty`, so a
// one-blank-route slice would emit a key the string never did.
func TestModelRoutes_ExplicitEmptyStringKeepsTheConfigIdentity(t *testing.T) {
	const preRoutesEmptyIdentity = "e6129f7e624d40d519e0545713aaa7b0200b680582a5c0698024d5faabcbe26d"
	for name, doc := range map[string]string{
		"phase":         `{"harnesses":{"opencode":{}},"phases":[{"name":"implement","model":""}]}`,
		"harness block": `{"harnesses":{"opencode":{"model":""}},"phases":[{"name":"implement"}]}`,
		"both":          `{"harnesses":{"opencode":{"model":""}},"phases":[{"name":"implement","model":""}]}`,
	} {
		var c Config
		if err := json.Unmarshal([]byte(doc), &c); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if got := c.Identity(); got != preRoutesEmptyIdentity {
			t.Errorf("%s: Identity() = %s, want the pre-route identity %s for an explicit empty model", name, got, preRoutesEmptyIdentity)
		}
	}
	var m ModelRoutes
	if err := json.Unmarshal([]byte(`""`), &m); err != nil || m != nil {
		t.Errorf(`decode "" = %#v, %v; want nil routes`, m, err)
	}
}

// A harness block holding only `"model": ""` names nothing, so it is still the
// empty block Validate refuses (review finding 1.review-1.2).
func TestModelRoutes_EmptyStringModelLeavesTheHarnessBlockEmpty(t *testing.T) {
	var hc HarnessConfig
	if err := json.Unmarshal([]byte(`{"model":""}`), &hc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !hc.Empty() {
		t.Fatalf("HarnessConfig{model: \"\"}.Empty() = false, want true")
	}
	c := mixedCfg()
	c.Harnesses["opencode"] = hc
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "missing or empty") {
		t.Fatalf("Validate() = %v, want a refusal saying the block is missing or empty", err)
	}
}
