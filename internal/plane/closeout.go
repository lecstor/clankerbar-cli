// The close-out surface (CLA-758): the plane calls a supervisor needs to finish
// an approved-awaiting-merge task with no agent session.
//
// The lifecycle is the one an agent close-out runs, minus the judgement: read the
// project's policy (who may integrate), list the approved queue, CLAIM the task
// (the claim is the lock, so a deterministic close-out and an agent never merge
// the same PR), release it honestly when the merge cannot be finished, and record
// the delivery. Kept as its own interface and its own constructor, like Releaser's
// siblings: a caller holds exactly the capability it asked for, and a not-wired
// plane degrades to "not wired" rather than to a silent no-op claim.
package plane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// CloseOutAPI is the plane surface the supervisor's deterministic close-out uses.
type CloseOutAPI interface {
	// ProjectPolicy reads the effective whose-turn dials (get_project_policy).
	ProjectPolicy(ctx context.Context) (ProjectPolicy, error)

	// ApprovedForCloseOut lists the project's approved-awaiting-merge tasks.
	ApprovedForCloseOut(ctx context.Context) (CloseOutQueue, error)

	// ClaimCloseOut claims one approved task through the close-out door and
	// returns the run id the rest of the close-out signs with. A refusal (the
	// task moved, another clanker holds it, the dial says the operator
	// integrates) is an error and means STOP.
	ClaimCloseOut(ctx context.Context, taskID string) (string, error)

	// ReleaseCloseOut hands a claim back with the task still approved, so the
	// agent close-out remains the fallback. checkGreen attests the PR's check was
	// green at the hand-back — true whenever this path released, because it never
	// merges an unjudged PR — which keeps the plane's CI-repair ladder from
	// charging a failed repair to a merge that simply did not happen.
	ReleaseCloseOut(ctx context.Context, taskID, runID string) error

	// CompleteCloseOut records the delivery and closes the task done, in the
	// claim's run.
	CompleteCloseOut(ctx context.Context, taskID, runID string, d CloseOutDelivery) error
}

// ProjectPolicy is the part of the whose-turn policy a close-out acts on. The
// other four dials are coordination signals this path does not touch.
type ProjectPolicy struct {
	// AuthorityIntegrate is "clanker" when an approved task is the fleet's to
	// merge, "human" when it is the operator's.
	AuthorityIntegrate string
}

// CloseOutTask is one approved-awaiting-merge task as the close-out needs it: the
// identity to log and claim, and where its work is.
type CloseOutTask struct {
	ID     string
	Ref    string
	Title  string
	Branch string
	Repo   string
}

// CloseOutQueue is one approved-awaiting-merge listing. Total is how many matched
// even when the served page was capped, so a caller can say so rather than
// silently closing out only part of the queue.
type CloseOutQueue struct {
	Tasks []CloseOutTask
	Total int
}

// CloseOutDelivery is the declaration a deterministic close-out records: the merge
// commit GitHub made, the PR it came from, the branch it landed on, and the
// driver's own ancestor verdict.
type CloseOutDelivery struct {
	Commit            string
	PR                string
	IntegrationBranch string
	MergeVerified     bool
}

// closeOutQueueLimit bounds one listing. The cap is the caller's to report when it
// bites: a project with more than this many approved tasks has a stalled
// integration queue, and quietly closing out a page of it would hide that.
const closeOutQueueLimit = 50

// NewCloseOutAPI builds the close-out half of the plane surface. Missing either
// the endpoint or the key yields the not-wired client, degrading exactly like New.
func NewCloseOutAPI(mcpURL, apiKey string) CloseOutAPI {
	if mcpURL == "" || apiKey == "" {
		return coNotWired{}
	}
	return wiredClient(mcpURL, apiKey)
}

// coNotWired is NewCloseOutAPI's not-wired answer — only the capabilities this
// interface names, so it cannot be asserted into a capability nobody requested.
type coNotWired struct{}

func (coNotWired) ProjectPolicy(context.Context) (ProjectPolicy, error) {
	return ProjectPolicy{}, ErrNotWired
}
func (coNotWired) ApprovedForCloseOut(context.Context) (CloseOutQueue, error) {
	return CloseOutQueue{}, ErrNotWired
}
func (coNotWired) ClaimCloseOut(context.Context, string) (string, error) {
	return "", ErrNotWired
}
func (coNotWired) ReleaseCloseOut(context.Context, string, string) error { return ErrNotWired }
func (coNotWired) CompleteCloseOut(context.Context, string, string, CloseOutDelivery) error {
	return ErrNotWired
}

// ProjectPolicy calls get_project_policy and reads the effective dials. A dial the
// plane does not send reads as empty, which the caller treats as "not mine" — the
// safe direction, because it stands down rather than merging.
func (r *mcpReleaser) ProjectPolicy(ctx context.Context) (ProjectPolicy, error) {
	text, err := r.callText(ctx, "get_project_policy", map[string]any{})
	if err != nil {
		return ProjectPolicy{}, err
	}
	var payload struct {
		Policy struct {
			AuthorityIntegrate string `json:"authorityIntegrate"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(text, &payload); err != nil {
		return ProjectPolicy{}, fmt.Errorf("get_project_policy: decode result: %w", err)
	}
	return ProjectPolicy{AuthorityIntegrate: strings.TrimSpace(payload.Policy.AuthorityIntegrate)}, nil
}

// ApprovedForCloseOut lists the project's approved-awaiting-merge tasks through
// `list_tasks(status: approved, view: full)`.
//
// The FULL view is what is asked for because the close-out needs three fields the
// compact listing deliberately omits — the recorded branch, the repo the work lives
// in, and nothing else — and a second `get_task` per candidate would be a wider read
// for a narrower row. The list is capped at closeOutQueueLimit; Total reports how
// many matched, so a caller can see when the cap withheld any.
func (r *mcpReleaser) ApprovedForCloseOut(ctx context.Context) (CloseOutQueue, error) {
	text, err := r.callText(ctx, "list_tasks", map[string]any{
		"status": "approved",
		"view":   "full",
		"limit":  closeOutQueueLimit,
	})
	if err != nil {
		return CloseOutQueue{}, err
	}
	var payload struct {
		Tasks []struct {
			ID     string `json:"id"`
			Ref    string `json:"ref"`
			Title  string `json:"title"`
			Branch string `json:"branch"`
			Repo   string `json:"repo"`
		} `json:"tasks"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(text, &payload); err != nil {
		return CloseOutQueue{}, fmt.Errorf("list_tasks: decode result: %w", err)
	}
	queue := CloseOutQueue{Tasks: make([]CloseOutTask, 0, len(payload.Tasks)), Total: payload.Total}
	for _, t := range payload.Tasks {
		queue.Tasks = append(queue.Tasks, CloseOutTask{
			ID:     strings.TrimSpace(t.ID),
			Ref:    strings.TrimSpace(t.Ref),
			Title:  strings.TrimSpace(t.Title),
			Branch: strings.TrimSpace(t.Branch),
			Repo:   strings.TrimSpace(t.Repo),
		})
	}
	if queue.Total < len(queue.Tasks) {
		queue.Total = len(queue.Tasks)
	}
	return queue, nil
}

// ClaimCloseOut takes one approved task through the plane's close-out door. The
// claim is atomic and first-wins, so it is also the lock that keeps an agent
// close-out and this path from both integrating the same PR; a refusal is the
// "someone else has it" answer and the caller stops.
//
// The returned id is the run CREDENTIAL (`<uuid>.<secret>`), not a bare uuid: every
// later call in this close-out signs with it, exactly as a session's run id does.
func (r *mcpReleaser) ClaimCloseOut(ctx context.Context, taskID string) (string, error) {
	if taskID == "" {
		return "", errors.New("claim close-out: taskId is required")
	}
	text, err := r.callText(ctx, "claim_task", map[string]any{"taskId": taskID})
	if err != nil {
		return "", err
	}
	var payload struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(text, &payload); err != nil {
		return "", fmt.Errorf("claim_task: decode result: %w", err)
	}
	runID := strings.TrimSpace(payload.Run.ID)
	if runID == "" {
		return "", errors.New("claim_task: the plane returned no run id, so nothing can be signed")
	}
	return runID, nil
}

// ReleaseCloseOut hands a close-out claim back, leaving the task APPROVED (the
// plane routes a release by the claim's own approve verdict, so an approved claim
// returns to approved and never to the fresh queue). The driver marker records that
// a driver made the hand-back; checkGreen attests the PR's check was green, which is
// true by construction here (this path only claims what it judged mergeable and
// green) and is what stops the CI-repair ladder counting our own failure to merge
// as a failed repair.
//
// A plane older than one of the markers refuses the field by name; the retry
// without the markers keeps the hand-back itself, which is the load-bearing part.
func (r *mcpReleaser) ReleaseCloseOut(ctx context.Context, taskID, runID string) error {
	if taskID == "" || runID == "" {
		return errors.New("release close-out: taskId and runId are both required")
	}
	err := r.call(ctx, "update_task", map[string]any{
		"taskId":         taskID,
		"runId":          runID,
		"release":        true,
		"driverReleased": true,
		"checkGreen":     true,
	})
	if err == nil {
		return nil
	}
	if refusedField(err, "driverReleased") || refusedField(err, "checkGreen") {
		return r.call(ctx, "update_task", map[string]any{
			"taskId":  taskID,
			"runId":   runID,
			"release": true,
		})
	}
	return err
}

// CompleteCloseOut declares the delivery and closes the task done in the claim's
// run.
//
// No `outcome` is sent, deliberately. The plane judges an outcome only when a call
// ASSERTS one; an absent outcome falls back to the one already stored, which is the
// reviewed text that earned the approval — so the close-out cannot replace evidence
// it never saw. What this adds to the record is the delivery block, including the
// mergeVerified attestation this driver actually ran.
func (r *mcpReleaser) CompleteCloseOut(ctx context.Context, taskID, runID string, d CloseOutDelivery) error {
	if taskID == "" || runID == "" {
		return errors.New("complete close-out: taskId and runId are both required")
	}
	if d.Commit == "" || d.IntegrationBranch == "" {
		return errors.New("complete close-out: the delivery must name a commit and an integration branch")
	}
	delivery := map[string]any{
		"commit":            d.Commit,
		"integrationBranch": d.IntegrationBranch,
		"mergeVerified":     d.MergeVerified,
	}
	if d.PR != "" {
		delivery["pr"] = d.PR
	}
	return r.call(ctx, "update_task", map[string]any{
		"taskId":   taskID,
		"runId":    runID,
		"status":   "done",
		"delivery": delivery,
	})
}
