package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/pflag"
)

// registerHelp adds `-h` / `--help` to a flag set. Every subcommand must call
// it, and not because the help text is nice to have.
//
// pflag reads a lone dash as a bundle of SHORT flags. If no `-h` is registered
// it treats *any* bundle starting with 'h' as a request for help: it prints
// usage and returns ErrHelp, which this package maps to "handled, exit 0". So
// an unregistered `-h` turns a typo like `clankerbar doctor -harnes claude`
// into a silent success that ran no preflight checks at all - and `doctor &&
// run` reads that as a green light. Go's stdlib `flag` errored on it.
//
// Registering the shorthand closes that: 'h' now consumes cleanly and the
// parser moves on to 'a', which is not a flag, so the command fails loudly.
func registerHelp(fs *pflag.FlagSet) {
	fs.BoolP("help", "h", false, "show this help and exit")
}

// helpRequested reports whether --help / -h was passed.
func helpRequested(fs *pflag.FlagSet) bool {
	f := fs.Lookup("help")
	return f != nil && f.Value.String() == "true"
}

// parseFlags parses args with fs and enforces the rules every subcommand
// shares. It returns pflag.ErrHelp when help was asked for, so callers can
// treat that as "handled" rather than as a failure.
//
// Usage is printed on every rejection. pflag under ContinueOnError prints
// nothing at all on a parse error (unlike stdlib `flag`, which printed the
// message and the flag list), so without this an unknown flag would produce one
// bare line and no hint about what the valid flags are.
func parseFlags(fs *pflag.FlagSet, args []string) error {
	if err := rejectSingleDashLongFlags(fs, args); err != nil {
		printUsage(fs)
		return err
	}
	if err := fs.Parse(args); err != nil {
		printUsage(fs)
		return err
	}
	if helpRequested(fs) {
		printUsage(fs)
		return pflag.ErrHelp
	}
	// No subcommand takes positional arguments, so a leftover is always a
	// mistake - and silently ignoring it is how "clankerbar run -config x.json"
	// would appear to work while reading a different config entirely.
	if fs.NArg() > 0 {
		printUsage(fs)
		return fmt.Errorf("unexpected argument(s): %s", strings.Join(fs.Args(), " "))
	}
	return nil
}

func printUsage(fs *pflag.FlagSet) {
	if fs.Usage != nil {
		fs.Usage()
	}
}

// rejectSingleDashLongFlags turns the pre-pflag spelling of a long option -
// `-harness claude` - into an error that names the GNU spelling.
//
// This is NOT a compatibility shim: the old form stays rejected, deliberately
// (CLA-140, pre-release). With registerHelp in place pflag would reject these
// on its own, so what this buys is only the message: "write --harness" instead
// of "unknown shorthand flag: 'a' in -harness", which does not obviously point
// at the double-dash fix. One case it does still catch outright is `-config`,
// which pflag would otherwise consume as `-c` with the value "onfig".
//
// It fires only on an exact registered long name, so it is a courtesy on top of
// the parser, never the thing standing between a typo and a wrong run.
func rejectSingleDashLongFlags(fs *pflag.FlagSet, args []string) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Everything after a bare `--` is positional by GNU convention.
		if arg == "--" {
			return nil
		}

		switch {
		case strings.HasPrefix(arg, "--"):
			// A long flag's value may itself look like a flag (a directory
			// genuinely named "-model"), so skip over it rather than judging it.
			name, _, hasEq := strings.Cut(arg[2:], "=")
			if !hasEq && takesValue(fs.Lookup(name)) {
				i++
			}

		case strings.HasPrefix(arg, "-") && len(arg) > 1:
			name, _, hasEq := strings.Cut(arg[1:], "=")
			if len(arg) > 2 && fs.Lookup(name) != nil {
				return fmt.Errorf("unknown flag %q: this CLI uses GNU-style long options, so write --%s (a single dash introduces short flags)", arg, name)
			}
			if !hasEq && shortsConsumeNextArg(fs, arg[1:]) {
				i++
			}
		}
	}
	return nil
}

// takesValue reports whether f needs a separate argument for its value. A
// boolean-style flag carries NoOptDefVal and never consumes the next token.
func takesValue(f *pflag.Flag) bool {
	return f != nil && f.NoOptDefVal == ""
}

// shortsConsumeNextArg reports whether a short bundle ("abc" from "-abc") ends
// with a value-taking flag whose value must come from the following argument.
func shortsConsumeNextArg(fs *pflag.FlagSet, shorts string) bool {
	for i := 0; i < len(shorts); i++ {
		f := fs.ShorthandLookup(string(shorts[i]))
		if f == nil {
			return false // unknown short; pflag will produce the error
		}
		if !takesValue(f) {
			continue
		}
		// A value-taking short takes the rest of the bundle inline if there is
		// any, and otherwise the next argument.
		return i == len(shorts)-1
	}
	return false
}
