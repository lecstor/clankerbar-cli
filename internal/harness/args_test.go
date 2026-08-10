package harness

import (
	"strings"
	"testing"
)

// The prompt is a bare POSITIONAL for codex (`codex exec <prompt>`) and opencode
// (`opencode run <message..>`), and a positional that begins with `-` is read as
// a flag. The failure mode is not a corrupted message - it is a session that runs
// with NO message at all, or blocks reading one from stdin. On codex there is a
// second one: `codex exec` has SUBCOMMANDS (resume, fork, review) in the prompt's
// old position, so a prompt of exactly `resume` was taken as one and the session
// ran a stored conversation instead of the drain.
//
// It is NOT that the pinned `--sandbox`/`--ask-for-approval` posture used to
// survive on last-wins - the commit that added this said so and was wrong. Those
// are clap 4 Option args (ArgAction::Set): a repeated occurrence is an ERROR, not
// a silent override, so the old shape failed loud. The terminator preserves that
// property; it did not introduce it.
//
// Nothing from the BACKLOG can get here: Invocation.Prompt is set once from the
// config's `prompt`, there is no --prompt flag, and the backlog client decodes
// counts rather than strings. So this closes the config-file path (the CLA-260
// class) rather than a live injection hole - which is exactly why it is worth one
// token and a test instead of a parser.
//
// claude is not in this table because it has no positional to protect: the prompt
// is the VALUE of `-p` (claude.go), so a leading `--` is consumed as that flag's
// argument and there is nothing a terminator would add.
var terminatorAdapters = []struct {
	name string
	args func(Invocation) []string
	// wantFlags is every argument the adapter emits for a drain, which must all
	// sit AHEAD of the terminator. A flag that fell behind it is not merely
	// misplaced: the child reads it as more message, so the posture it sets is
	// silently not applied at all - the sandbox pin would be absent rather than
	// overridden, and codex's own default would decide the session's write access.
	wantFlags []string
}{
	{"codex", codexArgs, []string{
		"--json", "--sandbox", "workspace-write", "--ask-for-approval", "never", "-m", "some/model",
	}},
	{"opencode", opencodeArgs, []string{"--format", "json", "--model", "some/model"}},
}

// promptsTheParserWouldSteal are the shapes that actually bite: a bare
// terminator, a long flag the target CLI really has (so it would be ACCEPTED, not
// rejected), a short one, a lone `-`, and a subcommand name.
var promptsTheParserWouldSteal = []string{
	"--",
	"--json",
	"--sandbox",
	"--format",
	"-m",
	"--help Work the backlog.",

	// A lone `-` is the one case in the original threat model the first fix did
	// not close, because it is not a flag: both CLIs read it as "take the
	// instructions from stdin", and cmd.Stdin is nil for every Invocation the loop
	// builds (codex.go, opencode.go), so the session gets an immediate EOF and no
	// instructions at all. Degenerate as a config value, free to cover.
	"-",

	// Not flag-shaped, and stolen anyway: `codex exec` has subcommands - resume,
	// fork, review - in the position the prompt used to sit in, so this ran a
	// stored conversation rather than the drain. Kept in the shared table because
	// costing opencode one extra case is cheaper than a second table.
	"resume",
}

func TestAPromptTheParserWouldStealArrivesAsTheMessage(t *testing.T) {
	for _, a := range terminatorAdapters {
		for _, prompt := range promptsTheParserWouldSteal {
			t.Run(a.name+"/"+prompt, func(t *testing.T) {
				args := a.args(Invocation{Prompt: prompt, Model: "some/model"})

				// The whole property: the prompt is the final argument and the one
				// before it is the terminator. Positional, not searched - a prompt of
				// exactly "--" is one of the cases below, and looking for the last "--"
				// would find the PROMPT and call the test green.
				if len(args) < 2 {
					t.Fatalf("%s built %q, which cannot carry a terminated prompt", a.name, args)
				}
				cut := len(args) - 2
				if args[cut] != "--" {
					t.Fatalf("%s built %q with no `--` terminator before the prompt - the child parses it as a flag", a.name, args)
				}
				if args[len(args)-1] != prompt {
					t.Fatalf("%s built %q: want the prompt last, got %q", a.name, args, args[len(args)-1])
				}

				// And every argument the adapter meant as a FLAG is in front of it -
				// searched in the head only, so a flag-shaped prompt cannot stand in for
				// the flag it imitates and mark this green.
				head := args[:cut]
				for _, want := range a.wantFlags {
					if indexOf(head, want) < 0 {
						t.Errorf("%s built %q: %q is missing from the flags ahead of the terminator", a.name, args, want)
					}
				}
			})
		}
	}
}

// The terminator must not cost the ordinary case. A normal drain prompt still
// arrives whole, and the flags either side of it are unchanged.
func TestOrdinaryPromptIsUnaffectedByTheTerminator(t *testing.T) {
	const prompt = "Work the backlog."
	for _, a := range terminatorAdapters {
		t.Run(a.name, func(t *testing.T) {
			args := a.args(Invocation{Prompt: prompt, Model: "some/model"})
			if args[len(args)-1] != prompt {
				t.Errorf("%s built %q: the prompt must be the final argument", a.name, args)
			}
			if got := strings.Count(strings.Join(args, " "), prompt); got != 1 {
				t.Errorf("%s built %q: the prompt should appear exactly once, got %d", a.name, args, got)
			}
			if indexOf(args, "some/model") < 0 {
				t.Errorf("%s built %q: the model must still be passed through", a.name, args)
			}
		})
	}
}

// The probe is the same argv builder on a different branch, so it needs the same
// guarantee - and it must still be READ-ONLY. A probe that lost its sandbox pin
// to a misordered flag would be a write-capable session polling every 30 minutes
// for as long as a usage cap lasts.
func TestProbeKeepsItsPinnedPostureAheadOfTheTerminator(t *testing.T) {
	for _, a := range terminatorAdapters {
		t.Run(a.name, func(t *testing.T) {
			// The real drain prompt is deliberately flag-shaped here: the probe must
			// substitute its own trivial one, and must not leak this past the
			// terminator either.
			args := a.args(Invocation{Probe: true, Prompt: "--json"})
			if n := len(args); n < 2 || args[n-2] != "--" || args[n-1] != "." {
				t.Fatalf("%s probe = %q, want a `--`-terminated trivial prompt", a.name, args)
			}
		})
	}

	// codex pins its read-only sandbox on this path; that pin now sits ahead of the
	// terminator rather than surviving on last-wins behind the positional. A probe
	// that lost it would be a write-capable session polling every 30 minutes for as
	// long as a usage cap lasts.
	codexProbe := codexArgs(Invocation{Probe: true, Prompt: "--json"})
	if i := indexOf(codexProbe, "read-only"); i < 0 || i > len(codexProbe)-2 {
		t.Errorf("codex probe = %q: --sandbox read-only must be pinned ahead of the terminator", codexProbe)
	}
	if indexOf(codexProbe, "workspace-write") >= 0 {
		t.Errorf("codex probe = %q: a probe must never be write-capable", codexProbe)
	}
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}
