// Session-initiated handoff (CLA-352): a session ends its FINAL message with a
// marker line followed by the prompt for its successor, and the driver respawns
// a fresh session on that prompt — same task, same run-continuity rules as a
// phase resume — instead of moving to the next backlog iteration. The phase
// split (v0.5.0) is this idea at two fixed, driver-chosen points; this
// generalises it to points the session chooses.
//
// The prompt is parsed out of the final message, NOT a file: the state dir is
// deliberately outside every session workdir (CLA-259) precisely so sessions
// cannot write control markers, and the result stream is a channel the driver
// already consumes.
//
// A session-authored respawn prompt is self-directed prompt injection, so it is
// honoured only inside three non-negotiable guards, enforced in drainPhases:
// a respawn counts as an iteration against max_iterations and the token budget
// (checked BEFORE the respawn), an over-size prompt is refused with a logged
// fallback, and a chain of consecutive handoffs is capped.
package loop

import (
	"fmt"
	"log"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// handoffChainCap bounds CONSECUTIVE handoff respawns within one phase, so a
// session cannot chain itself indefinitely: at the cap the emitted prompt is
// refused and the sequence falls back to the standard brief — the next
// configured phase, or the drain ending and the task returning to the queue,
// whose next pickup runs on the configured prompt either way.
//
// The chain resets at a phase boundary (drainPhases resets `chain` to 0
// whenever a phase ends without an accepted handoff), so a multi-phase
// sequence can chain up to this cap PER PHASE, not once for the whole task.
// max_iterations and the token budget still bound the total across every
// phase, so this is never the only ceiling — just the one that stops a single
// phase's session from respawning itself forever.
const handoffChainCap = 3

// parseHandoff scans a session's final message for the handoff block: a line
// that is exactly config.HandoffMarker (surrounding whitespace ignored),
// followed by the successor's prompt.
//
// found reports that a marker LINE is present at all; refusal, when non-empty,
// says why the block cannot be honoured anyway (no prompt, or over the size
// cap). The two are separate so a refusal can be logged as what it is — a
// handoff the session asked for and the driver declined — rather than
// disappearing into "no marker".
//
// The LAST marker line wins, because the block is defined as ending the
// message: a session that quotes the marker while discussing it and then emits
// a real block must get the real one, and everything above the last marker is
// by definition not the successor's prompt. A marker mentioned inline in prose
// does not match — it is not a line of its own.
//
// A marker line must be UNINDENTED, and must not sit inside a fenced code
// block: a genuine emission is neither. Indentation catches a quotation
// inside a list item; the fence tracking catches the other natural way a
// session quotes the mechanism (or explains why it decided against a
// handoff) — a ``` block, which is itself unindented and would otherwise
// match. Both are real shapes a session explaining itself produces, and
// neither may be mistaken for a real handoff block.
func parseHandoff(final string) (prompt string, found bool, refusal string) {
	at := -1
	inFence := false
	lines := strings.Split(final, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if !inFence && strings.TrimRight(line, " \t\r") == config.HandoffMarker {
			at = i
		}
	}
	if at < 0 {
		return "", false, ""
	}
	prompt = strings.TrimSpace(strings.Join(lines[at+1:], "\n"))
	if prompt == "" {
		return "", true, "the marker line carries no successor prompt"
	}
	if len(prompt) > config.HandoffPromptMax {
		return "", true, fmt.Sprintf("the successor prompt is %d bytes, over the %d-byte cap — refused rather than truncated, because a truncated brief is a successor confidently working half an instruction",
			len(prompt), config.HandoffPromptMax)
	}
	return prompt, true, ""
}

// detectHandoff reads a finished session's result for a handoff it is safe to
// act on, returning the successor prompt or "".
//
// Each condition is load-bearing, and they are checked here rather than at the
// seam so every refusal is logged as itself:
//
//   - an untrusted stream is not read for a handoff at all, for the same reason
//     it is not read for claim state (CLA-262): the block may be cut mid-prompt,
//     and half a session-authored prompt is worse than none. The untrusted path
//     already logs loudly, so nothing is added here.
//   - only a CLEAN exit hands off. A turn-capped or failed session's final
//     message is wherever it happened to stop, not a deliberate ending.
//   - the session must still HOLD its task: the successor resumes the same run,
//     so a settled or never-claimed task leaves nothing to resume.
func detectHandoff(drainNum int, t Target, res harness.Result) string {
	prompt, found, refusal := parseHandoff(res.FinalMessage)
	if !found || res.Untrusted != "" {
		return ""
	}
	switch {
	case res.ExitCode != 0:
		log.Printf("%siteration %d: handoff marker ignored — the session did not end cleanly (%s), so its final message is not a deliberate handoff",
			labelOf(t), drainNum, res.ExitString())
		return ""
	case !res.Claim.Held():
		log.Printf("%siteration %d: handoff marker ignored — the session no longer holds a task, so there is no run for a successor to resume",
			labelOf(t), drainNum)
		return ""
	case refusal != "":
		log.Printf("%siteration %d: handoff refused — %s; falling back to the normal path",
			labelOf(t), drainNum, refusal)
		return ""
	}
	return prompt
}
