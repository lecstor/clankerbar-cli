package loop

import (
	"strings"
	"testing"
	"time"
)

// A provider's UTC reset must render in the machine's local zone with the date
// and zone name, not as the bare UTC weekday and clock.
func TestStatedResetRendersLocalWithDateAndZone(t *testing.T) {
	old := time.Local
	t.Cleanup(func() { time.Local = old })
	time.Local = time.FixedZone("AEST", 10*3600)

	reset := time.Date(2026, 10, 27, 10, 56, 48, 0, time.UTC)
	const want = "Tue 27 Oct 20:56 AEST"

	if got := resetSuffix(reset); got != " (stated reset "+want+")" {
		t.Errorf("resetSuffix = %q, want the local %q", got, want)
	}
	if got := formatReset(reset); got != want {
		t.Errorf("formatReset = %q, want %q", got, want)
	}
	if got := routeRetryLabel(time.Now().Add(48 * time.Hour).UTC()); !strings.HasSuffix(got, " AEST") || strings.Count(got, " ") < 3 {
		t.Errorf("routeRetryLabel past a day = %q, want weekday, date, clock and zone", got)
	}
	if got := routeRetryLabel(time.Now().Add(time.Hour).UTC()); !strings.HasSuffix(got, " AEST") || strings.Count(got, " ") != 1 {
		t.Errorf("routeRetryLabel within a day = %q, want clock and zone", got)
	}
}
