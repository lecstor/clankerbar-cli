package harness

import (
	"strings"
	"testing"
)

// A turn-capped session exits NON-ZERO and matches neither the usage-limit scan
// nor the transient one, so without its own classification the driver reads it as
// a genuine failure and ends the whole run — the phase backstop killing the
// daemon it was added to protect.
//
// The shape is from the real CLI (claude 2.1.226), running
//
//	claude -p "…" --max-turns 1 --output-format stream-json --verbose
//
// which exits 1 and emits:
//
//	{"type":"result","subtype":"error_max_turns","is_error":true,
//	 "result":null,"terminal_reason":"max_turns",
//	 "errors":["Reached maximum number of turns (1)"]}
func TestClaudeTurnCapped(t *testing.T) {
	tests := []struct {
		name string
		res  Result
		want bool
	}{
		{
			name: "the real max-turns result",
			res:  Result{ExitCode: 1, Raw: map[string]any{"terminal_reason": "max_turns", "is_error": true}},
			want: true,
		},
		{
			name: "a usage limit is not a turn cap",
			res:  Result{ExitCode: 1, Raw: map[string]any{"terminal_reason": "usage_limit"}},
			want: false,
		},
		{
			name: "an ordinary clean finish",
			res:  Result{ExitCode: 0, Raw: map[string]any{"terminal_reason": "end_turn"}},
			want: false,
		},
		{
			name: "no Raw at all (a launch failure)",
			res:  Result{ExitCode: 1},
			want: false,
		},
		{
			name: "a non-string terminal_reason does not panic or match",
			res:  Result{ExitCode: 1, Raw: map[string]any{"terminal_reason": 7}},
			want: false,
		},
		{
			// The scan reads the PARSED field, never the free text, so a task body
			// or an agent's narration mentioning the phrase cannot forge one — the
			// same defect class as CLA-258's "hit your".
			name: "the phrase in narration is not a turn cap",
			res:  Result{ExitCode: 1, Stdout: `{"type":"text","text":"I stopped at max_turns"}`},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (claude{}).TurnCapped(tt.res); got != tt.want {
				t.Errorf("claude.TurnCapped() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The cap has to actually reach the CLI. Nothing else observes argv, so a flag
// that stopped being emitted would leave the backstop inert with every test still
// green — and this one is undocumented in `claude --help`, so a version bump is
// precisely how it would go.
func TestClaudeArgsCarryTheTurnCap(t *testing.T) {
	got := strings.Join(claudeArgs(Invocation{Prompt: "work", MaxTurns: 40}), " ")
	if !strings.Contains(got, "--max-turns 40") {
		t.Errorf("argv does not carry the turn cap: %s", got)
	}

	// 0 must still be ABSENT, not passed as zero, which claude would read as a cap
	// of nothing. The premise CHANGED with CLA-343: the driver used to hand 0 down
	// for every unphased run (the "0 = uncapped" reading, which left one session
	// unbounded at 1093 turns); it now resolves a cap in EffectivePhases before
	// building the invocation, so a bare 0 here is a test-built invocation, not a
	// real unphased run — and the flag must still be absent for it.
	if got := strings.Join(claudeArgs(Invocation{Prompt: "work"}), " "); strings.Contains(got, "--max-turns") {
		t.Errorf("an uncapped invocation still emitted --max-turns: %s", got)
	}
}

// Neither adapter is passed MaxTurns, so no exit of theirs can be attributed to
// one. Pinned so a future adapter that DOES grow a cap has to say so here.
func TestAdaptersWithoutATurnCapNeverReportOne(t *testing.T) {
	res := Result{ExitCode: 1, Raw: map[string]any{"terminal_reason": "max_turns"}}
	if (codex{}).TurnCapped(res) {
		t.Error("codex reported a turn cap; it is never given MaxTurns")
	}
	if (opencode{}).TurnCapped(res) {
		t.Error("opencode reported a turn cap; it is never given MaxTurns")
	}
}

// The mid-stream token ceiling (CLA-343) is the same shape as the turn cap: a
// kill the driver must read as an orderly end, never as a failure. The marker
// is the ADAPTER's own, so the classifier must not fire on anything the CLI or
// an agent could emit.
func TestClaudeTokenCeilingHit(t *testing.T) {
	tests := []struct {
		name string
		res  Result
		want bool
	}{
		{
			name: "the adapter's own kill marker",
			res:  Result{ExitCode: -1, Raw: map[string]any{"terminal_reason": tokenCeilingReason}},
			want: true,
		},
		{
			name: "a real max-turns result is not a ceiling hit",
			res:  Result{ExitCode: 1, Raw: map[string]any{"terminal_reason": "max_turns", "is_error": true}},
			want: false,
		},
		{
			name: "a usage limit is not a ceiling hit",
			res:  Result{ExitCode: 1, Raw: map[string]any{"terminal_reason": "usage_limit"}},
			want: false,
		},
		{
			name: "no Raw at all (a launch failure)",
			res:  Result{ExitCode: 1},
			want: false,
		},
		{
			// The marker is a typed field the adapter writes, never free text — the
			// same defect class as CLA-258's "hit your" and the phrase in narration
			// test above.
			name: "the phrase in narration is not a ceiling hit",
			res:  Result{ExitCode: 1, Stdout: `{"type":"text","text":"token_ceiling_hit"}`},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (claude{}).TokenCeilingHit(tt.res); got != tt.want {
				t.Errorf("claude.TokenCeilingHit() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The other adapters never kill a session mid-stream, so no exit of theirs can
// be attributed to the ceiling. Pinned so a future adapter that DOES grow one
// has to say so here.
func TestAdaptersWithoutATokenCeilingNeverReportOne(t *testing.T) {
	res := Result{ExitCode: -1, Raw: map[string]any{"terminal_reason": tokenCeilingReason}}
	if (codex{}).TokenCeilingHit(res) {
		t.Error("codex reported a token ceiling hit; it never kills a session mid-stream")
	}
	if (opencode{}).TokenCeilingHit(res) {
		t.Error("opencode reported a token ceiling hit; it never kills a session mid-stream")
	}
}

// Phases gate on this, in config.Validate, so it is not merely descriptive: the
// day an adapter starts observing claims, flipping the flag is what turns phases
// on for it.
func TestCapabilitiesMatchWhatTheAdaptersActuallyDo(t *testing.T) {
	for name, caps := range map[string]Capabilities{
		"claude":   (claude{}).Capabilities(),
		"opencode": (opencode{}).Capabilities(),
	} {
		if !caps.TracksClaims {
			t.Errorf("%s does populate Result.Claim; TracksClaims must say so or phases are refused for a harness that supports them", name)
		}
	}
	if !(claude{}).Capabilities().HonoursMaxTurns {
		t.Error("claude passes --max-turns; HonoursMaxTurns must say so")
	}
	if (opencode{}).Capabilities().HonoursMaxTurns {
		t.Error("opencode claims to honour max_turns, but Invocation.MaxTurns never reaches its CLI")
	}
	if (codex{}).Capabilities().TracksClaims {
		t.Error("codex claims to track claims, but it never assigns Result.Claim — phases would stop after phase 1 on every task")
	}
}

// CapabilitiesOf is what config.Validate calls, before any adapter is built.
func TestCapabilitiesOfResolvesEveryRegisteredHarness(t *testing.T) {
	for _, name := range Names() {
		if _, ok := CapabilitiesOf(name); !ok {
			t.Errorf("CapabilitiesOf(%q) reported unknown for a registered harness", name)
		}
	}
	if _, ok := CapabilitiesOf("nope-not-a-harness"); ok {
		t.Error("CapabilitiesOf accepted an unregistered harness")
	}
}
