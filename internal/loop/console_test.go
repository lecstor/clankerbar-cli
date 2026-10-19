package loop

// The console wiring (CLA-701): a session's stream is written to its iteration
// log, and — only when `echo_sessions` asks — to the daemon's terminal as well.
// There is deliberately no test asserting on os.Stderr itself: the echo
// destination is injectable (Driver.echoOut) precisely so this can be checked
// without capturing the test process's own stderr.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
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

// failingWriter refuses every write, standing in for a terminal whose reader has
// gone away.
type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

// 1.review-1.3: with `echo_sessions: true` the echo and the iteration log are
// fed INDEPENDENTLY. io.MultiWriter wrote the echo first and stopped on its
// error, so a terminal that had gone away cost the log the chunk it would
// otherwise have received — the duplicate was kept at the expense of the record.
func TestAFailingEchoStillLeavesTheWholeStreamInTheLog(t *testing.T) {
	cfg := fastCfg()
	cfg.StateDir = t.TempDir()
	cfg.MaxIterations = 1
	cfg.EchoSessions = true
	h := &consoleAdapter{fakeAdapter: &fakeAdapter{}, stream: "SESSION-STREAM\n"}
	d := New(cfg, h, &fakePoller{sum: backlog.Summary{Ready: 1, Claimable: 1}})
	d.echoOut = failingWriter{err: errors.New("the terminal is gone")}

	runConsoleDriver(t, d)

	logs := iterationLogPaths(t, cfg.StateDir)
	body, err := os.ReadFile(logs[len(logs)-1])
	if err != nil {
		t.Fatalf("read iteration log: %v", err)
	}
	if !strings.Contains(string(body), "SESSION-STREAM") {
		t.Errorf("a failing echo cost the iteration log the stream; got:\n%s", body)
	}
}

// 1.review-1.2 / 1.review-3.1 / 1.review-4.1: the joining is not just for the
// endings that went well. Every end-of-iteration classification names the log of
// the attempt it describes — including the ones an operator actually has to
// diagnose from (a transient blip, an exhausted retry ladder, a non-retryable
// failure, a truncated stream, a launch failure) — and the file it names exists.
func TestEveryIterationEndingNamesItsLog(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   func(*config.Config)
		steps []invokeStep
		// markers are the substrings that identify this ending's line(s) in the
		// daemon log or the returned error.
		markers []string
	}{
		{
			name:    "a recovered provider error",
			steps:   []invokeStep{{res: recoveredResult(10, 0.10)}},
			markers: []string{"recovered from"},
		},
		{
			name:    "a transient failure that is retried",
			steps:   []invokeStep{{res: transientResult()}, {res: okResult(0, 0)}},
			markers: []string{"transient failure"},
		},
		{
			name:    "transient failures that exhaust the retries",
			cfg:     func(c *config.Config) { c.MaxRetries = 1 },
			steps:   []invokeStep{{res: transientResult()}, {res: transientResult()}},
			markers: []string{"transient failures persisted"},
		},
		{
			name:    "a non-retryable failure",
			steps:   []invokeStep{{res: nonRetryableResult()}},
			markers: []string{"non-retryable"},
		},
		{
			name:    "an untrusted stream",
			steps:   []invokeStep{{res: untrustedResult(10, 0.5)}},
			markers: []string{"UNTRUSTED"},
		},
		{
			name:    "an untrusted stream that breaks a spend ceiling",
			cfg:     func(c *config.Config) { c.Budget = config.Budget{MaxTokens: 1_000_000} },
			steps:   []invokeStep{{res: untrustedResult(10, 0.5)}},
			markers: []string{"UNTRUSTED", "ceiling can no longer be honoured"},
		},
		{
			name:    "a launch failure",
			steps:   []invokeStep{{err: errors.New("exec: no such file")}},
			markers: []string{"invoke fake"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := captureLogs(t)
			cfg := fastCfg()
			if tc.cfg != nil {
				tc.cfg(cfg)
			}
			d := New(cfg, &fakeAdapter{steps: tc.steps}, busyPoller())
			openTestStateDir(t, d)

			// An ending that stops the run hands its reason back as an error as
			// often as it logs it, so both are what a reader has to join with.
			_, _, _, err := drainOnce(t, d)
			haystack := logged.String()
			if err != nil {
				haystack += "\n" + err.Error()
			}
			for _, m := range tc.markers {
				assertEndingNamesARealLog(t, haystack, m)
			}
		})
	}
}

// assertEndingNamesARealLog checks that every line carrying marker also names an
// iteration log, and that the file it names is really there. Spawn lines are
// skipped: they carry no marker of their own, but a subtest's temp directory is
// named after it, so a path can contain the marker by accident.
func assertEndingNamesARealLog(t *testing.T, haystack, marker string) {
	t.Helper()
	found := 0
	for _, line := range strings.Split(haystack, "\n") {
		if !strings.Contains(line, marker) {
			continue
		}
		if strings.Contains(line, "- spawning ") || strings.Contains(line, "- retry ") {
			continue
		}
		found++
		i := strings.Index(line, "(log: ")
		if i < 0 {
			t.Errorf("the %q ending does not name the attempt's iteration log: %q", marker, line)
			continue
		}
		rest := line[i+len("(log: "):]
		if j := strings.IndexByte(rest, ')'); j >= 0 {
			rest = rest[:j]
		}
		path := strings.TrimSpace(rest)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the %q ending names %q, which is not a file: %v", marker, path, err)
		}
	}
	if found == 0 {
		t.Errorf("nothing carried %q:\n%s", marker, haystack)
	}
}

// 1.review-4.3 (nit): when the iteration log cannot be opened, the spawn line
// must not send an operator to a file that was never created — it names the sink
// the stream is actually going to.
func TestSpawnLineNamesTheFallbackWhenTheLogCannotBeOpened(t *testing.T) {
	logged := captureLogs(t)
	cfg := fastCfg()
	d := New(cfg, &fakeAdapter{steps: []invokeStep{{res: okResult(0, 0)}}}, busyPoller())
	st, err := statedir.Open(t.TempDir())
	if err != nil {
		t.Fatalf("statedir.Open: %v", err)
	}
	d.state = st
	// Every Create now fails — the shape of a state dir the daemon can no longer
	// write to, without having to fake an ENOSPC.
	if err := st.Close(); err != nil {
		t.Fatalf("close the state dir: %v", err)
	}

	if _, _, _, err := drainOnce(t, d); err != nil {
		t.Fatalf("drain: %v", err)
	}

	spawn := ""
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, "- spawning ") {
			spawn = line
		}
	}
	if spawn == "" {
		t.Fatalf("no spawn line was logged:\n%s", logged.String())
	}
	if !strings.Contains(spawn, "log: none (could not open ") {
		t.Errorf("the spawn line must name the sink the stream really went to, not a file that was never created; got %q", spawn)
	}
}
