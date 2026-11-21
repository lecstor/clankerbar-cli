// Session-initiated handoff: a session ends its FINAL message with a marker line followed by the
// prompt for its successor, and the driver respawns a fresh session on that prompt — same task,
// same run-continuity rules as a phase resume — instead of moving to the next backlog iteration.
package loop

import (
	"fmt"
	"log"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// handoffChainCap bounds CONSECUTIVE handoff respawns within one phase, so a session cannot chain
// itself indefinitely: at the cap the emitted prompt is refused and the sequence falls back to the
// standard brief — the next configured phase, or the drain ending and the task returning to the
// queue, whose next pickup runs on the configured prompt either way. max_iterations and the token
// budget still bound the total across every phase, so this is never the only ceiling — just the
// one that stops a single phase's session from respawning itself forever.
const handoffChainCap = 3

// parseHandoff scans a session's final message for the handoff block: a line that is exactly
// config.HandoffMarker (surrounding whitespace ignored), followed by the successor's prompt. found
// reports that a marker LINE is present at all; refusal, when non-empty, says why the block cannot
// be honoured anyway (no prompt, or over the size cap).
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

// detectHandoff reads a finished session's result for a handoff it is safe to act on, returning the
// successor prompt or "". - only a CLEAN exit hands off.
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
	case res.Claim.TaskID == "":
		log.Printf("%siteration %d: handoff marker ignored — the driver observed no claim from this session, so it has no task/run ids to seed a successor's resume from; any lease the session took is left to expire, where the plane keeps a recorded branch attached as the takeover hand-off",
			labelOf(t), drainNum)
		return ""
	case res.Claim.Settled:
		log.Printf("%siteration %d: handoff marker ignored — the session settled %s itself (moved it off in_progress), so there is no run for a successor to resume",
			labelOf(t), drainNum, res.Claim.TaskID)
		return ""
	case refusal != "":
		log.Printf("%siteration %d: handoff refused — %s; falling back to the normal path",
			labelOf(t), drainNum, refusal)
		return ""
	}
	return prompt
}
