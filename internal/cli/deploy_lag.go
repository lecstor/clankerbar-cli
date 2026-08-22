package cli

// deploy_lag.go — CLA-322: doctor's deploy_lag check.
//
// The plane stamps version.commit into /health at BUILD time (CLA-313), which
// makes "is commit X live?" one request and a string compare. What nothing did
// was LOOK: staging once sat 21 commits ahead of production overnight while a
// verification session was on the verge of reporting working code as broken,
// because the only place the lag was visible was a standing promotion PR
// nobody had a reason to open. This check makes doctor look, every run,
// before an overnight starts.
//
// The comparison is between two facts that live on DIFFERENT machines:
//
//   - what is deployed: /health's version.commit, read from health_url;
//   - what should be deployed next: the REMOTE tip of the integration branch
//     (integration_branch, default staging), read with ls-remote — never a
//     local ref, which is routinely stale by exactly the commits in question.
//
// The local clone under the project's workdir settles ancestry and counts the
// gap only. "What is deployed" is never derived from local branch state: a
// working clanker routinely holds local branches no deployment has ever seen,
// and reading them as "the deployed version" is the false alarm this check
// must not raise (see the ahead case below).
//
// Fail open, like every other check here.
//
// Anything doctor cannot establish — /health unreachable, the commit absent
// from every local clone even after a bounded fetch, a remote that will not
// answer — is a WARN saying what could not be learned, never a PASS that
// implies the deployment is current. A green line is only allowed when the
// comparison actually happened.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

const (
	// deployHealthTimeout bounds the /health read. Doctor is documented as a
	// cron gate (`doctor && run`); an unbounded fetch turns a slow endpoint
	// into a hung preflight.
	deployHealthTimeout = 10 * time.Second

	// DeployLagWarnAfter is the firing threshold, recorded as a decision on
	// CLA-322 rather than left to fall out of the code: a gap is warned about
	// only when it is behind AND its oldest undeployed commit is older than
	// this. One commit for ten minutes is normal and healthy — deploys happen
	// continuously while work lands — and a check that fires on that gets
	// skimmed past by the time 21 commits have sat undeployed overnight, which
	// is precisely the case it exists to catch. Age is the discriminator; the
	// commit count is always reported either way.
	DeployLagWarnAfter = 60 * time.Minute

	// maxDeployLagFetches bounds how many clones under the workdir doctor may
	// fetch into while hunting for the deployed commit. A multi-repo parent can
	// hold dozens of repositories and none of them owes doctor a network round
	// trip; six covers any realistic single-project layout, and beyond that the
	// honest answer is the cannot-compute WARN anyway.
	maxDeployLagFetches = 6
)

// deployVersion mirrors the plane's `version` block from CLA-313:
//
//	{"commit": "732d0be…", "builtAt": "2026-08-10T10:40:00.217Z"}
type deployVersion struct {
	Commit  string `json:"commit"`
	BuiltAt string `json:"builtAt"`
}

// deployHealth mirrors the /health payload. Only version.commit is read; ok
// and db are the liveness flags the endpoint already reports elsewhere.
type deployHealth struct {
	Version deployVersion `json:"version"`
}

var deployHTTPClient = &http.Client{Timeout: deployHealthTimeout}

// fetchDeployHealth GETs a /health endpoint. No credentials are sent — the
// endpoint is public — and the read is bounded twice over (client timeout plus
// request context) because a cron gate has nobody to Ctrl-C it.
func fetchDeployHealth(ctx context.Context, rawURL string) (deployHealth, error) {
	ctx, cancel := context.WithTimeout(ctx, deployHealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return deployHealth{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := deployHTTPClient.Do(req)
	if err != nil {
		return deployHealth{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return deployHealth{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return deployHealth{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, firstLine(string(body)))
	}
	var h deployHealth
	if err := json.Unmarshal(body, &h); err != nil {
		return deployHealth{}, fmt.Errorf("decode /health response: %w", err)
	}
	return h, nil
}

// gitRunner is one bounded git exec, so the deploy-lag helpers can be driven
// against a fake in tests the way every other check drives its seam.
type gitRunner func(ctx context.Context, dir string, args ...string) (string, error)

// deployGitRun executes one git command in dir and returns trimmed stdout.
//
// The environment discipline is delivery.Verifier.run's, copied rather than
// imported for the same reason Repos is shared rather than reimplemented there:
// an unattended run has no terminal, and a git that asks for a password would
// hang the preflight instead of answering. WaitDelay is load-bearing for the
// same reason it is there — ls-remote spawns helpers that inherit the pipes,
// and a killed git can otherwise leave the call sitting past its deadline.
func deployGitRun(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=echo",
		"SSH_ASKPASS=echo",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=10",
		"GCM_INTERACTIVE=never",
	)
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", args[0], err, firstLine(msg))
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// deployRemoteOf picks which remote to judge against: origin when the repo
// has it, else its only remote, else origin. The same rule
// delivery.resolveRemote applies, kept in step because a rescue and this check
// must not disagree about which remote names the integration branch.
func deployRemoteOf(ctx context.Context, run gitRunner, repo string) string {
	out, err := run(ctx, repo, "remote")
	if err != nil || out == "" {
		return "origin"
	}
	names := strings.Fields(out)
	for _, n := range names {
		if n == "origin" {
			return "origin"
		}
	}
	if len(names) == 1 {
		return names[0]
	}
	return "origin"
}

// deployLsRemoteHead returns the SHA a remote branch points at, or "" when the
// remote genuinely has no such branch (a nil error with an empty SHA).
func deployLsRemoteHead(ctx context.Context, run gitRunner, repo, remote, branch string) (string, error) {
	out, err := run(ctx, repo, "ls-remote", "--heads", "--", remote, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "refs/heads/"+branch {
			return fields[0], nil
		}
	}
	return "", nil
}

func deployHaveObject(ctx context.Context, run gitRunner, repo, sha string) bool {
	_, err := run(ctx, repo, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func deployIsAncestor(ctx context.Context, run gitRunner, repo, a, b string) bool {
	_, err := run(ctx, repo, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// deployCountRange counts the commits in a rev-list range ("a..b").
func deployCountRange(ctx context.Context, run gitRunner, repo, rng string) (int, error) {
	out, err := run(ctx, repo, "rev-list", "--count", rng)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("parse rev-list --count output %q: %w", out, err)
	}
	return n, nil
}

// deployOldestAgeSeconds reports how long ago the OLDEST commit in rng was
// authored, as a wall-clock duration from now. It takes the MINIMUM timestamp
// across `%ct` lines explicitly rather than trusting log's ordering, so a
// future change of traversal order cannot silently turn "oldest" into
// "newest" and flip the threshold's verdict. An error means the age could not
// be read at all; callers must not treat that as fresh.
func deployOldestAgeSeconds(ctx context.Context, run gitRunner, repo, rng string, now time.Time) (time.Duration, error) {
	out, err := run(ctx, repo, "log", "--format=%ct", rng)
	if err != nil {
		return 0, err
	}
	var oldest int64
	found := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ts, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			continue
		}
		if !found || ts < oldest {
			oldest = ts
			found = true
		}
	}
	if !found {
		return 0, fmt.Errorf("no readable commit timestamps in the gap")
	}
	age := now.Sub(time.Unix(oldest, 0))
	if age < 0 {
		age = 0 // a clock skew into the future is a zero-age gap, not a negative one
	}
	return age, nil
}

// deployIsCommitID reports whether s spells an abbreviated or full object id.
// delivery.isCommitID copied rather than exported-for-one-caller: seven hex
// characters is git's own minimum useful abbreviation, and anything shorter or
// non-hex cannot be compared to version.commit meaningfully.
func deployIsCommitID(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// --- the checks --------------------------------------------------------------

// checkDeployLags runs one deploy_lag check per project. In single-project
// mode there is one, driven by the top-level fields; in multi-project mode
// each project's own health_url wins when set, and projects that resolve to no
// URL are skipped — they opted out per project rather than being forgotten.
func checkDeployLags(ctx context.Context, cfg *config.Config, e doctorEnv) []check {
	if len(cfg.Projects) == 0 {
		return []check{deployLagCheck(ctx, "deploy_lag",
			cfg.HealthURLFor(""), cfg.IntegrationBranchFor(""), cfg.WorkDir, e)}
	}
	configured := false
	out := make([]check, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		u := cfg.HealthURLFor(p.Slug)
		if u == "" {
			continue
		}
		configured = true
		out = append(out, deployLagCheck(ctx, "deploy_lag["+p.Slug+"]",
			u, cfg.IntegrationBranchFor(p.Slug), projectWorkDir(cfg, p), e))
	}
	if !configured {
		return []check{deployLagCheck(ctx, "deploy_lag", "", "", cfg.WorkDir, e)}
	}
	return out
}

// deployLagCheck compares what is deployed (health_url's version.commit)
// against what should be deployed next (the REMOTE tip of integration_branch)
// and warns when the deployment lags by a gap old enough to matter.
func deployLagCheck(ctx context.Context, name, healthURL, integration, workdir string, e doctorEnv) check {
	c := check{name: name}

	integration = strings.TrimSpace(integration)
	if integration == "" {
		integration = config.DefaultIntegrationBranch
	}
	// Name the comparison up front, on every verdict, so any single line of
	// output can be traced back to its inputs the way checkConfig names its
	// resolved values.
	c.info = append(c.info, "judged against the REMOTE tip of <remote>/"+integration)

	if healthURL == "" {
		c.status = pass
		c.detail = "not configured - set health_url (top-level or per project) to watch how far the deployed build lags " + integration
		return c
	}

	h, err := e.fetchHealth(ctx, healthURL)
	if err != nil {
		c.status = warn
		c.detail = "could not read " + healthURL + ": " + err.Error()
		c.remedy = "check health_url (" + healthURL + ") - while it does not answer, deploy lag cannot be monitored"
		return c
	}

	// Classify version.commit BEFORE treating it as a lag measurement. CLA-313
	// deliberately ships two non-SHA values, and neither is a lag: reporting
	// them as "N commits behind" would invent a number out of nothing.
	deployed := strings.TrimSpace(h.Version.Commit)
	switch {
	case deployed == "":
		c.status = warn
		c.detail = healthURL + " reports no version.commit stamp"
		c.remedy = "rebuild and redeploy so the build stamps version.commit; until then deploy lag cannot be measured"
		return c
	case strings.EqualFold(deployed, "unknown"):
		c.status = warn
		c.detail = "deployed version is \"unknown\" - the build lost its stamp"
		c.remedy = "rebuild and redeploy so version.commit names a commit"
		return c
	case strings.HasSuffix(strings.ToLower(deployed), "-dirty") && deployIsCommitID(deployed[:len(deployed)-len("-dirty")]):
		c.status = warn
		c.detail = "deployed version is " + deployed + " - a dirty-tree (break-glass) deploy whose exact tree was never committed"
		c.remedy = "redeploy from a clean checkout when convenient; until then nothing running corresponds to any single commit"
		return c
	case !deployIsCommitID(deployed):
		c.status = warn
		c.detail = "version.commit is " + strconv.Quote(deployed) + " - neither a commit id nor a known sentinel, so it cannot be compared against " + integration
		c.remedy = "check what this deployment stamps into version.commit"
		return c
	}

	repo, whyNot := locateDeployRepo(ctx, e, workdir, deployed, integration)
	if repo == "" {
		c.status = warn
		c.detail = whyNot
		c.remedy = "fetch the clone(s) under " + workdir + ", or point workdir at the tree holding the deployment's repository, so the deployed commit can be traced locally"
		return c
	}

	run := e.gitRun
	remote := deployRemoteOf(ctx, run, repo)

	tip, err := deployLsRemoteHead(ctx, run, repo, remote, integration)
	if err != nil {
		c.status = warn
		c.detail = fmt.Sprintf("could not read refs from %s in %s: %v", remote, repo, err)
		c.remedy = "check remote access - the gap to " + integration + " cannot be computed without its tip"
		return c
	}
	if tip == "" {
		c.status = warn
		c.detail = remote + " has no branch named " + integration
		c.remedy = "set integration_branch (top-level or per project) to the branch deployments are built from"
		return c
	}
	if !deployHaveObject(ctx, run, repo, tip) {
		_, _ = run(ctx, repo, "fetch", "--quiet", "--", remote, integration)
	}
	if !deployHaveObject(ctx, run, repo, tip) {
		c.status = warn
		c.detail = fmt.Sprintf("%s/%s is at %s, which could not be fetched into %s - ancestry cannot be settled here",
			remote, integration, shortSHA(tip), repo)
		c.remedy = "check remote access, then re-run doctor"
		return c
	}

	behind, err := deployCountRange(ctx, run, repo, deployed+".."+tip)
	if err != nil {
		c.status = warn
		c.detail = fmt.Sprintf("could not count commits between deployed %s and %s/%s: %v",
			shortSHA(deployed), remote, integration, err)
		c.remedy = "inspect the repository manually; the lag cannot be computed from here"
		return c
	}

	if !deployIsAncestor(ctx, run, repo, deployed, tip) {
		ahead, aerr := deployCountRange(ctx, run, repo, tip+".."+deployed)
		switch {
		case aerr == nil && ahead > 0 && behind == 0:
			// The bite case this check was written around, in both directions:
			// a working clanker normally holds branches and commits no shared
			// line has taken yet, and a deployment can legitimately run ahead
			// of the integration branch (a hotfix built and shipped directly).
			// Neither is a stale deploy, so neither may read as one.
			c.status = pass
			c.detail = fmt.Sprintf("deployed build (%s) is ahead of %s/%s by %d commits - newer than the shared line, not a stale deploy",
				shortSHA(deployed), remote, integration, ahead)
			c.info = append(c.info, "work reached the deployment without passing "+remote+"/"+integration+"; merge it back if that was not deliberate")
			return c
		case aerr == nil && behind > 0 && ahead > 0:
			c.status = warn
			c.detail = fmt.Sprintf("deployed build (%s) and %s/%s (%s) have diverged (%d vs %d commits) - the gap cannot be expressed as a simple lag",
				shortSHA(deployed), remote, integration, shortSHA(tip), behind, ahead)
			c.remedy = "inspect both histories - the deployment and the integration branch share no linear relationship right now"
			return c
		default:
			c.status = warn
			c.detail = fmt.Sprintf("cannot relate deployed %s to %s/%s (%s): %v",
				shortSHA(deployed), remote, integration, shortSHA(tip), aerr)
			c.remedy = "inspect the repository manually; the relationship between the two histories cannot be computed from here"
			return c
		}
	}

	// Behind by some amount. Age decides whether it fires (DeployLagWarnAfter);
	// the count is always reported, because size is the thing the operator
	// needs once age says look.
	switch {
	case behind == 0:
		c.status = pass
		c.detail = fmt.Sprintf("deployed build matches %s/%s (%s)", remote, integration, shortSHA(tip))
		return c
	default:
		age, ageErr := deployOldestAgeSeconds(ctx, run, repo, deployed+".."+tip, time.Now())
		var ageText string
		fires := true // cannot read the age -> do not answer with a green line
		if ageErr != nil {
			ageText = "(the gap's age could not be read: " + ageErr.Error() + ")"
		} else {
			ageText = fmt.Sprintf("oldest undeployed commit is %s old", humanAge(age))
			fires = age > DeployLagWarnAfter
		}
		c.detail = fmt.Sprintf("deployment is %s behind %s/%s; %s",
			plural(behind, "1 commit", fmt.Sprintf("%d commits", behind)), remote, integration, ageText)
		if !fires {
			c.status = pass
			c.info = append(c.info, fmt.Sprintf("within normal deploy cadence - doctor warns once a gap passes %s", humanAge(DeployLagWarnAfter)))
			return c
		}
		c.status = warn
		c.remedy = fmt.Sprintf("redeploy so the build carries %s/%s (%s) - production ships only when the operator merges the promotion",
			remote, integration, shortSHA(tip))
		return c
	}
}

// locateDeployRepo finds the repository whose object database holds the
// deployed commit, so ancestry can be judged locally. It searches the same
// candidate set delivery.Repos produces for the session workdir — linked
// worktrees share a ref database and are deduplicated there, so the search
// reaches `<workdir>/<repo>` and `<workdir>/<repo>-wt/<task>` alike.
//
// When no clone has the object — usually one that simply has not fetched since
// the build went out, which is exactly when the check matters most — it
// fetches the INTEGRATION BRANCH into up to maxDeployLagFetches candidates and
// re-probes. Fetching the bare SHA would need allow-any-SHA uploadpack on the
// server, which GitHub does not enable by default; fetching the branch brings
// its ancestors along, including (usually) the deployed commit.
func locateDeployRepo(ctx context.Context, e doctorEnv, workdir, sha, integration string) (repo, reason string) {
	repos := e.repos(ctx, workdir)
	if len(repos) == 0 {
		return "", "no git repository found at or below " + workdir + ", so the deployed commit cannot be traced locally"
	}
	// A commit id IS an identity: several repositories holding it are clones of
	// one history and answer alike (the reasoning delivery.mergeCheck records),
	// so the first holder is THE repository. Worktree duplicates were already
	// collapsed by the shared-ref-database dedup upstream.
	for _, r := range repos {
		if deployHaveObject(ctx, e.gitRun, r, sha) {
			return r, ""
		}
	}
	attempts := 0
	for _, r := range repos {
		if attempts >= maxDeployLagFetches {
			break
		}
		if _, err := e.gitRun(ctx, r, "fetch", "--quiet", "--", deployRemoteOf(ctx, e.gitRun, r), integration); err != nil {
			continue
		}
		attempts++
		if deployHaveObject(ctx, e.gitRun, r, sha) {
			return r, ""
		}
	}
	return "", fmt.Sprintf("deployed commit %s is held by no repository at or below %s, even after fetching %s into %s - the clones are shallow or belong to other histories",
		shortSHA(sha), workdir, integration, plural(attempts, "one repository", fmt.Sprintf("%d repositories", attempts)))
}

// humanAge renders a duration the way a preflight line wants it: coarse units,
// no sub-minute noise.
func humanAge(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return strconv.Itoa(int(d.Hours()/24)) + " days"
	case d >= 2*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " hours"
	case d >= time.Hour:
		mins := int(d.Minutes())
		return strconv.Itoa(mins/60) + "h" + strconv.Itoa(mins%60) + "m"
	default:
		return strconv.Itoa(int(d.Minutes())) + " min"
	}
}
