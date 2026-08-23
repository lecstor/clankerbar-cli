package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/delivery"
	"github.com/lecstor/clankerbar-cli/internal/harness"
	"github.com/lecstor/clankerbar-cli/internal/plane"
	"github.com/lecstor/clankerbar-cli/internal/salvage"
)

// CLA-437: the driver starts each phase's sessions in the TASK repo's checkout,
// fails the iteration with repo_not_found rather than start in the workdir, and
// hands every declared checkout to the permission policy. The fake here extends
// fakeReleaser with the two task-read methods the real plane client grew.

type repoReleaser struct {
	fakeReleaser
	next        plane.NextTask
	peekErr     error
	taskRepo    map[string]string // taskID -> repo
	repoErr     error
	peeks       int
	repoLookups []string
}

func (r *repoReleaser) PeekNextTask(context.Context) (plane.NextTask, error) {
	r.peeks++
	if r.peekErr != nil {
		return plane.NextTask{}, r.peekErr
	}
	return r.next, nil
}

func (r *repoReleaser) TaskRepo(_ context.Context, taskID string) (string, error) {
	r.repoLookups = append(r.repoLookups, taskID)
	if r.repoErr != nil {
		return "", r.repoErr
	}
	if repo, ok := r.taskRepo[taskID]; ok {
		return repo, nil
	}
	return "", nil
}

func gitCheckout(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	return path
}

// twoRepos builds a multi-repo parent with two checkouts and the Target that
// declares them.
func twoRepos(t *testing.T) (parent, dirA, dirB string, target Target) {
	t.Helper()
	parent = t.TempDir()
	dirA = gitCheckout(t, filepath.Join(parent, "repo-a"))
	dirB = gitCheckout(t, filepath.Join(parent, "repo-b"))
	target = Target{
		Poller:      busyPoller(),
		WorkDir:     parent,
		Repos:       map[string]string{"acme/repo-a": dirA, "acme/repo-b": dirB},
		PrimaryRepo: "acme/repo-a",
	}
	return parent, dirA, dirB, target
}

// d0 builds a Driver around one target for sessionDir/extraDirsFor tests, which
// need only cfg access, no state dir.
func d0(target Target) *Driver {
	return NewMulti(fastCfg(), nil, []Target{target})
}

func TestSessionDir_FreshPhaseResolvesTheQueueHead(t *testing.T) {
	_, _, dirB, target := twoRepos(t)
	rel := &repoReleaser{next: plane.NextTask{TaskID: "t-1", Repo: "acme/repo-b"}}
	target.Releaser = rel

	dir, err := d0(target).sessionDir(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("sessionDir: %v", err)
	}
	if dir != dirB {
		t.Errorf("sessionDir = %s, want the queue head's checkout %s", dir, dirB)
	}
	if rel.peeks != 1 || len(rel.repoLookups) != 0 {
		t.Errorf("a fresh phase peeks next_task (%d peeks, %d get_task lookups), never get_task", rel.peeks, len(rel.repoLookups))
	}
}

func TestSessionDir_ResumePhaseReadsTheExactTask(t *testing.T) {
	_, _, dirB, target := twoRepos(t)
	prev := &harness.Result{Claim: harness.Claim{TaskID: "t-9"}}
	rel := &repoReleaser{taskRepo: map[string]string{"t-9": "acme/repo-b"}}
	target.Releaser = rel

	dir, err := d0(target).sessionDir(context.Background(), target, prev)
	if err != nil {
		t.Fatalf("sessionDir: %v", err)
	}
	if dir != dirB {
		t.Errorf("sessionDir = %s, want the resumed task's own checkout %s", dir, dirB)
	}
	if rel.peeks != 0 || len(rel.repoLookups) != 1 || rel.repoLookups[0] != "t-9" {
		t.Errorf("a resumed phase reads get_task(%v), never peeks (%d)", rel.repoLookups, rel.peeks)
	}
}

func TestSessionDir_NoRepoFallsBackToPrimaryNeverWorkdir(t *testing.T) {
	parent, dirA, _, target := twoRepos(t)
	target.Releaser = &repoReleaser{} // the queue head carries NO repo

	dir, err := d0(target).sessionDir(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("sessionDir: %v", err)
	}
	if dir != dirA {
		t.Errorf("sessionDir = %q (parent %q), want the primary repo's checkout %s - NEVER the workdir", dir, parent, dirA)
	}
}

func TestSessionDir_LegacyWhenNothingConfigured(t *testing.T) {
	d := New(fastCfg(), nil, nil)
	target := Target{Poller: busyPoller(), WorkDir: "/repos/parent", Releaser: &fakeReleaser{}}

	dir, err := d.sessionDir(context.Background(), target, nil)
	if err != nil || dir != "" {
		t.Errorf("got (%q, %v); with nothing declared resolution has no opinion and the spawn keeps its legacy workdir", dir, err)
	}
}

func TestSessionDir_LookupFailureDegradesThenRefusesAmbiguity(t *testing.T) {
	_, dirA, _, target := twoRepos(t)
	// A peek blip is degradation: no identity is known, so the fallback chain runs.
	rel := &repoReleaser{peekErr: errors.New("plane 503")}
	target.Releaser = rel
	dir, err := d0(target).sessionDir(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("with a resolvable primary a peek blip must degrade, not fail: %v", err)
	}
	if dir != dirA {
		t.Errorf("dir = %q, want the primary after the failed peek", dir)
	}
	// But an AMBIGUOUS config (two repos, no primary) plus a lookup failure is
	// refused loudly - starting somewhere unconsidered is the bug this replaces.
	amb := Target{
		Poller:   busyPoller(),
		WorkDir:  "/repos",
		Repos:    map[string]string{"acme/a": "/repos/a", "acme/b": "/repos/b"},
		Releaser: &repoReleaser{peekErr: errors.New("plane 503")},
	}
	if _, err := d0(amb).sessionDir(context.Background(), amb, nil); !errors.Is(err, config.ErrRepoNotFound) {
		t.Errorf("err = %v, want ErrRepoNotFound for an ambiguous config that cannot even learn the task", err)
	}
}

func TestSessionDir_UnknownRepoIsLoud(t *testing.T) {
	_, _, _, target := twoRepos(t)
	target.Releaser = &repoReleaser{next: plane.NextTask{TaskID: "t-2", Repo: "other/nowhere"}}

	_, err := d0(target).sessionDir(context.Background(), target, nil)
	if !errors.Is(err, config.ErrRepoNotFound) || !strings.Contains(err.Error(), "repo_not_found") {
		t.Fatalf("err = %v, want the literal repo_not_found failure", err)
	}
}

// The full drain wiring: the session spawns in the RESOLVED checkout, and the
// salvage sees that same directory (its worktree lives where the session ran,
// not wherever the target points).
func TestDrainSpawnsInResolvedCheckoutAndSalvagesThere(t *testing.T) {
	cfg := fastCfg()
	// The scripted session ends HOLDING its claim with a delivery report, which
	// is what routes an ending through salvage and delivery verification at all.
	step := reported(held(okResult(7, 0), openClaim()), branchReport())
	h := &fakeAdapter{steps: []invokeStep{{res: step}}}
	_, dirA, dirB, target := twoRepos(t)
	target.Releaser = &repoReleaser{next: plane.NextTask{TaskID: "t-1", Repo: "acme/repo-b"}}
	d := NewMulti(cfg, h, []Target{target})
	openTestStateDir(t, d)

	var salved, verified []string
	d.newSalvager = func(workdir string) workSalvager {
		salved = append(salved, workdir)
		return noopSalvager{}
	}
	d.newVerifier = func(workdir string, _ bool) deliveryVerifier {
		verified = append(verified, workdir)
		return noopVerifier{}
	}

	if _, _, stop, err := d.drainWithRetries(context.Background(), 1, d.targets[0], spend{start: time.Now()}); err != nil || stop {
		t.Fatalf("stop=%v err=%v", stop, err)
	}
	if h.invokeCalls != 1 {
		t.Fatalf("got %d invokes, want 1", h.invokeCalls)
	}
	if got := h.invocations[0].WorkDir; got != dirB {
		t.Errorf("spawn cwd = %q, want the queue head's checkout %q", got, dirB)
	}
	// The policy covers the sibling too, so starting in repo B does not wall off A.
	if !containsPath(h.invocations[0].ExtraDirs, dirA) {
		t.Errorf("ExtraDirs = %v, want the other declared checkout %q", h.invocations[0].ExtraDirs, dirA)
	}
	if containsPath(h.invocations[0].ExtraDirs, dirB) {
		t.Errorf("ExtraDirs = %v; the spawn directory itself must not be re-granted", h.invocations[0].ExtraDirs)
	}
	if len(salved) != 1 || salved[0] != dirB {
		t.Errorf("salvage ran for %v, want the actual spawn dir %q", salved, dirB)
	}
	if len(verified) != 1 || verified[0] != dirB {
		t.Errorf("delivery checks ran for %v, want the actual spawn dir %q", verified, dirB)
	}
}

// An unresolvable task repo fails the iteration BEFORE anything spawns: no
// session, no tokens, one loud repo_not_found error.
func TestDrainRepoNotFoundFailsBeforeSpawning(t *testing.T) {
	parent, _, _, target := twoRepos(t)
	target.Repos["acme/ghost"] = filepath.Join(parent, "no-such-checkout")
	target.Releaser = &repoReleaser{next: plane.NextTask{TaskID: "t-3", Repo: "acme/ghost"}}
	h := &fakeAdapter{}
	d := NewMulti(fastCfg(), h, []Target{target})
	openTestStateDir(t, d)

	_, _, _, err := d.drainWithRetries(context.Background(), 1, d.targets[0], spend{start: time.Now()})
	if !errors.Is(err, config.ErrRepoNotFound) || !strings.Contains(err.Error(), "repo_not_found") {
		t.Fatalf("err = %v, want the literal repo_not_found failure", err)
	}
	if h.invokeCalls != 0 {
		t.Errorf("got %d invokes, want none - the iteration fails before any session", h.invokeCalls)
	}
}

// extraDirsFor: declared checkouts minus the spawn dir, plus the conventional
// <checkout>-wt worktree area when it exists on disk.
func TestExtraDirsForIncludesConventionalWorktreeArea(t *testing.T) {
	parent, dirA, dirB, target := twoRepos(t)
	wtHome := filepath.Join(parent, "repo-b-wt")
	gitCheckout(t, wtHome)

	got := d0(target).extraDirsFor(target, dirB)
	if !containsPath(got, dirA) || !containsPath(got, wtHome) {
		t.Errorf("extra dirs = %v, want the sibling repo %q and the existing worktree area %q", got, dirA, wtHome)
	}
	if containsPath(got, dirB) {
		t.Errorf("extra dirs = %v; the spawn dir itself must be excluded", got)
	}
	// No speculation: an absent -wt area grants nothing extra.
	if err := os.RemoveAll(wtHome); err != nil {
		t.Fatalf("remove: %v", err)
	}
	got = d0(target).extraDirsFor(target, dirB)
	if containsPath(got, wtHome) {
		t.Errorf("extra dirs = %v; a nonexistent worktree area must not be invented", got)
	}
	// Nothing configured: nothing granted.
	legacy := Target{Poller: busyPoller(), WorkDir: parent, Releaser: &fakeReleaser{}}
	if got := d0(legacy).extraDirsFor(legacy, dirB); len(got) != 0 {
		t.Errorf("extra dirs = %v, want none when nothing is declared and nothing exists", got)
	}
}

// --- helpers -----------------------------------------------------------------

type noopSalvager struct{}

func (noopSalvager) Salvage(context.Context, string, string) salvage.Outcome {
	return salvage.Outcome{Status: salvage.Nothing}
}

type noopVerifier struct{}

func (noopVerifier) Verify(context.Context, delivery.Claim) delivery.Report {
	return delivery.Report{}
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}
