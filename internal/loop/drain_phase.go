package loop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/logfile"
)

// drainPhase runs ONE phase to a clean finish, absorbing usage-limit pauses
// (supervised wait) and transient blips (exponential backoff) by re-running the
// SAME session — neither costs a drain count. Returns the tokens/cost consumed on
// a clean finish; stop=true if a STOP/cancel landed during a wait; err only on a
// genuine, non-retryable failure (or exhausted retries).
//
// held is non-nil only when this phase ended cleanly, is not the last, and left
// the task's claim open for its successor — see drainPhases, which either carries
// it to the next phase or hands it back.
//
// prev is the claim the PREVIOUS phase left held, and is what fills the task/run
// placeholders in this phase's prompt. nil for the first phase, which has no run
// to resume and claims one of its own.
//
// prior carries the run's ceilings down into the drain, because the budget breaker
// in Run only runs BETWEEN drains and every wait in here happens inside one — see
// the check at the top of the loop, and waitPastBudget.
// phaseIdx is this phase's 0-based position in the sequence, carried only so a
// phase with no name can still be NAMED in a log line — ph.Label(phaseIdx) falls
// back to "phase 2". An unphased drain passes 0 and never reaches those lines.
// ti is the target's index, for the fleet claim reflector wired into the lease
// renewer below (CLA-510).
//
// phases is the sequence as CAPTURED when the drain started — the same slice
// drainPhases iterates. It is what this phase's IDENTITY is read from (its
// built-in brief, its position in the sequence), never the live config
// (CLA-664): a RELOAD marker is applied inside waitOrStop, which runs during a
// first-phase limit wait or a transient backoff in THIS drain, so the live
// config can be a DIFFERENT sequence than the one in flight. Reading it here
// would let the two disagree — the in-flight review gated by the reloaded
// position, the "PHASE i of N" text rewritten — while drainPhases still walks
// the captured slice. The rule the capture enforces: a RELOAD takes effect at
// the next drain, never mid-sequence.
//
// panel is what the panel stage this phase follows left behind (CLA-639): the
// round its members' records landed in, zero when no panel ran or no member
// recorded. It is the fix phase's grading input (mergedFindingsDigest reads
// that round and no other); the unphased drain and a phase that does not follow
// a panel pass the zero value.
func (d *Driver) drainPhase(ctx context.Context, drainNum int, ti int, phaseIdx int, tag string, ph config.Phase, last bool, prev *harness.Result, t Target, prior spend, phases []config.Phase, panel panelState, opts phaseRunOpts) (tokens int, cost float64, stop bool, end phaseEnd, err error) {
	tc := d.targetCfg(t)
	shape := phaseShapeOf(phases, phaseIdx, prev)
	captured, nextReviewShaped, requireReviewRecord := shape.captured, shape.nextReviewShaped, shape.requireReviewRecord
	isVerify, reviewOnly := shape.isVerify, shape.reviewOnly
	panelMember, panelOptional, panelLast := shape.panelMember, shape.panelOptional, shape.panelLast
	// The review seam's freshness baseline, read ONCE before the attempt ladder:
	// the phase's outcome as the review found it. A handoff respawn re-enters
	// drainPhase with the same carried claim and takes a fresh baseline, so a
	// successor must still write the record its own session is responsible for.
	//
	// CLA-663: an unreadable baseline refuses the phase BEFORE it spawns. The
	// seam cannot prove a record is this trip's from a baseline it never read —
	// so the old order spawned the full review session and then threw its
	// checkpoint away, and the claim (HasWIP) lapsed into a takeover that
	// re-ran implement AND review. Refusing here pays the read — already
	// retried by readReviewBaseline — and nothing else, and leaves the claim
	// with the same disposition a seam refusal leaves it: released when it is
	// releasable, left to lapse into a takeover when it carries pushed work.
	//
	// The reviewer role is the other half of the gate's key (CLA-637): one
	// name, read from the same resolved config the brief is built from, so the
	// session records under exactly the role the seam will match.
	review := reviewGate{required: requireReviewRecord}
	end.panelMember = panelMember
	end.panelOptional = panelOptional
	end.panelLast = panelLast
	if review.required {
		var refused bool
		review, end, refused = d.openReviewGate(ctx, drainNum, t, ph, phaseIdx, phases, prev, opts, panelOptional, review, end)
		if refused {
			return 0, 0, false, end, nil
		}
	}
	// The adapter for THIS phase, resolved once: everything below — the spawn, the
	// classification of what came back, and the probe of any usage limit it hit —
	// has to be the harness that actually ran, not the run's default. Resolved
	// here rather than per attempt so a retry ladder cannot straddle two harnesses.
	a, aerr := d.adapterFor(ph)
	if aerr != nil {
		return 0, 0, false, end, fmt.Errorf("iteration %d: phase %q: %w", drainNum, ph.Label(phaseIdx), aerr)
	}
	// Where this phase's sessions run, resolved ONCE per phase (CLA-437): the
	// task repo's checkout when it resolves, the target's workdir when nothing
	// does. Before the attempt ladder, so a repo that resolves to nothing fails
	// the phase with repo_not_found before anything is spawned — a session that
	// starts in the multi-repo parent reads a sibling's rule files and, on a
	// cwd-scoped grant, hits the wall this replaces. The same directory then
	// serves every retry: the task does not change between attempts.
	//
	// The second value is the task this phase was briefed on (CLA-451): exact for
	// a resumed phase, the spawn-time queue-head peek for a fresh one. Held onto
	// here because the seam needs it AFTER the session ends — it is the one task
	// the driver can ask the plane about when the stream carried no claim state.
	spawnDir, briefedTask, briefedRepo, serr := d.sessionDir(ctx, t, prev)
	if serr != nil {
		return 0, 0, false, end, fmt.Errorf("iteration %d: phase %q: %w", drainNum, ph.Label(phaseIdx), serr)
	}
	// CLA-697: a parallel panel member starts in its OWN detached checkout at
	// the reviewed tip. The repo's resolution above still ran, so the briefed
	// task/repo and the PR-base probe are the ones it would have had anyway; only
	// the directory the session runs in (and therefore the tree it can dirty)
	// differs.
	if opts.worktree != "" {
		spawnDir = opts.worktree
	}
	// The branch this task's repo PRs target, resolved ONCE per phase like the
	// spawn directory above, and for the same reason: it does not change between
	// attempts. Filled into the prompt's {{prBase}} placeholder by invocationFor
	// (CLA-608). Never fails here: an unresolvable probe falls back to the
	// configured integration branch, which is the text the briefs carried
	// before this feature existed.
	prBase := d.prBaseFor(ctx, t, spawnDir, briefedRepo)
	fixGrading := d.fixGradingFor(ctx, t, prev, captured, phases, phaseIdx, panel)
	verifyDigest, end, verifyRefused := d.verifyDigestFor(ctx, drainNum, t, ph, phaseIdx, prev, isVerify, end)
	if verifyRefused {
		return 0, 0, false, end, nil
	}
	r := &phaseRun{
		d: d, t: t, tc: tc, ti: ti, drainNum: drainNum, phaseIdx: phaseIdx, tag: tag, ph: ph,
		last: last, prev: prev, prior: prior, phases: phases, panel: panel, opts: opts,
		shape: shape, a: a, spawnDir: spawnDir, prBase: prBase, briefedTask: briefedTask,
		fixGrading: fixGrading, verifyDigest: verifyDigest, review: review, end: end,
	}
	for {
		// The breaker, from inside the drain. Every path that loops back here has
		// just waited — a supervised wait on a usage limit, an exponential backoff
		// on a transient blip — and each of those waits used to be unbounded by
		// anything but the wall clock, which is only one of three dials and the one
		// an operator is least likely to have set. A drain that re-spawns a paid
		// session on a loop is the expensive failure this stops, whatever put it
		// there (CLA-258).
		spentTokens, spentCost := r.sofar()
		if dim := d.budgetTrip(spentTokens, spentCost, time.Since(prior.start)); dim != "" {
			log.Printf("iteration %d: budget reached mid-drain: %s — stopping (tokens=%d cost=$%.2f elapsed=%s)",
				drainNum, dim, spentTokens, spentCost, time.Since(prior.start).Round(time.Second))
			return r.tokens, r.cost, true, r.end, nil
		}

		// The breaker's blind spot, bounded. The check above can only stop spend it
		// was told about; an attempt that died before its harness reported usage
		// added nothing to either accumulator, so a ladder of those attempts leaves
		// a token or cost ceiling permanently unreachable and only max_wall_clock -
		// the dial an operator is least likely to have set as a spend bound - can
		// end the run. Reached only on an attempt re-spawn (the first pass has
		// counted nothing), so a single silent attempt that ends the phase some
		// other way never trips it.
		if bound := d.cfg.ZeroSpendAttemptBound(); r.noUsage >= bound {
			return r.tokens, r.cost, false, r.end, fmt.Errorf(
				"iteration %d: %w: %d consecutive attempts died before %s reported any usage, so nothing the budget breaker reads was ever fed - the sessions are failing early rather than working (see the attempt logs in the state dir; raise max_zero_spend_attempts to allow more)",
				drainNum, errZeroSpendLoop, r.noUsage, a.Name())
		}

		// Each attempt streams live to the terminal and to its own logfile. The name
		// carries the drain number and attempt counter as well as the timestamp: two
		// attempts in the same second (a sub-second backoff) would otherwise share a
		// name, and a name already taken is REFUSED rather than truncated.
		//
		// The random tail is not decoration. This log carries the session's whole
		// output — prompts, tool arguments, tool output, occasionally a token — and
		// the rest of the name is derivable to within a second by the session that
		// produced it. Unguessable names mean a session that CAN write to the state
		// dir (an operator who pointed state_dir back inside the workdir) still
		// cannot pre-plant a decoy at the name we are about to use. statedir.Create
		// refuses an existing path either way; this stops it turning into a denial
		// of the log itself.
		inv, ierr := d.invocationFor(t, phaseIdx, ph, prev, spawnDir, prBase, phases)
		if ierr != nil {
			// The session env refused to resolve (a failing or empty-output
			// fromCommand, an unreadable @path file — the error names the
			// variable). REFUSING THE SPAWN is the fail-closed contract: no
			// session starts without its declared env (CLA-462). This is not a
			// retryable failure — nothing was spawned and the cause is config,
			// not a blip — so it fails the phase like a bad spawnDir does.
			return 0, 0, false, r.end, fmt.Errorf("iteration %d: phase %q: refusing to spawn: %w", drainNum, ph.Label(phaseIdx), ierr)
		}
		// The graded list is PREPENDED after placeholder substitution, never
		// run through the one-pass replacer: it is driver-read text from the
		// plane, and a finding quoting `{{model}}` must stay a quote.
		if fixGrading != "" {
			inv.Prompt = fixGrading + inv.Prompt
		}
		// The verification digest is the same contract for the verify phase
		// (CLA-638): plane-read ids and findings, prepended verbatim.
		if verifyDigest != "" {
			inv.Prompt = verifyDigest + inv.Prompt
		}
		// CLA-697: a parallel member's own checkout is a fact only the driver
		// knows — the panel brief still says "work in the worktree for branch X",
		// which is the shared one. State it first, so the session's cwd and its
		// brief agree.
		if opts.worktree != "" {
			inv.Prompt = panelWorktreeNote + inv.Prompt
		}
		logName := fmt.Sprintf("iteration-%s-d%d%s-a%d-%s.log",
			time.Now().Format("20060102-150405"), drainNum, tag, r.retries, randomTail())
		f, ferr := d.state.Create(logName)
		logPath := filepath.Join(d.state.Path(), logName)
		// The console is where the adapter streams the session: the file, and
		// NOT the terminal unless the operator asked for the echo (CLA-701).
		//
		// The stream is the whole harness transcript — prompts, tool arguments,
		// tool output — and once it also went to the daemon's stderr it left only
		// the last few minutes of one session in the operator's scrollback, with
		// none of the daemon's own lines beside it. Those lines (spawn, exit,
		// classification, why the run ended) are the record of what the driver
		// DECIDED, and nothing re-reads a terminal; the file keeps the stream in
		// full, so echoing it only duplicates it over the top of the useful part.
		//
		// `echo_sessions: true` restores the old live echo for an operator
		// watching one session. What stays live either way is the daemon's own
		// logging (which also lands in daemon.log). Nothing harness-side is
		// echoed separately: claude's stderr is part of its stream and lands in
		// the iteration log with the rest of it, and a session worth watching
		// live is why the opt-in exists.
		var echo io.Writer
		if d.cfg.EchoSessions {
			echo = d.echoOut
			if echo == nil {
				echo = os.Stderr
			}
		}
		// sink names where the stream actually LANDS, on the spawn line the
		// operator joins the daemon log and the transcript by: whenever the
		// iteration log could not be opened (ferr != nil) there is no such file
		// to send a reader to, so the sink says so and names the fallback
		// instead of a path that was never created.
		sink := "log: " + logPath
		switch {
		case ferr == nil && echo != nil:
			// Both sinks get the stream, INDEPENDENTLY: a terminal that has gone
			// away must not cost the iteration log the chunk it would have
			// received — io.MultiWriter stops at the first failing writer, which
			// is the order that fails worst here (echo first, file second).
			inv.Console = logfile.Tee(echo, f)
		case ferr == nil:
			inv.Console = f
		case echo != nil:
			// No log file: keep the live view rather than swallow the session.
			inv.Console = echo
			sink = fmt.Sprintf("log: none (could not open %s: %v - the session's stream goes to the terminal)", logPath, ferr)
		default:
			// No file and no echo: the stream has nowhere to go. Say so once —
			// the daemon's own lines still land in daemon.log — rather than
			// failing a paid session over its transcript.
			inv.Console = io.Discard
			sink = fmt.Sprintf("log: none (could not open %s: %v - the session's stream is discarded; the daemon log still records its own lines)", logPath, ferr)
		}
		// The spawn line stamps what the session was actually spawned WITH:
		// phase and the resolved model id inv.Model carries (ModelForPhase's
		// output, never the tier alias). A token/cost line in an old log is then
		// attributable to the settings of THAT day rather than inferred from
		// whatever the config file says now (CLA-345). An empty model= is itself
		// evidence: no explicit model was configured at spawn time, and the
		// harness ran its own default.
		if r.retries == 0 {
			log.Printf("iteration %d %s- spawning %s (phase=%s model=%s %s)", drainNum, labelOf(t), a.Name(), ph.Label(phaseIdx), inv.Model, sink)
		} else {
			log.Printf("iteration %d %s- retry %d, spawning %s (phase=%s model=%s %s)", drainNum, labelOf(t), r.retries, a.Name(), ph.Label(phaseIdx), inv.Model, sink)
		}

		// CLA-358: while THIS child runs, renew whatever task-lease it holds.
		// Started before Invoke so a resumed phase's seeded claim renews from
		// the first tick; stopped the moment Invoke returns, which is what makes
		// renewal pause whenever no child process is running - between attempts,
		// across supervised waits, after exit. Inside the window the adapter may
		// run its own resurrection machinery (opencode CLA-406): those processes
		// continue the SAME session on the SAME claim, so renewal spans them.
		renewer := d.startLeaseRenewal(ctx, ti, t, &inv, fmt.Sprintf("%siteration %d: ", labelOf(t), drainNum))
		// The launch boundary itself (CLA-738 review 3.2): a panel member releases
		// its successor here, so nothing this member did before its adapter call
		// (worktree, brief, budget check) can be overtaken by a later member's.
		if opts.onSessionLaunch != nil {
			opts.onSessionLaunch()
		}
		res, ierr := a.Invoke(ctx, inv)
		renewer.stop()
		// CLA-782: record the routes this session was denied and write each SWITCH
		// to both logs before the iteration log is closed. Runs before every
		// classification below, because a transient retry must already start on the
		// route that worked.
		d.noteRouteFallbacks(t, drainNum, inv, res)
		if f != nil {
			_ = f.Close()
		}
		// The orderly-cap classifications, computed here (before the salvage) so
		// the salvage can tell whether a clean tree means "produced nothing" — a
		// question the caps answer: a capped session is an orderly end with its
		// own marker, not a silent death, and an untrusted stream's finish reason
		// cannot be read at all. Purely adapter reads of `res`, so moving them up
		// changes nothing else (review finding).
		capped := a.TurnCapped(res)
		ceiling := a.TokenCeilingHit(res)
		wallclock := a.WallClockCapped(res)
		zeroUsage := a.ZeroUsageUnknown(res)
		// CLA-739: the adapter killed this session for making the same failing
		// tool call over and over. That is a DEAD session - no delivery, whatever
		// it pushed before it started looping - and it joins the dead signature
		// everywhere below: the seam veto, the dead-phase ladder, the tally, the
		// panel's failed-member path and the fleet record. Not a cap: a cap is an
		// orderly end of a session doing its job, and this one had stopped.
		toolLoop := res.ToolLoop
		producedNothing := res.Untrusted == "" && !capped && !ceiling && !wallclock && (deadPhase(res) || toolLoop != "")
		// Rescue whatever the session left uncommitted, FIRST — before the handback,
		// because a successful salvage changes what the handback should do: a task
		// with a branch recorded on it is no longer safe to release (CLA-314).
		//
		// NOT for the review-only gate (CLA-split): its deliverable is the record
		// on the PLANE, not the tree, and anything uncommitted in its worktree is
		// generated output from running checks — which the brief forbids it to
		// commit. Salvaging it would (a) commit the unattributable change the
		// review's own record forbids, and (b) push a new tip AFTER the reviewed
		// tip it just recorded, so the fix phase's tip comparison would fail and
		// it would park a healthy task for a mismatch the DRIVER created. A
		// review that genuinely dies leaving source edits loses them, which is
		// exactly what "review only, change nothing" means.
		//
		// A tool-loop kill is salvaged like any other end, but not with the
		// emphatic "produced nothing" line: a clean tree there may well be work
		// the session committed and pushed before it started looping.
		if !requireReviewRecord {
			d.salvageStrandedWork(ctx, t, &res, producedNothing && toolLoop == "", inv.WorkDir)
		}
		// Hand back anything the session was still holding, BEFORE deciding what to
		// do next — every branch below either waits, retries or returns, and all of
		// them leave the lease unattended. Above the ierr check too: on every
		// adapter, an Invoke whose run failed with something other than an exit
		// status returns whatever the stream had announced by then, fully parsed,
		// alongside the error (CLA-299) — claude's drain path parses its stream as
		// it arrives; codex and opencode run their parsers to completion before
		// classifying the failure — so a claim observed there is real and must not
		// be dropped just because the process died untidily. A launch failure
		// yields a zero Result, which releases nothing.
		//
		// UNLESS this phase reached its checkpoint with another phase still to run.
		// Then the sequence is not over: the next phase resumes THIS run with
		// heartbeat(runId) instead of claiming, so handing the lease back here
		// would post the task to the queue while we are still working it, and the
		// next phase would find someone else's task or none at all. drainPhases
		// owns the handback in that case and performs it the moment the sequence
		// ends for any other reason.
		//
		// Only an ORDERLY end holds it open, and each conjunct is load-bearing: a
		// retry deliberately re-claims a half-done task with a fresh session
		// (which is why an ordinary non-zero exit still releases), and an untrusted
		// stream must not be read for claim state at all, because the settle we
		// never saw may be in the bytes that never arrived (CLA-262).
		//
		// A turn-capped end CAN be orderly — but only if something durable came out
		// of it. The salvage above is not a guarantee: it commits nothing on a
		// clean worktree, and refuses outright on a diverged remote or a tree in
		// the middle of a merge. A phase 1 that burned its turns reading and
		// planning without writing therefore leaves no branch at all, and calling
		// that a checkpoint spawns a session told "an earlier session has already
		// implemented, committed and pushed" and pointed at a branch that is not
		// there — a paid session that can only fail, or worse, move the task to
		// review over no work.
		//
		// HasWIP is exactly that signal: it is set by the only two things that make
		// a checkpoint real — the phase recording its own branch, or the salvage
		// recording one for it and the PLANE accepting the record.
		// A session the adapter ended on its wall-clock cap is the third member of
		// the same family: an orderly cut-off mid-thought, whose survivability rests
		// on the salvage exactly as a turn cap's does — so it earns a checkpoint on
		// the same terms, HasWIP and all (CLA-368). Classified by the PHASE's adapter
		// (`a`), never the run's: "did this session end on its own wall clock" is a
		// question only the harness that RAN it can answer, and under per-phase
		// harnesses (CLA-366) that harness is not necessarily d.h.
		// CLA-700: the exit status, and the ONE exception it does not decide. A
		// stream that shows the session took a provider error and then RECOVERED
		// from it — retried and carried on — ends on something other than the
		// error, and opencode 2.x exits non-zero anyway (pinned by the
		// conformance suite). Judging that exit by its status is what stranded two
		// delivered phases on 2026-10-19: the driver read the status as "failed
		// attempt" and either retried (a resumed phase ending the sequence, its
		// lease left to lapse into a takeover) or stopped, with the pushed work
		// already on the origin remote and the task held mid-sequence.
		//
		// So a recovered exit is asked the DELIVERY question instead, through the
		// same evidence gate an orderly checkpoint uses, read once below. It is
		// deliberately narrow: it needs the adapter's own marker (a stream whose
		// events include a recovered error and no terminal one), so a plain exit 1
		// from a harness with no such marker — or from a session that really did
		// stop on an error — keeps every shipped classification. A recovered exit
		// is eligible for a checkpoint on the evidence alone; when the evidence
		// says it delivered nothing, it is the existing held-but-empty shape
		// (retry once, then hand the task back), never a run-ending stop over an
		// exit status the harness itself explains.
		//
		// CLA-739: a tool-loop kill is none of these. The process usually reads
		// exit -1 from the kill, but the session can exit on its own in the
		// instant before the kill lands, and a zero exit must not turn a looping
		// session into a checkpoint - nor can its stream's recovered provider
		// errors (the incident's own log carries one).
		orderlyExit := toolLoop == "" && (res.ExitCode == 0 || capped || ceiling || wallclock)
		recoveredExit := !orderlyExit && toolLoop == "" && res.Untrusted == "" && harness.RecoveredErrors(res) > 0
		checkpointable := toolLoop == "" && (res.ExitCode == 0 || ((capped || ceiling || wallclock) && res.Claim.HasWIP) || recoveredExit)
		// CLA-386: a session whose final step finished with reason "unknown" and
		// left no branch recorded produced NOTHING — a dead phase, not a completed
		// one. It must not earn a checkpoint even on a zero exit, because a
		// checkpoint is what hands the next phase a brief that claims "an earlier
		// session has already implemented, committed and pushed". Two real
		// implement phases died exactly this way and the seam advanced to review
		// on the false premise; the signal has to veto the checkpoint.
		//
		// A capped session is excluded even when its stream carries "unknown": the
		// wall-clock kill can land mid-turn, but a cap is an orderly end with its
		// own marker — retrying it would re-spend against the same cap and parking
		// it would read a doing-its-job backstop as a silent death.
		dead := !last && res.Untrusted == "" && res.Claim.Held() && !capped && !ceiling && !wallclock && (deadPhase(res) || toolLoop != "")
		// CLA-402: book this session into the run's dead-phase tally, per phase
		// label and harness, whatever the seam decided about retrying it. The
		// tally's dead classification deliberately drops the `!last` conjunct
		// that guards the checkpoint veto — a dead LAST phase is still a dead
		// phase, and the rate is a measurement, not a seam decision (see
		// deadtally.go). The report rides the end (CLA-639): the panel's member
		// handler books a failed member into the same tally, and must not count
		// an exit this call already booked.
		bookedDead := d.tallyDead(ph.Label(phaseIdx), tc.HarnessFor(ph), res, capped, ceiling, wallclock)
		r.end = phaseEnd{}
		r.end.deadBooked = bookedDead
		// CLA-639: the panel identity of this pass, for drainPhases' member
		// handler. Set from the CAPTURED phase (a handoff respawn replaced
		// ph.Prompt, never its identity), so a skipped member is recorded against
		// the reviewer that was meant to run.
		r.end.panelMember = panelMember
		r.end.panelOptional = panelOptional
		r.end.panelLast = panelLast
		if res.Claim.TaskID != "" {
			r.end.claim = &res
		}
		// CLA-630: the session this pass actually spawned, in exactly the words
		// the spawn line above used — the phase's label, the phase's resolved
		// harness, and the resolved model (empty when the harness default
		// applied; never guessed). Set on every attempt, so the LAST attempt's
		// values are the ones that ride: the entry describes the PASS, and a
		// retry within the ladder cannot change what the pass ran — only a
		// reload landing mid-ladder can.
		//
		// CLA-691: its spend is filled in later, at the point the loop counts the
		// attempt's spend toward the iteration (recordPassSpend), so the two can
		// never disagree about which attempts were counted. The pass sums every
		// counted attempt, because the loop counts each one as its own paid
		// session: an entry reporting only the surviving attempt's money would
		// not add up to the iteration total.
		r.end.session = &fleet.Session{
			Phase:   ph.Label(phaseIdx),
			Harness: a.Name(),
			Model:   inv.Model,
			RunID:   sessionRunID(res, prev),
		}
		// CLA-739: the tool-loop kill's cause rides the session entry on the
		// fleet report, as CLA-703's no_mcp does - it is why this session
		// produced nothing.
		if toolLoop != "" {
			r.end.session.Cause = toolLoop
		}
		r.end.toolLoop = toolLoop
		// The wider classification rides the end for the fleet record only — see
		// phaseEnd.producedNothing.
		r.end.producedNothing = producedNothing
		// A handoff block ending the session's final message (CLA-352) holds the
		// task open exactly like a phase seam — even on the LAST phase, where
		// there is otherwise no successor to hold it for. The remaining guards
		// live in drainPhases; if they refuse, the deferred handback there
		// releases what this held.
		handoff := detectHandoff(drainNum, t, res)
		// A handoff the session emitted is evidence it worked, even where the
		// driver then REFUSES to chain it (a panel member, a verification round).
		// CLA-703's no-MCP backstop must not fire on such a session: it reached a
		// conclusion and wrote a successor prompt, which a session with no tools
		// does not do.
		emittedHandoff := handoff != ""
		// CLA-638: a verification round does not chain via a handoff. Its record
		// is the deliverable, and the DRIVER decides what follows (another fix, a
		// park, or nothing) from the open MUSTs the record releases; a successor
		// prompt here would have nothing to continue that this round has not
		// already written, and honoring it would defer the close/evaluation the
		// driver owes the round.
		if isVerify && handoff != "" {
			log.Printf("%siteration %d: the verification round emitted a handoff block; verification rounds do not chain — the driver judges the round's record and decides whether another fix runs", labelOf(t), drainNum)
			handoff = ""
		}
		if panelMember && handoff != "" {
			log.Printf("%siteration %d: the %s panel member emitted a handoff block; panel members do not chain — a successor would continue this reviewer's role, but the phase after it is the next member, and the member's record is its deliverable", labelOf(t), drainNum, ph.Label(phaseIdx))
			handoff = ""
		}
		// The seam contender: an orderly end still holding the task, minus the dead
		// signature. This is the gate the incident put in question — see below.
		canCheckpoint := res.Untrusted == "" && checkpointable && res.Claim.Held() && !dead
		// CLA-639: a panel member may be the LAST phase of the sequence when the
		// operator wrote no fix after the review, and its record gate must still
		// run — that is how the round closes and how the merged list is released.
		// `panelLast` (rather than !last) admits it, and only its review record
		// gate: a settled or non-checkpointable exit is excluded exactly as before.
		reviewable := (!last || panelLast) && canCheckpoint
		// CLA-457: held-ness alone must never advance the seam. Since CLA-358 the
		// driver's lease renewer renews a claim for a session's whole lifetime, so
		// ANY session that claimed ends held — "still holding" is a constant, not
		// a checkpoint signal. A non-final phase reaches its checkpoint ONLY when
		// the plane's record carries the phase's exit evidence: a branch recorded
		// by this run whose tip really exists on the origin remote (the implement
		// phase in the shipped sequence). The evidence is computed even on a
		// handoff: an HONORED handoff continues the same phase regardless (its
		// successor has nothing to falsely resume), but a REFUSED handoff must be
		// re-judged as the bare exit it really is, and this is the only place the
		// verification can run.
		evidenceOK := true
		evidenceUnverified := false
		evidenceWhy := ""
		evidenceTip := ""
		if isVerify && res.Untrusted == "" && (orderlyExit || recoveredExit) {
			// The verification seam runs even on the LAST phase. Unlike every
			// other terminal phase its evidence is not a checkpoint (nothing
			// follows to resume) but the durable record plus the open-MUST set:
			// that set is what decides whether ANOTHER fix is spawned, whether
			// the sequence is done, or whether the round cap parks the task
			// (CLA-638, B12).
			r.end.verifyChecked = true
			r.end.branch, evidenceTip, evidenceOK, evidenceUnverified, evidenceWhy = d.checkpointEvidence(ctx, t, res, inv.WorkDir, &r.review)
			if r.review.state == "sealed" {
				r.end.reviewRound = r.review.round
			}
			if evidenceOK && r.review.required && r.review.round > 0 {
				if r.review.state == "sealed" {
					// The verifier did not release its own round; the driver does,
					// exactly as for the review gate.
					if cerr := d.closeReviewRound(ctx, t, res, r.review.round); cerr != nil {
						evidenceOK = false
						evidenceWhy = fmt.Sprintf("verification round %d could not be closed (%v) — its findings would stay sealed and the next fix would have nothing to apply", r.review.round, cerr)
					}
				}
				if evidenceOK {
					// Closed (or already released by the verifier's terminal call);
					// nothing downstream may try to close it again.
					r.end.reviewRound = 0
					r.end.verifyRound = r.review.round
					// Fail-closed, so retried like the spawn read (CLA-686 D3):
					// one blip here ends a sequence the verification round has
					// already been paid for.
					if st, read := d.readTaskStateRetry(ctx, t, res.Claim.TaskID, "after the verification round closed"); !read {
						evidenceOK = false
						evidenceWhy = "the task could not be read after the verification round closed, so its open MUSTs cannot be judged"
					} else {
						r.end.verifyOpenMusts = append([]string(nil), st.ReviewOpenMustIDs...)
						r.end.verifyOpenDetails = openMustDetails(st.ReviewOpenMustIDs, st.Reviews)
						r.end.verifyCounted = st.ReviewCounted
						r.end.verifyResolvedIDs = resolvedFindingIDs(st.Reviews)
						r.end.verifyStatus = st.Status
					}
				}
			} else if evidenceOK && r.review.required {
				// CLA-638 × CLA-670: the checkpoint rested on the legacy `## Review`
				// block (the branchless arm of the gate), so there is no durable
				// round to release and no plane open-MUST set to judge — the block
				// is this shape's record. Read the task back anyway, for the status
				// the hand-over log needs: a branchless verifier that recorded its
				// pass but left the task in_progress must be named loudly, exactly
				// as the id-based shape names it. A failed read ends the sequence
				// fail-closed, like every other unreadable seam — so it retries
				// like them too (CLA-686 D3).
				r.end.verifyRound = 0
				if st, read := d.readTaskStateRetry(ctx, t, res.Claim.TaskID, "after the branchless verification round"); !read {
					evidenceOK = false
					evidenceWhy = "the task could not be read after the branchless verification round, so its hand-over cannot be judged"
				} else {
					r.end.verifyStatus = st.Status
				}
			}
			if !evidenceOK {
				r.end.verifyFailure = evidenceWhy
			}
			// Fold the verified branch/tip onto the claim exactly as the generic
			// checkpoint path below does, so a fix spawned after an open-MUST
			// round resumes with the verified hand-off.
			if r.end.claim != nil && evidenceOK {
				if r.end.branch != "" {
					r.end.claim.Claim.Branch = r.end.branch
				} else {
					r.end.claim.Claim.Branch = ""
				}
				r.end.claim.CheckpointUnverified = evidenceUnverified
				r.end.claim.CheckpointTip = evidenceTip
			}
		} else if reviewable {
			r.end.branch, evidenceTip, evidenceOK, evidenceUnverified, evidenceWhy = d.checkpointEvidence(ctx, t, res, inv.WorkDir, &r.review)
			// The matched round rides the end (CLA-637): drainPhases closes it if
			// the handoff guards refuse the respawn this detected handoff asked
			// for — the one advance path drainPhase cannot close on itself. Only a
			// still-sealed round is ours to close: a verifier closes its own in the
			// terminal call (CLA-638).
			if r.review.state == "sealed" {
				r.end.reviewRound = r.review.round
			}
			// CLA-637: the review phase's durable record is SEALED — the plane
			// withholds its findings from every reader until the driver closes
			// the round. The close is the driver's, made when the review stage
			// is OVER: not while the session hands off to a successor reviewer
			// (the successor continues this round), and not while the next
			// configured phase is another review-shaped one (a panel member,
			// whose independence depends on not reading the earlier member's
			// findings). A close that fails REFUSES the checkpoint, fail-closed:
			// the alternative is a fix phase spawned against findings the plane
			// still has sealed, which can only park the task.
			//
			// CLA-639: for a panel, the stage is the whole panel. Every member
			// but the last leaves the round sealed here; the last member's close
			// is what releases them all to the fix phase, and it happens in the
			// SAME condition below because nextReviewShaped is false only for
			// the last member (the panel's last phase, when no fix follows).
			// CLA-784: an OPTIONAL seat's record alone must not release the
			// round. If no required member has recorded, this trial seat's
			// record is kept sealed — the panel is treated as one where nobody
			// recorded, so no fix runs against a round that is no review.
			optionalOnly := panelOptional && !panel.recorded
			if evidenceOK && r.review.required && r.review.round > 0 && r.review.state == "sealed" && handoff == "" && !nextReviewShaped && !opts.panelParallel && !optionalOnly {
				if cerr := d.closeReviewRound(ctx, t, res, r.review.round); cerr != nil {
					evidenceOK = false
					r.end.reviewCloseFailed = true
					evidenceWhy = fmt.Sprintf("review round %d could not be closed (%v) — refusing the checkpoint, because the fix phase would be spawned against findings the plane still has sealed", r.review.round, cerr)
				}
			}
		}
		// A held-but-empty exit (2026-08-24): a would-be checkpoint refused by the
		// evidence gate — not by any of the older vetoes (dead, untrusted, not
		// checkpointable) — so this is a FAILED phase, not a completed one. It is
		// carried on the result in BOTH branches on purpose: for a bare exit it is
		// the classification of record, and for a handoff it is what lets
		// drainPhases downgrade the exit when the handoff is refused. An honored
		// handoff never reaches that downgrade — the phase continues first.
		//
		// CLA-639: a panel member is not special-cased here. A member whose
		// review record did not pass the gate is a FAILED member, and the panel
		// handler in drainPhases is what records it as failed and keeps the round
		// going; the flag is shared with the shipped single-review path, which
		// still refuses to advance on it.
		empty := reviewable && !evidenceOK
		r.end.empty = empty
		r.end.emptyWhy = evidenceWhy
		// CLA-639: whether this pass left a review RECORD behind, for the panel's
		// member handler. It is the seam gate's verdict, not the round number: a
		// member whose envelope was sealed and closed still recorded.
		r.end.reviewRecorded = reviewable && r.review.required && evidenceOK
		// CLA-703: did this review-only session ever reach the backlog? A clean
		// end with no successful clankerbar call and no review record is the
		// missing-MCP shape — the tools the brief tells it to use were not
		// offered, so "recorded nothing" is not a reviewer's verdict.
		//
		// The dead signature is EXCLUDED: a silent death is already classified
		// as dead and retried by the dead-phase ladder, and relabelling it no_mcp
		// here would hide a real death behind an infrastructure excuse. A
		// non-zero exit is excluded for the same reason — the transient/
		// non-retryable classification owns it. Caps and untrusted streams are
		// excluded because neither can be read as "the tools were missing".
		noMCPNow := reviewOnly && !r.end.reviewRecorded && !r.end.reviewCloseFailed && res.ClankerbarCalls == 0 &&
			!res.Claim.Settled && !emittedHandoff &&
			res.Untrusted == "" && res.ExitCode == 0 && !capped && !ceiling && !wallclock &&
			!producedNothing && a.Capabilities().TracksClaims
		retryNoMCP := noMCPNow && !r.mcpRetried
		if noMCPNow && r.mcpRetried {
			// The retry ended the same way: this is the final classification.
			r.end.noMCP = true
			// The generic dead booking (if the retry happened to look like a
			// death) and the held-but-empty booking below both belong to dead/
			// empty, not to an infrastructure miss. Unbook the first and stop the
			// second from firing; the handler books the no-MCP cell instead.
			if bookedDead {
				d.unbookDead(ph.Label(phaseIdx), tc.HarnessFor(ph))
			}
			r.end.deadBooked = true
			if r.end.session != nil {
				r.end.session.Cause = fleet.CauseNoMCP
			}
		}
		// A handoff holds the seam OPEN even without evidence: its successor
		// continues THIS phase's job, so it is not the false-premise advance the
		// gate exists to stop. The evidence gate governs only the bare seam
		// advance to the NEXT configured phase (handoff == ""); a refused handoff
		// is downgraded to that bare judgement in drainPhases.
		//
		// CLA-638: a verification round that left an OPEN MUST holds the seam
		// even though it is the last phase, because the driver spawns another
		// fix against it. A clean round does not hold.
		verifyNeedsHold := isVerify && r.end.verifyChecked && r.end.verifyFailure == "" && len(r.end.verifyOpenMusts) > 0
		if ((!last || panelLast || handoff != "") && canCheckpoint && (handoff != "" || evidenceOK)) ||
			(verifyNeedsHold && canCheckpoint) {
			r.end.checkpoint = true
			r.end.handoff = handoff
			if r.end.claim != nil {
				// The invariant "the carried claim's branch is the VERIFIED one"
				// is held by construction, whatever form the evidence took
				// (CLA-497): the branch form folds the verified branch on, and
				// the plane-record forms clear whatever the session recorded -
				// a recorded branch that FAILED origin verification must not
				// ride into the successor's brief, which names {{branch}} as
				// verified on the origin remote (CLA-457), and the no-code
				// brief is selected exactly on an empty claim branch.
				//
				// CLA-576: an UNVERIFIED checkpoint is the one exception, and it
				// is marked rather than hidden. Its branch rides the claim (the
				// review phase needs the name to verify it) with
				// CheckpointUnverified set, so the successor's brief is the
				// unverified variant - never the verified wording, which this
				// checkpoint cannot support.
				if r.end.branch != "" {
					r.end.claim.Claim.Branch = r.end.branch
				} else {
					r.end.claim.Claim.Branch = ""
				}
				r.end.claim.CheckpointUnverified = evidenceUnverified
				// CLA-637: the origin tip the verified branch check read. The
				// review brief hands it to the reviewer as the revision to
				// check out and record, and the seam matches the record's
				// reviewedTip against it. Empty exactly when no tip could be
				// read (branchless or unverified), which is when no tip can be
				// demanded of the reviewer either.
				r.end.claim.CheckpointTip = evidenceTip
				// CLA-685: a HANDOFF is the one checkpoint this session's own
				// exit did not earn — it holds only because the successor
				// continues THIS phase, which the claim carried in as `prev`
				// opened. So when the exit evidenced NOTHING (evidenceOK false)
				// the verdict just folded above is its REFUSAL's, not a
				// checkpoint's: the record gate zeroes the unverified mark even
				// though the branch leg had established it. The successor then
				// reads carried.CheckpointUnverified as false, is handed the
				// VERIFIED continuation - "The driver verified the origin
				// remote's tip ... as {{reviewedTip}}" - for a tip this driver
				// never read, and the placeholder stays literal, since it is
				// filled only when the tip is non-empty.
				//
				// The exit's verdict about the checkpoint is empty exactly when
				// it read no tip of its own: an evidenced branch leg always
				// brings a tip (the verified form), and a non-empty tip implies
				// unverified=false — the verified continuation, whose
				// {{reviewedTip}} that tip then fills. So the opening
				// checkpoint's mark is the only true one there, and it rides
				// forward, as the panel-member block in drainPhases rides a
				// claim's resumption facts onto the claim it hands on.
				//
				// The tip is carried only for the branch it was read for — it
				// is `prev`'s, and a claim that now names another one must not
				// be handed a tip that never belonged to it. The BRANCH stays as
				// folded: it is the live truth about where the work is. An exit
				// that WAS evidenced keeps its own verdict untouched, so the
				// verified path is unaffected.
				if handoff != "" && !evidenceOK && prev != nil && r.end.claim.CheckpointTip == "" {
					r.end.claim.CheckpointUnverified = prev.CheckpointUnverified
					if r.end.claim.Claim.Branch == prev.Claim.Branch {
						r.end.claim.CheckpointTip = prev.CheckpointTip
					}
				}
			}
			if handoff != "" {
				log.Printf("%siteration %d: the session ended its final message with a handoff block, still holding %s — keeping the lease for its successor",
					labelOf(t), drainNum, res.Claim.TaskID)
			} else if r.end.branch != "" && evidenceUnverified {
				// The cause is the CHECK not completing, named by evidenceWhy
				// (the Unknown check's own detail) — never "the origin remote
				// could not be read" asserted as fact: delivery.Unknown also
				// covers a check that never reached the remote (a branch no
				// local repository has, an ambiguous repo, no git on PATH), and
				// the log must not contradict the detail it prints.
				log.Printf("%siteration %d: phase reached an UNVERIFIED checkpoint holding %s (branch %s recorded, but it could not be verified on the origin remote: %s) — keeping the lease for the next phase; the review phase must verify the branch itself",
					labelOf(t), drainNum, res.Claim.TaskID, r.end.branch, evidenceWhy)
			} else if r.end.branch != "" {
				log.Printf("%siteration %d: phase reached its checkpoint holding %s (branch %s verified on the origin remote) — keeping the lease for the next phase",
					labelOf(t), drainNum, res.Claim.TaskID, r.end.branch)
			} else {
				// The plane-record forms of exit evidence (CLA-497): a no-code or
				// already-settled task checkpoints with no branch to name.
				log.Printf("%siteration %d: phase reached its checkpoint holding %s (exit evidenced by the plane's record — task left ready or declared a no-code delivery; no branch) — keeping the lease for the next phase",
					labelOf(t), drainNum, res.Claim.TaskID)
			}
		} else {
			// `released` records only a handback that could actually have happened:
			// a zero Result from a failed launch releases nothing, and reporting it
			// as released would erase a predecessor's claim that is still live.
			//
			// CLA-639 M4: a PANEL MEMBER that ended without a record does NOT hand
			// its claim back here. Everything the panel handler must do about a
			// failed member needs the claim to still be live: closing the round (only
			// the LIVE holder may, and a failed close would leave the earlier
			// members' findings sealed behind a lease that is already gone) and
			// handing the next member — or the fix phase — the same run to resume.
			// A member that genuinely lost the claim is not special-cased: Held() is
			// false, so there is nothing to release either way. The handler owns the
			// handback for this shape, and takes it AFTER the close (see
			// drainPhases).
			// CLA-703: a retrying no-MCP attempt is not an ending — keep the claim
			// (the retry resumes the same run) and book nothing yet. The final
			// attempt goes through the normal handback.
			if !panelMember && !retryNoMCP {
				r.end.released = res.Claim.Held()
			}
			r.end.dead = dead
			if r.end.empty && !r.end.noMCP && !retryNoMCP {
				log.Printf("%siteration %d: the %s phase ended holding %s with no recorded branch on the origin remote — not a checkpoint (%s), so no review phase spawns",
					labelOf(t), drainNum, ph.Label(phaseIdx), claimLabel(res.Claim), evidenceWhy)
				// Book it into the dead-phase tally (CLA-457): this is the
				// "produced nothing" class the rate exists to measure, and a
				// fleet-wide silent EOT is exactly the incident in question.
				//
				// Unless the session's exit was already booked as the CLA-402
				// SILENT death: a LAST phase can wear both shapes at once (its
				// finish reason is "unknown" AND its evidence gate failed), and
				// one session may not read as two dead ones out of one run —
				// CLA-639 M4 needs this booking to be exactly one per failed
				// panel member, and the operator's rate needs it to be honest.
				//
				// And unless this is a panel member whose review close failed
				// (CLA-639 fix pass 3): a member that WROTE its record and whose
				// round the plane then refused to release produced a review — the
				// empty shape here would report it as a session that died
				// producing nothing, which is the opposite of what happened. The
				// skip is PANEL-ONLY: the plain single review keeps the untaxed
				// CLA-457 booking it has had since the base commit, so an
				// operator's dead-phase rate reads the same with no panel
				// (CLA-639 fix pass 4).
				if !bookedDead && !(panelMember && r.end.reviewCloseFailed) {
					d.tallyEmpty(ph.Label(phaseIdx), tc.HarnessFor(ph))
					// CLA-639 M4: this exit is booked, so the panel's member
					// handler must not book it again when it records the member
					// as failed.
					r.end.deadBooked = true
				}
			}
			// last && a clean exit is the shape CLA-384 is about: the phase ran to
			// completion, pushed, and simply did not declare. A non-zero exit reached
			// the same place by crashing, which is a different failure with its own
			// reporting, and an earlier phase reaching here did not have declaring as
			// its job at all.
			//
			// The return tells whether the claim is actually back in the ready queue
			// — the thing a bounded retry can re-claim. A release the plane refused
			// (or that never ran) leaves the lease live, so a retry session would
			// only be refused and burned.
			if !panelMember && !retryNoMCP {
				// A tool-loop kill is never the undeclared hand-off CLA-384
				// counts, even when the process exited 0 an instant before the
				// kill landed: it was killed failing to move the task on.
				r.end.releasedToQueue = d.releaseHeldClaim(ctx, t, res, last && res.ExitCode == 0 && toolLoop == "")
			}

			// CLA-451: the blind-spot convergence. Everything above learns about
			// claims by parsing clankerbar tool calls out of the stream; a session
			// that claimed by another channel (a raw API call with the same key,
			// a harness quirk, a renamed tool) ends checkpointable with NO observed
			// claim — and used to end the sequence here, stranding its pushed work
			// behind an expiring lease until a takeover recovered it at a reclaim's
			// cost. Before declaring the phase empty, ask the plane about the one
			// task this driver can name: the task it BRIEFED. If that task is held,
			// with a branch recorded, by some run of this project's key, treat it
			// as the phase's checkpoint exactly as an observed claim would be.
			//
			// Every conjunct below keeps an existing guard absolute:
			//
			//   - end.claim == nil && !res.Claim.Held() — an OBSERVED claim,
			//     settled or not, stays the authoritative view; the peek never
			//     overwrites or second-guesses one.
			//   - res.Untrusted == "" — a stream that could not be read whole is
			//     never read for claim state, and the plane is not asked to fill
			//     the hole either: the settle we never saw may be in the missing
			//     bytes (CLA-262).
			//   - !producedNothing — the dead-phase veto (CLA-386): a final step
			//     that finished "unknown" having produced nothing is not a
			//     checkpoint no matter what the plane says, so it is not even
			//     consulted.
			//   - the orderly-end disjunct mirrors `checkpointable` exactly, with
			//     the plane's recorded branch standing in for the HasWIP the cap
			//     path needs — a branch the PLANE confirms is stronger evidence
			//     than one read off a stream.
			//   - !last — a last phase has no successor to hold the lease for;
			//     a handoff cannot widen this either, because detectHandoff
			//     refuses on no observed claim before we get here.
			//
			// A claim the peek recovers then faces the SAME evidence gate as an
			// observed one: the recorded branch must really be on the origin
			// remote before the seam advances (CLA-457), because a plane record
			// alone cannot tell a pushed branch from a phantom one. Evidence that
			// fails keeps today's behavior — the drain ends, and the live lease
			// expires with its recorded branch intact as the takeover hand-off.
			// The peek itself is a READ of the briefed task (get_task), never a
			// claim and never a heartbeat: it cannot race a session out of work,
			// and a peek that fails or comes back not-held simply falls through
			// to the standard ending below.
			if r.end.claim == nil && !last && res.Untrusted == "" && !producedNothing &&
				!res.Claim.Held() && (res.ExitCode == 0 || capped || ceiling || wallclock || recoveredExit) {
				if claim := d.peekCheckpointClaim(ctx, t, drainNum, ph.Label(phaseIdx), briefedTask); claim != nil {
					r.end.claim = claim
					var evidenceTip string
					r.end.branch, evidenceTip, evidenceOK, evidenceUnverified, evidenceWhy = d.checkpointEvidence(ctx, t, *claim, inv.WorkDir, &r.review)
					r.end.reviewRound = r.review.round
					// The review round's close rides this path too (CLA-637): a
					// recovered review checkpoint whose round stayed sealed would
					// spawn the fix phase against findings it cannot read. No
					// handoff is possible here (detectHandoff refuses an
					// unobserved claim), so the only skip is a following
					// review-shaped phase.
					//
					// CLA-784: the same "an optional seat's record alone must not
					// release the round" rule as the observed-claim close above.
					// A trial seat that records here and is then recovered through
					// the plane's peek must leave the round sealed unless a
					// REQUIRED member has recorded; otherwise this close releases
					// a round that is no review at all, before the panel handler's
					// own "no required member recorded" check can see it.
					optionalOnly := panelOptional && !panel.recorded
					if evidenceOK && r.review.required && r.review.round > 0 && r.review.state == "sealed" && !nextReviewShaped && !opts.panelParallel && !optionalOnly {
						if cerr := d.closeReviewRound(ctx, t, *claim, r.review.round); cerr != nil {
							evidenceOK = false
							evidenceWhy = fmt.Sprintf("review round %d could not be closed (%v) — refusing the checkpoint, because the fix phase would be spawned against findings the plane still has sealed", r.review.round, cerr)
						}
					}
					if evidenceOK {
						// Fold the VERIFIED branch onto the carried claim, exactly as
						// the observed-claim checkpoint above does: a recovered claim
						// carries the plane's recorded branch and checkpointEvidence
						// re-verified it, so the two already agree today — but the
						// invariant "the carried claim's branch is the verified one"
						// is what verifiedBranch and the takeover hand-off depend on,
						// so hold it by construction rather than by coincidence of a
						// single-source read. CLA-576: an unverified recovery rides
						// the same way, marked so its brief is the unverified variant.
						r.end.claim.Claim.Branch = r.end.branch
						r.end.claim.CheckpointUnverified = evidenceUnverified
						r.end.claim.CheckpointTip = evidenceTip
						r.end.checkpoint = true
						if evidenceUnverified {
							log.Printf("%siteration %d: %s ended with no claim observed in its stream, but the plane holds %s (run %s) with branch %s recorded — it could not be verified on the origin remote (%s), so this is an UNVERIFIED checkpoint: keeping the lease for the next phase; the review phase must verify the branch itself",
								labelOf(t), drainNum, ph.Label(phaseIdx), claim.Claim.TaskID, claim.Claim.RunID, r.end.branch, evidenceWhy)
						} else {
							log.Printf("%siteration %d: %s ended with no claim observed in its stream, but the plane holds %s (run %s) with branch %s verified on the origin remote — treating it as the phase checkpoint and keeping the lease for the next phase",
								labelOf(t), drainNum, ph.Label(phaseIdx), claim.Claim.TaskID, claim.Claim.RunID, r.end.branch)
						}
					} else {
						log.Printf("%siteration %d: the plane holds %s (run %s) with branch %q recorded, but that branch could not be verified on the origin remote (%s) — not treating it as a checkpoint; the lease expires with its hand-off intact",
							labelOf(t), drainNum, claim.Claim.TaskID, claim.Claim.RunID, r.end.branch, evidenceWhy)
					}
				}
			}
		}
		// CLA-703: a no-MCP end is an infrastructure miss, not a dead or empty
		// phase. It must not reach the dead-phase ladder (which would retry/park
		// a task whose reviewer never got its tools) or the empty-retry arm.
		// The class is carried on end.noMCP for the panel handler and the
		// drain-level booking.
		if r.end.noMCP {
			r.end.dead = false
			r.end.producedNothing = false
			r.end.empty = false
		}
		// Then check what it said it delivered. After the handback, because a dead
		// lease is time-sensitive and a git check is not; on every exit, because a
		// session that pushed nothing and died is exactly as likely to have recorded
		// a branch as one that finished cleanly.
		d.verifyDeliveries(ctx, t, res, inv.WorkDir)

		if ierr != nil {
			if ctx.Err() != nil {
				return r.tokens, r.cost, true, r.end, nil
			}
			// Count this attempt's spend before giving up on it. A non-exit run
			// failure is either a launch failure (nothing emitted, nothing parsed —
			// adding zeroes changes nothing) or a session that ran, announced its
			// spend and died in Wait (CLA-299): the Result was parsed precisely so
			// the budget would not lose what the attempt actually cost. That holds
			// only while the stream was read whole, which is what the guard below
			// makes binding: a failure whose stream ALSO came back untrusted
			// carries a floor, not a total, and gets the same refusal as the
			// untrusted branch below and endUntrustedDrain (CLA-262) - counted
			// nowhere, so no breaker ever acts on a figure that cannot be known.
			if res.Untrusted == "" {
				r.tokens += res.Tokens
				r.cost += res.CostUSD
				r.publishSpend()
				recordPassSpend(r.end.session, a, &r.pass, &res)
				d.charge(tc.HarnessFor(ph), res.Tokens, res.CostUSD)
			}
			// CLA-703: an MCP preflight failure is infrastructure, not a verdict
			// on the phase. The session never reached its first turn, so the
			// claim it was seeded with is untouched and the same run can be
			// retried. One retry — the same bound the no-record backstop uses —
			// then the run fails loudly with the reason rather than producing a
			// silent empty phase.
			if errors.Is(ierr, harness.ErrMCPNotConnected) {
				if !r.mcpRetried {
					r.mcpRetried = true
					log.Printf("%siteration %d: %s could not start its first turn: %v — retrying the %s phase once (MCP preflight failure, not a phase verdict)",
						labelOf(t), drainNum, a.Name(), ierr, ph.Label(phaseIdx))
					continue
				}
				return r.tokens, r.cost, false, r.end, fmt.Errorf("iteration %d: %w", drainNum, ierr)
			}
			// A run failure that was not an exit status — a launch failure, or one
			// of the deaths in Wait above — ends the attempt without any retry
			// classification. Not a blip.
			return r.tokens, r.cost, false, r.end, fmt.Errorf("invoke %s: %w (log: %s)", a.Name(), ierr, logPath)
		}

		// A session whose stream could not be read whole (CLA-262). Everything below
		// this line reads a figure parsed out of that stream, so counting the spend,
		// classifying the exit or retrying on the strength of it are three ways to
		// make a confident decision on data with a hole in it.
		if res.Untrusted != "" {
			// held is nil here by construction — an untrusted stream never holds a
			// phase open — so the sequence ends and nothing is carried forward.
			//
			// CLA-691: no spend figures were recorded on this pass's session entry
			// either (recordPassSpend runs only where the loop counts an attempt),
			// so the record says "unknown" instead of publishing a FLOOR from a
			// stream with a hole in it as if it were a report.
			utokens, ucost, ustop, uerr := d.endUntrustedDrain(t, drainNum, tc.HarnessFor(ph), logPath, res, r.tokens, r.cost)
			return utokens, ucost, ustop, r.end, uerr
		}

		// Count THIS attempt's spend toward the budget breaker regardless of how it
		// ends — usage-limit, transient, stop, or clean. A failed/retried attempt
		// still burned tokens, and a "leave headroom" breaker must err toward seeing
		// real spend, not under-counting it. Each attempt is a distinct session, so
		// summing per attempt (and returning the accumulator, not the final res)
		// counts every session exactly once.
		r.tokens += res.Tokens
		r.cost += res.CostUSD
		r.publishSpend()
		// The pass's own session entry takes the same figures, in the same breath
		// (CLA-691): every attempt the loop counts is money the record attributes
		// to the pass, and none it refuses is.
		recordPassSpend(r.end.session, a, &r.pass, &res)
		d.charge(tc.HarnessFor(ph), res.Tokens, res.CostUSD)

		// CLA-703 backstop: the primary fix is the adapter's MCP preflight, but
		// a session can still end with no clankerbar call and no record (an
		// older build, a session that acted before its tools were offered, a
		// harness whose preflight does not exist). Re-run it ONCE with the same
		// brief before the panel records the member as failed. The claim was
		// deliberately kept above, so a resumed phase retries the same run; the
		// spend is already counted, so the retry is visible to the budget.
		if retryNoMCP {
			r.mcpRetried = true
			log.Printf("%siteration %d: the %s phase made no successful clankerbar MCP call and recorded no review record — retrying it once (no_mcp: the backlog tools were not available to the session)",
				labelOf(t), drainNum, ph.Label(phaseIdx))
			continue
		}

		// A usage limit. A rolling-window subscription cap is waited out and the
		// session re-run; a hard budget/credit exhaustion (Stop) has no reset to
		// poll for, so the run stops cleanly and the operator resumes it once
		// they've topped up.
		if lim := a.DetectLimit(res); lim.Limited {
			if lim.Stop {
				// CLA-784: an OPTIONAL panel seat is a trial reviewer. A hard
				// stop on its account is recorded against that seat — never the
				// run. Today's stop path would end the daemon for a reviewer we
				// were only evaluating, throwing away the required members' round
				// with it. The optional member's failure rides the usual
				// per-member bookkeeping: named INCOMPLETE with this reason, and
				// the panel moves on.
				if panelOptional {
					log.Printf("%siteration %d: the %s member is an OPTIONAL trial seat — its usage stop is recorded against that seat, not the run (log: %s)",
						labelOf(t), drainNum, ph.Label(phaseIdx), logPath)
					r.end.empty = true
					r.end.emptyWhy = "stopped on a usage limit (no reset to wait for) before recording"
					return r.tokens, r.cost, false, r.end, nil
				}
				log.Printf("iteration %d stopped: %s — no reset to wait for, stopping (resume once resolved) (log: %s)",
					drainNum, limitReason(lim), logPath)
				d.writeLimitStop(lim)
				return r.tokens, r.cost, true, r.end, nil
			}
			log.Printf("iteration %d hit a usage limit: %s", drainNum, limitReason(lim))
			// CLA-784: an OPTIONAL seat never waits and never ends the sequence,
			// resumed or not. The sequence-end signal is the member handler's
			// reason; for a trial seat it is inert there (no run-level wait, no
			// shared-claim handback).
			if panelOptional {
				log.Printf("%siteration %d: the OPTIONAL %s seat hit a usage limit (%s) — recording it against that seat and carrying on rather than waiting out a reset on a trial reviewer (log: %s)",
					labelOf(t), drainNum, ph.Label(phaseIdx), limitReason(lim), logPath)
				r.end.sequenceEnd = &phaseSequenceEnd{limitWait: &phaseLimitWait{lim: lim, ph: ph, a: a}}
				return r.tokens, r.cost, false, r.end, nil
			}
			// A RESUMED phase must not wait one out (CLA-631). The seam's handback
			// has already ended the claim: `release` puts a no-work claim straight
			// back in the queue, and `expireLease` ends a pushed-work claim's lease
			// immediately, leaving the task an immediate takeover — so a supervised
			// wait of hours would only run against a task this sequence no longer
			// holds, with a runId in its prompt that no longer means anything. End
			// the sequence instead. The RUN, though, must not end with it — exiting
			// the daemon left a whole fleet idle until an operator noticed — so the
			// wait is handed to drainPhases, which owes it only once the sequence's
			// handback has run and no lease is held.
			if prev != nil {
				log.Printf("iteration %d: a resumed phase hit a usage limit — ending the sequence rather than waiting out a reset on a task the handback has already returned; it comes back as an immediate takeover, and the run stays up to wait for the reset instead of exiting (log: %s)",
					drainNum, logPath)
				r.end.sequenceEnd = &phaseSequenceEnd{limitWait: &phaseLimitWait{lim: lim, ph: ph, a: a}}
				return r.tokens, r.cost, false, r.end, nil
			}
			// Whatever this attempt held open is not held open any more: every path
			// below either waits or retries with a FRESH session, which re-claims.
			// Pre-phases the handback ran on every attempt, so this could not arise.
			r.end = d.releaseCarriedEnd(ctx, t, r.end)
			// Waiting out a reset that lands past the wall-clock ceiling buys
			// nothing: the budget check runs between drains, so the loop would
			// sleep through the window, run one session on the freshly reset quota
			// and stop on the very next check — having spent the night waiting for
			// headroom it then declines to use. Stop now and say when the quota
			// returns, so the operator can start a fresh run against it.
			remaining, bounded := tc.Budget.Remaining(time.Since(prior.start))
			if until, over := waitPastBudget(lim.ResetAt, remaining, bounded); over {
				log.Printf("iteration %d: the limit resets %s, in %s — more than the %s left of this run's ceiling; stopping now rather than waiting, so start a fresh run after the reset (log: %s)",
					drainNum, formatReset(until), time.Until(until).Round(time.Minute), remaining.Round(time.Minute), logPath)
				return r.tokens, r.cost, true, r.end, nil
			}
			// The wait's own probe spend lands in the SAME accumulator as the
			// sessions', so it reaches the breaker at the top of this loop, the one in
			// Run between drains, and the iteration's cost line — rather than a
			// separate figure nothing is measured against (CLA-287).
			// Probed on the harness that hit the limit, in that harness's own
			// session shape: a usage limit is a property of one provider's account,
			// so probing the run's default harness would answer a question nobody
			// asked, and spend on it. The panel AGGREGATE rides `sofar` too (see
			// above): a sibling's counted spend counts against the same ceiling
			// this wait's probes are about to spend under.
			waitTokens, waitCost := r.sofar()
			ptokens, pcost, pstop := d.supervisedWait(ctx, lim, a, ph, t, spend{
				start:  prior.start,
				tokens: waitTokens,
				cost:   waitCost,
			})
			r.tokens += ptokens
			r.cost += pcost
			r.publishSpend()
			if pstop {
				return r.tokens, r.cost, true, r.end, nil
			}
			continue
		}

		// Below the limit branch, so a usage-limit attempt is counted NEITHER way.
		// A session the subscription cap turned away often reports nothing at all
		// - under claude the notice can arrive on stderr with no stream, and under
		// codex as free text - so counting it as silence would end the run on the
		// ordinary overnight quota wait, and blame a harness that is starting
		// perfectly well. It is not a reset either: the cap is a known cause with
		// its own breakers (the budget check inside supervisedWait, waitPastBudget,
		// and the probes' own spend), and clearing the count on it would let a
		// limit between silences hide the loop this bounds.
		//
		// Everything from here down is an attempt that RAN and ended on its own
		// terms, which is where "did it tell us anything?" is the right question -
		// a different question from what it told us, since a report of zero is
		// still a report (CLA-288). Counted here, enforced at the top of the loop,
		// so the bound fires on the decision to spawn again rather than on this
		// attempt's exit.
		if res.UsageReported {
			r.noUsage = 0
		} else {
			r.noUsage++
		}

		// CLA-739: killed for repeating a failing tool call. Ahead of the exit
		// status, because the session can exit 0 in the instant before the kill
		// lands. Never retried here as a transient (a fresh session would loop
		// the same way, and each call re-sends the whole context), and never a
		// run failure (the kill did its job). The dead classification above has
		// already decided what follows: drainPhases' dead-phase ladder, or the
		// panel's failed-member path.
		if toolLoop != "" {
			// claude reports dollars only on its terminal result event, which a
			// killed stream never reaches - so say the cost was not captured
			// rather than print $0 as if it were measured (the CLA-343 ceiling
			// branch's rule). opencode sums each step's cost up to the kill.
			spent := fmt.Sprintf("tokens=%d cost=$%.4f", r.tokens, r.cost)
			if !res.CostReported {
				spent = fmt.Sprintf("tokens=%d; cost not captured - killed mid-stream", r.tokens)
			}
			log.Printf("%siteration %d: the %s phase was killed by the driver: %s — classified dead, no delivery (%s) (log: %s)",
				labelOf(t), drainNum, ph.Label(phaseIdx), toolLoop, spent, logPath)
			return r.tokens, r.cost, false, r.end, nil
		}

		if res.ExitCode == 0 {
			// A clean exit is the shape the CLA-398 quiet death wears, so the
			// marker gets named here rather than inferred downstream: a final
			// step_finish with reason "unknown" and all-zero usage used to read
			// as "iteration done (tokens=0 cost=$0.00)" — indistinguishable from
			// a cheap clean run, which is exactly what made these deaths
			// invisible. The adapter's own marker is the only thing that can
			// tell them apart.
			if zeroUsage {
				log.Printf("iteration %d: the session ended with the zero-usage-unknown signature (final step reason %q, all-zero usage) — a quiet death, not a clean run (log: %s)",
					drainNum, harness.FinishReasonUnknown, logPath)
			} else if finished := harness.RouteFinished(res); finished != "" && finished != inv.Model {
				// CLA-782: the spawn line names the route the session STARTED on;
				// this line adds the route that finished it when a usage limit moved
				// it mid-session.
				log.Printf("iteration %d done (tokens=%d cost=$%.4f; finished on %s) (log: %s)", drainNum, r.tokens, r.cost, finished, logPath)
			} else {
				log.Printf("iteration %d done (tokens=%d cost=$%.4f) (log: %s)", drainNum, r.tokens, r.cost, logPath)
			}
			return r.tokens, r.cost, false, r.end, nil
		}

		// CLA-700: a session that recovered from its provider errors is not judged
		// by its exit status — the stream shows the harness took the error, retried
		// and carried the session on, and 2.x exits non-zero regardless (pinned by
		// the conformance suite). Everything this exit could have meant was settled
		// above by the DELIVERY question: either the phase checkpoints (its work is
		// verified on the origin remote, or its review record appended) and the
		// sequence advances, or it delivered nothing and ends the drain as the
		// held-but-empty shape. Falling through to the classifiers here would undo
		// both — the transient ladder retries a session whose work is already
		// pushed, and the non-retryable stop ends the run over an exit status that
		// names no failure.
		//
		// Deliberately HERE, after the untrusted and spend-accounting arms and
		// after DetectLimit: a usage limit or budget stop is still honoured (its
		// scan reads stderr and a TERMINAL error event, so a recovered blip cannot
		// trip it while a genuine account stop still can), and the cap branches
		// below are unreachable from here by construction — a capped exit is
		// `orderlyExit`, so it never sets recoveredExit.
		if recoveredExit {
			log.Printf("iteration %d: the session's stream shows %d provider error event(s) it recovered from and no terminal one (exit %s) — judging this phase on its delivery, not on the exit status (log: %s)",
				drainNum, harness.RecoveredErrors(res), res.ExitString(), logPath)
			return r.tokens, r.cost, false, r.end, nil
		}

		// A turn-capped end is this PHASE ending, never the run failing. It exits
		// non-zero and matches neither the limit scan nor the transient one, so
		// without this it would fall through to the non-retryable branch below and
		// stop the daemon — the backstop killing what it was added to protect.
		// Not retried either: the cap would fire again at the same place.
		if capped {
			log.Printf("iteration %d: the session hit its turn cap (tokens=%d cost=$%.4f) — ending this phase; anything uncommitted was salvaged above (log: %s)",
				drainNum, r.tokens, r.cost, logPath)
			return r.tokens, r.cost, false, r.end, nil
		}

		// A session the ADAPTER killed for crossing its per-session token ceiling
		// is the same shape as a turn cap: an orderly end with the salvage's
		// problem to handle, never a failure and never a retry. Retrying would
		// re-spend against the same runaway ceiling; failing would stop the run
		// over a kill that did its job (CLA-343).
		//
		// The cost is reported as unmeasured, never as a number: the result event
		// — the only carrier of total_cost_usd — is deliberately not parsed on a
		// killed stream, so res.CostUSD is 0 and printing it would under-report
		// the drain's spend as exactly nothing. The tokens are the honest figure.
		if ceiling {
			log.Printf("iteration %d: the session crossed its per-session token ceiling (tokens=%d; cost not captured — killed mid-stream) — ending this phase; anything uncommitted was salvaged above (log: %s)",
				drainNum, r.tokens, logPath)
			return r.tokens, r.cost, false, r.end, nil
		}

		// A session the ADAPTER ended for outliving its wall-clock cap is the same
		// shape again: the phase ends, and neither a retry nor a failure is right -
		// a retry would spend the same hours over again and reach the same deadline,
		// and failing would stop the run over a cap doing its job (CLA-368).
		//
		// Spend IS reported here, unlike the token-ceiling branch: opencode's usage
		// arrives per step_finish and is summed all the way to the kill, so the
		// figures are the honest cost of the session up to the moment it ended.
		//
		// Since CLA-365 opencode has BOTH HonoursSessionWallClock and
		// TracksClaims (Capabilities.TracksClaims: true), so a capped opencode
		// session holding its claim IS salvaged - the res.Claim.Held() arm above
		// is the one that fires today. The else branch (nothing salvaged) applies
		// only if an adapter enforcing this cap ever lacks claim observation
		// again; it is kept defensive, not descriptive of the present. The claim-held
		// wording was earned by CLA-365, not reserved for a future adapter.
		if wallclock {
			if res.Claim.Held() {
				log.Printf("iteration %d: the session outlived its wall-clock cap (tokens=%d cost=$%.4f) — ending this phase; anything uncommitted was salvaged above (log: %s)",
					drainNum, r.tokens, r.cost, logPath)
			} else {
				log.Printf("iteration %d: the session outlived its wall-clock cap (tokens=%d cost=$%.4f) — ending this phase. NOTHING was salvaged: this harness does not observe the session's task claim, so whatever the session left uncommitted is still in the worktree (log: %s)",
					drainNum, r.tokens, r.cost, logPath)
			}
			return r.tokens, r.cost, false, r.end, nil
		}

		// Non-zero exit, not the usage cap: a transient server/network blip backs
		// off and retries; anything else is a genuine failure and stops.
		if a.IsTransient(res) {
			// A RESUMED phase does not retry, for the same reason it does not wait
			// out a usage limit — and this path is the more dangerous of the two,
			// because MaxRetries defaults to 0, meaning never give up. Seeding the
			// resumed claim made the seam's handback live for phase 2, so a blip
			// here hands the task back mid-sequence at once: `release` for a claim
			// with nothing pushed, `expireLease` for pushed work, which ends that
			// claim's lease immediately and leaves the task an immediate takeover.
			// A retry would then be handed the very same heartbeat(runId) brief
			// against a run that no longer holds anything. End the sequence. The
			// RUN, though, must not end with it (CLA-651) — exiting the daemon on
			// one provider blip left the fleet idle until an operator noticed, the
			// same failure CLA-631 fixed for limits — so the sequence-end signal
			// goes to drainPhases, which owes it a wait (CLA-662): there is no
			// reset for a blip, so the run waits the target's growing backoff rung
			// after the handback and then polls again. The rung matters precisely
			// when this phase's harness is NOT the one the queue drains on: an
			// opencode implement start can never see the claude blip, so without
			// the run-level wait the same implement-and-lose loop would repeat with
			// nothing bounding it.
			// CLA-784: like a usage limit, an OPTIONAL seat's transient end is
			// its own result — never the sequence's, resumed or not.
			if panelOptional {
				log.Printf("%siteration %d: the OPTIONAL %s seat hit a transient failure (%s) — recording it against that seat and carrying on rather than retrying a task the panel is still working (log: %s)",
					labelOf(t), drainNum, ph.Label(phaseIdx), res.ExitString(), logPath)
				r.end.sequenceEnd = &phaseSequenceEnd{}
				return r.tokens, r.cost, false, r.end, nil
			}
			if prev != nil {
				log.Printf("iteration %d: a resumed phase hit a transient failure (%s) — ending the sequence rather than retrying a task the handback has already returned; it comes back as an immediate takeover, and the run stays up to poll again instead of exiting (log: %s)",
					drainNum, res.ExitString(), logPath)
				r.end.sequenceEnd = &phaseSequenceEnd{}
				return r.tokens, r.cost, false, r.end, nil
			}
			// A retry the adapter could not NAME is bounded on its own, because
			// nothing else bounds it: the dials are off by default and the
			// zero-spend counter is reset by the usage this heuristic requires. The
			// diagnostic goes in the error because it is the whole remedy - the
			// adapter has no pattern for this text, so an operator who can see it
			// can say whether one should be added (CLA-381).
			if a.IsUnclassifiedTransient(res) {
				r.unclassified++
				if r.unclassified > unclassifiedRetryBound {
					return r.tokens, r.cost, false, r.end, fmt.Errorf(
						"iteration %d: %w: %d retries on %s failures that matched no known-transient pattern - retried because the session had reported usage, but the same unrecognised failure keeps happening, so this is a real fault rather than a blip%s (log: %s)",
						drainNum, errUnclassifiedRetryLoop, r.unclassified-1, a.Name(), failureDetail(a.Diagnostic(res)), logPath)
				}
			}
			// As in the limit branch: the retry is a fresh session that re-claims,
			// so nothing may stay held open across it.
			r.end = d.releaseCarriedEnd(ctx, t, r.end)
			r.retries++
			if d.cfg.MaxRetries > 0 && r.retries > d.cfg.MaxRetries {
				return r.tokens, r.cost, false, r.end, fmt.Errorf(
					"iteration %d: transient failures persisted after %d retries (check https://status.claude.com; rerun to resume) (log: %s)",
					drainNum, d.cfg.MaxRetries, logPath)
			}
			wait := d.backoff(r.retries)
			log.Printf("iteration %d transient failure (%s) — %s in %s (a fresh session reclaims any half-done task) (log: %s)",
				drainNum, res.ExitString(), retryLabel(r.retries, d.cfg.MaxRetries), wait, logPath)
			if d.waitOrStop(ctx, wait) {
				return r.tokens, r.cost, true, r.end, nil
			}
			continue
		}

		// Stopping here ends the whole run, so say WHAT was judged non-retryable.
		// Without it the operator gets "exited 1 (non-retryable)" and no way to
		// tell a genuine failure from a blip the classifier merely does not know
		// yet — which is the one thing they need in order to report the gap. The
		// text is the harness's own diagnostic scope, never the raw stream, so
		// the agent's narration is not quoted back at them (CLA-258).
		return r.tokens, r.cost, false, r.end, fmt.Errorf("iteration %d: %s %s (non-retryable) — stopping%s%s (log: %s)",
			drainNum, a.Name(), res.ExitString(), failureDetail(a.Diagnostic(res)), droppedNote(res.OutputDropped), logPath)
	}
}
