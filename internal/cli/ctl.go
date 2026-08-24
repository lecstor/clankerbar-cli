package cli

// `clankerbar ctl` — control a RUNNING daemon by writing markers into its state
// dir (CLA-461), the same channel STOP/HALT have always used. File-based keeps
// it consistent with those and works from any shell; the daemon picks each
// request up where it picks STOP up.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/loop"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
)

type ctlFlags struct {
	cfgPath string
	now     bool
}

func newCtlFlagSet(f *ctlFlags) *pflag.FlagSet {
	fs := newFlagSet("ctl")
	fs.StringVarP(&f.cfgPath, "config", "c", "", "the config file of the RUNNING daemon (default: ~/.config/clankerbar/config.json); its state_dir/workdir decides where the marker is written")
	fs.BoolVar(&f.now, "now", false, "restart only: kill the in-flight session first (existing process-group kill), release any held claim, then re-exec")
	return fs
}

// parseCtlArgs parses ctl's args. Unlike every other subcommand, ctl takes one
// POSITIONAL — the action — so parseFlags' no-positionals rule does not apply;
// this is that function with the rule replaced.
func parseCtlArgs(fs *pflag.FlagSet, args []string) (string, error) {
	if err := rejectSingleDashLongFlags(fs, args); err != nil {
		printUsage(fs)
		return "", err
	}
	if err := fs.Parse(args); err != nil {
		printUsage(fs)
		return "", err
	}
	if helpRequested(fs) {
		// Same convention as parseFlags: help SUCCEEDED, so stdout.
		if fs.Output() == os.Stderr {
			fs.SetOutput(os.Stdout)
			defer fs.SetOutput(os.Stderr)
		}
		printUsage(fs)
		return "", pflag.ErrHelp
	}
	if fs.NArg() != 1 {
		printUsage(fs)
		return "", fmt.Errorf("clankerbar ctl needs exactly one action: restart or reload")
	}
	action := fs.Arg(0)
	if action != "restart" && action != "reload" {
		printUsage(fs)
		return "", fmt.Errorf("unknown ctl action %q - want \"restart\" or \"reload\"", action)
	}
	return action, nil
}

// Ctl writes one control marker into the running daemon's state dir and prints
// what will happen next. It resolves the state dir EXACTLY as `run` does — same
// config file, same resolution — because a marker written anywhere else is a
// marker nothing reads, which is precisely the failure this command must not
// have: an operator who restarts nothing walks away believing they did.
//
// A missing state dir is refused rather than created: creating one would hand
// back success against a daemon that either is not running or was launched with
// a different config, and the surprise-restart-on-next-start failure class is
// what `doctor` warns about for exactly that shape.
func Ctl(_ context.Context, args []string) error {
	var f ctlFlags
	fs := newCtlFlagSet(&f)
	action, err := parseCtlArgs(fs, args)
	if err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil // --help already printed usage
		}
		return err
	}
	if f.now && action != "restart" {
		return fmt.Errorf("--now applies to restart only; reload has nothing to kill")
	}

	cfg, err := config.Load(f.cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// Validated, not just loaded: ResolveStateDir keys off workdir/state_dir,
	// and Validate is what pins workdir down absolutely — skipping it here could
	// resolve a DIFFERENT directory than the validated run resolved.
	if err := cfg.Validate(); err != nil {
		return err
	}
	stateDir, err := cfg.ResolveStateDir()
	if err != nil {
		return err
	}
	if _, err := os.Lstat(stateDir); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no clankerbar state dir at %s - is a loop running against this config? start one with `clankerbar run`, or point --config at the config it was started with", stateDir)
	}
	dir, err := statedir.Open(stateDir, cfg.SessionWorkDirs()...)
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck // read-side handle on the way out

	marker := MarkerRestartToName(action, f.now)
	body := []byte(fmt.Sprintf("requested by `clankerbar ctl %s` at %s\n",
		action, time.Now().UTC().Format(time.RFC3339)))
	if err := dir.WriteFile(marker, body); err != nil {
		if errors.Is(err, statedir.ErrExists) {
			fmt.Printf("%s already requested (%s exists) - one pending request is enough\n", action, marker)
			return nil
		}
		return fmt.Errorf("write %s into %s: %w", marker, stateDir, err)
	}

	fmt.Printf("wrote %s\n", filepath.Join(stateDir, marker))
	for _, line := range effectLines(action, f.now) {
		fmt.Println(line)
	}
	return nil
}

// MarkerRestartToName maps the parsed action to the loop's marker constant, so
// cli can never drift from the names loop reads.
func MarkerRestartToName(action string, now bool) string {
	if action == "reload" {
		return loop.MarkerReload
	}
	if now {
		return loop.MarkerRestartNow
	}
	return loop.MarkerRestart
}

// effectLines is what ctl tells the operator happens next, in the daemon log
// and to their session. The last line of the restart variants states the
// feature's known limit so it is said at the moment of use, not only in README.
func effectLines(action string, now bool) []string {
	const limit = "(re-exec preserves the daemon's CURRENT environment - it cannot conjure env the daemon was never launched with; per-session env ownership is CLA-462)"
	switch {
	case action == "restart" && now:
		return []string{
			"the running daemon kills its in-flight session within seconds, releases any claim it holds (recorded released, not failed), then re-executes",
			limit,
		}
	case action == "restart":
		return []string{
			"the running daemon lets its in-flight session finish, then re-executes at the next iteration boundary",
			limit,
		}
	default:
		return []string{
			"the running daemon re-reads its config file at the next iteration boundary - no process replacement; env and binary changes need restart",
		}
	}
}
