package supervisor

// The supervisor's status report (CLA-677): the POST body per instance state,
// the fail-soft reporter, and the wiring that makes a poll and a state change
// both reach the plane. The wire struct here is deliberately a separate decode
// of the documented JSON — a test that reused the production structs would
// pass even if both sides were renamed together.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/version"
)

// statusBody is the documented wire shape: what CLA-676's route stores and what
// the console reads.
type statusBody struct {
	Host              string               `json:"host"`
	SupervisorVersion string               `json:"supervisorVersion"`
	StartedAt         string               `json:"startedAt"`
	Instances         []statusBodyInstance `json:"instances"`
}

type statusBodyInstance struct {
	Name         string          `json:"name"`
	State        string          `json:"state"`
	Reason       string          `json:"reason"`
	Since        string          `json:"since"`
	ChildVersion string          `json:"childVersion"`
	LastExit     *statusBodyExit `json:"lastExit"`
	NextRetryAt  *string         `json:"nextRetryAt"`
}

type statusBodyExit struct {
	Code int    `json:"code"`
	At   string `json:"at"`
}

// exitErrorWithCode produces a real *exec.ExitError with the given code: the
// status report's `lastExit.code` must be the process's own status, and a
// fabricated error would not prove the mapping reads it.
func exitErrorWithCode(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if err == nil {
		t.Fatalf("sh -c \"exit %d\" returned nil, want an exit error", code)
	}
	return err
}

// TestStatusEndpoint pins the derivation: the roster URL is the supervisor's
// only operator-stated plane address, and the status route is its child.
func TestStatusEndpoint(t *testing.T) {
	cases := map[string]string{
		"": "",
		"https://clankerbar.com/api/daemon-roster":   "https://clankerbar.com/api/daemon-roster/status",
		"https://clankerbar.com/api/daemon-roster/":  "https://clankerbar.com/api/daemon-roster/status",
		"http://127.0.0.1:8080/api/daemon-roster///": "http://127.0.0.1:8080/api/daemon-roster/status",
	}
	for in, want := range cases {
		if got := statusEndpoint(in); got != want {
			t.Errorf("statusEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestStatusReportBodyPerState is the core mapping: every one of the five
// states, with the reason, since, childVersion, lastExit and nextRetryAt the
// console needs to tell a HALT apart from a crash, a sleeping machine apart
// from a stopped one.
func TestStatusReportBodyPerState(t *testing.T) {
	startedAt := time.Date(2026, 10, 18, 6, 35, 0, 0, time.UTC)
	aliveSince := time.Date(2026, 10, 18, 7, 0, 0, 0, time.UTC)
	exitAt := time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)
	retryAt := time.Date(2026, 10, 18, 8, 0, 2, 0, time.UTC)
	crash := exitErrorWithCode(t, 3)

	cases := []struct {
		name      string
		inst      *Instance
		stopping  bool
		replacing bool
		want      statusBodyInstance
	}{
		{
			name: "running",
			inst: &Instance{
				name: "clanker1", desired: RosterDesiredRunning,
				cmd: &exec.Cmd{}, aliveSince: aliveSince, childVersion: "0.16.0",
			},
			want: statusBodyInstance{
				Name: "clanker1", State: StatusRunning, Since: statusTime(aliveSince), ChildVersion: "0.16.0",
			},
		},
		{
			name: "running and draining for a requested stop",
			inst: &Instance{
				name: "clanker1", desired: RosterDesiredRunning,
				cmd: &exec.Cmd{}, aliveSince: aliveSince, childVersion: "0.16.0", stopRequested: true,
			},
			want: statusBodyInstance{
				Name: "clanker1", State: StatusRunning, Reason: "stop requested - draining at its iteration boundary",
				Since: statusTime(aliveSince), ChildVersion: "0.16.0",
			},
		},
		{
			// A REPLACE drain is a stop the supervisor ordered: the children it
			// has already drained say stopped, and the ones still alive name the
			// replacement — never `exited`, which is the crash vocabulary.
			name:      "running while replacing",
			replacing: true,
			inst: &Instance{
				name: "clanker1", desired: RosterDesiredRunning,
				cmd: &exec.Cmd{}, aliveSince: aliveSince, childVersion: "0.16.0",
			},
			want: statusBodyInstance{
				Name: "clanker1", State: StatusRunning, Reason: "replacing myself - draining at the iteration boundary",
				Since: statusTime(aliveSince), ChildVersion: "0.16.0",
			},
		},
		{
			name:      "drained for a replace",
			replacing: true,
			inst: &Instance{
				name: "clanker1", desired: RosterDesiredRunning,
				exited: true, childVersion: "0.16.0", exitErr: crash, exitAt: exitAt,
			},
			want: statusBodyInstance{
				Name: "clanker1", State: StatusStopped, Reason: "replacing myself - drained at the iteration boundary",
				Since: statusTime(exitAt), ChildVersion: "0.16.0",
				LastExit: &statusBodyExit{Code: 3, At: statusTime(exitAt)},
			},
		},
		{
			name: "halted",
			inst: &Instance{
				name: "clanker2", desired: RosterDesiredRunning, halted: true,
				childVersion: "0.16.0", exitErr: crash, exitAt: exitAt, stateDir: "/state/clanker2",
			},
			want: statusBodyInstance{
				Name: "clanker2", State: StatusHalted, Reason: "HALT marker present in /state/clanker2",
				Since: statusTime(exitAt), ChildVersion: "0.16.0",
				LastExit: &statusBodyExit{Code: 3, At: statusTime(exitAt)},
			},
		},
		{
			name: "backing off after a crash",
			inst: &Instance{
				name: "clanker3", desired: RosterDesiredRunning,
				childVersion: "0.16.0", exitErr: crash, exitAt: exitAt,
				restartAt: retryAt, lastBackoff: 2 * time.Second,
			},
			want: statusBodyInstance{
				Name: "clanker3", State: StatusBackingOff, Reason: "restart scheduled in 2s",
				Since: statusTime(exitAt), ChildVersion: "0.16.0",
				LastExit:    &statusBodyExit{Code: 3, At: statusTime(exitAt)},
				NextRetryAt: statusOptionalTime(retryAt),
			},
		},
		{
			name: "exited with no restart scheduled",
			inst: &Instance{
				name: "clanker4", desired: RosterDesiredRunning,
				exited: true, childVersion: "0.16.0", exitAt: exitAt,
			},
			want: statusBodyInstance{
				Name: "clanker4", State: StatusExited, Reason: "child exited and no restart is scheduled",
				Since: statusTime(exitAt), ChildVersion: "0.16.0",
				LastExit: &statusBodyExit{Code: 0, At: statusTime(exitAt)},
			},
		},
		{
			name: "clean stop",
			inst: &Instance{
				name: "clanker5", desired: RosterDesiredStopped,
				exited: true, childVersion: "0.16.0", exitAt: exitAt,
			},
			want: statusBodyInstance{
				Name: "clanker5", State: StatusStopped, Reason: "desired state is stopped",
				Since: statusTime(exitAt), ChildVersion: "0.16.0",
				LastExit: &statusBodyExit{Code: 0, At: statusTime(exitAt)},
			},
		},
		{
			// A crash scheduled a retry, then the operator flipped the entry to
			// stopped before the retry fired: the retry will never run
			// (respawnDue refuses against the current desired state), so the row
			// is stopped with a null nextRetryAt, not a phantom backing_off.
			name: "stopped while a retry is pending",
			inst: &Instance{
				name: "clanker5", desired: RosterDesiredStopped,
				exited: true, childVersion: "0.16.0", exitErr: crash, exitAt: exitAt,
				restartAt: retryAt, lastBackoff: 2 * time.Second,
			},
			want: statusBodyInstance{
				Name: "clanker5", State: StatusStopped, Reason: "desired state is stopped",
				Since: statusTime(exitAt), ChildVersion: "0.16.0",
				LastExit: &statusBodyExit{Code: 3, At: statusTime(exitAt)},
			},
		},
		{
			// While the supervisor stops, a retry scheduled before the stop is
			// moot (the select loop is not running, so it can never spawn), so
			// the row must not carry a nextRetryAt either.
			name:     "supervisor stopping while a retry is pending",
			stopping: true,
			inst: &Instance{
				name: "clanker5", desired: RosterDesiredRunning,
				exited: true, childVersion: "0.16.0", exitErr: crash, exitAt: exitAt,
				restartAt: retryAt, lastBackoff: 2 * time.Second,
			},
			want: statusBodyInstance{
				Name: "clanker5", State: StatusStopped, Reason: "supervisor stopping",
				Since: statusTime(exitAt), ChildVersion: "0.16.0",
				LastExit: &statusBodyExit{Code: 3, At: statusTime(exitAt)},
			},
		},
		{
			name: "stopped entry that never spawned",
			inst: &Instance{name: "clanker6", desired: RosterDesiredStopped},
			want: statusBodyInstance{Name: "clanker6", State: StatusStopped, Reason: "desired state is stopped"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Supervisor{hostname: "Jasons-MBP.localdomain", startedAt: startedAt, stopping: tc.stopping, replacing: tc.replacing}
			d.instances = []*Instance{tc.inst}

			body, err := marshalStatus(d.statusReport())
			if err != nil {
				t.Fatalf("marshalStatus: %v", err)
			}
			var got statusBody
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("decode: %v\nbody: %s", err, body)
			}
			if got.Host != "Jasons-MBP.localdomain" {
				t.Errorf("host = %q", got.Host)
			}
			if got.SupervisorVersion != version.Current {
				t.Errorf("supervisorVersion = %q, want %q", got.SupervisorVersion, version.Current)
			}
			if got.StartedAt != statusTime(startedAt) {
				t.Errorf("startedAt = %q, want %q", got.StartedAt, statusTime(startedAt))
			}
			if len(got.Instances) != 1 {
				t.Fatalf("instances = %+v, want one", got.Instances)
			}
			inst := got.Instances[0]
			if inst.Name != tc.want.Name || inst.State != tc.want.State || inst.Reason != tc.want.Reason ||
				inst.Since != tc.want.Since || inst.ChildVersion != tc.want.ChildVersion {
				t.Errorf("instance = %+v, want %+v", inst, tc.want)
			}
			if (inst.LastExit == nil) != (tc.want.LastExit == nil) {
				t.Fatalf("lastExit = %+v, want %+v", inst.LastExit, tc.want.LastExit)
			}
			if inst.LastExit != nil && *inst.LastExit != *tc.want.LastExit {
				t.Errorf("lastExit = %+v, want %+v", *inst.LastExit, *tc.want.LastExit)
			}
			if (inst.NextRetryAt == nil) != (tc.want.NextRetryAt == nil) {
				t.Fatalf("nextRetryAt = %v, want %v", inst.NextRetryAt, tc.want.NextRetryAt)
			}
			if inst.NextRetryAt != nil && *inst.NextRetryAt != *tc.want.NextRetryAt {
				t.Errorf("nextRetryAt = %q, want %q", *inst.NextRetryAt, *tc.want.NextRetryAt)
			}
		})
	}
}

// The wire shape uses explicit nulls for the two optional times, so the console
// can distinguish "never exited" from "field not served".
func TestStatusMarshalOptionalTimesAreExplicitNull(t *testing.T) {
	b, err := marshalStatus(StatusReport{
		Host:      "h",
		StartedAt: time.Now(),
		Instances: []StatusInstance{{Name: "a", State: StatusStopped}},
	})
	if err != nil {
		t.Fatalf("marshalStatus: %v", err)
	}
	for _, key := range []string{`"lastExit":null`, `"nextRetryAt":null`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("report does not carry %s: %s", key, b)
		}
	}
}

// A reporter bug must never store a garbage row, exactly as fleet refuses an
// unnamed instance and an unknown outcome: the plane's all-or-nothing write
// would lose the healthy rows beside it.
func TestStatusMarshalRefusesUnknownState(t *testing.T) {
	if _, err := marshalStatus(StatusReport{
		Instances: []StatusInstance{{Name: "a", State: "sleeping"}},
	}); err == nil {
		t.Error("an unknown state must be refused, not stored")
	}
	if _, err := marshalStatus(StatusReport{
		Instances: []StatusInstance{{State: StatusRunning}},
	}); err == nil {
		t.Error("an unnamed instance must be refused, not stored")
	}
}

// A failing status POST is an outage, not an event: the first failure is logged
// and the rest of the streak is silent. Close after the streak proves all
// deliveries were attempted, so the count is not a race.
func TestStatusReporterLogsOneLinePerOutage(t *testing.T) {
	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	var authMu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authMu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		authMu.Unlock()
		http.Error(w, "plane incident", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	rep := newStatusReporter(srv.URL, "acct-key")
	rep.Send(StatusReport{Host: "h"})
	rep.Send(StatusReport{Host: "h"})
	rep.Close(StatusReport{Host: "h"})

	if got := strings.Count(buf.String(), "supervisor status report failed and was dropped"); got != 1 {
		t.Fatalf("failure lines = %d, want exactly 1 for the whole outage:\n%s", got, buf.String())
	}
	authMu.Lock()
	defer authMu.Unlock()
	if len(auths) == 0 {
		t.Fatal("no request reached the endpoint")
	}
	for _, a := range auths {
		if a != "Bearer acct-key" {
			t.Errorf("Authorization = %q, want the account bearer key", a)
		}
	}
}

// A 404 is the pre-CLA-676 shape, not an outage: the route does not exist yet,
// so the reporter attempts the POST and stays silent — no failure line, no
// recovery line. Keeping it out of the failure streak matters: counting it
// would swallow the first real 5xx after the route ships.
func TestStatusReporterExpected404IsSilent(t *testing.T) {
	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "route not deployed yet", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	rep := newStatusReporter(srv.URL, "k")
	for i := 0; i < 3; i++ {
		rep.Send(StatusReport{Host: "h"})
	}
	rep.Close(StatusReport{Host: "h"})

	if got := hits.Load(); got != 4 {
		t.Fatalf("POSTs = %d, want 4 (three queued + the Close report)", got)
	}
	if got := buf.String(); strings.Contains(got, "supervisor status") {
		t.Fatalf("a 404 before CLA-676 must be silent, got:\n%s", got)
	}
}

// Recovery is said once, with how many reports the outage lost — and it proves
// the streak is reset rather than the reporter staying quiet for good.
func TestStatusReporterLogsRecoveryOnce(t *testing.T) {
	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	var failing atomic.Bool
	failing.Store(true)
	arrived := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			http.Error(w, "outage", http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		arrived <- struct{}{}
	}))
	t.Cleanup(srv.Close)

	rep := newStatusReporter(srv.URL, "k")
	rep.Send(StatusReport{Host: "h"})
	<-arrived
	waitFor(t, time.Second, "the first failure to be logged", func() bool {
		return strings.Contains(buf.String(), "failed and was dropped")
	})

	failing.Store(false)
	rep.Send(StatusReport{Host: "h"})
	<-arrived
	waitFor(t, time.Second, "the recovery line", func() bool {
		return strings.Contains(buf.String(), "supervisor status reporting recovered after 1 failed report")
	})
	rep.Close(StatusReport{Host: "h"})
}

// --- wiring: every poll and every transition reaches the plane ---------------

// runningInstance returns the first instance row named name across the reports.
func runningInstance(rep statusBody, name string) *statusBodyInstance {
	for i := range rep.Instances {
		if rep.Instances[i].Name == name {
			return &rep.Instances[i]
		}
	}
	return nil
}

// waitForStatusState waits until any report carries name in state and returns
// that report.
func waitForStatusState(t *testing.T, plane *fakePlane, name, state string) statusBody {
	t.Helper()
	var found statusBody
	waitFor(t, 5*time.Second, fmt.Sprintf("a status report with %s=%s", name, state), func() bool {
		for _, rep := range plane.statusReports(t) {
			if inst := runningInstance(rep, name); inst != nil && inst.State == state {
				found = rep
				return true
			}
		}
		return false
	})
	return found
}

// A running child is reported, every poll reports again, and the final report
// after a clean stop says `stopped` — not `running` for a process that is gone.
func TestStatusReportsOnEveryPollAndOnExit(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", "")})
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	state := entryStateDir(cacheDir, "daemon-one")
	waitFor(t, 5*time.Second, "the child to come up", func() bool { return countRuns(t, state) == 1 })

	running := waitForStatusState(t, plane, "daemon-one", StatusRunning)
	if running.Host == "" || running.SupervisorVersion != version.Current || running.StartedAt == "" {
		t.Errorf("report identity = %+v, want host, version and startedAt", running)
	}
	inst := runningInstance(running, "daemon-one")
	if inst.ChildVersion != version.Current {
		t.Errorf("childVersion = %q, want the version recorded at spawn (%q)", inst.ChildVersion, version.Current)
	}
	if inst.Since == "" {
		t.Error("running instance carries no since time")
	}

	// Every poll reports: a 50ms poll interval fills this quickly while the
	// child idles. THREE reports is the explicit "not just the state change"
	// assertion.
	waitFor(t, 5*time.Second, "at least three status reports (one per poll)", func() bool {
		return len(plane.statusReports(t)) >= 3
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Supervise returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Supervise did not return after the stop")
	}

	reports := plane.statusReports(t)
	last := reports[len(reports)-1]
	inst = runningInstance(last, "daemon-one")
	if inst == nil {
		t.Fatalf("final report carries no daemon-one row: %+v", last)
	}
	if inst.State != StatusStopped {
		t.Errorf("final state = %q, want stopped (the supervisor took the child down)", inst.State)
	}
	if inst.LastExit == nil {
		t.Errorf("final row has no lastExit: %+v", inst)
	}
}

// A HALT is not a crash: the report names the marker, keeps the exit code, and
// schedules no retry.
func TestStatusReportHaltedState(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "halt")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("halted-one", "")})
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	rep := waitForStatusState(t, plane, "halted-one", StatusHalted)
	inst := runningInstance(rep, "halted-one")
	if !strings.Contains(inst.Reason, "HALT marker present") {
		t.Errorf("reason = %q, want the HALT marker named", inst.Reason)
	}
	if inst.LastExit == nil || inst.LastExit.Code != 1 {
		t.Errorf("lastExit = %+v, want the fake daemon's exit 1", inst.LastExit)
	}
	if inst.NextRetryAt != nil {
		t.Errorf("nextRetryAt = %v, want null - a HALT is never respawned", *inst.NextRetryAt)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Supervise returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Supervise did not return")
	}
}

// A crash is reported as backing_off with the exit code and the retry time —
// the row the console needs to tell a crash loop from a sleeping machine.
func TestStatusReportCrashBackoffState(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "crash")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("crashy", "")})
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	rep := waitForStatusState(t, plane, "crashy", StatusBackingOff)
	inst := runningInstance(rep, "crashy")
	if inst.LastExit == nil || inst.LastExit.Code != 1 {
		t.Errorf("lastExit = %+v, want the crashing child's exit 1", inst.LastExit)
	}
	if inst.NextRetryAt == nil {
		t.Error("nextRetryAt is null while a restart is scheduled")
	}
	if inst.Since == "" {
		t.Error("backing-off row carries no since time")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Supervise returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Supervise did not return")
	}
}

// A 404 (CLA-676 has not landed) or a 5xx (plane incident) is telemetry, not
// control flow: children still spawn and stop either way. Only the 5xx is an
// outage — the 404 is the expected shape until the route lands, so it stays
// silent while the 5xx logs once.
func TestStatusReportFailureNeverAffectsSupervision(t *testing.T) {
	cases := []struct {
		name       string
		code       int
		wantOutage int
	}{
		{"HTTP_404_is_silent", http.StatusNotFound, 0},
		{"HTTP_500_logs_once", http.StatusInternalServerError, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf lockedBuffer
			log.SetOutput(&buf)
			defer log.SetOutput(os.Stderr)

			cacheDir := t.TempDir()
			setHelperMode(t, "sleep")
			plane := &fakePlane{}
			plane.set([]RosterEntry{runEntry("daemon-one", "")})
			plane.setStatusCode(tc.code)
			srv := plane.serve(t)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := runSupervise(ctx, testOptions(t, cacheDir, srv))

			state := entryStateDir(cacheDir, "daemon-one")
			waitFor(t, 5*time.Second, "the child to come up despite the failing status endpoint", func() bool {
				return countRuns(t, state) == 1
			})
			// The plane records a body before answering with the failure code,
			// so this proves the POST path ran (and was judged) rather than
			// never reaching the endpoint.
			waitFor(t, 5*time.Second, "a status report to reach the endpoint", func() bool {
				return len(plane.statusReports(t)) >= 1
			})

			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Supervise returned %v, want nil", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Supervise did not return; a failed status POST must not affect supervision")
			}
			if got := countRuns(t, state); got != 1 {
				t.Fatalf("spawns = %d, want exactly 1 - a failed status POST must not cause a respawn", got)
			}
			if got := strings.Count(buf.String(), "supervisor status report failed"); got != tc.wantOutage {
				t.Fatalf("failure lines = %d, want %d (404 is the expected pre-CLA-676 shape; 5xx is an outage):\n%s",
					got, tc.wantOutage, buf.String())
			}
		})
	}
}

// A fleet stop can drain for minutes (an in-flight session finishes at its
// boundary); the plane must learn the supervisor is stopping NOW, not when the
// first child's exit arrives. The child here ignores STOP forever, so no exit
// ever happens and the only report that can carry the stopping reason is the
// one stopAll queues on entering the draining state.
func TestStatusReportShutdownTransitionBeforeTheDrainEnds(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "stubborn")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("stuck", "")})
	srv := plane.serve(t)

	var buf lockedBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	o := testOptions(t, cacheDir, srv)
	o.StopLogEvery = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := runSupervise(ctx, o)

	waitFor(t, 5*time.Second, "the stubborn child to come up", func() bool {
		return strings.Contains(buf.String(), "spawned (pid")
	})
	cancel()

	waitFor(t, 5*time.Second, "a report naming the supervisor's stopping drain before any exit", func() bool {
		for _, rep := range plane.statusReports(t) {
			if inst := runningInstance(rep, "stuck"); inst != nil &&
				inst.State == StatusRunning && strings.Contains(inst.Reason, "supervisor stopping") {
				return true
			}
		}
		return false
	})

	// The stop is stuck by construction; kill the child so the test leaves no
	// orphan, and verify the supervisor then finishes.
	if pid := spawnPid(t, buf.String()); pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	waitFor(t, 5*time.Second, "the supervisor to return once the child is gone", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Supervise returned %v, want nil", err)
			}
			return true
		default:
			return false
		}
	})
}

// A REPLACE drain is a stop the supervisor ordered, so the children it drained
// must read as `stopped` (a planned upgrade), never `exited` (a fleet crash).
// The exec hook keeps the supervisor alive, so the drained report stays in the
// plane's history while the fleet respawns afterwards.
func TestStatusReportReplaceDrainReportsStopped(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "sleep")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("daemon-one", "")})
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := testOptions(t, cacheDir, srv)
	o.LaunchVersion = func() (string, error) { return version.Current, nil }
	o.VerifyTimeout = 5 * time.Second
	rec := &execRecorder{}
	o.ExecInPlace = rec.hook()
	done := runSupervise(ctx, o)

	state := entryStateDir(cacheDir, "daemon-one")
	waitFor(t, 5*time.Second, "the child to come up", func() bool { return countRuns(t, state) == 1 })

	if err := Replace(context.Background(), o); err != nil {
		t.Fatalf("Replace returned %v, want nil", err)
	}
	waitFor(t, 5*time.Second, "the exec to be invoked after the drain", func() bool {
		return rec.len() == 1
	})

	var drained *statusBodyInstance
	waitFor(t, 5*time.Second, "the replace drain reported as stopped", func() bool {
		for _, rep := range plane.statusReports(t) {
			if inst := runningInstance(rep, "daemon-one"); inst != nil &&
				inst.State == StatusStopped && strings.Contains(inst.Reason, "replacing myself") {
				drained = inst
				return true
			}
		}
		return false
	})
	if drained.NextRetryAt != nil {
		t.Errorf("drained row carries nextRetryAt = %v; a replace never schedules a retry", *drained.NextRetryAt)
	}
	if drained.LastExit == nil {
		t.Error("drained row carries no lastExit")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Supervise returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Supervise did not return after the stop")
	}
}

// A desired-state flip to stopped drops the retry the instance was waiting on:
// respawnDue refuses to spawn against a non-running desired state, so keeping
// the timestamp would let the report promise a retry that cannot fire.
func TestEnsureStoppedClearsAPendingRetry(t *testing.T) {
	d := &Supervisor{}
	inst := &Instance{
		name: "a", desired: RosterDesiredStopped,
		exited: true, restartAt: time.Now().Add(time.Minute),
	}
	d.ensureStopped(inst)
	if !inst.restartAt.IsZero() {
		t.Fatalf("restartAt = %v, want zero once the entry is stopped", inst.restartAt)
	}
}
