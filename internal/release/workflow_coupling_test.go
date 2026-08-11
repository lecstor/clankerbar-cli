package release

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The release gate waits for a check run BY NAME, and GitHub derives that name
// from the job that produced it. So `DefaultRequiredCheck` and ci.yml's job name
// are one fact stored in two files, in two languages, that nothing connects.
//
// The failure mode if they drift is the bad kind: renaming the CI job does not
// break anything visibly: the gate simply never finds its check, waits out its
// full timeout, and refuses the release. Publishing stops, and the reason is a
// string mismatch two files away from where anyone would look.
//
// This test is the connection. It reads the workflow and asserts the job name
// the gate is looking for is actually there.
func TestRequiredCheckMatchesTheCIWorkflow(t *testing.T) {
	path := filepath.Join("..", "..", ".github", "workflows", "ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		// Skip rather than fail: internal/release must remain testable if it is
		// ever vendored somewhere without the workflow tree beside it.
		t.Skipf("cannot read %s: %v", path, err)
	}
	content := string(raw)

	// The `name:` of a job, at job-key indentation. Anchored per-line so a `name:`
	// belonging to a STEP (indented deeper) cannot satisfy this.
	jobName := regexp.MustCompile(`(?m)^    name: (.+)$`)

	var names []string
	for _, m := range jobName.FindAllStringSubmatch(content, -1) {
		names = append(names, strings.TrimSpace(m[1]))
	}
	if len(names) == 0 {
		t.Fatalf("found no job-level `name:` in %s - the coupling this test guards "+
			"cannot be checked, which is itself the problem", path)
	}

	for _, n := range names {
		if n == DefaultRequiredCheck {
			return // found it
		}
	}

	t.Errorf("no job in %s is named %q, so the release gate would wait for a check "+
		"that is never produced, time out, and silently refuse every release. "+
		"Job names present: %q. Either restore the job name or update "+
		"DefaultRequiredCheck (and the --required flag in release-on-merge.yml) "+
		"to match.", path, DefaultRequiredCheck, names)
}

// A matrix on the ci job would rename its check runs to `ci (ubuntu-latest)` and
// friends, defeating the exact-name match above just as surely as a rename.
// There is no matrix today; this fails if one appears without the gate being
// taught about it.
func TestCIWorkflowHasNoMatrix(t *testing.T) {
	path := filepath.Join("..", "..", ".github", "workflows", "ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("cannot read %s: %v", path, err)
	}

	// Match the `strategy:`/`matrix:` KEYS only, not the words in prose comments.
	matrix := regexp.MustCompile(`(?m)^\s+(strategy|matrix):\s*$`)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if matrix.MatchString(line) {
			t.Errorf("%s declares a build matrix (%q). GitHub then names its check "+
				"runs %q (…), which the release gate's exact-name match on %q will "+
				"never find - it would time out and refuse every release. Teach "+
				"internal/release to match a prefix before adding one.",
				path, strings.TrimSpace(line), DefaultRequiredCheck+" (…)", DefaultRequiredCheck)
		}
	}
}

// Both integration branches must stay in ci.yml's `push:` list, and each is
// load-bearing for a DIFFERENT thing - so losing either is silent until someone
// tries to publish, which is exactly when a guard is worth having.
//
//   - `main`  - release-on-merge.yml and realign-staging.yml both wait for the
//     `ci` check on the merge commit ON MAIN. Drop it and both wait for a check
//     nothing produces.
//   - `staging` - it is what keeps the standing promotion PR MERGEABLE. promote.yml
//     opens that PR with GITHUB_TOKEN, which triggers no `pull_request` run, so
//     the `ci` check `main` requires can only come from the push run on the head
//     SHA.
//
// The `staging` half has no other protection: nothing fails until a human opens
// the promotion PR and finds a required check that never arrives.
func TestCIWorkflowRunsOnBothIntegrationBranches(t *testing.T) {
	path := filepath.Join("..", "..", ".github", "workflows", "ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("cannot read %s: %v", path, err)
	}

	// The `branches:` list under `push:`, as an inline flow sequence. Comments in
	// this file discuss both names in prose, so match the KEY's value, not the
	// words wherever they appear.
	pushBranches := regexp.MustCompile(`(?m)^  push:\n    branches: \[(.+)\]$`)
	m := pushBranches.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("could not find a `push:`/`branches: [...]` block in %s - the "+
			"coupling this test guards cannot be checked, which is itself the "+
			"problem. If the trigger was reshaped, reshape this test with it.", path)
	}

	var listed []string
	for _, b := range strings.Split(m[1], ",") {
		listed = append(listed, strings.TrimSpace(b))
	}

	for _, want := range []string{"main", "staging"} {
		if !slices.Contains(listed, want) {
			t.Errorf("%s does not run on push to %q (push branches: %q). See this "+
				"test's doc comment for what breaks - `main` starves the release "+
				"gate and the staging realign; `staging` strands the standing "+
				"promotion PR behind a required check that is never produced.",
				path, want, listed)
		}
	}
}
