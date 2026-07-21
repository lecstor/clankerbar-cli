package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
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
		configDir    = fs.String("config-dir", "", "harness config dir (CLAUDE_CONFIG_DIR / CODEX_HOME) — for skill/plugin/auth parity")
		maxIter      = fs.Int("max-iterations", 0, "stop after N drain iterations (0 = run as a daemon until stopped)")
		pollInterval = fs.Duration("poll-interval", 0, "while paused on a usage limit, re-probe this often to catch an early reset")
		idlePoll     = fs.Duration("idle-poll-interval", 0, "when the backlog has no claimable work, re-check this often (stay running)")
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
		Harness:          *harnessName,
		Model:            *model,
		WorkDir:          *workdir,
		ConfigDir:        *configDir,
		MaxIterations:    *maxIter,
		PollInterval:     time.Duration(*pollInterval),
		IdlePollInterval: time.Duration(*idlePoll),
	})
	if err := cfg.Validate(); err != nil {
		return err
	}

	adapter, err := harness.Get(cfg.Harness)
	if err != nil {
		return err
	}

	// The driver reads backlog counts directly (cheap, no tokens) to gate each
	// iteration and to keep polling while idle. It hits the same project-scoped MCP
	// endpoint the harness uses (resolved from .mcp.json); key is from the env.
	poller := backlog.New(cfg.BacklogEndpoint(), os.Getenv("CLANKERBAR_API_KEY"))

	return loop.New(cfg, adapter, poller).Run(ctx)
}
