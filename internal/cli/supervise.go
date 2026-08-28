package cli

// The fleet supervisor (CLA-525, phases 1-2 of docs/proposals/daemon-supervisor.md
// in the clankerbar repo): bare `clankerbar` — and the explicit
// `clankerbar supervise` alias — starts every daemon whose config file is in
// the config dir as a supervised child, restarts a child that exits
// unexpectedly (with backoff), and turns its own SIGINT/SIGTERM into a
// fleet-wide stop: every child gets a STOP marker in its state dir, the same
// marker a hand-written `touch STOP` writes, so each drains at its iteration
// boundary rather than being killed mid-session, and the supervisor waits for
// all of them. Since phase 2b each child runs on a config the supervisor
// GENERATES into the child's state dir (`materialized.json`), never on the
// hand-maintained file in the config dir.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/supervisor"
)

// WorkdirRootEnv is the environment variable naming the machine-stated root
// the supervisor derives each child's workdir from (daemon-supervisor phase
// 2a): <root>/<repo name> for the repo the instance's project names. It is an
// environment variable because the supervisor's launch story is env-shaped
// (launchd units, the account key beside it) and because the derived value is
// a machine fact, never something a config file on the plane may set. Empty =
// derivation is off and children run on their config files' own workdirs.
const WorkdirRootEnv = "CLANKERBAR_WORKDIR_ROOT"

// Supervise runs the fleet supervisor. args carries the flags of the
// invocation: the bare `clankerbar` form passes none, `clankerbar supervise`
// passes whatever followed the subcommand. Phase 1 takes no flags beyond the
// shared help.
func Supervise(ctx context.Context, args []string) error {
	fs := newFlagSet("supervise")
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil // --help already printed usage
		}
		return err
	}

	// Children are spawned with THIS binary, resolved the same way a restart
	// re-exec resolves itself (launchBinary): an operator who launches through
	// a symlink on PATH keeps getting the symlink, so installing a new build
	// and restarting the supervisor runs children on the new build.
	bin, err := launchBinary(os.Args)
	if err != nil {
		return fmt.Errorf("supervise: %w", err)
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return fmt.Errorf("supervise: %w", err)
	}
	log.Printf("supervising instances from %s", dir)
	o := supervisor.Options{
		ConfigDir: dir,
		Binary:    bin,
	}
	if root := os.Getenv(WorkdirRootEnv); root != "" {
		o.WorkdirRoot = root
		log.Printf("deriving children's workdirs from %s (set by %s) - an instance whose derived workdir is missing or is not a checkout of its project's repo will not be started", root, WorkdirRootEnv)
	}
	return supervisor.Supervise(ctx, o)
}
