package loop

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// recordedBranches lists the branch names the plane's record carries for a
// phase result: every branch this run recorded via record_progress(branch) that the
// plane accepted (the accepted Reports), plus the branch that came in on the
// claim itself (a resumed predecessor's recorded WIP, or the task's branch at
// claim time). These are the names a checkpoint can be verified against.
func recordedBranches(res harness.Result) []string {
	var out []string
	seen := map[string]bool{}
	add := func(b string) {
		if b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	for _, rep := range res.Reports {
		if res.Claim.Names(rep.TaskID) || (rep.Ref != "" && res.Claim.Names(rep.Ref)) {
			add(rep.Branch)
		}
	}
	if res.Claim.HasWIP {
		add(res.Claim.Branch)
	}
	return out
}

// reviewGate is the review-shaped phase's seam contract (CLA-637): whether a
// durable review record is required, and the outcome as it stood BEFORE the
// phase spawned. The record requirement makes the review this-trip rather than
// last-trip the way B1 prescribes: the seam demands a record keyed on the run
// that wrote it, and a request_changes re-trip (or a takeover) runs under a
// fresh run id, so a record from a previous trip can never satisfy it. The
// baseline is kept for the pre-spawn read's tri-state and the refusal's
// diagnostic.
//
// read is the tri-state the freshness check needs: false means the baseline
// could not be read (no plane reader, a failed read, no task id on the carried
// claim). Since CLA-663 that is refused BEFORE the phase spawns — a full
// review session whose checkpoint the seam would refuse anyway is pure waste,
// and the claim would lapse into a takeover that re-runs implement and review.
// baseline is meaningful only when read is true.
//
// Since CLA-637 the gate itself is the DURABLE review record (B1), keyed on
// the run that wrote it: role names the record this phase's session writes
// (both sides read the same name out of the brief), and round is written back
// by checkpointEvidence with the round the matched record came from — the round
// the driver then closes to release the findings to the fix phase. Zero means
// no record matched.
type reviewGate struct {
	required bool
	read     bool
	baseline string
	role     string
	round    int
	// state is the matched record's plane state (sealed / open / void). The
	// driver closes only a SEALED round: a verifier releases its own round in
	// the same call that hands the task over (CLA-638), and a second close would
	// fail with no_open_round. Void records never match.
	state string
}

// reviewerRoleFor is the reviewer role of the review-shaped phase at idx
// (CLA-637): `review-N`, counting the review-shaped phases up to and including
// it, so two review phases in one sequence (a panel, CLA-639) write distinct
// records and the plane's one-row-per-role-per-round rule is not tripped. The
// role is a name both sides read: the brief hands it to the session and the
// seam gate keys the record on it. Empty for a phase that is not review-shaped.
func reviewerRoleFor(phases []config.Phase, idx int) string {
	if idx < 0 || idx >= len(phases) || !config.ReviewShaped(phases[idx].Prompt) {
		return ""
	}
	n := 0
	for i := 0; i <= idx; i++ {
		if config.ReviewShaped(phases[i].Prompt) {
			n++
		}
	}
	return "review-" + strconv.Itoa(n)
}

// matchingReviewRecord finds the durable review record a review-shaped phase's
// seam gate demands (CLA-637, B1): a record THIS RUN wrote under this reviewer
// role. The run id is the freshness key — a request_changes re-trip or a
// takeover runs under a fresh run id, so the previous trip's record can never
// satisfy the gate — and records voided by a lease expiry or takeover (B4)
// never match either. The tip requirement follows the checkpoint the driver
// made:
//
//   - verified: tip is the origin tip the driver read for the branch, and the
//     record must name exactly it — the revision reviewed must still BE the
//     tip the fix phase will find.
//   - unverified (CLA-576): the driver has no tip, so the reviewer established
//     one itself; the record must name one, because the fix phase compares it
//     with the worktree.
//
// A BRANCHLESS checkpoint does not reach this matcher at all: the plane accepts
// review writes only from the task's live holder, and a settled task has none,
// so that variant keeps the legacy `## Review` block gate (the CLA-634 decision
// on question 03a12578, option 1).
//
// The run ids are compared by runUUID: the claim hands the driver the run
// CREDENTIAL, while the plane stores and serves a record's run as the bare id.
//
// The newest match wins, so a retried attempt's record supersedes an earlier
// one in the same run.
func matchingReviewRecord(records []plane.ReviewRecordState, role, runID, tip string, requireTip bool) (plane.ReviewRecordState, bool) {
	var best plane.ReviewRecordState
	found := false
	want := runUUID(runID)
	for _, r := range records {
		if runUUID(r.RunID) != want || r.ReviewerRole != role || r.State == "void" {
			continue
		}
		switch {
		case tip != "":
			if r.ReviewedTip != tip {
				continue
			}
		case requireTip:
			if r.ReviewedTip == "" {
				continue
			}
		}
		if !found || r.Round > best.Round {
			best, found = r, true
		}
	}
	return best, found
}

// panelMemberRecordMatch asks the plane whether a panel member's durable record
// is already in the round, using the SAME evidence the review seam uses:
// matchingReviewRecord keyed by this run and the member's positional role, at
// the checkpoint's reviewed tip.
//
// The seam asks that question after a session ends. A member that writes its
// record and only THEN ends the sequence — a usage limit or a transient failure
// — returns from drainPhase before the seam runs, so the panel handler must ask
// it here or the member is recorded as a failure while its record sits in the
// round (CLA-639 fix pass 4). Best-effort: an unreadable task or an unwired
// reader leaves the member classified as failed, exactly as before this check.
func (d *Driver) panelMemberRecordMatch(ctx context.Context, t Target, res harness.Result, role string) (plane.ReviewRecordState, bool) {
	if role == "" || res.Claim.TaskID == "" {
		return plane.ReviewRecordState{}, false
	}
	st, ok := d.readTaskState(ctx, t, res.Claim.TaskID)
	if !ok {
		return plane.ReviewRecordState{}, false
	}
	return matchingReviewRecord(st.Reviews, role, res.Claim.RunID, res.CheckpointTip, res.CheckpointUnverified)
}

// runUUID is a run id's identity: the part before the first dot. Since CLA-509
// claim_task returns a run as the credential `<uuid>.<secret>`, but the plane
// keys everything it stores (review records, claimedByRun) on the bare uuid,
// so two ids name the same run exactly when their uuids match. A bare id (a
// run claimed before credentials, or one read back from the plane) is its own
// uuid. This is the plane's own split (parseRunCredential).
func runUUID(id string) string {
	if i := strings.IndexByte(id, '.'); i >= 0 {
		return id[:i]
	}
	return id
}

// isReviewHeading reports whether a line is the review block's heading, with
// trailing whitespace tolerated so a CRLF paste or a stray trailing space still
// reads as the heading.
func isReviewHeading(line string) bool {
	return strings.TrimRight(line, " \t\r") == config.ReviewRecordHeading
}

// reviewRecordLine reports whether outcome carries the review heading as its
// own line. The BRANCHLESS review-only checkpoint gates on it (CLA-634
// decision, question 03a12578, option 1): a settled task cannot write the
// durable record — the plane accepts review writes only from the live holder —
// so that variant keeps the legacy block gate. The exact line keeps a
// look-alike heading (`## Reviewer notes`, or prose that merely mentions the
// words) from standing in for the block. Trailing whitespace is ignored — the
// plane stores text as sent, and a CRLF paste or a stray trailing space is
// still the heading — while leading whitespace is not: a heading the session
// indented into a code block is not the block the fix phase will find.
func reviewRecordLine(outcome string) bool {
	for _, line := range strings.Split(outcome, "\n") {
		if isReviewHeading(line) {
			return true
		}
	}
	return false
}

// lastReviewBlock returns the outcome's text from its LAST `## Review` heading
// line to the end — the block a fix session is to read and apply. A task that
// went through a request_changes re-trip, or a takeover, can carry several
// blocks: both the review and the fix APPEND, so round 1's block stays in the
// outcome above round 2's, and "the `## Review` block" is ambiguous unless it
// means the last one (which is why every fix brief says so). The heading line
// itself is included: a block whose text is only its heading would otherwise
// decode to "", which is a substring of everything. It recognises the heading
// exactly as reviewRecordLine does, so the two agree on what a block is.
//
// The BRANCHLESS review-only checkpoint's stale-round guard reads it: a
// session that makes some other statusless outcome edit while leaving an
// earlier round's block as the last one recorded nothing this trip. Empty when
// the outcome carries no heading line at all; the gate checks reviewRecordLine
// first, so an empty return there is not a block, not an old one.
func lastReviewBlock(outcome string) string {
	start, offset := -1, 0
	for _, line := range strings.SplitAfter(outcome, "\n") {
		if isReviewHeading(strings.TrimSuffix(line, "\n")) {
			start = offset
		}
		offset += len(line)
	}
	if start < 0 {
		return ""
	}
	return outcome[start:]
}

// reviewFinding is one parsed line of a review record's `### Findings` list.
// The driver parses the block it told the review to write (CLA-635) to build
// the fix phase's graded list, so the fields here are an interface with the
// shape in config.reviewOnlyRecordShape, not a convenience.
type reviewFinding struct {
	ID       string // the record's own id, e.g. "R1"
	Severity string // canonical lowercase: must | should | nit
	Kind     string // MUST evidence kind (the token in parentheses), "" when absent
	Evidence string // MUST evidence payload (the text after the kind's colon), "" when absent
	// Rest is the finding line from just after the severity bracket onwards —
	// evidence prefix, location, summary and required fix — so the digest can
	// reprint the finding verbatim without re-joining fields it did not parse.
	Rest string
}

// downgraded reports whether B5's structural check demotes this finding: a
// `must` written without an evidence kind, or with one but no evidence payload,
// is treated as a `should`. Only presence is checked — whether the evidence is
// any GOOD is a verifier's call in the next round, never the driver's.
func (f reviewFinding) downgraded() bool {
	return f.Severity == "must" && (f.Kind == "" || f.Evidence == "")
}

// gradedSeverity is the severity the fix phase is held to, after the
// structural downgrade.
func (f reviewFinding) gradedSeverity() string {
	if f.downgraded() {
		return "should"
	}
	return f.Severity
}

var (
	// A finding line as config.reviewOnlyRecordShape prescribes it: an id, a
	// bracketed severity, then the rest of the line. Malformed lines are
	// skipped, not guessed at — the fix phase still reads the raw block.
	reviewFindingLineRE = regexp.MustCompile(`^\s*[-*]\s+(R\d+)\s+\[([A-Za-z]+)\](.*)$`)
	// The evidence prefix: `(<kind>): <evidence>`. A colon is required, so
	// `(gate) go test fails` — kind without the prescribed prefix — reads as
	// no kind at all and downgrades the MUST.
	reviewEvidenceRE = regexp.MustCompile(`^\s*\(([^)]*)\)\s*:\s*(.*)$`)
)

// parseReviewFindings extracts the findings of the NEWEST review block in
// outcome. Newest, not first: a request_changes re-trip or a takeover returns
// the task with an earlier round's block still in the outcome, and the fix
// phase applies THIS round's review. Empty when there is no block or no line
// in it parses.
func parseReviewFindings(outcome string) []reviewFinding {
	lines := strings.Split(outcome, "\n")
	start := -1
	for i, line := range lines {
		if isReviewHeading(line) {
			start = i
		}
	}
	if start < 0 {
		return nil
	}
	var out []reviewFinding
	for _, line := range lines[start+1:] {
		// A new level-2 heading ends the block. `### Findings` is
		// level-3 and does not match the "## " prefix.
		if strings.HasPrefix(strings.TrimRight(line, " \t\r"), "## ") {
			break
		}
		m := reviewFindingLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		sev := strings.ToLower(m[2])
		switch sev {
		case "must", "should", "nit":
		default:
			continue
		}
		f := reviewFinding{ID: m[1], Severity: sev, Rest: m[3]}
		if em := reviewEvidenceRE.FindStringSubmatch(f.Rest); em != nil {
			f.Kind = strings.TrimSpace(em[1])
			f.Evidence = strings.TrimSpace(em[2])
		}
		out = append(out, f)
	}
	return out
}

// reviewFindingsDigest is the driver's graded list of a review record's
// findings, prepended to the fix phase's prompt (CLA-635). It exists because
// B5's structural downgrade is the DRIVER's to apply: a `must` recorded without
// an evidence kind or payload is listed here as a SHOULD, so the fixer is held
// to the same grade the record will be read with later. The severities are the
// ones the fix brief's obligation speaks about; the raw block stays in the
// outcome for the record.
//
// Empty when the record carries no parseable findings — the fix brief then
// works from the raw block alone, exactly as before this existed.
func reviewFindingsDigest(outcome string) string {
	findings := parseReviewFindings(outcome)
	if len(findings) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("The driver read the review record and graded its findings structurally: a finding recorded as a " +
		"MUST with no evidence kind (a parenthesised token right after the severity) or no evidence text is listed " +
		"below as a SHOULD, not a MUST. The severities below are the ones this phase is held to:\n")
	for _, f := range findings {
		b.WriteString("- " + f.ID + " [" + strings.ToUpper(f.gradedSeverity()) + "]" + f.Rest)
		if f.downgraded() {
			b.WriteString(" [downgraded: recorded as a MUST with no evidence kind or payload]")
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}
