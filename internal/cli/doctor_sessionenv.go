package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// --- 1c. session env ---------------------------------------------------------

// requiredSessionEnv is the minimum a triggered allowlist must keep (CLA-714).
// A session without PATH cannot run a command, without HOME cannot read git's
// or ssh's configuration, and without CLANKERBAR_API_KEY cannot reach the
// plane (the MCP config carries it as `Bearer {env:CLANKERBAR_API_KEY}`).
// CLANKERBAR_WORKDIR_ROOT joins them while the workdir derivation is on: the
// fleet's layout is what every brief names, and `doctor` has no way to tell an
// operator's shell from a supervisor's without the one signal both carry
// (`project_root` on a materialized config, the variable in a base config's
// environment).
func requiredSessionEnv(cfg *config.Config) []string {
	req := []string{"HOME", "PATH", "CLANKERBAR_API_KEY"}
	if cfg.ProjectRoot || os.Getenv(WorkdirRootEnv) != "" {
		req = append(req, WorkdirRootEnv)
	}
	return req
}

// checkSessionEnvAllowlist reports the shape in force for every (project,
// harness) pair the run will spawn (CLA-714). With no allowlist declared it
// PASSes and says so: full inheritance is the documented default, not a
// warning. With one declared it names the variables that will reach the
// session - names only, never values - and FAILs when the list omits one of
// requiredSessionEnv, because that failure otherwise appears later as an
// unexplained repo_not_found or an unauthenticated plane call inside a session.
//
// It asks SpawnedHarnesses, not PhaseHarnesses, for the reason the two exist:
// PhaseHarnesses seeds the run-wide `harness` unconditionally, which is right
// for "is every declared harness registered and usable" but wrong for "what is
// true of the sessions this run actually runs". A config whose every phase
// names its own harness never spawns the run-wide one, and this check is about
// what a session INHERITS - so reporting (and FAILing) that harness's scope
// would send the operator to fix a harness no session ever runs on, and a false
// FAIL blocks a run that works. Same doctrine as runsHarness and
// costBlindHarnesses in the budget check.
func checkSessionEnvAllowlist(cfg *config.Config) []check {
	harnesses := cfg.SpawnedHarnesses()
	name := func(slug, h string) string {
		label := "session env allowlist"
		qualifier := slug
		if len(harnesses) > 1 {
			// Same SHAPE as checkSessions' naming: the harness is part of the
			// name only when more than one is in play, so a single-harness report
			// stays unqualified. The count itself comes from SpawnedHarnesses, so
			// it can be smaller than checkSessions' - that check asks what is
			// configured, this one what a session inherits.
			if qualifier != "" {
				qualifier += ":"
			}
			qualifier += h
		}
		return label + bracket(qualifier)
	}
	// The env a session receives on top of the allowlist, by name. doctor never
	// reads a value: one of these is routinely a credential.
	declaredNames := func(h, slug string) []string { return cfg.SessionEnvNames(h, slug) }
	report := func(slug, h string) check {
		c := check{name: name(slug, h)}
		allow := cfg.SessionEnvAllowlist(h, slug)
		if len(allow) == 0 {
			c.status = pass
			c.detail = "no env_allowlist - sessions inherit the daemon's whole environment (the unchanged default)"
			return c
		}
		passes := append([]string(nil), allow...)
		sort.Strings(passes)
		c.detail = "sessions inherit only: " + strings.Join(passes, ", ")
		declared := declaredNames(h, slug)
		if len(declared) > 0 {
			c.detail += "; the env block adds: " + strings.Join(declared, ", ")
		}
		// The env overlay is appended AFTER the inheritEnv filter and WINS (every
		// adapter's env() puts in.Env last), so a required variable the env block
		// declares reaches the session even when the allowlist omits it. Computing
		// the miss from the allowlist alone FAILed that working shape, and a false
		// FAIL blocks a run that works (CLA-736 review 1.review-2.1).
		present := append(append([]string(nil), allow...), declared...)
		var missing []string
		for _, want := range requiredSessionEnv(cfg) {
			if !contains(present, want) {
				missing = append(missing, want)
			}
		}
		if len(missing) > 0 {
			c.status = fail
			c.detail += " - missing " + strings.Join(missing, ", ") + " (a session without it fails later, elsewhere)"
			c.remedy = "add " + strings.Join(missing, ", ") + " to the env_allowlist in force for this scope (the most specific level that declares one replaces the rest), or declare it in the session env block, or drop env_allowlist to inherit everything"
			return c
		}
		// ADVICE, not a verdict (CLA-736 review 1.review-2.2): an ssh clone_url_base
		// works from an on-disk key without the agent, so this is only a footgun
		// when the key is agent-held - not something to FAIL, or even WARN, a
		// working run over.
		if !contains(present, "SSH_AUTH_SOCK") {
			c.info = append(c.info, "SSH_AUTH_SOCK is not in the session env: a session pushing over an ssh origin needs it only when the key is agent-held (an on-disk key does not)")
		}
		c.status = pass
		return c
	}
	if len(cfg.Projects) == 0 {
		out := make([]check, 0, len(harnesses))
		for _, h := range harnesses {
			out = append(out, report("", h))
		}
		return out
	}
	out := make([]check, 0, len(cfg.Projects)*len(harnesses))
	for _, p := range cfg.Projects {
		for _, h := range harnesses {
			out = append(out, report(p.Slug, h))
		}
	}
	return out
}

// contains reports whether names holds want. Small and local: the minimum is
// four entries, and a set would cost more lines than it saves.
func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// checkSessionEnv verifies every DECLARED env entry that needs resolving can be
// resolved right now (CLA-462): each fromCommand is executed and each "@path"
// literal is re-read under its owner-only rule. Outputs are discarded — doctor
// reports the verdict and the variable's name, never a credential. One FAIL
// here means every spawn that overlay reaches will refuse until it is fixed,
// so seeing it in the preflight is the cheap copy of the same fact.
//
// Plain literals need no check and get none: there is nothing to resolve.
func checkSessionEnv(ctx context.Context, cfg *config.Config, e doctorEnv) []check {
	var out []check
	for _, d := range cfg.DeclaredEnvs() {
		switch {
		case d.FromCommand != "":
			c := check{name: "session env " + d.Name + " (" + d.Label + ")"}
			if _, err := e.runEnvCmd(ctx, d.FromCommand); err != nil {
				c.status = fail
				c.detail = fmt.Sprintf("fromCommand %q failed: %v", d.FromCommand, err)
				c.remedy = "fix the command - spawns it gates refuse rather than run without " + d.Name
				out = append(out, c)
				continue
			}
			c.status = pass
			c.detail = "fromCommand resolves"
			out = append(out, c)
		case d.Path != "":
			c := check{name: "session env file " + d.Name + " (" + d.Label + ")"}
			if err := e.envPath(d.Path); err != nil {
				c.status = fail
				c.detail = d.Path + ": " + err.Error()
				c.remedy = "fix the file's mode or path - spawns it gates refuse rather than run without " + d.Name
				out = append(out, c)
				continue
			}
			c.status = pass
			c.detail = "owner-only file readable"
			out = append(out, c)
		}
	}
	return out
}

// checkTokenSources warns when a scope whose repos PUSH has declared no source
// for GH_TOKEN anywhere its sessions' overlays look — top level, any harness
// block, or the project's own two blocks (CLA-462). This is exactly the shape
// of the 2026-08-24 incident: a daemon launched without the wrapper spawned
// sessions with no token, which died later at `git push` with an error naming
// nothing. A WARN, not a FAIL: a repo may push over ssh or not at all, and the
// operator owns that call — the heuristic (repos declared = pushes happen) is
// only allowed to speak up, not to stop the run.
func checkTokenSources(cfg *config.Config) []check {
	report := func(label, slug string) check {
		c := check{name: label}
		if len(cfg.ReposFor(slug)) == 0 {
			c.status = pass
			c.detail = "no repos declared - nothing here is known to push"
			return c
		}
		if cfg.TokenSourceDeclared(slug, "GH_TOKEN") {
			c.status = pass
			c.detail = "GH_TOKEN has a declared source"
			return c
		}
		c.status = warn
		c.detail = "repos are declared but no env level declares GH_TOKEN - sessions push with whatever identity their inherited environment happens to carry"
		c.remedy = `declare "env": {"GH_TOKEN": {"fromCommand": "gh auth token -u <account>"}} (top level, per harness, per project, or per project-per-harness)`
		return c
	}
	if len(cfg.Projects) == 0 {
		return []check{report("gh token source", "")}
	}
	out := make([]check, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		out = append(out, report("gh token source["+p.Slug+"]", p.Slug))
	}
	return out
}
