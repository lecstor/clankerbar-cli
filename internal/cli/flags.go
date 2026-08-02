package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/pflag"
)

// parseFlags parses args with fs, first rejecting the pre-pflag spelling of a
// long option. Every subcommand goes through here so they cannot drift.
func parseFlags(fs *pflag.FlagSet, args []string) error {
	if err := rejectSingleDashLongFlags(fs, args); err != nil {
		fs.Usage()
		return err
	}
	return fs.Parse(args)
}

// rejectSingleDashLongFlags turns a legacy `-harness claude` into a clear error
// naming the GNU spelling.
//
// This is NOT a compatibility shim: the old form stays rejected, deliberately
// (CLA-140, pre-release). It exists because leaving it to pflag produces two
// genuinely bad outcomes rather than one clear one:
//
//   - `-harness` starts with 'h'. pflag reads a single dash as a bundle of
//     SHORT flags, finds no `-h` registered, and treats it as a request for
//     help: usage is printed and Parse returns ErrHelp, so the command exits 0
//     having done nothing. In `doctor && run` that reads as success.
//   - `-config /path` starts with 'c', which IS registered (the short alias for
//     --config). pflag consumes it as `-c` with the value "onfig" and leaves
//     /path as a positional argument. No error at all - the run proceeds
//     against a config file named "onfig".
//
// Silently doing the wrong thing is worse than the break itself. So we name the
// break instead of letting pflag misread it.
func rejectSingleDashLongFlags(fs *pflag.FlagSet, args []string) error {
	for _, arg := range args {
		// Everything after a bare `--` is positional by GNU convention.
		if arg == "--" {
			return nil
		}
		if len(arg) < 3 || !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
			continue
		}
		name, _, _ := strings.Cut(arg[1:], "=")
		if fs.Lookup(name) == nil {
			continue
		}
		return fmt.Errorf("unknown flag %q: this CLI uses GNU-style long options, so write --%s (a single dash introduces short flags)", arg, name)
	}
	return nil
}
