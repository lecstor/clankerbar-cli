package reviewmap

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Range names the repository and the two revisions a review's diff spans.
type Range struct {
	// Repo is the worktree git commands run in.
	Repo string
	// Base and Head are the revisions to diff, e.g. a merge base and the tip.
	Base string
	Head string
}

// goAttributes maps Go files to git's built-in golang diff driver, which
// labels each hunk header with the enclosing func or type declaration.
const goAttributes = "*.go diff=golang\n"

// Diff returns Base..Head in r.Repo as a unified diff whose Go hunks carry
// their enclosing declaration.
//
// It passes a temporary attributes file with -c core.attributesFile rather
// than writing .gitattributes, so the repository is never modified. A
// repository's own .gitattributes takes precedence where it names a driver.
func Diff(r Range) (string, error) {
	name, cleanup, err := goAttributesFile()
	if err != nil {
		return "", err
	}
	defer cleanup()

	cmd := exec.Command("git", "-c", "core.attributesFile="+name,
		"diff", "--no-ext-diff", "--no-textconv", "--end-of-options",
		r.Base+".."+r.Head, "--")
	cmd.Dir = r.Repo
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return "", fmt.Errorf("git diff %s..%s: %w: %s",
				r.Base, r.Head, err, strings.TrimSpace(string(exit.Stderr)))
		}
		return "", fmt.Errorf("git diff %s..%s: %w", r.Base, r.Head, err)
	}
	return string(out), nil
}

// goAttributesFile writes goAttributes to a temporary file outside any
// repository and returns its path with the function that removes it.
func goAttributesFile() (string, func(), error) {
	f, err := os.CreateTemp("", "reviewmap-attrs-*")
	if err != nil {
		return "", nil, fmt.Errorf("reviewmap diff: %w", err)
	}
	// git runs with its own working directory, so the path must be absolute
	// for a relative TMPDIR to resolve the same file.
	name, err := filepath.Abs(f.Name())
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("reviewmap diff: %w", err)
	}
	cleanup := func() { os.Remove(name) }
	if _, err := f.WriteString(goAttributes); err != nil {
		f.Close()
		cleanup()
		return "", nil, fmt.Errorf("reviewmap diff: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("reviewmap diff: %w", err)
	}
	return name, cleanup, nil
}
