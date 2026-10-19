package config

import "testing"

// The tier map is the OPERATOR's, so every resolution falls back rather than
// refusing. These pin the whole chain, because the property that matters is not
// "a tier resolves" but "nothing a tier can be stops a run".

func TestModelForTier_AnUnsetTierTakesTheRunDefault(t *testing.T) {
	c := &Config{Model: "opus", Models: map[string]string{"cheap": "haiku"}}

	got, ok := c.ModelForTier("")
	if got != "opus" {
		t.Errorf("ModelForTier(\"\") = %q, want the run-wide model %q", got, "opus")
	}
	// Not a typo, so nothing to report: an unset tier is the ordinary case and a
	// log line on every unphased session would be noise.
	if !ok {
		t.Error("an unset tier was reported as unresolvable; it is the ordinary case")
	}
}

func TestModelForTier_AMappedTierTakesItsAlias(t *testing.T) {
	c := &Config{Model: "sonnet", Models: map[string]string{"strong": "opus", "cheap": "haiku"}}

	if got, ok := c.ModelForTier("strong"); got != "opus" || !ok {
		t.Errorf("ModelForTier(\"strong\") = (%q, %v), want (\"opus\", true)", got, ok)
	}
	if got, ok := c.ModelForTier("cheap"); got != "haiku" || !ok {
		t.Errorf("ModelForTier(\"cheap\") = (%q, %v), want (\"haiku\", true)", got, ok)
	}
}

// The typo case. It must not stop the run — an unattended drain that refuses to
// start at 3am over a mistyped bucket has turned a cost knob into an outage — but
// it must be visible, because a tier is set precisely when the default is NOT
// what was wanted, so silently using the default is the failure.
func TestModelForTier_AnUnmappedTierFallsBackAndSaysSo(t *testing.T) {
	c := &Config{Model: "opus", Models: map[string]string{"strong": "opus"}}

	got, ok := c.ModelForTier("strng")
	if got != "opus" {
		t.Errorf("ModelForTier(\"strng\") = %q, want a fallback to the run-wide model", got)
	}
	if ok {
		t.Error("a tier the map does not define was reported as resolved, so the driver logs nothing about a typo")
	}
}

// The empty-string path the whole guard exists for. A bucket present but blank is
// the same nothing an absent one is: resolving it to "" as a MODEL would put a
// blank alias on the way to the harness flag.
func TestModelForTier_ABucketMappedToNothingFallsBack(t *testing.T) {
	for _, alias := range []string{"", " ", "\t", "\n  "} {
		c := &Config{Model: "opus", Models: map[string]string{"strong": alias}}
		got, ok := c.ModelForTier("strong")
		if got != "opus" {
			t.Errorf("tier mapped to %q resolved to %q, want the run-wide model", alias, got)
		}
		if ok {
			t.Errorf("tier mapped to %q was reported as resolved", alias)
		}
	}
}

// Blank-but-not-empty at every step. A space in a JSON string is invisible in
// review and passes an `!= ""` check, so it is the one input that reaches the
// child as a model alias no provider has.
func TestModelForTier_TrimsBothSidesOfTheLookup(t *testing.T) {
	c := &Config{Model: "  opus  ", Models: map[string]string{"strong": "  sonnet  "}}

	if got, _ := c.ModelForTier("  strong  "); got != "sonnet" {
		t.Errorf("a padded tier name resolved to %q, want %q", got, "sonnet")
	}
	if got, _ := c.ModelForTier(""); got != "opus" {
		t.Errorf("a padded run-wide model resolved to %q, want %q", got, "opus")
	}
	// A run-wide model that is only whitespace is not a model.
	blank := &Config{Model: "   "}
	if got, _ := blank.ModelForTier(""); got != "" {
		t.Errorf("a whitespace-only model resolved to %q, want \"\" so no flag is emitted", got)
	}
}

// The untouched-config guarantee: no map, no tiers, nothing changes.
func TestModelForTier_ANoModelConfigResolvesToTheHarnessDefault(t *testing.T) {
	c := &Config{}

	if got, ok := c.ModelForTier(""); got != "" || !ok {
		t.Errorf("ModelForTier on a bare config = (%q, %v), want (\"\", true) — the harness picks", got, ok)
	}
	// A tier named against no map at all is still a fallback, not a panic on a nil
	// map read.
	if got, ok := c.ModelForTier("strong"); got != "" || ok {
		t.Errorf("ModelForTier(\"strong\") with no map = (%q, %v), want (\"\", false)", got, ok)
	}
}

// A phase carrying a tier survives EffectivePhases, which is where a built-in
// prompt is resolved and where a field could be dropped by rebuilding the struct.
func TestEffectivePhases_CarriesTheTierThrough(t *testing.T) {
	c := &Config{Phases: []Phase{{Name: "implement", Tier: "strong"}, {Name: "review", Tier: "cheap"}}}

	got := c.EffectivePhases()
	if len(got) != 2 {
		t.Fatalf("EffectivePhases returned %d phases, want 2", len(got))
	}
	if got[0].Tier != "strong" || got[1].Tier != "cheap" {
		t.Errorf("tiers = %q/%q, want strong/cheap — a phase's model would silently become the default",
			got[0].Tier, got[1].Tier)
	}
}
