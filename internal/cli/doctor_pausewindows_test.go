package cli

// The pause_windows preflight: an unset schedule is reported as the nothing it
// is, and a configured one says whether the loop would claim work right now and
// when it will next stop — the one thing reading the schedule cannot tell you.

import (
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// pauseCfg is a config carrying the DeepSeek peak schedule.
func pauseCfg(t *testing.T, lead time.Duration) *config.Config {
	t.Helper()
	pw := &config.PauseWindows{
		Timezone: "Australia/Brisbane",
		Lead:     config.Duration(lead),
		Windows: []config.PauseWindow{
			{Days: "mon-fri", From: "11:00", To: "14:00"},
			{Days: "mon-fri", From: "16:00", To: "20:00"},
		},
	}
	if err := pw.Validate(); err != nil {
		t.Fatalf("fixture pause windows do not validate: %v", err)
	}
	return &config.Config{PauseWindows: pw}
}

func TestCheckPauseWindowsUnset(t *testing.T) {
	c := checkPauseWindowsAt(&config.Config{}, time.Now())
	if c.status != pass {
		t.Fatalf("status = %v, want pass", c.status)
	}
	if !strings.Contains(c.detail, "none configured") {
		t.Errorf("detail = %q, want it to say none is configured", c.detail)
	}
}

func TestCheckPauseWindowsReportsTheScheduleAndTheMoment(t *testing.T) {
	loc, err := time.LoadLocation("Australia/Brisbane")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	cfg := pauseCfg(t, 30*time.Minute)
	const wantDetail = "mon-fri 11:00-14:00, mon-fri 16:00-20:00 (Australia/Brisbane, lead 30m)"

	// Friday 2026-11-13 10:45 local: inside the 30m lead of the 11:00 window.
	paused := checkPauseWindowsAt(cfg, time.Date(2026, 11, 13, 10, 45, 0, 0, loc))
	if paused.status != pass {
		t.Fatalf("status = %v, want pass", paused.status)
	}
	if paused.detail != wantDetail {
		t.Errorf("detail = %q, want %q", paused.detail, wantDetail)
	}
	if len(paused.info) != 1 || !strings.Contains(paused.info[0], "now: paused until Fri 14:00") {
		t.Errorf("info = %v, want a paused line ending at Fri 14:00", paused.info)
	}

	// Friday 21:00 local: running, and the next pause is Monday's lead.
	running := checkPauseWindowsAt(cfg, time.Date(2026, 11, 13, 21, 0, 0, 0, loc))
	if len(running.info) != 1 || !strings.Contains(running.info[0], "now: running; next pause Mon 10:30") {
		t.Errorf("info = %v, want a running line naming Mon 10:30", running.info)
	}
}
