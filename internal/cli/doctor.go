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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/statedir"
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
	checks = append(checks, checkBacklog(ctx, cfg, e)...)
	checks = append(checks, checkStateDir(cfg))
	checks = append(checks, checkSessions(cfg)...)
	checks = append(checks, checkMCPServers(cfg))
	checks = append(checks, checkPermissionsAll(cfg)...)
	return append(checks, checkToolchains(cfg), checkPower(ctx, e), checkBudget(cfg))
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
	// Which phase runs where, but only when that is not the same answer twice: a
	// mixed sequence is the one config where "harness: <x>" above is true and
	// incomplete, and the pairing is what an operator wants to eyeball before an
	// overnight run - not that two harnesses are involved, but which way round.
	if len(cfg.PhaseHarnesses()) > 1 {
		pairs := make([]string, 0, len(cfg.Phases))
		for i, ph := range cfg.EffectivePhases() {
			pairs = append(pairs, ph.Label(i)+" -> "+ph.Harness)
		}
		c.info = append(c.info, "phase harnesses: "+strings.Join(pairs, ", "))
	}
	// The one destination the account-scoped key is allowed to reach (CLA-257).
	// Named here so the preflight answers "where does my credential go" without the
	// operator having to reason about which file won.
	c.info = append(c.info, "api key origin: "+orNone(cfg.CredentialOrigin()))
	// What this config hands the child process, by NAME only - never a value, and
	// never the file an @path names. A config that reaches the loop decides the
	// spawned session's environment (CLA-260), so "which variables am I injecting"
	// should be answerable from the preflight rather than by re-reading the file.
	if names := envKeyNames(cfg.Env); names != "" {
		c.info = append(c.info, "env: "+names)
	}
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

// envKeyNames renders the config's extra environment as a sorted list of KEYS,
// with no values: one of them is routinely a credential, and doctor's output is
// the thing an operator pastes into an issue.
func envKeyNames(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// --- 2. harness --------------------------------------------------------------

// checkHarness reports on the run's own harness. Kept as the single-harness
// entry point; checkHarnesses below is what doctorChecks calls, because a
// sequence whose phases name other harnesses spawns binaries this one never
// looks at.
func checkHarness(ctx context.Context, cfg *config.Config, e doctorEnv) check {
	return checkHarnessNamed(ctx, "harness", cfg.Harness, e)
}

// checkHarnesses reports on EVERY harness this config DECLARES — the run's and
// every phase's (CLA-366) - every harness the config DECLARES, since a declared
// harness must be usable whether or not today's phases reach it. Checking only
// `harness` is how a mixed-harness config earns a green preflight and then dies at
// its second phase on a binary that was never on PATH.
func checkHarnesses(ctx context.Context, cfg *config.Config, e doctorEnv) []check {
	names := cfg.PhaseHarnesses()
	out := make([]check, 0, len(names))
	for i, n := range names {
		// The first keeps the bare name, so a single-harness report reads exactly
		// as it always has; the rest are qualified, so two lines cannot be confused.
		label := "harness"
		if i > 0 {
			label = "harness[" + n + "]"
		}
		out = append(out, checkHarnessNamed(ctx, label, n, e))
	}
	return out
}

func checkHarnessNamed(ctx context.Context, label, bin string, e doctorEnv) check {
	c := check{name: label}
	// Every adapter execs its own name verbatim (see internal/harness), so the
	// harness name IS the binary to look for.

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

// checkConfigDir reports on the run harness's config dir. See checkConfigDirs,
// which is what doctorChecks calls.
func checkConfigDir(cfg *config.Config) check {
	return checkConfigDirNamed("config_dir", cfg.Harness, cfg.SessionFor(cfg.Harness).ConfigDir)
}

// checkConfigDirs reports on the config dir of every harness this config will
// spawn (CLA-366). A phase on another harness inherits nothing from the run-wide
// `config_dir` — deliberately, since it is that harness's own dialect — so its
// dir is a separate field that can be separately absent.
func checkConfigDirs(cfg *config.Config) []check {
	names := cfg.PhaseHarnesses()
	out := make([]check, 0, len(names))
	for i, n := range names {
		label := "config_dir"
		if i > 0 {
			label = "config_dir[" + n + "]"
		}
		out = append(out, checkConfigDirNamed(label, n, cfg.SessionFor(n).ConfigDir))
	}
	return out
}

func checkConfigDirNamed(label, harnessName, dir string) check {
	c := check{name: label}

	// Validate has already expanded a leading ~, so what we stat is what the
	// adapter will export.
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
		c.remedy = "initialise it by running " + harnessName + " once with this config dir, or point it elsewhere"
		return c
	}
	// A harness with no marker list is one doctor has no opinion about — a newly
	// registered adapter, say. Asserting "no recognisable auth state" there would be
	// a permanent, unfixable WARN about a table this file forgot to update.
	if markers, known := authMarkers[harnessName]; known && !hasAnyMarker(dir, markers) {
		c.status = warn
		c.detail = dir + " has no recognisable " + harnessName + " auth state"
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
		// Named only when there is some, and named separately from `claimable`. The
		// warning below already reads through Spawnable() and so goes quiet on its
		// own, but a PASS reading "0 claimable" in front of an operator whose loop is
		// about to start spawning is the same drift one line earlier: they would read
		// it as an idle night (CLA-274).
		if sum.StaleClaimable > 0 {
			c.detail += fmt.Sprintf(", %d abandoned to recover", sum.StaleClaimable)
		}
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
		if !sum.Spawnable() && sum.OpenQuestions > 0 {
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
	stateDir, err := cfg.ResolveStateDir()
	if err != nil {
		c.status = fail
		c.detail = "cannot resolve the state dir: " + err.Error()
		c.remedy = "set state_dir explicitly, or XDG_STATE_HOME"
		return c
	}

	// Open it exactly as the loop will, so the preflight exercises the real
	// creation, the real mode tightening, the real symlink refusal and the real
	// .gitignore — not an approximation that could pass where the loop fails.
	dir, err := statedir.Open(stateDir, cfg.SessionWorkDirs()...)
	if err != nil {
		c.status = fail
		c.detail = err.Error()
		// Deliberately generic: Open refuses for several distinct reasons — mode,
		// ownership, a symlink on the path, a directory that is somebody else's —
		// and each of those errors already names the path and the way out. A
		// specific remedy here would contradict most of them.
		c.remedy = "point state_dir at a directory you own that is empty or already a clankerbar state dir; the message above says which part is wrong"
		return c
	}
	defer dir.Close()

	// WHERE the state dir sits is a capability question, not a tidiness one: STOP
	// and HALT live there, and a session may write anywhere under the workdir it is
	// spawned in. A state dir inside one hands every spawned session the daemon's
	// own stop switch. CLA-259 moved the DEFAULT out; an explicit state_dir still
	// wins, including one pointed back inside, so the operators this catches are
	// exactly the ones the new default never reached.
	inside := containingWorkDir(stateDir, cfg)

	// Creatable is not writable: an already-existing state dir can be read-only,
	// and the loop writes a log per iteration. Prove it with a real write, under
	// a name nobody could have got to first — a fixed probe name in a directory
	// somebody else can write is a path they can point at a file of their
	// choosing, and this used to truncate whatever it found there (CLA-259).
	probe := ".doctor-write-probe-" + randomTail()
	if err := dir.WriteFile(probe, []byte("ok")); err != nil {
		c.status = fail
		c.detail = stateDir + " is not writable: " + err.Error()
		c.remedy = "fix its permissions, or point state_dir somewhere writable"
		return c
	}
	_ = dir.Remove(probe)

	// A leftover marker stops the loop on its first tick — the failure that looks
	// exactly like "the backlog was empty".
	var found []string
	for _, m := range []string{"HALT", "STOP"} {
		if dir.Exists(m) {
			found = append(found, m)
		}
	}
	if len(found) > 0 {
		c.status = warn
		c.detail = stateDir + " has a leftover " + strings.Join(found, " and ") + " marker"
		c.remedy = "delete it, or the loop stops immediately: rm " + filepath.Join(stateDir, found[0])
		if inside.session {
			// Otherwise the operator deletes the marker, runs again, and a session
			// writes it back - the remedy above is a symptom's remedy when the state
			// dir is somewhere the sessions can reach. Only for a SESSION workdir: a
			// configured-but-unused one is not somewhere a marker can have come from.
			c.info = append(c.info, "it is inside the session workdir "+inside.dir+", so a session could have written that marker itself")
		}
		return c
	}

	// Reported ahead of the legacy leftover below, which is the smaller problem: a
	// leftover directory is only ever READ from, while this is a live write
	// capability. WARN, not FAIL - an explicit state_dir is supported and the run
	// still makes progress, and `doctor && run` must not be gated on a setup that
	// works.
	if inside.dir != "" {
		c.status = warn
		c.detail, c.remedy = stateDirInWorkDir(stateDir, inside, cfg)
		// The legacy report below is unreachable once we return, and it is a
		// separate fact about a separate directory - carry it rather than lose it.
		if legacy := cfg.LegacyStateDir(); legacy != "" {
			c.info = append(c.info, legacy+" is also a leftover from before the state dir moved out of the workdir; markers there are ignored now")
		}
		// Same for the implicit-workdir note the PASS line carries below: with no
		// workdir configured, `workdir` is the one knob that fixes both this and the
		// state dir moving with whatever directory doctor was run from.
		if cfg.WorkDirIsImplicit() {
			c.info = append(c.info, "workdir is not configured - it was derived from "+cfg.WorkDir+", so set workdir too, or the state dir moves with the directory you run from")
		}
		return c
	}

	// The state dir moved out of the workdir in CLA-259. A leftover one in the old
	// place is worth a WARN on both counts an operator cares about: markers they
	// touch there do nothing, and the transcripts already in it are sitting inside
	// a repo an unattended agent commits from.
	if legacy := cfg.LegacyStateDir(); legacy != "" {
		c.status = warn
		c.detail = stateDir + " writable; " + legacy + " is a leftover from before the state dir moved out of the workdir"
		c.remedy = "markers there are ignored now — move anything you want out of it, then: rm -rf " + legacy
		return c
	}

	c.status = pass
	c.detail = stateDir + " writable, no stop markers"
	// The state dir's name is a hash of the workdir, so when the workdir itself
	// came from the cwd, this line is about a directory that MOVES with where the
	// operator happened to be standing: doctor from ~ and `run` from the checkout
	// then talk about two different directories, and doctor's "no stop markers" is
	// true of one the loop never opens. Say which directory it resolved from.
	if cfg.WorkDirIsImplicit() {
		c.detail += " (workdir not configured - derived from " + cfg.WorkDir + ")"
		c.remedy = "set workdir so the state dir does not move with the directory you run from"
	}
	return c
}

// workDirMatch is the configured workdir a state dir was found at or under. dir
// is "" when it is outside every one of them.
//
// session says whether the loop actually SPAWNS sessions in that directory, and
// the two cases cost different things - see stateDirInWorkDir. Reporting the
// wrong one is not a wording detail: telling an operator a session can write
// their state dir when no session runs there is a claim they can check and
// disprove, which is how a warning gets skimmed past.
// outer is the OUTERMOST containing workdir, which is what a remedy has to name:
// every match is an ancestor of the same state dir, so they form a chain, and
// "point it outside ~/dev/acme" is satisfied by ~/dev/state - which warns again.
type workDirMatch struct {
	dir     string
	session bool
	outer   string
}

// containingWorkDir finds the configured workdir the state dir sits at or under.
//
// EVERY configured workdir is considered, not just the ones sessions run in: the
// top-level `workdir` and each project's (resolved with the same fallback the
// loop uses). A `projects[]` entry that omits `workdir` inherits the top-level
// one, so a state dir inside it is a capability waiting for the next config edit
// even when nothing is spawned there today.
//
// A session workdir wins over a merely-configured one, and the DEEPEST match wins
// within each group: for `workdir: ~/dev` with `projects[0].workdir: ~/dev/acme`,
// the answer an operator can act on is ~/dev/acme, the directory their sessions
// are really in.
//
// Both a lexical and a symlink-resolved comparison are made, and either one
// counts. The lexical pass is what an operator can check against their own config
// by eye; the resolved pass catches the case where two different-looking paths are
// the same directory - /var vs /private/var on macOS, or a workdir reached through
// a symlinked parent. Resolution is best-effort: an unresolvable path falls back
// to its lexical form rather than dropping the comparison. Both passes are
// CASE-SENSITIVE, so on a case-insensitive filesystem `~/Dev` against `~/dev` is a
// known miss - the same lexical assumption config and statedir already make, and
// a miss here costs a warning rather than a guarantee.
func containingWorkDir(stateDir string, cfg *config.Config) workDirMatch {
	sessions := make(map[string]bool)
	for _, d := range cfg.SessionWorkDirs() {
		sessions[d] = true
	}

	var best, bestConfigured, outer string
	for _, wd := range configuredWorkDirs(cfg) {
		if !pathWithin(stateDir, wd) && !pathWithin(resolvePath(stateDir), resolvePath(wd)) {
			continue
		}
		// Longest path == deepest directory, since every candidate is absolute and
		// cleaned and they are all ancestors of the same state dir.
		if sessions[wd] {
			if len(wd) > len(best) {
				best = wd
			}
		} else if len(wd) > len(bestConfigured) {
			bestConfigured = wd
		}
		if outer == "" || len(wd) < len(outer) {
			outer = wd
		}
	}
	if best != "" {
		return workDirMatch{dir: best, session: true, outer: outer}
	}
	return workDirMatch{dir: bestConfigured, outer: outer}
}

// stateDirInWorkDir words the in-workdir warning. Two populations, because they
// are owed different sentences: a SESSION workdir is a live capability, while a
// configured-but-unused one is a trap set for whoever next adds a `projects[]`
// entry without a workdir of its own.
//
// The remedy is the half that has to survive being FOLLOWED, so it is checked
// against the config rather than assumed. "Remove state_dir" is only offered when
// the default would actually land outside - a workdir of ~ contains
// ~/.local/state, so for those operators removing it changes the path and not the
// warning - and it names the OUTERMOST containing workdir, since moving just
// outside the innermost one lands in the next one out.
func stateDirInWorkDir(stateDir string, m workDirMatch, cfg *config.Config) (detail, remedy string) {
	if m.session {
		detail = stateDir + " is inside the session workdir " + m.dir + ": a session spawned there can write the loop's own STOP/HALT markers"
	} else {
		detail = stateDir + " is inside the configured workdir " + m.dir + ": no session runs there today, but a projects[] entry that omits workdir inherits it, and every session it spawns could then write the loop's STOP/HALT markers"
	}

	switch {
	case cfg.StateDir != "" && defaultStateDirIsOutside(cfg):
		remedy = "remove state_dir to take the default outside the workdir, or point it somewhere outside " + m.outer
	case cfg.StateDir != "":
		remedy = "point state_dir somewhere outside " + m.outer + " - removing it will not help, the default lands inside that workdir too"
	default:
		remedy = "set state_dir to a directory outside " + m.outer + " - the default lives under your home, which is inside the workdir here"
	}
	return detail, remedy
}

// defaultStateDirIsOutside reports whether dropping an explicit state_dir would
// actually move the state dir out of every workdir. Answering it by resolving the
// default is the only honest way: the default is under the state home, which an
// outer-enough workdir (~, or the cwd a workdir was derived from) contains.
func defaultStateDirIsOutside(cfg *config.Config) bool {
	def := *cfg
	def.StateDir = ""
	dir, err := def.ResolveStateDir()
	if err != nil {
		return false // cannot say it is outside, so do not advise it
	}
	return containingWorkDir(dir, cfg).dir == ""
}

// configuredWorkDirs is every directory this config names as a workdir, absolute,
// cleaned and de-duplicated.
//
// An empty value is resolved rather than skipped, matching config.absWorkDir and
// config.SessionWorkDirs: an unset workdir means the cwd everywhere else in the
// tool, and skipping it here would make the containment check silently answer
// about fewer directories than the loop uses.
func configuredWorkDirs(cfg *config.Config) []string {
	dirs := []string{cfg.WorkDir}
	for _, p := range cfg.Projects {
		dirs = append(dirs, projectWorkDir(cfg, p))
	}

	out := make([]string, 0, len(dirs))
	seen := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		abs, err := filepath.Abs(d)
		if err != nil || seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	return out
}

// pathWithin reports whether path is dir itself or sits under it, COMPONENT-WISE.
// A string prefix would say yes to /a/workdir-2 against /a/workdir, which is a
// different directory a session never touches - the false positive that would
// teach operators to skim past this warning.
func pathWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false // different volumes, or one is relative: not containment
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolvePath is EvalSymlinks that never fails: an unresolvable path (it does not
// exist yet, or a component is not readable) comes back as it went in, so the
// caller compares something rather than nothing.
func resolvePath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// randomTail is an unguessable filename suffix. See loop.randomTail — the same
// reasoning, for the probe this check writes.
func randomTail() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
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
// checkSessions reports on each session the loop will spawn: its workdir, and
// what the harness running there makes of the MCP config it will be handed.
//
// One check per project PER HARNESS (CLA-366), because both axes change the
// answer: the project decides which file (its `/mcp/<slug>`) and the harness
// decides what that file MEANS — a Claude-shaped `.mcp.json` is the session's
// tools for one adapter and a refusal to start for another. Qualified names only
// when a second harness is in play, so a single-harness report is unchanged.
func checkSessions(cfg *config.Config) []check {
	harnesses := cfg.PhaseHarnesses()
	suffix := func(h string) string {
		if len(harnesses) == 1 {
			return ""
		}
		return ":" + h
	}
	if len(cfg.Projects) == 0 {
		out := make([]check, 0, len(harnesses))
		for _, h := range harnesses {
			out = append(out, sessionCheck("workdir"+bracket(suffix(h)), cfg.WorkDir, cfg.ResolveMCPConfig(h, "", nil), h))
		}
		return out
	}
	out := make([]check, 0, len(cfg.Projects)*len(harnesses))
	for _, p := range cfg.Projects {
		for _, h := range harnesses {
			out = append(out, sessionCheck("workdir["+p.Slug+suffix(h)+"]", projectWorkDir(cfg, p),
				cfg.ResolveMCPConfig(h, projectMCPConfig(cfg, p), p.MCPConfigPaths), h))
		}
	}
	return out
}

// bracket wraps a non-empty qualifier in the [] the check names use, and leaves
// an empty one alone — so "workdir" stays "workdir" on a single-harness run.
func bracket(s string) string {
	if s == "" {
		return ""
	}
	return "[" + strings.TrimPrefix(s, ":") + "]"
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

// mcpConfigIsClaudeShaped reports whether the file at path declares Claude's
// `mcpServers` block - the shape config.Validate auto-discovers from a workdir's
// `.mcp.json`.
//
// It answers only on POSITIVE evidence. An empty path, an unreadable file or one
// that will not parse all return false, because each of those is already someone
// else's check (config.Validate fails closed on an unparseable MCP config) and a
// preflight that guesses at a file it could not read is how this arm went wrong in
// the first place.
func mcpConfigIsClaudeShaped(path string) bool {
	if path == "" {
		return false
	}
	// Validate has already expanded a leading ~, so this is the same path the
	// harness will be handed.
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return false
	}
	_, ok := top["mcpServers"]
	return ok
}

// mcpConfigNotCheckedNote is the workdir check's stand-in for a verdict on the
// `.mcp.json`, printed on every status it can still reach - so a PASS never reads
// as "the clankerbar wiring is there". The harness supplies the second half,
// because where a session really gets its MCP servers is a fact about the adapter
// and belongs next to the code that proves it.
func mcpConfigNotCheckedNote(use harness.MCPConfigUse) string {
	return ".mcp.json not checked: " + use.Note
}

func sessionCheck(name, dir, mcpConfigPath, harnessName string) check {
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

	// The `.mcp.json` arm, and what the configured harness makes of that file.
	//
	// The question this used to ask was "does the adapter HAND the path to the
	// session", and that question certifies opencode, which does hand it over and
	// then dies on it. The question it asks now is the one the check actually rests
	// on: does the file MEAN anything to this harness. Only the adapter knows, so
	// only the adapter is asked (harness.Adapter.MCPConfigUse) - see CLA-263.
	adapter, err := harness.Get(harnessName)
	if err != nil {
		// config.Validate rejects an unregistered harness long before doctor runs,
		// so this is all but unreachable in practice. It refuses to guess anyway:
		// assuming a default for an unknown harness is the precise shape of the bug
		// this arm is being fixed for.
		c.status = warn
		c.detail = workdirLabel(dir) + ": " + err.Error()
		c.remedy = "set harness to one of: " + strings.Join(harness.Names(), ", ")
		return c
	}
	use := adapter.MCPConfigUse()

	switch use.Schema {
	case harness.MCPConfigUnused:
		// The file is inert for this harness. Say the exclusion out loud and carry
		// it onto whatever verdict follows, rather than reporting on it either way.
		c.info = append(c.info, mcpConfigNotCheckedNote(use))

	case harness.MCPConfigNative:
		// The path IS handed over, but read in the harness's own schema. A
		// Claude-shaped file here is not "no clankerbar tools", it is a config the
		// harness refuses to start on - every iteration dies at spawn, forever. That
		// is strictly worse than the missing-file case below, so it FAILs rather
		// than WARNs: a WARN is advice, and there is no run to advise about.
		if mcpConfigIsClaudeShaped(mcpConfigPath) {
			c.status = fail
			c.detail = mcpConfigPath + " is a Claude-shaped .mcp.json (`mcpServers`), which " + harnessName + " cannot read"
			// Deliberately not "or unset it": an empty mcp_config_path re-runs the
			// <workdir>/.mcp.json discovery (config.discoverMCPConfig), which is
			// harness-blind, so removing the setting hands the same file over again.
			// Naming a config the harness can read is the only remedy that works.
			c.remedy = "point mcp_config_path at a " + harnessName + " config - " + use.Note
			return c
		}
		// Otherwise there is nothing here we can honestly verdict on: an absent
		// path is normal for this harness, and a present non-Claude one we have no
		// schema to judge. An absent one is NOT warned about here even though a
		// mixed-harness phase could reach the backlog through neither this file
		// nor its config dir (CLA-366): the config dir legitimately carries the
		// server for this harness, doctor cannot parse that schema to find out,
		// and TestSessionCheckDoesNotClaimMCPWiringForOpencode pins the resulting
		// PASS-with-caveat deliberately. The guard for a phase harness that
		// declares NOTHING is in config.Validate, which refuses an empty
		// `harnesses.<name>` block outright - the right place for it, since it
		// fires before a session is spawned rather than in a log at 3am.
		c.info = append(c.info, mcpConfigNotCheckedNote(use))

	case harness.MCPConfigClaudeJSON:
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

	default:
		// A schema added to harness but not taught to doctor. Go will not make this
		// a compile error, so it is made LOUD instead of silent - the whole point of
		// CLA-263 is that this arm must never pass a workdir on a premise it has not
		// checked.
		c.status = warn
		c.detail = workdirLabel(dir) + ": doctor cannot judge mcp_config_path for harness " + harnessName
		c.remedy = "teach sessionCheck this harness's MCPConfigUse schema before trusting a green workdir here"
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

// --- 7. mcp servers ----------------------------------------------------------

// checkMCPServers names the MCP entries that start a LOCAL PROCESS in every
// spawned session.
//
// `mcp_config_path` defaults to `<workdir>/.mcp.json` - a file inside a checkout,
// which the sessions themselves can write - and the harness starts every server
// it declares at MCP init, before any allow/deny rule is consulted. CLA-257
// constrains where that file may send the API key; nothing constrains what it may
// RUN, and nothing said so out loud. This says so.
//
// A WARN, never a FAIL: a local MCP server in a repo's .mcp.json is a normal,
// wanted thing, and failing the preflight over one would block runs that work.
// What an operator needs is to have seen the list once.
func checkMCPServers(cfg *config.Config) check {
	c := check{name: "mcp_servers"}
	local := cfg.LocalMCPServers()
	if len(local) == 0 {
		c.status = pass
		c.detail = "no MCP server starts a local process"
		return c
	}
	c.status = warn
	c.detail = plural(len(local), "1 MCP server starts a local process", fmt.Sprintf("%d MCP servers start local processes", len(local))) + " in every session"
	for _, s := range local {
		c.info = append(c.info, s.Name+": "+truncate(s.Command, 80)+"  ("+s.ConfigPath+")")
	}
	c.remedy = "confirm you meant each of these - they run at session start, before any permission rule applies, and a checkout's .mcp.json can declare them"
	return c
}

// --- 8. permission policy ----------------------------------------------------

// checkPermissions reports on the run harness's permission policy. See
// checkPermissionsAll, which is what doctorChecks calls.
func checkPermissions(cfg *config.Config) check {
	return checkPermissionsNamed(cfg, "permissions", cfg.Harness)
}

// checkPermissionsAll reports on the policy of every harness this config will
// declares (CLA-366). The policy is per harness in both senses — a different
// mechanism each (a settings file, a sandbox flag, an env var) and a different
// file — so a mixed run that checked only `harness` would certify the policy of
// one session and say nothing about the other.
func checkPermissionsAll(cfg *config.Config) []check {
	names := cfg.PhaseHarnesses()
	out := make([]check, 0, len(names))
	for i, n := range names {
		label := "permissions"
		if i > 0 {
			label = "permissions[" + n + "]"
		}
		out = append(out, checkPermissionsNamed(cfg, label, n))
	}
	return out
}

func checkPermissionsNamed(cfg *config.Config, label, harnessName string) check {
	c := check{name: label}
	settingsPath := cfg.SessionFor(harnessName).SettingsPath
	switch harnessName {
	case "claude":
		if settingsPath == "" {
			c.status = warn
			c.detail = "no settings_path — the unattended session runs on the ambient allowlist"
			c.remedy = "set settings_path to a headless permission policy (its deny rules win over the config dir's)"
			return c
		}
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			c.status = fail
			c.detail = settingsPath + " is unreadable: " + err.Error()
			c.remedy = "create the settings file, or clear settings_path"
			return c
		}
		if !json.Valid(data) {
			// Claude rejects an unparseable --settings file at startup, so this kills
			// the first session rather than degrading it.
			c.status = fail
			c.detail = settingsPath + " is not valid JSON"
			c.remedy = "fix the JSON — the harness refuses to start with an unparseable --settings file"
			return c
		}
		c.status = pass
		c.detail = settingsPath + " parses"

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
		c.detail = "no permission-policy checks for " + harnessName
	}
	return c
}

// --- 9. toolchain grants -----------------------------------------------------

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
// runsHarness reports whether the named harness will actually START A SESSION in
// this config. SpawnedHarnesses, not PhaseHarnesses: a run-wide `harness` that
// every phase overrides is declared and never spawned, and a check reasoning
// about what the run DOES must not count it.
func runsHarness(cfg *config.Config, name string) bool {
	for _, h := range cfg.SpawnedHarnesses() {
		if h == name {
			return true
		}
	}
	return false
}

// costBlindHarnesses names every harness this run spawns that cannot report what
// a session cost, in the order SpawnedHarnesses lists them. Empty means a cost
// ceiling can see the whole run.
//
// SPAWNED, not declared: a never-spawned cost-blind harness would make the run's
// live `max_cost_usd` report as INERT and send the operator to fix a harness no
// session ever runs on.
func costBlindHarnesses(cfg *config.Config) []string {
	var out []string
	for _, h := range cfg.SpawnedHarnesses() {
		if !harnessReportsCost(h) {
			out = append(out, h)
		}
	}
	return out
}

func checkToolchains(cfg *config.Config) check {
	c := check{name: "toolchains", status: pass}
	// The allowlist is Claude's, but claude need not be the RUN's harness: a
	// mixed sequence can implement elsewhere and review on claude, and it is the
	// claude session that runs the verification verbs this check is about
	// (CLA-366). So ask whether claude runs any phase at all, and audit the
	// settings file THAT session gets.
	if !runsHarness(cfg, "claude") {
		// SpawnedHarnesses, to agree with the gate directly above it. Listing
		// PhaseHarnesses here named the DECLARED harness too, so a config whose
		// phases all override it printed "no allowlist to audit for claude,
		// opencode" - a sentence naming claude among the harnesses it has just
		// said have no claude allowlist.
		c.detail = "no allowlist to audit for " + strings.Join(cfg.SpawnedHarnesses(), ", ")
		return c
	}
	settingsPath := cfg.SessionFor("claude").SettingsPath

	needed := detectToolchains(cfg)
	if len(needed) == 0 {
		c.detail = "no unambiguous build toolchains detected in the session workdirs"
		return c
	}
	tools := make([]string, 0, len(needed))
	for _, t := range needed {
		tools = append(tools, t.tool)
	}

	if settingsPath == "" {
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
		c.remedy = "remove the deny rule in " + settingsPath + ", or accept that tasks in those repos cannot be verified"
	case len(missing) > 0:
		c.status = warn
		c.detail = "no grant for: " + strings.Join(missing, ", ")
		c.remedy = "allow the verbs each one needs in " + settingsPath +
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
	// The claude SESSION's fields, not the run's: in a mixed sequence claude may
	// be a phase rather than the top-level harness, and then the policy that
	// gates it is the one in its own block (CLA-366). SessionFor collapses to the
	// run-wide fields whenever claude IS the top-level harness, which is every
	// single-harness config.
	hc := cfg.SessionFor("claude")
	paths := []string{hc.SettingsPath}
	if hc.ConfigDir != "" {
		paths = append(paths,
			filepath.Join(hc.ConfigDir, "settings.json"),
			filepath.Join(hc.ConfigDir, "settings.local.json"),
		)
	}
	// config owns this list: it is the confinement boundary, and statedir.Open is
	// handed the same one. Two copies of "where do sessions run" is exactly the
	// drift projectWorkDir exists to prevent.
	for _, dir := range cfg.SessionWorkDirs() {
		paths = append(paths,
			filepath.Join(dir, ".claude", "settings.json"),
			filepath.Join(dir, ".claude", "settings.local.json"),
		)
	}
	return paths
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

// --- 10. power ----------------------------------------------------------------

// unknownSleepRemedy is shared by both ways doctor can fail to learn the sleep
// policy — `pmset -g` not running at all, and running but reporting no
// idle-sleep field. The two states are the same state, so they say the same
// thing; keeping one string means a future edit cannot improve the advice for
// one of them and leave the other behind. It has to be actionable for a reader
// who has just been told the command doctor would send them to did not answer,
// so it names the mitigation as well as the manual check.
const unknownSleepRemedy = "check manually with `pmset -g` — if idle sleep is enabled, an unattended run will freeze mid-wait. Either way, `clankerbar run` holds a no-idle-sleep assertion for the length of the run; for any other invocation use `caffeinate -i …`"

// checkPower answers the most basic precondition of an unattended run, and the
// one nothing else checks: will this machine still be awake to do the work?
//
// Timers run on the monotonic clock, which does not advance while a machine is
// suspended. So idle sleep does not pause a run, it FREEZES it — mid-wait,
// silently, in a way that looks exactly like a hang. A real run lost 5h31m of a
// 10h window to this, waking only for 45-second Power Nap bursts.
//
// Two facts decide it: whether anything holds a no-idle-sleep assertion, and what
// the active power source's sleep timeout is. `clankerbar run` now takes an
// assertion itself, so a green line here usually means that is working — but
// doctor is also run standalone, before any loop exists, which is exactly when
// the answer is worth having.
func checkPower(ctx context.Context, e doctorEnv) check {
	c := check{name: "power"}
	if e.goos != "darwin" {
		c.status = pass
		c.detail = "no sleep-policy checks for " + e.goos
		return c
	}

	if out, err := e.pmset(ctx, "-g", "assertions"); err == nil && holdsNoIdleSleep(out) {
		c.status = pass
		// Point-in-time, and said as such: assertions are commonly short-lived (the
		// `caffeinate` default is 300 seconds), so one held while doctor runs is not
		// proof one will be held all night. `clankerbar run` takes its own, tied to
		// its pid, which is the only kind that covers a whole run.
		c.detail = "a no-idle-sleep assertion is held right now — note assertions can be short-lived; `run` takes its own for the length of the run"
		return c
	}

	out, err := e.pmset(ctx, "-g")
	if err != nil {
		// Not being able to ask is not evidence of a problem, and a FAIL here would
		// block a cron gate over a missing binary.
		c.status = warn
		c.detail = "could not read the sleep settings: " + err.Error()
		c.remedy = unknownSleepRemedy
		return c
	}
	mins, found := idleSleepMinutes(out)
	switch {
	case !found:
		// Informationally identical to the read failing outright: the command
		// exited zero, but no idle-sleep field came back, so doctor does not know
		// the answer. A renamed, re-cased or locale-shifted field, or a VM whose
		// `pmset` omits it, used to render a green line on the exact question this
		// check exists to answer. Failing open on the unknown case is worse than
		// not checking, because the operator gets a green line telling them not to
		// look. Note what the WARN does and does not buy: only `fail` stops
		// `doctor && run`, so the documented cron gate opens either way — what
		// changes is that a human reading the output has a line worth reading. Do
		// not "restore consistency" by making this a FAIL; that would block the
		// gate over a machine that may well be fine, which is why the read-failure
		// branch above is a WARN too.
		c.status = warn
		c.detail = "no idle-sleep timeout reported for the active power source, so the sleep policy is unknown"
		c.remedy = unknownSleepRemedy
	case mins == 0:
		c.status = pass
		c.detail = "idle sleep is disabled for the active power source (a closed lid still sleeps)"
	default:
		c.status = warn
		c.detail = fmt.Sprintf("the machine idle-sleeps after %d min on the active power source, and nothing holds an assertion", mins)
		c.remedy = "`clankerbar run` holds one itself; for any other invocation use `caffeinate -i …`. Start the run on AC — plugging in later does NOT wake a sleeping Mac, and a closed lid sleeps regardless of either"
	}
	return c
}

// holdsNoIdleSleep reports whether `pmset -g assertions` shows a live
// PreventUserIdleSystemSleep. Only two line shapes count as evidence, because
// they are the only two pmset actually emits for this assertion (verified against
// live output on 2026-08-22):
//
//   - the summary row, whose FIRST field is exactly the assertion name and whose
//     SECOND field is an integer count — held only when that count is non-zero;
//   - a per-process detail line under "Listed by owning process:", which begins
//     `pid <n>(<proc>):` and names the holder — held whenever one appears.
//
// Anything else mentioning the name is NOT PROVEN HELD and falls through, so
// checkPower goes on to the `pmset -g` settings read rather than reporting PASS
// from a shape it does not recognise. Falling through cannot invent a problem —
// it only defers to the more direct question — whereas the previous test ("the
// last token is not an integer means a detail line") answered YES, held for every
// unrecognised shape, including Apple appending a token to the summary row or a
// locale shifting its number format (CLA-306).
func holdsNoIdleSleep(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "PreventUserIdleSystemSleep") {
			continue
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) >= 2 && fields[0] == "PreventUserIdleSystemSleep":
			// Summary-row shape. Parse the COUNT position, not the tail: trailing
			// tokens are exactly the unknown we must not guess from. A count here
			// that will not parse is an unrecognised variant of the row — fall
			// through rather than answer either way.
			if n, err := strconv.Atoi(fields[1]); err == nil && n > 0 {
				return true
			}
		case len(fields) >= 2 && fields[0] == "pid" && strings.HasSuffix(fields[1], ":") && strings.Contains(fields[1], "("):
			// Per-process detail line: `pid 81237(caffeinate): … named: "…"`. Its
			// presence means a live process holds the assertion.
			return true
		}
	}
	return false
}

// idleSleepMinutes pulls the `sleep` timeout out of `pmset -g`, which reports the
// settings currently in force for whichever power source is active — the right
// question, since battery and AC usually differ sharply.
func idleSleepMinutes(out string) (int, bool) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "sleep" {
			continue
		}
		if n, err := strconv.Atoi(fields[1]); err == nil {
			return n, true
		}
	}
	return 0, false
}

// --- 11. budget --------------------------------------------------------------

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
	// The same reading of a negative, one level down: a per-harness block's dials
	// are guarded with `> 0` too (config.HarnessBudget.ExceededBy), so a negative
	// there reads as a ceiling and is none. Sorted so the finding does not depend
	// on map iteration order.
	for _, name := range slices.Sorted(maps.Keys(b.PerHarness)) {
		hb := b.PerHarness[name]
		if hb.MaxTokens < 0 {
			bad = append(bad, fmt.Sprintf("per_harness[%s].max_tokens", name))
		}
		if hb.MaxCostUSD < 0 {
			bad = append(bad, fmt.Sprintf("per_harness[%s].max_cost_usd", name))
		}
	}
	if len(bad) > 0 {
		c.status = fail
		c.detail = "negative budget " + plural(len(bad), "value", "values") + ": " + strings.Join(bad, ", ")
		c.remedy = "use a positive ceiling; 0 disables that dimension"
		return c
	}

	// A cost ceiling on a harness that never reports cost is not a weak ceiling
	// but an ABSENT one: no code path can reach it, because nothing populates the
	// figure it compares against (codex exec reports tokens, not money). So every
	// verdict below reasons about the ceilings that can actually FIRE, not the ones
	// written down - otherwise a codex run whose only dial is cost reports a set
	// budget while having none, and a codex run with cost plus wall clock escapes
	// the wall-clock-only warning that in reality describes it exactly (CLA-288).
	// ANY harness the run spawns that cannot report cost makes a cost ceiling
	// unreliable, not just the top-level one: in a mixed sequence a share of the
	// run's spend never reaches the accumulator the ceiling reads, so a dial
	// reported as live would be holding a line it cannot see past (CLA-366).
	costBlind := costBlindHarnesses(cfg)
	inertCost := b.MaxCostUSD > 0 && len(costBlind) > 0
	costLive := b.MaxCostUSD > 0 && !inertCost

	var set []string
	if b.MaxTokens > 0 {
		set = append(set, fmt.Sprintf("max_tokens=%d", b.MaxTokens))
	}
	if b.MaxCostUSD > 0 {
		entry := fmt.Sprintf("max_cost_usd=%g", b.MaxCostUSD)
		if inertCost {
			// Annotated wherever it appears, not only when it is the last ceiling
			// standing: a dial that does nothing is worth saying out loud even
			// when something else is holding the line.
			entry += fmt.Sprintf(" (INERT: the %s harness never reports cost)", strings.Join(costBlind, "/"))
		}
		set = append(set, entry)
	}
	if b.MaxWallClock > 0 {
		set = append(set, "max_wall_clock="+b.MaxWallClock.Duration().String())
	}

	// Per-harness blocks (CLA-367), read the same way: what can FIRE, not what is
	// written down. Two ways one cannot. Its cost dial may name a harness that
	// never reports cost - the case above, one level down. Or the block may name a
	// harness THIS config never spawns, whose ceilings are then unreachable however
	// sane the numbers are. Both are annotated in place, and neither counts toward
	// the "there is a live ceiling" reasoning below.
	// "Does this run SPAWN it" - not "is it THE harness". A phase names its own
	// harness since CLA-366, so a block for the harness the implement phase runs on
	// is perfectly reachable while not being cfg.Harness, and calling it inert would
	// be a false statement about a ceiling that will fire.
	spawned := cfg.SpawnedHarnesses()
	// The spawned harness whose block sets a cost dial and NOTHING else - under a
	// harness that never reports cost that block is a ceiling in name only, and the
	// remedy has to name the unit rather than the placement. "" when no spawned
	// harness is in that state.
	costOnlyInertHarness := ""
	// Whether any spawned harness's block carries a token ceiling, for the
	// enforced-between-sessions warning further down.
	spawnedBlockTokens := false
	for _, name := range spawned {
		hb, _ := b.ForHarness(name)
		if hb.MaxTokens > 0 {
			spawnedBlockTokens = true
		}
		if costOnlyInertHarness == "" && hb.MaxCostUSD > 0 && hb.MaxTokens == 0 && !harnessReportsCost(name) {
			costOnlyInertHarness = name
		}
	}
	perHarnessLive := false
	for _, name := range slices.Sorted(maps.Keys(b.PerHarness)) {
		hb := b.PerHarness[name]
		if !hb.CountsSpend() {
			// A block with no dial at all - `"claude": {}`, or a mistyped key,
			// since the config decoder ignores fields it does not know. Silence
			// would leave an operator staring at a ceiling they wrote and doctor
			// never mentioned, so it is named as the nothing it is.
			set = append(set, fmt.Sprintf("per_harness[%s]: no ceiling set (INERT)", name))
			continue
		}
		var dials []string
		tokensLive := hb.MaxTokens > 0
		costFires := hb.MaxCostUSD > 0 && harnessReportsCost(name)
		if hb.MaxTokens > 0 {
			dials = append(dials, fmt.Sprintf("max_tokens=%d", hb.MaxTokens))
		}
		if hb.MaxCostUSD > 0 {
			dial := fmt.Sprintf("max_cost_usd=%g", hb.MaxCostUSD)
			if !costFires {
				dial += " (INERT: never reports cost)"
			}
			dials = append(dials, dial)
		}
		entry := fmt.Sprintf("per_harness[%s]: %s", name, strings.Join(dials, ", "))
		runs := runsHarness(cfg, name)
		if !runs {
			entry += fmt.Sprintf(" (INERT: this config runs %s)", strings.Join(spawned, " / "))
		}
		set = append(set, entry)
		if runs && (tokensLive || costFires) {
			perHarnessLive = true
		}
	}
	// Every ceiling this run has that can actually fire. The branches below ask
	// this rather than "is anything configured", because a config whose only
	// ceilings are inert has none at all - the CLA-288 reading, now including the
	// two ways a per-harness block can be inert.
	anyLive := b.MaxTokens > 0 || costLive || b.MaxWallClock > 0 || perHarnessLive

	// The guards that were silently absent from the config that ran a 285.9M
	// single session (CLA-344): doctor's job is preflight, so the dials most
	// likely to be missing are the ones it should name. Every line describes
	// REAL behaviour — a reassuring falsehood is the exact defect CLA-290
	// removed from the no-ceiling detail, and the same bar applies here.
	var guardNotes []string
	var guardDials []string
	// Turn cap: since CLA-343 the effective CLAUDE config ALWAYS resolves one —
	// the operator's, else the built-in default — so "no turn cap" cannot
	// happen; what can happen is that the cap is the DEFAULT, a runaway
	// detector tuned to the largest measured session, not a budget the
	// operator chose. The codex adapter has NO turn cap at all (its
	// Invocation.MaxTurns never reaches the CLI), so for it the warning is
	// skipped rather than claiming a guard that does not exist there.
	// runsHarness, not cfg.Harness == "claude": on a mixed sequence the claude phase
	// is bounded by the built-in default turn cap exactly as a claude-only run is
	// (CLA-366), and the opencode phase beside it does not make that untrue.
	if runsHarness(cfg, "claude") && anyPhaseRunsTheDefaultTurnCap(cfg) {
		guardNotes = append(guardNotes, fmt.Sprintf(
			"max_turns: at the built-in default (%d turns): a runaway detector, not a budget, so a deep task that reaches it is cut off at the phase boundary (the salvage commits what it left); set max_turns (or a phase's) to tune",
			config.DefaultMaxTurns))
		guardDials = append(guardDials, "max_turns")
	}
	// A session wall-clock cap the configured harness never enforces is the same
	// defect max_cost_usd-under-codex is (CLA-288): a dial the operator set as
	// their backstop, doing nothing, discoverable only from a session that ran
	// all night. Named here, before the run, rather than left to be inferred.
	if dial, on := inertSessionWallClock(cfg); dial != "" {
		// The advice rides in the NOTE, not in c.remedy: the remedy line is one
		// per check and the guard block's generic "set <dials>" usually claims it,
		// so advice parked there is advice an operator routinely never sees. The
		// tail clause is gated on the harness actually HAVING a turn cap: under
		// codex MaxTurns never reaches the CLI either, so telling a codex operator
		// the turn cap is their live backstop is advice that WOULD BE FOLLOWED
		// straight back into the inert-dial hole this note exists to warn about
		// (the reasoning harnessReportsCost uses below).
		// `on`, not cfg.Harness: the dial is inert on the harness of the PHASE that
		// would have used it (CLA-366), which on a mixed sequence is routinely not
		// the run-wide one - and the backstop advice that follows is that harness's
		// too, so naming the wrong one is advice that would be FOLLOWED wrongly.
		backstop := "under " + on + " the turn cap is the backstop that does fire"
		if !harnessHonoursMaxTurns(on) {
			backstop = "under " + on + " there is NO per-session backstop at all - bound the run with max_iterations / max_tokens instead"
		}
		guardNotes = append(guardNotes, fmt.Sprintf(
			"%s: set, but harness %q does not enforce a per-session wall-clock cap, so it is INERT there - drop it or run the harness that enforces it (opencode); %s",
			dial, on, backstop))
	}
	// The complement, and the sharper case: the harness CAN enforce a session cap,
	// takes no turn flag, has no mid-stream token ceiling — and the dial is unset.
	// That run's sessions are bounded by nothing at all, which is the silently
	// absent guard CLA-344 added this whole block for.
	if on := noSessionBackstop(cfg); on != "" {
		guardNotes = append(guardNotes, fmt.Sprintf(
			"max_session_wall_clock: unset, and harness %q has no turn cap and no mid-session token ceiling - so nothing bounds a single session there; a run that works past its brief is stopped only by a run-level ceiling",
			on))
		guardDials = append(guardDials, "max_session_wall_clock")
	}
	if cfg.MaxRetries == 0 {
		guardNotes = append(guardNotes,
			fmt.Sprintf("max_retries: 0 - transient failures are retried forever (backoff capped at retry_cap), each retry a fresh paid session redoing the task; set a positive max_retries to bound a run window. Only a run of attempts reporting NO usage is bounded regardless, at max_zero_spend_attempts=%d", cfg.ZeroSpendAttemptBound()))
		guardDials = append(guardDials, "max_retries")
	}
	if cfg.MaxIterations == 0 {
		guardNotes = append(guardNotes,
			"max_iterations: 0 — no session cap: the loop stops only on a STOP/HALT marker, a signal or a budget ceiling (a dry backlog idle-polls rather than exiting); set max_iterations to bound a run window")
		guardDials = append(guardDials, "max_iterations")
	}

	c.status = pass
	if len(set) == 0 {
		// The wording names what actually STOPS the loop, because the obvious
		// sentence — "runs until the backlog is dry" — is false: a dry backlog
		// idle-polls by design, so the daemon can react to answered questions,
		// promotions and newly filed work (CLA-290). `--max-iterations` is a run
		// flag doctor cannot see, so the config's contribution is described alone.
		c.detail = "no ceiling configured — the loop stops on a STOP/HALT marker or signal; a dry backlog idle-polls rather than exiting"
		// The per-session runaway ceiling still applies whatever the run-level
		// dials say, and the operator should know the number (CLA-343). Asked of
		// the harnesses that RUN A PHASE rather than the run-wide name (CLA-366):
		// a sequence implementing on opencode and reviewing on claude still has the
		// ceiling active on its claude phase, and reporting on cfg.Harness alone
		// would either hide that or claim it for a phase where TokenCeilingHit
		// never fires. Harnesses with no mid-session ceiling are omitted, not
		// listed as zero - claiming one is "still active" there would be false.
		if bounds := sessionTokenBounds(cfg); bounds != "" {
			c.info = append(c.info, "per-session runaway ceiling still active: "+bounds+" (the operator's own, else 2x a token ceiling when set, else the floor)")
		}
	} else if b.MaxWallClock > 0 && !costLive && b.MaxTokens == 0 && !perHarnessLive {
		// Wall clock is the weakest proxy for spend of the three, because it counts
		// the hours a run spends WAITING OUT a usage limit — time in which nothing is
		// billed. A run capped at 8h can spend five of them asleep and stop having done
		// three iterations. Cost is the dial that tracks what an operator actually
		// means by "leave headroom", and it comes straight from the harness.
		c.status = warn
		c.detail = strings.Join(set, ", ") + " - wall clock is the only ceiling that can fire, and it counts time spent waiting out usage limits"
		// The usual advice is "add max_cost_usd" - which under a harness that
		// cannot report cost is advice to set a dial that does nothing, and it
		// would be FOLLOWED, landing the operator in the cost-only case the
		// sibling branch below warns about. Gated on the HARNESS, not on
		// inertCost: the commonest codex config is a bare wall clock with cost
		// unset, and that is the one that needs the corrected advice most.
		if len(costBlind) > 0 {
			c.remedy = "add max_tokens as the real ceiling; under " + strings.Join(costBlind, "/") + " max_cost_usd cannot fire, and max_wall_clock is only the outer bound on how late a run may finish"
		} else {
			c.remedy = "add max_cost_usd as the real ceiling; keep max_wall_clock as the outer bound on how late a run may finish"
		}
	} else if inertCost && b.MaxTokens == 0 && b.MaxWallClock == 0 && !perHarnessLive {
		// The sibling of the wall-clock-only warning, and the sharper case of the
		// two: wall clock at least measures something, whereas this run's every
		// configured ceiling is unreachable, so it has none at all. That cannot be
		// left to the operator to notice from a line reporting a set budget
		// (CLA-288).
		c.status = warn
		c.detail = strings.Join(set, ", ") + " - so this run has NO effective ceiling"
		// costBlind, not cfg.Harness: on a mixed sequence it is every harness the
		// run spawns that cannot report cost which makes the global dial inert, so
		// naming the run-wide harness alone would point the operator at the wrong
		// one (CLA-366).
		c.remedy = "set max_tokens (or max_wall_clock) beside it - under " + strings.Join(costBlind, "/") + " those are the dials that can fire"
	} else if !anyLive {
		// The same finding reached the other way: every ceiling written down is a
		// per-harness block this run cannot reach (a harness it never drives, or a
		// cost dial that harness never reports), so the config LOOKS bounded and is
		// not. Distinct from the branch above, which is the global cost dial's case
		// and keeps its own wording.
		c.status = warn
		c.detail = strings.Join(set, ", ") + " - none of them can fire, so this run has NO effective ceiling"
		// A remedy will be FOLLOWED, so it must not point at something the
		// operator has already done. When the running harness HAS a block and its
		// only dial is a cost one that harness never reports, the placement is
		// right and the unit is wrong - saying "add a per_harness block" there
		// would be a no-op dressed as advice.
		if costOnlyInertHarness != "" {
			c.remedy = "set per_harness[" + costOnlyInertHarness + "].max_tokens - " + costOnlyInertHarness + " reports tokens, never cost"
		} else {
			c.remedy = "put the ceiling where the run will reach it: a per_harness block for " + strings.Join(spawned, " or ") + ", or the run-wide max_tokens / max_wall_clock"
		}
	} else {
		c.detail = strings.Join(set, ", ")
		// The run-wide breaker is checked BETWEEN sessions: it cannot see a single
		// huge session coming, which is exactly what happened to the 285.9M run
		// under max_tokens=75M. Say so on the line that reports it (CLA-344).
		// Gated on a token ceiling from EITHER place: a run whose only token
		// ceiling is in its harness's per_harness block is enforced between
		// sessions in exactly the same way, so it needs the same warning and the
		// same number (CLA-367).
		if b.MaxTokens > 0 || spawnedBlockTokens {
			c.detail += " — the token ceiling is enforced BETWEEN sessions, so one session can overrun it"
			// sessionTokenBounds is EMPTY when no spawned harness has a mid-session
			// ceiling, which is every opencode-only and codex-only run. Appending it
			// unguarded printed a bare "()" there - a broken sentence in the one
			// surface whose whole job is not making sloppy or false statements. The
			// two cases need different sentences, not one sentence with a hole:
			// with a bound, name it; without, say plainly that nothing bounds the
			// overrun, which is the sharper finding and was previously unsaid.
			if bounds := sessionTokenBounds(cfg); bounds != "" {
				c.detail += " (" + bounds + ")"
			} else {
				// The singular verb is safe because this branch implies exactly ONE
				// spawned harness today: claude is the only harness with a
				// mid-session ceiling, so reaching here means claude is not among
				// the spawned. If a second such harness is ever added, make this
				// agree in number rather than letting the sentence go ungrammatical.
				c.detail += " by any amount — " + strings.Join(spawned, "/") + " enforces no mid-session token ceiling"
			}
		}
	}

	// A missing guard turns the verdict WARN (a FAIL above stays a FAIL — a
	// broken budget is worse than a bare one). The findings ride as info lines
	// so the detail line stays the budget's own story; the remedy names the
	// dials, whose specifics are one info line each — unless the branch above
	// already had a more specific one (the wall-clock-only case's "add
	// max_cost_usd" is the better advice there).
	if len(guardNotes) > 0 {
		if c.status == pass {
			c.status = warn
		}
		if c.remedy == "" && len(guardDials) > 0 {
			// Named from the dials actually warned on, so a codex run (no
			// max_turns note) is not told to set a dial that does nothing there.
			c.remedy = "set " + strings.Join(guardDials, ", ") + " (see the guard lines), or accept the defaults"
		}
		c.info = append(c.info, guardNotes...)
	}
	return c
}

// harnessReportsCost asks the registry whether this harness ever populates
// Result.CostUSD. An UNKNOWN harness is treated as reporting cost - i.e. the
// warning is withheld - because config.Validate has already refused that name, so
// the only way to get here is a doctor run over a config too broken to run at
// all, and warning about an inert dial on a harness that does not exist would bury
// the real finding under a speculative one.
func harnessReportsCost(name string) bool {
	caps, ok := harness.CapabilitiesOf(name)
	if !ok {
		return true
	}
	return caps.ReportsCost
}

// inertSessionWallClock names the wall-clock dial this config sets that will not
// be enforced, and the harness it is inert ON - two empty strings when there is
// nothing to say. The phase's own `max_wall_clock` is named in preference to the
// run-wide `max_session_wall_clock`, so an operator is pointed at the line they
// wrote.
//
// Judged PER PHASE-HARNESS since CLA-366, because that is the level at which the
// question has an answer. A phase's `max_wall_clock` is inert exactly when THAT
// phase's harness will not enforce it - so a sequence implementing on opencode
// and reviewing on claude gets the warning for its claude phase and not for the
// opencode one, where the dial does fire. The run-wide `max_session_wall_clock`
// reaches every phase, so it is only worth calling inert when NO harness the run
// spawns honours it; if one does, the dial fires somewhere and saying otherwise
// would be the reassuring falsehood this check exists to remove. Judging the
// whole config by cfg.Harness would get both of those wrong in opposite
// directions - a false INERT on a mixed run whose opencode phase honours the
// dial, and silence on a claude phase where it truly does nothing.
//
// PARTIAL inertness of the run-wide dial is deliberately NOT reported, and this
// is the one place the check chooses silence over a true statement. On the
// implement-on-opencode / review-on-claude sequence the dial fires on the opencode
// phase and is dead on the claude one - true, and not worth saying, because the
// claude phase is bounded by its turn cap, so it is not the CLA-344 "bounded by
// nothing" case that the sibling noSessionBackstop note exists for. Saying it
// anyway would put a WARN on the shipped mixed-harness config that has no defect
// and no action behind it, and a warning an operator learns to ignore costs more
// than the sentence buys. If a harness ever appears with no turn cap AND no
// session cap, noSessionBackstop is what catches it - per phase, by name.
//
// An UNKNOWN harness says nothing, for the reason harnessReportsCost gives:
// config.Validate has already refused the name, and a speculative inert-dial
// warning would bury the real finding.
func inertSessionWallClock(cfg *config.Config) (dial, on string) {
	// Phase dials first, so the operator is pointed at the line they wrote - and
	// read off cfg.Phases, NOT EffectivePhases, for exactly that reason: the
	// effective phases have the run-wide dial resolved into every one of them, so
	// iterating those would report an operator's `max_session_wall_clock` back to
	// them as `max_wall_clock (phases)`, a line they never wrote. The HARNESS still
	// comes from the resolver, since that is what a phase inherits.
	for _, ph := range cfg.Phases {
		if ph.MaxWallClock == 0 {
			continue
		}
		h := cfg.HarnessFor(ph)
		caps, ok := harness.CapabilitiesOf(h)
		if ok && !caps.HonoursSessionWallClock {
			return "max_wall_clock (phases)", h
		}
	}
	if cfg.MaxSessionWallClock == 0 {
		return "", ""
	}
	// The run-wide dial: inert only if NOTHING this run spawns enforces it. The
	// first such harness is named, since with none enforcing it they are all
	// equally the answer and naming one keeps the note short.
	named := ""
	for _, h := range cfg.SpawnedHarnesses() {
		caps, ok := harness.CapabilitiesOf(h)
		if !ok {
			return "", ""
		}
		if caps.HonoursSessionWallClock {
			return "", ""
		}
		if named == "" {
			named = h
		}
	}
	if named == "" {
		return "", ""
	}
	return "max_session_wall_clock", named
}

// harnessHonoursMaxTurns asks the registry whether this harness takes a turn
// flag at all. An UNKNOWN harness is treated as honouring one — the withheld
// advice, for the reason harnessReportsCost gives.
func harnessHonoursMaxTurns(name string) bool {
	caps, ok := harness.CapabilitiesOf(name)
	if !ok {
		return true
	}
	return caps.HonoursMaxTurns
}

// noSessionBackstop names a harness whose sessions in THIS run are bounded by
// NOTHING - it takes no turn flag and kills on no token ceiling, and the phases
// it runs leave unset the one dial that could bound it. "" when every spawned
// harness has some backstop. Gated on the harness being able to enforce the
// dial, so the advice ("set it") is advice that would work.
//
// Per phase-harness since CLA-366, and per phase within it: an unbounded
// opencode implement phase is exactly as unbounded when a claude review phase
// runs after it, so asking only about cfg.Harness would go silent on the very
// sequence this warning is for. A phase carrying its own max_wall_clock is
// bounded and does not count against its harness; the run-wide dial is resolved
// into every phase by EffectivePhases, so setting it clears all of them at once.
func noSessionBackstop(cfg *config.Config) string {
	for _, ph := range cfg.EffectivePhases() {
		if ph.MaxWallClock > 0 {
			continue
		}
		caps, ok := harness.CapabilitiesOf(ph.Harness)
		if !ok || caps.HonoursMaxTurns || !caps.HonoursSessionWallClock {
			continue
		}
		return ph.Harness
	}
	return ""
}

// sessionTokenBounds renders the per-session runaway token ceiling for every
// harness this run spawns that actually HAS one, or "" when none does.
//
// Per harness for two reasons since CLA-366. The ceiling itself is resolved per
// harness (CLA-367's SessionTokenCeilingFor reads that harness's own block), so
// one number cannot describe a mixed run; and whether the ceiling exists at all
// is a capability - codex has no mid-session ceiling, so listing it with a number
// would claim a guard that never fires. A harness without one is OMITTED rather
// than shown as zero, which is the same choice the check makes everywhere else.
func sessionTokenBounds(cfg *config.Config) string {
	var parts, only []string
	for _, h := range cfg.SpawnedHarnesses() {
		caps, ok := harness.CapabilitiesOf(h)
		if !ok || !caps.HasSessionTokenCeiling {
			continue
		}
		only = append(only, h)
		parts = append(parts, fmt.Sprintf("%s=%d", h, cfg.Budget.SessionTokenCeilingFor(h)))
	}
	if len(parts) == 0 {
		return ""
	}
	// Gated on the run spawning ONE harness, not on one harness having a ceiling.
	// The bare number is only unambiguous when there is a single session kind for
	// it to bound: on a mixed run it reads as a bound on every session, when in
	// fact the opencode phase beside it has no mid-session ceiling at all. So the
	// common unphased config's line is unchanged byte for byte, and a mixed run
	// always names whose ceiling it is quoting.
	if len(cfg.SpawnedHarnesses()) == 1 {
		return fmt.Sprintf("max_session_tokens=%d is the mid-session bound", cfg.Budget.SessionTokenCeilingFor(only[0]))
	}
	return "max_session_tokens is the mid-session bound, per harness: " + strings.Join(parts, ", ")
}

// anyPhaseRunsTheDefaultTurnCap reports whether the effective config bounds any
// session by the built-in default rather than an operator-chosen cap — i.e.
// whether CLA-343's default is load-bearing for this run. The effective phases
// resolve phase → top-level → default, so a phase carrying DefaultMaxTurns is
// exactly a phase the operator never capped (an operator who set the same
// number explicitly is indistinguishable, and the statement "the default
// applies" is still true for them).
func anyPhaseRunsTheDefaultTurnCap(cfg *config.Config) bool {
	for _, ph := range cfg.EffectivePhases() {
		if ph.MaxTurns == config.DefaultMaxTurns {
			return true
		}
	}
	return false
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
