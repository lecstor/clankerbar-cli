package cli

// deadrate_test.go — CLA-584: the stalled column is part of what `clankerbar
// dead-rate` shows. The scan's classification is pinned in internal/deadscan;
// these tests pin the presentation and the wiring from the command down.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/deadscan"
)

func TestPrintTable_CarriesTheStalledColumn(t *testing.T) {
	var buf strings.Builder
	printTable(&buf, []deadscan.Cell{
		{Day: "2026-09-24", Phase: "review", Harness: "opencode", Run: 4, Dead: 1, Stalled: 2},
	})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("table = %d lines, want 3 (header, row, total):\n%s", len(lines), buf.String())
	}
	if got := strings.Fields(lines[0]); len(got) != 7 || got[5] != "stalled" {
		t.Errorf("header = %v, want a stalled column before rate", got)
	}
	if got := strings.Fields(lines[1]); len(got) != 7 || got[4] != "1" || got[5] != "2" {
		t.Errorf("row = %v, want dead=1 stalled=2 as separate columns", got)
	}
	// The total row's empty phase/harness fields collapse under Fields, so it
	// reads total, run, dead, stalled, rate.
	if got := strings.Fields(lines[2]); len(got) != 5 || got[0] != "total" || got[1] != "4" || got[2] != "1" || got[3] != "2" {
		t.Errorf("total = %v, want total run=4 dead=1 stalled=2", got)
	}
	// The rate stays dead/run — 1 of 4 — and the two stalls must not move it.
	if !strings.Contains(lines[1], "25.0%") {
		t.Errorf("rate must stay dead/run (1 of 4 = 25.0%%): %q", lines[1])
	}
}

// End to end through the command: one stalled iteration log under --root shows
// up in the stalled column.
func TestDeadRate_ShowsAStalledSession(t *testing.T) {
	root := t.TempDir()
	logDir := filepath.Join(root, "dev-abc123")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	log := `{"type":"step_start","timestamp":1,"sessionID":"ses_stall","part":{"id":"p1","type":"step-start"}}
{"type":"tool_use","timestamp":2,"sessionID":"ses_stall","part":{"type":"tool","tool":"clankerbar_heartbeat","callID":"c1","state":{"status":"completed","input":{"runId":"r-1"},"output":"{\"ok\":true}"}}}
{"type":"step_finish","timestamp":3,"sessionID":"ses_stall","part":{"id":"p2","reason":"length","tokens":{"total":306592,"input":5280,"output":0,"reasoning":32000,"cache":{"write":0,"read":269312}},"cost":0.020799936}}
`
	name := "iteration-20260924-133347-d15-preview-a0-ca2a5eb5.log"
	if err := os.WriteFile(filepath.Join(logDir, name), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := DeadRate(context.Background(), []string{"--root", root})
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	if runErr != nil {
		t.Fatalf("DeadRate: %v", runErr)
	}
	line := ""
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, "2026-09-24") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no row for the stalled session:\n%s", out)
	}
	if got := strings.Fields(line); len(got) != 7 || got[4] != "0" || got[5] != "1" {
		t.Errorf("row = %v, want dead=0 stalled=1 — a stall is not a death", got)
	}
}
