package cli

// mcp.go — `clankerbar mcp`, the stdio-to-Streamable-HTTP bridge every local
// harness can configure identically. The credential stays in the CLI's own
// store (internal/auth), so a harness config carries no key and the agent
// session never sees one in its environment.

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/mcpbridge"
)

type mcpFlags struct {
	host    string
	project string
	multi   bool
}

func newMcpFlagSet(f *mcpFlags) *pflag.FlagSet {
	fs := newFlagSet("mcp")
	fs.StringVar(&f.host, "host", "", "clankerbar origin to bridge to (default: this config's backlog_url, else "+mcpbridge.DefaultOrigin+")")
	fs.StringVar(&f.project, "project", "", "pin the bridge to one project slug (targets <origin>/mcp/<slug>)")
	fs.BoolVar(&f.multi, "multi", false, "use the account-wide multi-project /mcp surface (the default when no project is pinned)")
	return fs
}

// Mcp is `clankerbar mcp`: run the stdio-to-Streamable-HTTP bridge on
// stdin/stdout until the client closes the pipe or the process is signalled.
func Mcp(ctx context.Context, args []string) error {
	return mcpRun(ctx, args, os.Stdin, os.Stdout, os.Stderr)
}

// mcpRun is Mcp with its streams injected. stdout is the protocol pipe: from
// the moment the bridge starts, only newline-delimited JSON-RPC responses are
// written there. Everything else belongs on stderr.
func mcpRun(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	var f mcpFlags
	fs := newMcpFlagSet(&f)
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}
	project := strings.TrimSpace(f.project)
	if f.multi && project != "" {
		return errors.New("--multi and --project are mutually exclusive: --multi uses the account-wide /mcp surface, --project pins one project")
	}
	if fs.Changed("project") && project == "" {
		return errors.New("--project: must name a project slug")
	}
	origin, err := mcpbridge.ResolveOrigin(f.host)
	if err != nil {
		return err
	}
	target, err := mcpbridge.ResolveTarget(mcpbridge.TargetOptions{
		Origin:  origin,
		Project: project,
		Multi:   f.multi,
		Warn:    stderr,
	})
	if err != nil {
		return err
	}
	return mcpbridge.Run(ctx, mcpbridge.Config{
		Target: target,
		Origin: origin,
		In:     stdin,
		Out:    stdout,
		Err:    stderr,
	})
}
