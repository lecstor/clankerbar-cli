// claim.go — the runner's claim surface: next_task to find the task and
// claim_task to take it. A session used to make both calls; the runner makes
// them now and hands the session a task it already holds, in a worktree that
// already exists (docs/runner-owned-mechanics.md).
//
// The two refusal classes are typed because they need opposite answers.
// ErrClaimLost is a race: re-read the queue and take another task, never retry
// this one. ErrNotClaimable is withheld work: no retry reaches it.
package plane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrClaimLost means the task is no longer this claim's to take: another
	// clanker took it over, its status moved, or the lease that was offered as
	// stale came back to life.
	ErrClaimLost = errors.New("the claim was lost")

	// ErrNotClaimable means the plane withholds the task from the fleet - a
	// container, human-only work, a `someday` lane, approved work the operator
	// integrates, or a task at its reclaim bound.
	ErrNotClaimable = errors.New("the task is not claimable")
)

// TaskBrief is one task as the runner briefs it: the words to work to, where
// the work goes, and what is already settled about it.
//
// Branch is ALWAYS set - the plane synthesises a name when nothing is recorded
// - so a worktree can be created before any work exists. HasWIP is the separate
// signal that the branch is RECORDED.
type TaskBrief struct {
	ID       string
	Ref      string
	Title    string
	Detail   string
	DoneWhen string
	Status   string
	Repo     string
	Category string
	Branch   string

	// HasWIP reports the branch is RECORDED (a clanker committed and pushed);
	// AwaitingMerge marks approved work offered for its close-out;
	// RequiresTakeover marks a stale-lease offer whose claim must take over.
	HasWIP           bool
	AwaitingMerge    bool
	RequiresTakeover bool

	// MergeHint is the plane's close-out instruction, served on an awaitingMerge
	// offer; empty otherwise.
	MergeHint string

	// Decisions is the project's standing decisions, byte-bounded by the plane,
	// so an elided one carries only its ruling's first line. Questions is what
	// was asked and answered about this task. PhasesDone is the per-phase marker,
	// keyed by phase name, and nil on a plane that does not serve it.
	Decisions  []Decision
	Questions  []AnsweredQuestion
	PhasesDone map[string]PhaseDone
}

// Decision is one standing decision's ruling: the framing it settled and the
// call itself. Provenance is not carried - the brief asks the session to work
// within the decision, not to weigh who made it.
type Decision struct {
	Question string
	Answer   string
}

// AnsweredQuestion is one question filed against the task, with the answer it
// got. Only answered, non-superseded questions are kept: a question still open,
// dismissed, or superseded by a later answer is not something to work to.
type AnsweredQuestion struct {
	Body     string
	Answer   string
	Blocking bool
}

// PhaseDone is the tip at which one phase finished, and when.
type PhaseDone struct {
	Tip string
	At  string
}

// Offer is what next_task proposed. Both are nil when nothing is queued.
type Offer struct {
	Next *TaskBrief
	// ReadyNext is what a caller declines a TAKEOVER or a CLOSE-OUT by
	// plain-claiming; the plane serves it only when declining is possible.
	ReadyNext *TaskBrief
}

// ClaimedTask is a held task and the credential every later call signs with.
type ClaimedTask struct {
	Task TaskBrief
	// RunID is the plane's run CREDENTIAL (`<uuid>.<secret>`), opaque: every
	// consumer echoes it back and none parses it.
	RunID          string
	LeaseExpiresAt string
}

// TaskClaimer is the loop-facing claim surface: find the task, then take it.
type TaskClaimer interface {
	NextOffer(ctx context.Context) (Offer, error)
	ClaimTask(ctx context.Context, taskID string, takeover bool) (ClaimedTask, error)
}

var (
	_ TaskClaimer = notWired{}
	_ TaskClaimer = (*mcpReleaser)(nil)
)

func (notWired) NextOffer(context.Context) (Offer, error) { return Offer{}, ErrNotWired }
func (notWired) ClaimTask(context.Context, string, bool) (ClaimedTask, error) {
	return ClaimedTask{}, ErrNotWired
}

// NextOffer calls next_task and decodes both slots. The arguments are an EMPTY
// OBJECT rather than nil, for the reason PeekNextTask records: the plane's
// next_task schema is `inputSchema: {}` and an MCP server rejects a null
// `arguments` against an object schema.
func (r *mcpReleaser) NextOffer(ctx context.Context) (Offer, error) {
	raw, err := r.callText(ctx, "next_task", map[string]any{})
	if err != nil {
		return Offer{}, err
	}
	offer, err := decodeOffer(raw)
	if err != nil {
		return Offer{}, err
	}
	return offer, nil
}

// ClaimTask claims one task: a fresh `ready` claim, or with takeover a stale
// in-progress task the queue offered with requiresTakeover. The claim is
// atomic and first-wins, so a refusal is the plane's answer about who owns it
// now, and it is classified before it reaches the caller.
//
// `takeover` is sent only when true: its absence IS the fresh claim.
func (r *mcpReleaser) ClaimTask(ctx context.Context, taskID string, takeover bool) (ClaimedTask, error) {
	if taskID == "" {
		return ClaimedTask{}, errors.New("claim task: taskId is required")
	}
	args := map[string]any{"taskId": taskID}
	if takeover {
		args["takeover"] = true
	}
	raw, err := r.callText(ctx, "claim_task", args)
	if err != nil {
		return ClaimedTask{}, classifyRefusal(err)
	}
	claimed, err := decodeClaim(raw)
	if err != nil {
		return ClaimedTask{}, err
	}
	// Without a run id nothing later in the run can be signed, so a claim that
	// serves no credential is not a claim.
	if claimed.RunID == "" {
		return ClaimedTask{}, errors.New("claim_task: the plane returned no run id, so nothing can be signed")
	}
	return claimed, nil
}

// classifyRefusal wraps a plane refusal in the sentinel its code belongs to, so
// a caller branches with errors.Is instead of matching prose. Codes this build
// does not name pass through untouched: the plane may refuse for reasons a
// retry or a re-read fixes, and that verdict is the caller's to make.
func classifyRefusal(err error) error {
	code, message, ok := refusalCode(err)
	if !ok {
		return err
	}
	switch code {
	case "claim_lost", "not_ready", "lease_live", "not_stale", "not_found":
		return fmt.Errorf("%w: %s", ErrClaimLost, refusalText(code, message))
	case "not_claimable", "human_only", "is_container", "reclaim_bound_exhausted":
		return fmt.Errorf("%w: %s", ErrNotClaimable, refusalText(code, message))
	}
	return err
}

// refusalCode recovers the plane's structured refusal (`{"error":{"code",
// "message"}}`) from the error checkResult builds around it. The code is the
// only machine-readable half of a ToolError, and it must keep decoding here if
// that error's shape ever changes.
func refusalCode(err error) (code, message string, ok bool) {
	if err == nil {
		return "", "", false
	}
	text := err.Error()
	start := strings.Index(text, "{")
	if start < 0 {
		return "", "", false
	}
	var wire struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(text[start:]), &wire) != nil {
		return "", "", false
	}
	code = strings.TrimSpace(wire.Error.Code)
	if code == "" {
		return "", "", false
	}
	return code, strings.TrimSpace(wire.Error.Message), true
}

// refusalText keeps the plane's own wording, which is what tells the caller
// what to do next, and falls back to the code when a refusal carries no prose.
func refusalText(code, message string) string {
	if message == "" {
		return code
	}
	return message
}
