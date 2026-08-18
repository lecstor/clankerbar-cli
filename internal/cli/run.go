package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/loop"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// runFlags holds the parsed `run` options. It exists so the parse step is
// testable on its own: Run's very next act is to load config and start the
// loop, which a flag test has no business doing.
type runFlags struct {
	cfgPath      string
	harness      string
	model        string
	workdir      string
	configDir    string
	maxIter      int
	pollInterval time.Duration
	idlePoll     time.Duration
}

// newRunFlagSet registers the `run` flags onto a pflag set bound to f.
//
// pflag, not the stdlib `flag` package, because the CLI follows GNU convention:
// `--word` for long options, `-x` for short. Go's stdlib parses Plan 9 style
// (a single dash for both), and the language a tool happens to be written in
// should not leak into its UX. One consequence is deliberate and breaking: a
// legacy `-harness claude` is no longer accepted, because pflag reads a single
// dash as a bundle of SHORT flags (`-h -a -r ...`). See flags_test.go.
func newRunFlagSet(f *runFlags) *pflag.FlagSet {
	fs := newFlagSet("run")
	fs.StringVarP(&f.cfgPath, "config", "c", "", "config file (default: ~/.config/clankerbar/config.json; a ./clankerbar.json is never auto-loaded - name it here)")
	// Flag help lists exactly the registered adapters (derived from the harness
	// registry, the same source config validation checks) so it can't drift.
	fs.StringVar(&f.harness, "harness", "", "coding-agent harness to drive: "+strings.Join(harness.Names(), " | "))
	fs.StringVar(&f.model, "model", "", "model to pin (harness-specific alias, e.g. opus)")
	fs.StringVar(&f.workdir, "workdir", "", "directory to run the harness in (default: current dir)")
	fs.StringVar(&f.configDir, "config-dir", "", "harness config dir (CLAUDE_CONFIG_DIR / CODEX_HOME) — for skill/plugin/auth parity")
	fs.IntVar(&f.maxIter, "max-iterations", 0, "stop after N drain iterations (0 = run as a daemon until stopped)")
	fs.DurationVar(&f.pollInterval, "poll-interval", 0, "while paused on a usage limit, re-probe this often to catch an early reset")
	fs.DurationVar(&f.idlePoll, "idle-poll-interval", 0, "when the backlog has no claimable work, re-check this often (stay running)")
	return fs
}

// Run parses the `run` flags, resolves config (flags override the file, which
// overrides defaults), selects the harness adapter, and drives the loop.
func Run(ctx context.Context, args []string) error {
	var f runFlags
	fs := newRunFlagSet(&f)
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil // --help already printed usage
		}
		return err
	}

	cfg, err := config.Load(f.cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.ApplyFlagOverrides(config.Overrides{
		Harness:          f.harness,
		Model:            f.model,
		WorkDir:          f.workdir,
		ConfigDir:        f.configDir,
		MaxIterations:    f.maxIter,
		PollInterval:     f.pollInterval,
		IdlePollInterval: f.idlePoll,
	})
	if err := cfg.Validate(); err != nil {
		return err
	}

	adapter, err := harness.Get(cfg.Harness)
	if err != nil {
		return err
	}

	// A suspended machine freezes the loop's timers, so an unattended run has to
	// hold the machine up itself — see awake.go.
	defer keepAwake(ctx)()

	// The driver reads backlog state directly (cheap, no tokens) to gate each
	// iteration, to keep polling while idle, and to honour the console pause flag —
	// one GET per project of the plane's backlog-summary surface (counts +
	// loopPaused in one read). The API key comes from the env: the operator's
	// ACCOUNT key covers every configured project (CLA-142); a project-scoped key
	// still works for single-project / CI setups.
	apiKey := os.Getenv("CLANKERBAR_API_KEY")

	// Say where the key is going, once, before the first request carries it
	// (CLA-257). A redirected credential used to be invisible: the origin was
	// derived from a file in the workdir and never printed, so the only trace of it
	// was in the traffic. In the first ten lines of an overnight log it is a fact
	// the operator can check at a glance.
	log.Print(credentialNotice(cfg, apiKey))

	// The same key also authorises the driver's one WRITE: handing back a task a
	// session was still holding when it ended, so an interrupted iteration does not
	// leave a lease to expire and charge the task a reclaim (CLA-242).
	if projects := cfg.Projects; len(projects) > 0 {
		targets := make([]loop.Target, 0, len(projects))
		for _, p := range projects {
			targets = append(targets, loop.Target{
				Name:          p.Slug,
				Poller:        backlog.New(cfg.ProjectSummaryURL(p), apiKey),
				Releaser:      plane.New(cfg.ProjectEndpoint(p), apiKey),
				WorkDir:       p.WorkDir,
				MCPConfigPath: p.MCPConfigPath,
				// The per-harness files for this project, for a sequence whose
				// phases are not all on one harness (CLA-366). Nil for every
				// single-harness config, which is what leaves the resolution above
				// exactly as it was.
				MCPConfigPaths: p.MCPConfigPaths,
			})
		}
		return loop.NewMulti(cfg, adapter, targets).Run(ctx)
	}

	// One unnamed target — NewMulti with a single entry is exactly what New builds,
	// and it is the only form that can carry a Releaser.
	return loop.NewMulti(cfg, adapter, []loop.Target{{
		Poller:   backlog.New(cfg.BacklogSummaryURL(), apiKey),
		Releaser: plane.New(cfg.BacklogEndpoint(), apiKey),
	}}).Run(ctx)
}

// credentialNotice is the startup line naming where CLANKERBAR_API_KEY will be
// sent. Only the ORIGIN is named — never the key, and never the full path, which
// would put a project slug into a log an operator may paste.
func credentialNotice(cfg *config.Config, apiKey string) string {
	if apiKey == "" {
		return "CLANKERBAR_API_KEY is unset — no credential will be sent (the driver polls blind)"
	}
	origin := cfg.CredentialOrigin()
	if origin == "" {
		return "no plane origin is configured — no credential will be sent (the driver polls blind)"
	}
	return "sending the API key to " + origin
}
