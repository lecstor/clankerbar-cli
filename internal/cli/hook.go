package cli

// hook.go — `clankerbar hook emit`, the Claude Code hook adapter (CLA-605).
//
// An interactive Claude Code session has no CLI wrapper reading its stream, so
// its own PostToolUse hook runs this command once per tool call and the same
// activity events the driver emits (CLA-603) go to the plane. The command is
// deliberately quiet and total: it exits 0 and writes nothing to stdout on every
// path, because Claude Code waits for it and feeds its stdout back into the
// conversation. `docs/agent-events.md` carries the wiring and the bounds.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/hook"
)

const hookUsageText = `Usage: clankerbar hook emit [flags]

Reads one Claude Code PostToolUse payload on stdin and sends the matching
activity event to the plane's per-run event channel: a tool_call, a todo_change
for TodoWrite, or a clankerbar_mcp_call for an mcp__clankerbar__ tool. Nothing is
sent without CLANKERBAR_RUN_ID and CLANKERBAR_API_KEY in the hook's environment.

It exits 0 and sends nothing when either variable is unset, when --no-events or
CLANKERBAR_EVENTS=off is set, when the payload is not a PostToolUse naming a tool,
or when the project's events.activity dial is off (the plane refuses the batch).
A send is bounded: at most 2s waiting for another hook's send plus at most a 2s
request, after which the event is dropped. A failed send never fails the hook.

Wire it in settings.json as:  {"type":"command","command":"clankerbar hook emit"}
`

// hookFlags holds the parsed `hook emit` options.
type hookFlags struct {
	cfgPath  string
	noEvents bool
}

func newHookFlagSet(f *hookFlags) *pflag.FlagSet {
	fs := newFlagSet("hook emit")
	fs.StringVarP(&f.cfgPath, "config", "c", "", "config file naming the plane (default: ~/.config/clankerbar/config.json)")
	fs.BoolVar(&f.noEvents, "no-events", false, "send nothing, whatever CLANKERBAR_EVENTS and the project dial say")
	return fs
}

// Hook dispatches the `hook` command family.
func Hook(ctx context.Context, args []string) error {
	_ = ctx // one bounded send; no long-running work to cancel
	if len(args) > 0 {
		switch args[0] {
		case "help", "-h", "--help":
			fmt.Print(hookUsageText)
			return nil
		case "emit":
			return hookEmit(args[1:], os.Stdin)
		default:
			fmt.Fprint(os.Stderr, hookUsageText)
			return fmt.Errorf("unknown hook subcommand %q: the command is `clankerbar hook emit`", args[0])
		}
	}
	fmt.Fprint(os.Stderr, hookUsageText)
	return errors.New("clankerbar hook needs a subcommand: `clankerbar hook emit`")
}

// hookEmit is the whole hook path. stdin is a parameter so the tests can feed a
// payload without replacing the process's stdin.
func hookEmit(args []string, stdin io.Reader) error {
	var f hookFlags
	fs := newHookFlagSet(&f)
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return err
	}

	// The machine-local opt-out, exactly as the driver honours it. It is
	// checked before the environment, so a wired hook can be muted without
	// touching settings.json.
	if f.noEvents || eventsEnvOff() {
		return nil
	}
	runID := strings.TrimSpace(os.Getenv("CLANKERBAR_RUN_ID"))
	if runID == "" {
		return nil
	}
	apiKey := strings.TrimSpace(os.Getenv("CLANKERBAR_API_KEY"))
	if apiKey == "" {
		// Quiet like the run-id case: a session without the key sends nothing, and
		// a line per tool call would be noise the operator cannot act on from here.
		//
		// Deliberately env-only: the hook runs INSIDE a spawned session, and a
		// standalone review sets CLANKERBAR_AUTH_FILE to the null device so that
		// an isolated session cannot reach the operator's stored login
		// (CLA-1038). Reading the store here would defeat exactly that.
		return nil
	}

	cfg, err := config.Load(f.cfgPath)
	if err != nil {
		log.Printf("agent events: cannot load the config that names the plane (%v) — no event sent", err)
		return nil
	}
	origin := cfg.CredentialOrigin()
	if origin == "" {
		log.Print("agent events: no trusted plane origin in the config (set backlog_url) — no event sent")
		return nil
	}
	seqPath, err := hookSeqPath()
	if err != nil {
		log.Printf("agent events: cannot locate the hook's sequence file (%v) — no event sent", err)
		return nil
	}

	hook.Emit(hook.Config{
		RunID:   runID,
		APIKey:  apiKey,
		Origin:  origin,
		Harness: "claude",
		SeqPath: seqPath,
		Logf:    log.Printf,
	}, stdin)
	return nil
}

// hookSeqPath is the per-run sequence file the hook reserves from. It sits
// beside the loop's state root (not inside a workdir), so every session on the
// machine shares one counter file and a run's numbers cannot restart when the
// operator runs the hook from a different directory.
func hookSeqPath() (string, error) {
	root, err := config.StateRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(root), "hook", "run-event-seq.json"), nil
}
