package config

import (
	"encoding/json"
	"testing"
)

// The event channel's dials are a PRIVACY surface, so the tests here pin the
// two properties that matter more than the happy path: an absent block reads as
// off, and nothing machine-local can turn it on (the local file cannot set it
// at all, and --no-events only ever sets NoEvents).

func TestRunConfigEventsAppliedFromStoredDocument(t *testing.T) {
	base := defaults()
	if base.Events.Activity || base.Events.Transcript {
		t.Fatal("the default config already has the event dials on")
	}
	doc := &RunConfigDoc{SchemaVersion: 1, Events: &RunConfigEvents{Activity: true}}
	if doc.Empty() {
		t.Fatal("a document carrying only `events` read as empty; the dial would never apply")
	}
	if err := base.ApplyRunConfig(doc); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}
	if !base.Events.Activity {
		t.Fatal("events.activity did not apply")
	}
	if base.Events.Transcript {
		t.Fatal("events.transcript turned on without the document saying so")
	}
}

func TestRunConfigEventsAbsentMeansOffAndClearsAStaleOverlay(t *testing.T) {
	base := defaults()
	base.Events = RunConfigEvents{Activity: true}
	// A document that sets another family and no `events` is the project saying
	// where the dials sit: absent is off, not "keep what a previous fetch left".
	doc := &RunConfigDoc{SchemaVersion: 1, MaxTurns: 5}
	if err := base.ApplyRunConfig(doc); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}
	if base.Events.Activity {
		t.Fatal("an absent events block did not read as off")
	}
}

func TestRunConfigEventsEmptyDocumentAlsoFailsClosed(t *testing.T) {
	// The plane's default document is `{}`, and it must still turn the dials
	// off: a caller reusing a live config would otherwise keep sending on a
	// project whose document says nothing.
	base := defaults()
	base.Events = RunConfigEvents{Activity: true}
	doc := &RunConfigDoc{SchemaVersion: 1}
	if !doc.Empty() {
		t.Fatal("test precondition: the document is not empty")
	}
	if err := base.ApplyRunConfig(doc); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}
	if base.Events.Activity {
		t.Fatal("an empty document left a stale events.activity on")
	}
}

func TestRunConfigEventsTranscriptAppliesBesideActivity(t *testing.T) {
	base := defaults()
	doc := &RunConfigDoc{SchemaVersion: 1, Events: &RunConfigEvents{Activity: true, Transcript: true}}
	if err := base.ApplyRunConfig(doc); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}
	if !base.Events.Activity || !base.Events.Transcript {
		t.Fatalf("events = %+v, want both dials on", base.Events)
	}
}

func TestRunConfigEventsTranscriptWithoutActivityFailsClosed(t *testing.T) {
	// The plane refuses this at save time. A document that still carries it (a
	// hand-edited store) must not leak the full tier.
	base := defaults()
	doc := &RunConfigDoc{SchemaVersion: 1, Events: &RunConfigEvents{Transcript: true}}
	if err := base.ApplyRunConfig(doc); err != nil {
		t.Fatalf("ApplyRunConfig: %v", err)
	}
	if base.Events.Transcript {
		t.Fatal("transcript read as on while activity was off")
	}
}

func TestRunConfigEventsDecodeFromThePlaneDocument(t *testing.T) {
	var doc RunConfigDoc
	if err := json.Unmarshal([]byte(`{"$schema_version":1,"events":{"activity":true,"transcript":false}}`), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Events == nil || !doc.Events.Activity || doc.Events.Transcript {
		t.Fatalf("decoded events = %+v", doc.Events)
	}
}

func TestLocalFileCannotTurnEventsOn(t *testing.T) {
	// Both machine-local fields are `json:"-"`: a config file carrying them must
	// be ignored, or a repo's checked-in config could opt a fleet into sending
	// transcripts. The project dial is the only way on.
	var cfg Config
	if err := json.Unmarshal([]byte(`{"events":{"activity":true},"no_events":false}`), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Events.Activity {
		t.Fatal("a local config file turned events.activity on")
	}
}

func TestNoEventsOverrideIsOneWay(t *testing.T) {
	cfg := defaults()
	cfg.ApplyFlagOverrides(Overrides{NoEvents: true})
	if !cfg.NoEvents {
		t.Fatal("--no-events did not set the opt-out")
	}
	// A later overlay re-applies the SAME overrides; and a zero Overrides (a
	// caller with no flags) must not flip it back, because no machine-local value
	// can turn the channel on.
	cfg.ApplyFlagOverrides(Overrides{})
	if !cfg.NoEvents {
		t.Fatal("a zero override cleared the machine-local opt-out")
	}
}
