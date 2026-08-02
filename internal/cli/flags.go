package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"
)

// newFlagSet builds a subcommand's flag set with the two things parseFlags
// depends on already in place: `-h`/`--help` registered, and a Usage function.
//
// Both used to be hand-copied into each subcommand's constructor, which made
// them a convention rather than an invariant. A future subcommand that forgot
// registerHelp would silently reinstate the exit-0 bug described below, and one
// that forgot Usage would print nothing at all on a rejection - neither with a
// compile error or a failing test to show for it. Building the set here is what
// makes them structural.
//
// name is both the pflag set's name and the subcommand in the usage heading.
func newFlagSet(name string) *pflag.FlagSet {
	fs := pflag.NewFlagSet(name, pflag.ContinueOnError)
	// Explicit rather than inherited: rejections belong on stderr, and parseFlags
	// recognises this exact writer when it moves --help output to stdout.
	fs.SetOutput(os.Stderr)
	registerHelp(fs)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: clankerbar %s [flags]\n\nFlags:\n", name)
		fs.PrintDefaults()
	}
	return fs
}

// registerHelp adds `-h` / `--help` to a flag set. newFlagSet calls it for every
// subcommand, and not because the help text is nice to have.
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
	// Self-heal a set that did not come from newFlagSet. Everything below assumes
	// -h is registered: without it pflag reads any single-dash bundle beginning
	// with 'h' as a help request and the command exits 0 (see registerHelp).
	if fs.Lookup("help") == nil {
		registerHelp(fs)
	}
	if err := rejectSingleDashLongFlags(fs, args); err != nil {
		printUsage(fs)
		return err
	}
	if err := fs.Parse(args); err != nil {
		printUsage(fs)
		return err
	}
	if helpRequested(fs) {
		// --help is a request that SUCCEEDED, so its output belongs on stdout -
		// `clankerbar run --help | less` should show something. Rejections stay on
		// stderr. A set whose output was redirected elsewhere (tests) is left alone.
		if fs.Output() == os.Stderr {
			fs.SetOutput(os.Stdout)
			defer fs.SetOutput(os.Stderr)
		}
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
		return
	}
	// A set built without newFlagSet still gets the flag list, which is the part
	// a user who has just mistyped a flag actually needs. Silently printing
	// nothing is the one outcome this function exists to prevent.
	fs.PrintDefaults()
}

// rejectSingleDashLongFlags rejects every single-dash token that is not a
// genuine bundle of registered short flags.
//
// It has two jobs. The first is cosmetic: the pre-pflag spelling of a long
// option - `-harness claude` - is deliberately gone (CLA-140, pre-release, no
// shim), and with registerHelp in place pflag would reject it anyway, so what
// this buys is the message. "write --harness" points at the fix; pflag's
// "unknown shorthand flag: 'a' in -harness" does not.
//
// The second is not cosmetic. pflag lets a value-taking short flag take the
// rest of its token inline, so with `-c` registered EVERY single-dash token
// beginning with c parses clean and silently: `-cofnig ./x.json` becomes
// --config="ofnig", `-configg` becomes --config="onfigg". Go's stdlib flag
// rejected all of them. So the inline `-cVALUE` form is not accepted here: a
// short flag's value comes from the next argument (`-c ./x.json`) or after an
// `=` (`-c=./x.json`). That costs one GNU nicety and closes the whole class.
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
			if len(arg) > 2 {
				// An exact long name gets the message that names the fix.
				if fs.Lookup(name) != nil {
					return fmt.Errorf("unknown flag %q: this CLI uses GNU-style long options, so write --%s (a single dash introduces short flags)", arg, name)
				}
				if err := rejectShortBundle(fs, arg, name); err != nil {
					return err
				}
			}
			if !hasEq && shortsConsumeNextArg(fs, arg[1:]) {
				i++
			}
		}
	}
	return nil
}

// rejectShortBundle rejects a multi-letter single-dash token unless every
// letter in it is a registered short flag AND only the last of them takes a
// value. Anything else is a long option spelled with one dash, or a typo, and
// pflag would swallow the tail as an inline value rather than erroring.
func rejectShortBundle(fs *pflag.FlagSet, arg, bundle string) error {
	for i := 0; i < len(bundle); i++ {
		c := string(bundle[i])
		f := fs.ShorthandLookup(c)
		if f == nil {
			return fmt.Errorf("unknown flag %q: a single dash introduces short flags (%s), so every letter after it must be one - %q is not; long options take two dashes", arg, shorthandList(fs), c)
		}
		if takesValue(f) && i < len(bundle)-1 {
			return fmt.Errorf("ambiguous flag %q: the short flag -%s takes a value, so this would be read as --%s=%q; write -%s <value> or --%s=<value>", arg, c, f.Name, bundle[i+1:], c, f.Name)
		}
	}
	return nil
}

// shorthandList renders the registered short flags, so a rejection can name
// what a single dash CAN mean here rather than only what it cannot.
func shorthandList(fs *pflag.FlagSet) string {
	var shorts []string
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Shorthand != "" {
			shorts = append(shorts, "-"+f.Shorthand)
		}
	})
	return strings.Join(shorts, ", ")
}

// takesValue reports whether f needs a separate argument for its value. A
// boolean-style flag carries NoOptDefVal and never consumes the next token.
func takesValue(f *pflag.Flag) bool {
	return f != nil && f.NoOptDefVal == ""
}

// shortsConsumeNextArg reports whether a short bundle ("abc" from "-abc") ends
// with a value-taking flag whose value must come from the following argument.
// rejectShortBundle has already ruled out the inline case for bundles longer
// than one letter, so a value-taking short here is always the last one.
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
