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
)

// version is overwritten at release time via -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	log.SetFlags(log.Ltime)
	log.SetPrefix("clankerbar: ")

	if len(os.Args) < 2 {
		cli.Usage(os.Stderr)
		os.Exit(2)
	}

	// Ctrl-C / SIGTERM cancels the context; the loop finishes the current
	// iteration's cleanup and stops, rather than dying mid-write.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "run":
		err = cli.Run(ctx, os.Args[2:])
	case "doctor":
		err = cli.Doctor(ctx, os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("clankerbar", version)
	case "help", "--help", "-h":
		cli.Usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "clankerbar: unknown command %q\n\n", os.Args[1])
		cli.Usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}
