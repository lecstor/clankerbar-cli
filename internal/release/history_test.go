package release

import (
	"os/exec"
	"strings"
	"testing"
)

// The derivation rules above are asserted against synthetic commit messages.
// This test asserts them against THIS REPOSITORY'S ACTUAL HISTORY, which is the
// only thing that can catch the rules drifting away from how people here really
// write commits - a parser that is internally consistent and disagrees with the
// repo derives a wrong version very confidently.
//
// It SKIPS rather than fails whenever the history is not available: CI checks
// out at the default depth with no tags, and a fresh or shallow clone has
// nothing to read. A skip is honest there; failing would make every CI run red
// for a reason that has nothing to do with the change under test.
func TestDerivation_AgainstRealHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}

	// The two most recent releases at the time this was written. Reading a REAL
	// released range means the expectation below is a fact about what shipped,
	// not a guess.
	const from, to = "v0.3.3", "v0.3.4"

	if err := exec.Command("git", "rev-parse", "--verify", from+"^{commit}").Run(); err != nil {
		t.Skipf("tag %s is not present (shallow clone or no tags fetched)", from)
	}
	if err := exec.Command("git", "rev-parse", "--verify", to+"^{commit}").Run(); err != nil {
		t.Skipf("tag %s is not present (shallow clone or no tags fetched)", to)
	}

	// The same command the workflow runs: non-merge commits only, full messages,
	// NUL-separated.
	out, err := exec.Command("git", "log", "--no-merges", "--format=%B%x00", from+".."+to).Output()
	if err != nil {
		t.Skipf("git log failed: %v", err)
	}

	var messages []string
	for _, m := range strings.Split(string(out), "\x00") {
		if strings.TrimSpace(m) != "" {
			messages = append(messages, m)
		}
	}
	if len(messages) == 0 {
		t.Skipf("no commits found in %s..%s", from, to)
	}

	d, err := NextFromCommits(from, messages)
	if err != nil {
		t.Fatalf("NextFromCommits over real history: %v", err)
	}

	t.Logf("%s..%s: %d non-merge commits imply a %s bump -> %s",
		from, to, len(messages), d.Bump, d.Next)

	if !d.Release {
		t.Errorf("a released range derived no release at all (bump=%s)", d.Bump)
	}

	// Every commit in a real released range must be CLASSIFIABLE. This is the
	// clause that actually earns the test: if someone starts writing subjects
	// this parser does not recognise, they silently fall into the
	// unrecognised->patch branch, and a `feat` written the wrong way would stop
	// producing a minor. Catch it here rather than in a wrong tag.
	var unrecognised []string
	for _, m := range messages {
		// Subject() deliberately, not an inline first-line split: the two must not
		// drift, and it was an inline split that hid the leading-newline bug this
		// test first caught.
		subject := Subject(m)
		if subjectRe.FindStringSubmatch(subject) == nil {
			unrecognised = append(unrecognised, subject)
		}
	}
	if len(unrecognised) > 0 {
		t.Errorf("%d/%d real commits are not conventional-commit shaped, so they fall to the "+
			"unrecognised->patch branch: %q", len(unrecognised), len(messages), unrecognised)
	}
}
