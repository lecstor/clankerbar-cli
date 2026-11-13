package config

// prompts_maps.go — the maps-session review method's turn prompts (CLA-864,
// CLA-869). This is not a brief of its own: the phase's review brief stays the
// first turn's substance (runner context, read-only rules, the bar, the done
// report), and the section below is APPENDED to it. The later turns are
// deliberately lean — they rely on the first turn's transcript, which the same
// session still holds.

import (
	"fmt"
	"strings"
)

// mapsSessionChecklist is the review checklist the notes turns work through. It
// is the adversarial gate's categories stated as checks a per-item note can
// answer, since a note is written about one item at a time.
const mapsSessionChecklist = "## Checklist\n" +
	"- Correctness: wrong logic, an inverted condition, off-by-one, a missing case, a path that skips a step its " +
	"siblings perform; async/concurrency; nil/null; swallowed errors; values wrong for some inputs, including on " +
	"another platform.\n" +
	"- Contracts and data: a changed signature, return shape or config key whose callers or docs were not " +
	"updated; parameters that must be supplied together; DB constraints, indexes or migrations; input left " +
	"unvalidated where a sibling validates it.\n" +
	"- Security: input that reaches a shell, a path, a URL or a query without validation; a secret or credential " +
	"logged or passed where it should not be; a permission widened beyond what the change needs.\n" +
	"- UI: only when the change touches UI — a state that cannot be reached, a control with no handler, a " +
	"loading or error path the other views handle.\n" +
	"- Performance: N+1 queries, repeated reads, work done in a loop that could be done once.\n" +
	"- Maintainability: dead code, impossible fallbacks, leftover debugging, inconsistency with the conventions " +
	"around it, a misleading name.\n" +
	"- Tests: new behaviour or a new branch with no test, including sibling paths; a test that would still pass " +
	"if the change regressed; an assertion that no longer matches the code.\n" +
	"- Documentation: docs, comments, READMEs or examples the change made wrong."

// MapsSessionFirstTurn is the section appended to the phase's review brief for
// the first notes turn: what the maps are, how to read an M and an F item, the
// checklist, and chunk 1 of the items.
//
// base is the commit the per-file diff starts from (the runner's merge-base),
// filled in literally: this text is appended after the driver's placeholder
// substitution, so a placeholder here would reach the session unsubstituted.
func MapsSessionFirstTurn(base string, chunk, chunks int, items string) string {
	var b strings.Builder
	b.WriteString("## How this review works\n")
	b.WriteString("The runner has mapped the change into items:\n")
	b.WriteString("- `M` items: each changed region of the diff, with the function or block it sits in.\n")
	b.WriteString("- `F` items: each definition the change adds or changes (function, method, type, component, hook), " +
		"with the places in the repository that use it (from git grep, capped; there may be more).\n\n")
	b.WriteString("You will get the items in chunks. For each chunk, write a note per item. When every chunk is done, " +
		"you will be asked to turn your notes into findings. Nothing you write in a note is reported as it stands, " +
		"so note everything you see: problems, doubts, things that look inconsistent or untested, however small. " +
		"A note that says only \"fine\" should mean you checked and found nothing.\n\n")
	b.WriteString("For an `M` item: read the region at head with enough surrounding code to understand it, and its " +
		"diff. Compare it with sibling code that does the same job elsewhere. Apply the checklist below.\n\n")
	b.WriteString("For an `F` item: read the definition, then each listed use, and compare: argument count, order " +
		"and types against the parameters; object fields and prop names written against those read; optional " +
		"against required; sync against async (missing await, unhandled promise, a goroutine or error that is " +
		"dropped); return values the caller ignores or assumes non-null; renamed or removed exports still " +
		"referenced. Follow a use one level further when the value is passed on.\n\n")
	if base != "" {
		fmt.Fprintf(&b, "Use `git diff %s..HEAD -- <path>` for the diff of a file, git grep to find more uses, "+
			"and read whole files where you need them.\n\n", base)
	} else {
		b.WriteString("Use `git diff <base>..HEAD -- <path>` for the diff of a file, git grep to find more uses, " +
			"and read whole files where you need them.\n\n")
	}
	b.WriteString(mapsSessionChecklist + "\n\n")
	b.WriteString(mapsSessionChunkHeading(chunk, chunks) + "\n\n" + items + "\n\n")
	b.WriteString(mapsSessionNotesRule)
	return b.String()
}

// MapsSessionNotesTurn is one follow-up notes turn: the next chunk, with a
// reminder that the instructions still stand and the report still does not
// belong in this turn.
func MapsSessionNotesTurn(chunk, chunks int, items string) string {
	return "## Chunk " + fmt.Sprintf("%d of %d", chunk, chunks) + "\n" +
		"Same instructions as before: read each item and its context, apply the checklist, and note everything " +
		"you notice, however small.\n\n" + items + "\n\n" +
		"Reply with one note per item, in order, each starting with its id. " +
		"Do NOT write a done report in this turn."
}

// The two endings a findings turn can have, chosen by the phase's own brief: a
// runner-claimed review ends with a done report the runner turns into records,
// and a panel member (or any phase writing its own record) ends by writing that
// record itself.
const (
	MapsSessionDoneReportEnding = "Then end with the done report exactly as the review brief specifies " +
		"(verdict, reviewedTip, findings, omitted, notes)."
	MapsSessionRecordEnding = "Then end exactly as your brief specifies: write the record it asks for " +
		"(verdict, reviewedTip, findings, omitted, notes)."
)

// MapsSessionFindingsTurn is the final turn: confirm every doubtful note against
// the code, look across the notes for problems no single item showed, and end
// with the record the phase's brief specifies.
func MapsSessionFindingsTurn(ending string) string {
	return "You have been through every item. Now turn your notes into findings.\n" +
		"1. Go through all your notes from every chunk. For each note that names a problem or a doubt, confirm it " +
		"against the code: find the line that proves it, or the line that shows it is fine. Use the tools again " +
		"where you need to.\n" +
		"2. Look across the notes for problems that span items: a definition and a caller that disagree, the same " +
		"mistake in several regions, a behaviour the task or PR claims that no item implements, new behaviour no " +
		"test covers.\n" +
		"3. Write every confirmed problem as a finding. A note can give several findings; the same problem in " +
		"several places gives one finding per place.\n\n" +
		"Report a problem if the change introduced it, or made an existing problem reachable or more visible, or " +
		"touched the code and left it in place where a reviewer would expect it fixed; and you confirmed it " +
		"against the code. Report low-severity problems too (a missing test, a stale comment, dead code, an " +
		"inconsistency). Do not report pure formatting, naming preferences, errors the compiler or linter already " +
		"rejects, or concerns you could not confirm.\n\n" +
		"Severity: MUST only for what violates the task's done-when bar, the repo's gates, or correctness/safety; " +
		"SHOULD for a real problem that does not block; NIT for small issues. " + ending
}

// mapsSessionChunkHeading names one chunk in a notes turn.
func mapsSessionChunkHeading(chunk, chunks int) string {
	return fmt.Sprintf("## Chunk %d of %d", chunk, chunks)
}

// mapsSessionNotesRule closes every notes turn: notes are notes, and the report
// is the findings turn's alone.
const mapsSessionNotesRule = "Reply with one note per item in this chunk, in order, each starting with its id " +
	"(`M3: ...`). Do NOT write a done report in this turn."
