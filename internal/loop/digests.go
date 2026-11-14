package loop

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// findingIDRound parses the round prefix out of a plane finding id
// (`<round>.<reviewer role>.<n>`, B13). The plane assigns and serves these
// namespaced ids (CLA-636); the driver reads the round back rather than
// tracking one of its own, so the derivation survives a takeover.
func findingIDRound(id string) (int, bool) {
	dot := strings.Index(id, ".")
	if dot <= 0 {
		return 0, false
	}
	n, err := strconv.Atoi(id[:dot])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// verificationDigest is the driver's brief for a verification round (CLA-638):
// the prior open MUST ids, their findings, and the delta base, read from the
// plane's released records and PREPENDED to the verify brief the way the fix
// phase's graded list is. It exists because ids and rounds are plane state, not
// config: the driver reads them after the previous round closed and hands the
// session exactly what it must adjudicate.
//
// A record still sealed or void contributes nothing: a sealed round's findings
// are withheld by design (B2), and a void round never happened (B4). An empty
// open list is not an empty digest — the session is still told the delta base
// and that the gate currently reads as clear, so a verifier that sees a problem
// states it rather than silently passing a hand-over it cannot explain.
func verificationDigest(records []plane.ReviewRecordState, openMustIDs []string, recordsTotal int) string {
	byID := map[string]plane.ReviewFindingState{}
	newestRound, newestTip, newestCycle := 0, "", 0
	for _, r := range records {
		if r.State == "sealed" || r.State == "void" {
			continue
		}
		if r.Round > newestRound {
			newestRound, newestTip = r.Round, r.ReviewedTip
		}
		if r.Cycle > newestCycle {
			newestCycle = r.Cycle
		}
	}
	// cycleOf maps a finding to its record's cycle, so the settled list below is
	// this review cycle's alone: a human verdict starts a new cycle, and the
	// previous cycle's MUSTs were judged by that verdict, not by this round.
	cycleOf := map[string]int{}
	for _, r := range records {
		if r.State == "sealed" || r.State == "void" {
			continue
		}
		for _, f := range r.Findings {
			byID[f.ID] = f
			cycleOf[f.ID] = r.Cycle
		}
	}
	// The plane's openMustIds is the authoritative gate input (CLA-636); the
	// records supply the finding detail. Union them, so a findings block
	// truncated by the read budget still yields every id it must adjudicate.
	openIDs := append([]string(nil), openMustIDs...)
	seen := map[string]bool{}
	for _, id := range openMustIDs {
		seen[id] = true
	}
	for id, f := range byID {
		if f.Severity == "must" && f.Status == "open" && !seen[id] {
			openIDs = append(openIDs, id)
			seen[id] = true
		}
	}
	sort.Strings(openIDs)
	// CLA-671: the fix phase marks the MUSTs it answers `fixed` or `refuted`
	// itself, so a verifier handed only the OPEN list never sees a refutation —
	// it changes no code, so the delta does not show it either — and a wrong one
	// rode to in_review unexamined. Every MUST of this cycle already marked
	// settled is listed for the verifier to confirm (leave it) or reject (set it
	// back to open). Bounded: the round cap keeps a cycle to three rounds.
	var settledIDs []string
	for id, f := range byID {
		if f.Severity != "must" || f.Status == "open" || seen[id] {
			continue
		}
		if newestCycle > 0 && cycleOf[id] != newestCycle {
			continue
		}
		settledIDs = append(settledIDs, id)
	}
	sort.Strings(settledIDs)
	// The plane serves only the newest records, and their findings within a byte
	// budget (get_task's reviews block); open MUST ids survive the trim, settled
	// ones do not. Say what was left out, so a verifier never reads a partial
	// settled list as the whole of it.
	var unserved []string
	for _, r := range records {
		if r.State == "sealed" || r.State == "void" {
			continue
		}
		if newestCycle > 0 && r.Cycle != newestCycle {
			continue
		}
		if r.FindingsTotal > len(r.Findings) {
			unserved = append(unserved, fmt.Sprintf("round %d: %d of %d finding(s) not served", r.Round, r.FindingsTotal-len(r.Findings), r.FindingsTotal))
		}
	}
	sort.Strings(unserved)
	if recordsTotal > len(records) {
		unserved = append(unserved, fmt.Sprintf("%d older review record(s) not served", recordsTotal-len(records)))
	}
	var b strings.Builder
	b.WriteString("The driver read the released durable review records before this round. ")
	if newestRound > 0 {
		tip := newestTip
		if tip == "" {
			tip = "(the record names no tip)"
		}
		fmt.Fprintf(&b, "The newest released round is %d and its reviewed tip is %s — that tip is the fix delta's base (`git diff <reviewedTip>..HEAD`).\n", newestRound, tip)
	} else {
		b.WriteString("No RELEASED review record is on the task. If the review round did not run, that is a failed hand-off to report in your record and hand over to a human; do not invent findings.\n")
	}
	if len(openIDs) == 0 && len(settledIDs) == 0 && len(unserved) == 0 {
		b.WriteString("The plane currently shows NO OPEN MUST on a released record. Still verify the fix delta of the newest round and its regression surface; if it is clean, carry the terminal step. Everything you find goes in this round's own record.\n\n")
		return b.String()
	}
	switch {
	case len(openIDs) == 0 && len(settledIDs) == 0:
		b.WriteString("The plane currently shows NO OPEN MUST on a released record, but it did not serve every finding (below).\n")
	case len(openIDs) == 0:
		b.WriteString("The plane currently shows NO OPEN MUST, but the MUST(s) below are only MARKED settled; adjudicate each before you carry the terminal step.\n")
	default:
		b.WriteString("The following MUST(s) are OPEN and this round MUST adjudicate each BY ITS PLANE ID — resolved (status \"fixed\"), resolved by dispatch (status \"filed\"), refutation accepted (status \"refuted\"), or left \"open\" with a recorded reason:\n")
		for _, id := range openIDs {
			b.WriteString(digestFindingLine(id, byID))
		}
	}
	if len(settledIDs) > 0 {
		b.WriteString("The following MUST(s) of this review cycle are already MARKED settled on the plane — by the fix phase, or by an earlier verification round. A fixer's own marking is its claim, not a verdict, and a refutation changes no code, so the fix delta will not show it: check each one against HEAD and its stated evidence. CONFIRM it by leaving its status as it is. REJECT it by setting its status back to \"open\" through " + config.ReviewFindingsCall + " and recording why in this round's record; a rejected MUST is open, so the gate does not pass and the driver spawns another fix:\n")
		for _, id := range settledIDs {
			b.WriteString(digestFindingLine(id, byID))
		}
	}
	if len(unserved) > 0 {
		b.WriteString("The plane did NOT serve every review finding (" + strings.Join(unserved, "; ") + "), so a MUST marked settled in what was left out is not listed above. Before you carry the terminal step, read the full records (export_project carries every record in full, and the task's console page renders them) and adjudicate every MUST of this cycle marked settled there too. If you cannot read them, do NOT carry the terminal step: record in this round's record that it could not see every settled MUST, and end, so a human looks.\n")
	}
	b.WriteString("\n")
	return b.String()
}

// planeUnparkOption is the plane's own unpark offer, byte for byte
// (ESCALATION_UNPARK_OPTION, apps/web/src/server/mcp/stale.ts). The console acts
// on an answered question by VALUE: when the operator picks exactly this string,
// it sets the task back to ready. Any other wording is prose the plane does not
// act on, so the round-cap park offers this one verbatim for the unpark path.
const planeUnparkOption = "Set it back to `ready` — the blocker is gone, let the fleet try again"

// mergedFindingsDigest is the fix phase's work list when the review ran as a
// PANEL (CLA-639, B11): every finding from every member's released record, in
// one list, with the union rule the design settled on (A1).
//
//   - Every MUST from ANY member is open and printed by its plane id. Consensus
//     never filters a MUST out — the evidence that motivated the panel (section
//     1.2) is two blockers a single reviewer missed entirely, so the union is the
//     gate.
//   - SHOULD and NIT are listed with the members that raised them, so the fixer
//     can see which are opinions repeated by two reviewers and which are one
//     reviewer's preference. They are recorded, never gating.
//
// The driver builds the list, never the fixer (B11/D3): the fixer applies it and
// has no vote on severity — a MUST it disagrees with is refuted through the
// plane, not downgraded here.
//
// expected is the panel's reviewer roles, in order, as the driver resolved them.
// A role with no record in round is named INCOMPLETE: that is the member a
// dead, capped or failed session never got to record, and the fixer must know
// the list is the union of the reviewers that RAN, not of the panel that was
// meant to. round is the panel's round; 0 means no record was matched at all.
//
// Round and status are BOTH filters (CLA-639 S1). The round is the one this
// panel actually wrote: listing every released record would relist an earlier
// cycle's findings after a verify rewind, and a member that recorded in an
// earlier round would count as having recorded THIS one. The status filter
// drops every finding the plane already shows settled — fixed, refuted or filed
// by the fix phase's own findingUpdates, or by a verifier — because handing the
// fixer a settled MUST back as open work is how a fix round spends itself
// re-fixing what already landed. An open (`""`/`open`) finding is the list.
//
// Empty only when there is nothing to grade (no member recorded and none is
// missing) — the shipped single-review path then keeps its own digest, so a
// config with no panel is byte-for-byte unchanged.
//
// failed carries, per reviewer role, the reason that member recorded nothing
// and whether the seat was a TRIAL one (CLA-639 M4, CLA-784): naming the
// missing member is not enough for the fixer to judge whether the short list is
// bad luck or a member that never ran, and a trial seat's failure must not read
// as a lost required reviewer.
func mergedFindingsDigest(records []plane.ReviewRecordState, expected []string, round int, failed map[string]panelMemberFailure) string {
	type merged struct {
		finding plane.ReviewFindingState
		record  plane.ReviewRecordState
	}
	byID := map[string]merged{}
	var ids []string
	recorded := map[string]bool{}
	var unserved []string
	settled := 0
	for _, r := range records {
		if r.State == "sealed" || r.State == "void" || (round > 0 && r.Round != round) {
			continue
		}
		// Only a RELEASED record counts as this member having recorded — a sealed
		// one exists but its findings are withheld, so the fixer is seeing the
		// panel's shape without its content. It counts even when EVERY finding in
		// it is settled: the member did record, and saying it did not would read
		// as a lost reviewer.
		recorded[r.ReviewerRole] = true
		if r.FindingsTotal > len(r.Findings) {
			unserved = append(unserved, fmt.Sprintf("%s: %d of %d finding(s) not served", r.ReviewerRole, r.FindingsTotal-len(r.Findings), r.FindingsTotal))
		}
		for _, f := range r.Findings {
			// A finding the plane already shows settled is not open work. The
			// statuses are the plane's own (CLA-636): "", "open", "fixed",
			// "refuted", "filed", and any future one is treated as NOT open — a
			// status the driver does not know is not evidence that work remains.
			switch f.Status {
			case "", "open":
			default:
				settled++
				continue
			}
			if _, seen := byID[f.ID]; !seen {
				ids = append(ids, f.ID)
			}
			byID[f.ID] = merged{finding: f, record: r}
		}
	}
	var missing []string
	for _, role := range expected {
		if !recorded[role] {
			missing = append(missing, role)
		}
	}
	// Nothing to grade at all: no finding to list, no member missing, and nothing
	// the plane shows settled either — the shipped single-review path then keeps
	// its own digest, byte for byte. A round whose findings are ALL settled is
	// not nothing: the fixer is told what this round holds and that every one of
	// them is already answered.
	if len(ids) == 0 && len(missing) == 0 && settled == 0 {
		return ""
	}
	sort.Strings(ids)
	sort.Strings(unserved)

	var b strings.Builder
	b.WriteString("The driver read the released durable review records of this review panel before this fix round. ")
	b.WriteString("This is the MERGED list from every member that recorded: every MUST from ANY member is open and is yours to fix or refute by its plane id — a MUST raised by one reviewer is never dropped because another did not raise it. ")
	b.WriteString("SHOULD and NIT findings are annotated with the members that raised them; they are recorded, never gating.\n")
	if round > 0 {
		fmt.Fprintf(&b, "The list is round %d's records only — an earlier round's findings, and anything already marked fixed, refuted or filed on the plane, are not relisted here.\n", round)
	}
	if settled > 0 {
		fmt.Fprintf(&b, "%d finding(s) of this round are already MARKED settled on the plane and are not listed; `reviews.openMustIds` is the plane's own open set, and a settled marking you disagree with is refuted through "+config.ReviewFindingsCall+", not by re-fixing it blind.\n", settled)
	}
	// CLA-639 M5: a panel member writes no `## Review` block — its record is the
	// durable one — so the fixer must not wait for a block to preserve, and its
	// own outcome is where this merged list reaches the task.
	b.WriteString("No panel member wrote a `## Review` block into the task's outcome (each writes only its sealed record), so there is no block to keep verbatim and no legacy copy to rely on: your outcome is the only place this round's result reaches the task. Keep everything the outcome already carries unchanged, answer EVERY finding above in it by its plane id (severity, one line on what it was, and what you did about it), and add your `### Fixes` list.\n")
	if len(missing) > 0 {
		// CLA-784: a trial seat is marked `(optional)` in the fix brief exactly
		// as the daemon log marks it — in BOTH the INCOMPLETE sentence and the
		// per-member line — so the fixer never reads a lost trial seat as a lost
		// required reviewer.
		names := make([]string, len(missing))
		for i, role := range missing {
			names[i] = panelIncompleteRole(role, failed[role].optional)
		}
		fmt.Fprintf(&b, "The panel was INCOMPLETE: %s recorded no review this round (dead, capped or failed before recording), so the merged list below is the union of the reviewers that ran, not of the panel that was configured.\n", strings.Join(names, ", "))
		for _, role := range missing {
			if why := failed[role].reason; why != "" {
				fmt.Fprintf(&b, "- %s: %s\n", panelIncompleteRole(role, failed[role].optional), why)
			}
		}
	}
	if len(unserved) > 0 {
		fmt.Fprintf(&b, "The plane did not serve every finding (%s); read the full records before you close the round.\n", strings.Join(unserved, "; "))
	}
	b.WriteString("\n")
	for _, sev := range []string{"must", "should", "nit"} {
		var group []merged
		for _, id := range ids {
			if strings.EqualFold(byID[id].finding.Severity, sev) {
				group = append(group, byID[id])
			}
		}
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s (%d):\n", strings.ToUpper(sev), len(group))
		for _, m := range group {
			b.WriteString(mergedFindingLine(m.finding, m.record))
			if sev != "must" {
				fmt.Fprintf(&b, "  [raised by %s]\n", m.record.ReviewerRole)
			}
		}
	}
	b.WriteString("\n")
	return b.String()
}

// mergedFindingLine is one finding as the merged list prints it: its plane id,
// reviewer, location, summary and required fix — the same fields the shipped
// digest prints, plus the member, because "who raised this" is the field the
// panel exists to make visible.
func mergedFindingLine(f plane.ReviewFindingState, r plane.ReviewRecordState) string {
	loc := f.File
	if f.Line > 0 {
		loc = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	if loc == "" {
		loc = "(no location)"
	}
	summary := strings.TrimSpace(f.Summary)
	required := strings.TrimSpace(f.RequiredFix)
	if required == "" {
		required = "(none stated)"
	}
	line := fmt.Sprintf("- %s [%s, round %d, %s] %s Required fix: %s", f.ID, r.ReviewerRole, r.Round, loc, summary, required)
	if evidence := strings.TrimSpace(f.Evidence); evidence != "" {
		kind := strings.TrimSpace(f.EvidenceKind)
		if kind == "" {
			kind = "evidence"
		}
		line += fmt.Sprintf(" [%s: %s]", kind, evidence)
	}
	return line + "\n"
}

// panelRoles lists the reviewer roles a fix phase's prompt should treat as the
// panel: the member phases immediately before it in the CAPTURED sequence.
// Empty when the phase does not follow a panel, which is the shipped
// single-review shape.
//
// The role is POSITIONAL (`reviewerRoleFor`), never the member's log label. The
// durable record and the seam gate are keyed on the positional role, so a named
// member (`alpha`) writes `review-1`; returning the label here would make the
// merged list look for an `alpha` record and report a member that recorded as
// INCOMPLETE (CLA-639).
func panelRoles(phases []config.Phase, fixIdx int) []string {
	var roles []string
	for i := fixIdx - 1; i >= 0 && phases[i].IsPanelMember(); i-- {
		roles = append([]string{reviewerRoleFor(phases, i)}, roles...)
	}
	return roles
}

// openMustFixDigest is the fix phase's work list across rounds (CLA-671): every
// MUST the plane shows open on any released record, with its detail. Empty when
// none is open, which on a first fix round is the review's own list anyway.
func openMustFixDigest(records []plane.ReviewRecordState, openMustIDs []string) string {
	if len(openMustIDs) == 0 {
		return ""
	}
	byID := map[string]plane.ReviewFindingState{}
	for _, r := range records {
		if r.State == "sealed" || r.State == "void" {
			continue
		}
		for _, f := range r.Findings {
			byID[f.ID] = f
		}
	}
	ids := append([]string(nil), openMustIDs...)
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("The driver read the plane before this fix round. These MUST(s) are OPEN on released review records, from ANY round, and every one is yours to fix or refute by its plane id — not only the findings in the newest record:\n")
	for _, id := range ids {
		b.WriteString(digestFindingLine(id, byID))
	}
	b.WriteString("\n")
	return b.String()
}

// digestFindingLine is one MUST as the verification digest lists it: id, round,
// location, what is wrong and the required fix, plus — for a finding already
// marked settled — its status and the evidence the marking carries.
func digestFindingLine(id string, byID map[string]plane.ReviewFindingState) string {
	f, ok := byID[id]
	if !ok {
		return fmt.Sprintf("- %s [finding detail not served — read it from `reviews.records`]\n", id)
	}
	loc := f.File
	if f.Line > 0 {
		loc = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	if loc == "" {
		loc = "(no location)"
	}
	round := "?"
	if r, ok := findingIDRound(f.ID); ok {
		round = strconv.Itoa(r)
	}
	summary := strings.TrimSpace(f.Summary)
	required := strings.TrimSpace(f.RequiredFix)
	if required == "" {
		required = "(none stated)"
	}
	line := fmt.Sprintf("- %s [round %s, %s] %s Required fix: %s", f.ID, round, loc, summary, required)
	switch f.Status {
	case "open", "":
	case "fixed":
		line += fmt.Sprintf(" MARKED fixed (fixCommit: %s).", orNone(f.FixCommit))
	case "refuted":
		line += fmt.Sprintf(" MARKED refuted (reason: %s).", orNone(f.RefutedReason))
	case "filed":
		line += fmt.Sprintf(" MARKED filed (task: %s).", orNone(f.FiledTaskRef))
	default:
		line += fmt.Sprintf(" MARKED %s.", f.Status)
	}
	return line + "\n"
}

// orNone is a digest field's text, or a visible placeholder when the plane
// carries none, so a marking with no evidence reads as exactly that.
func orNone(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return "none given"
	}
	return s
}

// branchlessVerificationDigest is verificationDigest for a BRANCHLESS task
// (CLA-638 × CLA-670): the branchless review writes no durable record (question
// 03a12578, option 1), so there are no plane finding ids for the round to
// adjudicate. The digest therefore hands the verifier the LAST `## Review`
// block itself — the block IS this shape's round record — and says so up front,
// because the empty record set is expected here and must not read as a failed
// hand-off.
func branchlessVerificationDigest(outcome string) string {
	block := lastReviewBlock(outcome)
	if block == "" {
		return "The driver read the task before this round. This is a BRANCHLESS task (its correct delivery is no code) and the outcome carries NO `" +
			config.ReviewRecordHeading + "` block: the review round either did not run or recorded nothing. Report that as a failed hand-off in your own block and hand over to a human; do not invent findings.\n\n"
	}
	return "The driver read the task before this round. This is a BRANCHLESS task (its correct delivery is no code): the branchless review writes no durable record and there are no plane finding ids (question 03a12578, option 1), so the LAST `" +
		config.ReviewRecordHeading + "` block below IS this round's record — adjudicate it in its own terms, not by a plane id. The fix round amended it; verify the amendment.\n\n" + block + "\n"
}

// resolvedFindingIDs lists the MUST ids on released records that a verifier has
// ALREADY marked resolved (fixed, filed or refuted). The driver remembers them
// across the round loop: an id that is open again in a later round was reopened
// after a verifier marked it resolved, which is non-convergence and parks the
// task (A6). Sealed and void records contribute nothing, exactly as in the
// digest.
func resolvedFindingIDs(records []plane.ReviewRecordState) []string {
	var out []string
	for _, r := range records {
		if r.State == "sealed" || r.State == "void" {
			continue
		}
		for _, f := range r.Findings {
			if f.Severity == "must" && f.Status != "" && f.Status != "open" {
				out = append(out, f.ID)
			}
		}
	}
	return out
}

// openMustDetails pairs each open MUST id with its served summary, so the park
// question carries what is actually open rather than only the ids. A finding
// the read did not serve is still named by id.
func openMustDetails(openIDs []string, records []plane.ReviewRecordState) []string {
	summary := map[string]string{}
	for _, r := range records {
		if r.State == "sealed" || r.State == "void" {
			continue
		}
		for _, f := range r.Findings {
			if s := strings.TrimSpace(f.Summary); s != "" {
				summary[f.ID] = s
			}
		}
	}
	out := make([]string, 0, len(openIDs))
	for _, id := range openIDs {
		if s, ok := summary[id]; ok {
			out = append(out, id+" — "+s)
		} else {
			out = append(out, id)
		}
	}
	return out
}

// nonConvergenceReason reports why the round loop must stop and park rather than
// spawn another fix (CLA-638, A6): a MUST a previous verifier marked resolved
// that the latest round has open again, or a MUST still open after two fix
// attempts (its id's round prefix is at least two behind the round that just
// closed). Empty means the loop may continue. It reads plane state plus the
// driver's in-memory resolved set, so a takeover (empty set) loses the reopened
// half but the round-prefix half still holds.
func nonConvergenceReason(open []string, round int, resolved map[string]bool) string {
	for _, id := range open {
		if resolved[id] {
			return fmt.Sprintf("finding %s was marked resolved by an earlier verification round and is open again", id)
		}
	}
	for _, id := range open {
		if r, ok := findingIDRound(id); ok && round-r >= 2 {
			return fmt.Sprintf("finding %s (raised in round %d) is still open in round %d after two fix attempts", id, r, round)
		}
	}
	return ""
}

// parkVerification parks a task whose verification round cannot proceed
// (CLA-638): the round cap with an open MUST (B7), the non-convergence breaker
// (A6), or a round the driver could not judge. The question CARRIES THE RECORDS
// — the open ids, the round, and where the durable records live — because the
// operator is being asked to overrule the stopping rule, and that needs the
// evidence.
//
// Non-blocking `clarification`, matching parkDeadPhase: the task is already
// parked, so a blocking question would be the wrong shape (any answer would
// un-block it to `ready`, handing the same open MUSTs straight back to a fleet
// whose cycle counter has not moved — the cap would park it again). The park's
// outcome stands alone if the question insert fails.
func (d *Driver) parkVerification(ctx context.Context, t Target, res harness.Result, reason string, round, counted int, open, details []string) {
	if res.Claim.TaskID == "" {
		log.Printf("%sverification park: no claim observed, so there is no task to park (%s)", labelOf(t), reason)
		return
	}
	pk, ok := t.Releaser.(plane.ParkAPI)
	if !ok {
		log.Printf("%sCANNOT PARK %s — the verification round cannot proceed (%s), but the releaser cannot park or raise a question; the task stays in the queue and the next claim will retry it again",
			labelOf(t), claimLabel(res.Claim), reason)
		return
	}
	ref := claimLabel(res.Claim)
	openList := "none"
	detailList := "none"
	if len(open) > 0 {
		openList = strings.Join(open, ", ")
	}
	if len(details) > 0 {
		detailList = "\n- " + strings.Join(details, "\n- ")
	}
	outcome := fmt.Sprintf(
		"Parked on the review stopping rule: %s. Round %d of the %d-round cap this cycle (non-void rounds counted by the plane); open MUSTs: %s. The durable review records are on the task (`reviews.records`). Setting this task back to ready does not start a new review cycle (the plane counts cycles by human verdicts, which it records only on an in_review task): a re-run gets one more review, fix and verification round, and parks again if any MUST is still open after it. The operator unparks it for that attempt, settles the open MUST(s) by hand, re-files the work as a new task, or drops it.",
		reason, round, config.ReviewRoundCap, openList)
	body := fmt.Sprintf(
		"**%s needs a human decision: the fix-verification loop stopped.** %s\n\n"+
			"Round %d closed; the plane has counted %d non-void round(s) in this review cycle and the cap is %d. "+
			"Open MUST finding(s): %s\n\n"+
			"The evidence is in the task's durable review records (`reviews.records`; every finding is `must`/`should`/`nit` with its status, and the legacy `## Review` block is in the outcome). "+
			"The stopping rule exists so two agents cannot argue past a bound: the reviewer's findings, the fixer's answers and the verifier's adjudication are all on the task, and the disagreement is the question.\n\n"+
			"Setting this task back to `ready` does NOT give it another review cycle. The plane opens a new cycle only on a human verdict, and it records a verdict only on an `in_review` task, so a re-run stays in this cycle: it gets exactly one more review, fix and verification round, and any MUST still open after that verification round parks the task again. For more rounds than that, re-file the work as a new task with whatever re-scoping the open MUST(s) need.",
		ref, reason, round, counted, config.ReviewRoundCap, detailList)
	options := []string{
		planeUnparkOption,
		"I will settle the open MUST(s) myself — fix it by hand and close the task",
		"Re-file it as a new task, re-scoped so the open MUST(s) can be met — archive this one",
		"Drop it — the task is not worth another round",
	}
	pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer pcancel()
	if err := pk.Park(pctx, res.Claim.TaskID, res.Claim.RunID, outcome); err != nil {
		log.Printf("%sverification park failed for %s: %v — the task is left for the next claim to retry it again",
			labelOf(t), ref, err)
		return
	}
	// The park moved the task out of the holding run: nothing is left for a
	// supervisor to hand back (CLA-708), exactly as a settle observed from the
	// stream would have said.
	d.clearCurrentClaim()
	if err := pk.AskQuestion(pctx, res.Claim.TaskID, body, options, false, "clarification"); err != nil {
		log.Printf("%sparked %s — %s, but the question for the operator could not be filed: %v",
			labelOf(t), ref, reason, err)
		return
	}
	log.Printf("%sparked %s — %s; question filed for the operator", labelOf(t), ref, reason)
}

// verifiedBranch returns the branch the previous phase recorded — the hand-off
// the successor's brief names (CLA-457). The seam folds the branch onto the
// carried claim when it checkpoints, so for a checkpointed phase the claim's
// branch is the one the evidence gate judged; the recorded-Branches fallback
// covers a claim that was never re-judged (a resumed-with-WIP predecessor, or
// a defensive hole). The predecessor's checkpoint is gated on this same
// evidence, so an empty return here is a misconfigured sequence announcing
// itself via a standing {{branch}} placeholder rather than a plausible runtime
// state.
//
// CLA-576: for an UNVERIFIED checkpoint — the origin could not be read — the
// returned branch is recorded but NOT verified, and the caller that builds the
// successor's brief must read Result.CheckpointUnverified and select the
// unverified brief: this function names the branch, it does not vouch for it.
func verifiedBranch(res *harness.Result) string {
	if res == nil {
		return ""
	}
	if res.Claim.Branch != "" {
		return res.Claim.Branch
	}
	if bs := recordedBranches(*res); len(bs) > 0 {
		return bs[0]
	}
	return ""
}

// attributionOr renders a harness/model value for a phase brief's record block.
// An empty model is normal (the harness default) and must not render as the bare
// word "model" followed by nothing, which reads like a truncated write.
func attributionOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// peekCheckpointClaim asks the plane whether the task this phase was BRIEFED on
// is now held, with a branch recorded, by some run of this project's key — the
// claim a session took through a channel the driver cannot observe (CLA-451:
// a raw API call with the same credential, a harness quirk, a renamed tool).
// It returns that claim as a synthesized Result for the seam to gate and carry,
// or nil when today's ending should stand.
//
// nil is returned — and the standard ending follows — when: the driver never
// knew which task it briefed (empty id; nothing to ask about); the target has
// no plane reads wired; the read fails (a plane blip degrades exactly like the
// spawn-dir peek does); or the answer is not unambiguous custody — status
// still "in_progress", a holding run, AND a recorded branch, all three. A task
// that moved on (reviewed, done, released) was settled by its own session and
// the sequence is over; a hold without a branch is the empty exit CLA-457
// already refuses to advance on.
//
// CLA-476: the hold must also be NEW. `hold` is the task's custody at SPAWN
// time (see briefedHold); a task already held-with-branch then is refused, and
// only a task that moved into that shape during the phase is recovered. The one
// exception is the exact task a predecessor held (hold.Exact), whose identity is
// not a guess.
//
// The transition is necessary, not sufficient: a task that was `ready` at spawn
// and is held at the end may have been claimed by a DIFFERENT run during the
// phase. That run's branch is likewise not this phase's work, but the blind path
// cannot tell — it never saw the claim, and the plane does not say which key
// holds the task. Closing it needs the end-time ClaimedByRun compared against a
// run this drain actually held (CLA-888).
//
// The three fields together are also the honest boundary of what this can
// know: the plane does not expose WHICH key holds a task, so "some run of this
// project's credential" is approximated by "held at all". The driver shares
// one key with every session it spawns, the briefed task came from this
// driver's own dispatch, and a wrong attribution fails safe — a foreign run's
// id seeds a heartbeat the plane refuses, ending the drain loudly rather than
// silently stranding work.
func (d *Driver) peekCheckpointClaim(ctx context.Context, t Target, drainNum int, phaseLabel, briefedTask string, hold briefedHold) *harness.Result {
	if briefedTask == "" {
		return nil
	}
	src, ok := t.Releaser.(plane.TaskStateSource)
	if !ok {
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	st, err := src.TaskState(sctx, briefedTask)
	switch {
	case err == nil:
	case errors.Is(err, plane.ErrNotWired):
		return nil
	default:
		log.Printf("%siteration %d: %s ended without an observed claim; the plane read of %s failed (%v) — keeping today's ending rather than guessing",
			labelOf(t), drainNum, phaseLabel, briefedTask, err)
		return nil
	}
	if st.Status != "in_progress" || st.ClaimedByRun == "" || st.Branch == "" {
		return nil
	}
	if !hold.Exact && hold.HeldWithBranch {
		held := "a branch (not read at spawn)"
		if hold.Branch != "" {
			held = fmt.Sprintf("branch %q", hold.Branch)
		}
		log.Printf("%siteration %d: %s ended without an observed claim, but %s was ALREADY held with %s when the phase spawned — that hold predates the phase (a stale-lease takeover candidate or another run's work), so it is not this phase's checkpoint; keeping today's ending",
			labelOf(t), drainNum, phaseLabel, briefedTask, held)
		return nil
	}
	return &harness.Result{Claim: harness.Claim{
		TaskID: briefedTask,
		Ref:    st.Ref,
		RunID:  st.ClaimedByRun,
		HasWIP: true,
		Branch: st.Branch,
	}}
}
