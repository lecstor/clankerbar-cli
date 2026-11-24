package loop

// panel_parallel.go — running a review panel's members concurrently.

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
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/secureurl"
)

const (
	// panelFetchTimeout bounds the one network call a fan-out may need: fetching a reviewed tip this
	// checkout does not yet hold.
	panelFetchTimeout = 30 * time.Second
	// panelCleanupTimeout bounds removing a member's throwaway worktree.
	panelCleanupTimeout = 30 * time.Second
)

// panelMemberOutcome is one member's finished pass, before the join folds it into the drain's panel
// bookkeeping.
type panelMemberOutcome struct {
	label string
	role  string
	// optional marks a TRIAL seat: its end is that seat's own result and never the panel's. The join
	// must not fold its sequence-end into the drain's run-level wait, nor let its error end the drain,
	// and its record alone never earns a round close.
	optional bool
	// setupErr is a member whose own worktree could not be made.
	setupErr string
	// end/perr/stop are exactly drainPhase's returns.
	end    phaseEnd
	perr   error
	stop   bool
	tokens int
	cost   float64
}

// panelRunOutcome is what the join hands back to drainPhases: the accumulated spend and sessions,
// the member results in spawn order, and the panel-level facts (round, incomplete list, whether
// anyone recorded, whether the join owes a wait) it derives from them.
type panelRunOutcome struct {
	members []panelMemberOutcome
	tokens  int
	cost    float64
	stop    bool
	err     error

	recorded    bool
	round       int
	incomplete  []string
	failed      map[string]panelMemberFailure
	closeFailed bool

	// limitWait/sequenceEnded carry a member's run-level sequence end (a rolling limit or a transient
	// failure) for the drain to take after the join, exactly as the sequential member handler does.
	limitWait     *phaseLimitWait
	sequenceEnded bool
	transientKey  resumedTransientKey

	// claim is the last member claim observed, for the drain's iteration record (the deferred handback
	// keeps `carried`, the run the panel resumed).
	claim       *harness.Result
	disposition string

	// blockingAsk is the last member's successful blocking ask, if any: the shared task is now
	// `blocked`, so the drain reports waiting on it (CLA-535).
	blockingAsk string

	// finalDead/lastProducedNothing mirror the LAST member's classification for the iteration record,
	// exactly as the sequential loop's locals do.
	finalDead           bool
	lastProducedNothing bool

	// runLost reports that a member's session was ended because the plane says the run no longer holds
	// the task.
	runLost bool
}

// runPanelMembers runs n work functions with at most `concurrency` in flight. A finished member
// frees its slot and the next member in list order starts immediately — a backfill queue, not a
// wave barrier, so a short reviewer never waits for a long one in its batch.
func runPanelMembers(n, concurrency int, run func(k int, started func()), onRelease func(k int)) {
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for k := 0; k < n; k++ {
		sem <- struct{}{}
		started := make(chan struct{})
		var once sync.Once
		release := func() {
			once.Do(func() {
				releaseMember(k, onRelease, func() { close(started) })
			})
		}
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			defer func() { <-sem }()
			// The defer is a safety net: a path that forgets to call started still unblocks the loop when
			// the member ends, instead of deadlocking the panel.
			defer release()
			run(k, release)
		}(k)
		<-started
	}
	wg.Wait()
}

// releaseMember is the WHOLE of a member's release: it reports the release to the observer
// (onRelease, test-only), and only then unblocks the scheduler that spawns the successor. Keeping
// both steps in one function makes that order structural rather than two adjacent statements a
// later edit could silently swap: a swap would leave the panel fixture green, because the successor
// needs a `git worktree add` subprocess before it can reach its own launch and would win that race
// every time, turning the count observed at a launch into a race.
func releaseMember(k int, onRelease func(k int), unblock func()) {
	if onRelease != nil {
		onRelease(k)
	}
	unblock()
}

// runPanelInParallel runs phases[start:end] concurrently, each in its own detached worktree at the
// reviewed tip. ok=false means the fan-out could not be set up at all (no live claim, no repo, no
// resolvable tip) and the caller must run the shipped sequential path instead — a worktree
// failure that would only make one member fail does not refuse the panel.
func (d *Driver) runPanelInParallel(ctx context.Context, drainNum, ti int, t Target, phases []config.Phase, start, end, concurrency int, carried *harness.Result, prior spend, baseSHA string) (*panelRunOutcome, bool) {
	if carried == nil || !carried.Claim.Held() {
		log.Printf("%siteration %d: the review panel has no live claim to resume — running its members sequentially",
			labelOf(t), drainNum)
		return nil, false
	}
	baseDir, _, err := d.sessionDir(ctx, t, carried)
	if err != nil {
		log.Printf("%siteration %d: the review panel could not resolve its checkout (%v) — running its members sequentially",
			labelOf(t), drainNum, err)
		return nil, false
	}
	// sessionDir returns "" for the legacy single-repo shape (nothing declared to resolve); the
	// session then runs in the configured workdir, and so must the git probes.
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

	out := &panelRunOutcome{failed: map[string]panelMemberFailure{}, sequenceEnded: false}
	n := end - start
	out.members = make([]panelMemberOutcome, n)
	// One presence beacon for the panel, not one per member: beginIteration also writes the local
	// BEACON file (remove-then-create), and N goroutines doing that to one name would race for a line
	// nobody reads mid-panel.
	d.beginIteration(ti, t, drainNum, claimLabel(carried.Claim), phases[start].Label(start))
	log.Printf("%siteration %d: review panel: running %d members concurrently (cap %d), each in its own detached worktree at %s",
		labelOf(t), drainNum, n, concurrency, shortSHA(tip))

	// One shared account serves the whole panel through the Driver's per-target ledger: charge() books
	// each member's counted spend as it lands, so every member's pre-spawn check already sees a
	// sibling's money.

	runPanelMembers(n, concurrency, func(k int, started func()) {
		idx := start + k
		ph := phases[idx]
		label := ph.Label(idx)
		role := reviewerRoleFor(phases, idx)
		last := idx == len(phases)-1

		m := panelMemberOutcome{label: label, role: role, optional: ph.IsOptionalPanelMember()}
		dir, werr := d.createPanelWorktree(ctx, repoRoot, tip, label)
		if werr != nil {
			m.setupErr = "could not create its own detached worktree: " + werr.Error()
			out.members[k] = m
			// This member never launches a session, so nothing will fire the launch boundary: release the
			// next member here or the panel waits forever for a session that will not run.
			started()
			return
		}
		defer d.removePanelWorktree(repoRoot, dir)

		tag := "-p" + label
		m.tokens, m.cost, m.stop, m.end, m.perr = d.drainPhase(
			ctx, drainNum, ti, idx, tag, ph, last, carried, t, prior, phases,
			panelState{}, phaseRunOpts{
				worktree:      dir,
				panelParallel: true,
				// A member carries no runner briefing (it writes no report), so the prep's merge-base rides the
				// opts instead: its brief must diff from the same commit the single reviewer's would.
				baseSHA: baseSHA,
				// The next member starts only once THIS member's session launch has actually begun.
				onSessionLaunch: func() {
					if d.panelLaunch != nil {
						d.panelLaunch(role)
					}
					started()
				},
			})
		out.members[k] = m
	}, d.panelRelease)

	for k, m := range out.members {
		out.tokens += m.tokens
		out.cost += m.cost
		// CLA-784: a CANCEL is the run's, not the member's, so it stops the run whoever the seat is.
		if m.stop {
			out.stop = true
		}
		// An optional seat's error is that seat's failure, not the drain's: the panel carries on and the
		// required members' records still reach the fix.
		if m.perr != nil && out.err == nil && !m.optional {
			out.err = m.perr
		}
		if m.end.claim != nil {
			out.claim = m.end.claim
			out.disposition = claimDisposition(m.end.claim)
		}
		if m.end.blockingAsk != "" {
			out.blockingAsk = m.end.blockingAsk
		}
		if m.end.runLost {
			out.runLost = true
		}
		out.finalDead = m.end.dead
		out.lastProducedNothing = m.end.producedNothing
		out.round = max(out.round, m.end.reviewRound)
		// CLA-784: an optional seat's sequence-end (a rolling limit or a transient failure) is recorded
		// against that seat and never ends the sequence or hands back the panel's shared claim.
		if m.end.sequenceEnd != nil && !m.optional && !out.sequenceEnded {
			out.sequenceEnded = true
			out.limitWait = m.end.sequenceEnd.limitWait
			if out.limitWait == nil {
				// The drain notes the streak when it takes the wait; this is the target+harness whose ladder
				// the failure belongs to.
				out.transientKey = resumedTransientKey{target: ti, harness: d.targetCfg(t).HarnessFor(phases[start+k])}
			}
		}
	}

	// Judge every member exactly as the sequential handler does: recorded, or named INCOMPLETE with
	// the reason.
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
			// CLA-784: a required member's record is a review; an optional (trial) seat's record is that
			// seat's own result.
			if !m.optional {
				out.recorded = true
			}
			d.panelMemberRecorded(m.label)
			continue
		}
		reason := panelMemberFailureReason(m)
		out.incomplete = append(out.incomplete, panelIncompleteRole(m.role, m.optional)+": "+reason)
		out.failed[m.role] = panelMemberFailure{reason: reason, optional: m.optional}
		verdict := "recording it as failed"
		if m.end.windowHeld {
			// CLA-947 review 1.review-1.6: the fan-out's own hold runs over every member's list, so this is
			// the race where a window opened between that check and the spawn.
			verdict = "a pricing window opened before it could spawn; not counted against the member"
		}
		log.Printf("%siteration %d: the %s panel member recorded nothing (%s) — %s",
			labelOf(t), drainNum, m.label, reason, verdict)
		if !m.end.windowHeld && !m.end.deadBooked && !m.end.runLost {
			d.tallyEmpty(m.label, d.targetCfg(t).HarnessFor(phases[start+k]))
		}
		// The streak is charged only for a member that actually failed: a window-held seat never ran, so
		// counting it would let a schedule put a healthy reviewer's harness on the "not producing
		// reviews" warning.
		if !m.end.windowHeld {
			if n := d.panelMemberFailed(m.label); n >= panelMemberFailBound {
				ph := phases[start+k]
				model, _ := d.targetCfg(t).ModelForPhase(ph)
				log.Printf("%siteration %d: WARNING — the %s review-panel member has recorded nothing on %d consecutive tasks (harness %s, model %s); the panel runs on and the fix applies what the other members found, but this member's harness, model or brief is not producing reviews — check its iteration logs",
					labelOf(t), drainNum, m.label, n, d.targetCfg(t).HarnessFor(ph), attributionOr(model, "harness default"))
			}
		}
	}

	// Close the round once, by the live holder, before anything hands the claim back — a sealed
	// round would be voided by a takeover and every member's record lost.
	if out.recorded && out.round > 0 && live {
		if cerr := d.closeReviewRound(ctx, t, *carried, out.round); cerr != nil {
			log.Printf("%siteration %d: the panel's round %d could not be closed (%v) — ending the drain without spawning the fix: a fix spawned now would run against findings the plane still has sealed",
				labelOf(t), drainNum, out.round, cerr)
			out.closeFailed = true
		}
	}
	if !out.recorded {
		log.Printf("%siteration %d: no REQUIRED member of the review panel recorded — the round is not closed and no fix spawns", labelOf(t), drainNum)
	}
	return out, true
}

// panelIncompleteRole is how a failed panel member is named in the INCOMPLETE log line: its
// positional reviewer role, marked "(optional)" for a TRIAL seat, so an operator can tell a
// required reviewer's failure from a seat we were only evaluating.
func panelIncompleteRole(role string, optional bool) string {
	if optional {
		return role + " (optional)"
	}
	return role
}

// panelMemberFailureReason names why a member produced no durable record, in the same words the
// sequential handler uses.
func panelMemberFailureReason(m panelMemberOutcome) string {
	switch {
	case m.setupErr != "":
		return m.setupErr
	case m.end.toolLoop != "":
		return m.end.toolLoop
	case m.end.dead:
		return "died producing nothing"
	case m.end.sequenceEnd != nil && m.end.sequenceEnd.limitWait != nil:
		return "hit a usage limit before recording"
	case m.end.sequenceEnd != nil:
		return "ended on a transient failure before recording"
	case m.perr != nil:
		return failureDetail(m.perr.Error())
	case m.end.windowHeld:
		return m.end.windowWhy
	case m.end.empty:
		return m.end.emptyWhy
	default:
		return "recorded no durable review record"
	}
}

// shortSHA is the first 12 characters of a commit id for a log line; the full id is in the member's
// own log when it matters.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// panelRepoRoot is the git top-level of dir: the checkout the worktrees are created from.
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

// panelReviewedTip is the commit every member must review: the tip the checkpoint verified on the
// origin remote. A branchless or UNVERIFIED checkpoint has no such tip and refuses the fan-out —
// the sequential path lets each reviewer establish the hand-off itself, which is what the
// unverified brief prescribes.
func panelReviewedTip(ctx context.Context, dir string, prev *harness.Result) (string, bool) {
	if prev == nil || prev.Claim.Branch == "" || prev.CheckpointTip == "" {
		// A BRANCHLESS or UNVERIFIED checkpoint has no tip the driver verified.
		return "", false
	}
	tip := prev.CheckpointTip
	if panelHasCommit(ctx, dir, tip) {
		return tip, true
	}
	// The seam verified the tip on the ORIGIN remote, which does not mean this checkout has the
	// object.
	fctx, cancel := context.WithTimeout(ctx, panelFetchTimeout)
	ferr := panelFetch(fctx, dir, "origin", prev.Claim.Branch)
	cancel()
	if ferr == nil {
		if got, gerr := panelGit(ctx, dir, "rev-parse", "FETCH_HEAD^{commit}"); gerr == nil && got == tip {
			return tip, true
		}
	}
	return "", false
}

// panelFetch fetches one branch scoped to the account owning the remote it reads
// (delivery.ScopeRemoteEnv via taskWorktreeRemoteCredEnv, CLA-970). The reviewed
// branch lives on a private origin the ACTIVE gh account need not see, and a
// refused fetch here is silent - panelReviewedTip falls back to running the
// panel sequentially - so it must go out exactly as the prep's fetch does.
func panelFetch(ctx context.Context, dir, remote, branch string) error {
	args := []string{"fetch", "--no-tags", remote, branch}
	_, err := panelGitWithEnv(ctx, dir, taskWorktreeRemoteCredEnv(ctx, dir, args), args...)
	return err
}

// panelHasCommit reports whether the checkout already holds commit-ish (no network): an object
// present in the shared object database needs no fetch.
func panelHasCommit(ctx context.Context, dir, commitish string) bool {
	_, err := panelGit(ctx, dir, "cat-file", "-e", commitish+"^{commit}")
	return err == nil
}

// createPanelWorktree makes the member's own DETACHED checkout at tip. The directory is unique and
// does not pre-exist: git itself refuses an existing path, so the driver cannot accidentally adopt
// a tree somebody else owns.
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

// removePanelWorktree discards a member's throwaway checkout. A dirty tree is reported (paths,
// first line each) and then discarded: a review-only session's deliverable is its DURABLE record,
// and salvage from this tree is forbidden by design — committing a member's generated artifacts
// would move the tip past the revision every member recorded.
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

// panelWorktreeRoot picks where a member's worktree is made, preferring the fleet's own worktrees
// directory so it sits inside the project's permission scope.
func panelWorktreeRoot(repoRoot string) string {
	if root, ok := fleetWorktreeRoot(repoRoot); ok {
		return root
	}
	return os.TempDir()
}

// fleetWorktreeRoot resolves the fleet's worktree area for a checkout. ok=false means no known
// layout exists; each caller supplies its own fallback, because a panel tree is throwaway and a
// task tree is not.
func fleetWorktreeRoot(repoRoot string) (string, bool) {
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", false
	}
	// The checkout may already BE a worktree under <project>/worktrees/<name>; its parent is then the
	// fleet's area.
	if filepath.Base(filepath.Dir(abs)) == config.WorktreesDirName {
		return filepath.Dir(abs), true
	}
	// The documented per-project layout: <project root>/worktrees beside the repo clone.
	if wt := filepath.Join(filepath.Dir(abs), config.WorktreesDirName); isDir(wt) {
		return wt, true
	}
	// The older convention: <checkout>-wt beside the checkout.
	if wt := abs + "-wt"; isDir(wt) {
		return wt, true
	}
	return "", false
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

// panelGit runs one git command with the same non-interactive environment the driver's other git
// reads use: an unattended run has no terminal, and a git that decides to ask for a password would
// hang a member instead of failing it.
func panelGit(ctx context.Context, dir string, args ...string) (string, error) {
	return panelGitWithEnv(ctx, dir, nil, args...)
}

// panelGitWithEnv is panelGit with environment additions for THIS invocation -
// the credential scoping panelFetch needs. delivery.GitEnv strips inherited
// GIT_CONFIG_* before appending them, so an inherited GIT_CONFIG_COUNT cannot
// shadow the scoped rewrite (duplicate env keys have unspecified precedence).
func panelGitWithEnv(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = delivery.GitEnv(os.Environ(), extraEnv)
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			// A scoped fetch's token rides the URL git echoes on failure.
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, secureurl.RedactUserinfo(firstLineOf(msg)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}
