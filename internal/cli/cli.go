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
  clankerbar                    THE FLEET SUPERVISOR: start every daemon whose
                                config file is in ~/.config/clankerbar as a
                                supervised child ("run -c <file>" per file),
                                restart one that exits unexpectedly (with
                                backoff), and on SIGINT/SIGTERM stop them all
                                by writing each one's STOP marker, so every
                                daemon drains at its iteration boundary.
                                Same as "clankerbar supervise".
  clankerbar run [flags]        Start the loop: respawn fresh harness sessions that
                                work the backlog one task at a time, surviving usage
                                limits. The "prompt" config knob changes how much a
                                single session takes on.
  clankerbar ctl [flags]        Control a RUNNING loop by writing a marker into its
                                state dir: "ctl restart" re-execs the daemon at the
                                next iteration boundary (--now kills the in-flight
                                session and releases its claim first); "ctl reload"
                                re-reads the config file without replacing anything.
                                A restart preserves the daemon's CURRENT environment
                                — it cannot conjure env it was never launched with.
  clankerbar doctor [flags]     Preflight the setup — config, harness, backlog
                                wiring, workdir, permissions. Exits non-zero on any
                                FAIL, so it gates a run: doctor && run.
  clankerbar propose-config     Import the local config's movable dials (harness,
  [flags]                       model, tiers, budget, escalation) into the plane as a
                                PENDING proposal the operator ratifies in the console.
  clankerbar dead-rate          Scan the iteration logs and report, per day, per
                                phase and per harness, how many sessions ran and
                                how many died producing nothing (the dead-phase
                                rate).
  clankerbar version            Print the version.
  clankerbar help               Show this help.

Run 'clankerbar supervise --help', 'clankerbar run --help', 'clankerbar ctl
--help', 'clankerbar doctor --help' or 'clankerbar propose-config --help' for
their flags.
`)
}
