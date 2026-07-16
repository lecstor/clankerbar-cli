package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/loop"
)

// Run parses the `run` flags, resolves config (flags override the file, which
// overrides defaults), selects the harness adapter, and drives the loop.
func Run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var (
		cfgPath      = fs.String("config", "", "config file (default: ./clankerbar.json, then ~/.config/clankerbar/config.json)")
		harnessName  = fs.String("harness", "", "coding-agent harness to drive: claude | codex")
		model        = fs.String("model", "", "model to pin (harness-specific alias, e.g. opus)")
		workdir      = fs.String("workdir", "", "directory to run the harness in (default: current dir)")
		maxIter      = fs.Int("max-iterations", 0, "stop after N iterations (0 = until the backlog is dry or stopped)")
		pollInterval = fs.Duration("poll-interval", 0, "while paused on a usage limit, re-probe this often to catch an early reset")
	)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: clankerbar run [flags]\n\nFlags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // --help already printed usage
		}
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.ApplyFlagOverrides(config.Overrides{
		Harness:       *harnessName,
		Model:         *model,
		WorkDir:       *workdir,
		MaxIterations: *maxIter,
		PollInterval:  time.Duration(*pollInterval),
	})
	if err := cfg.Validate(); err != nil {
		return err
	}

	adapter, err := harness.Get(cfg.Harness)
	if err != nil {
		return err
	}

	return loop.New(cfg, adapter).Run(ctx)
}
