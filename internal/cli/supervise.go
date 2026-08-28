package cli

// The fleet supervisor (CLA-525, phase 1 of docs/proposals/daemon-supervisor.md
// in the clankerbar repo): bare `clankerbar` — and the explicit
// `clankerbar supervise` alias — starts every daemon whose config file is in
// the config dir as a supervised child, restarts a child that exits
// unexpectedly (with backoff), and turns its own SIGINT/SIGTERM into a
// fleet-wide stop: every child gets a STOP marker in its state dir, the same
// marker a hand-written `touch STOP` writes, so each drains at its iteration
// boundary rather than being killed mid-session, and the supervisor waits for
// all of them.

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
	return supervisor.Supervise(ctx, supervisor.Options{
		ConfigDir: dir,
		Binary:    bin,
	})
}
