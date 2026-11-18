package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// The composed prompt is the ONLY mechanism by which a turn is answered: if it
// stops naming reply_conversation (or drops the claim token), every turn goes
// unanswered while the loop tests stay green. These pin the instruction and the
// history bounds.

func promptClaim() *plane.ConversationClaim {
	return &plane.ConversationClaim{
		ID:           "conv-1",
		PendingCount: 1,
		Messages:     []plane.ConversationMessage{{ID: "m1", Seq: 2, AuthorKind: "operator", AuthorSource: "console", AuthorName: "alice", Body: "what is next?"}},
		Context:      []plane.ConversationMessage{{Seq: 1, AuthorKind: "operator", AuthorSource: "console", Body: "hello"}},
		ClaimToken:   "conv-1.secret",
	}
}

func assertReplyInstruction(t *testing.T, prompt string) {
	t.Helper()
	for _, want := range []string{
		"reply_conversation",
		`conversationId "conv-1"`,
		`claimToken "conv-1.secret"`,
		`sessionRef "ref-9"`,
		`sessionHarness "claude"`,
		"Do not call any task claim tool",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not carry %q:\n%s", want, prompt)
		}
	}
}

func TestPrimePromptCarriesTheReplyInstruction(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude"})
	sess := config.PlannerSession{Harness: "claude", Prompt: "You are the planner."}
	prompt := f.planner.primePrompt(context.Background(), f.planner.targets[0], sess, promptClaim(), "ref-9")
	assertReplyInstruction(t, prompt)
	for _, want := range []string{"You are the planner.", "## Conversation history", "what is next?"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prime prompt does not carry %q:\n%s", want, prompt)
		}
	}
}

func TestResumePromptCarriesTheReplyInstruction(t *testing.T) {
	f := newFixture(t, map[string]any{"harness": "claude"})
	sess := config.PlannerSession{Harness: "claude", Prompt: "You are the planner."}
	prompt := f.planner.resumePrompt(sess, promptClaim(), "ref-9")
	assertReplyInstruction(t, prompt)
	if strings.Contains(prompt, "## Conversation history") {
		t.Errorf("a resume prompt re-sent the history the session already has:\n%s", prompt)
	}
}

// The shipped brief's half of the contract: no task claims, one reply, and the
// two conversation tools named. The envelope supplies the per-turn values, so a
// brief that quietly told the model to claim tasks would break the role.
func TestBuiltinPlannerBriefPinsTheTurnRules(t *testing.T) {
	for _, want := range []string{
		"reply_conversation",
		"never call claim_task",
		"ONE reply",
		"read_conversation",
	} {
		if !strings.Contains(config.BuiltinPlannerPrompt, want) {
			t.Errorf("built-in planner brief does not carry %q", want)
		}
	}
}

func TestRenderHistoryHonoursBothBounds(t *testing.T) {
	msgs := func(n, body int) []plane.ConversationMessage {
		out := make([]plane.ConversationMessage, n)
		for i := range out {
			out[i] = plane.ConversationMessage{Seq: i + 1, AuthorKind: "operator", AuthorSource: "console", Body: strings.Repeat("x", body)}
		}
		return out
	}

	t.Run("message count keeps the newest 50", func(t *testing.T) {
		lines, trimmedAt := renderHistory(msgs(60, 10))
		if len(lines) != maxPrimeHistoryMessages {
			t.Fatalf("kept %d lines, want %d", len(lines), maxPrimeHistoryMessages)
		}
		if trimmedAt != 11 {
			t.Errorf("trimmedAt = %d, want the oldest kept seq 11", trimmedAt)
		}
	})

	t.Run("byte cap keeps the newest stretch that fits", func(t *testing.T) {
		lines, trimmedAt := renderHistory(msgs(4, 40000))
		if len(lines) != 3 {
			t.Fatalf("kept %d lines, want 3 that fit %d bytes", len(lines), maxPrimeHistoryBytes)
		}
		if trimmedAt != 2 {
			t.Errorf("trimmedAt = %d, want the oldest kept seq 2", trimmedAt)
		}
	})

	t.Run("everything kept reports no trim", func(t *testing.T) {
		if _, trimmedAt := renderHistory(msgs(3, 10)); trimmedAt != 0 {
			t.Errorf("trimmedAt = %d, want 0 when nothing was dropped", trimmedAt)
		}
	})
}
