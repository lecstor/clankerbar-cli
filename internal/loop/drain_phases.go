package loop

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// drainPhases runs ONE drain as its configured sequence of phase sessions —
// each a separate harness process, so the context resets between them. With no
// phases configured that sequence is one session and this is exactly the old
// behaviour; see config.Phase for why the split exists and config.EffectivePhases
// for why "unphased" is expressed as a single phase rather than a second path.
//
// Two things have to be true across a seam, and they are the whole design:
//
//  1. The claim is NOT handed back. The next phase resumes the same run with
//     heartbeat(runId) rather than claiming, so the lease has to survive the gap.
//     A handback here would put the task in the queue while we are still on it.
//  2. If the next phase will never run, the handback skipped at the seam happens
//     here instead — otherwise a budget stop leaves the task leased to nobody.
func (d *Driver) drainPhases(ctx context.Context, drainNum, ti int, t Target, prior spend) (tokens int, cost float64, handoffs int, stop bool, err error) {
	// The target's own effective config (CLA-410): a stored run-config overlay
	// changes harness/model/budget here, not on the shared base.
	tc := d.targetCfg(t)
	phases := tc.EffectivePhases()
	// rawPhases is the same sequence with every EXPANSION collapsed: index-
	// aligned 1:1 with phases, but each review-panel member maps back to the one
	// `review` phase the operator wrote (CLA-639), so a member is recognised as
	// running the resolved built-in brief its configured phase asked for rather
	// than a prompt of its own. The handoff branch below reads it to tell a
	// built-in-brief phase from an operator's custom prompt, and it MUST be this
	// captured copy (CLA-664): a live read would let a RELOAD applied mid-drain
	// reshape the continuation of the sequence in flight — a 3 -> 2 reload drops
	// the in-flight fix successor's terminal step, and a custom-prompt phase
	// later covered by a built-in gains one.
	rawPhases := make([]config.Phase, len(phases))
	for i := range phases {
		rawPhases[i] = phases[i]
		if src := phases[i].ConfiguredIndex(); src < len(tc.Phases) {
			rawPhases[i] = tc.Phases[src]
		}
	}
	// CLA-638: where the fix→verify pair lives, when the sequence has one. The
	// verification round is last by validation, and the driver repeats the pair
	// by rewinding to fixIdx after a round with open MUSTs. Zero when the
	// sequence has no verification round (the legacy fix-is-terminal shape).
	fixIdx := -1
	for i := range phases {
		if i+1 < len(phases) && phases[i].Name == config.FixPhaseName && isVerifyPhase(phases[i+1]) {
			fixIdx = i
		}
	}
	s := &drainSeq{
		d: d, t: t, tc: tc, ti: ti, drainNum: drainNum, prior: prior, phases: phases, rawPhases: rawPhases, fixIdx: fixIdx,
		verifyResolved: map[string]bool{}, panelFailed: map[string]panelMemberFailure{}, drainStart: time.Now(),
	}
	// CLA-466: one iteration record posts at this drain's boundary, whatever way
	// it ends — hence the defer, and hence state the deferred record reads.
	defer s.recordIteration()
	for s.i = 0; s.i < len(phases); {
		st := &seqStep{}
		st.ph = phases[s.i]
		// CLA-639: whether this position is one reviewer of a review panel. The
		// members are consecutive review-shaped phases, so "before the next
		// configured phase" is the same test the shipped sequence already uses.
		// Captured BEFORE the handoff branch replaces ph.Prompt with a
		// continuation: a panel member never honours a handoff, but the
		// continuation's own review position must still be read from the phase
		// identity, not from a prompt that is no longer a brief.
		st.panelMember = st.ph.IsPanelMember()
		st.last = s.i == len(phases)-1
		// CLA-639 M5: a BRANCHLESS task cannot run a panel AT ALL, so its panel
		// is one reviewer. This shape's review record is the legacy `## Review`
		// block written to the task's outcome — the plane accepts a durable
		// record only from the task's live holder, and a settled task has none
		// (question 03a12578, option 1) — so a second member could record nothing
		// a durable round could hold, and the block it would append is the FIRST
		// member's own record.
		//
		// CLA-784: that ONE reviewer is the first REQUIRED member, never blindly
		// the first configured one. An optional (trial) seat is skipped here: its
		// failure is that seat's own result, and if it consumed the single review
		// slot a branchless task would be left with no required review at all
		// (review 1.review-2.1 / 1.review-4.1). Validate refuses an all-optional
		// panel, so a required seat always exists; the members after the chosen
		// one are skipped, with one log line.
		//
		// The test is the claim CARRIED INTO this phase (`carried`), not
		// `branchlessCheckpoint`: that flag is set at the seam from the claim the
		// PREVIOUS phase was entered with, so it is still false at the FIRST
		// panel member — exactly the position where the trial seat sits in the
		// shape this finding names. `carried` is what `drainPhase` itself reads
		// for `branchlessPanel` (`prev.Claim.Branch == ""`), so the two agree.
		if st.panelMember && s.carried != nil && s.carried.Claim.Branch == "" {
			if st.ph.IsOptionalPanelMember() {
				log.Printf("%siteration %d: this task's checkpoint is BRANCHLESS (its review record is the legacy %q block, not a durable one), so its review panel runs ONE reviewer — skipping the OPTIONAL (trial) seat %s: the required seat is the one that must record",
					labelOf(t), drainNum, config.ReviewRecordHeading, st.ph.Label(s.i))
				s.i++
				continue
			}
			if s.branchlessPanelRan {
				if !s.branchlessPanelNoted {
					log.Printf("%siteration %d: this task's checkpoint is BRANCHLESS (its review record is the legacy %q block, not a durable one), so the review panel runs ONE reviewer: %s",
						labelOf(t), drainNum, config.ReviewRecordHeading, s.branchlessPanelChosen)
					s.branchlessPanelNoted = true
				}
				s.i++
				continue
			}
			s.branchlessPanelRan = true
			s.branchlessPanelChosen = st.ph.Label(s.i)
		}
		// CLA-639: the panel's last member is the last REVIEW of the sequence even
		// when a fix follows it, and the driver's plain-review seam must run for it
		// (that is the seam that carries the record gate). Only a PANEL member gets
		// this treatment: a shipped `review` before a fix is not "terminal", and
		// passing that word over would tell it there is nothing to checkpoint for.
		st.terminal = st.last
		if st.panelMember && !st.last && !config.ReviewShaped(phases[s.i+1].Prompt) {
			st.terminal = true
		}
		st.isHandoff = s.nextPrompt != ""
		// CLA-697: fan the panel's contiguous members out concurrently, each in
		// its own detached worktree. A handoff respawn continues ONE phase (never
		// a panel run), and a branchless checkpoint runs one reviewer by design —
		// both take the shipped sequential path below.
		//
		// The join owns everything the sequential member handler would have done
		// one member at a time: the incomplete list, the round close, the run-level
		// wait a limited member owes, and the decision not to spawn a fix when
		// nobody recorded. With cap = 1 (or a single member) this block does
		// nothing at all and the old path runs byte for byte.
		if !st.panelMember {
			s.panelFanOutOff = false
		}
		if st.panelMember && !st.isHandoff && !s.branchlessCheckpoint && !s.panelFanOutOff {
			runEnd := s.i + 1
			for runEnd < len(phases) && phases[runEnd].IsPanelMember() {
				runEnd++
			}
			if n, conc := runEnd-s.i, tc.EffectivePanelConcurrency(runEnd-s.i); n > 1 && conc > 1 {
				// The breaker at the boundary BEFORE the fan-out, exactly as the
				// sequential path's per-spawn check would have stopped before the
				// first member it could not afford.
				if s.spawned > 0 {
					if dim := d.budgetTrip(prior.tokens+s.tokens, prior.cost+s.cost, time.Since(prior.start)); dim != "" {
						log.Printf("iteration %d: budget reached at the %s phase boundary: %s — stopping (tokens=%d cost=$%.2f elapsed=%s)",
							drainNum, st.ph.Label(s.i), dim, prior.tokens+s.tokens, prior.cost+s.cost,
							time.Since(prior.start).Round(time.Second))
						s.stop = true
						break
					}
				}
				if out, ok := d.runPanelInParallel(ctx, drainNum, ti, t, phases, s.i, runEnd, conc, s.carried, spend{
					start:  prior.start,
					tokens: prior.tokens + s.tokens,
					cost:   prior.cost + s.cost,
				}); ok {
					s.tokens += out.tokens
					s.cost += out.cost
					s.spawned += n
					for k, m := range out.members {
						if m.end.session != nil {
							s.sessionsRun = append(s.sessionsRun, *m.end.session)
						}
						label := phases[s.i+k].Label(s.i + k)
						if len(s.phasesRun) == 0 || s.phasesRun[len(s.phasesRun)-1] != label {
							s.phasesRun = append(s.phasesRun, label)
						}
					}
					// The member claims are the same run the panel resumed; keep
					// the CARRIED claim (with its verified branch and tip) rather
					// than adopting one member's copy. A member that recorded
					// nothing can end holding a claim that never had the tip
					// folded onto it, and adopting that one would hand the fix
					// phase an unverified-looking checkpoint.
					if out.claim != nil && (s.carried == nil || out.claim.Claim.TaskID == s.carried.Claim.TaskID) {
						s.seen = out.claim.Claim
						s.finalOutcome = out.disposition
					}
					s.finalDead = out.finalDead
					s.lastProducedNothing = out.lastProducedNothing
					s.panelRecorded = s.panelRecorded || out.recorded
					s.panelRound = max(s.panelRound, out.round)
					s.panelIncomplete = append(s.panelIncomplete, out.incomplete...)
					for role, why := range out.failed {
						s.panelFailed[role] = why
					}
					// The run-level endings, in the order the sequential handler
					// checks them: a close that failed is fail-closed and stops the
					// drain; a member's budget stop ends the run; a limit or
					// transient end hands the drain its wait (the claim is left to
					// lapse into a takeover, never released); no record at all means
					// no fix.
					switch {
					case out.closeFailed:
						if s.limitWait == nil && out.sequenceEnded && out.limitWait == nil {
							s.transientKey = out.transientKey
							s.transientRung = d.noteResumedTransient(s.transientKey)
						}
					case out.stop:
						s.stop = true
					case out.err != nil:
						s.err = out.err
					case out.sequenceEnded:
						s.limitWait = out.limitWait
						if s.limitWait == nil {
							s.transientKey = out.transientKey
							s.transientRung = d.noteResumedTransient(s.transientKey)
						}
					}
					if s.stop || s.err != nil || out.closeFailed || out.sequenceEnded || !out.recorded || runEnd >= len(phases) {
						break
					}
					s.i = runEnd
					continue
				}
			}
		}
		st.promptBytes = 0
		if st.isHandoff {
			// The respawn continues this phase's job under its knobs — turn cap
			// and tier still apply. The session's own prompt is framed by
			// config.HandoffPreamble, not handed over bare: the successor needs
			// the same "resume, don't claim" contract a phase resume gets from
			// its brief, and a session-authored prompt cannot be trusted to
			// carry that forward on its own. promptBytes is the session's own
			// prompt alone — what the size cap was measured against — not the
			// preamble-inflated total, so the log line stays comparable to it.
			//
			// config.HandoffContinuation (CLA-353) is appended after it for the
			// same reason: the phase's OWN brief — not only the preamble's
			// contract — is otherwise dropped on a handoff, since ph.Prompt is
			// replaced wholesale rather than framing the built-in text. For the
			// review phase that would silently drop the PR-then-hand-off
			// terminal step the moment a session hands off mid-review.
			//
			// Gated on the phase actually RUNNING the built-in brief (review
			// finding): HandoffContinuation matches on name alone, so an
			// operator who reuses the name "review" for a phase carrying their
			// own custom Prompt would otherwise have the shipped terminal step
			// force-injected into a handoff respawn of a brief that never
			// established that contract in the first place — clashing with
			// whatever terminal step their own prompt names, if any.
			// rawPhases (the configured sequence captured at drain start, not
			// EffectivePhases' output) is index-aligned with phases: a panel
			// member maps back to the `review` phase it expanded from, and every
			// other entry is its own configured phase. Never re-read live: a
			// RELOAD applied mid-drain must not reshape the in-flight sequence's
			// continuation (CLA-664) — the same rule the mode and the count follow.
			st.promptBytes = len(s.nextPrompt)
			continuation := ""
			if s.i < len(rawPhases) && rawPhases[s.i].Prompt == "" {
				// CLA-497: a handoff during a NO-CODE review must not append the
				// branch-shaped reviewTerminalStep, which tells the successor to
				// open a PR for a task that has no branch and no code.
				// branchlessCheckpoint records the checkpoint that opened this
				// phase (captured at the seam from the carried claim), so the
				// continuation matches the brief this phase's first session ran.
				//
				// The split adds the POSITION-dependent review mode: a handoff
				// during a review-only phase must carry the record-then-hold
				// ending, never the commit/push/PR/in_review terminal step. A
				// fix phase carries that terminal step by name.
				reviewOnly := st.ph.Name == config.ReviewPhaseName && !st.terminal
				// CLA-637: the checkpoint that OPENED this phase decides which
				// review-only variant the successor continues. A branchless
				// checkpoint's successor gets the no-tip shape; an UNVERIFIED
				// one has no driver-known tip to hand over either, so its
				// successor gets the variant that tells the reviewer to
				// establish the tip itself — the verified shape's {{reviewedTip}}
				// could not be filled and would leak as a literal.
				unverifiedCheckpoint := s.carried != nil && s.carried.CheckpointUnverified
				switch {
				case st.ph.Name == config.ReviewPhaseName && s.branchlessCheckpoint && reviewOnly:
					continuation = config.NoCodeReviewOnlyHandoffContinuation()
				case st.ph.Name == config.ReviewPhaseName && s.branchlessCheckpoint:
					continuation = config.NoCodeHandoffContinuation()
				case st.ph.Name == config.ReviewPhaseName && reviewOnly && unverifiedCheckpoint:
					continuation = config.UnverifiedReviewOnlyHandoffContinuation()
				case st.ph.Name == config.FixPhaseName && s.branchlessCheckpoint && s.i+1 < len(phases) && isVerifyPhase(phases[s.i+1]):
					// CLA-638: a BRANCHLESS fix whose verify round follows must
					// still stop before the hand-over — the legacy no-code fix
					// continuation would tell its successor to move the task to
					// in_review (noCodeReviewTerminalStep), which is the
					// verifier's move.
					continuation = config.NoCodeFixVerifyHandoffContinuation()
				case st.ph.Name == config.FixPhaseName && s.branchlessCheckpoint:
					continuation = config.NoCodeFixHandoffContinuation()
				case st.ph.Name == config.FixPhaseName && s.i+1 < len(phases) && isVerifyPhase(phases[s.i+1]):
					// CLA-638: the successor of a mid-fix handoff must still stop
					// before the PR; the verification round owns the hand-over.
					continuation = config.FixVerifyHandoffContinuation()
				default:
					continuation = config.HandoffContinuationFor(st.ph.Name, reviewOnly)
				}
			}
			st.ph.Prompt = config.HandoffPreamble + s.nextPrompt + continuation
			s.nextPrompt = ""
		}

		if s.spawned > 0 {
			// The breaker at a phase boundary — the third place a session ends and
			// the loop decides whether to spawn another, alongside between-drains
			// and between-attempts, and the finest granularity the ceiling has had.
			// It is not decoration: one measured task spent 92% of a whole run's
			// 75M ceiling inside a single session, which a between-drains check
			// cannot see coming and cannot interrupt. A handoff respawn gets the
			// same check, BEFORE the respawn: a session cannot spend its way past
			// the ceiling by handing off to itself.
			if dim := d.budgetTrip(prior.tokens+s.tokens, prior.cost+s.cost, time.Since(prior.start)); dim != "" {
				boundary := "the " + st.ph.Label(s.i) + " phase boundary"
				if st.isHandoff {
					boundary = "the handoff respawn"
				}
				log.Printf("iteration %d: budget reached at %s: %s — stopping (tokens=%d cost=$%.2f elapsed=%s)",
					drainNum, boundary, dim, prior.tokens+s.tokens, prior.cost+s.cost,
					time.Since(prior.start).Round(time.Second))
				s.stop = true
				break
			}
		}

		// Unphased runs keep their log names byte-identical: no phase tag. A
		// handoff respawn is tagged too, so the state dir shows the chain the way
		// the daemon log does.
		st.tag = ""
		if len(phases) > 1 {
			st.tag = "-p" + st.ph.Label(s.i)
		}
		if st.isHandoff {
			s.handoffs++
			s.chain++
			st.tag += fmt.Sprintf("-h%d", s.chain)
			log.Printf("%siteration %d: handoff respawn %d — spawning a fresh session on the predecessor's own %d-byte prompt, framed by the driver's resume preamble (counts as an iteration)",
				labelOf(t), drainNum, s.chain, st.promptBytes)
		}
		s.spawned++

		// CLA-466: announce the phase change NOW rather than at the next poll, so
		// the console sees which phase is running without waiting out an idle
		// interval. taskRef is empty until a claim has been observed (the first
		// phase claims inside drainPhase); from then on it rides every beacon.
		// CLA-510: a first-phase claim no longer waits for the next phase
		// boundary — the renewer's claim reflector (startLeaseRenewal) revises
		// the in-flight ref and beacons the moment the session's claim lands.
		ref := ""
		if s.carried != nil {
			ref = claimLabel(s.carried.Claim)
		}
		if len(s.phasesRun) == 0 || s.phasesRun[len(s.phasesRun)-1] != st.ph.Label(s.i) {
			s.phasesRun = append(s.phasesRun, st.ph.Label(s.i))
		}
		d.beginIteration(ti, t, drainNum, ref, st.ph.Label(s.i))

		// CLA-662: whether this session RESUMED a predecessor's claim — the
		// exact condition drainPhase's transient arm keys on (prev != nil), so
		// the streak follows the ends that arm produces. Captured BEFORE the
		// call: the seam below may replace `carried` with this phase's own
		// claim, which says nothing about how the phase was entered.
		st.phaseResumed = s.carried != nil
		st.ptokens, st.pcost, st.pstop, st.end, st.perr = d.drainPhase(ctx, drainNum, ti, s.i, st.tag, st.ph, st.terminal, s.carried, t, spend{
			start:  prior.start,
			tokens: prior.tokens + s.tokens,
			cost:   prior.cost + s.cost,
		}, phases, panelState{round: s.panelRound, failed: s.panelFailed, recorded: s.panelRecorded}, phaseRunOpts{})
		s.tokens += st.ptokens
		s.cost += st.pcost
		// CLA-630: one entry per session this pass spawned, in spawn order. nil
		// means the pass never launched a session (a harness or spawn-dir
		// refusal above), which is nothing to record.
		if st.end.session != nil {
			s.sessionsRun = append(s.sessionsRun, *st.end.session)
		}
		s.finalDead = st.end.dead
		s.lastProducedNothing = st.end.producedNothing
		// CLA-466: the record's task identity and claim disposition follow EVERY
		// claim the sequence observed — including one a dead phase already handed
		// back itself (end.released), which the switch below deliberately drops
		// from `carried`. The off-brief exception matches the switch's own first
		// arm: a claim on a DIFFERENT task does not re-attribute the iteration.
		if st.end.claim != nil && !(s.carried != nil && s.carried.Claim.TaskID != st.end.claim.Claim.TaskID) {
			s.seen = st.end.claim.Claim
			s.finalOutcome = claimDisposition(st.end.claim)
		}
		// What, if anything, is still owed a handback. Three cases, and getting any
		// of them wrong is a task either stranded on a dead lease or posted back to
		// the queue over work already in review:
		//
		//   - the phase handed the claim back itself → nothing is owed;
		//   - the phase observed claim state → that is now the authoritative view,
		//     including Settled and HasWIP, which releaseHeldClaim reads;
		//   - the phase observed nothing (it never launched) → KEEP the predecessor's,
		//     or a phase 2 that failed to start would silently drop the handback for
		//     the task phase 1 is still holding.
		//
		// A phase that went off-brief and claimed a DIFFERENT task is its own case.
		// noteClaimed replaces the observed claim wholesale, so the seam releases
		// that task and reports `released` — which would drop the predecessor's
		// still-live claim and hand back a task the sequence was never meant to
		// touch. Keep what we had: the task phase 1 is holding is the one owed a
		// handback, whatever its successor went and did.
		st.seamClaim = s.carried
		switch {
		case s.carried != nil && st.end.claim != nil && st.end.claim.Claim.TaskID != s.carried.Claim.TaskID:
			log.Printf("iteration %d: the %s phase claimed %s, which is NOT the task it was resuming (%s) — it was told not to claim at all; keeping the original for the handback",
				drainNum, st.ph.Label(s.i), st.end.claim.Claim.TaskID, s.carried.Claim.TaskID)
		case st.end.released:
			s.carried = nil
		case st.end.claim != nil:
			s.carried = st.end.claim
		}
		// CLA-497: whether the checkpoint that opened THIS phase was evidenced
		// by the plane's record (no verified branch). Read from the PRE-switch
		// claim - the one the seam advanced with: after the switch, carried is
		// this phase's OWN claim, whose branch says nothing about the checkpoint
		// that opened the phase, and the handoff continuation must match the
		// brief this phase's FIRST session ran, not what its last one recorded.
		s.branchlessCheckpoint = st.seamClaim != nil && st.seamClaim.Claim.Branch == ""

		// CLA-639 M4: a PANEL MEMBER that produced no record leaves the run
		// resumable — the next member, or the fix phase, picks up the SAME run
		// from the SAME verified checkpoint. Its own result is what carries the
		// claim forward (the switch above), and a session that was turned away or
		// died before reading the task reports no branch: without this, the next
		// member's brief would be handed an empty branch and read as the
		// BRANCHLESS variant, whose record is the legacy block the panel cannot
		// use. So the checkpoint's resumption facts ride forward from the claim
		// that opened this member, onto a claim that is still live and short of
		// them. Only a member: every other phase's claim is its own.
		if st.panelMember && !st.end.reviewRecorded && st.seamClaim != nil && s.carried != nil && s.carried.Claim.Held() {
			if s.carried.Claim.Branch == "" {
				s.carried.Claim.Branch = st.seamClaim.Claim.Branch
			}
			if s.carried.CheckpointTip == "" {
				s.carried.CheckpointTip = st.seamClaim.CheckpointTip
			}
			s.carried.CheckpointUnverified = st.seamClaim.CheckpointUnverified
		}

		// A run-level stop wins over a dead phase. drainPhase returns stop=true
		// for a budget trip and credit exhaustion (lim.Stop) — and on those
		// paths end.dead can be set too (a credit-starved session is exactly the
		// shape that dies on reason "unknown" with no branch). Retrying a dead
		// phase against a stopped run burns another session into the same limit
		// and, on the second, parks a task for what was actually a run-wide
		// outage instead of stopping the daemon. A run stop is final; the dead
		// branch only decides retry vs park when the run is continuing. (A
		// resumed phase ending on a live lease is NOT one of those paths — see
		// the sequence-end break below, deliberately before the dead arm,
		// because that end leaves the run continuing.)
		if st.pstop {
			s.stop = true
			break
		}

		// CLA-631/CLA-651/CLA-662: a RESUMED phase ended the sequence without
		// completing it — a rolling usage limit (CLA-631) or a transient failure
		// (CLA-651) — and the RUN must not end with it. The sequence stops
		// exactly like the pstop above — the claim is not handed back to the
		// queue, so the lease lapses into a takeover with its branch recorded —
		// but the run carries on: the wait the signal owes is taken below,
		// after the sequence's deferred handback, holding no lease. Broken here
		// rather than flowing into the checks below because those read a session
		// that merely got turned away: the dead/fleet counters would tick on a
		// blip's exit shape (or a limit's "unknown" finish reason), and none of
		// that machinery applies to a sequence end the run is ordered to survive.
		//
		// The transient arm ALSO extends its target's streak here (CLA-662):
		// nothing else bounds a provider that keeps blipping the resumed phase
		// while the implementing harness stays healthy — the drain is over, so
		// drainPhase's retry ladder was never entered, and the re-drain would
		// otherwise land the very next instant. A rolling limit extends no
		// streak: its wait is a reset, not a rung, and the next drain is
		// scheduled after it anyway.
		// CLA-639: a PANEL member's sequence-end is judged by the panel handler
		// below first — it records the member as failed, closes the round that the
		// earlier members wrote into, and only then lets the run-level wait take
		// over. Every other resumed phase keeps the shipped break here.
		if st.end.sequenceEnd != nil && !st.end.panelMember {
			s.limitWait = st.end.sequenceEnd.limitWait
			if s.limitWait == nil {
				s.transientKey = resumedTransientKey{target: ti, harness: tc.HarnessFor(st.ph)}
				s.transientRung = d.noteResumedTransient(s.transientKey)
			}
			break
		}

		// CLA-662: a resumed phase that ended any OTHER way than the transient
		// sequence-end above — completed, settled, died — breaks its target's
		// streak: the next blip on that target+harness is not consecutive with
		// the last, and starts the ladder over. The other sequence-end arm, a
		// rolling limit, also breaks out above and so neither extends nor
		// clears: the reset the run waits out is its own bound. Reset on the
		// seam BETWEEN the arms, so "consecutive" means exactly what it says:
		// the arm above extends, this one clears.
		if st.phaseResumed {
			d.clearResumedTransient(resumedTransientKey{target: ti, harness: tc.HarnessFor(st.ph)})
		}

		// CLA-639: the panel's members are skippable where a lone review phase is
		// not. A member that recorded nothing — dead, capped before its record
		// write, empty under the evidence gate, or turned away by a non-retryable
		// error — is RECORDED AS FAILED and the panel moves on, instead of ending
		// the drain with three reviewers still unrun. That is the panel's version
		// of the doneWhen's "recorded or been recorded as failed/capped".
		//
		// Deliberately BEFORE the dead/fleet arms: those exist for a phase whose
		// death leaves the task going nowhere, but a panel member's death is one
		// reviewer lost, not the task lost. Counting it toward the fleet pause
		// would let a flaky reviewer bench the whole target.
		//
		// The round is NOT closed while another member follows: closing it
		// would release the earlier members' findings to that member, which is
		// exactly the panel independence the seal exists to protect. It is closed
		// when the panel is over, whichever way the last member ends.
		if st.end.panelMember {
			if round := st.end.reviewRound; round > 0 {
				s.panelRound = round
			}
			// The member's reviewer ROLE is positional (CLA-639): the durable
			// record and the seam gate are keyed on it, and the operator's `name`
			// is only the log label. The incomplete list and the fix brief must
			// therefore speak roles, or a named member would be reported missing
			// while its role-keyed record is sitting right there.
			role := reviewerRoleFor(phases, s.i)
			member := st.ph.Label(s.i)
			// CLA-639 M4: whether the NEXT member, or the fix phase after the
			// panel, could still resume this run. The member's own claim is the
			// live one when it observed any; a member that observed NOTHING (a
			// launch failure) leaves the predecessor's carried claim in place, and
			// that is still the run the panel is working. A claim the session
			// settled itself, or one already handed back, is not live: nothing may
			// be resumed from it, and only the live holder may close a round.
			live := s.carried != nil && (st.end.claim == nil || st.end.claim.Claim.Held())
			// A member ends the PANEL's stage when it recorded (its record passed
			// the seam gate) or when no next member can run — the last member, a
			// claim the failed member took with it, or a harness sequence-end.
			// Only a failed member with a live claim and a member after it is
			// skipped past.
			//
			// A member whose own round close failed DID record: the seam matched
			// a record and the close is a later, separate step. Counting that as
			// "recorded nothing" would name a reviewer INCOMPLETE and charge its
			// failure streak for the plane's failure to release the round.
			recorded := st.end.reviewRecorded || st.end.reviewCloseFailed
			seqEnd := st.end.sequenceEnd
			// A member that writes its durable record and only THEN ends the
			// sequence (a usage limit or a transient failure) returns before the
			// seam evaluates the record gate. Ask the plane the same question the
			// seam asks, so the member is counted as recorded instead of unnamed
			// INCOMPLETE and charged a failure for writing exactly what it owed
			// (CLA-639 fix pass 4). Only the seam's own matcher inputs qualify: a
			// verified tip (match it exactly) or the unverified mark (demand a
			// tip). A carried claim with neither — a branch with no tip and no
			// unverified mark, the shape the checkpoint-fact copy cannot fill
			// when the claim is not held — would make the matcher accept any
			// record by run and role, looser than the seam ever is, so it is left
			// to the failure arm (CLA-639 fix pass 5).
			if !recorded && seqEnd != nil && s.carried != nil && (s.carried.CheckpointTip != "" || s.carried.CheckpointUnverified) {
				if rec, matched := d.panelMemberRecordMatch(ctx, t, *s.carried, role); matched {
					recorded = true
					if rec.Round > 0 {
						s.panelRound = rec.Round
					}
				}
			}
			// endSequenceEnd is the shared tail of the panel's run-level endings:
			// a usage limit or transient failure owes the run its wait after the
			// sequence's handback, exactly as the single-review seam does.
			endSequenceEnd := func() {
				if seqEnd == nil {
					return
				}
				// CLA-784: an OPTIONAL seat's end is that seat's own result. It
				// may not end the sequence with a run-level wait; the panel
				// carries on, and the fix spawns from the required members'
				// records exactly as for any incomplete panel.
				if st.end.panelOptional {
					return
				}
				s.limitWait = seqEnd.limitWait
				if s.limitWait == nil {
					s.transientKey = resumedTransientKey{target: ti, harness: tc.HarnessFor(st.ph)}
					s.transientRung = d.noteResumedTransient(s.transientKey)
				}
			}
			if recorded {
				// The member recorded, so its failure streak is over (CLA-639
				// M4): the next failure, if any, is not consecutive with the last.
				d.panelMemberRecorded(member)
				// CLA-784: only a REQUIRED member's record is a review. An
				// optional (trial) seat's record is that seat's result — it counts
				// toward its own assessment, but a round where only optional
				// seats recorded is no review at all (see the panel-last check
				// below).
				if !st.end.panelOptional {
					s.panelRecorded = true
				}
			}
			// A round that could not be closed is the single-review seam's
			// fail-closed shape, for the panel: the member recorded, but the fix
			// must not spawn against findings the plane still holds sealed. End
			// the drain here (the deferred handback releases the claim exactly as
			// the single-review close-failure path does) and say why.
			if st.end.reviewCloseFailed {
				log.Printf("%siteration %d: the %s panel member recorded, but the panel's round %d could not be closed (%s) — ending the drain without spawning the fix: a fix spawned now would run against findings the plane still has sealed",
					labelOf(t), drainNum, member, s.panelRound, st.end.emptyWhy)
				endSequenceEnd()
				break
			}
			if !recorded {
				reason := st.end.emptyWhy
				switch {
				case st.end.noMCP:
					// CLA-703: an infrastructure miss, not a reviewer's verdict.
					// The `no_mcp` token is stable so the fixer's INCOMPLETE list
					// and the analytics can exclude it without parsing prose.
					reason = "no_mcp: no successful clankerbar MCP call and no review record"
				case st.end.toolLoop != "":
					reason = st.end.toolLoop
				case st.end.dead:
					reason = "died producing nothing"
				case seqEnd != nil && seqEnd.limitWait != nil:
					reason = "hit a usage limit before recording"
				case seqEnd != nil:
					reason = "ended on a transient failure before recording"
				case st.perr != nil:
					reason = failureDetail(st.perr.Error())
				case st.end.empty:
					reason = st.end.emptyWhy
				case reason == "":
					reason = "recorded no durable review record"
				}
				s.panelIncomplete = append(s.panelIncomplete, panelIncompleteRole(role, st.end.panelOptional)+": "+reason)
				s.panelFailed[role] = panelMemberFailure{reason: reason, optional: st.end.panelOptional}
				log.Printf("%siteration %d: the %s panel member recorded nothing (%s) — recording it as failed", labelOf(t), drainNum, member, reason)
				// CLA-639 M4: the member is a failure the operator has to be able
				// to see, so it is counted in the run's per-harness dead-phase
				// tally (CLA-402) — the rate that exists for exactly this shape —
				// unless the seam's own classification already booked this exit
				// (a dead session, or a held-but-empty one). And a member that
				// keeps failing task after task is a fault in that member rather
				// than bad luck: the streak is counted across tasks and said
				// loudly once it reaches panelMemberFailBound. Deliberately NOT a
				// park: one reviewer of several failing is not a reason to strand
				// the panel's real findings.
				//
				// CLA-703: a no_mcp miss belongs to NEITHER counter. It is not a
				// dead phase (no work was attempted), and it is not the member's
				// fault — charging its streak would make an MCP outage look like a
				// broken reviewer. It gets its own tally cell and its own log line.
				if st.end.noMCP {
					d.tallyNoMCP(member, tc.HarnessFor(st.ph))
					log.Printf("%siteration %d: the %s panel member's failure is no_mcp (infrastructure), not a review failure — not counted against the member's streak or the dead rate",
						labelOf(t), drainNum, member)
				} else {
					if !st.end.deadBooked {
						d.tallyEmpty(member, tc.HarnessFor(st.ph))
					}
					if n := d.panelMemberFailed(member); n >= panelMemberFailBound {
						model, _ := tc.ModelForPhase(st.ph)
						log.Printf("%siteration %d: WARNING — the %s review-panel member has recorded nothing on %d consecutive tasks (harness %s, model %s); the panel runs on and the fix applies what the other members found, but this member's harness, model or brief is not producing reviews — check its iteration logs",
							labelOf(t), drainNum, member, n, tc.HarnessFor(st.ph), attributionOr(model, "harness default"))
					}
				}
				if live && !st.end.panelLast {
					// The next member resumes this claim: keep the panel going, and
					// keep the round sealed — a close here would release the earlier
					// members' findings to the next reviewer. ADVANCE, never re-run
					// this member: the loop has no post statement, so a `continue`
					// without the increment would re-spawn index i forever.
					s.i++
					continue
				}
				// The panel is over: nothing follows this member, or the claim it
				// would have resumed is gone, or the harness ended the sequence.
				// Release what the earlier members wrote rather than leave it
				// sealed behind a lease that will lapse — BEFORE any handback,
				// because the close is legal only while we are still the holder.
				if s.panelRecorded && s.panelRound > 0 && live {
					if cerr := d.closeReviewRound(ctx, t, *s.carried, s.panelRound); cerr != nil {
						log.Printf("%siteration %d: the panel's round %d could not be closed after %s failed (%v) — ending the drain without spawning the fix: a fix spawned now would run against findings the plane still has sealed",
							labelOf(t), drainNum, s.panelRound, member, cerr)
						endSequenceEnd()
						break
					}
				}
				switch {
				case live && s.panelRecorded && s.i+1 < len(phases):
					// Earlier members DID record, so the panel hands over rather
					// than ending: the phase after it (the fix) applies the merged
					// list, which names this member as INCOMPLETE, and resumes the
					// same run. Checked BEFORE the sequence-end arm on purpose — a
					// member turned away by a usage limit is exactly the case the
					// bar names ("a usage-limited or dead member does not stop the
					// round"), and the sequence is not ending here: if the phase
					// that follows hits the same limit it says so itself, with the
					// wait that belongs to ITS end.
					s.i++
					continue
				case seqEnd != nil:
					// The run-level sequence-end break above was deferred for this
					// member so the round could be released first; end the attempt
					// here, with the run's wait machinery to follow.
					endSequenceEnd()
				}
				break
			}
			// CLA-784: a panel where ONLY optional seats recorded has no review
			// to apply — an optional seat's record is that seat's result, not a
			// review the fix was waiting for. Treat it exactly as a panel where
			// nobody recorded: leave the round sealed (no close) and spawn no
			// fix. Reached only at the panel's LAST member; an earlier optional
			// seat's record is kept while a required member may still record.
			if st.end.panelLast && !s.panelRecorded {
				log.Printf("%siteration %d: no REQUIRED member of the review panel recorded — the round is not closed and no fix spawns", labelOf(t), drainNum)
				break
			}
			// This member recorded. A recorded mid-panel member whose session was
			// then turned away by a run-level limit or transient failure continues
			// the panel exactly as a failed one does: its deliverable is in the
			// round, and the wait belongs to the end that actually stops the
			// sequence.
			//
			// The LAST member is the panel's close, so its sequence-end must do
			// what the failed last member's arm does: release the round while the
			// holder is still live (a sealed round would be voided by the
			// takeover, losing every member's record), then hand over to the fix
			// that applies the merged list. A close that fails is fail-closed —
			// the drain ends with the wait and no fix runs against a sealed
			// round.
			if seqEnd != nil {
				if live && !st.end.panelLast && s.i+1 < len(phases) {
					s.i++
					continue
				}
				// Close only when the seam did not already close the round for
				// this member. A session the adapter ALSO classifies as a limit
				// or transient can exit cleanly (or be capped with WIP), so the
				// seam runs first, matches the record and closes the round; the
				// sequence-end is then read by the arm below. A second close is
				// the plane's no_open_round, and failing closed on it would skip
				// the fix against a round that is already released.
				if live && s.panelRecorded && s.panelRound > 0 && !st.end.reviewRecorded {
					if cerr := d.closeReviewRound(ctx, t, *s.carried, s.panelRound); cerr != nil {
						log.Printf("%siteration %d: the panel's round %d could not be closed after %s recorded but ended the sequence (%v) — ending the drain without spawning the fix: a fix spawned now would run against findings the plane still has sealed",
							labelOf(t), drainNum, s.panelRound, member, cerr)
						endSequenceEnd()
						break
					}
				}
				if live && s.panelRecorded && s.i+1 < len(phases) {
					s.i++
					continue
				}
				endSequenceEnd()
				break
			}
		}

		// CLA-703: a LONE review-only phase (no panel) that never reached the
		// backlog is an infrastructure miss, not a dead phase and not a review
		// verdict. It gets its own drained ending: the claim was handed back (or
		// left to lapse) by drainPhase, the tally books the miss separately, and
		// the LOG names it so the operator does not read a review as having been
		// attempted when the tools were absent.
		//
		// CLA-738: the name is a log line only. The iteration Outcome stays the
		// four-word enum the plane accepts — prose there makes marshalReport
		// refuse the whole report, presence refresh included.
		if st.end.noMCP && !st.end.panelMember {
			d.tallyNoMCP(st.ph.Label(s.i), tc.HarnessFor(st.ph))
			note := fmt.Sprintf("no_mcp: the %s session made no successful clankerbar MCP call and recorded no review record", st.ph.Label(s.i))
			log.Printf("%siteration %d: %s — ending the drain; this is an infrastructure miss, not a review result", labelOf(t), drainNum, note)
			break
		}

		// CLA-386: a dead phase is a FAILED phase, not a completed one, and it is
		// not "ended without holding the task" either — it never finished the job
		// it was spawned to do. The claim was released back in drainPhase (a retry
		// re-claims, exactly like a transient one), so the only question left is
		// retry vs park. Consecutive dead phases on the SAME task: retry up to the
		// per-task budget, then park — a task that can kill four full sessions
		// reaches the operator rather than a fifth (CLA-396, raised from two by
		// the 2026-08-20 operator decision).
		//
		// Deliberately BEFORE the error break below: the dead classification
		// names a phase that produced nothing whatever its exit code, so a dead
		// phase is retried-then-parked, never a run-failing error — the non-zero
		// exit that would otherwise stop the daemon is itself part of the silent
		// death, not a verdict.
		//
		// The retry is scoped to the FIRST phase (`i == 0`), the one that claimed
		// its own task: a retry cannot re-seed a resumed phase — invocationFor
		// only substitutes the task/run placeholders and sets ResumeClaim when the
		// predecessor claim is present, which the dead handback cleared — so
		// retrying a dead RESUMED phase would spawn a session whose brief still
		// carries the literal {{taskId}}/{{runId}} and whose handback, salvage and
		// delivery checks are all switched off. A resumed phase's dead signature
		// still vetoes its checkpoint below (the false-premise guard applies to
		// every phase); it just is not retried or parked here — the drain ends and
		// the task returns to the queue, where a fresh claiming session retries it
		// with a valid seed (review finding). The operator decision's "retry the
		// implement phase N times, then park" is exactly `i == 0` in the shipped
		// [implement, review] sequence.
		//
		// CLA-396: a dead phase feeds BOTH counters, but the per-task park is
		// checked and taken FIRST — before the fleet counter's trip is even
		// consulted. The fleet counter persists on the Driver across drains and
		// only resets on a successful implement phase, so it can carry a residual
		// count left over from an earlier, unrelated task's death into THIS
		// task's own run of deaths. Checking fleet first would let that leftover
		// tip the fleet bound one death before this task reaches its own —
		// pre-empting a park this task legitimately earned with a fleet-wide
		// pause that blames the provider for what is, this time, actually the
		// task. Per-task takes priority: a task that has itself earned a park is
		// parked, full stop; only once that is ruled out does a fleet-wide run
		// of deaths across DIFFERENT tasks get to trip the fleet pause. A session
		// that never got past its claim is excluded from both by construction:
		// `end.dead` requires `res.Claim.Held()`, and a refused claim (lease_live,
		// a lost race) observes no task id, so its death counts toward neither
		// counter. This is the discriminator CLA-402's retrospective scan must
		// agree with.
		if st.end.dead {
			taskID := ""
			if st.end.claim != nil {
				taskID = st.end.claim.Claim.TaskID
			}

			if !st.last && s.i == 0 && taskID != "" && taskID == s.deadTask && s.deadRetries >= perTaskDeadBound-1 {
				how := "died producing nothing"
				if st.end.toolLoop != "" {
					how = "was killed (" + st.end.toolLoop + "), dead"
				}
				log.Printf("%siteration %d: the %s phase %s for the %dth consecutive time on this task — parking it per the 2026-08-20 decision (%d consecutive dead phases, then park)",
					labelOf(t), drainNum, st.ph.Label(s.i), how, s.deadRetries+1, perTaskDeadBound)
				d.parkDeadPhase(ctx, t, st.ph.Label(s.i), st.end.claim)
				s.parkFired = true // CLA-466: the record says parked, not dead
				// The task is parked for the operator and this drain is over; the
				// daemon carries on with the next task. Not a stop: the run did
				// not fail, one task reached a human.
				break
			}

			d.fleetDead[ti]++
			if d.fleetDead[ti] >= fleetDeadBound {
				d.fleetTrip(ctx, t, ti, drainNum, st.end.claim)
				s.tripFired = true // CLA-466: a fleet trip ends this drain on a death
				// The fleet trip pauses this target and this drain is over; the
				// in-flight task was already released with the dead phase. Not a
				// stop: the run itself did not fail.
				break
			}

			if !st.last && s.i == 0 {
				// Either the first dead phase this task produced, or a dead phase
				// on a task other than the one counted above: that task has died
				// once, and earns its own retry ladder rather than being parked
				// for deaths it did not have.
				if taskID != s.deadTask {
					s.deadRetries = 1
					s.deadTask = taskID
				} else {
					s.deadRetries++
				}
				if st.end.toolLoop != "" {
					log.Printf("%siteration %d: the %s phase was killed (%s) — retrying it (%d of %d) before any review brief sees the task",
						labelOf(t), drainNum, st.ph.Label(s.i), st.end.toolLoop, s.deadRetries, perTaskDeadBound)
				} else {
					log.Printf("%siteration %d: the %s phase died producing nothing (final step reason %q, no branch recorded) — retrying it (%d of %d) before any review brief sees the task",
						labelOf(t), drainNum, st.ph.Label(s.i), deadReason(st.end.claim), s.deadRetries, perTaskDeadBound)
				}
				continue
			}
		}

		// CLA-784: an optional (trial) seat's invocation error is that seat's
		// result, not the drain's — the same rule the parallel join applies with
		// its `!m.optional` guard. A member that recorded and THEN hit a harness
		// error has already delivered its record; ending the drain on it would
		// throw away the required members' round and the fix, the exact harm the
		// trial-seat contract exists to prevent. A REQUIRED member's error still
		// ends the drain exactly as before.
		if st.perr != nil && !(st.end.panelMember && st.end.panelOptional) {
			s.err = st.perr
			break
		}

		// A handoff the session emitted and drainPhase judged safe (clean exit,
		// trusted stream, task still held, prompt within the size cap). Two guards
		// remain, and both refuse toward the STANDARD path — the next configured
		// brief, or the drain ending — never toward truncating or rewriting the
		// session's prompt (CLA-352).
		if st.end.handoff != "" {
			switch {
			case s.chain >= handoffChainCap:
				log.Printf("%siteration %d: handoff refused — this task has already chained %d consecutive handoff respawns, the cap; falling back to the standard brief so a session cannot chain itself indefinitely",
					labelOf(t), drainNum, s.chain)
			case d.cfg.MaxIterations > 0 && drainNum+s.handoffs >= d.cfg.MaxIterations:
				log.Printf("%siteration %d: handoff refused — a respawn counts as an iteration and max-iterations (%d) is already spent; falling back to the normal path",
					labelOf(t), drainNum, d.cfg.MaxIterations)
			default:
				// Same phase index: the successor continues this phase's job. The
				// budget breaker at the top of the loop runs before the spawn.
				s.nextPrompt = st.end.handoff
				continue
			}
			// The handoff was REFUSED, so its exemption from the evidence gate ends
			// here: the exit is re-judged as the bare exit it really is (CLA-457).
			// With no durable, origin-reachable branch evidence that is a FAILED
			// empty phase — logged, tallied, never handed to the review brief.
			//
			// CLA-637: the review stage is over after all. drainPhase deliberately
			// left the matched review round SEALED when it detected the handoff —
			// the successor was going to close it at its own seam — but the refused
			// respawn is the last session that continuation was ever going to get,
			// so the round must be released HERE, before the checkpoint advances: a
			// fix phase spawned against a sealed round can neither read nor answer
			// (finding_sealed) the findings it was told to apply. A close that
			// fails refuses the checkpoint, exactly as the seam's own close does.
			nextReviewShaped := s.i+1 < len(phases) && config.ReviewShaped(phases[s.i+1].Prompt)
			if st.end.reviewRound > 0 && st.end.checkpoint && st.end.claim != nil && !nextReviewShaped {
				if cerr := d.closeReviewRound(ctx, t, *st.end.claim, st.end.reviewRound); cerr != nil {
					log.Printf("%siteration %d: the %s phase's review round %d could not be closed after its handoff was refused (%v) — refusing the checkpoint, because the next phase would be spawned against findings the plane still has sealed",
						labelOf(t), drainNum, st.ph.Label(s.i), st.end.reviewRound, cerr)
					d.tallyEmpty(st.ph.Label(s.i), tc.HarnessFor(st.ph))
					break
				}
			}
			if !st.last && st.end.empty {
				log.Printf("%siteration %d: the %s phase ended holding a task with no recorded branch on the origin remote — not a checkpoint (%s), so no review phase spawns (its handoff was refused)",
					labelOf(t), drainNum, st.ph.Label(s.i), st.end.emptyWhy)
				d.tallyEmpty(st.ph.Label(s.i), tc.HarnessFor(st.ph))
				break
			}
		}
		// This session ended without a respawn being accepted, so any chain is
		// broken: a later handoff starts counting from a standard-brief session.
		s.chain = 0

		// A held-but-empty exit is a FAILED phase (2026-08-24), not a checkpoint
		// and not an ordinary "ended without holding the task" end: the claim was
		// held to the end, it just carried no durable, origin-reachable branch
		// evidence. It gets ONE bounded retry, and only when the claim was ACTUALLY
		// released back to the queue — a fresh claiming session can then pick the
		// task straight back up. A recorded-but-unreachable branch was left to the
		// takeover path by the handback (never released), and retrying would seed a
		// session against a live lease nothing here renews, so such an exit ends the
		// drain. This also covers a handoff the guards REFUSED above (chain cap,
		// max-iterations): the handoff exemption ends the moment the successor is
		// not going to run, so the exit must be re-judged as the bare exit it really
		// is. Deliberately OUTSIDE the dead-phase ladder: the dead shape
		// retries-then-parks a task that can kill four sessions, while the empty
		// shape returns to the queue for the next drain — the tally measures it
		// without tripping the park or fleet machinery, which is how a fleet-wide
		// silent EOT shows up in the rate the operator watches.
		if st.end.empty {
			if !st.last && s.i == 0 && s.emptyRetries < 1 && st.end.releasedToQueue {
				s.emptyRetries++
				continue
			}
			break
		}

		// A non-final phase that did not reach its checkpoint leaves the next one
		// nothing to resume: the session settled the task itself (worked past its
		// brief and finished it), its stream could not be trusted to say either way,
		// or the adapter does not observe claims at all. The sequence is over, and
		// none of those is a failure — a task that reached in_review in one session
		// is a task that got done.
		if !st.last && !st.end.checkpoint {
			log.Printf("iteration %d: the %s phase ended without holding the task — nothing for the next phase to resume, so this drain ends here",
				drainNum, st.ph.Label(s.i))
			break
		}
		// CLA-638: the verification round is the stopping rule's gate. It is the
		// LAST configured phase, so the generic `!last` branches above never
		// applied to it; what follows is decided HERE from the durable records
		// the round just released:
		//
		//   - no open MUST → the gate passed. The verifier carried the terminal
		//     step (push check, PR, in_review) itself; the sequence is done.
		//   - open MUST(s) → another fix is spawned, unless the round cap or the
		//     non-convergence breaker parks the task first. Never one more round
		//     on the driver's own initiative.
		if isVerifyPhase(st.ph) {
			vres := harness.Result{}
			if st.end.claim != nil {
				vres = *st.end.claim
			}
			if st.end.verifyFailure != "" {
				// The seam could not judge the round: no durable record this run
				// wrote, a round that would not close, or a task that could not
				// be read back. That is the same fail-closed shape CLA-637 gives
				// a refused review checkpoint — the drain ends without the
				// hand-over and the claim is released/lapses, so a takeover can
				// retry it. It is not the stopping rule parking a disagreement.
				log.Printf("%siteration %d: the %s phase's round could not be judged (%s) — refusing the sequence: no hand-over, and no PR was accepted from this round",
					labelOf(t), drainNum, st.ph.Label(s.i), st.end.verifyFailure)
				break
			}
			if !st.end.verifyChecked {
				// A verification session that died or produced no judgeable exit.
				// End the sequence; the normal takeover path retries it.
				log.Printf("%siteration %d: the %s phase produced no judgeable verification round — ending the sequence without a hand-over",
					labelOf(t), drainNum, st.ph.Label(s.i))
				break
			}
			if len(st.end.verifyOpenMusts) == 0 {
				if st.end.verifyStatus == "in_progress" || st.end.verifyStatus == "ready" {
					log.Printf("%siteration %d: the %s phase closed round %d with NO open MUST but left the task %q — it recorded a pass but did not carry the terminal step (push check, PR, in_review); the sequence ends without a hand-over for the operator to see",
						labelOf(t), drainNum, st.ph.Label(s.i), st.end.verifyRound, st.end.verifyStatus)
				}
				if st.end.verifyRound == 0 {
					// CLA-638 × CLA-670: a BRANCHLESS round. Its record is the
					// legacy `## Review` block, so there is no plane open-MUST set
					// for the id-based gate to read — the verifier's own block
					// carried the verdict, and the sequence ends without claiming
					// the id-based stopping rule ran (it cannot, on this shape).
					log.Printf("iteration %d: the %s phase's verification round rests on the legacy %q block (this shape writes no durable record) — no plane open-MUST set to judge; ending the sequence",
						drainNum, st.ph.Label(s.i), config.ReviewRecordHeading)
					break
				}
				log.Printf("iteration %d: the %s phase closed round %d with NO open MUST — the stopping rule is satisfied and the sequence is done",
					drainNum, st.ph.Label(s.i), st.end.verifyRound)
				break
			}
			s.verifyRoundsRun++
			reason := nonConvergenceReason(st.end.verifyOpenMusts, st.end.verifyRound, s.verifyResolved)
			// The local fallback counts the same ROUNDS the plane does (CLA-686
			// D2). A review cycle is one review round plus the verification
			// rounds that follow it, and the plane's `counted` includes the
			// review round — so comparing verification rounds ALONE against the
			// cap allowed a cycle one round longer than the counted path, and
			// the park then read "Round 4 of the 3-round cap". `+1` is the
			// review round that opened this cycle.
			localRounds := s.verifyRoundsRun + 1
			capHit := st.end.verifyCounted >= config.ReviewRoundCap || localRounds >= config.ReviewRoundCap
			if reason == "" && !capHit && fixIdx >= 0 && st.end.checkpoint && s.carried != nil {
				for _, id := range st.end.verifyResolvedIDs {
					s.verifyResolved[id] = true
				}
				log.Printf("iteration %d: the %s phase closed round %d (%d non-void round(s) this cycle) with open MUST(s) [%s] — spawning another %s round against them",
					drainNum, st.ph.Label(s.i), st.end.verifyRound, st.end.verifyCounted,
					strings.Join(st.end.verifyOpenMusts, ", "), phases[fixIdx].Label(fixIdx))
				// CLA-639: the merged panel list belongs to the FIRST fix after
				// the panel, before any verification round has run. A verify
				// rewind is a later fix round: its grading is the verifier's own
				// record (plus the plane's open-MUST list), and reprinting the
				// panel's list would replace the verifier's findings with a
				// stale round. Clear the panel round so drainPhase falls back to
				// the shipped single-review path unchanged.
				s.panelRound = 0
				s.i = fixIdx
				continue
			}
			if reason == "" {
				switch {
				case capHit:
					reason = fmt.Sprintf("the %d-round cap for this review cycle is reached", config.ReviewRoundCap)
				case fixIdx < 0:
					reason = "the sequence has no fix phase to run between verification rounds"
				default:
					reason = "the round ended without holding the task, so no fix can be spawned against its open MUSTs"
				}
			}
			d.parkVerification(ctx, t, vres, reason, st.end.verifyRound, st.end.verifyCounted, st.end.verifyOpenMusts, st.end.verifyOpenDetails)
			s.parkFired = true
			s.carried = nil
			break
		}
		// A phase that ended dead was retried above and, on the retry, either
		// parked or advanced — either way this phase's dead-phase budget is spent.
		// Resetting on the advance keeps the retry per-PHASE rather than per-run:
		// two dead phases separated by a healthy one are not "consecutive", and
		// each phase earns its own retry ladder (CLA-386).
		s.deadRetries = 0
		s.deadTask = ""
		s.emptyRetries = 0
		// CLA-396: a successful IMPLEMENT phase resets the FLEET-wide dead-phase
		// counter. Only the implement phase runs the harness that can exhibit the
		// zero-usage death — the review phase runs on the claude harness, which
		// cannot — so a claude success must not vouch for the opencode gateway:
		// letting any success reset the counter would hide a fleet fault from the
		// detector that exists to find it. Reaching here means this phase did not
		// die and did not fail, so for the implement phase it is the "successful
		// implement" the reset is keyed to.
		if st.ph.Name == config.ImplementPhaseName {
			d.fleetDead[ti] = 0
		}
		// CLA-639: a member that DID record leaves its round open for the next
		// member (the close belongs to the panel's last phase, not to each
		// reviewer), so remember it — a later member that fails must still be able
		// to release what the earlier ones wrote.
		if st.panelMember && st.end.reviewRound > 0 {
			s.panelRound = st.end.reviewRound
		}
		s.i++
	}

	// The deferred handback (2, above). Skipped at every seam that led to another
	// phase; owed at the one that did not — including a final-phase handoff the
	// guards above refused, whose claim was held for a successor that never ran.
	s.carried = d.releaseCarried(ctx, t, s.carried)

	// CLA-662: the transient arm's wait, in the same place and for the same
	// reason as the limit wait below — after the sequence's handback, with NO
	// lease held. The rung is the target's streak of consecutive transient
	// ends, so a provider that keeps blipping the resumed phase is slowed
	// between drains instead of being re-drained the instant the last sequence
	// died (and, with implement and the resumed phase on different harnesses,
	// the only momentum the first-phase backoff could ever have was this).
	// The wait is bounded by retry_cap like every backoff(n), and STOP /
	// RESTART / RELOAD are honoured through waitOrStop exactly as they are
	// during the limit wait; the drain returns when it ends and Run polls again.
	//
	// Deliberately NOT fed into sidelineTarget, which the task that added this
	// asked us to consider: a sideline says "this target's harness is broken,
	// bench it" — its counters and log line are built for a drain that FAILED
	// to run a session — and this end returns err == nil. Run's success branch
	// would immediately clear the failure counter it read while leaving the
	// 15m+ skipUntil in place, so the log would report "the harness succeeded
	// again — rejoining the rotation" against a target still benched by a
	// sit-out nothing would then account for. Doing it properly means
	// redefining what a successful drain proves about the harness, well beyond
	// this bound's need: the ladder's growth, capped by retry_cap, plus
	// max_iterations is the brake the un-waited re-drain was missing.
	if s.transientRung > 0 {
		wait := d.resumedTransientDuration(s.transientRung)
		log.Printf("iteration %d: the sequence is over and no lease is held — waiting %s before polling again (transient failure %d in a row on %s; the run stays up)",
			drainNum, wait, s.transientRung, s.transientKey.harness)
		if d.waitOrStop(ctx, wait) {
			s.stop = true
		}
	}

	// CLA-631: the sequence ended on a rolling usage limit and the run is owed
	// its wait — after the handback above, deliberately, so the wait is taken
	// with NO lease held: the claim is back in the queue or, with pushed work,
	// left to lapse into a takeover exactly as the old stop path left it. The
	// wait itself is the first-phase path's own machinery, reused: the same
	// budget breaker inside supervisedWait, the same STOP/RESTART/RELOAD reads
	// in waitOrStop, the same probes on the harness that hit the limit, and the
	// same stated-reset handling — the difference is only that there is no
	// session left to re-run when it lifts, so the drain returns and Run polls
	// again. waitPastBudget is consulted first, exactly as on the first-phase
	// path: a reset beyond the run's remaining ceiling is not worth sleeping
	// through, so the run stops and says when to start a fresh one.
	if s.limitWait != nil {
		remaining, bounded := tc.Budget.Remaining(time.Since(prior.start))
		if until, over := waitPastBudget(s.limitWait.lim.ResetAt, remaining, bounded); over {
			log.Printf("iteration %d: the limit resets %s, in %s — more than the %s left of this run's ceiling; stopping now rather than waiting, so start a fresh run after the reset",
				drainNum, formatReset(until), time.Until(until).Round(time.Minute), remaining.Round(time.Minute))
			s.stop = true
		} else {
			log.Printf("iteration %d: the sequence is over and no lease is held — waiting for the reset%s before polling again (the run stays up)",
				drainNum, resetSuffix(s.limitWait.lim.ResetAt))
			ptokens, pcost, pstop := d.supervisedWait(ctx, s.limitWait.lim, s.limitWait.a, s.limitWait.ph, t, spend{
				start:  prior.start,
				tokens: prior.tokens + s.tokens,
				cost:   prior.cost + s.cost,
			})
			s.tokens += ptokens
			s.cost += pcost
			if pstop {
				s.stop = true
			}
		}
	}
	return s.tokens, s.cost, s.handoffs, s.stop, s.err
}

// drainWithRetries runs a single unphased drain — the whole task in one session.
// It is what a config with no `phases` gets, and it is kept as its own entry
// point because that is the shape every existing test drives.
//
// The phase comes from EffectivePhases, not an inline literal, so the unphased
// path carries the same resolution every other path does — the top-level or
// default turn cap (CLA-343), which is precisely what the inline zero-value
// version failed to do.
func (d *Driver) drainWithRetries(ctx context.Context, drainNum int, t Target, prior spend) (tokens int, cost float64, stop bool, err error) {
	phases := d.targetCfg(t).EffectivePhases()
	ph := phases[0]
	tokens, cost, stop, _, err = d.drainPhase(ctx, drainNum, 0, 0, "", ph, true, nil, t, prior, phases, panelState{}, phaseRunOpts{})
	return tokens, cost, stop, err
}

// phaseRunOpts are the per-spawn facts a panel's parallel fan-out adds to
// drainPhase (CLA-697). The zero value is every other caller: the phase runs in
// the checkout sessionDir resolved, and drainPhase itself closes the review
// round when the review stage is over.
type phaseRunOpts struct {
	// worktree, when non-empty, is the session's start directory instead of the
	// one sessionDir resolved: the member's own DETACHED checkout at the reviewed
	// tip. The brief tells the session it is there, so a member cannot wander
	// into the shared task worktree.
	worktree string

	// panelParallel says the JOIN that spawned this member owns the review
	// round's close. A member must not close it itself: its siblings may still be
	// recording, and the close is what releases the findings they must not see.
	panelParallel bool

	// panelSpend, when non-nil, is this member's handle on its panel's shared
	// spend ledger (CLA-697 review finding 1.review-1.1). Every attempt's counted
	// spend is published to it the moment the ladder counts it, and the ladder's
	// pre-spawn budget check measures the run's totals PLUS the siblings' counted
	// spend. Without it each member compared the same prior snapshot plus its own
	// local spend against a run-wide ceiling none of them could see the others
	// approaching. Nil for every caller that is not a parallel panel member.
	panelSpend *panelSpendHandle

	// onSessionLaunch, when non-nil, is called immediately before the adapter is
	// invoked — the first moment this phase's session launch has actually BEGUN —
	// on every attempt the ladder makes. A parallel panel member uses it to
	// release the NEXT member's start: signalling when its worktree was ready let
	// a later member's own setup and launch overtake an earlier member's, so the
	// scheduler's list-order guarantee (CLA-697) did not extend to the adapters
	// (CLA-738 review 3.2). Idempotent on the panel side, so a retry's repeat
	// call is harmless. Nil for every other caller.
	onSessionLaunch func()
}

// panelWorktreeNote is prepended to a PARALLEL panel member's brief (CLA-697).
// The shipped brief says to work in "the worktree for branch X", which is the
// task's shared one; giving each member its own detached checkout would be
// contradicted by that sentence, so the driver states the fact first.
const panelWorktreeNote = "YOUR CHECKOUT: you are one member of a review PANEL running in PARALLEL " +
	"with its siblings. The driver created a DETACHED worktree of the task's repository at the reviewed tip " +
	"and started this session IN it; work here for the whole session. Do NOT switch to, edit, or leave " +
	"artifacts in the task's shared worktree — another member and the fix phase use that one. This checkout " +
	"is discarded when you end and is never salvaged, so put nothing you need in it. Nothing you do here can " +
	"affect a sibling's checkout, so a probe is yours alone. "
