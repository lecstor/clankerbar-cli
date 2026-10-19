package loop

// The console wiring (CLA-701): a session's stream is written to its iteration
// log, and — only when `echo_sessions` asks — to the daemon's terminal as well.
// There is deliberately no test asserting on os.Stderr itself: the echo
// destination is injectable (Driver.echoOut) precisely so this can be checked
// without capturing the test process's own stderr.

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// consoleAdapter writes a marker into whatever Console the driver hands it,
// standing in for a real adapter streaming its session, then behaves like the
// ordinary fake (a clean exit).
type consoleAdapter struct {
	*fakeAdapter
	stream string
}

func (c *consoleAdapter) Invoke(ctx context.Context, in harness.Invocation) (harness.Result, error) {
	if in.Console != nil {
		fmt.Fprint(in.Console, c.stream)
	}
	return c.fakeAdapter.Invoke(ctx, in)
}

func runConsoleDriver(t *testing.T, d *Driver) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

// iterationLogs returns the iteration logs in dir, newest name last (the names
// carry their spawn timestamp).
func iterationLogPaths(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "iteration-*.log"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("no iteration log was written in %s", dir)
	}
	return matches
}

func TestSessionStreamIsNotEchoedByDefault(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 1
	h := &consoleAdapter{fakeAdapter: &fakeAdapter{}, stream: "SESSION-STREAM\n"}
	var echoed bytes.Buffer
	d := New(cfg, h, &fakePoller{sum: backlog.Summary{Ready: 1, Claimable: 1}})
	d.echoOut = &echoed

	runConsoleDriver(t, d)

	if echoed.Len() != 0 {
		t.Errorf("the default must not echo the session stream to the terminal; terminal received:\n%s", echoed.String())
	}
	logs := iterationLogPaths(t, cfg.StateDir)
	body, err := os.ReadFile(logs[len(logs)-1])
	if err != nil {
		t.Fatalf("read iteration log: %v", err)
	}
	if !strings.Contains(string(body), "SESSION-STREAM") {
		t.Errorf("the iteration log must still carry the whole session stream; got:\n%s", body)
	}
}

func TestEchoSessionsRestoresTheLiveStream(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 1
	cfg.EchoSessions = true
	h := &consoleAdapter{fakeAdapter: &fakeAdapter{}, stream: "SESSION-STREAM\n"}
	var echoed bytes.Buffer
	d := New(cfg, h, &fakePoller{sum: backlog.Summary{Ready: 1, Claimable: 1}})
	d.echoOut = &echoed

	runConsoleDriver(t, d)

	if !strings.Contains(echoed.String(), "SESSION-STREAM") {
		t.Errorf("echo_sessions: true must mirror the stream to the terminal; got:\n%s", echoed.String())
	}
	logs := iterationLogPaths(t, cfg.StateDir)
	body, err := os.ReadFile(logs[len(logs)-1])
	if err != nil {
		t.Fatalf("read iteration log: %v", err)
	}
	if !strings.Contains(string(body), "SESSION-STREAM") {
		t.Errorf("the opt-in must ADD the echo, not move the stream out of the log; got:\n%s", body)
	}
}

// The two lines an operator joins the daemon log and the iteration log by: the
// spawn line and the clean end line both name the file.
func TestIterationStartAndEndLinesNameTheLog(t *testing.T) {
	var logged strings.Builder
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 1
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(0, 0)}}}

	if err := runLoop(t, cfg, h, &fakePoller{sum: backlog.Summary{Ready: 1, Claimable: 1}}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	logs := iterationLogPaths(t, cfg.StateDir)
	path := logs[0]

	start, end := "", ""
	for _, line := range strings.Split(logged.String(), "\n") {
		switch {
		case strings.Contains(line, "- spawning "):
			start = line
		case strings.Contains(line, "done (tokens="):
			end = line
		}
	}
	if start == "" || !strings.Contains(start, "log: "+path) {
		t.Errorf("the spawn line must name the iteration log %s; got %q", path, start)
	}
	if end == "" || !strings.Contains(end, "log: "+path) {
		t.Errorf("the end line must name the iteration log %s; got %q", path, end)
	}
}
