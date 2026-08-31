package cli

// The fleet supervisor (CLA-525, phases 1-3 of docs/proposals/daemon-supervisor.md
// in the clankerbar repo): bare `clankerbar` — and the explicit
// `clankerbar supervise` alias — reconciles the machine against the plane's
// account-scoped roster: every declared instance with `desired: running` runs
// as a supervised child, every one with `desired: stopped` gets its STOP
// marker and drains at its iteration boundary, and flipping desired state in
// the console lands on the machine within one poll. Since phase 2b each child
// runs on a config the supervisor GENERATES into the child's state dir
// (`materialized.json`) from the operator's local config and the roster entry
// — never on a hand-maintained per-daemon file.
//
// The irreducible local input is the proposal's whole machine layer: the
// ACCOUNT key in CLANKERBAR_API_KEY (a supervisor spans projects) and, when
// derivation is wanted, one workdir root.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/supervisor"
)

// WorkdirRootEnv is the environment variable naming the machine-stated root
// the supervisor derives each child's workdir from (daemon-supervisor phase
// 2a): <root>/<repo name> for the repo the instance's project names, taken
// from the roster entry. It is an environment variable because the
// supervisor's launch story is env-shaped (launchd units, the account key
// beside it) and because the derived value is a machine fact, never something
// a config file on the plane may set. Empty = derivation is off and children
// run on the base config's own workdirs.
const WorkdirRootEnv = "CLANKERBAR_WORKDIR_ROOT"

// Supervise runs the fleet supervisor. args carries the flags of the
// invocation: the bare `clankerbar` form passes none, `clankerbar supervise`
// passes whatever followed the subcommand. Phase 3 takes no flags beyond the
// shared help. `clankerbar supervise roll` is the phase-5b one-shot fleet
// roll, routed here before the flag parse because it is the one positional
// form this command takes.
func Supervise(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "roll" {
		return Roll(ctx, args[1:])
	}
	fs := newFlagSet("supervise")
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil // --help already printed usage
		}
		return err
	}

	// Children are spawned with THIS binary, resolved the same way a restart
	// re-exec resolves itself (launchBinary): an operator who launches through
	// a symlink on PATH keeps getting the symlink, so installing a new build
	// and restarting the supervisor runs children on the new build.
	bin, err := launchBinary(os.Args)
	if err != nil {
		return fmt.Errorf("supervise: %w", err)
	}

	// The machine layer every instance is built from: the operator's local
	// config (env, settings_path, config_dir, mcp_config_path, backlog_url and
	// the rest of what can never come from the plane), or bare defaults when
	// none exists. The roster entry names the projects and the one permitted
	// override; everything a daemon needs that the plane may not set comes
	// from here.
	base, err := config.Load("")
	if err != nil {
		return fmt.Errorf("supervise: %w", err)
	}

	// The account key is the whole credential: the roster is account-scoped
	// (Decision 3) and the children inherit the same key for their own plane
	// calls. A supervisor without it has no desired state to reconcile
	// against, which is a wiring error to say loudly, not a degraded mode to
	// invent.
	key := os.Getenv("CLANKERBAR_API_KEY")
	if key == "" {
		return errors.New("supervise: CLANKERBAR_API_KEY is required - the supervisor reconciles against the account-scoped roster, and the account key is the irreducible credential (a supervisor spans projects, so a project key will not do)")
	}

	o := supervisor.Options{
		Binary:    bin,
		RosterURL: strings.TrimRight(base.BacklogURL, "/") + "/api/daemon-roster",
		APIKey:    key,
		BaseCfg:   base,
	}
	if root := os.Getenv(WorkdirRootEnv); root != "" {
		o.WorkdirRoot = root
		log.Printf("deriving children's workdirs from %s (set by %s) - an instance whose derived workdir is missing or is not a checkout of its project's repo will not be started", root, WorkdirRootEnv)
	}
	return supervisor.Supervise(ctx, o)
}

// Roll runs the phase-5b one-shot fleet roll: every local running child is
// restarted at its iteration boundary and verified — via its next local
// beacon — to be reporting the version THIS binary runs, one child at a
// time; a child that fails to come back on it halts the roll. The operator
// installs the new build at the fleet's launch path and runs
// `clankerbar supervise roll` from it; the roll refuses to touch any child
// when the launch path still carries another version.
//
// The wiring is the supervisor's own: the same account key, the same roster
// endpoint, the same launch-path resolution — a roll over the same fleet the
// supervisor serves, never over process listings.
func Roll(ctx context.Context, args []string) error {
	fs := newFlagSet("supervise roll")
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil // --help already printed usage
		}
		return err
	}

	// Children re-exec (RESTART) and respawn from the launch path, so that is
	// the file the new build must replace — and the roll's pre-flight probes
	// exactly that path.
	bin, err := launchBinary(os.Args)
	if err != nil {
		return fmt.Errorf("supervise roll: %w", err)
	}

	base, err := config.Load("")
	if err != nil {
		return fmt.Errorf("supervise roll: %w", err)
	}

	// Same irreducible credential as the supervisor: the roll reads the
	// account-scoped roster, so the account key is the whole wiring.
	key := os.Getenv("CLANKERBAR_API_KEY")
	if key == "" {
		return errors.New("supervise roll: CLANKERBAR_API_KEY is required - the roll reads the same account-scoped roster the supervisor reconciles against")
	}

	o := supervisor.Options{
		Binary:    bin,
		RosterURL: strings.TrimRight(base.BacklogURL, "/") + "/api/daemon-roster",
		APIKey:    key,
		BaseCfg:   base,
	}
	return supervisor.Roll(ctx, o)
}
