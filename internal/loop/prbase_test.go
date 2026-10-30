package loop

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The two shapes `git ls-remote --symref origin HEAD refs/heads/<integration>`
// actually prints, captured from the real command: the symref line for a
// remote whose HEAD is symbolic, then one line per matching ref. parseOriginRefs
// is exact-field matching on purpose, and the trailing-name case below is why.
func TestParseOriginRefs(t *testing.T) {
	tests := []struct {
		name      string
		out       string
		integ     string
		onOrigin  bool
		defaultBr string
	}{
		{
			name:      "branch present, HEAD symbolic",
			out:       "ref: refs/heads/main\tHEAD\n228b8021ea1b4cbf32cdac14b9e10dc52c490fcb\tHEAD\n228b8021ea1b4cbf32cdac14b9e10dc52c490fcb\trefs/heads/staging\n",
			integ:     "staging",
			onOrigin:  true,
			defaultBr: "main",
		},
		{
			name:      "branch absent, HEAD symbolic",
			out:       "ref: refs/heads/main\tHEAD\n228b8021ea1b4cbf32cdac14b9e10dc52c490fcb\tHEAD\n",
			integ:     "staging",
			onOrigin:  false,
			defaultBr: "main",
		},
		{
			name:     "detached remote HEAD: no symref line, default unknown",
			out:      "228b8021ea1b4cbf32cdac14b9e10dc52c490fcb\tHEAD\n",
			integ:    "staging",
			onOrigin: false,
		},
		{
			name:     "empty output (e.g. filtered nothing)",
			out:      "",
			integ:    "staging",
			onOrigin: false,
		},
		{
			// Exact-field matching: "refs/heads/staging" must not satisfy an
			// integration branch whose name is a suffix of it.
			name:      "a ref is not matched by a suffix of its name",
			out:       "ref: refs/heads/main\tHEAD\n228b8021ea1b4cbf32cdac14b9e10dc52c490fcb\trefs/heads/staging\n",
			integ:     "ging",
			onOrigin:  false,
			defaultBr: "main",
		},
		{
			// The integration branch IS the default: both answers are the branch.
			name:      "integration branch is the default branch",
			out:       "ref: refs/heads/main\tHEAD\n228b8021ea1b4cbf32cdac14b9e10dc52c490fcb\trefs/heads/main\n",
			integ:     "main",
			onOrigin:  true,
			defaultBr: "main",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			onOrigin, defaultBr := parseOriginRefs(tc.out, tc.integ)
			if onOrigin != tc.onOrigin || defaultBr != tc.defaultBr {
				t.Errorf("parseOriginRefs(%q) = (%v, %q), want (%v, %q)", tc.integ, onOrigin, defaultBr, tc.onOrigin, tc.defaultBr)
			}
		})
	}
}

// ProbeOriginPRBase against real repositories: an origin whose default is main
// with a staging branch on top (clankerbar-cli/ezyapp's shape), and an origin
// with only main (the makesPDF plugins' shape). This covers the exec, the
// --symref invocation and the parse together — the parse alone is unit-tested
// above, but only a real git proves the invocation asks the right question.
func TestProbeOriginPRBase_RealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()
	root := t.TempDir()

	// Origin A: main (default) + staging.
	originA := filepath.Join(root, "a.git")
	runGitTest(t, root, "init", "--bare", "-b", "main", originA)
	workA := filepath.Join(root, "work-a")
	runGitTest(t, root, "clone", originA, workA)
	runGitTest(t, workA, "-c", "user.email=t@test", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")
	runGitTest(t, workA, "push", "-u", "origin", "main")
	runGitTest(t, workA, "branch", "staging")
	runGitTest(t, workA, "push", "origin", "staging")

	onOrigin, def, err := ProbeOriginPRBase(ctx, workA, "staging")
	if err != nil || !onOrigin || def != "main" {
		t.Errorf("probe with staging on origin = (%v, %q, %v), want (true, main, nil)", onOrigin, def, err)
	}
	// An integration branch that is NOT on origin falls to the default branch.
	onOrigin, def, err = ProbeOriginPRBase(ctx, workA, "integration")
	if err != nil || onOrigin || def != "main" {
		t.Errorf("probe with no integration on origin = (%v, %q, %v), want (false, main, nil)", onOrigin, def, err)
	}

	// Origin B: main only — the makesPDF plugin shape.
	originB := filepath.Join(root, "b.git")
	runGitTest(t, root, "init", "--bare", "-b", "main", originB)
	workB := filepath.Join(root, "work-b")
	runGitTest(t, root, "clone", originB, workB)
	runGitTest(t, workB, "-c", "user.email=t@test", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")
	runGitTest(t, workB, "push", "-u", "origin", "main")

	onOrigin, def, err = ProbeOriginPRBase(ctx, workB, "staging")
	if err != nil || onOrigin || def != "main" {
		t.Errorf("probe against a main-only origin = (%v, %q, %v), want (false, main, nil)", onOrigin, def, err)
	}

	// Not a repository at all: the probe errors, which prBaseFor turns into the
	// integration-branch fallback rather than a failed spawn.
	if _, _, err := ProbeOriginPRBase(ctx, root, "staging"); err == nil {
		t.Error("probe outside any repository must error so the caller can fall back")
	}
}

func runGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
