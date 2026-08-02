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
                             drain the backlog, surviving usage limits.
  clankerbar doctor [flags]  Preflight the setup — config, harness, backlog
                             wiring, workdir, permissions. Exits non-zero on any
                             FAIL, so it gates a run: doctor && run.
  clankerbar version         Print the version.
  clankerbar help            Show this help.

Run 'clankerbar run --help' or 'clankerbar doctor --help' for their flags.
`)
}
