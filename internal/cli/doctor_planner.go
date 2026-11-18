package cli

// doctor_planner.go — the planner role's preflight (studio-chat.md §2).
//
// D23's "reports that and does not fall back" is enforced here, at the machine
// boundary: the stored document says WHICH harness and model, and this machine
// says whether it can run them. A planner whose harness is missing or
// unconfigured is a FAIL — never a silent substitution of the local default.
//
// The role check is REPORTING plus one aggregate refusal, exactly as the design
// words it: every project whose resolved document has no `roles.planner` is
// named, but the instance is refused only when NONE of them does. One project
// that opted out must not stop the instance serving the others.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// plannerTarget is one project the instance would serve chat for.
type plannerTarget struct {
	name            string
	url             string
	mcpConfigPath   string
	perHarnessPaths map[string]string
}

func checkPlanner(ctx context.Context, cfg *config.Config, e doctorEnv) []check {
	if cfg.EffectiveRole() != config.RolePlanner {
		return []check{{name: "role", status: pass, detail: "worker (backlog drain)"}}
	}
	checks := []check{{name: "role", status: pass, detail: "planner (conversation turns)"}}
	targets := plannerTargets(cfg)
	if len(targets) == 0 {
		checks = append(checks, check{
			name:   "planner",
			status: fail,
			detail: "role is planner, but no project slug can be derived from the config's MCP wiring - there is no pending-conversation endpoint to poll",
			remedy: "point mcp_config_path at /mcp/<slug>, or declare `projects` with the slugs this instance serves",
		})
		return checks
	}

	anyConfigured := false
	anyResolved := false
	// unverified counts projects whose role could not be READ. The aggregate
	// refusal below may only fire when every project was resolved: a transient
	// plane error is a WARN in checkRunConfigs, and it must not become a doctor
	// rejection here.
	unverified := 0
	usable := 0
	for _, t := range targets {
		// The per-project line is always qualified; the bare `planner` name is
		// the INSTANCE-level verdict appended last, so the two can never be
		// confused in the output or in a test.
		name := t.name
		if name == "" {
			name = cfg.Slug()
		}
		label := "planner[" + name + "]"
		newRCfg := e.newRCfgAPI
		if newRCfg == nil {
			newRCfg = plane.NewRunConfigAPI
		}
		rc := newRCfg(t.url, e.apiKey)
		st, err := rc.RunConfig(ctx)
		switch {
		case errors.Is(err, plane.ErrNotWired):
			unverified++
			checks = append(checks, check{
				name:   label,
				status: warn,
				detail: "run-config reads are not wired - cannot verify roles.planner",
				remedy: "set CLANKERBAR_API_KEY and a project-scoped MCP config; without them the planner can neither poll nor claim",
			})
			continue
		case errors.Is(err, plane.ErrNoConfig):
			checks = append(checks, check{
				name:   label,
				status: pass,
				detail: "no stored run config - no roles.planner, chat off for this project",
			})
			anyResolved = true
			continue
		case err != nil:
			unverified++
			checks = append(checks, check{
				name:   label,
				status: warn,
				detail: fmt.Sprintf("the stored run config could not be read (%v) - cannot verify roles.planner", err),
				remedy: "check the plane is reachable; the planner's next turn re-reads it",
			})
			continue
		}
		var doc config.RunConfigDoc
		if err := json.Unmarshal(st.Config, &doc); err != nil {
			unverified++
			checks = append(checks, check{
				name:   label,
				status: warn,
				detail: fmt.Sprintf("stored v%d could not be decoded (%v) - cannot verify roles.planner", st.Version, err),
			})
			continue
		}
		if doc.SchemaNewer() {
			unverified++
			checks = append(checks, check{
				name:   label,
				status: warn,
				detail: fmt.Sprintf("stored v%d uses schema v%d, newer than this build understands (v%d) - cannot verify roles.planner",
					st.Version, doc.SchemaVersion, config.RunConfigSchemaVersion),
				remedy: "upgrade the CLI",
			})
			continue
		}
		// Apply to a clone so the role is resolved exactly as the loop will see
		// it (including a harness-swap refusal), without touching the base.
		eff := cfg.Clone()
		if err := eff.ApplyRunConfig(&doc); err != nil {
			unverified++
			checks = append(checks, check{
				name:   label,
				status: warn,
				detail: fmt.Sprintf("stored v%d was REFUSED locally (%v) - cannot verify roles.planner", st.Version, err),
			})
			continue
		}
		anyResolved = true
		if !eff.PlannerEnabled() {
			checks = append(checks, check{
				name:   label,
				status: pass,
				detail: "no roles.planner - chat off for this project",
			})
			continue
		}
		anyConfigured = true
		c := checkPlannerRole(ctx, cfg, eff, t, e)
		c.name = label
		if c.status != fail {
			usable++
		}
		if st.Pending {
			c.detail += "; a proposed change awaits ratification"
		}
		checks = append(checks, c)
	}

	if anyResolved && !anyConfigured {
		if unverified > 0 {
			// Some projects could not be read: one of them may carry the planner,
			// so this stays the WARN the per-project lines are, never a refusal.
			checks = append(checks, check{
				name:   "planner",
				status: warn,
				detail: fmt.Sprintf("no RESOLVED project carries roles.planner, but %d project(s) could not be read - chat may still be served",
					unverified),
				remedy: "check the plane is reachable, then run doctor again to verify the remaining project(s)",
			})
		} else {
			checks = append(checks, check{
				name:   "planner",
				status: fail,
				detail: "no project's resolved run config carries roles.planner - a planner instance has no chat to serve",
				remedy: "propose a run-config document with a `roles.planner` block and have the operator ratify it, or run this instance as a worker",
			})
		}
	} else if !anyResolved && unverified > 0 {
		checks = append(checks, check{
			name:   "planner",
			status: warn,
			detail: fmt.Sprintf("could not verify roles.planner for any of the %d project(s) - doctor and run stay usable while the plane is unreachable", unverified),
			remedy: "check the plane is reachable, then run doctor again",
		})
	} else if anyConfigured && usable == 0 {
		// Every configured project's role failed its machine check; the
		// per-project FAILs already say why. An unreadable project may still
		// carry a servable role, so that case stays the WARN the per-project
		// lines are rather than turning a plane error into a refusal.
		if unverified > 0 {
			checks = append(checks, check{
				name:   "planner",
				status: warn,
				detail: fmt.Sprintf("every READ project's planner role failed its machine check, but %d project(s) could not be read - the instance may still serve them", unverified),
				remedy: "fix the per-project findings above, then check the plane is reachable and run doctor again",
			})
			return checks
		}
		checks = append(checks, check{
			name:   "planner",
			status: fail,
			detail: "no project's planner role can be served on this machine",
			remedy: "fix the per-project findings above; the role is not substituted with the local default harness or model",
		})
	}
	return checks
}

// checkPlannerRole is the machine half of one project's assigned role: the
// harness must exist here, carry its own wiring, and be runnable with the MCP
// config that lets the session reply. The model is policy this machine cannot
// verify, so it is reported as model_unverified and never substituted.
func checkPlannerRole(ctx context.Context, cfg, eff *config.Config, t plannerTarget, e doctorEnv) check {
	c := check{status: pass}
	sess, err := eff.ResolvePlanner()
	if err != nil {
		c.status = fail
		c.detail = "roles.planner cannot be served: " + err.Error()
		c.remedy = "fix the stored role (its harness must be one this build knows, and its tier must resolve there) and ratify it again; no fallback harness or model is used"
		return c
	}
	where := ""
	if t.name != "" {
		where = fmt.Sprintf("project %q: ", t.name)
	}
	h := sess.Harness
	// A harness other than the run-wide one inherits none of its machine wiring;
	// the same rule Validate applies to a phase on another harness.
	if h != cfg.Harness {
		hc, ok := cfg.Harnesses[h]
		if !ok || hc.Empty() {
			c.status = fail
			c.detail = fmt.Sprintf("%sroles.planner runs on harness %q, but `harnesses.%s` is missing or empty - the session would start with no config dir, no MCP config and no settings",
				where, h, h)
			c.remedy = fmt.Sprintf("declare `harnesses.%s` (config_dir / mcp_config_path) in this machine's config file", h)
			return c
		}
	}
	if e.lookPath != nil {
		path, err := e.lookPath(h)
		if err != nil {
			c.status = fail
			c.detail = fmt.Sprintf("%sroles.planner runs on harness %q, which is not on PATH", where, h)
			c.remedy = "install " + h + ", or change the stored role to a harness this machine has and ratify it again"
			return c
		}
		if e.binVersion != nil {
			if ver, err := e.binVersion(ctx, h); err != nil {
				c.status = fail
				c.detail = fmt.Sprintf("%s%q is on PATH but not runnable: %v", where, h, err)
				c.remedy = "check it runs non-interactively: " + h + " --version"
				return c
			} else {
				path += " (" + ver + ")"
			}
		}
		c.detail = fmt.Sprintf("%sharness %s", where, path)
	} else {
		c.detail = fmt.Sprintf("%sharness %q", where, h)
	}
	hc := eff.SessionFor(h)
	// Without an MCP config the session has no clankerbar tools, so it can
	// neither read the turn's subjects nor call reply_conversation. This is the
	// one wiring gap that makes the whole turn impossible, so it FAILs.
	if mcp := eff.ResolveMCPConfig(h, t.mcpConfigPath, t.perHarnessPaths); mcp == "" {
		c.status = fail
		c.detail += fmt.Sprintf("; no clankerbar MCP config resolves for it, so a session could not call reply_conversation")
		c.remedy = fmt.Sprintf("give harness %q an MCP config (`mcp_config_path`, or `harnesses.%s.mcp_config_path`)", h, h)
		return c
	}
	if sess.Model != "" {
		c.status = warn
		c.detail += fmt.Sprintf(", model %s (model_unverified: this harness exposes no model list)", sess.Model)
		if c.remedy == "" {
			c.remedy = "the model alias is policy the machine cannot check; a wrong alias will fail the first turn rather than run on a substituted model"
		}
	} else {
		c.detail += ", model (harness default)"
	}
	caps, known := harness.CapabilitiesOf(h)
	if known {
		if !caps.HonoursMaxTurns && sess.MaxTurns > 0 {
			c.status = warn
			c.detail += fmt.Sprintf("; max_turns=%d is INERT under %s (it takes no turn flag)", sess.MaxTurns, h)
		}
		if !caps.ResumesSessions {
			c.status = warn
			c.detail += "; this harness cannot resume a session, so every turn re-primes from the plane's history"
		}
	}
	if strings.TrimSpace(hc.ConfigDir) == "" {
		if c.status == pass {
			c.status = warn
		}
		c.detail += "; no config_dir pinned (ambient credentials - an unattended environment may have none)"
	} else {
		// The same credential/config-dir check every other harness gets: an
		// absent, empty or unreadable dir is a real planner outage, and its
		// marker heuristic already words the "may be in the keychain" caveat.
		cd := checkConfigDirNamed("config_dir", h, hc.ConfigDir)
		switch cd.status {
		case fail:
			c.status = fail
			c.detail += "; " + cd.detail
			c.remedy = cd.remedy
		case warn:
			if c.status == pass {
				c.status = warn
			}
			c.detail += "; " + cd.detail
			if c.remedy == "" {
				c.remedy = cd.remedy
			}
		}
	}
	return c
}

// plannerTargets mirrors checkRunConfigs' target list: every configured project,
// or the single unnamed target.
func plannerTargets(cfg *config.Config) []plannerTarget {
	var targets []plannerTarget
	for _, p := range cfg.Projects {
		targets = append(targets, plannerTarget{
			name:            p.Slug,
			url:             cfg.ProjectEndpoint(p),
			mcpConfigPath:   p.MCPConfigPath,
			perHarnessPaths: p.MCPConfigPaths,
		})
	}
	if len(cfg.Projects) == 0 {
		if cfg.Slug() == "" {
			return nil
		}
		targets = append(targets, plannerTarget{
			url:           cfg.BacklogEndpoint(),
			mcpConfigPath: cfg.MCPConfigPath,
		})
	}
	return targets
}
