package loop

// maps_session.go — the review phase's maps-session method (CLA-869).
//
// The method is the runner's, not the model's: the driver maps the change into
// items (CLA-866/CLA-868), sends them in chunks, and the SAME opencode session
// writes notes per item before a final findings turn turns those notes into the
// review record (CLA-864). The brief is unchanged — the maps section is appended
// to it — so the single-pass review is untouched and only a phase that names
// the method takes this path.

import (
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/reviewmap"
)

// mapsSessionPlan is everything one maps-session review adds to an attempt's
// invocation: the section appended to the first turn's brief, the follow-up
// turns (the remaining chunks, then the findings turn), and the model the
// findings turn runs on (the record's attribution).
type mapsSessionPlan struct {
	// notesTurns counts the NOTES turns, including the first. TurnMessages
	// before this index are notes and go to the iteration log.
	notesTurns int
	// section is appended to the first turn's prompt, after the brief.
	section string
	// turns are the follow-up turns: notes turns 2..notesTurns, then findings.
	turns []harness.Turn
	// notesModel is the model the notes turns run on (resolved: the configured
	// stage model, else the phase's).
	notesModel string
	// reportModel is the model the findings turn runs on: what the review record
	// must name, since it is the turn that wrote the findings.
	reportModel string
}

// mapsSessionRequested reports whether this attempt is a review phase that asked
// for the maps-session method.
//
// The identity comes from the CAPTURED prompt: a handoff respawn replaces
// ph.Prompt with the predecessor's continuation, and that successor is not the
// phase that maps a diff — it continues work the predecessor already read.
func (r *phaseRun) mapsSessionRequested() bool {
	if !r.ph.UsesMapsSession() {
		return false
	}
	if r.shape.captured.Prompt != r.ph.Prompt {
		return false
	}
	return config.ReviewShaped(r.shape.captured.Prompt)
}

// mapsSessionEnding is the last sentence of the findings turn: the record the
// phase's own brief asks for. A runner-claimed review ends with a done report;
// a panel member writes the durable record itself.
func (r *phaseRun) mapsSessionEnding() string {
	if r.kind == reportNone {
		return config.MapsSessionRecordEnding
	}
	return config.MapsSessionDoneReportEnding
}

// mapsSessionPlanFor builds this attempt's maps-session plan, or returns nil
// when the phase did not ask for the method. Every failure is a refusal to have
// the phase run at all: a half-built map would make the reviewer's checklist
// silently wrong, which is the one thing this method must not do.
func (r *phaseRun) mapsSessionPlanFor(at *phaseAttempt) (*mapsSessionPlan, error) {
	if !r.ph.UsesMapsSession() {
		return nil, nil
	}
	if !r.mapsSessionRequested() {
		log.Printf("%siteration %d: the %s phase runs a handoff continuation, so its maps-session turns are not applied — the successor continues the work the predecessor already read",
			labelOf(r.t), r.drainNum, r.phaseLabel())
		return nil, nil
	}
	// The maps are built from the tree the session will read, and that tree has
	// to be the reviewed tip. Only the runner's prepared worktree (or a parallel
	// panel member's detached checkout) is: a session-claimed review starts in
	// the repo's own checkout, which may sit on any branch — a map from it would
	// be a map of the wrong diff, presented as the change's checklist.
	if r.opts.taskDir == "" && r.opts.worktree == "" {
		return nil, errors.New("review_method maps-session builds its maps in the task worktree the runner prepares, and this phase has none: it is a session-claimed run (no runner-owned worktree) or a branchless checkpoint (nothing to map)")
	}
	repo := at.inv.WorkDir
	if repo == "" {
		repo = r.spawnDir
	}
	// The merge-base the brief diffs from, resolved exactly as invocationFor
	// resolves it: a runner briefing is authoritative when there is one (even
	// with an empty SHA), and a parallel panel member carries it in opts.
	baseSHA := r.opts.baseSHA
	if r.opts.runner != nil {
		baseSHA = r.opts.runner.baseSHA
	}
	base := diffBase(r.prBase, baseSHA)
	if base == "" {
		return nil, errors.New("review_method maps-session needs a diff base (the runner's merge-base, or the repo's PR base), and neither is known")
	}
	head := r.checkpointTip()
	if head == "" {
		// A checkpoint with no verified tip still has a tree: the runner's prep
		// left the worktree at the branch's origin tip, and HEAD is that tree.
		head = "HEAD"
	}
	// The diff is revision-exact, but the flow map's `git grep` reads the
	// CHECKED-OUT tree. A worktree parked on another revision would list uses
	// from code the review is not of, so refuse rather than present that as the
	// change's checklist.
	if head != "HEAD" {
		if at, err := gitHeadSHA(repo); err != nil {
			return nil, fmt.Errorf("review_method maps-session could not read the worktree's HEAD: %w", err)
		} else if at != head {
			return nil, fmt.Errorf("review_method maps-session needs the worktree at the reviewed tip %s, but it is at %s: the flow map's git grep reads the checked-out tree, so a map built here would list uses from the wrong revision", head, at)
		}
	}
	rng := reviewmap.Range{Repo: repo, Base: base, Head: head}
	diff, err := reviewmap.Diff(rng)
	if err != nil {
		return nil, fmt.Errorf("review_method maps-session could not read the change: %w", err)
	}
	opts := r.ph.MapsSessionDefaults()
	flow, err := reviewmap.FlowMap(diff, rng, opts.FlowMax)
	if err != nil {
		return nil, fmt.Errorf("review_method maps-session could not map the changed definitions: %w", err)
	}
	return planMapsSession(opts, at.inv.Model, base, diff, flow, r.mapsSessionEnding()), nil
}

// gitHeadSHA is the commit the worktree has checked out.
func gitHeadSHA(repo string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// planMapsSession turns a diff and its flow items into the plan: numbered items,
// chunks, the first-turn section and the follow-up turns. Pure, so the
// numbering, chunking and per-stage models are testable without a repository.
func planMapsSession(opts config.MapsSessionOptions, phaseModel, base, diff string, flow []string, ending string) *mapsSessionPlan {
	items, overflow := mapsSessionItems(reviewmap.ChangeMap(diff, reviewmap.Unlimited), flow)
	chunks := mapsSessionChunks(items, opts.ChunkSize)
	bodies := make([]string, len(chunks))
	for i, chunk := range chunks {
		bodies[i] = strings.Join(chunk, "\n")
	}
	// The cap's note is not an item: it is context for the reviewer, so it rides
	// the last chunk unnumbered rather than getting an id to note.
	if overflow != "" && len(bodies) > 0 {
		bodies[len(bodies)-1] += "\n" + overflow
	}

	notesModel := opts.NotesModel
	if notesModel == "" {
		notesModel = phaseModel
	}
	reportModel := opts.FindingsModel
	if reportModel == "" {
		reportModel = phaseModel
	}

	plan := &mapsSessionPlan{
		notesTurns:  len(chunks),
		section:     config.MapsSessionFirstTurn(base, 1, len(chunks), bodies[0]),
		notesModel:  notesModel,
		reportModel: reportModel,
	}
	for i := 1; i < len(chunks); i++ {
		plan.turns = append(plan.turns, harness.Turn{Prompt: config.MapsSessionNotesTurn(i+1, len(chunks), bodies[i])})
	}
	findings := harness.Turn{Prompt: config.MapsSessionFindingsTurn(ending)}
	// A blank Turn.Model means "inherit the session's current model", which after
	// the notes turns is the notes model. Name the findings model whenever that
	// inheritance would land somewhere else — including when the phase names no
	// model and only the notes stage did. There is no per-turn way back to a
	// harness default inside a live session, so that one combination runs the
	// findings turn on the notes model; the README says so.
	if opts.FindingsModel != "" || reportModel != notesModel {
		findings.Model = reportModel
	}
	plan.turns = append(plan.turns, findings)
	return plan
}

// mapsSessionItems numbers the change map M1.. and the flow map F1.. in that
// order. The flow map's cap note is returned apart, because it is not an item to
// note.
func mapsSessionItems(changes, flow []string) (items []string, overflow string) {
	items = make([]string, 0, len(changes)+len(flow))
	for i, item := range changes {
		items = append(items, fmt.Sprintf("M%d. %s", i+1, item))
	}
	n := 0
	for _, item := range flow {
		if reviewmap.IsOverflowItem(item) {
			overflow = item
			continue
		}
		n++
		items = append(items, fmt.Sprintf("F%d. %s", n, item))
	}
	return items, overflow
}

// mapsSessionChunks splits items into chunks of size. A change with no map items
// still gets ONE chunk, carrying the sentence the model is told about it —
// otherwise there would be no notes turns at all and the findings turn would ask
// it to turn notes it never wrote into findings.
func mapsSessionChunks(items []string, size int) [][]string {
	if len(items) == 0 {
		return [][]string{{"(no changed regions found)"}}
	}
	if size < 1 {
		size = config.DefaultMapsSessionChunkSize
	}
	chunks := make([][]string, 0, (len(items)+size-1)/size)
	for start := 0; start < len(items); start += size {
		end := min(start+size, len(items))
		chunks = append(chunks, items[start:end])
	}
	return chunks
}

// applyMapsSession wires a plan onto the attempt's invocation: the first-turn
// section, the turn-1 model and the follow-up turns. It also records how many
// notes turns the log has to capture.
func (r *phaseRun) applyMapsSession(at *phaseAttempt, plan *mapsSessionPlan) {
	if plan == nil {
		return
	}
	at.mapsNotes = plan.notesTurns
	// Turn 1 is the phase's own invocation, so a notes-stage model has to be
	// pinned here, by the same rule a follow-up turn's own model follows: an
	// explicit stage model is a single route, never a fallback list.
	if plan.notesModel != at.inv.Model {
		at.inv = at.inv.OnTurn(harness.Turn{Model: plan.notesModel})
	}
	at.inv.Prompt += "\n\n" + plan.section
	at.inv.Turns = plan.turns
	// The record is the findings turn's work, so the attribution the runner
	// writes names that stage's model — the one whose output it is.
	at.attr.Model = plan.reportModel
}

// writeMapsNotes puts every notes turn's reply in the iteration log under a
// heading naming the turn and chunk. The notes are the evidence a later
// missed-finding analysis reads; they are never sent to the plane, where they
// would be mistaken for findings.
func (r *phaseRun) writeMapsNotes(at *phaseAttempt) {
	if at.mapsNotes == 0 || at.inv.Console == nil {
		return
	}
	n := min(at.mapsNotes, len(at.res.TurnMessages))
	for i := 0; i < n; i++ {
		fmt.Fprintf(at.inv.Console, "\n===== maps-session notes: turn %d of %d (chunk %d of %d) =====\n%s\n",
			i+1, at.mapsNotes, i+1, at.mapsNotes, at.res.TurnMessages[i])
	}
	if len(at.res.TurnMessages) <= at.mapsNotes {
		fmt.Fprintf(at.inv.Console, "\n!! maps-session: the findings turn produced no message (turns run: %d of %d planned)\n",
			len(at.res.TurnMessages), at.mapsNotes+1)
	}
}
