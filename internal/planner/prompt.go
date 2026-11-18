package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// The priming prompt's bound (studio-chat.md §3): a fresh session gets the
// newest stretch of history as prose, not the whole conversation. The plane is
// the transcript of record and `read_conversation` pages it, so the prompt
// carries enough to answer coherently and tells the model where the rest is.
const (
	maxPrimeHistoryMessages = 50
	maxPrimeHistoryBytes    = 128 << 10
)

// primePrompt composes the first turn of a fresh session: the role's brief, this
// turn's claim envelope, the bounded history, and the pending messages in full.
// The plane serves rows; the CLI writes the prose.
func (p *Planner) primePrompt(ctx context.Context, t Target, sess config.PlannerSession, claim *plane.ConversationClaim, ref string) string {
	history := p.olderHistory(ctx, t, claim)
	// claim.Context is the newest prior page (the claim asked for history:
	// "full"); older pages are prepended, so the whole slice runs oldest ->
	// newest and one trim can drop its head.
	all := append(append([]plane.ConversationMessage(nil), history...), claim.Context...)
	lines, trimmedAt := renderHistory(all)

	var b strings.Builder
	writeEnvelope(&b, sess, claim, ref)
	b.WriteString("\n## Conversation history (oldest to newest)\n")
	if len(claim.Context) == 0 && len(history) == 0 {
		b.WriteString("(no earlier messages; this is the conversation's first turn)\n")
	} else {
		for _, line := range lines {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if trimmedAt > 0 {
		fmt.Fprintf(&b, "\nOlder messages are not included here. If you need them, call `read_conversation` with conversationId %q and beforeSeq=%d, paging back until hasMore is false.\n", claim.ID, trimmedAt)
	}
	writePending(&b, claim)
	writeReplyInstruction(&b, claim, ref, sess)
	return b.String()
}

// resumePrompt composes a later turn of the same session: the brief, the
// envelope, and the pending messages. The history is already in the session's
// transcript, so repeating it would only spend tokens.
func (p *Planner) resumePrompt(sess config.PlannerSession, claim *plane.ConversationClaim, ref string) string {
	var b strings.Builder
	writeEnvelope(&b, sess, claim, ref)
	b.WriteString("\nYour session's transcript carries the earlier conversation; this turn continues it.\n")
	writePending(&b, claim)
	writeReplyInstruction(&b, claim, ref, sess)
	return b.String()
}

func writeEnvelope(b *strings.Builder, sess config.PlannerSession, claim *plane.ConversationClaim, ref string) {
	b.WriteString(sess.Prompt)
	b.WriteString("\n\n## This turn\n")
	fmt.Fprintf(b, "conversationId: %s\n", claim.ID)
	if claim.Title != "" {
		fmt.Fprintf(b, "title: %s\n", claim.Title)
	}
	fmt.Fprintf(b, "claimToken: %s\n", claim.ClaimToken)
	if ref != "" {
		fmt.Fprintf(b, "sessionRef: %s\n", ref)
	}
	fmt.Fprintf(b, "sessionHarness: %s\n", sess.Harness)
	if len(claim.Subjects) > 0 {
		if raw, err := json.Marshal(claim.Subjects); err == nil {
			fmt.Fprintf(b, "subjects: %s\n", raw)
		}
	}
	if claim.Note != "" {
		fmt.Fprintf(b, "note from the plane: %s\n", claim.Note)
	}
}

func writePending(b *strings.Builder, claim *plane.ConversationClaim) {
	fmt.Fprintf(b, "\n## Pending operator messages (%d)\n", len(claim.Messages))
	if len(claim.Messages) == 0 {
		b.WriteString("(none - the claim's watermark advanced past every message; answer briefly or ask what is needed)\n")
		return
	}
	for _, m := range claim.Messages {
		fmt.Fprintf(b, "\n### seq %d - %s (%s)\n", m.Seq, authorOf(m), m.AuthorSource)
		b.WriteString(m.Body)
		if m.Truncated {
			b.WriteString("\n\n(truncated by the plane's 4 KB served cap; the full body is on the stored row)")
		}
		b.WriteByte('\n')
	}
}

func writeReplyInstruction(b *strings.Builder, claim *plane.ConversationClaim, ref string, sess config.PlannerSession) {
	b.WriteString("\n## How to answer\n")
	fmt.Fprintf(b, "Call `reply_conversation` once, with conversationId %q, claimToken %q, and the body of your reply.",
		claim.ID, claim.ClaimToken)
	if ref != "" {
		fmt.Fprintf(b, " Pass sessionRef %q and sessionHarness %q so the next turn can resume this session.", ref, sess.Harness)
	} else {
		b.WriteString(" Omit sessionRef: this harness keeps no session handle.")
	}
	b.WriteString("\nDo not call any task claim tool. The claim token is this turn's only credential, and it answers every pending message above.\n")
}

// renderHistory renders the history into per-message lines, keeping the NEWEST
// stretch that fits both bounds. It returns the lines in order and the oldest
// seq that was kept, or 0 when everything given was kept.
func renderHistory(msgs []plane.ConversationMessage) ([]string, int) {
	if len(msgs) == 0 {
		return nil, 0
	}
	lines := make([]string, 0, len(msgs))
	bytes := 0
	keptFrom := len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		body := m.Body
		if body == "" {
			body = m.Snippet
		}
		line := fmt.Sprintf("[seq %d] %s (%s): %s", m.Seq, authorOf(m), m.AuthorSource, strings.TrimRight(body, "\n"))
		if m.Truncated {
			line += " …"
		}
		if len(lines) > 0 && (len(lines) >= maxPrimeHistoryMessages || bytes+len(line) > maxPrimeHistoryBytes) {
			break
		}
		lines = append(lines, line)
		bytes += len(line)
		keptFrom = i
	}
	// Reverse into oldest -> newest.
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	if keptFrom == 0 || keptFrom >= len(msgs) {
		return lines, 0
	}
	return lines, msgs[keptFrom].Seq
}

func authorOf(m plane.ConversationMessage) string {
	if m.AuthorName != "" {
		return m.AuthorName
	}
	return m.AuthorKind
}

// olderHistory walks read_conversation BACKWARDS from the oldest message the
// claim already carries, keeping the newest bounded stretch. A read failure is
// logged and dropped: a prime with the claim's first page is still a coherent
// answer, and failing the turn over optional history would strand the messages.
func (p *Planner) olderHistory(ctx context.Context, t Target, claim *plane.ConversationClaim) []plane.ConversationMessage {
	if t.Conversations == nil {
		return nil
	}
	before := oldestSeq(claim.Context)
	if before <= 0 {
		before = oldestSeq(claim.Messages)
	}
	var older []plane.ConversationMessage
	for page := 0; before > 1 && page < 20; page++ {
		res, err := t.Conversations.ReadConversation(ctx, claim.ID, before, 10)
		if err != nil {
			log.Printf("%splanner: could not read history before seq %d of conversation %s (%v) - priming with what the claim carried", labelOf(t), before, claim.ID, err)
			break
		}
		if len(res.Messages) == 0 {
			break
		}
		older = append(append([]plane.ConversationMessage(nil), res.Messages...), older...)
		if len(older) >= maxPrimeHistoryMessages {
			break
		}
		next := res.NextBeforeSeq
		if next <= 0 || next >= before {
			break
		}
		before = next
	}
	return older
}

func oldestSeq(msgs []plane.ConversationMessage) int {
	oldest := 0
	for _, m := range msgs {
		if m.Seq > 0 && (oldest == 0 || m.Seq < oldest) {
			oldest = m.Seq
		}
	}
	return oldest
}
