// Package cli wires the command-line surface to the loop driver.
package cli

import (
	"fmt"
	"io"
)

// Usage prints the top-level help.
func Usage(w io.Writer) {
	fmt.Fprint(w, `clankerbar — drive a coding agent through your clankerbar backlog, unattended.

Usage:
  clankerbar run [flags]     Start the loop: respawn fresh harness sessions that
                             work the backlog one task at a time, surviving usage
                             limits. The "prompt" config knob changes how much a
                             single session takes on.
  clankerbar ctl [flags]     Control a RUNNING loop by writing a marker into its
                             state dir: "ctl restart" re-execs the daemon at the
                             next iteration boundary (--now kills the in-flight
                             session and releases its claim first); "ctl reload"
                             re-reads the config file without replacing anything.
                             A restart preserves the daemon's CURRENT environment
                             — it cannot conjure env it was never launched with.
  clankerbar doctor [flags]  Preflight the setup — config, harness, backlog
                             wiring, workdir, permissions. Exits non-zero on any
                             FAIL, so it gates a run: doctor && run.
  clankerbar dead-rate       Scan the iteration logs and report, per day, per
                             phase and per harness, how many sessions ran and
                             how many died producing nothing (the dead-phase
                             rate).
  clankerbar version         Print the version.
  clankerbar help            Show this help.

Run 'clankerbar run --help', 'clankerbar ctl --help' or 'clankerbar doctor --help'
for their flags.
`)
}
