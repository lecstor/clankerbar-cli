package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
)

// --- 13. delivery PR gate ----------------------------------------------------

// prGateProbeWorkdirs lists the directories whose repositories the delivery gate judges, so the
// preflight exercises the SAME ones. For every configured project that is its effective workdir
// (its own, falling back to the top-level one — see projectWorkDir) plus the checkouts its repos
// map declares, because a session starts in a resolved checkout rather than in the project root.
func prGateProbeWorkdirs(cfg *config.Config) []string {
	var dirs []string
	seen := map[string]bool{}
	add := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			return
		}
		dir = filepath.Clean(dir)
		if seen[dir] {
			return
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	if len(cfg.Projects) == 0 {
		add(cfg.WorkDir)
		for _, checkout := range config.DeclaredCheckouts(cfg.Repos, cfg.PrimaryRepo, cfg.WorkDir) {
			add(checkout)
		}
		return dirs
	}
	for _, p := range cfg.Projects {
		workdir := projectWorkDir(cfg, p)
		add(workdir)
		for _, checkout := range config.DeclaredCheckouts(cfg.ReposFor(p.Slug), cfg.PrimaryRepoFor(p.Slug), workdir) {
			add(checkout)
		}
	}
	return dirs
}

// checkPRGate reports CLA-310's delivery gate and its prerequisites: the driver's verifier reaches
// GitHub through the `gh` CLI, a delivery whose PR is CONFLICTING or carries NO checks is refused
// when it runs, and — since CLA-776 — gh's `--json` output must actually be parseable, because
// the shipped GH_FORCE_TTY=0 made gh colour it and every real delivery's PR went unjudged for two
// months. A missing `gh` is a WARN, not a FAIL — the check degrades to an explicit
// refusal-to-verify and the run carries on, which is the package's fail-open discipline — but the
// operator should see the gate's prerequisite BEFORE the first delivery goes out unchecked.
func checkPRGate(ctx context.Context, cfg *config.Config, e doctorEnv) check {
	c := check{name: "pr_gate", status: pass}
	path, err := e.lookPath("gh")
	if err != nil {
		c.status = warn
		c.detail = "gh is not on PATH — deliveries naming a PR cannot be verified (an unchecked PR is not caught)"
		c.remedy = "install the GitHub CLI (https://cli.github.com) so the driver can check mergeability and CI before accepting a delivery"
		return c
	}
	c.detail = path + " present; a delivery's PR must be MERGEABLE with a passing check rollup"

	// Probe a REAL `gh pr view --json`.
	if e.probePRGate != nil {
		probe, perr := e.probePRGate(ctx, path, prGateProbeWorkdirs(cfg))
		switch {
		case perr == nil:
			c.info = append(c.info, fmt.Sprintf(
				"probe: `gh pr view %d --repo %s --json` parsed — the gate can read gh's output",
				probe.Number, probe.Slug))
		case errors.Is(perr, delivery.ErrNoPRProbeTarget):
			c.status = warn
			c.info = append(c.info, "probe: NOT exercised — "+firstLine(perr.Error())+
				"; the gate's parse path is untested until a repository with a PR is in one of the configured workdirs")
			c.remedy = "run doctor where a github.com repository with a pull request exists (every configured project's workdir and its declared checkouts were searched); until then this check has NOT validated that the gate can parse gh's output"
		case delivery.IsGHParseError(perr):
			c.status = fail
			c.detail = "gh ran, but its `--json` output is not parseable — EVERY delivery's PR goes unjudged"
			c.info = append(c.info, "probe: "+firstLine(perr.Error()))
			c.remedy = "clear whatever decorates gh's stdout — a wrapper or alias on gh forcing colour (GH_FORCE_TTY / CLICOLOR_FORCE / FORCE_COLOR), or a gh that ignores NO_COLOR; colour in parsed output is the CLA-776 fault"
		case errors.Is(perr, context.DeadlineExceeded), errors.Is(perr, context.Canceled):
			c.status = warn
			c.info = append(c.info, "probe: timed out reading a real PR — "+firstLine(perr.Error()))
			c.remedy = "check network access to api.github.com; the delivery check degrades to could-not-verify while the probe cannot reach GitHub"
		default:
			c.status = warn
			c.info = append(c.info, "probe: could not read a real PR — "+firstLine(perr.Error()))
			c.remedy = "run `gh auth status` and confirm it can read `gh pr view --json`; the delivery check degrades to could-not-verify while gh cannot"
		}
	}

	if len(cfg.Projects) == 0 {
		if cfg.AllowUncheckedPR {
			c.info = append(c.info, "empty check rollups: WARNED, not refused (allow_unchecked_pr: true)")
		} else {
			c.info = append(c.info, "empty check rollups: REFUSED (the default; allow_unchecked_pr opts out for a repo with no CI)")
		}
		return c
	}
	// Per project, because the opt-out is per project: one line each, so a loose project is visible
	// without grepping the config.
	for _, p := range cfg.Projects {
		state := "REFUSED"
		if cfg.AllowUncheckedPRFor(p.Slug) {
			state = "WARNED"
		}
		c.info = append(c.info, "empty rollups["+p.Slug+"]: "+state+" (allow_unchecked_pr)")
	}
	return c
}
