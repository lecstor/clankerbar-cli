package loop

import (
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// panelState is what a phase following a review panel needs to know about it
// (CLA-639): the round the panel's members' records landed in, and — for the
// members that recorded NOTHING — why, keyed by reviewer role. Zero when the
// phase does not follow a panel, or when the panel never got a record into a
// round — which is exactly when there is no merged list to build.
type panelState struct {
	round int
	// failed maps a member's role to the reason it recorded nothing, so the fix
	// brief names the incomplete members AND what happened to them, not only
	// that the list is short. The entry also carries whether the seat was a
	// TRIAL seat (CLA-784), so the fix brief can mark it `(optional)` exactly as
	// the daemon log does — a fixer must not read a lost trial seat as a lost
	// required reviewer.
	failed map[string]panelMemberFailure
	// recorded reports that a REQUIRED member has already recorded in this
	// panel (CLA-784). The last panel member's seam closes the round; an
	// OPTIONAL seat may only trigger that close when a required member has
	// actually recorded, because a round where only trial seats recorded is no
	// review at all and must stay sealed — the same path as a panel where none
	// recorded.
	recorded bool
}

// panelMemberFailure is a panel member that produced no durable record: the
// reason the daemon log and the fix brief name, and whether the seat was an
// OPTIONAL (trial) one. The optional flag is what lets the fix brief say
// `review-N (optional)` — the same marker the INCOMPLETE log line carries
// (CLA-784 review 1.review-1.2 / 1.review-3.1).
type panelMemberFailure struct {
	reason   string
	optional bool
}

// phaseEnd is how a phase reports what it left behind, so drainPhases can decide
// whether to run the next one and what is still owed a handback.
//
// The three fields are deliberately independent. A phase can end holding a claim
// without it being a checkpoint (it was the last one), and can observe claim
// state without owing anything (it settled the task, or already handed it back).
type phaseEnd struct {
	// claim is the final attempt's result when that result carried claim state at
	// all — the thing a deferred handback acts on, and the source of the ids the
	// next phase's prompt is seeded with. nil when the phase observed nothing:
	// a launch failure, or an adapter that does not track claims. nil means "I
	// know nothing", NOT "nothing is owed" — the caller keeps what it had.
	claim *harness.Result

	// released reports that this phase already handed a genuinely-held claim back
	// itself, so nothing is owed and a second handback would post `ready` over
	// whatever came next.
	released bool

	// runLost reports that the phase ended because the plane said the run no
	// longer holds the task; the drain ends and nothing is salvaged or handed back.
	runLost bool

	// checkpoint reports an orderly end still holding the task, with the sequence
	// meant to continue into the next phase.
	checkpoint bool

	// dead reports the CLA-386 dead-phase signature: the session's final step
	// finished with reason "unknown" and no branch was recorded on the task, so
	// the phase produced nothing. It is a FAILED phase, not a completed one —
	// the seam must not hand it on to the next phase's brief. The claim has
	// already been released (a retry re-claims); drainPhases owns the retry-once-
	// then-park decision and its per-task budget.
	dead bool

	// producedNothing is the WIDER dead classification (the tally's, CLA-402):
	// the same signature WITHOUT the !last conjunct end.dead carries, because a
	// dead LAST phase is still a death even though no retry/park machinery can
	// apply to it. Only the fleet record reads this field (CLA-466) — the
	// iteration record must say `dead` when the drain ended on exactly such a
	// final-phase death — so it deliberately does not feed the counters or the
	// seam, whose !last gating is pinned by its own tests.
	producedNothing bool

	// deadBooked reports that the run's dead-phase tally (CLA-402) already
	// counted THIS exit as a dead phase: either tallyDead's own predicate fired
	// or the seam's held-but-empty branch booked it (tallyEmpty). The review
	// panel's member handler books a failed member into the same tally (CLA-639
	// M4), and this is how it avoids counting one exit twice — see
	// notePanelMemberFailure's caller.
	deadBooked bool

	// session is the harness session this pass spawned, for the fleet record's
	// sessions list (CLA-630): the phase label, the resolved harness and the
	// resolved model the spawn line named. Only the fleet record reads it.
	// nil means the pass spawned nothing (a harness or spawn-dir resolution
	// refused before any Invoke), which is not a session to report.
	session *fleet.Session

	// empty reports a held-but-empty exit — the 2026-08-24 fleet-incident shape:
	// the phase ended cleanly still holding the task with NO durable,
	// origin-reachable branch evidence on the plane record. It is a FAILED phase,
	// not a checkpoint: the seam must not hand it to the review brief. The claim
	// has already been released (a fresh session re-claims) or left to expire (a
	// recorded-but-unreachable branch keeps its takeover hand-off); drainPhases
	// owns the one-bounded retry. Deliberately distinct from dead, which keys on
	// the "unknown" finish reason — the incident's sessions finished with reason
	// "stop", so the dead signature alone did not catch them. Set on a HANDOFF
	// too, so a refused handoff is re-judged as the bare exit it really is.
	empty bool

	// emptyWhy is the evidence-gate reason when empty fired, for the "not a
	// checkpoint" log line ("no branch recorded on the task", the failing
	// branch check's detail, or — since CLA-497 — the plane record's answer
	// when the branch-less evidence forms were consulted and showed nothing).
	emptyWhy string

	// branch is the branch name when this phase checkpointed — the
	// plane-record evidence the checkpoint is gated on (CLA-457), carried here
	// so the successor's brief can name it instead of asserting an unverified
	// phase-1 success. Folded onto the carried claim by the seam, which marks
	// it UNVERIFIED (CLA-576) when the origin could not be read; the brief
	// selection reads that mark, so the branch reaches the successor named but
	// never presented as verified.
	branch string

	// releasedToQueue reports that this phase's handback ACTUALLY returned the
	// claim to the ready queue (releaseHeldClaim succeeded), as opposed to
	// leaving it to the takeover path or a failed release. It is the only signal
	// a retry may re-claim against: a release the plane refused leaves the lease
	// live, and a fresh claiming session would only be refused (CLA-457).
	releasedToQueue bool

	// handoff is the successor prompt the session emitted in its final message's
	// handoff block, already past the parse-time guards (marker present, prompt
	// non-empty and under the size cap, clean exit, trusted stream, task still
	// held — see detectHandoff). Non-empty implies checkpoint: the lease is
	// being held for the successor. drainPhases owns the remaining guards — the
	// chain cap, max-iterations, the budget breaker — and either respawns on it
	// or falls back to the standard path (CLA-352).
	handoff string

	// panelMember reports that this pass was one reviewer of a review panel
	// (CLA-639), and panelLast that it was the panel's last member. drainPhases
	// reads the pair to tell a skippable reviewer — a dead, capped or
	// record-less member is recorded as failed and the round still closes — from
	// a lone review phase, whose failure keeps the shipped fail-closed ending.
	panelMember bool
	panelLast   bool
	// panelOptional reports that this member is an OPTIONAL (trial) seat of the
	// review panel (CLA-784). Its failure is recorded against the member and
	// nothing more: drainPhases must not end the sequence, hand the shared claim
	// back, or take a run-level wait for it. False for every phase that is not a
	// panel member and for a required member.
	panelOptional bool
	// reviewRecorded reports that this pass's review seam gate PASSED — the
	// member left a durable record behind. It is what the panel handler's
	// "recorded or been recorded as failed" decision reads.
	reviewRecorded bool

	// noMCP reports that this pass ended with NO review record and NO successful
	// clankerbar MCP call at all (CLA-703). That is an infrastructure miss, not
	// a reviewer's verdict: the tools the brief tells it to use were not there.
	// The panel handler names it distinctly and keeps it off the member's
	// failure streak; drainPhases books it apart from dead/capped so analytics
	// can exclude it. It is set only on the FINAL attempt — the one bounded
	// retry the same pass already took is not reported as the member's outcome.
	noMCP bool

	// toolLoop is the cause when the adapter killed this pass's session for
	// repeating a failing tool call (CLA-739), "" otherwise. It is also
	// `idle: <IdleKilled>` when the adapter killed the session for silence. Both
	// are driver kills: the pass is also dead (when !last and held), and this
	// carries the NAME of the death, so the panel handler records the member as
	// failed with the cause rather than with "died producing nothing".
	toolLoop string

	// reviewCloseFailed reports that this pass's review seam gate passed far
	// enough to find a SEALED round, but the driver's own close of that round
	// failed. The record exists (the reviewer did record), so the panel must not
	// name the member INCOMPLETE or charge its failure streak; the drain must
	// end without spawning the fix, because the fix would run against findings
	// the plane still holds sealed. It is the close-failure half of
	// reviewRecorded, kept separate because the evidence gate sets both to the
	// same value everywhere else.
	reviewCloseFailed bool

	// reviewRound is the review round the seam gate MATCHED this pass (CLA-637),
	// zero when the phase is not review-shaped or no record matched. The round is
	// normally closed inside drainPhase the moment the stage is over — but a
	// DETECTED handoff defers that close to the successor's seam, and the
	// remaining handoff guards (chain cap, max-iterations) live in drainPhases:
	// when they REFUSE the respawn, the stage is over after all, and drainPhases
	// closes this round before advancing — otherwise the fix phase would spawn
	// against findings the plane still holds sealed, which it can neither read
	// nor answer (finding_sealed).
	reviewRound int

	// verifyChecked reports that this pass was a fix-verification round and the
	// driver evaluated its seam (CLA-638): matched the durable record, closed the
	// round, and read the task's open MUSTs back. It is set even when the
	// evaluation FAILED, so drainPhases can tell "verification was attempted and
	// could not be judged" from "this was an ordinary phase".
	verifyChecked bool
	// verifyFailure is the reason a verification seam could not be evaluated
	// (no record this run wrote, an unreadable task, a round that would not
	// close). Non-empty means the driver must park rather than hand over or
	// loop: an unverifiable round cannot be the gate.
	verifyFailure string
	// verifyRound is the round the verification record landed in — the plane
	// assigns it, the driver reads it back.
	verifyRound int
	// verifyOpenMusts is every open MUST on a RELEASED record, by plane finding
	// id, as of AFTER this verification round closed. Empty means the gate
	// passed: the verifier's own record was closed, so its new findings (if any)
	// are included. The driver loops another fix when it is non-empty.
	verifyOpenMusts []string
	// verifyOpenDetails is the same list with each finding's summary appended,
	// for the park's question: "carrying the records" means the operator reads
	// WHAT is open, not only its id.
	verifyOpenDetails []string
	// verifyCounted is the plane's count of NON-VOID rounds in the newest
	// record's cycle, read after the close. The round cap reads it; a void round
	// does not count (B4/B8).
	verifyCounted int
	// verifyResolvedIDs are the MUST ids on released records a verifier has
	// already marked resolved (fixed/filed/refuted). The driver remembers them
	// across the round loop: an id that is open AGAIN after being resolved is
	// non-convergence, and parks the task (A6).
	verifyResolvedIDs []string
	// verifyStatus is the task's plane status after the round closed: a clean
	// round must have handed the task off (in_review/done/...); one that left it
	// in_progress recorded a pass but did not carry the terminal step, which the
	// driver says loudly rather than mistaking for a hand-over.
	verifyStatus string

	// reportProblem is why a runner-claimed session's done report became no
	// records: missing, unreadable, invalid, or a write the plane refused. The
	// seam gates append it to their own "no record matched" reason, because a
	// phase that wrote nothing looks identical either way.
	reportProblem string
	// doneReport is the verification round's report, kept so the hand-over can
	// carry its risk declaration after the round is closed.
	doneReport *config.DoneReport

	// sequenceEnd, when non-nil, reports that this RESUMED phase ended the
	// sequence without completing it, and the RUN must not end with it. Two
	// arms produce it: a ROLLING usage limit (CLA-631) and a TRANSIENT failure
	// (CLA-651). In both cases the sequence ends exactly as the old stop did —
	// the claim is not handed back to the queue, so the lease lapses into a
	// takeover with its branch recorded — but drainPhases returns to Run, which
	// carries on polling. Both arms owe a wait, taken after the sequence's
	// deferred handback with no lease held: a set limitWait is the reset the
	// run waits out (CLA-631); a nil one is a transient blip, and the run waits
	// the target's growing backoff rung instead (CLA-662) — because with the
	// resumed phase on a DIFFERENT harness from the one that claims the queue
	// (implement opencode, review claude), there is no first-phase failure for
	// the in-drain ladder to see. Nil on every other path, including a hard
	// stop (lim.Stop), which still stops the run in drainPhase.
	sequenceEnd *phaseSequenceEnd
}

// phaseSequenceEnd is the signal the sequenceEnd field carries: a resumed
// phase could not continue its sequence, but the RUN must (CLA-631, CLA-651).
// It is deliberately not a stop — the sequence ends, the daemon does not.
type phaseSequenceEnd struct {
	// limitWait, when non-nil, is the rolling usage limit whose reset the run
	// owes a wait for after the sequence's handback, on the harness that hit
	// it (CLA-631). Nil means the TRANSIENT arm (CLA-651): there is no reset
	// to wait out, so drainPhases instead extends the target's consecutive-
	// transient streak and waits backoff(n) before polling again (CLA-662).
	limitWait *phaseLimitWait
}

// phaseLimitWait is the wait a rolling usage limit owes a run whose RESUMED
// phase was turned away: a request for drainPhases to wait the limit out after
// the sequence's handback, with no lease held — the shape the resumed phase
// itself must never take, because the wait would outlive its 30-minute lease
// (CLA-631).
type phaseLimitWait struct {
	lim harness.Limit   // the rolling limit, for the reset and its reason
	ph  config.Phase    // the phase whose harness was turned away
	a   harness.Adapter // that harness, so the probes run on the account that is limited
}

// deadPhase reports the CLA-386 dead-phase signature: the session's final step
// finished with reason "unknown" — opencode's marker for a session that died
// without producing a final answer — AND no branch is recorded on the task, so
// nothing durable came out of it. The two conjuncts together are what "produced
// nothing" means: the reason names the death, the missing branch rules out the
// one thing that would have made the phase survivable anyway. A session that
// pushed work and THEN died on an unknown reason has still produced something,
// and its phase keeps its checkpoint.
//
// It reads the reason off the adapter's Raw map rather than asking the adapter:
// the key is stable across the harnesses that observe it, and no Adapter method
// needs to grow for a signal only opencode emits today.
func deadPhase(res harness.Result) bool {
	r, _ := res.Raw[harness.FinishReasonKey].(string)
	return r == harness.FinishReasonUnknown && !res.Claim.HasWIP
}

// isReviewPhase reports whether a phase is a review-phase (the gate where
// escalation applies). Named phases whose name is "review" are review phases;
// custom prompts with no name are never treated as review for escalation.
func isReviewPhase(ph config.Phase) bool {
	return ph.Name == config.ReviewPhaseName
}

// isVerifyPhase reports whether a RESOLVED phase is the fix-verification round
// (CLA-638). It keys on the resolved prompt, like every other review-shaped
// predicate: a handoff respawn replaces the prompt, and an operator's custom
// prompt named "verify" owns its own contract and is not one.
func isVerifyPhase(ph config.Phase) bool {
	return config.VerifyShaped(ph.Prompt)
}
