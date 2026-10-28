package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLA-702 review 1.review-3.1: the doneWhen requires the verified panel member
// brief and the single reviewer's two briefs to be UNCHANGED, and a seam check
// cannot establish that — an edit anywhere else in an eleven-kilobyte brief
// would pass it. The goldens are those briefs' fully resolved text as it stood
// on the branch's base (origin/staging at 3dedf26, which this branch left them
// byte-identical to); any unintended change anywhere in them fails here.
//
// The panel member's UNVERIFIED brief is deliberately not pinned: this task
// rewrites exactly that one. A deliberate change to a shared fragment these
// briefs embed (the gate scope, the record shape, the handoff guidance) means
// updating the matching goldens by hand — that friction is the point, because
// it is the only way an edit elsewhere in the brief cannot slip through.
func TestReviewBriefsUnchangedByTheUnverifiedSplit(t *testing.T) {
	for _, tc := range []struct{ name, brief, golden string }{
		{"the single reviewer's verified brief", reviewOnlyBrief, "single-reviewer-verified.txt"},
		{"the verified panel member brief", panelMemberReviewOnlyBrief, "panel-member-verified.txt"},
		{"the single reviewer's unverified brief", unverifiedReviewOnlyBrief, "single-reviewer-unverified.txt"},
	} {
		want, err := os.ReadFile(filepath.Join("testdata", "review-briefs", tc.golden))
		if err != nil {
			t.Fatalf("read the pre-split golden %s for %s: %v", tc.golden, tc.name, err)
		}
		// A file needs its closing newline; the brief itself does not carry one.
		wantBrief := strings.TrimSuffix(string(want), "\n")
		if tc.brief == wantBrief {
			continue
		}
		t.Errorf("%s changed from its pre-split golden (%s): %s",
			tc.name, tc.golden, firstBriefDifference(wantBrief, tc.brief))
	}
}

// firstBriefDifference reports where a brief first departs from its golden, so
// a whole-brief mismatch yields one sentence instead of two eleven-kilobyte
// texts. Runes, not bytes: the briefs carry em dashes.
func firstBriefDifference(want, got string) string {
	w, g := []rune(want), []rune(got)
	i := 0
	for i < len(w) && i < len(g) && w[i] == g[i] {
		i++
	}
	if i == len(w) && i == len(g) {
		return "no difference"
	}
	window := func(rs []rune) string {
		from, to := i-60, i+60
		if from < 0 {
			from = 0
		}
		if to > len(rs) {
			to = len(rs)
		}
		if from > len(rs) {
			from = len(rs)
		}
		return string(rs[from:to])
	}
	return fmt.Sprintf("first difference at rune %d: want …%s…, got …%s…", i, window(w), window(g))
}
