package planner

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/backlog"
	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/fleet"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// turn executes one claimed conversation turn: resolve the role, claim it, run
// ONE session, record what the session ended as. The session answers the
// operator itself; the loop never writes the reply.
func (p *Planner) turn(ctx context.Context, i int, t Target, pending *backlog.PendingConversation) error {
	role := p.resolve(ctx, i, t)
	if !role.usable {
		// The poll named a turn, but the effective config no longer has a
		// planner (or cannot serve one here). Claim nothing: the turn waits for
		// a planner that can answer it, and an empty claim would be worse than
		// leaving it pending.
		return nil
	}
	if t.Conversations == nil {
		return plane.ErrNotWired
	}
	claim, err := t.Conversations.ClaimConversation(ctx, plane.ConversationClaimOptions{
		// The specific conversation the poll named, so two planners racing cannot
		// BOTH walk away with different rows while one turn is served twice.
		ConversationID: pending.ID,
		Instance:       p.instance,
		History:        "full",
	})
	if err != nil {
		return err
	}
	if claim == nil {
		// Lost the claim race (or the turn was answered between poll and claim).
		// The next poll names the next claimable conversation; nothing is lost.
		return nil
	}

	p.mu.Lock()
	p.turns++
	n := p.turns
	p.mu.Unlock()
	// TaskRef is deliberately empty: the console renders it as a task ref, and a
	// conversation UUID is not one. The conversation id is in the local turn log.
	// Model is the role's resolved model — the same one the session's invocation is
	// built with (CLA-1026), so a planner mid-turn shows its live route like a loop card.
	p.sendBeacon(i, t, fleet.State{Kind: fleet.StateIteration, N: n, Phase: "planner", Model: role.session.Model})

	// The wall clock bounds the WHOLE turn from the claim, not just the harness
	// invocation: resolve, the claim and the history pages spend the same budget,
	// and the plane derived the claim's lease from the role's max_wall_clock.
	// LeaseExpiresIn is the backstop when the operator raised the wall clock.
	turnCtx := ctx
	if budget := turnBudget(role.session.MaxWallClock, claim.LeaseExpiresIn); budget > 0 {
		var cancel context.CancelFunc
		turnCtx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}

	// A turn can run for the whole wall-clock bound, which is the plane's stale
	// window; refresh this instance's presence (all targets) while it runs. The
	// running target's row carries the role's resolved model, like the turn's first
	// beacon (CLA-1026).
	stopPresence := p.startPresence(i, n, role.session.Model)
	start := p.now()
	res, mode, err := p.runSession(turnCtx, t, role, claim)
	stopPresence()
	p.logTurn(t, role.session, claim, mode, res, err, p.now().Sub(start))
	// The turn is over, so the row must stop saying `iteration` now rather than
	// at the next throttled idle beacon.
	p.beaconIdleNow(i, t)
	return err
}

// claimReplyMargin is reserved out of the claim's lease so the session can post
// its reply before the plane refuses it. The plane's own grace over the role's
// wall clock is 120 s; this is a smaller reserve INSIDE the lease.
const claimReplyMargin = 30 * time.Second

// turnBudget picks the smaller of the role's wall clock and the claim's lease
// (less the reply margin). Zero means neither states a bound.
func turnBudget(maxWallClock time.Duration, leaseSeconds int) time.Duration {
	budget := maxWallClock
	if leaseSeconds > 0 {
		lease := time.Duration(leaseSeconds) * time.Second
		if lease > claimReplyMargin {
			lease -= claimReplyMargin
		} else {
			lease /= 2
		}
		if budget <= 0 || lease < budget {
			budget = lease
		}
	}
	return budget
}

// runSession decides resume-vs-prime, spawns the session, and recovers from a
// missing session WITHIN the same turn: the claim is still held, so re-priming
// is one fresh session from the plane's history, not a lost turn.
//
// mode is the word the turn log names: prime, resume or re-prime. It is decided
// HERE because only this function knows a resume became a re-prime.
func (p *Planner) runSession(ctx context.Context, t Target, role *projectRole, claim *plane.ConversationClaim) (harness.Result, string, error) {
	sess := role.session
	adapter, err := p.adapterFor(sess.Harness)
	if err != nil {
		return harness.Result{}, "", err
	}
	key := sessionKey(t.Name, claim.ID)

	decision := p.sessions.Decide(key, sess.Harness, claim.Resume, sess.MaxSessionTokens)
	// A harness with no session ids cannot keep one session across turns; it
	// primes from the plane's history every time. The plane will never see a ref
	// from this instance, so Decide's answer is moot and no handle is minted.
	sessionsSupported := adapter.Capabilities().ResumesSessions
	if !sessionsSupported {
		decision = SessionDecision{}
	}
	ref := decision.Ref
	// prime runs a fresh session from the bounded history and reports the handle
	// to record.
	prime := func() (harness.Result, string, error) {
		ref := ""
		if sessionsSupported {
			ref = p.newRef()
		}
		prompt := p.primePrompt(ctx, t, sess, claim, ref)
		res, err := p.invoke(ctx, adapter, t, role.config, sess, prompt, ref, false)
		return res, refFor(res, ref, err), err
	}

	if decision.Resume {
		res, err := p.invoke(ctx, adapter, t, role.config, sess, p.resumePrompt(sess, claim, ref), ref, true)
		if err == nil && res.SessionMissing {
			log.Printf("%splanner: session %s for conversation %s is gone on harness %s - re-priming from the plane's history",
				labelOf(t), ref, claim.ID, sess.Harness)
			res, ref, err = prime()
			p.sessions.Record(key, sess.Harness, ref, res.Tokens)
			return res, "re-prime", err
		}
		tokens := decision.Tokens + res.Tokens
		p.sessions.Record(key, sess.Harness, refFor(res, ref, err), tokens)
		return res, "resume", err
	}
	res, ref, err := prime()
	p.sessions.Record(key, sess.Harness, ref, res.Tokens)
	return res, "prime", err
}

// invoke spawns one process and returns its result. The console is the
// planner's own stderr: a chat turn's progress is worth watching live, and it
// lands in the same log as the loop's lines.
func (p *Planner) invoke(ctx context.Context, adapter harness.Adapter, t Target, eff *config.Config, sess config.PlannerSession, prompt, ref string, resume bool) (harness.Result, error) {
	inv, err := p.invocation(t, eff, sess, prompt, ref, resume)
	if err != nil {
		return harness.Result{}, err
	}
	// The turn's wall clock is the CLI's to enforce, not the adapter's: claude
	// honours no session time cap, and the plane derives the conversation-claim
	// lease from this same figure plus a grace window. turn() already carries the
	// claim-derived deadline, so this re-application is the smaller-bound
	// backstop for a direct caller; every adapter honours context cancellation.
	if sess.MaxWallClock > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, sess.MaxWallClock)
		defer cancel()
	}
	return adapter.Invoke(ctx, inv)
}

// invocation is the machine wiring a planner session starts with: the same
// overlays a drain session gets, but the model comes from the ROLE and the
// session handle is the conversation's.
func (p *Planner) invocation(t Target, eff *config.Config, sess config.PlannerSession, prompt, ref string, resume bool) (harness.Invocation, error) {
	workdir := t.WorkDir
	if workdir == "" {
		workdir = p.base.WorkDir
	}
	hc := eff.SessionFor(sess.Harness)
	env, err := p.base.SessionEnv(sess.Harness, t.Name)
	if err != nil {
		return harness.Invocation{}, err
	}
	idle, toolCall := eff.SessionIdleFor(sess.Harness)
	return harness.Invocation{
		Prompt:              prompt,
		Model:               sess.Model,
		WorkDir:             workdir,
		MCPConfigPath:       eff.ResolveMCPConfig(sess.Harness, t.MCPConfigPath, t.MCPConfigPaths),
		ConfigDir:           hc.ConfigDir,
		SettingsPath:        hc.SettingsPath,
		Env:                 env,
		EnvAllowlist:        p.base.SessionEnvAllowlist(sess.Harness, t.Name),
		Nice:                p.base.SessionNice,
		Console:             p.console,
		MaxTurns:            sess.MaxTurns,
		MaxSessionTokens:    sess.MaxSessionTokens,
		MaxSessionWallClock: sess.MaxWallClock,
		// The repeated-failure and idle watchdogs are machine backstops, not
		// policy: the same bounds a drain session gets, from the effective
		// config so a stored document can tighten them.
		MaxRepeatedToolFailures: eff.RepeatedToolFailureBound(),
		MaxSessionIdle:          idle,
		MaxToolCallDuration:     toolCall,
		SessionRef:              ref,
		ResumeSession:           resume,
	}, nil
}

// logTurn says one line for every turn: enough to correlate a conversation with
// the spend and the end, and never the reply text (that is the plane's).
func (p *Planner) logTurn(t Target, sess config.PlannerSession, claim *plane.ConversationClaim, mode string, res harness.Result, err error, elapsed time.Duration) {
	model := sess.Model
	if model == "" {
		model = "(harness default)"
	}
	end := fmt.Sprintf("exit=%d tokens=%d clankerbar_calls=%d", res.ExitCode, res.Tokens, res.ClankerbarCalls)
	if err != nil {
		end = "error: " + err.Error()
	}
	log.Printf("%splanner: conversation %s (%d pending) %s on %s/%s in %s - %s",
		labelOf(t), claim.ID, claim.PendingCount, mode, sess.Harness, model, elapsed.Round(time.Millisecond), end)
}

// refFor is the handle to record: the adapter's if it named one, else the ref
// the invocation asked for. A run that ERRORED records nothing: the handle may
// name a session that never started, and the plane stores no ref either (no
// reply was written), so the next attempt primes.
func refFor(res harness.Result, asked string, err error) string {
	if err != nil {
		return ""
	}
	if r := strings.TrimSpace(res.SessionRef); r != "" {
		return r
	}
	return asked
}

// newSessionRef mints a UUIDv4 for a fresh session. Claude creates a session
// under it (`--session-id`); opencode creates one too (`run -s`), and the id its
// stream reports replaces this one if the two differ.
func newSessionRef() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on a supported platform; an empty ref just
		// means this session cannot be resumed, which is survivable.
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
