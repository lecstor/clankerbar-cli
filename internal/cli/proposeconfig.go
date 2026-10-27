package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// propose-config (CLA-410): the doctor-style one-shot that imports today's
// config.json into the plane as a PENDING proposal, so the operator can ratify
// it in the console and turn the local file into the project's first stored
// execution config (memo open decision 4). It proposes; it never sets - the
// ratify click is the operator's, exactly as for a clanker's own proposal over
// MCP. Until they click, the loop keeps running the local file byte for byte.
//
// What is imported is the MOVABLE half only (the pipeline — prompt, phases,
// review_panel and a phase's model — plus harness, model, tier buckets, budget,
// escalation rules and the two backstops): workdir, wiring, env and state stay
// machine-local, because the plane must not become a custodian of paths and
// credentials, and the origin rule depends on the trusted statement staying
// where the operator wrote it. `review_panel_concurrency` is machine-owned
// (decision e1365721) and is deliberately not exported either: how many panel
// seats run at once changes only how fast a machine reaches the result.

type proposeConfigFlags struct {
	cfgPath string
	slug    string
	notes   string
}

func newProposeConfigFlagSet(f *proposeConfigFlags) *pflag.FlagSet {
	fs := newFlagSet("propose-config")
	fs.StringVarP(&f.cfgPath, "config", "c", "", "config file (default: ~/.config/clankerbar/config.json)")
	fs.StringVar(&f.slug, "slug", "", "clankerbar project to propose against (default: this config's own). In single-project mode it redirects the proposal to the project it names; when projects[] names more than one it is required and must name one of them")
	fs.StringVar(&f.notes, "notes", "", "extra rationale carried on the proposal for the operator")
	return fs
}

// ProposeConfig resolves the config, picks the one project to propose against,
// renders its movable half as a run document, and records it as a pending
// proposal. Exit non-zero on any refusal, like doctor.
func ProposeConfig(ctx context.Context, args []string) error {
	var f proposeConfigFlags
	fs := newProposeConfigFlagSet(&f)
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}

	cfg, err := config.Load(f.cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	slug, err := pickSlug(cfg, f.slug, "--slug")
	if err != nil {
		return err
	}

	doc, err := cfg.RunConfigDocument()
	if err != nil {
		return err
	}
	if len(doc) == 0 {
		return errors.New("nothing to import: this config sets none of the dials the plane stores (prompt, phases, review_panel, harness, model, models, budget, escalation, max_turns, max_session_wall_clock)")
	}

	// The endpoint's slug is the RESOLVED one, so the write and the name printed
	// below cannot disagree; ProjectEndpoint keeps the origin on CredentialOrigin.
	endpoint := cfg.ProjectEndpoint(config.Project{Slug: slug})
	// Unreachable after Validate; kept so a moved origin/slug invariant fails with
	// this sentence rather than ErrNotWired.
	if endpoint == "" {
		return errors.New("cannot resolve a project-scoped MCP endpoint - set backlog_url in the config")
	}
	apiKey := os.Getenv("CLANKERBAR_API_KEY")
	rc := plane.NewRunConfigAPI(endpoint, apiKey)

	notes := fmt.Sprintf(
		"Imported from the local config file by `clankerbar propose-config` (%s). "+
			"This is the file's movable half verbatim - the machine-local core "+
			"(workdir, MCP wiring, env) stays local. Ratifying makes this document "+
			"the project's execution config; dismissing keeps the local file in force.",
		srcLabel(cfg))
	if f.notes != "" {
		notes += "\n\n" + f.notes
	}

	if err := rc.ProposeRunConfig(ctx, doc, notes); err != nil {
		return fmt.Errorf("propose run config for %s: %w", slug, err)
	}

	fmt.Printf("proposed %d dial(s) for project %q - PENDING until the operator ratifies it in the console (Settings -> Run configuration).\n", len(doc), slug)
	fmt.Println("until then the daemon keeps running the local config file unchanged.")
	return nil
}

// pickSlug resolves the ONE project a command targets: the caller's explicit
// name when there is one, otherwise the config's own answer. A multi-project
// config has no single default and is refused rather than fanned out silently.
//
// An explicit name is honoured when the config drives at most one project,
// whatever spelling named that project: `upload --project X` targets /mcp/X in
// this shape, so refusing here only because X is not in a one-entry projects[]
// would make the two commands disagree about what a named project means. With
// several projects the name must match one, so a typo cannot drift onto a project
// the config never mentioned.
//
// Called by propose-config with --slug and by upload/deck with --project.
func pickSlug(cfg *config.Config, want, flag string) (string, error) {
	switch {
	case want != "":
		for _, p := range cfg.Projects {
			if p.Slug == want {
				return want, nil
			}
		}
		if len(cfg.Projects) <= 1 {
			return want, nil
		}
		var have []string
		for _, p := range cfg.Projects {
			have = append(have, p.Slug)
		}
		return "", fmt.Errorf("%s %q matches no configured project (have: %s)", flag, want, strings.Join(have, ", "))
	case len(cfg.Projects) == 1:
		return cfg.Projects[0].Slug, nil
	case len(cfg.Projects) > 1:
		var have []string
		for _, p := range cfg.Projects {
			have = append(have, p.Slug)
		}
		return "", fmt.Errorf("%s is required when the config names more than one project (have: %s)", flag, strings.Join(have, ", "))
	default:
		// Single-project mode: the .mcp.json's /mcp/<slug> URL names the project
		// the same way the poll does. No slug there means the operator must say.
		if slug := cfg.Slug(); slug != "" {
			return slug, nil
		}
		return "", fmt.Errorf("%s is required: neither a projects[] entry nor an .mcp.json naming /mcp/<slug> says which project to target", flag)
	}
}

func srcLabel(cfg *config.Config) string {
	if s := cfg.Source(); s != "" {
		return s
	}
	return "defaults"
}
