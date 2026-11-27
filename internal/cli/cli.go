// Package cli wires the command-line surface to the loop driver.
package cli

import (
	"fmt"
	"io"
)

// Usage prints the top-level help.
func Usage(w io.Writer) {
	fmt.Fprint(w, `clankerbar — drive a coding agent through your clankerbar backlog, unattended.

Usage:
  clankerbar                    THE FLEET SUPERVISOR: reconcile this machine
                                against the plane's account-scoped roster: a
                                supervised child per entry with desiredState:
                                running (placement local), restarted with
                                backoff if it exits unexpectedly; a stopped
                                entry is drained by its STOP marker; and
                                SIGINT/SIGTERM drains the whole fleet. Each
                                child runs on a config generated from the base
                                config (~/.config/clankerbar/config.json) and
                                its roster entry. Same as "clankerbar supervise".
  clankerbar run [flags]        Start the loop: respawn fresh harness sessions that
                                work the backlog one task at a time, surviving usage
                                limits. The "prompt" config knob changes how much a
                                single session takes on.
  clankerbar ctl [flags]        Control a RUNNING loop by writing a marker into its
                                state dir: "ctl restart" re-execs the daemon at the
                                next phase seam or iteration boundary (--now kills the in-flight
                                session and releases its claim first); "ctl reload"
                                re-reads the config file without replacing anything.
                                A restart preserves the daemon's CURRENT environment
                                — it cannot conjure env it was never launched with.
  clankerbar doctor [flags]     Preflight the setup — config, harness, backlog
                                wiring, workdir, permissions. Exits non-zero on any
                                FAIL, so it gates a run: doctor && run.
  clankerbar login [flags]      Authenticate this machine through the browser device
                                flow and save the key in the owner-only local
                                credential store, so CLI commands and sessions work
                                without CLANKERBAR_API_KEY in their environment.
                                --host picks the origin (default: the config's
                                backlog_url, else https://clankerbar.com);
                                --project-scope asks for a project-bound key
                                instead of the account-wide user key.
  clankerbar logout [flags]     Forget the stored key for one origin, or every
                                origin with --all. Idempotent when already out.
  clankerbar whoami [flags]     Report which credential an origin would use (and
                                where it came from), then verify the plane still
                                accepts it. Exits non-zero when unauthenticated or
                                when the key is rejected.
  clankerbar mcp [flags]        Run a stdio-to-Streamable-HTTP MCP bridge on
                                stdin/stdout. One harness config — command
                                "clankerbar", args ["mcp"] — then works for every
                                harness and shares this machine's stored login,
                                so CLANKERBAR_API_KEY never enters a harness config
                                or an agent session environment. stdout carries
                                only newline-delimited JSON-RPC responses;
                                diagnostics go to stderr. --host picks the origin,
                                --project pins one project slug, and --multi uses
                                the account-wide /mcp surface. Without either, a
                                .clankerbar.json found upward from the working
                                directory supplies the project.
  clankerbar propose-config     Import the local config's movable dials (harness,
  [flags]                       model, tiers, budget, escalation) into the plane as a
                                PENDING proposal the operator ratifies in the console.
  clankerbar dead-rate          Scan the iteration logs and report, per day, per
                                phase and per harness, how many sessions ran and
                                how many died producing nothing (the dead-phase
                                rate).
  clankerbar upload <file>      Upload a file (screenshot, video, HTML, or a
                                markdown/text/json/pdf document) as a project
                                asset and print exactly asset:<id> on stdout.
                                The byte-free path: only the file's path, type,
                                size and sha256 leave the shell — its bytes are
                                sent by this process, never through a model or a
                                tool argument. --task attributes the file to a
                                task; --type overrides the content type its
                                extension implies.
  clankerbar download <assetId> Fetch an uploaded asset and stream it to stdout,
    [-o <path>]                 or to -o <path> (an existing directory writes the
    [--project <slug>]          plane's filename inside it, and stdout carries the
    [--sha256 <hex>]            path written). --project targets a project the
                                config does not name; --sha256 verifies the bytes
                                against a 64-character hash and refuses a
                                mismatch. Same byte-free path, in reverse: the
                                file is streamed by this process, never through a
                                model.
  clankerbar deck <file.html>   Upload a self-contained review deck for a task
    --task <ref|id>             and print its review URL. Same byte-free path:
                                the HTML goes up as a text/html asset, then
                                binds to the task as its review deck.
  clankerbar hook emit         Read one Claude Code PostToolUse payload on stdin
                               and send it to the plane's per-run activity channel.
                               Exits 0 silently when the session is not bound to a
                               run (CLANKERBAR_RUN_ID / CLANKERBAR_API_KEY unset).
  clankerbar review run        Review one local base..head with the driver's own
    --base <commit>            review, outside any task, claim or plane, and
                               print the findings as JSON. Runs in a temporary
                               detached worktree; the repository is never
                               modified. --method maps-session runs the map
                               method on a session that can continue turns.
  clankerbar review map        Print the maps-session plan for one local
    --base <commit>            base..head as JSON without starting a session:
                               the brief-plus-first-chunk prompt and every
                               follow-up turn, for an agent that sends them
                               itself. Takes the same repo/base/head, chunk and
                               JSON flags as review run, and spends nothing.
                               The checkout must be at --head.
  clankerbar supervise roll    Roll the fleet onto the version THIS binary
                                runs, one child at a time: install the new
                                build at the fleet's launch path and run this
                                from it. Each child gets a RESTART marker, drains
                                at its next phase seam or iteration boundary, and is verified — via
                                its next beacon — to report the new version
                                before the next child is touched; a child that
                                does not come back on it halts the roll.
  clankerbar supervise replace  Replace the SUPERVISOR itself in place: run
                                this from the new build after installing it at
                                the fleet's launch path (and after the roll).
                                The running supervisor hands its live children
                                to the new image and execs the launch path at
                                once — no child is signalled and nothing is
                                drained. The new image adopts them by pid and
                                rolls each older-version child onto itself at
                                that child's own phase seam or iteration
                                boundary; idle children come back at once. A
                                unit restart still drains, and is the fallback.
  clankerbar version            Print the version.
  clankerbar help               Show this help.

Run 'clankerbar supervise --help', 'clankerbar run --help', 'clankerbar ctl
--help', 'clankerbar doctor --help', 'clankerbar login --help', 'clankerbar
logout --help', 'clankerbar whoami --help', 'clankerbar mcp --help',
'clankerbar propose-config --help', 'clankerbar upload --help', 'clankerbar
download --help', 'clankerbar deck --help', 'clankerbar hook --help',
'clankerbar review run --help' or 'clankerbar review map --help' for their
flags.
`)
}
