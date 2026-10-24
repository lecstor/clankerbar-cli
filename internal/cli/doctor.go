package cli

// doctor.go — preflight diagnostics for unattended runs.
//
// Every failure mode this checks for is one that otherwise surfaces only as
// DEGRADED BEHAVIOUR, hours in: a rejected key drops the loop into blind mode, a
// missing binary kills the first session, an unreachable endpoint is
// indistinguishable from an empty queue. The point of `doctor` is to convert all
// of those into one cheap, loud answer before an overnight run starts.
//
// The checks are deliberately split into small functions over an injectable
// doctorEnv: PATH lookups, version execs and the backlog read are the three
// things a test cannot do for real, and they are the three things most worth
// testing.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// status is a check's verdict. The ordering matters only for reporting.
type status int

const (
	pass status = iota
	warn
	fail
)

func (s status) String() string {
	switch s {
	case pass:
		return "PASS"
	case warn:
		return "WARN"
	default:
		return "FAIL"
	}
}

// check is one diagnostic result. remedy is printed only for WARN/FAIL — a
// remedy line under a PASS is noise the operator learns to skim past, which is
// how the real ones get missed.
type check struct {
	name   string
	status status
	detail string
	remedy string
	info   []string // extra indented lines (the config check's resolved values)
}

// binVersionTimeout bounds the harness version probe. Generous enough for a cold
// Node/Bun start, short enough that a wedged shim still reports before a cron
// window is wasted.
const binVersionTimeout = 10 * time.Second

// doctorEnv is the seam between the checks and the world. Defaults hit the real
// PATH, the real binary and the real plane; tests substitute fakes.
type doctorEnv struct {
	lookPath   func(file string) (string, error)
	binVersion func(ctx context.Context, bin string) (string, error)
	newPoller  func(summaryURL, apiKey string) backlog.Poller
	apiKey     string
	goos       string
	pmset      func(ctx context.Context, args ...string) (string, error)

	// The deploy_lag check's three seams (CLA-322): the /health read,
	// repository discovery under a workdir, and every git exec it runs.
	fetchHealth func(ctx context.Context, healthURL string) (deployHealth, error)
	repos       func(ctx context.Context, workdir string) []string
	gitRun      func(ctx context.Context, dir string, args ...string) (string, error)

	// The session-env check's two seams (CLA-462): running one declared
	// fromCommand, and re-checking one "@path" file's owner-only rule.
	runEnvCmd func(ctx context.Context, command string) (string, error)
	envPath   func(path string) error

	// The fleet check's one seam (CLA-466): the harmless empty-body probe of the
	// report endpoint. Production is fleet.Probe; tests substitute statuses.
	fleetProbe func(ctx context.Context, endpoint, apiKey string) error

	// The run-config check's seam (CLA-410): building the read/propose client
	// for one project's MCP endpoint. Production is plane.NewRunConfigAPI; a
	// field so tests serve stored documents from an httptest plane.
	newRCfgAPI func(mcpURL, apiKey string) plane.RunConfigAPI

	// The opencode auth check's seam (CLA-675): count the rows of opencode
	// 2.x's credential store, per provider, without reading any secret.
	// Production runs sqlite3 read-only; tests substitute counts.
	opencodeCredentials func(ctx context.Context, dbPath string) (map[string]int, error)

	// The pr_gate probe's seam (CLA-776): run a REAL `gh pr view --json` against
	// a pull request found in the workdirs the delivery gate judges, and parse
	// it with the gate's own decoder. Production is delivery.ProbeGHJSONDirs;
	// tests substitute every outcome so doctor's classification is pinned
	// without a network or a gh.
	probePRGate func(ctx context.Context, ghBin string, workdirs []string) (delivery.PRProbe, error)
}

func defaultDoctorEnv() doctorEnv {
	return doctorEnv{
		lookPath: exec.LookPath,
		binVersion: func(ctx context.Context, bin string) (string, error) {
			// Bounded, because the shim this is written to catch typically BLOCKS
			// rather than erroring — a wrapper waiting on a TTY, say. doctor is
			// documented as a cron gate (`doctor && run`), where there is nobody to
			// Ctrl-C it, so an unbounded exec turns a FAIL into a hang.
			ctx, cancel := context.WithTimeout(ctx, binVersionTimeout)
			defer cancel()
			out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
			if err != nil {
				return "", err
			}
			return firstLine(string(out)), nil
		},
		newPoller: backlog.New,
		apiKey:    os.Getenv("CLANKERBAR_API_KEY"),
		goos:      runtime.GOOS,
		pmset: func(ctx context.Context, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, binVersionTimeout)
			defer cancel()
			out, err := exec.CommandContext(ctx, "pmset", args...).Output()
			return string(out), err
		},
		fetchHealth: fetchDeployHealth,
		repos:       delivery.Repos,
		gitRun:      deployGitRun,
		runEnvCmd: func(ctx context.Context, command string) (string, error) {
			return config.RunEnvCommand(command)
		},
		envPath:             config.VerifyEnvFilePath,
		fleetProbe:          fleet.Probe,
		newRCfgAPI:          plane.NewRunConfigAPI,
		opencodeCredentials: sqliteOpencodeCredentials,
		probePRGate:         delivery.ProbeGHJSONDirs,
	}
}

// doctorFlags holds the parsed `doctor` options, split out from Doctor for the
// same reason runFlags is: so the parse step can be tested without running the
// preflight checks behind it.
type doctorFlags struct {
	cfgPath   string
	harness   string
	workdir   string
	configDir string
}

// newDoctorFlagSet registers the `doctor` flags onto a pflag set bound to f.
// GNU-style long options, matching `run` - see newRunFlagSet for why, and for
// the deliberate break this carries with it.
func newDoctorFlagSet(f *doctorFlags) *pflag.FlagSet {
	fs := newFlagSet("doctor")
	fs.StringVarP(&f.cfgPath, "config", "c", "", "config file (default: ~/.config/clankerbar/config.json; a ./clankerbar.json is never auto-loaded - name it here)")
	fs.StringVar(&f.harness, "harness", "", "harness to check: "+strings.Join(harness.Names(), " | "))
	fs.StringVar(&f.workdir, "workdir", "", "directory the harness would run in (default: current dir)")
	fs.StringVar(&f.configDir, "config-dir", "", "harness config dir (CLAUDE_CONFIG_DIR / CODEX_HOME)")
	return fs
}

// Doctor parses the `doctor` flags and runs the preflight checks. It exits
// non-zero (via a returned error) when any check FAILs, so it is usable as a
// gate in a cron wrapper: `clankerbar doctor && clankerbar run`.
func Doctor(ctx context.Context, args []string) error {
	var f doctorFlags
	fs := newDoctorFlagSet(&f)
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil // --help already printed usage
		}
		return err
	}

	return doctorRun(ctx, os.Stdout, f.cfgPath, config.Overrides{
		Harness:   f.harness,
		WorkDir:   f.workdir,
		ConfigDir: f.configDir,
	}, defaultDoctorEnv())
}

// doctorRun is Doctor with its IO and world injected, so tests drive the whole
// command end to end (including the exit-code contract) without a real PATH,
// plane or stdout.
func doctorRun(ctx context.Context, w io.Writer, cfgPath string, ov config.Overrides, e doctorEnv) error {
	// A config that will not load or validate makes every later check meaningless
	// — they would all be answering questions about a config the loop would never
	// accept. Report it as the config check and stop there.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		printCheck(w, check{
			name:   "config",
			status: fail,
			detail: err.Error(),
			remedy: "fix the config file, or name the one you mean with --config (nothing outside ~/.config/clankerbar/config.json is loaded implicitly)",
		})
		return doctorFailed(1)
	}
	cfg.ApplyFlagOverrides(ov)
	if err := cfg.Validate(); err != nil {
		printCheck(w, check{
			name:   "config",
			status: fail,
			detail: err.Error(),
			remedy: "fix the config, or override it with the doctor flags (--harness, --workdir, --config-dir)",
		})
		return doctorFailed(1)
	}

	failed := 0
	for _, c := range doctorChecks(ctx, cfg, e) {
		printCheck(w, c)
		if c.status == fail {
			failed++
		}
	}
	if failed > 0 {
		return doctorFailed(failed)
	}
	return nil
}

// doctorChecks runs every check, in the order an operator would want to read
// them: what am I configured as, can I run the agent, can I reach the backlog,
// can I write my state, and what will I be allowed to do.
func doctorChecks(ctx context.Context, cfg *config.Config, e doctorEnv) []check {
	checks := []check{checkConfig(cfg)}
	checks = append(checks, checkHarnesses(ctx, cfg, e)...)
	checks = append(checks, checkConfigDirs(cfg)...)
	checks = append(checks, checkRepos(cfg)...)
	checks = append(checks, checkSessionEnv(ctx, cfg, e)...)
	checks = append(checks, checkSessionEnvAllowlist(cfg)...)
	checks = append(checks, checkTokenSources(cfg)...)
	checks = append(checks, checkBacklog(ctx, cfg, e)...)
	checks = append(checks, checkFleets(ctx, cfg, e)...)
	checks = append(checks, checkInstanceIdentity(cfg))
	checks = append(checks, checkRunConfigs(ctx, cfg, e)...)
	checks = append(checks, checkDeployLags(ctx, cfg, e)...)
	checks = append(checks, checkStateDir(cfg))
	checks = append(checks, checkSessions(cfg)...)
	checks = append(checks, checkStranded(ctx, cfg, e))
	checks = append(checks, checkMCPServers(cfg), checkOpencodeAmbientConfigs(cfg))
	checks = append(checks, checkOpencodeAuth(ctx, cfg, e)...)
	checks = append(checks, checkOpencodeMCPDisables(cfg)...)
	checks = append(checks, checkPermissionsAll(cfg)...)
	return append(checks, checkToolchains(cfg), checkPower(ctx, e), checkBudget(cfg), checkPRGate(ctx, cfg, e))
}

func doctorFailed(n int) error {
	return fmt.Errorf("doctor: %d %s failed", n, plural(n, "check", "checks"))
}

// --- rendering ---------------------------------------------------------------

// checkLabelWidth is the column the check names are padded to. Wide enough for
// the qualified labels a mixed-harness run produces (`config_dir[opencode]`,
// `workdir[clankerbar:opencode]`), because doctor output is read at a glance and
// one over-long name used to push every following detail out of alignment.
const checkLabelWidth = 28

func printCheck(w io.Writer, c check) {
	fmt.Fprintf(w, "%-4s  %-*s %s\n", c.status, checkLabelWidth, c.name, c.detail)
	for _, line := range c.info {
		fmt.Fprintf(w, "                   %s\n", line)
	}
	if c.remedy != "" && c.status != pass {
		fmt.Fprintf(w, "                -> %s\n", c.remedy)
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none derivable)"
	}
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// truncate cuts to at most n bytes, backing off to a rune boundary so a
// multi-byte character straddling the cut does not render as mojibake.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
