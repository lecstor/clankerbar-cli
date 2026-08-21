package harness

import (
	"fmt"
	"strings"
	"time"
)

// CLA-406: resurrecting a quietly-dead opencode session.
//
// The failure this answers was measured across 2026-08-20/21: the gateway drops
// a stream mid-generation — no terminal chunk, no usage, nothing on stderr —
// and opencode's own loop-exit bug (prompt.ts treating an "unknown" finish as a
// completed answer) ends the process cleanly with exit 0. The adapter sees the
// CLA-398 quiet-death signature: a final step whose reason is "unknown" and
// whose OWN usage is all-zero. Until now that was terminal.
//
// It does not have to be. The session transcript survives on opencode's side —
// only the in-flight turn died — so the session can be RESUMED in place via
// `opencode run --session <id>`, exactly as an interactive operator types
// "continue" after a cutoff. One small probe call risks recovering minutes to
// hours of accumulated session work; a failed probe costs only the wait it
// already took to reach this branch.
//
// The rules, agreed with the operator and recorded on the task:
//
//   - the resume prompt INFORMS THE AGENT of the convention (mirroring the
//     operator's own interactive pattern): you were cut off, your transcript is
//     intact up to the cut, confirm task ref + last action, then continue;
//   - coherence is decided MECHANICALLY: the reply must name the claimed task's
//     ref ("EZY-196" form). An amnesiac model cannot guess it; a genuinely
//     resumed session answers immediately. No judgement calls;
//   - ONE probe per death. A probe that fails or times out is not re-probed —
//     that death counts, and the existing dead-phase path takes over unchanged;
//   - every FUTURE death gets its own fresh probe (surviving one resurrection
//     does not spend the next);
//   - an overall cap bounds the whole loop, so a pathological night cannot turn
//     this into a perpetual churn of probe-and-die;
//   - a resurrected-and-recovered phase counts as never-dead for the dead-phase
//     counters; only FAILED probes reach that accounting.

const (
	// opencodeSessionIDKey is the Raw key carrying the opencode session id the
	// run streamed under — the handle a resurrection resumes into.
	opencodeSessionIDKey = "opencode_session_id"

	// opencodeResurrectionsKey is the Raw key counting completed resurrections
	// on the final Result. Nothing in the driver reads it today — the operator-
	// visible record is the `!! resurrection …` console lines, which land in
	// the iteration log — so this is correlation/debugging state, kept because
	// a merged Result that does not say it was stitched together invites the
	// next reader to assume one process ran.
	opencodeResurrectionsKey = "resurrections"

	// opencodeMaxResurrections caps the WHOLE loop per Invoke: after this many
	// successful-or-failed rounds the next quiet death is accepted as terminal
	// and the existing dead-phase path takes over. A bound named here rather
	// than unbounded "try again" because each round costs real seconds against
	// a gateway that is, by hypothesis, currently failing.
	opencodeMaxResurrections = 5

	// opencodeProbeTimeout time-boxes the probe itself. A healthy model answers
	// the one-line confirmation in single-digit seconds; 30s covers a slow one
	// plus gateway latency. A probe that cannot answer inside this window is a
	// death, not a pause — the alternative (waiting longer) is indistinguishable
	// from prodding a corpse.
	opencodeProbeTimeout = 30 * time.Second
)

// opencodeResumeBackoff is the pause between noticing a quiet death and
// sending the probe. The gateway is dropping streams under load; prodding it
// the same instant the corpse lands rolls the dice against the same
// congestion. Short enough not to matter when it works, long enough to step
// over the worst of a burst.
//
// A var rather than a const purely so the Invoke-level tests can shorten it:
// the production value above is the tested-against-default value.
var opencodeResumeBackoff = 25 * time.Second

// resumeTargets extracts what a resurrection needs from a quietly-dead Result:
// the session id to resume INTO, and the claimed task's ref to verify the
// resumed agent against. Either missing means no resurrection is possible:
//
//   - no session id: the stream died before its first parseable event, so there
//     is no session on opencode's side to continue — nothing to attach work to;
//   - no held claim: the session died before claiming, so there is no work at
//     stake worth a probe (those deaths land in the first seconds anyway) AND
//     no ref to verify coherence against. Both disqualifiers are deliberate.
func resumeTargets(res Result) (sid, ref string) {
	if res.Raw != nil {
		sid, _ = res.Raw[opencodeSessionIDKey].(string)
	}
	if res.Claim.Held() {
		ref = res.Claim.Ref
	}
	return sid, ref
}

// resurrectionProbePrompt is the prompt a resumed session is asked FIRST. It
// does two jobs at once, and both are load-bearing:
//
//   - it INFORMS the agent of what happened and of the convention, mirroring
//     the operator's own interactive pattern ("continue means you were cut
//     off") — an informed agent resumes confidently where an uninformed one
//     guesses at why the conversation has a hole in it;
//   - it demands a one-line answer naming the task ref, which turns "is this
//     agent actually resumed?" into a mechanical string match instead of a
//     judgement call. The ref is knowable only from the intact transcript: a
//     session that lost its context cannot produce it.
func resurrectionProbePrompt(ref string) string {
	return fmt.Sprintf(`Your previous response was interrupted mid-stream by the provider; your `+
		`session transcript is intact up to the interruption, and you are being resumed `+
		`in place. Confirm you are intact before continuing: reply with ONE line naming `+
		`the task ref you are working on (for example %q) and your last completed action. `+
		`Then stop — you will be told to continue.`, ref)
}

// continuePrompt is sent once the probe has verified coherence. Deliberately
// terse: everything the agent needs is already in its transcript.
const continuePrompt = "Confirmed. Continue exactly where you left off."

// probeCoherent decides whether a probe reply proves the session resumed with
// its context intact. Mechanical, case-insensitive containment of the claimed
// ref: the agent was asked to name its task and the only defensible answer is
// the one its own claim_task call made. Anything else — silence, a different
// ref, an apology, a re-read of the brief — fails, and the death stands.
func probeCoherent(reply, ref string) bool {
	return strings.Contains(strings.ToUpper(reply), strings.ToUpper(ref))
}

// opencodeResumeArgs builds the args for a continuation run into an existing
// session: the same dialect as opencodeArgs, plus `-s <id>` and WITHOUT the
// phase prompt — a continuation carries its own short message.
func opencodeResumeArgs(in Invocation, sid, prompt string) []string {
	args := []string{"run", "--format", "json", "-s", sid}
	if m := in.ModelArg(); m != "" {
		args = append(args, "--model", m)
	}
	return append(args, "--", prompt)
}

// mergeResume folds a successful continuation run's Result back into the base
// result of the original (dead) run, so what the driver sees is ONE session
// that was interrupted and carried on — not two.
//
// Field by field:
//
//   - Process outcome takes the CONTINUATION's wholesale: it is the run that
//     actually ended the merged session, and the dead run exited 0 with empty
//     stderr by definition of the quiet death. Keeping the dead run's zeroes
//     would hide a continuation that hit budget exhaustion (DetectLimit's scan
//     reads these streams), died non-zero, or was killed by a cap.
//   - Stdout/Stderr APPEND rather than replace: a post-mortem wants both halves
//     of the session. Each run's capture is already tail-bounded on its own, so
//     the sum stays bounded by two tails, not by the whole session.
//   - scans is reset: the classifier text just changed under any memoized
//     cache, and stale cache would feed DetectLimit/IsTransient pre-merge text.
//     Nil is a fully working, simply uncached Result.
//   - Untrusted/OutputDropped: an untrusted continuation makes the MERGED
//     result untrusted (the CLA-262 gates read this); dropped-output counts add.
//   - Tokens/CostUSD SUM: the budget must see every request the fleet made,
//     including the ones around the hole;
//   - UsageReported ORs: either run reporting is enough to count spend;
//   - FinalMessage takes the continuation's (the latest text wins, matching
//     the parser's own last-text-wins rule);
//   - Claim: the continuation's claim state stands only when it OBSERVED
//     something (held or settled) — `opencode run -s` streams only the new
//     turn, so an unobserved continuation knows NOTHING about the claim and
//     must not wipe the dead run's. Reports APPEND with sameClaim dedupe for
//     the same reason: neither run saw strictly more of the session than the
//     other, so both halves' delivery reports deserve to reach verification.
//   - Raw copies the continuation's (summing the numeric usage-breakdown keys
//     rather than overwriting them — Tokens sums, so the breakdown should too),
//     and TerminalReasonKey is RECOMPUTED rather than inherited: the key is
//     SHARED between the quiet-death marker and wall_clock_capped, so the
//     continuation's own terminal verdict — whatever it is — stands, and its
//     absence clears the dead run's. A clean continuation therefore clears the
//     quiet-death mark (the loop and the driver both read it), while a
//     continuation that itself died quietly keeps it — which is what lets the
//     caller's loop take its next round.
func mergeResume(base *Result, add Result) {
	base.ExitCode = add.ExitCode
	base.ExitSignal = add.ExitSignal
	base.Stdout += add.Stdout
	base.Stderr += add.Stderr
	base.scans = nil
	if add.OutputDropped > 0 {
		base.OutputDropped += add.OutputDropped
	}
	if add.Untrusted != "" {
		base.markUntrusted(add.Untrusted)
	}
	base.Tokens += add.Tokens
	base.CostUSD += add.CostUSD
	base.UsageReported = base.UsageReported || add.UsageReported
	if add.FinalMessage != "" {
		base.FinalMessage = add.FinalMessage
	}
	if add.Claim.Held() || add.Claim.Settled {
		base.Claim = add.Claim
	}
	for _, rep := range add.Reports {
		dup := false
		for i, have := range base.Reports {
			if have.sameClaim(rep) {
				// LATER WINS, matching settleReport's own rule: sameClaim
				// ignores Status, and the most recent status is the one that
				// says whether the work is headed to review or declared
				// landed. Keeping the earlier duplicate would drop exactly
				// that upgrade — an in_review declaration superseded by a
				// done — and skip merge attestation.
				base.Reports[i] = rep
				dup = true
				break
			}
		}
		if !dup {
			base.Reports = append(base.Reports, rep)
		}
	}
	if base.Raw == nil {
		base.Raw = map[string]any{}
	}
	for k, v := range add.Raw {
		if k == TerminalReasonKey {
			continue // recomputed below, never inherited blind
		}
		if nv, ok := v.(int); ok {
			if bv, ok := base.Raw[k].(int); ok {
				base.Raw[k] = bv + nv
				continue
			}
		}
		base.Raw[k] = v
	}
	if tr, _ := add.Raw[TerminalReasonKey].(string); tr != "" {
		base.Raw[TerminalReasonKey] = tr
	} else {
		delete(base.Raw, TerminalReasonKey)
	}
}
