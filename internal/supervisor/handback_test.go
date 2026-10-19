package supervisor

// CLA-708: a child the supervisor sees exit while it still holds a claim. The
// daemon leaves CURRENT_CLAIM in its state dir on every path where it cannot let
// go itself; the supervisor reads it before any respawn and hands the claim back
// — `release` when nothing was pushed, `expireLease` when a branch is recorded.

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/loop"
)

func TestDeadChildsClaimIsHandedBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   string
		branch string
	}{
		{name: "no branch: released", mode: "crash-claim"},
		{name: "branch recorded: expireLease", mode: "crash-claim-branch", branch: "clanker/claim-wip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cacheDir := t.TempDir()
			setHelperMode(t, tc.mode)
			plane := &fakePlane{}
			plane.set([]RosterEntry{runEntry("acme-1", "")})
			srv := plane.serve(t)

			o := testOptions(t, cacheDir, srv)
			// One crash, no respawn: the respawn timeout is far outside the test
			// window, so exactly one exit reaches onExit and one handback is owed.
			o.BackoffBase = time.Hour
			o.BackoffCap = time.Hour

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := runSupervise(ctx, o)

			state := entryStateDir(cacheDir, "acme-1")
			waitFor(t, 5*time.Second, "the dead child's claim to be handed back", func() bool {
				return len(plane.planeCalls()) > 0
			})

			calls := plane.planeCalls()
			if len(calls) != 1 {
				t.Fatalf("%d handback call(s), want exactly 1: %+v", len(calls), calls)
			}
			call := calls[0]
			if call.path != "/mcp/acme" {
				t.Errorf("call path = %q, want the project's MCP endpoint /mcp/acme", call.path)
			}
			if call.tool != "update_task" {
				t.Errorf("tool = %q, want update_task", call.tool)
			}
			for key, want := range map[string]any{
				"taskId":         "t-claim",
				"runId":          "r-claim",
				"driverReleased": true,
			} {
				if got := call.args[key]; got != want {
					t.Errorf("arguments[%q] = %v, want %v (args: %+v)", key, got, want, call.args)
				}
			}
			if tc.branch == "" {
				if call.args["release"] != true {
					t.Errorf("a claim with no branch must be released (args: %+v)", call.args)
				}
				if _, present := call.args["expireLease"]; present {
					t.Errorf("release and expireLease are mutually exclusive; got both: %+v", call.args)
				}
			} else {
				if call.args["expireLease"] != true {
					t.Errorf("a claim with a recorded branch must use expireLease (args: %+v)", call.args)
				}
				if _, present := call.args["release"]; present {
					t.Errorf("expireLease must not be paired with release; got both: %+v", call.args)
				}
			}

			// The record describes a process that is gone; leaving it would make
			// the NEXT child's exit hand back the wrong claim.
			cancel()
			waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
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
			if _, err := os.Lstat(filepath.Join(state, loop.CurrentClaimName)); !os.IsNotExist(err) {
				t.Fatalf("%s still present after the handback (err=%v)", loop.CurrentClaimName, err)
			}
		})
	}
}

// A refusal means the run already ended or was taken over: nothing to do, and
// nothing to crash the supervisor over. This is the common shape after a daemon
// settles its task and dies before removing its record.
func TestDeadChildsClaimRefusalIsALoggedNoOp(t *testing.T) {
	for _, refusal := range []string{"run_superseded", "not_holding"} {
		t.Run(refusal, func(t *testing.T) {
			var buf lockedBuffer
			log.SetOutput(&buf)
			defer log.SetOutput(os.Stderr)

			cacheDir := t.TempDir()
			setHelperMode(t, "crash-claim-branch")
			plane := &fakePlane{}
			plane.set([]RosterEntry{runEntry("acme-1", "")})
			plane.setPlaneRefusal(refusal)
			srv := plane.serve(t)

			o := testOptions(t, cacheDir, srv)
			o.BackoffBase = time.Hour
			o.BackoffCap = time.Hour

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := runSupervise(ctx, o)
			state := entryStateDir(cacheDir, "acme-1")

			waitFor(t, 5*time.Second, "the refused handback", func() bool {
				return len(plane.planeCalls()) > 0
			})
			waitFor(t, 5*time.Second, "the refusal to be logged", func() bool {
				return strings.Contains(buf.String(), "nothing to hand back")
			})
			if strings.Contains(buf.String(), "could not hand") {
				t.Errorf("a refusal must not be logged as a failure:\n%s", buf.String())
			}
			cancel()
			waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
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
			if _, err := os.Lstat(filepath.Join(state, loop.CurrentClaimName)); !os.IsNotExist(err) {
				t.Fatalf("%s survived a refused handback (err=%v)", loop.CurrentClaimName, err)
			}
		})
	}
}

// A clean exit has no record, so the supervisor makes no plane call at all: the
// feature must not turn every respawn into a write.
func TestCleanChildExitMakesNoHandbackCall(t *testing.T) {
	cacheDir := t.TempDir()
	setHelperMode(t, "crash")
	plane := &fakePlane{}
	plane.set([]RosterEntry{runEntry("acme-1", "")})
	srv := plane.serve(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runSupervise(ctx, testOptions(t, cacheDir, srv))

	waitFor(t, 5*time.Second, "the crashing child to be respawned", func() bool {
		return countRuns(t, entryStateDir(cacheDir, "acme-1")) >= 2
	})
	if calls := plane.planeCalls(); len(calls) != 0 {
		t.Fatalf("a child that left no claim still produced %d plane call(s): %+v", len(calls), calls)
	}

	cancel()
	waitFor(t, 5*time.Second, "the supervisor to return after the stop", func() bool {
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

func TestMCPEndpointFromRoster(t *testing.T) {
	for _, tc := range []struct{ name, roster, project, want string }{
		{"project path", "https://clankerbar.com/api/daemon-roster", "acme", "https://clankerbar.com/mcp/acme"},
		{"no project keeps the legacy slug-less path", "https://clankerbar.com/api/daemon-roster", "", "https://clankerbar.com/mcp"},
		{"unparseable roster URL", "not a url", "acme", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mcpEndpointFromRoster(tc.roster, tc.project); got != tc.want {
				t.Errorf("mcpEndpointFromRoster(%q, %q) = %q, want %q", tc.roster, tc.project, got, tc.want)
			}
		})
	}
}

// The two refusals the plane uses for "the run is already over", in the shapes
// its text arrives in, are no-ops; anything else is a real failure.
func TestRefusedClaimHandback(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{"update_task refused: not_holding", true},
		{"run_superseded", true},
		{"update_task refused: run superseded", true},
		{"update_task: HTTP 500: boom", false},
		{"not_holding_but_broken", true},
	} {
		err := errors.New(tc.msg)
		if got := refusedClaimHandback(err); got != tc.want {
			t.Errorf("refusedClaimHandback(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
	if refusedClaimHandback(nil) {
		t.Error("nil must not be a refusal")
	}
}
