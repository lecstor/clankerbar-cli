package cli

// The daemon's own log file (CLA-701).
//
// The loop's own lines — which session it spawned, what the child exited with,
// how that exit was classified, why the run ended — used to go to stderr and
// nowhere else. Those are the lines that say what the driver DECIDED, and the
// only other copy was the operator's terminal: a few thousand rows of
// scrollback, shared with the session stream that has since been moved out of
// it (session output now goes to the iteration log alone unless
// `echo_sessions` asks for the old echo).
//
// So each `run` process writes them here as well: `daemon.log` in the daemon's
// own state dir, beside the markers and iteration logs, rotated at a size bound
// so a daemon that runs for weeks does not leave one enormous file behind.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/logfile"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

// daemonLogPath is where this config's daemon log lives — the state dir the
// loop itself opens, so the two can never disagree.
func daemonLogPath(cfg *config.Config) (string, error) {
	dir, err := cfg.ResolveStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, statedir.DaemonLogName), nil
}

// installDaemonLog points the standard logger at stderr plus the daemon log,
// and returns a func that restores the previous output and closes the file.
//
// Best-effort by contract: every caller treats an error as a warning and keeps
// running. Losing the log degrades observability, and a daemon that refuses to
// start because its log file could not be opened would turn that into an
// outage.
func installDaemonLog(cfg *config.Config) (restore func(), err error) {
	dir, err := cfg.ResolveStateDir()
	if err != nil {
		return nil, err
	}
	// Open the state dir exactly as the loop will, before writing into it: this
	// is the package that refuses a symlinked or foreign directory, and a log
	// file is not a reason to bypass those checks. The handle is released
	// immediately — the loop opens its own — and the path is used only after it
	// has passed them.
	sd, err := statedir.Open(dir, cfg.SessionWorkDirs()...)
	if err != nil {
		return nil, err
	}
	if err := sd.Close(); err != nil {
		return nil, err
	}
	return logfile.Install(dir, statedir.DaemonLogName)
}

// warnNoDaemonLog says the log could not be installed without making it a
// startup failure, naming the path it tried.
func warnNoDaemonLog(cfg *config.Config, err error) {
	path, perr := daemonLogPath(cfg)
	if perr != nil {
		path = "daemon.log"
	}
	fmt.Fprintf(os.Stderr, "clankerbar: warning: cannot write the daemon log %s (%v) - the daemon's own lines go to stderr only\n", path, err)
}
