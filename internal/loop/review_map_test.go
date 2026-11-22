package loop

// CLA-975: `clankerbar review map`. The tests drive the REAL git plumbing and
// assert the two things the command exists for: the turns it prints are
// byte-equal to the ones `review run --method maps-session` puts on its
// invocation, and no session is ever started.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// reviewMapFiveItemRepo builds a checkout whose base..head diff has exactly
// five map items: five text files, one hunk each, and no changed definition to
// add a flow item.
func reviewMapFiveItemRepo(t *testing.T) (*reviewRunTestRepo, string, string) {
	t.Helper()
	r := newReviewRunRepo(t)
	for i := 1; i <= 5; i++ {
		r.write(fmt.Sprintf("note%d.txt", i), fmt.Sprintf("base %d\n", i))
	}
	base := r.commit("base")
	for i := 1; i <= 5; i++ {
		r.write(fmt.Sprintf("note%d.txt", i), fmt.Sprintf("head %d\n", i))
	}
	head := r.commit("head")
	return r, base, head
}

// TestReviewMapMatchesReviewRunTurns is decision 1's equality test: both
// commands build through one function, and the map's turn 1 is the review-run
// invocation's prompt (brief plus first chunk) while every later turn is the
// one on inv.Turns.
func TestReviewMapMatchesReviewRunTurns(t *testing.T) {
	r := newReviewRunRepo(t)
	r.write("sample.go", "package sample\n\nfunc Alpha(x int) int {\n\treturn x + 1\n}\n\nfunc Beta(x int) int {\n\treturn x + 2\n}\n")
	base := r.commit("base")
	r.write("sample.go", "package sample\n\nfunc Alpha(x int) int {\n\treturn x + 11\n}\n\nfunc Beta(x int) int {\n\treturn x + 22\n}\n\nfunc Gamma() int {\n\treturn Alpha(1) + Beta(2)\n}\n")
	head := r.commit("head")

	var inv harness.Invocation
	_, err := RunReview(context.Background(), ReviewRunOptions{
		Repo:      r.dir,
		Base:      base,
		Head:      head,
		Method:    config.ReviewMethodMapsSession,
		Harness:   "opencode",
		Model:     "test/phase-model",
		ChunkSize: 1,
		Title:     "Ship the guard",
		cfg:       &config.Config{},
		invoke: func(_ context.Context, i harness.Invocation) (harness.Result, error) {
			inv = i
			messages := make([]string, 0, len(i.Turns)+1)
			for range i.Turns {
				messages = append(messages, "a note")
			}
			messages = append(messages, reviewRunDone(head, reviewRunMustFinding))
			return harness.Result{TurnMessages: messages, FinalMessage: messages[len(messages)-1]}, nil
		},
	})
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if inv.Prompt == "" || len(inv.Turns) == 0 {
		t.Fatal("review run put no maps sequence on its invocation")
	}
	if len(inv.Turns) < 2 {
		t.Fatalf("review run's invocation carries %d follow-up turns; the fixture must exercise several notes turns", len(inv.Turns))
	}

	report, err := ReviewMap(ReviewMapOptions{
		Repo: r.dir, Base: base, Head: head, ChunkSize: 1, Title: "Ship the guard",
	})
	if err != nil {
		t.Fatalf("ReviewMap: %v", err)
	}
	if report.Base != base || report.Head != head {
		t.Errorf("report commits = %s..%s, want the resolved %s..%s", report.Base, report.Head, base, head)
	}
	if len(report.Turns) != 1+len(inv.Turns) {
		t.Fatalf("turns = %d, want %d (the invocation's prompt plus its follow-up turns): %+v",
			len(report.Turns), 1+len(inv.Turns), report.Turns)
	}
	if report.Turns[0].Prompt != inv.Prompt {
		t.Errorf("turn 1's prompt is not review run's invocation prompt\nmap:\n%s\nreview run:\n%s",
			report.Turns[0].Prompt, inv.Prompt)
	}
	for i, turn := range inv.Turns {
		if got, want := report.Turns[i+1].Prompt, turn.Prompt; got != want {
			t.Errorf("turn %d's prompt is not review run's turn prompt\nmap:\n%s\nreview run:\n%s", i+2, got, want)
		}
	}
	if got := report.Turns[0]; got.Kind != reviewMapNotesKind || got.Chunk != 1 || got.Of != report.Items.Chunks {
		t.Errorf("turn 1 = %+v, want the first notes turn of %d", got, report.Items.Chunks)
	}
	last := report.Turns[len(report.Turns)-1]
	if last.Kind != reviewMapFindingsKind {
		t.Errorf("last turn kind = %q, want the findings turn", last.Kind)
	}
	if last.Chunk != 0 || last.Of != 0 {
		t.Errorf("the findings turn carries chunk/of %d/%d, want neither", last.Chunk, last.Of)
	}
	notes := 0
	for _, turn := range report.Turns {
		if turn.Kind == reviewMapNotesKind {
			notes++
		}
	}
	if notes != report.Items.Chunks {
		t.Errorf("notes turns = %d, want items.chunks %d", notes, report.Items.Chunks)
	}
	if report.Items.Change < 1 || report.Items.Flow < 1 {
		t.Errorf("items = %+v, want at least one M and one F item from this fixture", report.Items)
	}
}

// TestReviewMapRefusesACheckoutAtTheWrongCommit: the flow map's git grep reads
// the checked-out tree, so a checkout at another revision would map uses from
// the wrong one. The refusal names both SHAs.
func TestReviewMapRefusesACheckoutAtTheWrongCommit(t *testing.T) {
	r := newReviewRunRepo(t)
	r.write("README.md", "base\n")
	base := r.commit("base")
	r.write("README.md", "head\n")
	head := r.commit("head")
	r.write("README.md", "later\n")
	at := r.commit("later")

	_, err := ReviewMap(ReviewMapOptions{Repo: r.dir, Base: base, Head: head})
	if err == nil {
		t.Fatal("ReviewMap accepted a checkout that is not at --head")
	}
	for _, want := range []string{head, at, r.dir} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q", err, want)
		}
	}
}

// TestReviewMapEmptyDiff: a change with no map items still gets its one notes
// turn, then the findings turn.
func TestReviewMapEmptyDiff(t *testing.T) {
	r := newReviewRunRepo(t)
	r.write("README.md", "only\n")
	base := r.commit("base")

	report, err := ReviewMap(ReviewMapOptions{Repo: r.dir, Base: base, Head: base})
	if err != nil {
		t.Fatalf("ReviewMap: %v", err)
	}
	if report.Items != (ReviewMapItems{Chunks: 1}) {
		t.Errorf("items = %+v, want zero items in one chunk for an empty diff", report.Items)
	}
	if len(report.Turns) != 2 {
		t.Fatalf("turns = %d, want one notes turn plus the findings turn", len(report.Turns))
	}
	if got := report.Turns[0]; got.Kind != reviewMapNotesKind || got.Chunk != 1 || got.Of != 1 || got.Prompt == "" {
		t.Errorf("notes turn = %+v, want 1/1 with a prompt", got)
	}
	if got := report.Turns[1]; got.Kind != reviewMapFindingsKind || got.Prompt == "" {
		t.Errorf("findings turn = %+v, want a findings turn with a prompt", got)
	}
	if !strings.Contains(report.Turns[0].Prompt, "(no changed regions found)") {
		t.Errorf("the empty change's notes turn does not carry the no-regions sentence:\n%s", report.Turns[0].Prompt)
	}
}

// TestReviewMapFlowOverflowIsNotAnItem: the flow cap's "(N more changed
// definitions not listed)" note is context for the reviewer, not a numbered
// item, so items.flow counts only the numbered F items.
func TestReviewMapFlowOverflowIsNotAnItem(t *testing.T) {
	r := newReviewRunRepo(t)
	r.write("sample.go", "package sample\n\nfunc Alpha(x int) int {\n\treturn x + 1\n}\n\nfunc Beta(x int) int {\n\treturn x + 2\n}\n")
	base := r.commit("base")
	r.write("sample.go", "package sample\n\nfunc Alpha(x int) int {\n\treturn x + 10\n}\n\nfunc Beta(x int) int {\n\treturn x + 20\n}\n")
	head := r.commit("head")

	report, err := ReviewMap(ReviewMapOptions{Repo: r.dir, Base: base, Head: head, FlowMax: 1})
	if err != nil {
		t.Fatalf("ReviewMap: %v", err)
	}
	if report.Items.Flow != 1 {
		t.Errorf("items.flow = %d, want 1: the overflow note is not an item", report.Items.Flow)
	}
	var prompts strings.Builder
	for _, turn := range report.Turns {
		prompts.WriteString(turn.Prompt)
	}
	if !strings.Contains(prompts.String(), "not listed") {
		t.Error("the prompts carry no overflow note, want the unnumbered note still in the reviewer's context")
	}
}

// TestReviewMapChunkSizeSplitsTheItems: --chunk-size 2 over 5 items is three
// notes turns, numbered 1/3, 2/3 and 3/3.
func TestReviewMapChunkSizeSplitsTheItems(t *testing.T) {
	r, base, head := reviewMapFiveItemRepo(t)

	report, err := ReviewMap(ReviewMapOptions{Repo: r.dir, Base: base, Head: head, ChunkSize: 2})
	if err != nil {
		t.Fatalf("ReviewMap: %v", err)
	}
	if want := (ReviewMapItems{Change: 5, Flow: 0, Chunks: 3}); report.Items != want {
		t.Errorf("items = %+v, want %+v", report.Items, want)
	}
	if len(report.Turns) != 4 {
		t.Fatalf("turns = %d, want three notes turns plus the findings turn", len(report.Turns))
	}
	for i, want := range []struct{ chunk, of int }{{1, 3}, {2, 3}, {3, 3}} {
		got := report.Turns[i]
		if got.Kind != reviewMapNotesKind || got.Chunk != want.chunk || got.Of != want.of {
			t.Errorf("turn %d = %+v, want notes %d/%d", i+1, got, want.chunk, want.of)
		}
	}
	if got := report.Turns[3]; got.Kind != reviewMapFindingsKind {
		t.Errorf("last turn = %+v, want findings", got)
	}
	if !strings.Contains(report.Turns[0].Prompt, "M1.") || !strings.Contains(report.Turns[0].Prompt, "M2.") {
		t.Errorf("turn 1 does not carry its first chunk (M1, M2):\n%s", report.Turns[0].Prompt)
	}
	if !strings.Contains(report.Turns[2].Prompt, "M5.") {
		t.Errorf("turn 3 does not carry the last item (M5):\n%s", report.Turns[2].Prompt)
	}
}

// TestReviewMapJSONShape pins the wire shape the served skill reference cites:
// the fixed field names, chunk/of on notes turns only, and the findings turn
// last.
func TestReviewMapJSONShape(t *testing.T) {
	r, base, head := reviewMapFiveItemRepo(t)
	report, err := ReviewMap(ReviewMapOptions{Repo: r.dir, Base: base, Head: head, ChunkSize: 2})
	if err != nil {
		t.Fatalf("ReviewMap: %v", err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("the document does not parse: %v\n%s", err, data)
	}
	for _, key := range []string{"base", "head", "items", "turns"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("the document carries no %q: %s", key, data)
		}
	}
	var items map[string]json.RawMessage
	if err := json.Unmarshal(doc["items"], &items); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"change", "flow", "chunks"} {
		if _, ok := items[key]; !ok {
			t.Errorf("items carries no %q: %s", key, doc["items"])
		}
	}
	var turns []map[string]json.RawMessage
	if err := json.Unmarshal(doc["turns"], &turns); err != nil {
		t.Fatal(err)
	}
	if len(turns) != 4 {
		t.Fatalf("turns = %d, want 4", len(turns))
	}
	for _, key := range []string{"kind", "chunk", "of", "prompt"} {
		if _, ok := turns[0][key]; !ok {
			t.Errorf("a notes turn carries no %q: %s", key, doc["turns"])
		}
	}
	last := turns[len(turns)-1]
	for _, key := range []string{"kind", "prompt"} {
		if _, ok := last[key]; !ok {
			t.Errorf("the findings turn carries no %q: %s", key, doc["turns"])
		}
	}
	for _, key := range []string{"chunk", "of"} {
		if _, ok := last[key]; ok {
			t.Errorf("the findings turn carries %q, want it left to the notes turns: %s", key, doc["turns"])
		}
	}
}
