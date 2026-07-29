package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
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
		cfgPath = fs.String("config", "", "config file (default: ./clankerbar.json, then ~/.config/clankerbar/config.json)")
		// Flag help lists exactly the registered adapters (derived from the harness
		// registry, the same source config validation checks) so it can't drift.
		harnessName  = fs.String("harness", "", "coding-agent harness to drive: "+strings.Join(harness.Names(), " | "))
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

	// The driver reads backlog state directly (cheap, no tokens) to gate each
	// iteration, to keep polling while idle, and to honour the console pause flag —
	// one GET per project of the plane's backlog-summary surface (counts +
	// loopPaused in one read). The API key comes from the env: the operator's
	// ACCOUNT key covers every configured project (CLA-142); a project-scoped key
	// still works for single-project / CI setups.
	apiKey := os.Getenv("CLANKERBAR_API_KEY")

	if projects := cfg.Projects; len(projects) > 0 {
		targets := make([]loop.Target, 0, len(projects))
		for _, p := range projects {
			targets = append(targets, loop.Target{
				Name:          p.Slug,
				Poller:        backlog.New(cfg.ProjectSummaryURL(p), apiKey),
				WorkDir:       p.WorkDir,
				MCPConfigPath: p.MCPConfigPath,
			})
		}
		return loop.NewMulti(cfg, adapter, targets).Run(ctx)
	}

	return loop.New(cfg, adapter, backlog.New(cfg.BacklogSummaryURL(), apiKey)).Run(ctx)
}
