package loop

// review_map.go — `clankerbar review map` (CLA-975): print the plan a
// maps-session review would send, as JSON, without starting a session. An
// interactive agent sends the turns itself (turn 1 to a fresh reviewer, each
// later turn to that same reviewer), so this command and `review run` build the
// plan through one function: the sequence printed here is the sequence sent
// there.

import (
	"fmt"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// ReviewMapOptions is one `clankerbar review map`. The plan is always the
// maps-session one with embedding off; there is no method, harness or model
// knob because this command starts no session.
type ReviewMapOptions struct {
	Repo        string
	Base        string
	Head        string
	ChunkSize   int
	FlowMax     int
	Title       string
	Description string
}

// ReviewMapItems counts what the plan numbered: the M items, the F items (the
// flow map's overflow note is not an item), and the notes chunks.
type ReviewMapItems struct {
	Change int `json:"change"`
	Flow   int `json:"flow"`
	Chunks int `json:"chunks"`
}

// ReviewMapTurn is one turn of the plan. A notes turn carries its chunk number
// and count; the findings turn carries neither and is always last.
type ReviewMapTurn struct {
	Kind   string `json:"kind"`
	Chunk  int    `json:"chunk,omitempty"`
	Of     int    `json:"of,omitempty"`
	Prompt string `json:"prompt"`
}

// ReviewMapReport is the JSON document `review map` prints. The field names are
// part of the contract the served skill reference cites.
type ReviewMapReport struct {
	Base  string          `json:"base"`
	Head  string          `json:"head"`
	Items ReviewMapItems  `json:"items"`
	Turns []ReviewMapTurn `json:"turns"`
}

// The turn kinds, as the skill reference cites them.
const (
	reviewMapNotesKind    = "notes"
	reviewMapFindingsKind = "findings"
)

// ReviewMap builds the plan `review run --method maps-session` would send for
// repo@base..head and returns it as the document the skill reference reads. It
// starts no harness session, loads no config and touches no network: it works
// on a machine with no clankerbar config.
//
// Unlike RunReview it reads Repo as given rather than checking out a temporary
// worktree — the reviewer subagent must work in the checkout the map came from
// — so the checkout has to be at Head: the flow map's `git grep` reads the
// checked-out tree. The repository is never modified.
func ReviewMap(opts ReviewMapOptions) (*ReviewMapReport, error) {
	repo, err := reviewRunRepo(opts.Repo)
	if err != nil {
		return nil, err
	}
	// The flow map's git grep runs in this directory and reports paths relative to it, while the
	// map's definitions carry repository-relative paths: from a subdirectory it would both miss uses
	// outside the subtree and fail to drop a definition's own line. Map from the checkout root even
	// when --repo names a subdirectory, so this stays the plan review run maps from its worktree root.
	if repo, err = reviewMapRoot(repo); err != nil {
		return nil, err
	}
	base, err := reviewRunCommit(repo, "base", opts.Base)
	if err != nil {
		return nil, err
	}
	headRev := strings.TrimSpace(opts.Head)
	if headRev == "" {
		headRev = "HEAD"
	}
	head, err := reviewRunCommit(repo, "head", headRev)
	if err != nil {
		return nil, err
	}
	// The diff is revision-exact, but the flow map's `git grep` reads the CHECKED-OUT tree.
	at, err := gitHeadSHA(repo)
	if err != nil {
		return nil, fmt.Errorf("read the checkout's HEAD: %w", err)
	}
	if at != head {
		return nil, fmt.Errorf("review map needs the checkout at the reviewed head %s, but %s is at %s: the flow map's git grep reads the checked-out tree, so a map built here would list uses from the wrong revision", head, repo, at)
	}
	// No model: the plan's prompts do not depend on one for the taskless review ending.
	plan, err := reviewMapsPlan(repo, base, head, "", ReviewRunOptions{
		ChunkSize: opts.ChunkSize,
		FlowMax:   opts.FlowMax,
	})
	if err != nil {
		return nil, err
	}
	report := &ReviewMapReport{
		Base: base,
		Head: head,
		Items: ReviewMapItems{
			Change: plan.changeItems,
			Flow:   plan.flowItems,
			Chunks: plan.notesTurns,
		},
		Turns: make([]ReviewMapTurn, 0, plan.notesTurns+1),
	}
	report.Turns = append(report.Turns, ReviewMapTurn{
		Kind:   reviewMapNotesKind,
		Chunk:  1,
		Of:     plan.notesTurns,
		Prompt: reviewMapFirstTurn(base, head, opts.Title, opts.Description, plan),
	})
	// plan.turns is the follow-up turns: notes turns 2..n then the findings turn, which is last.
	for i, turn := range plan.turns {
		if i == len(plan.turns)-1 {
			report.Turns = append(report.Turns, ReviewMapTurn{Kind: reviewMapFindingsKind, Prompt: turn.Prompt})
			continue
		}
		report.Turns = append(report.Turns, ReviewMapTurn{
			Kind:   reviewMapNotesKind,
			Chunk:  i + 2,
			Of:     plan.notesTurns,
			Prompt: turn.Prompt,
		})
	}
	return report, nil
}

// reviewMapRoot resolves a checkout directory to its worktree root. git grep is
// scoped to and prints paths relative to its working directory, so the root,
// not whatever directory --repo happens to name, is what the flow map has to be
// built from.
func reviewMapRoot(repo string) (string, error) {
	out, err := reviewGit(repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("resolve --repo %s to its checkout root: %w%s", repo, err, reviewGitDetail(out))
	}
	root := strings.TrimSpace(out)
	if root == "" {
		return "", fmt.Errorf("resolve --repo %s to its checkout root: git named none", repo)
	}
	return root, nil
}

// reviewMapFirstTurn is turn 1's prompt: the standalone brief with the plan's
// section appended, the composition wireMapsPlan applies to `review run`'s
// invocation.
func reviewMapFirstTurn(base, head, title, description string, plan *mapsSessionPlan) string {
	brief := config.ReviewRunBrief(config.ReviewRunBriefOptions{
		Base:        base,
		Head:        head,
		Title:       title,
		Description: description,
	})
	return brief + "\n\n" + plan.section
}
