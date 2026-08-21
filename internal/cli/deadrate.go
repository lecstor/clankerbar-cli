package cli

// deadrate.go — the retrospective dead-phase scan (CLA-402).
//
// `clankerbar dead-rate` walks the loop's iteration logs and reports, per day,
// per phase and per harness, how many sessions ran and how many of those died
// producing nothing — the number that decides whether a fix to the opencode
// gateway worked, previously reconstructed by hand (CLA-396). The
// classification is the driver's own dead-phase predicate plus the operator's
// exclusion (a session that never got past its claim counts toward neither
// counter), applied to the logs, so the historical rate and the live rate
// measure the same thing.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/deadscan"
)

// deadRateFlags holds the parsed `dead-rate` options.
type deadRateFlags struct {
	root  string
	error string
}

func newDeadRateFlagSet(f *deadRateFlags) *pflag.FlagSet {
	fs := newFlagSet("dead-rate")
	fs.StringVar(&f.root, "root", "", "loop state root (the directory holding per-workdir iteration-log dirs; default: the resolved state root)")
	fs.StringVar(&f.error, "error", "", "list only the logs whose APIError events contain this text (known-positive control for the scan)")
	return fs
}

// DeadRate scans the iteration logs under the loop state root and prints the
// per-day/phase/harness dead-phase table. With --error, it prints the logs
// carrying a matching APIError event instead — the tool_count_limit control.
func DeadRate(ctx context.Context, args []string) error {
	var f deadRateFlags
	fs := newDeadRateFlagSet(&f)
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}

	root := f.root
	if root == "" {
		r, err := config.StateRoot()
		if err != nil {
			return err
		}
		root = r
	}

	logs, err := deadscan.Scan(root)
	if err != nil {
		return err
	}

	if f.error != "" {
		printErrorMatches(os.Stdout, logs, f.error)
		return nil
	}

	cells := deadscan.Summarize(logs)
	printTable(os.Stdout, cells)
	return nil
}

func printTable(w io.Writer, cells []deadscan.Cell) {
	fmt.Fprintf(w, "%-12s %-12s %-10s %5s %5s %7s\n", "day", "phase", "harness", "run", "dead", "rate")
	var totRun, totDead int
	for _, c := range cells {
		fmt.Fprintf(w, "%-12s %-12s %-10s %5d %5d %6.1f%%\n",
			c.Day, c.Phase, c.Harness, c.Run, c.Dead, c.Rate())
		totRun += c.Run
		totDead += c.Dead
	}
	rate := 0.0
	if totRun > 0 {
		rate = 100 * float64(totDead) / float64(totRun)
	}
	fmt.Fprintf(w, "%-12s %-12s %-10s %5d %5d %6.1f%%\n", "total", "", "", totRun, totDead, rate)
}

func printErrorMatches(w io.Writer, logs []deadscan.Log, text string) {
	matches := deadscan.FindErrors(logs, text)
	if len(matches) == 0 {
		fmt.Fprintf(w, "no iteration log carries an APIError event containing %q\n", text)
		return
	}
	fmt.Fprintf(w, "%d log(s) carry an APIError event containing %q:\n", len(matches), text)
	for _, l := range matches {
		fmt.Fprintf(w, "  %s  %s  %s/%s\n", l.Day, filepath.Base(l.Path), l.Phase, l.Harness)
	}
}
