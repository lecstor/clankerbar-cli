package supervisor

// The supervisor's own log file (CLA-701): Options.LogToFile puts its lines in
// supervise.log in its state dir as well as stderr. Off by default — a library
// caller must not have the process-wide logger taken over — so this is also the
// pin on that default.

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogToFileWritesTheSupervisorsOwnLines(t *testing.T) {
	cacheDir := t.TempDir()
	plane := &fakePlane{}
	plane.set([]RosterEntry{})
	srv := plane.serve(t)

	prev := log.Writer()
	t.Cleanup(func() { log.SetOutput(prev) })

	o := testOptions(t, cacheDir, srv)
	o.LogToFile = true
	if err := Supervise(context.Background(), o); err != nil {
		t.Fatalf("Supervise: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(cacheDir, SuperviseLogName))
	if err != nil {
		t.Fatalf("the supervisor log was not written: %v", err)
	}
	if !strings.Contains(string(body), "roster holds no local instances") {
		t.Errorf("the supervisor's own line did not land in %s; got:\n%s", SuperviseLogName, body)
	}
}

func TestLogToFileDefaultsOff(t *testing.T) {
	cacheDir := t.TempDir()
	plane := &fakePlane{}
	plane.set([]RosterEntry{})
	srv := plane.serve(t)

	prev := log.Writer()
	t.Cleanup(func() { log.SetOutput(prev) })

	o := testOptions(t, cacheDir, srv)
	if err := Supervise(context.Background(), o); err != nil {
		t.Fatalf("Supervise: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, SuperviseLogName)); !os.IsNotExist(err) {
		t.Errorf("LogToFile defaults off, so no supervisor log should be written (stat err=%v)", err)
	}
}
