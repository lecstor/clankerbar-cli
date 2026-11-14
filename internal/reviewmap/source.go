package reviewmap

import (
	"fmt"
	"os/exec"
)

// gitSource reads a path from the revisions a Range names, without depending
// on the worktree's checkout state.
type gitSource struct {
	dir  string
	base string
	head string
}

func (s gitSource) Head(path string) ([]byte, error) { return s.show(s.head, path) }

func (s gitSource) Base(path string) ([]byte, error) { return s.show(s.base, path) }

func (s gitSource) show(rev, path string) ([]byte, error) {
	cmd := exec.Command("git", "show", rev+":"+path)
	cmd.Dir = s.dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git show %s:%s: %w", rev, path, err)
	}
	return out, nil
}

// sourceFor returns the file reader for r.
func sourceFor(r Range) Source {
	return gitSource{dir: r.Repo, base: r.Base, head: r.Head}
}
