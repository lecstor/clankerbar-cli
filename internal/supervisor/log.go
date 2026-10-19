package supervisor

// The supervisor's own log file (CLA-701).
//
// A supervisor is the process an operator asks "why did that child not come
// back?" about — and every answer it gave ("refusing to start", "cannot spawn",
// "exited unexpectedly, retrying with backoff", the fleet status listing) went
// to a terminal and nowhere else. It now lands in `supervise.log` in the
// supervisor's own state dir (CacheDir), rotated at a size bound, on the same
// terms as each daemon's `daemon.log`.

import (
	"fmt"

	"github.com/lecstor/clankerbar-cli/internal/logfile"
)

// SuperviseLogName is the supervisor's own log file inside CacheDir.
const SuperviseLogName = "supervise.log"

// installLog points the standard logger at stderr plus the supervisor log. It
// is only reached when Options.LogToFile is set, so a library caller never has
// the process-wide logger taken over without asking.
func installLog(dir string) (func(), error) {
	restore, err := logfile.Install(dir, SuperviseLogName)
	if err != nil {
		return nil, fmt.Errorf("supervise log: %w", err)
	}
	return restore, nil
}
