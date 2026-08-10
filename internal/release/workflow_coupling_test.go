package release

import (
	"os"
	"path/filepath"
	"regexp"
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
