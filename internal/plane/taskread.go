package plane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The driver's small READ surface over the same MCP endpoint its writes use.
//
// It exists so the dispatcher knows a task's repo BEFORE it spawns a harness
// (agent-rule-scoping piece 3): a session's working directory is fixed at spawn
// time, but the task it will claim is only known to the plane — a fresh phase
// has not claimed anything yet, and a resumed phase knows only the task ID its
// predecessor's claim carried. So the driver asks: next_task for the queue head
// a fresh phase would claim, get_task for the exact task a resumed phase
// continues. Both responses carry the task's `repo` field, which is what the
// loop resolves to a checkout.
//
// Both are best-effort by contract: a read that fails degrades the spawn to the
// fallback chain (primary repo, then the legacy workdir), never to a failed
// run — the plane being briefly unreachable must not cost an iteration, when
// the session it would have spawned is itself about to need the plane anyway.

// NextTask is what the queue-head peek learned. Repo is the task's `repo`
// field, empty when the task carries none (or there is nothing queued).
type NextTask struct {
	TaskID string
	Repo   string
}

// TaskPeeker reads which task a FRESH phase would claim, so its repo can steer
// the spawn cwd. A zero NextTask with a nil error means "nothing claimable, or
// the head names no repo" — both resolve through the caller's fallback chain.
type TaskPeeker interface {
	PeekNextTask(ctx context.Context) (NextTask, error)
}

// TaskRepoSource reads one task's repo, for a RESUMED phase that already knows
// its task ID. Empty repo, nil error = the task carries none.
type TaskRepoSource interface {
	TaskRepo(ctx context.Context, taskID string) (string, error)
}

// TaskState is what a get_task read learned about WHO holds a task right now:
// the plane's own status word, the qualified ref, the id of the run currently
// holding it (empty when nothing does), any work-in-progress branch
// recorded on it, and whether the record carries a declared no-code
// delivery (CLA-497).
type TaskState struct {
	Status       string
	Ref          string
	ClaimedByRun string
	Branch       string

	// DeliveryNoCode reports the task record's delivery.noCode flag: the
	// plane's first-class "this task shipped no code" declaration. It rides
	// the SAME get_task read as everything above — one peek mechanism
	// (CLA-451), extended rather than duplicated (CLA-497) — so a reader of
	// the holder state never needs a second round-trip to learn it.
	DeliveryNoCode bool

	// Outcome is the task's outcome text. The review seam reads the legacy
	// `## Review` block off it (CLA-637 keeps writing that block for one
	// release), and the fix phase's structural grading falls back to it when no
	// durable record is available. The field is descriptive like the rest: a
	// caller that does not need it ignores it.
	Outcome string

	// Reviews is the task's durable per-reviewer review records, as get_task
	// serves them (CLA-636/637). The review→fix seam gate keys on one record's
	// round, reviewer role, run and reviewed tip; the fix phase's digest reads
	// the released findings. Records still sealed in an open round carry no
	// findings, by the plane's design — the driver gates on their metadata.
	Reviews []ReviewRecordState
	// ReviewCounted is the plane's count of non-void rounds in the newest
	// record's cycle. Zero when the task has no reviews.
	ReviewCounted int
	// ReviewOpenMustIDs is every open MUST on a RELEASED record, by the plane's
	// finding id. Empty while a round is sealed.
	ReviewOpenMustIDs []string
	// ReviewsTotal is how many durable records the task has. The plane serves
	// only the newest few in Reviews, so a total above len(Reviews) means older
	// records were left out.
	ReviewsTotal int
}

// ReviewRecordState is one durable review record's metadata (and, once the
// round is closed, its findings). The wire shape is the compact `reviews`
// block on get_task; see clankerbar's CLA-636 for the store.
type ReviewRecordState struct {
	ID           string
	Round        int
	Cycle        int
	ReviewerRole string
	// RunID is the run that wrote the record — the seam gate's freshness key.
	// A record from an earlier trip has a different run and cannot satisfy it.
	RunID       string
	Harness     string
	Model       string
	Branch      string
	ReviewedTip string
	Scope       ReviewScopeState
	Verdict     string
	// State is the plane's seal state: sealed (round open, findings withheld),
	// open (round closed, findings released) or void (aborted by lease expiry
	// or takeover).
	State         string
	Counts        ReviewSeverityCounts
	FindingsTotal int
	At            string
	Findings      []ReviewFindingState
}

// ReviewScopeState is what a record says it reviewed.
type ReviewScopeState struct {
	Kind  string
	Base  string
	Range string
}

// ReviewSeverityCounts is the compact per-severity count on every record,
// served even while sealed.
type ReviewSeverityCounts struct {
	Must   int
	Should int
	Nit    int
}

// ReviewFindingState is one graded finding as the plane serves it. ID is the
// plane-assigned key (`<round>.<reviewer role>.<n>`) the fix phase updates by;
// RaisedByModel is the per-finding attribution the analytics need.
type ReviewFindingState struct {
	ID            string
	Severity      string
	EvidenceKind  string
	Evidence      string
	File          string
	Line          int
	Kind          string
	Summary       string
	RequiredFix   string
	RaisedByModel string
	Status        string
	FixCommit     string
	RefutedReason string
	FiledTaskRef  string
}

// TaskStateSource reads one task's holder state, for the CLA-451 checkpoint
// peek: when a phase ends without an observed claim, the driver asks this about
// the task it briefed before declaring the phase empty. Like both reads above,
// it is a READ of one named task — it holds nothing and claims nothing — and
// best-effort by contract: an error degrades to today's ending, never to a
// failed run.
type TaskStateSource interface {
	TaskState(ctx context.Context, taskID string) (TaskState, error)
}

// PeekNextTask calls the plane's next_task — the same read a session makes to
// find work, which is a read and not a claim: it holds nothing, so the peek
// cannot race a session out of work. It may hand back a stale-lease takeover
// candidate; that is still the task the spawned session will be offered first,
// so its repo is the right cwd to start in.
//
// The arguments are an EMPTY OBJECT, not nil: the plane's next_task schema is
// `inputSchema: {}` and MCP servers reject a null `arguments` against an object
// schema (observed live as "Invalid input / expected: record / params.arguments"
// on every daemon, which sent the peek to the primary-repo fallback and
// silently defeated CLA-437's repo routing, CLA-448).
func (r *mcpReleaser) PeekNextTask(ctx context.Context) (NextTask, error) {
	raw, err := r.callText(ctx, "next_task", map[string]any{})
	if err != nil {
		return NextTask{}, err
	}
	var payload struct {
		Next *struct {
			ID   string `json:"id"`
			Repo any    `json:"repo"`
		} `json:"next"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return NextTask{}, fmt.Errorf("next_task: decode response: %w", err)
	}
	if payload.Next == nil || payload.Next.ID == "" {
		return NextTask{}, nil
	}
	return NextTask{TaskID: payload.Next.ID, Repo: wireString(payload.Next.Repo)}, nil
}

// TaskRepo calls get_task for one task and returns its `repo` field. The
// response is accepted in both envelopes the plane has used: wrapped
// ({"task": {...}}) and bare (the task object itself).
func (r *mcpReleaser) TaskRepo(ctx context.Context, taskID string) (string, error) {
	if taskID == "" {
		return "", errors.New("task repo: taskId is required")
	}
	raw, err := r.callText(ctx, "get_task", map[string]any{"taskId": taskID})
	if err != nil {
		return "", err
	}
	var payload struct {
		Task *struct {
			Repo any `json:"repo"`
		} `json:"task"`
		Repo any `json:"repo"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("get_task: decode response: %w", err)
	}
	if payload.Task != nil {
		return wireString(payload.Task.Repo), nil
	}
	return wireString(payload.Repo), nil
}

// TaskState calls get_task for one task and returns the fields naming its
// holder: status, claimedByRun, branch and ref, plus the delivery record's
// noCode flag. The response is accepted in
// both envelopes the plane has used (see TaskRepo). A field the plane sends as
// null decodes to empty, like repo above — an unheld task reads as
// status "ready" (or whatever it moved to) with no run and no branch, which is
// exactly the shape the caller's guard wants to see.
func (r *mcpReleaser) TaskState(ctx context.Context, taskID string) (TaskState, error) {
	if taskID == "" {
		return TaskState{}, errors.New("task state: taskId is required")
	}
	raw, err := r.callText(ctx, "get_task", map[string]any{"taskId": taskID})
	if err != nil {
		return TaskState{}, err
	}
	var payload struct {
		Task *taskHolderWire `json:"task"`
		taskHolderWire
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return TaskState{}, fmt.Errorf("get_task: decode response: %w", err)
	}
	w := payload.taskHolderWire
	if payload.Task != nil {
		w = *payload.Task
		// The plane serves the `reviews` block as a SIBLING of `task` in the
		// get_task response (server.ts spreads reviewsFor at the top level),
		// and has never put it inside the task object. Reading it only from
		// the task object left every durable record unseen in production, so
		// the review and verify seams refused every healthy review. The
		// top-level block wins; a nested one is read only when it is absent.
		if top := payload.taskHolderWire.Reviews; top != nil {
			w.Reviews = top
		}
	}
	return TaskState{
		Status:            wireString(w.Status),
		Ref:               wireString(w.Ref),
		ClaimedByRun:      wireString(w.ClaimedByRun),
		Branch:            wireString(w.Branch),
		DeliveryNoCode:    w.Delivery != nil && wireBool(w.Delivery.NoCode),
		Outcome:           wireString(w.Outcome),
		Reviews:           wireReviews(w.Reviews),
		ReviewCounted:     wireReviewsCounted(w.Reviews),
		ReviewsTotal:      wireReviewsTotal(w.Reviews),
		ReviewOpenMustIDs: wireReviewsOpenMustIDs(w.Reviews),
	}, nil
}

// taskHolderWire is the slice of a get_task response that names its holder.
type taskHolderWire struct {
	Status       any `json:"status"`
	Ref          any `json:"ref"`
	ClaimedByRun any `json:"claimedByRun"`
	Branch       any `json:"branch"`
	Outcome      any `json:"outcome"`
	Delivery     *struct {
		NoCode any `json:"noCode"`
	} `json:"delivery"`
	// The compact `reviews` block (CLA-636): present only when the task has
	// durable review records.
	Reviews *reviewsWire `json:"reviews"`
}

// reviewsWire is the compact reviews block get_task serves when a task has
// durable records. OpenMustIDs is only present when a RELEASED record has open
// MUSTs; absent on a task with no reviews or a sealed round.
type reviewsWire struct {
	Records     []reviewRecordWire `json:"records"`
	Total       any                `json:"total"`
	Counted     any                `json:"counted"`
	OpenMustIDs any                `json:"openMustIds"`
}

// reviewRecordWire is one record of the get_task `reviews` block. Every field
// is decoded through a narrowing helper: the block is descriptive, and one
// unexpected shape must not fail a seam decision that the branch evidence can
// still answer.
type reviewRecordWire struct {
	ID           any `json:"id"`
	Round        any `json:"round"`
	Cycle        any `json:"cycle"`
	ReviewerRole any `json:"reviewerRole"`
	RunID        any `json:"runId"`
	Harness      any `json:"harness"`
	Model        any `json:"model"`
	Branch       any `json:"branch"`
	ReviewedTip  any `json:"reviewedTip"`
	Scope        *struct {
		Kind  any `json:"kind"`
		Base  any `json:"base"`
		Range any `json:"range"`
	} `json:"scope"`
	Verdict any `json:"verdict"`
	State   any `json:"state"`
	Counts  *struct {
		Must   any `json:"must"`
		Should any `json:"should"`
		Nit    any `json:"nit"`
	} `json:"counts"`
	FindingsTotal any           `json:"findingsTotal"`
	At            any           `json:"at"`
	Findings      []findingWire `json:"findings"`
}

type findingWire struct {
	ID            any `json:"id"`
	Severity      any `json:"severity"`
	EvidenceKind  any `json:"evidenceKind"`
	Evidence      any `json:"evidence"`
	File          any `json:"file"`
	Line          any `json:"line"`
	Kind          any `json:"kind"`
	Summary       any `json:"summary"`
	RequiredFix   any `json:"requiredFix"`
	RaisedByModel any `json:"raisedByModel"`
	Status        any `json:"status"`
	FixCommit     any `json:"fixCommit"`
	RefutedReason any `json:"refutedReason"`
	FiledTaskRef  any `json:"filedTaskRef"`
}

// wireReviews shapes the compact reviews block into the driver's view. A nil
// block (no records, or an older plane) yields nil, which is exactly "nothing
// durable to gate on".
func wireReviews(block *reviewsWire) []ReviewRecordState {
	if block == nil {
		return nil
	}
	out := make([]ReviewRecordState, 0, len(block.Records))
	for _, r := range block.Records {
		rec := ReviewRecordState{
			ID:            wireString(r.ID),
			Round:         wireInt(r.Round),
			Cycle:         wireInt(r.Cycle),
			ReviewerRole:  wireString(r.ReviewerRole),
			RunID:         wireString(r.RunID),
			Harness:       wireString(r.Harness),
			Model:         wireString(r.Model),
			Branch:        wireString(r.Branch),
			ReviewedTip:   wireString(r.ReviewedTip),
			Verdict:       wireString(r.Verdict),
			State:         wireString(r.State),
			FindingsTotal: wireInt(r.FindingsTotal),
			At:            wireString(r.At),
		}
		if r.Scope != nil {
			rec.Scope = ReviewScopeState{
				Kind:  wireString(r.Scope.Kind),
				Base:  wireString(r.Scope.Base),
				Range: wireString(r.Scope.Range),
			}
		}
		if r.Counts != nil {
			rec.Counts = ReviewSeverityCounts{
				Must:   wireInt(r.Counts.Must),
				Should: wireInt(r.Counts.Should),
				Nit:    wireInt(r.Counts.Nit),
			}
		}
		for _, f := range r.Findings {
			rec.Findings = append(rec.Findings, ReviewFindingState{
				ID:            wireString(f.ID),
				Severity:      wireString(f.Severity),
				EvidenceKind:  wireString(f.EvidenceKind),
				Evidence:      wireString(f.Evidence),
				File:          wireString(f.File),
				Line:          wireInt(f.Line),
				Kind:          wireString(f.Kind),
				Summary:       wireString(f.Summary),
				RequiredFix:   wireString(f.RequiredFix),
				RaisedByModel: wireString(f.RaisedByModel),
				Status:        wireString(f.Status),
				FixCommit:     wireString(f.FixCommit),
				RefutedReason: wireString(f.RefutedReason),
				FiledTaskRef:  wireString(f.FiledTaskRef),
			})
		}
		out = append(out, rec)
	}
	return out
}

// wireReviewsCounted is the non-void round count the plane serves beside the
// records. Zero when the block or the field is absent (no reviews, old plane).
func wireReviewsCounted(block *reviewsWire) int {
	if block == nil {
		return 0
	}
	return wireInt(block.Counted)
}

// wireReviewsTotal is the task's full record count, served beside the newest
// records get_task carries.
func wireReviewsTotal(block *reviewsWire) int {
	if block == nil {
		return 0
	}
	return wireInt(block.Total)
}

// wireReviewsOpenMustIDs is the released open-MUST list, absent while a round
// is sealed or when the task has no reviews.
func wireReviewsOpenMustIDs(block *reviewsWire) []string {
	if block == nil {
		return nil
	}
	return wireStrings(block.OpenMustIDs)
}

// wireInt narrows a decoded JSON number. JSON decodes numbers as float64 into
// an `any`; a non-number is absent, like every other descriptive field.
func wireInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

// wireStrings narrows a decoded JSON string array (openMustIds).
func wireStrings(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// wireString narrows a decoded JSON field the plane sends as a string or null
// (repo, status, ref, branch, claimedByRun). A non-string is treated as absent
// rather than an error: every field read here is descriptive, and no value one
// can carry turns a spawn or a seam decision into a failure — resolution and
// classification act only on real strings.
func wireString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// wireBool narrows a decoded JSON field the plane sends as a boolean or null.
// A non-boolean is treated as absent (false), matching wireString's posture:
// the fields read here describe the record, and none of them turns a peek
// into an error.
func wireBool(v any) bool {
	b, _ := v.(bool)
	return b
}

// callText is call for callers who need the result's text payload, not just its
// success: the two task-read methods decode JSON out of the tool's content.
func (r *mcpReleaser) callText(ctx context.Context, tool string, args map[string]any) ([]byte, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d: %s", tool, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := checkResult(tool, raw); err != nil {
		return nil, err
	}
	return resultText(sseData(raw)), nil
}

// resultText concatenates a tools/call result's text contents — where the
// plane's task reads carry their JSON payload. Empty when the shape is not what
// the writers here expect; the JSON decode upstream reports that as its own
// error, so this stays silent.
func resultText(payload []byte) []byte {
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &rpc); err != nil {
		return nil
	}
	var b strings.Builder
	for _, c := range rpc.Result.Content {
		b.WriteString(c.Text)
	}
	return []byte(b.String())
}
