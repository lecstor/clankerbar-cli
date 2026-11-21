package harness

import (
	"strings"
	"time"
)

const (
	// opencodeSessionIDKey is the Raw key carrying the opencode session id the run streamed under —
	// the handle a resurrection resumes into.
	opencodeSessionIDKey = "opencode_session_id"

	// opencodeResurrectionsKey is the Raw key counting completed resurrections on the final Result.
	opencodeResurrectionsKey = "resurrections"

	// opencodeMaxResurrections caps the WHOLE loop per Invoke: after this many successful-or-failed
	// rounds the next quiet death is accepted as terminal and the existing dead-phase path takes over.
	opencodeMaxResurrections = 5

	// opencodeProbeTimeout time-boxes the probe itself.
	opencodeProbeTimeout = 30 * time.Second

	// opencodeResumeWallClockFloor is the smallest slice of the Invoke's wall-clock budget worth
	// CONTINUING a resurrection round against, re-checked after the probe: a continuation that could
	// only be cap-killed on arrival must not be started.
	opencodeResumeWallClockFloor = time.Minute

	opencodeWallClockExhaustedMsg = "!! wall-clock budget exhausted across resurrections - counting the death\n"
)

// opencodeResumeRoundFloor is the smallest slice of the Invoke's wall-clock budget worth STARTING a
// resurrection round against.
var opencodeResumeRoundFloor = opencodeResumeBackoff + opencodeProbeTimeout + opencodeResumeWallClockFloor

// opencodeResumeBackoff is the pause between noticing a quiet death and sending the probe.
var opencodeResumeBackoff = 25 * time.Second

// resumeTargets extracts what a resurrection needs from a quietly-dead Result: the session id to
// resume INTO, and the claimed task's ref to verify the resumed agent against.
func resumeTargets(res Result) (sid, ref string) {
	if res.Raw != nil {
		sid, _ = res.Raw[opencodeSessionIDKey].(string)
	}
	if res.Claim.Held() {
		ref = res.Claim.Ref
	}
	return sid, ref
}

// resurrectionProbePrompt is the prompt a resumed session is asked FIRST.
const resurrectionProbePrompt = `Your previous response was interrupted mid-stream by the provider; your ` +
	`session transcript is intact up to the interruption, and you are being resumed ` +
	`in place. Confirm you are intact before continuing: reply with ONE line naming ` +
	`the task ref you are working on (the ref your claim_task call returned) and your ` +
	`last completed action. Then stop - you will be told to continue.`

// continuePrompt is sent once the probe has verified coherence.
const continuePrompt = "Confirmed. Continue exactly where you left off."

// probeCoherent decides whether a probe reply proves the session resumed with its context intact.
func probeCoherent(reply, ref string) bool {
	return strings.Contains(strings.ToUpper(reply), strings.ToUpper(ref))
}

// opencodeResumeArgs builds the args for a continuation run into an existing session: the same
// dialect as opencodeArgs, plus `-s <id>` and WITHOUT the phase prompt — a continuation carries
// its own short message.
func opencodeResumeArgs(in Invocation, sid, prompt string) []string {
	prompt = opencodePrompt(prompt)
	args := []string{"run", "--standalone", "--format", "json", "--agent", opencodeAgent, "-s", sid}
	if m := in.ModelArg(); m != "" {
		args = append(args, "--model", m)
	}
	return append(args, "--", prompt)
}

// mergeResume folds a successful continuation run's Result back into the base result of the
// original (dead) run, so what the driver sees is ONE session that was interrupted and carried on
// — not two.
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
	if add.ToolLoop != "" && base.ToolLoop == "" {
		base.ToolLoop = add.ToolLoop
	}
	// The idle watchdog's verdict folds the same way: a continuation killed for silence is how the
	// merged session ended, and its cause must survive the merge or the driver would see an
	// unexplained exit.
	if add.IdleKilled != "" && base.IdleKilled == "" {
		base.IdleKilled = add.IdleKilled
		base.IdleToolCall = add.IdleToolCall
	}
	base.Tokens += add.Tokens
	base.CostUSD += add.CostUSD
	base.ClankerbarCalls += add.ClankerbarCalls
	base.Usage.Add(add.Usage)
	base.UsageReported = base.UsageReported || add.UsageReported
	// CostReported folds the same way as the spend above: a continuation that reported its own dollars
	// makes the merged session's figure known even when the run it continued never reached its own
	// accounting event.
	base.CostReported = base.CostReported || add.CostReported
	if add.FinalMessage != "" {
		base.FinalMessage = add.FinalMessage
	}
	mergeObservation(base, add)
	if base.Raw == nil {
		base.Raw = map[string]any{}
	}
	for k, v := range add.Raw {
		if k == TerminalReasonKey || k == FinishReasonKey || k == OutputCapReasoningKey || k == RecoveredErrorsKey {
			continue // all four recomputed below, never inherited blind
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
	// finish_reason recomputed SYMMETRICALLY with terminal_reason: the parser writes the key only when
	// the stream carried a step_finish reason, so inheriting the corpse's would leave a merged Result
	// asserting the dead run's "unknown" while the mark above cleared - ZeroUsageUnknown says
	// recovered while deadPhase/tallyDead/deadscan (which read THIS key) say dead, and the phase parks
	// despite a successful resurrection.
	if fr, _ := add.Raw[FinishReasonKey].(string); fr != "" {
		base.Raw[FinishReasonKey] = fr
	} else {
		delete(base.Raw, FinishReasonKey)
	}
	if n, ok := add.Raw[OutputCapReasoningKey].(int); ok {
		base.Raw[OutputCapReasoningKey] = n
	} else {
		delete(base.Raw, OutputCapReasoningKey)
	}
	// The recovered-error marker is recomputed from the continuation for the reason spelled out in the
	// field list above: the parser writes it only when the stream did NOT end on an error, so its
	// presence is the continuation's own verdict that this session recovered, and its absence — a
	// terminal error, or nothing to recover from — must clear the dead run's.
	if n, ok := add.Raw[RecoveredErrorsKey].(int); ok && n > 0 {
		base.Raw[RecoveredErrorsKey] = n
	} else {
		delete(base.Raw, RecoveredErrorsKey)
	}
}

// mergeObservation folds one run's CLAIM OBSERVATION - claim state plus delivery reports - into a
// base Result, by mergeResume's rules: an unobserved claim wipes nothing (`run -s` streams only the
// new turn, so a run that saw no clankerbar tool events knows nothing), reports append with
// later-wins dedupe.
func mergeObservation(base *Result, add Result) {
	if add.Claim.Held() || add.Claim.Settled {
		base.Claim = add.Claim
	}
	for _, rep := range add.Reports {
		dup := false
		for i, have := range base.Reports {
			if have.sameClaim(rep) {
				base.Reports[i] = rep
				dup = true
				break
			}
		}
		if !dup {
			base.Reports = append(base.Reports, rep)
		}
	}
}
