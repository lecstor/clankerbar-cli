package config

import (
	"strings"
	"testing"
)

// The live shape: a valid report followed by DeepSeek's leaked closing tags.
func TestParseDoneIgnoresTrailingDSMLMarkup(t *testing.T) {
	final := "Review done.\n\n" + DoneMarker + "\n" +
		`{"verdict":"pass","reviewedTip":"2a86a2e90b6c61d3183bb4b45eb51808bca307c8"}` +
		"\n</｜DSML｜ parameter>\n</｜DSML｜ invoke>\n</｜DSML｜ calls>"
	rep, found, err := ParseDone(final)
	if !found || err != nil {
		t.Fatalf("ParseDone = found %v, err %v; want the report parsed", found, err)
	}
	if rep.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass", rep.Verdict)
	}
}

// Only the markup is forgiven: prose or other tags after the object still refuse.
func TestParseDoneStillRefusesOtherTrailingText(t *testing.T) {
	for name, tail := range map[string]string{
		"prose":            "\nThanks!",
		"other tag":        "\n</parameter>",
		"markup then text": "\n</｜DSML｜ calls>\nmore",
	} {
		final := DoneMarker + "\n" + `{"verdict":"pass","reviewedTip":"abcdef1"}` + tail
		if _, found, err := ParseDone(final); !found || err == nil || !strings.Contains(err.Error(), "follows") {
			t.Errorf("%s: found %v err %v; want the trailing-text refusal", name, found, err)
		}
	}
}

// A fenced report followed by leaked markup parses too.
func TestParseDoneFencedReportThenDSMLMarkup(t *testing.T) {
	final := DoneMarker + "\n```json\n" + `{"summary":"s","tests":"t"}` + "\n```\n</｜DSML｜ calls>"
	if rep, found, err := ParseDone(final); !found || err != nil || rep.Summary != "s" {
		t.Fatalf("ParseDone = %+v, %v, %v", rep, found, err)
	}
}
