package cli

// The daemon's own log file (CLA-701): its `log.Printf` lines reach
// <state dir>/daemon.log as well as stderr, and the restore func puts the
// process-wide logger back where it was.

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

func TestInstallDaemonLogWritesTheDaemonsOwnLines(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	path := filepath.Join(cfg.StateDir, statedir.DaemonLogName)

	prev := log.Writer()
	t.Cleanup(func() { log.SetOutput(prev) })

	restore, err := installDaemonLog(cfg)
	if err != nil {
		t.Fatalf("installDaemonLog: %v", err)
	}
	log.Print("iteration 1 done (tokens=0 cost=$0.0000)")
	restore()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the daemon log was not written: %v", err)
	}
	if !strings.Contains(string(body), "iteration 1 done (tokens=0 cost=$0.0000)") {
		t.Errorf("the daemon's own line did not land in %s; got:\n%s", path, body)
	}

	// Restored: a later line must not reach the file, or the command would keep
	// writing into a closed handle after Run returns.
	log.Print("after restore")
	body, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read daemon log: %v", err)
	}
	if strings.Contains(string(body), "after restore") {
		t.Errorf("restore did not detach the daemon log:\n%s", body)
	}
}

// The log lives in the state dir the loop opens, so an explicit state_dir is
// honoured — and the path doctor reports is the same one.
func TestDaemonLogPathFollowsTheStateDir(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{StateDir: dir}
	got, err := daemonLogPath(cfg)
	if err != nil {
		t.Fatalf("daemonLogPath: %v", err)
	}
	want := filepath.Join(dir, statedir.DaemonLogName)
	if got != want {
		t.Errorf("daemonLogPath = %q, want %q", got, want)
	}
}
