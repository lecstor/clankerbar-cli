package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

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
	if bin == "opencode" {
		checkOpencodeVersion(&c, ver)
	}
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
	"claude": {".credentials.json", ".claude.json", "settings.json"},
	"codex":  {"auth.json", "config.toml"},
	// 2.x keeps its config here and its credentials in the data dir's
	// opencode.db, which the opencode_auth check reports on separately.
	"opencode": {"opencode.json", "opencode.jsonc", "config.json"},
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
