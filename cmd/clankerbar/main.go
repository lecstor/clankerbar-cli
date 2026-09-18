// Command clankerbar drives a coding-agent CLI (Claude Code, Codex, ...) through
// a clankerbar backlog, unattended: it respawns fresh harness sessions that work
// the backlog one task at a time, and survives usage limits. State lives in the backlog (over MCP),
// not in any session — so a killed session's task is reclaimed and continued.
//
// This is a local, open-source client of the clankerbar control plane. See the
// design memo (docs/proposals/looping.md in the private clankerbar repo).
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/lecstor/clankerbar-cli/internal/cli"
	"github.com/lecstor/clankerbar-cli/internal/version"
)

func main() {
	os.Exit(run(os.Args))
}

// run executes one invocation and returns the process exit code. Extracted
// from main so the command routing — bare `clankerbar` is the fleet
// supervisor (CLA-525), help/version exit 0, an unknown command exits 2 — is
// testable. A path that reaches log.Fatal still exits the process directly
// (exit 1), exactly as main did; tests exercise only the paths that return.
func run(args []string) int {
	log.SetFlags(log.Ltime)
	log.SetPrefix("clankerbar: ")

	// Ctrl-C / SIGTERM cancels the context; the loop finishes the current
	// iteration's cleanup and stops, rather than dying mid-write.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	if len(args) < 2 {
		// Bare `clankerbar` is the fleet supervisor (CLA-525): every daemon
		// whose config file is in the config dir runs as a supervised child,
		// and SIGINT/SIGTERM to this process is a fleet-wide stop. The old
		// usage-and-exit-2 for a bare invocation is what this replaces.
		err = cli.Supervise(ctx, nil)
	} else {
		switch args[1] {
		case "run":
			err = cli.Run(ctx, args[2:])
		case "ctl":
			err = cli.Ctl(ctx, args[2:])
		case "doctor":
			err = cli.Doctor(ctx, args[2:])
		case "propose-config":
			err = cli.ProposeConfig(ctx, args[2:])
		case "dead-rate":
			err = cli.DeadRate(ctx, args[2:])
		case "supervise":
			// The explicit alias of the bare invocation, so scripts and
			// `--help` can name the supervisor.
			err = cli.Supervise(ctx, args[2:])
		case "version", "--version", "-v":
			fmt.Println("clankerbar", version.Current)
			return 0
		case "help", "--help", "-h":
			cli.Usage(os.Stdout)
			return 0
		default:
			fmt.Fprintf(os.Stderr, "clankerbar: unknown command %q\n\n", args[1])
			cli.Usage(os.Stderr)
			return 2
		}
	}
	if err != nil {
		log.Fatal(err)
	}
	return 0
}
