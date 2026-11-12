// claim_wire.go — the shapes next_task and claim_task serve, decoded into
// TaskBrief. Split from claim.go so the call surface and its refusals stay
// readable beside it.
package plane

import (
	"encoding/json"
	"fmt"
)

// The collection fields get names of their own so the wire structs stay one
// screen wide and a decode helper reads as what it takes.
type (
	phaseDoneMap map[string]phaseDoneWire
	questionList []questionWire
	decisionList []decisionWire
)

// briefWire is one task as either call serves it. Every leaf is narrowed rather
// than type-asserted: the fields are descriptive, and one unexpected shape must
// not cost the runner the task it just claimed.
type briefWire struct {
	ID               any          `json:"id"`
	Ref              any          `json:"ref"`
	Title            any          `json:"title"`
	Detail           any          `json:"detail"`
	DoneWhen         any          `json:"doneWhen"`
	Status           any          `json:"status"`
	Repo             any          `json:"repo"`
	Category         any          `json:"category"`
	Branch           any          `json:"branch"`
	HasWIP           any          `json:"hasWip"`
	AwaitingMerge    any          `json:"awaitingMerge"`
	RequiresTakeover any          `json:"requiresTakeover"`
	MergeHint        any          `json:"mergeHint"`
	PhasesDone       phaseDoneMap `json:"phasesDone"`
	Questions        questionList `json:"questions"`
	Decisions        decisionList `json:"decisions"`
}

type phaseDoneWire struct {
	Tip any `json:"tip"`
	At  any `json:"at"`
}

// questionWire is one entry of the Q&A block qaFor serves. `status` and
// `superseded` say whether the answer is still the task's to work to.
type questionWire struct {
	Body       any `json:"body"`
	Answer     any `json:"answer"`
	Blocking   any `json:"blocking"`
	Status     any `json:"status"`
	Superseded any `json:"superseded"`
}

// decisionWire is one entry of the standing-decisions block decisionsFor
// serves: the full form carries `context` and `ruling`, and a decision elided to
// stay inside the block's byte ceiling carries only its ruling's first line.
type decisionWire struct {
	Context    any `json:"context"`
	Ruling     any `json:"ruling"`
	RulingLine any `json:"rulingLine"`
}

// runWire is the claim's run block. ID is the compound credential, served
// exactly once, here.
type runWire struct {
	ID             any `json:"id"`
	LeaseExpiresAt any `json:"leaseExpiresAt"`
}

// claimWire is claim_task's response: the task view under `task`, with the
// branch, Q&A and decisions spread BESIDE it at the top level.
type claimWire struct {
	Task          *briefWire   `json:"task"`
	Run           runWire      `json:"run"`
	Branch        any          `json:"branch"`
	HasWIP        any          `json:"hasWip"`
	AwaitingMerge any          `json:"awaitingMerge"`
	MergeHint     any          `json:"mergeHint"`
	Questions     questionList `json:"questions"`
	Decisions     decisionList `json:"decisions"`
}

// decodeOffer reads next_task's two offer slots. The response is accepted in
// both envelopes the task reads have used: the wrapper (`{"next": …}`), and the
// offered task itself, which is what a plane that serves the offer bare puts at
// the top level.
func decodeOffer(raw []byte) (Offer, error) {
	var payload struct {
		Next      *briefWire `json:"next"`
		ReadyNext *briefWire `json:"readyNext"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Offer{}, fmt.Errorf("next_task: decode response: %w", err)
	}
	offer := Offer{}
	if payload.Next != nil {
		next := payload.Next.brief()
		offer.Next = &next
	}
	if payload.ReadyNext != nil {
		ready := payload.ReadyNext.brief()
		offer.ReadyNext = &ready
	}
	if offer.Next == nil && offer.ReadyNext == nil {
		var bare briefWire
		if err := json.Unmarshal(raw, &bare); err == nil && wireString(bare.ID) != "" {
			next := bare.brief()
			offer.Next = &next
		}
	}
	return offer, nil
}

// decodeClaim reads claim_task's response. claim_task always wraps the task, so
// accepting a bare task object is only the same tolerance the task reads have.
func decodeClaim(raw []byte) (ClaimedTask, error) {
	var payload claimWire
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ClaimedTask{}, fmt.Errorf("claim_task: decode response: %w", err)
	}
	task, err := payload.taskBrief(raw)
	if err != nil {
		return ClaimedTask{}, err
	}
	return ClaimedTask{
		Task:           task,
		RunID:          wireString(payload.Run.ID),
		LeaseExpiresAt: wireString(payload.Run.LeaseExpiresAt),
	}, nil
}

// taskBrief shapes the claimed task. The fields the plane spreads beside the
// task object WIN where present: the task's own `branch` is the raw recorded
// name (null when nothing is recorded) while the sibling is the always-set name
// a worktree is created from.
func (w claimWire) taskBrief(raw []byte) (TaskBrief, error) {
	base := briefWire{}
	if w.Task != nil {
		base = *w.Task
	} else if err := json.Unmarshal(raw, &base); err != nil {
		return TaskBrief{}, fmt.Errorf("claim_task: decode response: %w", err)
	}
	if v := wireString(w.Branch); v != "" {
		base.Branch = v
	}
	if v := wireString(w.MergeHint); v != "" {
		base.MergeHint = v
	}
	if wireBool(w.HasWIP) {
		base.HasWIP = true
	}
	if wireBool(w.AwaitingMerge) {
		base.AwaitingMerge = true
	}
	if w.Questions != nil {
		base.Questions = w.Questions
	}
	if w.Decisions != nil {
		base.Decisions = w.Decisions
	}
	return base.brief(), nil
}

// brief shapes one decoded task.
func (w briefWire) brief() TaskBrief {
	return TaskBrief{
		ID:               wireString(w.ID),
		Ref:              wireString(w.Ref),
		Title:            wireString(w.Title),
		Detail:           wireString(w.Detail),
		DoneWhen:         wireString(w.DoneWhen),
		Status:           wireString(w.Status),
		Repo:             wireString(w.Repo),
		Category:         wireString(w.Category),
		Branch:           wireString(w.Branch),
		HasWIP:           wireBool(w.HasWIP),
		AwaitingMerge:    wireBool(w.AwaitingMerge),
		RequiresTakeover: wireBool(w.RequiresTakeover),
		MergeHint:        wireString(w.MergeHint),
		PhasesDone:       wirePhasesDone(w.PhasesDone),
		Questions:        wireQuestions(w.Questions),
		Decisions:        wireDecisions(w.Decisions),
	}
}

// wireQuestions shapes the Q&A block into the brief's view, keeping only the
// questions a session must still work to: an answered question the plane has
// since superseded, or one dismissed or still open, is not an answer. An absent
// block (a plane that serves none, or a task with no such questions) yields nil.
func wireQuestions(w questionList) []AnsweredQuestion {
	if len(w) == 0 {
		return nil
	}
	out := make([]AnsweredQuestion, 0, len(w))
	for _, q := range w {
		answer := wireString(q.Answer)
		// An empty status is an older plane that predates the field.
		status := wireString(q.Status)
		if wireBool(q.Superseded) || answer == "" || (status != "" && status != "answered") {
			continue
		}
		out = append(out, AnsweredQuestion{
			Body:     wireString(q.Body),
			Answer:   answer,
			Blocking: wireBool(q.Blocking),
		})
	}
	return out
}

// wireDecisions shapes the standing-decisions block. Every entry the plane sent
// is kept, elided ones included, so the brief can say how much was settled even
// where a decision's bodies were withheld.
func wireDecisions(w decisionList) []Decision {
	if len(w) == 0 {
		return nil
	}
	out := make([]Decision, 0, len(w))
	for _, d := range w {
		answer := wireString(d.Ruling)
		if answer == "" {
			answer = wireString(d.RulingLine)
		}
		out = append(out, Decision{Question: wireString(d.Context), Answer: answer})
	}
	return out
}

// wirePhasesDone shapes the per-phase marker, keyed by the phase name the plane
// used. Absent (a plane that predates the field) stays nil, which reads as "no
// phase recorded done" rather than as a marker of zero phases.
func wirePhasesDone(w phaseDoneMap) map[string]PhaseDone {
	if len(w) == 0 {
		return nil
	}
	out := make(map[string]PhaseDone, len(w))
	for phase, done := range w {
		out[phase] = PhaseDone{Tip: wireString(done.Tip), At: wireString(done.At)}
	}
	return out
}
