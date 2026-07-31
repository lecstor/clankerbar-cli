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
	"time"
	"unicode/utf8"

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
	checks = append(checks, checkStateDir(cfg))
	checks = append(checks, checkSessions(cfg)...)
	return append(checks, checkPermissions(cfg), checkToolchains(cfg), checkBudget(cfg))
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
			// Spelled exactly like the check name below, so an operator can grep one
			// line for the other.
			c.info = append(c.info, "backlog["+p.Slug+"]: "+orNone(cfg.ProjectSummaryURL(p)))
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
	// A harness with no marker list is one doctor has no opinion about — a newly
	// registered adapter, say. Asserting "no recognisable auth state" there would be
	// a permanent, unfixable WARN about a table this file forgot to update.
	if markers, known := authMarkers[cfg.Harness]; known && !hasAnyMarker(dir, markers) {
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
	return firstMarker(dir, markers) != ""
}

// firstMarker returns the first of markers that exists in dir, or "". Callers
// report the name they found, so an operator can see WHICH file satisfied a
// check rather than trusting that one did.
func firstMarker(dir string, markers []string) string {
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return m
		}
	}
	return ""
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
		// A paused queue is the most certain no-op there is: the driver polls all
		// night and spawns nothing. That is precisely the "the window produced
		// nothing and I did not know" class doctor exists for, so it WARNs rather
		// than decorating a PASS with a footnote the operator skims past.
		if sum.Paused {
			c.status = warn
			c.detail += " — paused from the console; the loop will not spawn sessions"
			c.remedy = "resume the loop at clankerbar.com, or expect an idle run"
			return c
		}
		// Nothing claimable is not itself a problem — the loop idle-polls for free
		// rather than spawning. It IS worth saying out loud when the operator holds
		// the only key, because "no work" and "work you have not unblocked" look
		// identical in the counts and read as a healthy queue.
		if sum.Claimable == 0 && sum.OpenQuestions > 0 {
			c.status = warn
			c.detail += " — nothing to claim; the loop will idle without spawning"
			c.remedy = "answer the open question(s) at clankerbar.com, or expect an idle run"
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

// --- 5. state dir ------------------------------------------------------------

// checkStateDir covers the one directory the DRIVER itself writes: iteration
// logs and the stop/pause markers. The directories the SESSIONS run in are a
// separate, per-project concern — see checkSessions.
func checkStateDir(cfg *config.Config) check {
	c := check{name: "state_dir"}
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

	c.status = pass
	c.detail = stateDir + " writable, no stop markers"
	return c
}

// --- 6. session workdirs -----------------------------------------------------

// agentInstructionFiles are the names a harness reads a project's standing
// instructions from, in the session's cwd. AGENTS.md is the cross-tool
// convention and CLAUDE.md the Claude Code one; either is enough to prove the
// operator has oriented the sessions that start here.
var agentInstructionFiles = []string{"AGENTS.md", "CLAUDE.md"}

// checkSessions inspects the directory each project's sessions are spawned in.
// ONE CHECK PER PROJECT, for the same reason the backlog check is per project: a
// multi-project instance can have one queue pointed somewhere that teaches its
// sessions nothing while the others are fine, and an aggregate line would hide
// exactly that.
//
// The failure this exists for is silent and expensive rather than loud. A
// harness reads its instruction file, its skills and its project settings from
// the session's cwd AND UPWARD — never from repos below it. Spawn in a multi-repo
// parent and every session starts with no protocol, no conventions and no
// permissions beyond the ambient ones, then spends its opening minutes
// rediscovering the layout. It still works, which is why nobody notices for
// thirty iterations.
func checkSessions(cfg *config.Config) []check {
	if len(cfg.Projects) == 0 {
		return []check{sessionCheck("workdir", cfg.WorkDir, cfg.MCPConfigPath)}
	}
	out := make([]check, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		out = append(out, sessionCheck("workdir["+p.Slug+"]", projectWorkDir(cfg, p), projectMCPConfig(cfg, p)))
	}
	return out
}

// projectWorkDir and projectMCPConfig resolve a project entry EXACTLY as
// loop.Driver.invocation does — the project's own value, falling back to the
// top-level one. Doctor answering a differently-resolved question is the whole
// failure mode it exists to prevent: read p.WorkDir raw and an empty entry
// resolves to the current directory, so a config like
//
//	{"workdir":"~/dev","projects":[{"slug":"acme"}]}
//
// would be diagnosed against wherever the operator happened to run doctor. That
// never FAILs (a cwd always resolves), so it reports green for a directory the
// loop will never use — the dangerous direction.
func projectWorkDir(cfg *config.Config, p config.Project) string {
	if p.WorkDir != "" {
		return p.WorkDir
	}
	return cfg.WorkDir
}

func projectMCPConfig(cfg *config.Config, p config.Project) string {
	if p.MCPConfigPath != "" {
		return p.MCPConfigPath
	}
	return cfg.MCPConfigPath
}

func sessionCheck(name, dir, mcpConfigPath string) check {
	c := check{name: name}

	resolved := dir
	if resolved == "" {
		resolved = "."
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		// The harness cannot start in a directory that is not there, so every
		// iteration for this project would die on spawn.
		c.status = fail
		c.detail = workdirLabel(dir) + " does not resolve: " + err.Error()
		c.remedy = "create it, or point this project's workdir at a real checkout"
		return c
	}
	if !fi.IsDir() {
		c.status = fail
		c.detail = workdirLabel(dir) + " is not a directory"
		c.remedy = "point the workdir at a directory, not a file"
		return c
	}

	parent := isMultiRepoParent(resolved)

	// A session with no .mcp.json reaching it gets no clankerbar tools at all — it
	// starts, burns tokens, and cannot see the backlog. config.Validate has already
	// defaulted this to <workdir>/.mcp.json when that file exists, so an empty value
	// here means none was found and none was configured.
	//
	// This is NOT gated on the multi-repo-parent shape: a plain single-repo workdir
	// with no .mcp.json blinds its sessions just as completely. `parent` only
	// selects the wording, because that is the case an operator is most likely to
	// have reached by accident.
	if mcpConfigPath == "" {
		c.status = warn
		if parent {
			c.detail = workdirLabel(dir) + " looks like a multi-repo parent and has no .mcp.json"
		} else {
			c.detail = workdirLabel(dir) + " has no .mcp.json"
		}
		c.remedy = "add an .mcp.json there (or set mcp_config_path) — sessions spawned here would have no clankerbar tools"
		return c
	}

	if marker := firstMarker(resolved, agentInstructionFiles); marker == "" {
		c.status = warn
		c.detail = workdirLabel(dir) + " has no agent-instructions file (" + strings.Join(agentInstructionFiles, " / ") + ")"
		if parent {
			c.remedy = "add one here naming each repo below and where its protocol lives — a session started in a multi-repo parent loads nothing from the repos under it"
		} else {
			c.remedy = "add one so unattended sessions read the project's conventions instead of inferring them"
		}
		return c
	}

	c.status = pass
	c.detail = resolved + " (" + firstMarker(resolved, agentInstructionFiles) + ")"
	if parent {
		c.detail += ", multi-repo parent"
	}
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

// --- 7. permission policy ----------------------------------------------------

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

// --- 8. toolchain grants -----------------------------------------------------

// toolchainMarkers maps a marker file to the command a session must be allowed
// to execute to verify that repo. Only markers that name their tool
// UNAMBIGUOUSLY are listed: a bare package.json could be npm, pnpm or yarn, so
// the lockfile is the marker and a lockless repo is left alone rather than
// guessed at.
var toolchainMarkers = []struct{ marker, tool string }{
	{"go.mod", "go"},
	{"Cargo.toml", "cargo"},
	{"pnpm-lock.yaml", "pnpm"},
	{"yarn.lock", "yarn"},
	{"package-lock.json", "npm"},
}

// checkToolchains cross-references the build tools the backlog's repos need
// against what the permission policy actually grants.
//
// A headless claude session FAILS CLOSED: anything the policy does not name is
// refused outright, with no prompt reaching the operator. So a toolchain the
// repos need but the allowlist never mentions does not stop a run — it produces
// work that CANNOT BE VERIFIED, which is worse. One overnight run wrote a whole
// Go subcommand, pushed it, and re-discovered the same wall on three separate
// iterations because nothing checked this up front.
//
// Every finding here is a WARN, never a FAIL: doctor reads the settings files it
// knows about, and a grant can also arrive by a rule form it does not parse or a
// flag on the harness invocation. A false FAIL would block a run that works.
func checkToolchains(cfg *config.Config) check {
	c := check{name: "toolchains", status: pass}
	if cfg.Harness != "claude" {
		c.detail = "no allowlist to audit for " + cfg.Harness
		return c
	}

	needed := detectToolchains(cfg)
	if len(needed) == 0 {
		c.detail = "no unambiguous build toolchains detected in the session workdirs"
		return c
	}
	tools := make([]string, 0, len(needed))
	for _, t := range needed {
		tools = append(tools, t.tool)
	}

	if cfg.SettingsPath == "" {
		// checkPermissions already warns about running on the ambient allowlist; here
		// the useful thing is to name what would have to be in it.
		c.detail = "needs " + strings.Join(tools, ", ") + " — grants come from the ambient allowlist, which doctor does not audit"
		return c
	}

	allow, denyAll, denyVerbs := grantedCommands(cfg)
	var missing, blocked, narrowed []string
	for _, t := range needed {
		switch {
		case denyAll[t.tool]:
			// A bare-head deny wins over every allow, so this one is decided and worth
			// naming separately — an operator looking at an allow entry would call it
			// granted.
			blocked = append(blocked, t.tool+" ("+t.where+")")
		case !allow[t.tool]:
			missing = append(missing, t.tool+" ("+t.where+")")
		case len(denyVerbs[t.tool]) > 0:
			// Granted, but with holes. Not a warning on its own — this is what a
			// careful policy looks like — so it is reported alongside the PASS.
			narrowed = append(narrowed, t.tool+" (except "+strings.Join(denyVerbs[t.tool], ", ")+")")
		}
	}

	switch {
	case len(blocked) > 0:
		c.status = warn
		c.detail = "denied by policy: " + strings.Join(blocked, ", ")
		c.remedy = "remove the deny rule in " + cfg.SettingsPath + ", or accept that tasks in those repos cannot be verified"
	case len(missing) > 0:
		c.status = warn
		c.detail = "no grant for: " + strings.Join(missing, ", ")
		c.remedy = "allow the verbs each one needs in " + cfg.SettingsPath +
			" (e.g. Bash(go build:*), Bash(go vet:*), Bash(go test:*)) — a headless session fails closed, so an ungranted tool is refused with no prompt and its task ships unverified"
	default:
		c.detail = "granted: " + strings.Join(tools, ", ")
		if len(narrowed) > 0 {
			c.detail += "; narrowed by deny rules: " + strings.Join(narrowed, ", ")
		}
	}
	return c
}

// neededToolchain is a tool the repos require, with one example of where the
// requirement was found — an operator fixing a grant wants to know which repo
// asked for it.
type neededToolchain struct {
	tool  string
	where string
}

// detectToolchains looks in each session workdir and, when that workdir is a
// multi-repo parent, in the checkouts under it that belong to the project.
//
// Scope is the whole difficulty here. A session rooted in a multi-repo parent
// COULD be sent into any repo below it, but only the project's own repos are
// plausibly in its backlog — and warning about every unrelated checkout on the
// machine is how a WARN line becomes something an operator skims. The project's
// repos are taken to be the ones named after its slug (`clankerbar` covers
// `clankerbar/` and `clankerbar-cli/`), which under-reports for a project whose
// repos are named differently. That direction is deliberate: a missed grant
// surfaces as one refused command, while a wall of irrelevant warnings buries the
// one that mattered.
func detectToolchains(cfg *config.Config) []neededToolchain {
	type target struct{ dir, slug string }
	var targets []target
	if len(cfg.Projects) == 0 {
		targets = append(targets, target{dir: cfg.WorkDir})
	} else {
		for _, p := range cfg.Projects {
			targets = append(targets, target{dir: projectWorkDir(cfg, p), slug: p.Slug})
		}
	}

	found := map[string]string{}
	for _, t := range targets {
		dir := t.dir
		if dir == "" {
			dir = "."
		}
		for _, scan := range append([]string{dir}, projectRepos(dir, t.slug)...) {
			for _, m := range toolchainMarkers {
				if _, ok := found[m.tool]; ok {
					continue
				}
				if _, err := os.Stat(filepath.Join(scan, m.marker)); err == nil {
					found[m.tool] = scan
				}
			}
		}
	}

	// Iterate the marker table, not the map, so the output order is stable.
	out := make([]neededToolchain, 0, len(found))
	for _, m := range toolchainMarkers {
		if where, ok := found[m.tool]; ok {
			out = append(out, neededToolchain{tool: m.tool, where: where})
		}
	}
	return out
}

// projectRepos returns the immediate subdirectories of dir that are checkouts
// belonging to slug. An empty slug matches nothing: without a project name there
// is no way to tell the project's repos from everything else on the disk.
func projectRepos(dir, slug string) []string {
	if slug == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), slug) {
			continue
		}
		child := filepath.Join(dir, entry.Name())
		if _, err := os.Stat(filepath.Join(child, ".git")); err == nil {
			out = append(out, child)
		}
	}
	return out
}

// policySettingsPaths lists every settings file Claude will MERGE for these
// sessions, in no particular order — auditing only the drain policy would report
// a tool as ungranted when another layer already allows it.
//
// The session's cwd matters as much as the config dir: `<workdir>/.claude/settings.json`
// and its `.local` sibling are the project layer, and are where an operator is
// most likely to have put `Bash(go test:*)` in the first place.
func policySettingsPaths(cfg *config.Config) []string {
	paths := []string{cfg.SettingsPath}
	if cfg.ConfigDir != "" {
		paths = append(paths,
			filepath.Join(cfg.ConfigDir, "settings.json"),
			filepath.Join(cfg.ConfigDir, "settings.local.json"),
		)
	}
	for _, dir := range sessionWorkDirs(cfg) {
		if dir == "" {
			dir = "."
		}
		paths = append(paths,
			filepath.Join(dir, ".claude", "settings.json"),
			filepath.Join(dir, ".claude", "settings.local.json"),
		)
	}
	return paths
}

// sessionWorkDirs is the set of directories sessions are actually spawned in,
// resolved the way the loop resolves them.
func sessionWorkDirs(cfg *config.Config) []string {
	if len(cfg.Projects) == 0 {
		return []string{cfg.WorkDir}
	}
	out := make([]string, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		out = append(out, projectWorkDir(cfg, p))
	}
	return out
}

// grantedCommands returns, per Bash command head, whether the policy grants it
// and whether it denies it OUTRIGHT.
//
// "Outright" is the load-bearing word. Deny wins over allow in Claude's policy,
// but only for what the deny rule actually covers: a narrow `Bash(go run:*)` deny
// alongside a `Bash(go test:*)` allow is a normal, careful setup, and reporting
// that as "go is denied, tasks in those repos cannot be verified" would be flatly
// wrong. So only a BARE head deny (`Bash(go)` / `Bash(go:*)`) counts as decisive;
// a verb-qualified deny is recorded separately and merely narrows.
func grantedCommands(cfg *config.Config) (allow, denyAll map[string]bool, denyVerbs map[string][]string) {
	allow, denyAll, denyVerbs = map[string]bool{}, map[string]bool{}, map[string][]string{}
	for _, path := range policySettingsPaths(cfg) {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue // absent or unreadable is not a grant; other checks report it
		}
		var doc struct {
			Permissions struct {
				Allow []string `json:"allow"`
				Deny  []string `json:"deny"`
			} `json:"permissions"`
		}
		if json.Unmarshal(data, &doc) != nil {
			continue // checkPermissions reports unparseable JSON as a FAIL
		}
		for _, rule := range doc.Permissions.Allow {
			if head, _ := bashHead(rule); head != "" {
				allow[head] = true
			}
		}
		for _, rule := range doc.Permissions.Deny {
			head, verb := bashHead(rule)
			switch {
			case head == "":
			case verb == "":
				denyAll[head] = true
			default:
				denyVerbs[head] = append(denyVerbs[head], head+" "+verb)
			}
		}
	}
	return allow, denyAll, denyVerbs
}

// bashHead splits a Bash permission rule into its command word and the verb that
// qualifies it, if any: `Bash(go build:*)` yields ("go", "build"), while
// `Bash(go:*)` and `Bash(go)` yield ("go", ""). Non-Bash rules yield ("", "").
func bashHead(rule string) (head, verb string) {
	inner, ok := strings.CutPrefix(strings.TrimSpace(rule), "Bash(")
	if !ok {
		return "", ""
	}
	inner = strings.TrimSuffix(inner, ")")
	if before, _, found := strings.Cut(inner, ":"); found {
		inner = before
	}
	fields := strings.Fields(inner)
	switch len(fields) {
	case 0:
		return "", ""
	case 1:
		return fields[0], ""
	default:
		return fields[0], fields[1]
	}
}

func firstField(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// --- 9. budget ---------------------------------------------------------------

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

	// Wall clock is the weakest proxy for spend of the three, because it counts
	// the hours a run spends WAITING OUT a usage limit — time in which nothing is
	// billed. A run capped at 8h can spend five of them asleep and stop having done
	// three iterations. Cost is the dial that tracks what an operator actually
	// means by "leave headroom", and it comes straight from the harness.
	if b.MaxWallClock > 0 && b.MaxCostUSD == 0 && b.MaxTokens == 0 {
		c.status = warn
		c.detail = strings.Join(set, ", ") + " — wall clock is the only ceiling, and it counts time spent waiting out usage limits"
		c.remedy = "add max_cost_usd as the real ceiling; keep max_wall_clock as the outer bound on how late a run may finish"
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
