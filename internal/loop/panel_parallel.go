package loop

// panel_parallel.go — running a review panel's members concurrently (CLA-697).
//
// The panel (CLA-639) expands one review phase into N review-shaped phases.
// Until CLA-697 the phase loop ran them one after another, which made a round
// cost the SUM of its members' sessions. The members are independent by design
// (D5): the same diff, no fixes, and no member sees another's findings until the
// driver closes the round. Nothing about the review needs the ordering, so the
// fan-out is driver-level: this file spawns the contiguous run of panel-member
// phases at once, joins, and lets the ordinary phase loop continue at the fix.
//
// Two things make concurrency safe that are NOT this file's invention:
//
//   - the plane's heartbeat is an idempotent lease renewal — one UPDATE of
//     last_heartbeat_at and `heartbeat_count + 1`, with no version predicate
//     (plane: apps/web/src/repositories/runs.ts heartbeat), so N members
//     renewing ONE run id cannot corrupt each other; and
//   - a review record is keyed by (round, reviewerRole), and each member has a
//     distinct positional role, so concurrent appends land as distinct rows in
//     the same round (plane: apps/web/src/repositories/task-reviews.ts append).
//     The round number is derived from the rows already present (max + 1 when
//     none is sealed), which is deterministic for concurrent first writers; the
//     duplicate check is per role, which is exactly what makes two DIFFERENT
//     roles safe and a repeated role a retry.
//
// What IS this file's job is keeping the members from sharing a checkout. Each
// member gets a DETACHED worktree at the reviewed tip, created here and removed
// when the member ends. The worktree is throwaway by construction: a review-only
// session changes nothing, salvage is already disabled for a review-shaped
// phase, and a dirty member tree is logged and DISCARDED — never committed,
// never handed to the fix phase.
//
// With `review_panel_concurrency: 1` none of this runs: the phase loop's shipped
// sequential path executes, byte for byte, worktree and all.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

const (
	// panelFetchTimeout bounds the one network call a fan-out may need: fetching
	// a reviewed tip this checkout does not yet hold.
	panelFetchTimeout = 30 * time.Second
	// panelCleanupTimeout bounds removing a member's throwaway worktree. It runs
	// detached from the drain context so a cancelled run still cleans up, which
	// means it needs a bound of its own.
	panelCleanupTimeout = 30 * time.Second
)

// panelMemberOutcome is one member's finished pass, before the join folds it
// into the drain's panel bookkeeping.
type panelMemberOutcome struct {
	label string
	role  string
	// setupErr is a member whose own worktree could not be made. It is a FAILED
	// member (recorded as INCOMPLETE), never a run error: one reviewer's checkout
	// failing says nothing about the others.
	setupErr string
	// end/perr/stop are exactly drainPhase's returns.
	end    phaseEnd
	perr   error
	stop   bool
	tokens int
	cost   float64
}

// panelRunOutcome is what the join hands back to drainPhases: the accumulated
// spend and sessions, the member results in spawn order, and the panel-level
// facts (round, incomplete list, whether anyone recorded, whether the join owes
// a wait) it derives from them.
type panelRunOutcome struct {
	members []panelMemberOutcome
	tokens  int
	cost    float64
	stop    bool
	err     error

	recorded    bool
	round       int
	incomplete  []string
	failed      map[string]string
	closeFailed bool

	// limitWait/sequenceEnded carry a member's run-level sequence end (a rolling
	// limit or a transient failure) for the drain to take after the join, exactly
	// as the sequential member handler does.
	limitWait     *phaseLimitWait
	sequenceEnded bool
	transientKey  resumedTransientKey

	// claim is the last member claim observed, for the drain's iteration record
	// (the deferred handback keeps `carried`, the run the panel resumed).
	claim       *harness.Result
	disposition string

	// finalDead/lastProducedNothing mirror the LAST member's classification for
	// the iteration record, exactly as the sequential loop's locals do.
	finalDead           bool
	lastProducedNothing bool
}

// runPanelInParallel runs phases[start:end] concurrently, each in its own
// detached worktree at the reviewed tip. ok=false means the fan-out could not be
// set up at all (no live claim, no repo, no resolvable tip) and the caller must
// run the shipped sequential path instead — a worktree failure that would only
// make one member fail does not refuse the panel.
func (d *Driver) runPanelInParallel(ctx context.Context, drainNum, ti int, t Target, phases []config.Phase, start, end, concurrency int, carried *harness.Result, prior spend) (*panelRunOutcome, bool) {
	if carried == nil || !carried.Claim.Held() {
		log.Printf("%siteration %d: the review panel has no live claim to resume — running its members sequentially",
			labelOf(t), drainNum)
		return nil, false
	}
	baseDir, _, _, err := d.sessionDir(ctx, t, carried)
	if err != nil {
		log.Printf("%siteration %d: the review panel could not resolve its checkout (%v) — running its members sequentially",
			labelOf(t), drainNum, err)
		return nil, false
	}
	// sessionDir returns "" for the legacy single-repo shape (nothing declared
	// to resolve); the session then runs in the configured workdir, and so must
	// the git probes. Never the process cwd: `git -C ""` reads the driver's own
	// checkout, which is not the task's.
	if baseDir == "" {
		baseDir = t.WorkDir
		if baseDir == "" {
			baseDir = d.cfg.WorkDir
		}
	}
	repoRoot, err := panelRepoRoot(ctx, baseDir)
	if err != nil {
		log.Printf("%siteration %d: the review panel could not find the repository at %s (%v) — running its members sequentially",
			labelOf(t), drainNum, baseDir, err)
		return nil, false
	}
	tip, ok := panelReviewedTip(ctx, baseDir, carried)
	if !ok {
		log.Printf("%siteration %d: the review panel could not resolve the reviewed tip of %s — running its members sequentially so each can establish it itself",
			labelOf(t), drainNum, carried.Claim.Branch)
		return nil, false
	}

	out := &panelRunOutcome{failed: map[string]string{}, sequenceEnded: false}
	n := end - start
	out.members = make([]panelMemberOutcome, n)
	// One presence beacon for the panel, not one per member: beginIteration also
	// writes the local BEACON file (remove-then-create), and N goroutines doing
	// that to one name would race for a line nobody reads mid-panel. The sessions
	// themselves are reported per member in the iteration record's Sessions.
	d.beginIteration(ti, t, drainNum, claimLabel(carried.Claim), phases[start].Label(start))
	log.Printf("%siteration %d: review panel: running %d members concurrently (cap %d), each in its own detached worktree at %s",
		labelOf(t), drainNum, n, concurrency, shortSHA(tip))

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for k := 0; k < n; k++ {
		idx := start + k
		ph := phases[idx]
		label := ph.Label(idx)
		role := reviewerRoleFor(phases, idx)
		last := idx == len(phases)-1
		wg.Add(1)
		go func(k, idx int, ph config.Phase, label, role string, last bool) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			m := panelMemberOutcome{label: label, role: role}
			dir, werr := d.createPanelWorktree(ctx, repoRoot, tip, label)
			if werr != nil {
				m.setupErr = "could not create its own detached worktree: " + werr.Error()
				out.members[k] = m
				return
			}
			defer d.removePanelWorktree(repoRoot, dir)

			tag := "-p" + label
			m.tokens, m.cost, m.stop, m.end, m.perr = d.drainPhase(
				ctx, drainNum, ti, idx, tag, ph, last, carried, t, prior, phases,
				panelState{}, phaseRunOpts{worktree: dir, panelParallel: true})
			out.members[k] = m
		}(k, idx, ph, label, role, last)
	}
	wg.Wait()

	for k, m := range out.members {
		out.tokens += m.tokens
		out.cost += m.cost
		if m.stop {
			out.stop = true
		}
		if m.perr != nil && out.err == nil {
			out.err = m.perr
		}
		if m.end.claim != nil {
			out.claim = m.end.claim
			out.disposition = claimDisposition(m.end.claim)
		}
		out.finalDead = m.end.dead
		out.lastProducedNothing = m.end.producedNothing
		out.round = max(out.round, m.end.reviewRound)
		if m.end.sequenceEnd != nil && !out.sequenceEnded {
			out.sequenceEnded = true
			out.limitWait = m.end.sequenceEnd.limitWait
			if out.limitWait == nil {
				// The drain notes the streak when it takes the wait; this is the
				// target+harness whose ladder the failure belongs to.
				out.transientKey = resumedTransientKey{target: ti, harness: d.targetCfg(t).HarnessFor(phases[start+k])}
			}
		}
	}

	// Judge every member exactly as the sequential handler does: recorded, or
	// named INCOMPLETE with the reason. The plane is asked only for the members
	// whose session ended the sequence before the seam could match the record —
	// the same narrow question the sequential path asks (CLA-639 fix pass 4).
	live := carried != nil && carried.Claim.Held()
	for k, m := range out.members {
		recorded := m.end.reviewRecorded || m.end.reviewCloseFailed
		if !recorded && m.end.sequenceEnd != nil && carried != nil && (carried.CheckpointTip != "" || carried.CheckpointUnverified) {
			if rec, matched := d.panelMemberRecordMatch(ctx, t, *carried, m.role); matched {
				recorded = true
				if rec.Round > 0 {
					out.round = max(out.round, rec.Round)
				}
			}
		}
		if recorded {
			out.recorded = true
			d.panelMemberRecorded(m.label)
			continue
		}
		reason := panelMemberFailureReason(m)
		out.incomplete = append(out.incomplete, m.role+": "+reason)
		out.failed[m.role] = reason
		log.Printf("%siteration %d: the %s panel member recorded nothing (%s) — recording it as failed",
			labelOf(t), drainNum, m.label, reason)
		if !m.end.deadBooked {
			d.tallyEmpty(m.label, d.targetCfg(t).HarnessFor(phases[start+k]))
		}
		if n := d.panelMemberFailed(m.label); n >= panelMemberFailBound {
			ph := phases[start+k]
			model, _ := d.targetCfg(t).ModelForPhase(ph)
			log.Printf("%siteration %d: WARNING — the %s review-panel member has recorded nothing on %d consecutive tasks (harness %s, model %s); the panel runs on and the fix applies what the other members found, but this member's harness, model or brief is not producing reviews — check its iteration logs",
				labelOf(t), drainNum, m.label, n, d.targetCfg(t).HarnessFor(ph), attributionOr(model, "harness default"))
		}
	}

	// Close the round once, by the live holder, before anything hands the claim
	// back — a sealed round would be voided by a takeover and every member's
	// record lost. A close is owed only when a member recorded; a panel where
	// none did has no round to release and ends without a fix.
	if out.recorded && out.round > 0 && live {
		if cerr := d.closeReviewRound(ctx, t, *carried, out.round); cerr != nil {
			log.Printf("%siteration %d: the panel's round %d could not be closed (%v) — ending the drain without spawning the fix: a fix spawned now would run against findings the plane still has sealed",
				labelOf(t), drainNum, out.round, cerr)
			out.closeFailed = true
		}
	}
	if !out.recorded {
		log.Printf("%siteration %d: no member of the review panel recorded — the round is not closed and no fix spawns", labelOf(t), drainNum)
	}
	return out, true
}

// panelMemberFailureReason names why a member produced no durable record, in the
// same words the sequential handler uses.
func panelMemberFailureReason(m panelMemberOutcome) string {
	switch {
	case m.setupErr != "":
		return m.setupErr
	case m.end.dead:
		return "died producing nothing"
	case m.end.sequenceEnd != nil && m.end.sequenceEnd.limitWait != nil:
		return "hit a usage limit before recording"
	case m.end.sequenceEnd != nil:
		return "ended on a transient failure before recording"
	case m.perr != nil:
		return failureDetail(m.perr.Error())
	case m.end.empty:
		return m.end.emptyWhy
	default:
		return "recorded no durable review record"
	}
}

// shortSHA is the first 12 characters of a commit id for a log line; the full
// id is in the member's own log when it matters.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// panelRepoRoot is the git top-level of dir: the checkout the worktrees are
// created from.
func panelRepoRoot(ctx context.Context, dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("no checkout to read")
	}
	out, err := panelGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", fmt.Errorf("git rev-parse --show-toplevel returned nothing")
	}
	return out, nil
}

// panelReviewedTip is the commit every member must review: the tip the
// checkpoint verified on the origin remote. A branchless or UNVERIFIED
// checkpoint has no such tip and refuses the fan-out — the sequential path lets
// each reviewer establish the hand-off itself, which is what the unverified
// brief prescribes.
func panelReviewedTip(ctx context.Context, dir string, prev *harness.Result) (string, bool) {
	if prev == nil || prev.Claim.Branch == "" || prev.CheckpointTip == "" {
		// A BRANCHLESS or UNVERIFIED checkpoint has no tip the driver verified.
		// The sequential path lets each reviewer establish the hand-off itself,
		// which is exactly what the unverified brief prescribes; fanning out on a
		// guessed local ref would let members review a revision nobody verified.
		return "", false
	}
	if tip := prev.CheckpointTip; tip != "" {
		if panelHasCommit(ctx, dir, tip) {
			return tip, true
		}
		// The seam verified the tip on the ORIGIN remote, which does not mean
		// this checkout has the object. Fetch the reviewed branch and demand the
		// exact tip: a branch that moved since the checkpoint is not the revision
		// the review is for, and the sequential path is the safe answer.
		fctx, cancel := context.WithTimeout(ctx, panelFetchTimeout)
		_, ferr := panelGit(fctx, dir, "fetch", "--no-tags", "origin", prev.Claim.Branch)
		cancel()
		if ferr == nil {
			if got, gerr := panelGit(ctx, dir, "rev-parse", "FETCH_HEAD^{commit}"); gerr == nil && got == tip {
				return tip, true
			}
		}
		return "", false
	}
	return "", false
}

// panelHasCommit reports whether the checkout already holds commit-ish (no
// network): an object present in the shared object database needs no fetch.
func panelHasCommit(ctx context.Context, dir, commitish string) bool {
	_, err := panelGit(ctx, dir, "cat-file", "-e", commitish+"^{commit}")
	return err == nil
}

// createPanelWorktree makes the member's own DETACHED checkout at tip. The
// directory is unique and does not pre-exist: git itself refuses an existing
// path, so the driver cannot accidentally adopt a tree somebody else owns. It
// lives in the fleet's worktrees directory when that exists (the project-root
// layout), else beside the checkout's conventional `-wt` area, else the OS temp
// directory — each still inside a directory the session is allowed to use.
func (d *Driver) createPanelWorktree(ctx context.Context, repoRoot, tip, label string) (string, error) {
	root := panelWorktreeRoot(repoRoot)
	dir := filepath.Join(root, fmt.Sprintf("panel-%s-%s-%s", sanitizePanelLabel(label), time.Now().Format("20060102-150405"), randomTail()))
	if _, err := panelGit(ctx, repoRoot, "worktree", "add", "--detach", dir, tip); err != nil {
		_ = os.RemoveAll(dir)
		_, _ = panelGit(ctx, repoRoot, "worktree", "prune")
		return "", err
	}
	log.Printf("review panel: %s reviews in detached worktree %s at %s", label, dir, shortSHA(tip))
	return dir, nil
}

// removePanelWorktree discards a member's throwaway checkout. A dirty tree is
// reported (paths, first line each) and then discarded: a review-only session's
// deliverable is its DURABLE record, and salvage from this tree is forbidden by
// design — committing a member's generated artifacts would move the tip past the
// revision every member recorded.
func (d *Driver) removePanelWorktree(repoRoot, dir string) {
	if dir == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), panelCleanupTimeout)
	defer cancel()
	if out, err := panelGit(ctx, dir, "status", "--porcelain"); err == nil {
		if dirty := strings.TrimSpace(out); dirty != "" {
			lines := strings.Split(dirty, "\n")
			if len(lines) > 5 {
				lines = append(lines[:5], fmt.Sprintf("... and %d more", len(lines)-5))
			}
			log.Printf("review panel: discarding uncommitted changes in the member worktree %s (a review-only tree is never salvaged): %s",
				dir, strings.Join(lines, "; "))
		}
	}
	if _, err := panelGit(ctx, repoRoot, "worktree", "remove", "--force", dir); err != nil {
		log.Printf("review panel: could not remove the member worktree %s (%v) — removing the directory and pruning", dir, err)
		_ = os.RemoveAll(dir)
		_, _ = panelGit(ctx, repoRoot, "worktree", "prune")
	}
}

// panelWorktreeRoot picks where a member's worktree is made, preferring the
// fleet's own worktrees directory so it sits inside the project's permission
// scope.
func panelWorktreeRoot(repoRoot string) string {
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return os.TempDir()
	}
	// The checkout may already BE a worktree under <project>/worktrees/<name>;
	// its parent is then the fleet's area.
	if filepath.Base(filepath.Dir(abs)) == config.WorktreesDirName {
		return filepath.Dir(abs)
	}
	// The documented per-project layout: <project root>/worktrees beside the
	// repo clone.
	if wt := filepath.Join(filepath.Dir(abs), config.WorktreesDirName); isDir(wt) {
		return wt
	}
	// The older convention: <checkout>-wt beside the checkout.
	if wt := abs + "-wt"; isDir(wt) {
		return wt
	}
	return os.TempDir()
}

func isDir(dir string) bool {
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}

// sanitizePanelLabel keeps a member's log label safe to embed in a path.
func sanitizePanelLabel(label string) string {
	var b strings.Builder
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "member"
	}
	return b.String()
}

// panelGit runs one git command with the same non-interactive environment the
// driver's other git reads use: an unattended run has no terminal, and a git
// that decides to ask for a password would hang a member instead of failing it.
func panelGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=echo",
		"SSH_ASKPASS=echo",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=10",
		"GCM_INTERACTIVE=never",
	)
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, firstLineOf(msg))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}
