package harness

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func init() { Register(opencode{}) }

// opencode drives the open, provider-agnostic opencode CLI, 2.x line (`opencode run`).
// `--standalone` is load-bearing: 2.x is client/server and `run` otherwise attaches to the managed
// background service, whose own config then wins over everything this adapter sets.
type opencode struct{}

func (opencode) Name() string { return "opencode" }

// opencode DOES receive mcp_config_path - as OPENCODE_CONFIG and, rewritten, as
// OPENCODE_CONFIG_CONTENT (see opencode_permission.go) - and that is exactly why "does the adapter hand it over"
// was the wrong question to gate a preflight on. A Claude-shaped file is a different schema: 1.18.2
// refused to start on it, and 1.18.18 and 2.0.32 silently skip its `mcpServers` key, so the session
// gets no clankerbar tools.
func (opencode) MCPConfigUse() MCPConfigUse {
	return MCPConfigUse{
		Schema: MCPConfigNative,
		Note: "the opencode adapter passes mcp_config_path as OPENCODE_CONFIG (plus a rewritten copy as OPENCODE_CONFIG_CONTENT), which must be an opencode " +
			"config (servers under `mcp` or `mcp.servers`, not Claude's `mcpServers`) - opencode skips a key it does not know, so a Claude-shaped file gives sessions no clankerbar tools",
	}
}

// Invoke runs one opencode session for the driver, and — CLA-406 — when that session dies the
// quiet death (stream dropped mid-generation, no terminal chunk, no usage), tries to RESURRECT it
// in place rather than hand the driver a corpse: back off, probe the session with a coherence
// question, and on a mechanical match continue it where it left off. See opencode_resume.go for the
// rules and their rationale; the loop here is only their orchestration.
func (o opencode) Invoke(ctx context.Context, in Invocation) (Result, error) {
	// Console -> agent delivery. The RUN id is not known at spawn for a fresh claim — the session
	// claims inside itself — so the adapter learns it from ResumeClaim (a resumed phase) or the
	// OnClaim watcher, exactly as claude does.
	var delivery *opencodeInbox
	if in.Inbox != nil && !in.Probe {
		runIDs := make(chan string, 8)
		if in.ResumeClaim.RunID != "" {
			runIDs <- in.ResumeClaim.RunID
		}
		lastRunID := in.ResumeClaim.RunID
		origOnClaim := in.OnClaim
		in.OnClaim = func(c Claim) {
			// A settle fires this callback with the run id unchanged; only a NEW run is a delivery target.
			if c.RunID != "" && c.RunID != lastRunID {
				lastRunID = c.RunID
				select {
				case runIDs <- c.RunID:
				default: // pathological churn; the newest claim will start the next poller
				}
			}
			if origOnClaim != nil {
				origOnClaim(c)
			}
		}
		delivery = newOpencodeInbox(ctx, in.Console, *in.Inbox, runIDs)
		defer delivery.close()
	}

	// The wall-clock cap bounds the WHOLE Invoke, not each process: the deadline is computed BEFORE
	// the first run so the original run's time counts against the budget the continuations inherit.
	var deadline time.Time
	if in.MaxSessionWallClock > 0 {
		deadline = time.Now().Add(in.MaxSessionWallClock)
	}
	res, err := o.runSession(ctx, in, opencodeArgs(in), delivery)
	if err != nil {
		return res, err
	}
	if in.Probe {
		// Never resurrect a probe: a probe IS the liveness check.
		return res, nil
	}
	if len(in.Turns) > 0 {
		// A planned multi-turn session: the follow-up turns run in this same session before any recovery
		// pass, because a turn that ends badly must stop the sequence rather than be resumed behind the
		// caller's back. A completed sequence hands back the FINAL turn's invocation: everything below
		// judges the turn that actually ended the session, never the first one's model or route list.
		var completed bool
		in, completed, err = o.runTurns(ctx, in, &res, deadline, delivery)
		if err != nil {
			return res, err
		}
		if !completed {
			return res, nil
		}
	}
	if res.ToolLoop != "" || res.IdleKilled != "" {
		// CLA-739 / the idle watchdog: killed for a cause of the adapter's own.
		return res, nil
	}
	console := in.Console
	// CLA-584: the output-cap stall. A final step that hit the output token limit with zero output is
	// NOT the quiet death the loop below resumes — the session is alive and its transcript is
	// intact; the step failed because the model spent its whole output budget thinking.
	if o.OutputCapNoOutput(res) {
		o.resumeOutputCapStall(ctx, in, &res, deadline, delivery)
	}
	o.resumeInterrupted(ctx, in, &res, deadline, delivery)
	if !in.Probe {
		if err := o.switchRoutesOnLimit(ctx, in, in.RouteList(), &res, deadline, delivery); err != nil {
			return res, err
		}
		// Keep later resumes on the route that actually ran: the resurrection probe and continuation
		// below read in.ModelArg(), and resuming on the route that was just denied would only re-hit its
		// limit.
		if finished := RouteFinished(res); finished != "" {
			in = in.OnRoute(finished)
		}
	}
	// An UNTRUSTED stream is never resurrected: CLA-262 forbids reading claim state off an over-run
	// capture, and both the coherence ref and the continuation's seed come from exactly that state -
	// while the driver's own untrusted path declines to count further spend, so probing would enlarge
	// the accounting blind spot endUntrustedDrain exists to bound.
	for attempt := 1; attempt <= opencodeMaxResurrections && o.ZeroUsageUnknown(res) && res.Untrusted == "" && res.ToolLoop == ""; attempt++ {
		sid, ref := resumeTargets(res)
		if sid == "" || ref == "" {
			break
		}
		// Budget check BEFORE the round spends anything: a round's backoff plus probe box come off the
		// clock before any continuation could, so a slice under the round floor can only buy a probe
		// whose answer is never acted on.
		if !deadline.IsZero() && time.Until(deadline) < opencodeResumeRoundFloor {
			if console != nil {
				fmt.Fprint(console, opencodeWallClockExhaustedMsg)
			}
			break
		}
		if console != nil {
			fmt.Fprintf(console, "\n!! quiet death detected — resurrecting session %s (attempt %d of %d): backing off %s, then probing for coherence against %s\n",
				sid, attempt, opencodeMaxResurrections, opencodeResumeBackoff, ref)
		}
		select {
		case <-ctx.Done():
			// Run-wide cancellation (Ctrl-C / supervised wait): not this phase's verdict.
			return res, nil
		case <-time.After(opencodeResumeBackoff):
		}

		pctx, pcancel := context.WithTimeout(ctx, opencodeProbeTimeout)
		probeIn := in
		probeIn.Probe = true        // no wall-clock double-cap, no console tee
		probeIn.NoBacklogMCP = true // coherence probe must never reach the backlog
		probeIn.ResumeClaim = res.Claim
		probeRes, probeErr := o.runSession(pctx, probeIn,
			opencodeResumeArgs(in, sid, resurrectionProbePrompt), nil) // a probe has no private server to steer
		pcancel()
		res.Tokens += probeRes.Tokens
		res.CostUSD += probeRes.CostUSD
		res.Usage.Add(probeRes.Usage)
		res.UsageReported = res.UsageReported || probeRes.UsageReported
		res.CostReported = res.CostReported || probeRes.CostReported
		// The claim observation folds ONLY when it is still about THIS task and arrived on a capture we
		// may read: noteClaimed replaces a parser's claim WHOLESALE, so a decohered probe that re-claimed
		// a different task would otherwise redirect the merged Result's identity — the real lease
		// expiring while release/salvage/tally all fire against a task the dead session never held.
		if probeRes.Untrusted == "" && probeRes.Claim.Names(res.Claim.TaskID) {
			mergeObservation(&res, probeRes)
		}
		coherent := probeErr == nil && probeRes.Untrusted == "" && probeCoherent(probeRes.FinalMessage, ref)
		if !coherent {
			if console != nil {
				fmt.Fprintf(console, "!! resurrection probe FAILED (err=%v, reply=%.200q) — counting the death\n",
					probeErr, probeRes.FinalMessage)
			}
			break
		}

		// Coherent: continue the session for real. The continuation's parser is seeded with the DEAD
		// RUN'S live claim (not in.ResumeClaim, which on a phase 2+ holds the staler predecessor):
		// `opencode run -s` streams only the new turn, so without the seed every progress write the
		// continuation makes would go unobserved — no settle, no HasWIP, no delivery report — and
		// releaseHeldClaim would post ready over work the session pushed.
		contIn := in
		contIn.ResumeClaim = res.Claim
		if !deadline.IsZero() {
			// Re-checked here, not just at the top of the round: the probe just consumed up to its own
			// time-box, and a continuation that could only be cap-killed on arrival must not be started.
			remaining := time.Until(deadline)
			if remaining < opencodeResumeWallClockFloor {
				if console != nil {
					fmt.Fprint(console, opencodeWallClockExhaustedMsg)
				}
				break
			}
			contIn.MaxSessionWallClock = remaining
		}
		contRes, contErr := o.runSession(ctx, contIn,
			opencodeResumeArgs(in, sid, continuePrompt), delivery)
		if contErr != nil {
			// Spend first, even on the error path.
			res.Tokens += contRes.Tokens
			res.CostUSD += contRes.CostUSD
			res.Usage.Add(contRes.Usage)
			res.UsageReported = res.UsageReported || contRes.UsageReported
			res.CostReported = res.CostReported || contRes.CostReported
			if console != nil {
				fmt.Fprintf(console, "!! resurrection continuation errored (%v) — counting the death\n", contErr)
			}
			break
		}
		n := 0
		if nRaw, ok := res.Raw[opencodeResurrectionsKey].(int); ok {
			n = nRaw
		}
		mergeResume(&res, contRes)
		if res.Raw == nil {
			res.Raw = map[string]any{}
		}
		res.Raw[opencodeResurrectionsKey] = n + 1
		if console != nil {
			fmt.Fprintf(console, "!! resurrection %d succeeded — session continued in place\n", n+1)
		}
	}
	// A route that failed for a provider reason — transient, a quiet death left unrecovered, or a
	// death before any tool call — is abandoned for the next configured route, resuming the session
	// when it named one and starting fresh when it died before it could.
	if !in.Probe {
		if err := o.switchRoutesOnProviderError(ctx, in, in.RouteList(), &res, deadline, delivery); err != nil {
			return res, err
		}
		if finished := RouteFinished(res); finished != "" {
			in = in.OnRoute(finished)
		}
	}
	o.resumeBadReport(ctx, in, &res, deadline, delivery)
	if len(res.TurnMessages) > 0 {
		res.TurnMessages[len(res.TurnMessages)-1] = res.FinalMessage
	}
	// Studio chat (studio-chat.md §3): a stream that never named a session still ran under the handle
	// the invocation gave it, and that is what the planner records.
	if res.SessionRef == "" {
		res.SessionRef = strings.TrimSpace(in.SessionRef)
	}
	return res, nil
}

// opencodeArgs maps an Invocation to `opencode run`'s CLI dialect. Split out so the probe's
// read-only shape is unit-testable without executing anything.
func opencodeArgs(in Invocation) []string {
	prompt := opencodePrompt(in.Prompt)
	if in.Probe {
		// Explicit no-tool prompt plus deny-all permission policy: a bare "." invites
		// models with AGENTS.md in context to treat the probe as a backlog drain.
		prompt = probePrompt
	}
	args := []string{"run", "--standalone", "--format", "json", "--agent", opencodeAgent}
	if m := in.ModelArg(); m != "" {
		args = append(args, "--model", m)
	}
	if ref := strings.TrimSpace(in.SessionRef); ref != "" {
		args = append(args, "-s", ref)
	}
	return append(args, "--", prompt)
}

// opencodePrompt is the message a working (non-probe) run sends. CLA-703 removed the race that note
// described: the adapter now starts its own server, waits until the clankerbar server reports
// connected, and only then attaches the run, so the first model turn already has the tools.
func opencodePrompt(prompt string) string {
	return prompt
}

// opencodeMCPResourcePattern is the pattern shape opencode's MCP resource tools ask with under the
// `read` permission: "mcp:<server>:<uri>" for a read and "mcp:<server>:*" for a listing.
const opencodeMCPResourcePattern = "mcp:*"

// No turn cap: Invocation.MaxTurns never reaches the CLI, so no exit can be attributed to one.
func (opencode) TurnCapped(Result) bool { return false }

// wallClockReason is the terminal_reason the ADAPTER writes into a Result when it ended the session
// for outliving Invocation.MaxSessionWallClock. Like tokenCeilingReason it is deliberately not
// something the CLI emits: the marker exists so the driver can tell an orderly cap from a genuine
// failure, and opencode's stream carries no reason field a task could write into anyway.
const wallClockReason = "wall_clock_capped"

func (opencode) WallClockCapped(res Result) bool {
	if res.Raw == nil {
		return false
	}
	r, _ := res.Raw["terminal_reason"].(string)
	return r == wallClockReason
}

// markWallClockCapped writes the adapter's own cap marker onto a Result, PRESERVING the parsed
// usage already there: unlike the claude token-ceiling kill, this session's spend was summed from
// step_finish events all the way to the kill, and replacing Raw would throw the per-step breakdown
// away while the budget still has to see what the session cost.
func (r *Result) markWallClockCapped() {
	if r.Raw == nil {
		r.Raw = map[string]any{}
	}
	r.Raw["terminal_reason"] = wallClockReason
}

// ZeroUsageUnknown reports whether this session ended with the CLA-398 quiet-death signature: a
// FINAL step_finish carrying reason "unknown" whose OWN usage was all-zero — whatever the earlier
// steps cost (a dead session may have done real paid work before the silent final step).
// The marker is the adapter's own (a terminal_reason it writes, never text the CLI or an agent
// could emit), and it exists so the driver can NAME the end instead of logging "iteration done
// (tokens=0 cost=$0.00)" over a session that produced nothing.
func (opencode) ZeroUsageUnknown(res Result) bool {
	if res.Raw == nil {
		return false
	}
	r, _ := res.Raw[TerminalReasonKey].(string)
	return r == ZeroUsageReason
}

// OutputCapNoOutput reports whether this session ended on the CLA-584 output-cap stall: a FINAL
// step_finish carrying reason "length" whose OWN output tokens were zero — the model spent the
// step's whole output budget thinking and emitted nothing.
func (opencode) OutputCapNoOutput(res Result) bool {
	if res.Raw == nil {
		return false
	}
	r, _ := res.Raw[TerminalReasonKey].(string)
	return r == OutputCapReason
}

// No mid-stream token ceiling: opencode sums per-step usage but the adapter never kills the
// process, so no exit of its can be attributed to one.
func (opencode) TokenCeilingHit(Result) bool { return false }

// TracksClaims is true as of CLA-365, and on 2.x only because the adapter takes every MCP server
// out of Code Mode (opencodeMCPOutOfCodeMode): opencode's `--format json` stream then carries each
// tool call's arguments AND its result on one terminal `tool_use` event, so the adapter reads the
// claim off the session the same way claude does and populates Result.Claim. HonoursMaxTurns stays
// false — Invocation.MaxTurns still never reaches the CLI.
func (opencode) Capabilities() Capabilities {
	// DeliversMessages is true because every non-probe session with a clankerbar MCP server runs
	// attached to a private `opencode serve` this adapter owns, and that server's session API steers
	// a message into the live run (opencode_inbox.go). A session with no such server has no delivery
	// target and simply receives nothing.
	return Capabilities{TracksClaims: true, ReportsCost: true, HonoursSessionWallClock: true, RunsTurnSequences: true, HonoursNoBacklogMCP: true, EmitsEvents: true, DeliversMessages: true, ResumesSessions: true}
}

func (o opencode) Probe(ctx context.Context, in Invocation) (ProbeResult, error) {
	in.Probe = true
	in.NoBacklogMCP = true
	res, err := o.Invoke(ctx, in)
	// Spend is taken off the Result on every path — see claude.Probe for why.
	out := ProbeResult{Tokens: res.Tokens, CostUSD: res.CostUSD}
	if err != nil {
		return out, err
	}
	return probeVerdict(out, res, o.DetectLimit)
}

func (opencode) ReadUsage(context.Context, Invocation) (Usage, error) {
	// opencode surfaces no remaining-quota/balance figure to a headless caller; billing lives with the
	// configured provider.
	return Usage{}, ErrUsageUnsupported
}
