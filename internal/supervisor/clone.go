package supervisor

// Cloning a project's repos into the fleet's own layout (CLA-678).
//
// The fleet works in its OWN clones, one per project under the machine root
// (see workdir.go). A clone is made from the plane's repo identity — which is
// why the identity has to be `owner/name` and not a bare name: an identity with
// no owner names no clone URL, and guessing one would clone somebody else's
// repo or nobody's. The credential comes from the run's own git credential
// (delivery.TokenForOwner), the same `gh auth token --user <owner>` resolution
// the delivery verifier and doctor's deploy-lag check already read remotes
// with, scoped to this one git invocation through the route in delivery.ScopeEnv
// — the token rides the child's ENVIRONMENT, never its argv.
//
// What is deliberately NOT here: fetching, resetting, pulling. A clone is made
// once; after that the repo directory is the fleet's working checkout and its
// own operations (fetch, branch, worktree) belong to the sessions. The
// supervisor only ever creates, verifies, and reports.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/delivery"
)

// cloneGHBin is the gh binary the clone's credential scoping resolves an owner's
// token with; empty means no `gh`, and the clone then runs on whatever ambient
// credential git finds (which is enough for a public repo, and fails loudly for
// a private one). A var so tests can point it at a stub.
var cloneGHBin = "gh"

// cloneRemoteBase is the URL prefix a repo identity is cloned from:
// <base><owner>/<name>.git. github.com is also what the credential scoping
// understands (delivery.GithubOwner); an ssh/other-host remote in the
// operator's own clones has no bearing here, because those clones are not read.
// A var so a test can point the fleet at a local bare repository instead of the
// network — the clone is otherwise the only thing here that cannot be faked.
var cloneRemoteBase = "https://github.com/"

// cloneTimeout bounds one clone. A clone is a full history fetch: it is the
// longest thing the supervisor does, and the only one that cannot be allowed to
// hang a reconcile forever. A timeout refuses the instance for this reconcile
// (the next one retries) rather than wedging the supervisor.
var cloneTimeout = 5 * time.Minute

// cloneRepo materializes repo ("owner/name") at dir, creating the project root
// above it if needed. It refuses when the identity names no owner, because a
// bare name cannot be resolved to a clone URL.
//
// The clone lands in a temporary sibling and is RENAMED into place only once it
// has succeeded, so a failed or interrupted clone can never leave a
// directory-shaped failure at the derived path (the next reconcile would read
// that as "exists but is not a checkout", which is the shape it must refuse).
//
// The install is NO-REPLACE. git clone runs for minutes, and a destination that
// appears in the meantime belongs to whoever created it: Go's os.Rename already
// refuses to replace an existing DIRECTORY (os/file_unix.go returns EEXIST), but
// it does replace a file or a symlink, so the destination is Lstat-ed
// immediately before the rename and every shape is refused. A raced-in path is
// left exactly as it was and the clone is discarded.
func cloneRepo(ctx context.Context, dir, repo string) error {
	owner, name, err := splitRepoIdentity(repo)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("create %s: %v", filepath.Dir(dir), err)
	}
	url := cloneRemoteBase + owner + "/" + name + ".git"
	tmp := fmt.Sprintf("%s.clone-%d", dir, os.Getpid())

	env := []string{
		// The same interaction-hostile discipline delivery.gitRun applies to
		// every network git it runs: an unattended supervisor has no terminal,
		// and a git that decides to ask for a password would hang the reconcile
		// instead of answering the question.
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=echo",
		"SSH_ASKPASS=echo",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=10",
		"GCM_INTERACTIVE=never",
	}
	if token := delivery.TokenForOwner(ctx, filepath.Dir(dir), cloneGHBin, owner); token != "" {
		env = append(env, delivery.ScopeEnv(token)...)
	}

	ctx, cancel := context.WithTimeout(ctx, cloneTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "clone", "--", url, tmp)
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("git clone %s: %v: %s", url, err, firstLine(stderr.String()))
	}
	if err := installClone(tmp, dir, repo); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return nil
}

// installClone moves a completed clone from tmp into dir with NO-REPLACE
// semantics: git clone runs for minutes, and a destination that appears in the
// meantime belongs to whoever created it. Go's os.Rename refuses to replace an
// existing DIRECTORY (os/file_unix.go returns EEXIST) but WOULD replace a file
// or a symlink, so every shape is checked here, immediately before the rename,
// and a path that raced in while git ran is left untouched — the completed clone
// is discarded rather than installed over somebody else's entry.
//
// The check and the rename are two calls, so a path created between them would
// still be replaced; that window is microseconds rather than the whole clone,
// which is the portable limit without a platform-specific no-replace rename
// (renameat2/renamex_np) and a new dependency.
func installClone(tmp, dir, repo string) error {
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("%s appeared while %s was being cloned - refusing to replace a path the fleet did not create", dir, repo)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("cannot check the destination %s for the clone of %s: %v", dir, repo, err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		return fmt.Errorf("move the clone of %s into %s: %v", repo, dir, err)
	}
	return nil
}

// splitRepoIdentity splits "owner/name" into its two halves, refusing anything
// that is not exactly that shape. A bare name is refused rather than resolved
// against a host: there is no owner to clone from and no owner whose token the
// credential scoping could ask for.
func splitRepoIdentity(repo string) (string, string, error) {
	parts := strings.Split(strings.Trim(repo, "/"), "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("repo identity %q is not owner/name, so there is no clone URL to derive", repo)
	}
	owner, name := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if owner == "" || name == "" || name == "." || name == ".." || strings.ContainsAny(name, `\`) {
		return "", "", fmt.Errorf("repo identity %q is not owner/name, so there is no clone URL to derive", repo)
	}
	return owner, name, nil
}

// sortedKeys returns a map's keys in sorted order, so a derivation's failures
// and log lines are stable across runs rather than iterating a map.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// firstLine trims a git stderr message to its first line, so a multi-line
// remote error does not run into the log's next sentence.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
