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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
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

// doctorEnv is the seam between the checks and the world. Defaults hit the real
// PATH, the real binary and the real plane; tests substitute fakes.
type doctorEnv struct {
	lookPath   func(file string) (string, error)
	binVersion func(ctx context.Context, bin string) (string, error)
	newPoller  func(summaryURL, apiKey string) backlog.Poller
	apiKey     string
}

func defaultDoctorEnv() doctorEnv {
	return doctorEnv{
		lookPath: exec.LookPath,
		binVersion: func(ctx context.Context, bin string) (string, error) {
			out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
			if err != nil {
				return "", err
			}
			return firstLine(string(out)), nil
		},
		newPoller: backlog.New,
		apiKey:    os.Getenv("CLANKERBAR_API_KEY"),
	}
}

// Doctor parses the `doctor` flags and runs the preflight checks. It exits
// non-zero (via a returned error) when any check FAILs, so it is usable as a
// gate in a cron wrapper: `clankerbar doctor && clankerbar run`.
func Doctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	var (
		cfgPath     = fs.String("config", "", "config file (default: ./clankerbar.json, then ~/.config/clankerbar/config.json)")
		harnessName = fs.String("harness", "", "harness to check: "+strings.Join(harness.Names(), " | "))
		workdir     = fs.String("workdir", "", "directory the harness would run in (default: current dir)")
		configDir   = fs.String("config-dir", "", "harness config dir (CLAUDE_CONFIG_DIR / CODEX_HOME)")
	)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: clankerbar doctor [flags]\n\nFlags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // --help already printed usage
		}
		return err
	}

	return doctorRun(ctx, os.Stdout, *cfgPath, config.Overrides{
		Harness:   *harnessName,
		WorkDir:   *workdir,
		ConfigDir: *configDir,
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
			remedy: "fix the config file, or point --config at a different one",
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
	checks := []check{
		checkConfig(cfg),
		checkHarness(ctx, cfg, e),
		checkConfigDir(cfg),
	}
	checks = append(checks, checkBacklog(ctx, cfg, e)...)
	return append(checks, checkWorkdir(cfg), checkPermissions(cfg), checkBudget(cfg))
}

func doctorFailed(n int) error {
	return fmt.Errorf("doctor: %d %s failed", n, plural(n, "check", "checks"))
}

// --- 1. config ---------------------------------------------------------------

func checkConfig(cfg *config.Config) check {
	c := check{name: "config", status: pass}
	if src := cfg.Source(); src != "" {
		c.detail = "loaded " + src
	} else {
		// Flags-only is a legitimate way to run, not a missing-file problem — say
		// so plainly rather than implying something is absent.
		c.detail = "no config file — defaults plus flags"
	}

	workdir := cfg.WorkDir
	if workdir == "" {
		workdir = "(current directory)"
	}
	c.info = append(c.info, "harness: "+cfg.Harness, "workdir: "+workdir)
	if len(cfg.Projects) > 0 {
		for _, p := range cfg.Projects {
			c.info = append(c.info, "backlog ["+p.Slug+"]: "+orNone(cfg.ProjectSummaryURL(p)))
		}
	} else {
		c.info = append(c.info, "backlog: "+orNone(cfg.BacklogSummaryURL()))
	}
	return c
}

// --- 2. harness --------------------------------------------------------------

func checkHarness(ctx context.Context, cfg *config.Config, e doctorEnv) check {
	c := check{name: "harness"}
	// Every adapter execs its own name verbatim (see internal/harness), so the
	// harness name IS the binary to look for.
	bin := cfg.Harness

	path, err := e.lookPath(bin)
	if err != nil {
		c.status = fail
		c.detail = bin + " is not on PATH"
		c.remedy = "install " + bin + ", or select another harness (" + strings.Join(harness.Names(), " | ") + ")"
		return c
	}
	ver, err := e.binVersion(ctx, bin)
	if err != nil {
		// On PATH but not runnable — a broken shim, or a wrapper that wants a TTY.
		// The loop's first session dies on this, so it is a FAIL, not a WARN.
		c.status = fail
		c.detail = path + " is not runnable: " + err.Error()
		c.remedy = "check it runs non-interactively: " + bin + " --version"
		return c
	}
	c.status = pass
	c.detail = path + " (" + ver + ")"
	return c
}

// --- 3. config_dir -----------------------------------------------------------

// authMarkers are files that suggest a harness config dir has actually been
// initialised, rather than being an empty directory pointed at by mistake.
//
// Their ABSENCE is only ever a WARN: on macOS the credential may live in the
// system keychain instead of the config dir, so a missing marker means "I cannot
// confirm this", never "this is broken".
var authMarkers = map[string][]string{
	"claude":   {".credentials.json", ".claude.json", "settings.json"},
	"codex":    {"auth.json", "config.toml"},
	"opencode": {"auth.json", "config.json"},
}

func checkConfigDir(cfg *config.Config) check {
	c := check{name: "config_dir"}

	// Validate has already expanded a leading ~, so what we stat is what the
	// adapter will export.
	dir := cfg.ConfigDir
	if dir == "" {
		// Unset is survivable interactively (the ambient environment carries it)
		// and is exactly what bites an unattended cron/launchd run, whose bare
		// environment has no skills, plugins or auth at all.
		c.status = warn
		c.detail = "not set — the session inherits the ambient environment"
		c.remedy = "set config_dir (or --config-dir) so a cron/launchd run loads the same skills, plugins and auth as your terminal"
		return c
	}

	fi, err := os.Stat(dir)
	if err != nil {
		c.status = fail
		c.detail = dir + " does not resolve: " + err.Error()
		c.remedy = "create it, or point config_dir at your real harness config dir"
		return c
	}
	if !fi.IsDir() {
		c.status = fail
		c.detail = dir + " is not a directory"
		c.remedy = "point config_dir at a directory, not a file"
		return c
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		c.status = fail
		c.detail = dir + " is not readable: " + err.Error()
		c.remedy = "fix its permissions — the harness must be able to read it"
		return c
	}
	if len(entries) == 0 {
		c.status = warn
		c.detail = dir + " is empty — no skills, plugins or auth state"
		c.remedy = "initialise it by running " + cfg.Harness + " once with this config dir, or point it elsewhere"
		return c
	}
	if !hasAnyMarker(dir, authMarkers[cfg.Harness]) {
		c.status = warn
		c.detail = dir + " has no recognisable " + cfg.Harness + " auth state"
		c.remedy = "confirm a headless run can authenticate (the credential may be in the OS keychain, which doctor cannot see)"
		return c
	}
	c.status = pass
	c.detail = dir
	return c
}

func hasAnyMarker(dir string, markers []string) bool {
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	return false
}

// --- 4. backlog wiring -------------------------------------------------------

// checkBacklog verifies the driver's cheap control-plane read. A multi-project
// instance gets ONE CHECK PER PROJECT: each has its own slug-ful route and its
// own .mcp.json, so one of them can be wired wrong while the others are fine —
// and a single aggregate line would hide exactly that.
func checkBacklog(ctx context.Context, cfg *config.Config, e doctorEnv) []check {
	if len(cfg.Projects) == 0 {
		return []check{backlogCheck(ctx, "backlog", cfg.BacklogSummaryURL(), e)}
	}
	out := make([]check, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		out = append(out, backlogCheck(ctx, "backlog["+p.Slug+"]", cfg.ProjectSummaryURL(p), e))
	}
	return out
}

func backlogCheck(ctx context.Context, name, summaryURL string, e doctorEnv) check {
	c := check{name: name}

	// The four not-wired cases are kept distinct on purpose. "Blind" (no creds, no
	// endpoint, unreachable) still makes progress, so it WARNs; a rejected key or a
	// key/route mismatch will never self-heal and burns doomed sessions, so it FAILs.
	if e.apiKey == "" {
		c.status = warn
		c.detail = "loop would run blind: no creds — CLANKERBAR_API_KEY is unset"
		c.remedy = "export CLANKERBAR_API_KEY (mint one at clankerbar.com/projects/<slug>/api-keys)"
		return c
	}
	if summaryURL == "" {
		c.status = warn
		c.detail = "loop would run blind: no summary endpoint could be derived"
		c.remedy = "declare projects[].slug, or point mcp_config_path at an .mcp.json naming /mcp/<slug>"
		return c
	}

	sum, err := e.newPoller(summaryURL, e.apiKey).Poll(ctx)
	switch {
	case err == nil:
		c.status = pass
		c.detail = fmt.Sprintf("%s — %d claimable, %d open question(s)", summaryURL, sum.Claimable, sum.OpenQuestions)
		if sum.Paused {
			c.detail += " [loop paused from the console]"
		}
	case errors.Is(err, backlog.ErrUnauthorized):
		c.status = fail
		c.detail = "key rejected (401/403) by " + summaryURL
		c.remedy = "CLANKERBAR_API_KEY is revoked, wrong or malformed — mint a fresh key; the harness sessions carry this same key"
	case errors.Is(err, backlog.ErrProjectRequired):
		// The remedy follows the 2026-07-29 decision: the loop runs on the ACCOUNT
		// key, so the fix is a project SELECTOR, not a different key.
		c.status = fail
		c.detail = "key/route mismatch: 400 project_required from the slug-less route"
		c.remedy = "declare the project slug(s) so the driver polls /api/projects/<slug>/backlog-summary (a project-scoped key also works, CI-style)"
	case errors.Is(err, backlog.ErrNotWired):
		c.status = warn
		c.detail = "loop would run blind: backlog polling is not wired"
		c.remedy = "set both a derivable project endpoint and CLANKERBAR_API_KEY so the loop can gate on live counts"
	default:
		c.status = warn
		c.detail = "loop would run blind: endpoint unreachable — " + err.Error()
		c.remedy = "check the plane is reachable: " + summaryURL
	}
	return c
}

// --- 5. workdir --------------------------------------------------------------

func checkWorkdir(cfg *config.Config) check {
	c := check{name: "workdir"}
	stateDir := cfg.ResolveStateDir()

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		c.status = fail
		c.detail = stateDir + " is not creatable: " + err.Error()
		c.remedy = "create it by hand, or point state_dir somewhere writable"
		return c
	}
	// Creatable is not writable: an already-existing state dir can be read-only,
	// and the loop writes a log per iteration. Prove it with a real write.
	probe := filepath.Join(stateDir, ".doctor-write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		c.status = fail
		c.detail = stateDir + " is not writable: " + err.Error()
		c.remedy = "fix its permissions, or point state_dir somewhere writable"
		return c
	}
	_ = os.Remove(probe)

	// A leftover marker stops the loop on its first tick — the failure that looks
	// exactly like "the backlog was empty".
	var found []string
	for _, m := range []string{"HALT", "STOP"} {
		if _, err := os.Stat(filepath.Join(stateDir, m)); err == nil {
			found = append(found, m)
		}
	}
	if len(found) > 0 {
		c.status = warn
		c.detail = stateDir + " has a leftover " + strings.Join(found, " and ") + " marker"
		c.remedy = "delete it, or the loop stops immediately: rm " + filepath.Join(stateDir, found[0])
		return c
	}

	// Sessions spawned in a multi-repo parent with no .mcp.json get no clankerbar
	// tools at all — they start, burn tokens, and cannot see the backlog.
	if cfg.MCPConfigPath == "" && isMultiRepoParent(cfg.WorkDir) {
		c.status = warn
		c.detail = workdirLabel(cfg.WorkDir) + " looks like a multi-repo parent but has no .mcp.json"
		c.remedy = "add an .mcp.json there (or set mcp_config_path) — sessions spawned here would have no clankerbar tools"
		return c
	}

	c.status = pass
	c.detail = stateDir + " writable, no stop markers"
	return c
}

// isMultiRepoParent reports whether dir is not itself a checkout but holds two or
// more of them — the `~/dev` shape, where a session's cwd is a parent rather than
// a repo. Two is the threshold because a single nested checkout is more likely a
// vendored dependency than a workspace layout.
func isMultiRepoParent(dir string) bool {
	if dir == "" {
		dir = "."
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	repos := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, entry.Name(), ".git")); err == nil {
			repos++
			if repos >= 2 {
				return true
			}
		}
	}
	return false
}

func workdirLabel(dir string) string {
	if dir == "" {
		return "the current directory"
	}
	return dir
}

// --- 6. permission policy ----------------------------------------------------

func checkPermissions(cfg *config.Config) check {
	c := check{name: "permissions"}
	switch cfg.Harness {
	case "claude":
		if cfg.SettingsPath == "" {
			c.status = warn
			c.detail = "no settings_path — the unattended session runs on the ambient allowlist"
			c.remedy = "set settings_path to a headless permission policy (its deny rules win over the config dir's)"
			return c
		}
		data, err := os.ReadFile(cfg.SettingsPath)
		if err != nil {
			c.status = fail
			c.detail = cfg.SettingsPath + " is unreadable: " + err.Error()
			c.remedy = "create the settings file, or clear settings_path"
			return c
		}
		if !json.Valid(data) {
			// Claude rejects an unparseable --settings file at startup, so this kills
			// the first session rather than degrading it.
			c.status = fail
			c.detail = cfg.SettingsPath + " is not valid JSON"
			c.remedy = "fix the JSON — the harness refuses to start with an unparseable --settings file"
			return c
		}
		c.status = pass
		c.detail = cfg.SettingsPath + " parses"

	case "codex":
		// The adapter pins both axes itself, so there is nothing an operator can
		// leave incoherent today. Report what will actually be used.
		c.status = pass
		c.detail = "adapter pins --sandbox workspace-write --ask-for-approval never"

	case "opencode":
		// The adapter always exports a fail-closed OPENCODE_PERMISSION — but exec
		// takes the LAST duplicate key, so an operator's own env silently wins.
		if v, ok := cfg.Env["OPENCODE_PERMISSION"]; ok {
			c.status = warn
			c.detail = "env overrides the adapter's fail-closed OPENCODE_PERMISSION"
			c.remedy = "drop it, or confirm it denies what an unattended run must not call: " + truncate(v, 60)
			return c
		}
		c.status = pass
		c.detail = "adapter exports a fail-closed OPENCODE_PERMISSION"

	default:
		c.status = pass
		c.detail = "no permission-policy checks for " + cfg.Harness
	}
	return c
}

// --- 7. budget ---------------------------------------------------------------

func checkBudget(cfg *config.Config) check {
	c := check{name: "budget"}
	b := cfg.Budget

	// A negative ceiling is never what anyone meant, and Budget.Exceeded ignores it
	// (its guards are all `> 0`) — so it reads as "set" while doing nothing at all.
	var bad []string
	if b.MaxTokens < 0 {
		bad = append(bad, "max_tokens")
	}
	if b.MaxCostUSD < 0 {
		bad = append(bad, "max_cost_usd")
	}
	if b.MaxWallClock < 0 {
		bad = append(bad, "max_wall_clock")
	}
	if len(bad) > 0 {
		c.status = fail
		c.detail = "negative budget " + plural(len(bad), "value", "values") + ": " + strings.Join(bad, ", ")
		c.remedy = "use a positive ceiling; 0 disables that dimension"
		return c
	}

	var set []string
	if b.MaxTokens > 0 {
		set = append(set, fmt.Sprintf("max_tokens=%d", b.MaxTokens))
	}
	if b.MaxCostUSD > 0 {
		set = append(set, fmt.Sprintf("max_cost_usd=%g", b.MaxCostUSD))
	}
	if b.MaxWallClock > 0 {
		set = append(set, "max_wall_clock="+b.MaxWallClock.Duration().String())
	}

	c.status = pass
	if len(set) == 0 {
		// Informational, not a failure: an unbounded daemon is a legitimate setup.
		c.detail = "no ceiling configured — the loop runs until the backlog is dry or it is stopped"
		return c
	}
	c.detail = strings.Join(set, ", ")
	return c
}

// --- rendering ---------------------------------------------------------------

func printCheck(w io.Writer, c check) {
	fmt.Fprintf(w, "%-4s  %-12s %s\n", c.status, c.name, c.detail)
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
