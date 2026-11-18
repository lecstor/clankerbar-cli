package cli

// planner.go — the `role: "planner"` branch of `run` (studio-chat.md §2/§3).
//
// One role per instance, so this either runs the planner loop or the drain; it
// never runs both. The loop owns the 2-second conversation poll, and its turns
// take the same machine wiring (config dirs, env overlays, MCP config, session
// bounds) a drain session gets.

import (
	"context"
	"errors"
	"log"
	"os"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/plane"
	"github.com/lecstor/clankerbar-cli/internal/planner"
)

// runPlanner builds the per-project planner targets and runs the loop until the
// context ends, then re-execs when a RESTART marker asked for it.
//
// The role's harness/model come from the plane and are re-read before every
// turn; a local RELOAD re-reads the machine wiring the loop applies to its next
// turn, and a role/target change needs a restart (which the marker family
// provides through restartSelf, the same path the drain uses).
func runPlanner(ctx context.Context, cfg *config.Config, apiKey, cfgPath string, overrides config.Overrides) error {
	// The poll, the claim and the reply all use this key. Refusing before the
	// loop starts is honest: an idle planner that can never claim looks like a
	// plane with no work.
	if apiKey == "" {
		return errors.New("role planner needs CLANKERBAR_API_KEY: the pending poll, the claim and the reply all carry it")
	}

	var targets []planner.Target
	if len(cfg.Projects) > 0 {
		for _, p := range cfg.Projects {
			targets = append(targets, planner.Target{
				Name:           p.Slug,
				Poller:         backlog.NewConversationPoller(cfg.ProjectConversationPendingURL(p), apiKey),
				Conversations:  plane.NewConversationAPI(cfg.ProjectEndpoint(p), apiKey),
				RCfg:           plane.NewRunConfigAPI(cfg.ProjectEndpoint(p), apiKey),
				Fleet:          fleet.New(cfg.ProjectFleetReportURL(p), apiKey),
				WorkDir:        p.WorkDir,
				MCPConfigPath:  p.MCPConfigPath,
				MCPConfigPaths: p.MCPConfigPaths,
			})
		}
	} else {
		if cfg.ConversationPendingURL() == "" {
			return errors.New("role planner: no project slug can be derived from this config's MCP wiring, so there is no pending-conversation endpoint to poll - point mcp_config_path at /mcp/<slug>, or declare `projects`")
		}
		targets = append(targets, planner.Target{
			Name:          "",
			Poller:        backlog.NewConversationPoller(cfg.ConversationPendingURL(), apiKey),
			Conversations: plane.NewConversationAPI(cfg.BacklogEndpoint(), apiKey),
			RCfg:          plane.NewRunConfigAPI(cfg.BacklogEndpoint(), apiKey),
			Fleet:         fleet.New(cfg.FleetReportURL(), apiKey),
			MCPConfigPath: cfg.MCPConfigPath,
			// The project's per-harness files are a multi-project shape; the
			// harness block's own MCPConfigPath covers this one.
			MCPConfigPaths: nil,
		})
	}

	p, err := planner.New(planner.Config{Base: cfg, Targets: targets})
	if err != nil {
		return err
	}
	p.SetReloader(newConfigReloader(cfgPath, overrides))
	host, _ := os.Hostname()
	instance := cfg.ResolvedInstanceName(host)
	log.Printf("planner: instance %q serving %d project(s); polling conversations every %s, one turn at a time",
		instance, len(targets), planner.DefaultPollInterval)
	if err := p.Run(ctx); err != nil {
		return err
	}
	if p.RestartRequested() {
		return restartSelf(os.Args)
	}
	return nil
}
