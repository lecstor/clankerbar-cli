package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// --- run-config in force (CLA-410) -------------------------------------------

// checkRunConfigs reports, per target, WHICH execution config the loop will run
// under — the local file, or a stored plane document overlaid on it — plus any
// machine-fit warnings about the STORED dials. The plane validates shape; only
// this machine can see whether a dial it ratified can actually fire (a turn cap
// under opencode, a cost ceiling on a harness that never reports cost), which is
// the validation split the design memo draws.
//
// Plane trouble never FAILs: an unreadable stored document leaves the loop
// running its previous config (the same posture applyReload takes), so doctor
// says WARN and keeps `doctor && run` usable. A stored step this machine cannot
// spawn is NOT plane trouble — it is ratified policy that would silently not
// run here — so that one deliberately FAILs, and `doctor && run` refuses to
// start until it is fixed.
func checkRunConfigs(ctx context.Context, cfg *config.Config, e doctorEnv) []check {
	type rcTarget struct {
		name string // "" for the unnamed single target
		url  string
	}
	var targets []rcTarget
	for _, p := range cfg.Projects {
		targets = append(targets, rcTarget{name: p.Slug, url: cfg.ProjectEndpoint(p)})
	}
	if len(cfg.Projects) == 0 {
		targets = append(targets, rcTarget{url: cfg.BacklogEndpoint()})
	}

	src := "defaults"
	if s := cfg.Source(); s != "" {
		src = s
	}

	var checks []check
	for _, t := range targets {
		name := t.name
		label := "run-config"
		if name != "" {
			label += "[" + name + "]"
		}
		newRCfg := e.newRCfgAPI
		if newRCfg == nil {
			// An env literal that predates the seam (every existing test) still
			// gets production wiring, not a nil call.
			newRCfg = plane.NewRunConfigAPI
		}
		rc := newRCfg(t.url, e.apiKey)
		st, err := rc.RunConfig(ctx)
		switch {
		case errors.Is(err, plane.ErrNotWired):
			// No endpoint or no key: nothing to compare against, and the backlog
			// wiring check already reports that gap. Silence here is not hiding
			// a finding; it is declining to duplicate one.
			continue
		case errors.Is(err, plane.ErrNoConfig):
			checks = append(checks, check{
				name:   label,
				status: pass,
				detail: "local rules (" + src + ") - nothing stored on the plane",
			})
			continue
		case err != nil:
			checks = append(checks, check{
				name:   label,
				status: warn,
				detail: fmt.Sprintf("unreadable (%v) - local rules (%s) stay in force", err, src),
				remedy: "check the plane is reachable; the loop keeps its previous config until the document fetches",
			})
			continue
		}

		c := check{name: label, status: pass}
		var doc config.RunConfigDoc
		if err := json.Unmarshal(st.Config, &doc); err != nil {
			c.status = warn
			c.detail = fmt.Sprintf("stored v%d undecodable (%v) - local rules (%s) stay in force", st.Version, err, src)
			c.remedy = "re-propose a valid document from the console or propose-config"
			checks = append(checks, c)
			continue
		}
		if doc.SchemaNewer() {
			c.status = warn
			c.detail = fmt.Sprintf("stored v%d uses schema v%d, newer than this build understands (v%d) - local rules (%s) stay in force", st.Version, doc.SchemaVersion, config.RunConfigSchemaVersion, src)
			c.remedy = "upgrade the CLI, then re-ratify the document from the console"
			checks = append(checks, c)
			continue
		}
		if doc.Empty() {
			c.detail = fmt.Sprintf("stored v%d sets nothing consumable - local rules (%s) in force", st.Version, src)
			checks = append(checks, c)
			continue
		}
		eff := cfg.Clone()
		if err := eff.ApplyRunConfig(&doc); err != nil {
			c.status = warn
			c.detail = fmt.Sprintf("stored v%d REFUSED locally (%v) - local rules (%s) stay in force", st.Version, err, src)
			c.remedy = "fix the stored document and ratify again; until then the loop ignores it"
			checks = append(checks, c)
			continue
		}
		if err := eff.Validate(); err != nil {
			// A stored STEP this machine cannot spawn is the one refusal that
			// earns FAIL: the document is ratified policy that silently will not
			// run here, and the operator has to know before an unattended drain
			// quietly falls back to local rules instead (CLA-598). That FAIL is
			// the point — `doctor && run` refuses to start on such a document.
			// Every OTHER refusal stays WARN, because plane trouble must not make
			// `doctor && run` unusable (applyRunConfig's own posture).
			var she *config.StepHarnessError
			if errors.As(err, &she) {
				where := ""
				if name != "" {
					where = fmt.Sprintf("project %q: ", name)
				}
				c.status = fail
				c.detail = fmt.Sprintf("%sstored v%d step %s runs on harness %q, which this machine cannot run (%s) - local rules (%s) stay in force",
					where, st.Version, she.Step, she.Harness, she.Problem, src)
				c.remedy = fmt.Sprintf("declare `harnesses.%s` locally, or change the stored step's harness and ratify again; until then the loop ignores the document", she.Harness)
				checks = append(checks, c)
				continue
			}
			c.status = warn
			c.detail = fmt.Sprintf("stored v%d REFUSED locally (%v) - local rules (%s) stay in force", st.Version, err, src)
			c.remedy = "fix the stored document and ratify again; until then the loop ignores it"
			checks = append(checks, c)
			continue
		}
		c.detail = fmt.Sprintf("stored v%d overlaid on %s", st.Version, src)
		if st.Pending {
			c.detail += "; a proposed change awaits ratification"
		}
		// Machine-fit notes on the STORED dials: what shape-validates but cannot
		// fire here. Each names the dial as stored, so the operator edits the
		// document rather than hunting their local file for a line they have.
		var inert []string
		if doc.MaxTurns > 0 {
			var blind []string
			for _, h := range eff.SpawnedHarnesses() {
				if !harnessHonoursMaxTurns(h) {
					blind = append(blind, h)
				}
			}
			if len(blind) > 0 {
				inert = append(inert, fmt.Sprintf("max_turns=%d is INERT under %s (it takes no turn flag)", doc.MaxTurns, strings.Join(blind, "/")))
			}
		}
		if doc.MaxSessionWallClock > 0 {
			if dial, on := inertSessionWallClock(eff); dial == "max_session_wall_clock" && on != "" {
				inert = append(inert, fmt.Sprintf("max_session_wall_clock is INERT under %s (it enforces no session clock)", on))
			}
		}
		if b := doc.Budget; b != nil {
			for _, h := range slices.Sorted(maps.Keys(b.PerHarness)) {
				if hb := b.PerHarness[h]; hb.MaxCostUSD > 0 && !harnessReportsCost(h) {
					inert = append(inert, fmt.Sprintf("budget.per_harness[%s].max_cost_usd is INERT (%s never reports cost)", h, h))
				}
			}
		}
		if len(inert) > 0 {
			c.status = warn
			c.detail += "; " + strings.Join(inert, "; ")
			c.remedy = "these dials ride along harmlessly but bound nothing on this machine"
		}
		checks = append(checks, c)
	}
	return checks
}
