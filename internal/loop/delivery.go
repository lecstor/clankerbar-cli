package loop

import (
	"context"
	"errors"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// verifyDeliveries checks, against local git, what the session told the plane it
// delivered — and says so out loud (CLA-253).
//
// The plane is a pipe: it stores a recorded branch and a declared merge and takes
// the clanker at its word, because it has no access to a git host and, by the
// standing decision behind this, is not being given any. The driver is already
// local and already in the tree, so it is the one place the claim can simply be
// looked at.
//
// # Warn, do not refuse
//
// A failed check is logged loudly and names what is unpushed or unmerged. It does
// NOT override the session's report, and this is a deliberate first cut rather
// than an oversight: refusing would mean the driver reverting a status a session
// chose, on the strength of a check that can be wrong about which tree it is
// looking at. Loud logging is the floor, it is cheap, and it is reversible.
// Recorded as a decision on CLA-253.
//
// "Refusing the delivery" (CLA-310) lives inside that shape: a PR check that
// fails is a Fail with the failing condition and its remedy named in the log,
// and no verified attestation behind it — the driver's verdict on the delivery
// is refusal, not a rewrite of what the session told the plane.
//
// # Fail open
//
// A check that could not run reports "could not verify" and the run carries on.
// Blocking a legitimate closure because the tree could not be found would be worse
// than the gap this replaces — and an Unknown is never allowed to read as a pass:
// the attestation below is written only for a check that actually ran.
func (d *Driver) verifyDeliveries(ctx context.Context, t Target, res harness.Result, workdir string) {
	if len(res.Reports) == 0 || d.newVerifier == nil {
		return
	}
	// Unlike the handback, this is NOT worth outliving a Ctrl-C. The handback races
	// a lease that is already ticking; a delivery claim will still be wrong in the
	// morning, and the checks reach the network, so insisting on them would add a
	// visible stall to every interactive shutdown.
	if ctx.Err() != nil {
		return
	}

	// workdir is the session's actual spawn directory (CLA-437), for the same
	// reason salvageStrandedWork takes it: the commits being checked live in the
	// repo the session worked in, which is no longer always the target's.
	v := d.newVerifier(workdir, d.cfg.AllowUncheckedPRFor(t.Name))
	for _, rep := range res.Reports {
		// Resolve the claim's repo AUTHORITATIVELY from the task's own record —
		// "the task's repo field (primary repo when unset)" is the doneWhen's
		// words, and the plane's task row always carries it (CLA-437). The
		// session-declared value is secondary: the new tool set has no call that
		// carries `repo` alongside a branch (record_progress has no repo field),
		// so it is normally empty. Mirror sessionDir's degradation exactly: an
		// unwired plane falls back to the session's declaration, then to this
		// target's primary repo.
		repo := rep.Repo
		if src, ok := t.Releaser.(plane.TaskRepoSource); ok && rep.TaskID != "" {
			r, err := src.TaskRepo(ctx, rep.TaskID)
			switch {
			case err == nil && r != "":
				repo = r
			case err == nil && repo == "":
				repo = t.PrimaryRepo
			case errors.Is(err, plane.ErrNotWired):
				if repo == "" {
					repo = t.PrimaryRepo
				}
			default:
				log.Printf("%scould not read the repo of task %s (%v) — the claim's session-declared repo is used as-is", labelOf(t), rep.Ref, err)
			}
		} else if repo == "" {
			repo = t.PrimaryRepo
		}
		claim := delivery.Claim{Label: rep.Label(), Branch: rep.Branch, PR: rep.PR, Repo: repo}
		if rep.ClaimsMerge() {
			claim.Commit, claim.IntegrationBranch = rep.Commit, rep.IntegrationBranch
		}
		// Detached and bounded PER REPORT: a shared deadline would silently degrade
		// every claim after the first slow one to "cannot check". Detached because a
		// cancel arriving mid-check should not turn a real answer into a half one —
		// the guard above is what makes cancellation prompt. The PR check's own
		// polling budget sits well inside this deadline, so one slow GitHub read
		// cannot starve the git checks that share the report.
		vctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryCheckTimeout)
		out := v.Verify(vctx, claim)
		cancel()

		for _, c := range out.Checks {
			where := labelOf(t)
			if out.Repo != "" && out.Repo != workdir {
				where += "[" + out.Repo + "] "
			}
			switch c.Status {
			case delivery.Fail:
				log.Printf("%sDELIVERY UNVERIFIED — %s: %s", where, rep.Label(), c.Detail)
			case delivery.Warn:
				log.Printf("%sDELIVERY WARN — %s: %s", where, rep.Label(), c.Detail)
			case delivery.Unknown:
				log.Printf("%scould not verify %s: %s — carrying on", where, rep.Label(), c.Detail)
			default:
				log.Printf("%sverified %s: %s", where, rep.Label(), c.Detail)
			}
		}
		d.attestMerge(ctx, t, rep, out)
	}
}

// deliveryCheckTimeout bounds one session's worth of git checks, including the
// `ls-remote` that goes over the network.
const deliveryCheckTimeout = 60 * time.Second

// attestMerge writes the driver's verdict onto the task's delivery record, so the
// answer survives the log. Only for a check that RAN: `mergeVerified` means "I ran
// the ancestor check and this is what it said", and writing it off the back of a
// check that could not run would be the same false attestation this exists to
// catch. Best-effort throughout — an unreachable or unwired plane costs the record,
// not the run, and the loud log above already carries the finding.
func (d *Driver) attestMerge(ctx context.Context, t Target, rep harness.Report, out delivery.Report) {
	verified, ran := out.MergeVerified()
	if !ran || rep.TaskID == "" || rep.RunID == "" {
		return
	}
	a, ok := t.Releaser.(plane.Attester)
	if !ok {
		return
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()

	if err := a.AttestMergeVerified(actx, rep.TaskID, rep.RunID, plane.Delivery{
		Commit:            rep.Commit,
		IntegrationBranch: rep.IntegrationBranch,
		PR:                rep.PR,
	}, verified); err != nil {
		if !errors.Is(err, plane.ErrNotWired) {
			log.Printf("%scould not record the merge check for %s: %v", labelOf(t), rep.Label(), err)
		}
		return
	}
	log.Printf("%srecorded delivery.mergeVerified=%t for %s", labelOf(t), verified, rep.Label())
}

// diffAgainstIntegration compares a pushed branch to the integration branch
// (staging) for changed paths, using git in the work directory.
// It returns the list of changed file paths, or an error if the diff fails.
func (d *Driver) diffAgainstIntegration(t Target, branchName string, workDir string) ([]string, error) {
	if branchName == "" {
		return nil, errors.New("no branch recorded for diff")
	}
	wd := workDir
	if wd == "" {
		wd = d.cfg.WorkDir
		if wd == "" {
			wd = "."
		}
	}
	// Use git diff against the integration branch (staging by default).
	// The remote-tracking ref is the only safe base: a local branch can be far
	// behind origin, and a diff from the stale one would mis-shape the escalation
	// rules that read this list.
	cmd := exec.Command("git", "-C", wd, "diff", "--name-only", "origin/staging..."+branchName)
	out, err := cmd.Output()
	if err != nil {
		// No remote-tracking base to compare against: return no paths rather than
		// falling back to the bare local branch, which can be far behind origin.
		return nil, nil
	}
	paths := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(paths) == 1 && paths[0] == "" {
		return nil, nil
	}
	result := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result, nil
}
