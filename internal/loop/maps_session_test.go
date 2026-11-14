package loop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// mapsTsDiff is a two-hunk TypeScript diff: two changed regions and one added
// definition, i.e. M1/M2 and F1.
const mapsTsDiff = `diff --git a/src/a.ts b/src/a.ts
index 1111111..2222222 100644
--- a/src/a.ts
+++ b/src/a.ts
@@ -10,3 +10,3 @@
 export function alphaThing(x: number) {
-  return x + 1
+  return x + 2
 }
@@ -40,3 +40,7 @@
 export function gammaThing(x: number) {
   return x
 }
+
+export function betaThing(x: number) {
+  return x * 2
+}
`

func mapsTestOpts() config.MapsSessionOptions {
	return config.MapsSessionOptions{ChunkSize: 2, FlowMax: config.DefaultMapsSessionFlowMax}.Resolved()
}

// TestPlanMapsSession_NumbersAndChunks (CLA-869): the change map is numbered
// M1.. before the flow map's F1.., chunked by chunk_size, and the follow-up turns
// are the remaining chunks plus the findings turn.
func TestPlanMapsSession_NumbersAndChunks(t *testing.T) {
	// ChangeMap on the fixture yields 2 hunks; the flow items are supplied
	// directly so the numbering and cap behaviour are tested without git.
	flow := []string{
		"`betaThing` defined or changed at src/a.ts:43; used at: src/b.ts:9",
		"`alphaThing` defined or changed at src/a.ts:10 (body changed); used at: src/b.ts:2",
	}
	plan := planMapsSession(mapsTestOpts(), "phase-model", "abc1234", mapsTsDiff, flow, config.MapsSessionSeverityTask, config.MapsSessionDoneReportEnding)

	// 2 M items + 2 F items at 2 per chunk = 2 notes turns.
	if plan.notesTurns != 2 {
		t.Fatalf("notesTurns = %d, want 2 (4 items at chunk_size 2)", plan.notesTurns)
	}
	if len(plan.turns) != 2 {
		t.Fatalf("follow-up turns = %d, want 2 (one notes turn and the findings turn)", len(plan.turns))
	}
	first := plan.section
	for _, want := range []string{
		"## How this review works", "## Checklist", "## Chunk 1 of 2",
		"M1. src/a.ts:10-12", "M2. src/a.ts:40-46",
		"`git diff abc1234..HEAD -- <path>`", "Do NOT write a done report",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("turn 1's section does not carry %q:\n%s", want, first)
		}
	}
	if strings.Contains(first, "F1.") {
		t.Errorf("turn 1 carries a flow item from chunk 2:\n%s", first)
	}
	second := plan.turns[0].Prompt
	for _, want := range []string{"## Chunk 2 of 2", "F1. `betaThing`", "F2. `alphaThing`", "Same instructions as before"} {
		if !strings.Contains(second, want) {
			t.Errorf("turn 2 does not carry %q:\n%s", want, second)
		}
	}
	findings := plan.turns[1].Prompt
	for _, want := range []string{"turn your notes into findings", "Severity: MUST", "done report"} {
		if !strings.Contains(findings, want) {
			t.Errorf("the findings turn does not carry %q:\n%s", want, findings)
		}
	}
}

// TestPlanMapsSession_NoItemsStillOneChunk (CLA-869): a change with no map items
// still gets one notes turn carrying the sentence, so the findings turn never
// asks a session to turn notes it never wrote into findings.
func TestPlanMapsSession_NoItemsStillOneChunk(t *testing.T) {
	plan := planMapsSession(mapsTestOpts(), "", "", "", nil, config.MapsSessionSeverityTask, config.MapsSessionDoneReportEnding)
	if plan.notesTurns != 1 {
		t.Fatalf("notesTurns = %d, want 1", plan.notesTurns)
	}
	if !strings.Contains(plan.section, "(no changed regions found)") {
		t.Errorf("the empty map's turn 1 does not say so:\n%s", plan.section)
	}
	if !strings.Contains(plan.section, "## Chunk 1 of 1") {
		t.Errorf("the empty map's turn 1 does not number its one chunk:\n%s", plan.section)
	}
	if len(plan.turns) != 1 || !strings.Contains(plan.turns[0].Prompt, "turn your notes into findings") {
		t.Fatalf("turns = %d, want only the findings turn", len(plan.turns))
	}
	if !strings.Contains(plan.section, "git diff <base>..HEAD") {
		t.Errorf("an unknown base was not left as the placeholder:\n%s", plan.section)
	}
}

// TestPlanMapsSession_SeverityMatchesTheReviewContract (CLA-870 round 1
// 1.review-1.2): a phase review grades MUST against its task's done-when bar,
// while `clankerbar review run` has no task and its findings turn must not name
// a bar the brief says does not exist.
func TestPlanMapsSession_SeverityMatchesTheReviewContract(t *testing.T) {
	task := planMapsSession(mapsTestOpts(), "", "", "", nil, config.MapsSessionSeverityTask, config.MapsSessionDoneReportEnding)
	taskFindings := task.turns[len(task.turns)-1].Prompt
	if !strings.Contains(taskFindings, "violates the task's done-when bar") {
		t.Errorf("the phase's findings turn lost its done-when clause:\n%s", taskFindings)
	}

	taskless := planMapsSession(mapsTestOpts(), "", "", "", nil, config.MapsSessionSeverityTaskless, config.MapsSessionDoneReportEnding)
	tasklessFindings := taskless.turns[len(taskless.turns)-1].Prompt
	if strings.Contains(tasklessFindings, "done-when") {
		t.Errorf("the taskless findings turn names a done-when bar it does not have:\n%s", tasklessFindings)
	}
	if !strings.Contains(tasklessFindings, "violates the repo's gates, or correctness/safety") {
		t.Errorf("the taskless findings turn does not state the remaining MUST bar:\n%s", tasklessFindings)
	}
}

// TestPlanMapsSession_StageModels (CLA-869): a stage model that is set runs its
// stage and nothing else; an empty one inherits the phase's model.
func TestPlanMapsSession_StageModels(t *testing.T) {
	cases := []struct {
		name         string
		opts         config.MapsSessionOptions
		phaseModel   string
		wantNotes    string
		wantReport   string
		wantTurnFind string // the findings turn's own Model, blank = inherit
	}{
		{name: "neither set inherits the phase", opts: config.MapsSessionOptions{ChunkSize: 25}, phaseModel: "base", wantNotes: "base", wantReport: "base"},
		{name: "notes only", opts: config.MapsSessionOptions{ChunkSize: 25, NotesModel: "notes"}, phaseModel: "base", wantNotes: "notes", wantReport: "base", wantTurnFind: "base"},
		{name: "findings only", opts: config.MapsSessionOptions{ChunkSize: 25, FindingsModel: "findings"}, phaseModel: "base", wantNotes: "base", wantReport: "findings", wantTurnFind: "findings"},
		{name: "both", opts: config.MapsSessionOptions{ChunkSize: 25, NotesModel: "notes", FindingsModel: "findings"}, phaseModel: "base", wantNotes: "notes", wantReport: "findings", wantTurnFind: "findings"},
		{name: "same model on both stages", opts: config.MapsSessionOptions{ChunkSize: 25, NotesModel: "same", FindingsModel: "same"}, phaseModel: "base", wantNotes: "same", wantReport: "same", wantTurnFind: "same"},
		// The documented edge: with no phase model, a findings stage that names
		// nothing cannot be returned to the harness default inside a live
		// session, so it inherits the notes model — and the record must name that
		// model, since it is the one that produced the findings.
		{name: "notes named, phase model absent", opts: config.MapsSessionOptions{ChunkSize: 25, NotesModel: "notes"}, wantNotes: "notes", wantReport: "notes", wantTurnFind: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := planMapsSession(tc.opts.Resolved(), tc.phaseModel, "", "", nil, config.MapsSessionSeverityTask, config.MapsSessionDoneReportEnding)
			if plan.notesModel != tc.wantNotes {
				t.Errorf("notes model = %q, want %q", plan.notesModel, tc.wantNotes)
			}
			if plan.reportModel != tc.wantReport {
				t.Errorf("report model = %q, want %q", plan.reportModel, tc.wantReport)
			}
			if got := plan.turns[len(plan.turns)-1].Model; got != tc.wantTurnFind {
				t.Errorf("findings turn model = %q, want %q", got, tc.wantTurnFind)
			}
			for i, turn := range plan.turns[:len(plan.turns)-1] {
				if turn.Model != "" {
					t.Errorf("notes turn %d names model %q; the notes turns must inherit turn 1's model", i+2, turn.Model)
				}
			}
		})
	}
}

// TestPlanMapsSession_RecordNamesTheFindingsStageModel (CLA-869 round 1
// 1.review-1.3): a phase that writes its own record had its brief's `model` and
// `raisedByModel` placeholders filled from the PHASE's attribution before the
// stage models were resolved, so a findings stage on another model would sign
// findings it did not produce. The findings turn names the real model instead.
func TestPlanMapsSession_RecordNamesTheFindingsStageModel(t *testing.T) {
	cases := []struct {
		name       string
		opts       config.MapsSessionOptions
		phaseModel string
		wantModel  string // "" = the clause is not needed
	}{
		{name: "findings model differs", opts: config.MapsSessionOptions{ChunkSize: 25, FindingsModel: "findings-model"}, phaseModel: "base-model", wantModel: "findings-model"},
		{name: "notes only, no phase model", opts: config.MapsSessionOptions{ChunkSize: 25, NotesModel: "notes-model"}, wantModel: "notes-model"},
		{name: "findings inherits the phase", opts: config.MapsSessionOptions{ChunkSize: 25}, phaseModel: "base-model"},
		{name: "neither names a model", opts: config.MapsSessionOptions{ChunkSize: 25}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := planMapsSession(tc.opts.Resolved(), tc.phaseModel, "", "", nil, config.MapsSessionSeverityTask, config.MapsSessionRecordEnding)
			last := plan.turns[len(plan.turns)-1].Prompt
			if tc.wantModel == "" {
				if strings.Contains(last, "raisedByModel") {
					t.Errorf("the findings turn overrides the record's model though the brief already names it:\n%s", last)
				}
				return
			}
			for _, want := range []string{"raisedByModel", `"` + tc.wantModel + `"`} {
				if !strings.Contains(last, want) {
					t.Errorf("the panel member's findings turn does not name %q for the record:\n%s", want, last)
				}
			}
			// A runner-claimed review's record is written by the runner from the
			// stage model; its done report owes no model sentence.
			done := planMapsSession(tc.opts.Resolved(), tc.phaseModel, "", "", nil, config.MapsSessionSeverityTask, config.MapsSessionDoneReportEnding)
			if strings.Contains(done.turns[len(done.turns)-1].Prompt, "raisedByModel") {
				t.Errorf("the done-report ending was given the record-model override:\n%s", done.turns[len(done.turns)-1].Prompt)
			}
		})
	}
}

// TestPlanMapsSession_CapNoteIsNotAnItem (CLA-869): the flow map's "more not
// listed" note is context, not a definition, so it gets no id and rides the last
// chunk unnumbered.
func TestPlanMapsSession_CapNoteIsNotAnItem(t *testing.T) {
	flow := []string{
		"`betaThing` defined or changed at src/a.ts:43; used at: nowhere else",
		"(3 more changed definitions not listed)",
	}
	plan := planMapsSession(config.MapsSessionOptions{ChunkSize: 25}.Resolved(), "", "", mapsTsDiff, flow, config.MapsSessionSeverityTask, config.MapsSessionDoneReportEnding)
	if strings.Contains(plan.section, "F2.") {
		t.Errorf("the cap note was numbered as an item:\n%s", plan.section)
	}
	if !strings.Contains(plan.section, "(3 more changed definitions not listed)") {
		t.Errorf("the cap note is missing from the turn:\n%s", plan.section)
	}
}

// writeMapsNotes renders the replies it is given, with the turn and chunk
// headings the missed-finding analysis reads.
func TestWriteMapsNotes_Headings(t *testing.T) {
	var log strings.Builder
	at := &phaseAttempt{
		mapsNotes: 3,
		inv:       harness.Invocation{Console: &log},
		res: harness.Result{TurnMessages: []string{
			"M1: fine", "M2: looks wrong",
			"F1: fine",
			`{"verdict":"findings"}`,
		}},
	}
	(&phaseRun{}).writeMapsNotes(at)
	out := log.String()
	for _, want := range []string{
		"maps-session notes: turn 1 of 3 (chunk 1 of 3)", "M2: looks wrong",
		"maps-session notes: turn 2 of 3 (chunk 2 of 3)", "F1: fine",
		"maps-session notes: turn 3 of 3 (chunk 3 of 3)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the notes log does not carry %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `{"verdict":"findings"}`) {
		t.Errorf("the findings turn's report was logged as notes:\n%s", out)
	}
	if strings.Contains(out, "findings turn produced no message") {
		t.Errorf("a completed sequence must not be reported as a missing findings turn:\n%s", out)
	}

	// An early stop at a notes turn logs what ran and says the findings turn
	// never did.
	var stopped strings.Builder
	at.inv.Console = &stopped
	at.res.TurnMessages = []string{"M1: fine", "M2: stopped"}
	(&phaseRun{}).writeMapsNotes(at)
	if !strings.Contains(stopped.String(), "findings turn produced no message") {
		t.Errorf("an early stop was not named in the log:\n%s", stopped.String())
	}
}

// mapsRepo builds a repository with a base commit and a change commit, and
// returns the worktree, the base SHA and the tip SHA. The change touches two
// regions and adds one definition, so the map has both M and F items.
func mapsRepo(t *testing.T) (dir, base, tip string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	var b strings.Builder
	b.WriteString("export function alphaThing(x: number) {\n  return x + 1\n}\n")
	for i := 0; i < 30; i++ {
		b.WriteString("// filler line\n")
	}
	b.WriteString("export function gammaThing(x: number) {\n  return x\n}\n")
	path := filepath.Join(dir, "a.ts")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write a.ts: %v", err)
	}
	run("add", ".")
	run("commit", "-qm", "base")
	base = run("rev-parse", "HEAD")

	changed := strings.Replace(b.String(), "return x + 1", "return x + 2", 1)
	changed += "\nexport function betaThing(x: number) {\n  return x * 2\n}\n"
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatalf("write a.ts: %v", err)
	}
	run("add", ".")
	run("commit", "-qm", "change")
	tip = run("rev-parse", "HEAD")
	return dir, base, tip
}

// TestMapsSessionPlanFor_BuildsFromTheMergeBaseDiff (CLA-869): the map is built
// from <mergeBase>..<tip> in the task worktree, numbered M then F, and chunked.
func TestMapsSessionPlanFor_BuildsFromTheMergeBaseDiff(t *testing.T) {
	repo, base, tip := mapsRepo(t)
	ph := config.Phase{
		Name:         config.ReviewPhaseName,
		Prompt:       config.ReviewOnlyBrief(),
		ReviewMethod: config.ReviewMethodMapsSession,
		MapsSession:  &config.MapsSessionOptions{ChunkSize: 25},
	}
	r := &phaseRun{
		ph:     ph,
		shape:  phaseShape{captured: ph},
		opts:   phaseRunOpts{taskDir: repo, runner: &runnerBriefing{baseSHA: base}},
		prev:   &harness.Result{Claim: harness.Claim{TaskID: "t-1", RunID: "r-1"}, CheckpointTip: tip},
		prBase: "main",
	}
	at := &phaseAttempt{inv: harness.Invocation{WorkDir: repo}}
	plan, err := r.mapsSessionPlanFor(at)
	if err != nil {
		t.Fatalf("mapsSessionPlanFor: %v", err)
	}
	if plan == nil {
		t.Fatal("no plan for a maps-session review phase")
	}
	if !strings.Contains(plan.section, "M1. a.ts:") {
		t.Errorf("turn 1 has no numbered change item:\n%s", plan.section)
	}
	if !strings.Contains(plan.section, "F1. `betaThing` defined or changed at a.ts:") {
		t.Errorf("turn 1 has no numbered flow item for the added definition:\n%s", plan.section)
	}
	if strings.Contains(plan.section, "chunk 1 of 0") || !strings.Contains(plan.section, "## Chunk 1 of 1") {
		t.Errorf("the fixture's items were not one chunk at chunk_size 25:\n%s", plan.section)
	}
	// Every item is numbered exactly once, M before F.
	// The fixture has two hunks: the alpha body change and the appended beta.
	if strings.Count(plan.section, "M1. ") != 1 || strings.Count(plan.section, "M2. ") != 1 {
		t.Errorf("change items are not the fixture's two hunks:\n%s", plan.section)
	}
}

// mapsDrainRun is the review phase of a runner-claimed drain, ready to spawn
// against repo. The driver is the standard phase fixture with a plane that holds
// a live run, so the review gate's baseline reads.
func mapsDrainRun(t *testing.T, h harness.Adapter, phases []config.Phase, repo, base, tip string) (*Driver, *reviewReleaser, *harness.Result, phaseRunOpts) {
	t.Helper()
	rel := &reviewReleaser{status: "in_progress", holder: "r-1", outcome: "implemented\n"}
	d := phaseDriverWithReleaser(t, h, phases, rel)
	d.cfg.WorkDir = repo
	prev := &harness.Result{
		ExitCode:      0,
		Claim:         harness.Claim{TaskID: "t-1", RunID: "r-1", Branch: "clanker/x", HasWIP: true},
		CheckpointTip: tip,
	}
	opts := phaseRunOpts{
		taskDir: repo,
		runner:  &runnerBriefing{task: plane.TaskBrief{ID: "t-1", Ref: "CB-1", Branch: "clanker/x"}, runID: "r-1", baseSHA: base},
	}
	return d, rel, prev, opts
}

// mapsReviewPhases is [implement, review, fix] with the review on maps-session
// and a chunk size that makes the fixture at least two notes turns.
func mapsReviewPhases() []config.Phase {
	return []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName, ReviewMethod: config.ReviewMethodMapsSession},
		{Name: config.FixPhaseName},
	}
}

func TestMapsSessionDrain_WiresOneInvocationWithEveryTurn(t *testing.T) {
	repo, base, tip := mapsRepo(t)
	phases := mapsReviewPhases()
	phases[1].MapsSession = &config.MapsSessionOptions{ChunkSize: 2, NotesModel: "notes-model", FindingsModel: "findings-model"}
	h := &fakeAdapter{steps: []invokeStep{{res: harness.Result{
		ExitCode: 0,
		Claim:    harness.Claim{TaskID: "t-1", RunID: "r-1", Branch: "clanker/x", HasWIP: true},
		TurnMessages: []string{
			"M1: fine",
			"F1: betaThing takes an unused argument",
			"the findings turn's report",
		},
	}}}}
	d, rel, prev, opts := mapsDrainRun(t, h, phases, repo, base, tip)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	effective := d.cfg.EffectivePhases()
	_, _, _, _, err := d.drainPhase(ctx, 1, 0, 1, "-p2", effective[1], true, prev, d.targets[0],
		spend{start: time.Now()}, effective, panelState{}, opts)
	if err != nil {
		t.Fatalf("drainPhase: %v", err)
	}
	if h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want ONE invocation carrying every turn", h.invokeCalls)
	}
	inv := h.invocations[0]
	// The fixture's diff has 2 hunks + 2 flow items = 4 items at chunk_size 2, so
	// 2 notes turns (turn 1 carries chunk 1) plus the findings turn.
	if len(inv.Turns) != 2 {
		t.Fatalf("follow-up turns = %d, want 2 (1 notes turn + findings)", len(inv.Turns))
	}
	for _, want := range []string{"## How this review works", "## Chunk 1 of 2", "M1. "} {
		if !strings.Contains(inv.Prompt, want) {
			t.Errorf("turn 1's prompt does not carry %q:\n%s", want, inv.Prompt)
		}
	}
	if !strings.Contains(inv.Prompt, "REVIEW ONLY") {
		t.Errorf("the review brief was replaced instead of extended:\n%s", inv.Prompt)
	}
	if !strings.Contains(inv.Turns[0].Prompt, "## Chunk 2 of 2") {
		t.Errorf("the notes follow-up does not carry chunk 2:\n%s", inv.Turns[0].Prompt)
	}
	if !strings.Contains(inv.Turns[1].Prompt, "turn your notes into findings") {
		t.Errorf("the last turn is not the findings turn:\n%s", inv.Turns[1].Prompt)
	}
	// Per-stage models: turn 1 is the notes stage's first turn, notes follow-ups
	// inherit it, and the findings turn names its own.
	if inv.Model != "notes-model" {
		t.Errorf("turn 1 model = %q, want the notes stage's notes-model", inv.Model)
	}
	if inv.Turns[0].Model != "" {
		t.Errorf("the notes follow-up names model %q; it must inherit turn 1's", inv.Turns[0].Model)
	}
	if inv.Turns[1].Model != "findings-model" {
		t.Errorf("findings turn model = %q, want findings-model", inv.Turns[1].Model)
	}
	// Only the final turn is a done report: the check refuses a notes reply and
	// accepts the findings turn's report.
	if inv.CheckReport == nil {
		t.Fatal("the review phase lost its done-report check")
	}
	if err := inv.CheckReport("M1: fine"); err == nil {
		t.Error("a notes reply passed the done-report check")
	}
	report := `{"verdict":"pass","reviewedTip":"` + tip + `"}`
	if err := inv.CheckReport(config.DoneMarker + "\n" + report); err != nil {
		t.Errorf("the findings turn's report was refused: %v", err)
	}
	// The notes are in the iteration log, under their headings; the findings
	// report is not.
	logs := iterationLogs(t, d.state.Path())
	var logText string
	for _, name := range logs {
		data, rerr := os.ReadFile(filepath.Join(d.state.Path(), name))
		if rerr == nil {
			logText += string(data)
		}
	}
	for _, want := range []string{"maps-session notes: turn 1 of 2 (chunk 1 of 2)", "M1: fine", "maps-session notes: turn 2 of 2 (chunk 2 of 2)", "F1: betaThing takes an unused argument"} {
		if !strings.Contains(logText, want) {
			t.Errorf("the iteration log does not carry %q:\n%s", want, logText)
		}
	}
	if strings.Contains(logText, "the findings turn's report") {
		t.Errorf("the findings report was logged as notes:\n%s", logText)
	}
	if len(rel.records) != 0 {
		t.Errorf("a report took the review path with no done report behind it, yet %d record(s) landed", len(rel.records))
	}
}

// TestMapsSessionDrain_BadTurnFailsClosed (CLA-869): a turn that ends badly ends
// the phase exactly as a failed single-pass review does — the run fails, nothing
// is recorded, and no later turn or phase runs.
func TestMapsSessionDrain_BadTurnFailsClosed(t *testing.T) {
	repo, base, tip := mapsRepo(t)
	phases := mapsReviewPhases()
	phases[1].MapsSession = &config.MapsSessionOptions{ChunkSize: 1}
	bad := harness.Result{
		ExitCode:       1,
		Claim:          harness.Claim{TaskID: "t-1", RunID: "r-1", Branch: "clanker/x", HasWIP: true},
		TurnStopped:    2,
		TurnStopReason: "nonzero_exit",
		Raw:            map[string]any{"kind": "fail"},
	}
	h := &fakeAdapter{steps: []invokeStep{{res: bad}}}
	d, rel, prev, opts := mapsDrainRun(t, h, phases, repo, base, tip)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	effective := d.cfg.EffectivePhases()
	_, _, _, end, err := d.drainPhase(ctx, 1, 0, 1, "-p2", effective[1], true, prev, d.targets[0],
		spend{start: time.Now()}, effective, panelState{}, opts)
	if err == nil {
		t.Fatal("a maps-session review whose turn 2 ended badly finished the phase cleanly — it must fail closed like a failed single-pass review")
	}
	if end.reviewRecorded {
		t.Error("a failed turn sequence was recorded as a review")
	}
	if len(rel.records) != 0 {
		t.Errorf("a failed turn sequence wrote %d durable record(s)", len(rel.records))
	}
	if h.invokeCalls != 1 {
		t.Errorf("spawned %d sessions, want 1 — a failed sequence never retries inside itself", h.invokeCalls)
	}
}

// TestMapsSessionDrain_NoPreparedWorktreeRefuses (CLA-869): the maps need the
// tree at the reviewed tip. A review phase without one is REFUSED before
// anything is spawned, rather than reviewing a map of the wrong diff.
func TestMapsSessionDrain_NoPreparedWorktreeRefuses(t *testing.T) {
	repo, base, tip := mapsRepo(t)
	phases := mapsReviewPhases()
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(1, 0)}}}
	d, _, prev, opts := mapsDrainRun(t, h, phases, repo, base, tip)
	opts.taskDir = ""
	opts.worktree = ""

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	effective := d.cfg.EffectivePhases()
	_, _, _, _, err := d.drainPhase(ctx, 1, 0, 1, "-p2", effective[1], true, prev, d.targets[0],
		spend{start: time.Now()}, effective, panelState{}, opts)
	if err == nil || !strings.Contains(err.Error(), "task worktree") {
		t.Fatalf("drainPhase = %v, want a refusal naming the missing task worktree", err)
	}
	if h.invokeCalls != 0 {
		t.Errorf("spawned %d sessions, want none — the refusal is before the spawn", h.invokeCalls)
	}
}

// TestMapsSessionDrain_DefaultReviewUnchanged (CLA-869): a review phase that
// names no method gets the single-pass invocation, byte for byte — no turns and
// no maps section.
func TestMapsSessionDrain_DefaultReviewUnchanged(t *testing.T) {
	repo, base, tip := mapsRepo(t)
	phases := []config.Phase{
		{Name: config.ImplementPhaseName},
		{Name: config.ReviewPhaseName},
		{Name: config.FixPhaseName},
	}
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(1, 0)}}}
	d, _, prev, opts := mapsDrainRun(t, h, phases, repo, base, tip)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	effective := d.cfg.EffectivePhases()
	if _, _, _, _, err := d.drainPhase(ctx, 1, 0, 1, "-p2", effective[1], true, prev, d.targets[0],
		spend{start: time.Now()}, effective, panelState{}, opts); err != nil {
		t.Fatalf("drainPhase: %v", err)
	}
	inv := h.invocations[0]
	if len(inv.Turns) != 0 {
		t.Errorf("the default review carried %d follow-up turns, want none", len(inv.Turns))
	}
	if strings.Contains(inv.Prompt, "How this review works") {
		t.Errorf("the default review got the maps-session section:\n%s", inv.Prompt)
	}
}

// TestPlanMapsSession_RecordEndingForAPanelMember (CLA-869): a phase that writes
// its own record (a panel member) is told to write that record, not a done
// report it does not owe.
func TestPlanMapsSession_RecordEndingForAPanelMember(t *testing.T) {
	r := &phaseRun{
		ph:    config.Phase{Name: "review-1", Prompt: config.PanelMemberReviewOnlyBrief(), ReviewMethod: config.ReviewMethodMapsSession},
		shape: phaseShape{captured: config.Phase{Name: "review-1", Prompt: config.PanelMemberReviewOnlyBrief()}},
		kind:  reportNone,
	}
	if got := r.mapsSessionEnding(); got != config.MapsSessionRecordEnding {
		t.Errorf("a report-less review phase's ending = %q, want the record ending", got)
	}
	r.kind = reportReview
	if got := r.mapsSessionEnding(); got != config.MapsSessionDoneReportEnding {
		t.Errorf("a done-report review phase's ending = %q, want the done-report ending", got)
	}
	plan := planMapsSession(mapsTestOpts(), "", "", "", nil, config.MapsSessionSeverityTask, config.MapsSessionRecordEnding)
	if !strings.Contains(plan.turns[len(plan.turns)-1].Prompt, "write the record it asks for") {
		t.Errorf("the record ending is not on the findings turn:\n%s", plan.turns[len(plan.turns)-1].Prompt)
	}
}

// TestMapsSessionPlanFor_RefusesAWorktreePastTheTip (CLA-869): the flow map's
// git grep reads the checked-out tree, so a worktree parked past the reviewed
// tip would list uses from the wrong revision. It is refused, not mapped.
func TestMapsSessionPlanFor_DiffsTheReviewedTip(t *testing.T) {
	repo, base, tip := mapsRepo(t)
	// Move the worktree past the reviewed tip, as a stray commit would.
	extra := filepath.Join(repo, "later.ts")
	if err := os.WriteFile(extra, []byte("export function afterTipThing() {\n  return 1\n}\n"), 0o600); err != nil {
		t.Fatalf("write later.ts: %v", err)
	}
	cmd := exec.Command("git", "add", ".")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "later")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	ph := config.Phase{
		Name:         config.ReviewPhaseName,
		Prompt:       config.ReviewOnlyBrief(),
		ReviewMethod: config.ReviewMethodMapsSession,
	}
	r := &phaseRun{
		ph:     ph,
		shape:  phaseShape{captured: ph},
		opts:   phaseRunOpts{taskDir: repo, runner: &runnerBriefing{baseSHA: base}},
		prev:   &harness.Result{Claim: harness.Claim{TaskID: "t-1", RunID: "r-1"}, CheckpointTip: tip},
		prBase: "main",
	}
	plan, err := r.mapsSessionPlanFor(&phaseAttempt{inv: harness.Invocation{WorkDir: repo}})
	if err == nil {
		t.Fatalf("a worktree past the reviewed tip was mapped:\n%+v", plan)
	}
	if !strings.Contains(err.Error(), "reviewed tip") || !strings.Contains(err.Error(), "wrong revision") {
		t.Errorf("the refusal does not say why the tree must be at the tip: %v", err)
	}
}

// TestMapsSessionHandoffContinuation_SkipsTheTurns (CLA-869 round 1
// 1.review-1.6): a handoff respawn replaces ph.Prompt with the predecessor's
// continuation, so that successor did not start this phase's review and must not
// receive the map — and the refusal-free skip is the point: the phase still runs,
// just as the single-pass review it really is.
func TestMapsSessionHandoffContinuation_SkipsTheTurns(t *testing.T) {
	repo, base, tip := mapsRepo(t)
	phases := mapsReviewPhases()
	h := &fakeAdapter{steps: []invokeStep{{res: okResult(1, 0)}}}
	d, _, prev, opts := mapsDrainRun(t, h, phases, repo, base, tip)
	effective := d.cfg.EffectivePhases()

	// Exactly the handoff respawn: the captured sequence still carries the
	// review phase, while the phase passed to the attempt carries the successor's
	// continuation text instead of the brief.
	continuation := effective[1]
	continuation.Prompt = "Continue the work the predecessor handed off; it already read the change."

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, _, _, err := d.drainPhase(ctx, 1, 0, 1, "-p2", continuation, true, prev, d.targets[0],
		spend{start: time.Now()}, effective, panelState{}, opts); err != nil {
		t.Fatalf("drainPhase refused a maps-session phase's handoff continuation: %v", err)
	}
	if h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want 1", h.invokeCalls)
	}
	inv := h.invocations[0]
	if len(inv.Turns) != 0 {
		t.Errorf("the handoff continuation carried %d maps-session turn(s), want none", len(inv.Turns))
	}
	if strings.Contains(inv.Prompt, "How this review works") {
		t.Errorf("the maps section was appended to a handoff continuation:\n%s", inv.Prompt)
	}
}

// TestMapsSessionDrain_PanelMemberWritesTheRecord (CLA-869 round 1 1.review-1.7,
// 1.review-1.3): a panel member runs the same sequence from its own checkout at
// the reviewed tip, ends with the RECORD ending rather than a done report, and
// its findings turn names the findings stage's model because the member writes
// the durable record itself.
func TestMapsSessionDrain_PanelMemberWritesTheRecord(t *testing.T) {
	repo, base, tip := mapsRepo(t)
	phases := []config.Phase{
		{Name: config.ImplementPhaseName},
		{
			Name:         config.ReviewPhaseName,
			Model:        config.ModelRoutes{"opencode-go/base-model"},
			ReviewMethod: config.ReviewMethodMapsSession,
			MapsSession:  &config.MapsSessionOptions{ChunkSize: 2, FindingsModel: "findings-model"},
		},
		{Name: config.FixPhaseName},
	}
	// A panel member's session does reach the backlog (it reads the task before
	// recording); report that so the no_mcp backstop does not retry the fixture.
	panelResult := okResult(1, 0)
	panelResult.ClankerbarCalls = 1
	h := &fakeAdapter{steps: []invokeStep{{res: panelResult}}}
	d, _, prev, opts := mapsDrainRun(t, h, phases, repo, base, tip)
	d.cfg.ReviewPanel = []config.ReviewPanelMember{{Model: config.ModelRoutes{"opencode-go/base-model"}}}
	effective := d.cfg.EffectivePhases()
	if len(effective) != 3 || !effective[1].IsPanelMember() {
		t.Fatalf("the fixture did not expand one panel member: %+v", effective)
	}
	// The panel's parallel runner owns a detached checkout at the reviewed tip
	// and carries no runner briefing; the merge-base rides opts.
	opts.taskDir = ""
	opts.worktree = repo
	opts.runner = nil
	opts.baseSHA = base

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, _, _, err := d.drainPhase(ctx, 1, 0, 1, "-preview-1", effective[1], false, prev, d.targets[0],
		spend{start: time.Now()}, effective, panelState{}, opts); err != nil {
		t.Fatalf("drainPhase: %v", err)
	}
	if h.invokeCalls != 1 {
		t.Fatalf("spawned %d sessions, want ONE invocation carrying every turn", h.invokeCalls)
	}
	inv := h.invocations[0]
	// The fixture's 4 items at chunk_size 2 are 2 notes turns; turn 1 carries
	// chunk 1 and one follow-up carries chunk 2, then the findings turn.
	if len(inv.Turns) != 2 {
		t.Fatalf("follow-up turns = %d, want 2 (1 notes turn + findings)", len(inv.Turns))
	}
	for _, want := range []string{"## How this review works", "## Chunk 1 of 2", "a review PANEL"} {
		if !strings.Contains(inv.Prompt, want) {
			t.Errorf("the panel member's turn 1 does not carry %q:\n%s", want, inv.Prompt)
		}
	}
	// The brief's record contract was substituted with the PHASE's model; the
	// findings stage runs on another one, and the findings turn says so because
	// the member writes the record itself.
	if !strings.Contains(inv.Prompt, `model: "opencode-go/base-model"`) {
		t.Errorf("the panel brief no longer names the phase model the clause corrects:\n%s", inv.Prompt)
	}
	if inv.Model != "opencode-go/base-model" {
		t.Errorf("turn 1 model = %q, want the notes stage's inherited base model", inv.Model)
	}
	if inv.Turns[0].Model != "" {
		t.Errorf("the notes follow-up names model %q; it must inherit turn 1's", inv.Turns[0].Model)
	}
	findings := inv.Turns[1]
	if findings.Model != "findings-model" {
		t.Errorf("findings turn model = %q, want findings-model", findings.Model)
	}
	for _, want := range []string{"write the record it asks for", "raisedByModel", `"findings-model"`} {
		if !strings.Contains(findings.Prompt, want) {
			t.Errorf("the panel member's findings turn does not carry %q:\n%s", want, findings.Prompt)
		}
	}
}
