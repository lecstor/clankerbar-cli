package harness

import (
	"path/filepath"
	"strings"
)

// pinPWD returns env with the child's PWD pinned to workDir - the directory the
// caller is about to set as cmd.Dir - and OLDPWD dropped. An empty workDir
// returns env untouched: there is nothing to pin it to, and inventing one would
// move a session the caller deliberately left in the driver's own cwd.
//
// exec.Cmd.Dir changes the child's REAL working directory and nothing else. The
// inherited PWD still names wherever the daemon was started, because a shell -
// not the kernel - is what keeps that variable honest, and no shell is involved
// in an exec. Most tools ignore PWD. opencode does not.
//
// CLA-441, verified live against opencode 1.18.19: opencode bootstraps an
// instance at the real cwd, then creates a SECOND instance at $PWD and runs the
// session THERE. Reproduced without the driver - `opencode run` with cwd
// ~/dev/ezyapp and PWD=/Users/jason/dev/clankerbar-cli lands in clankerbar-cli;
// the same command with no PWD lands in ezyapp. So every session every daemon
// spawned ran in the daemon's start directory, whatever the project's workdir
// said, and three separate defects were all this one:
//
//   - the wrong project's MCP server, because opencode merges the project-level
//     opencode.json it discovers from the INSTANCE directory after the file the
//     driver names, so the start directory's repo redirected `mcp.clankerbar`
//     (14 CLA-* leases were held by sessions labelled [ezyapp]);
//   - read/edit asks in a shape the permission policy cannot match, because
//     opencode makes them relative to the git worktree it resolves at the
//     instance directory, not at cmd.Dir (see opencodePermission);
//   - repo files - AGENTS.md, CLAUDE.md - read from the wrong repo.
//
// Every adapter here inherits the parent environment wholesale, so every adapter
// carried it; the fix is applied to all three rather than to opencode alone,
// because "which harness honours $PWD" is not a fact this repo controls.
//
// OLDPWD is dropped rather than rewritten. It means "the directory a shell cd'd
// FROM", and a value inherited from the daemon's shell describes a jump the
// child never made; there is no honest value to give it here.
//
// The pin is ABSOLUTE. Whoever honours $PWD reads it as a path with no cwd to
// resolve it against except the child's own - the very thing being pinned - so a
// relative cmd.Dir must not travel as a relative PWD. An unresolvable workDir
// falls back to the value as given: still the caller's directory, merely
// unnormalised, which is strictly better than the daemon's.
//
// It filters the INHERITED environment only. Callers apply it to os.Environ()
// before appending Invocation.Env, so an explicit caller-supplied PWD still wins
// on the last-duplicate-wins rule os/exec applies - the same precedence the
// adapters give every other variable they set.
func pinPWD(env []string, workDir string) []string {
	if workDir == "" {
		return env
	}
	dir := workDir
	if abs, err := filepath.Abs(workDir); err == nil {
		dir = abs
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "PWD=") || strings.HasPrefix(kv, "OLDPWD=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PWD="+dir)
}
