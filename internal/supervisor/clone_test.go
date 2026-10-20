package supervisor

// The clone's install step (CLA-678): a completed clone is moved into the
// derived path only when that path is still absent. git clone runs for minutes,
// so a directory that appears while it runs belongs to whoever created it and
// must be left untouched — the fail-closed posture the derivation keeps for
// every other path it does not own.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
)

// installClone moves a completed clone into place when nothing is there.
func TestInstallCloneMovesIntoAnAbsentPath(t *testing.T) {
	base := t.TempDir()
	tmp := filepath.Join(base, "widgets.clone-1")
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "widgets")
	if err := installClone(tmp, dir, "acme/widgets"); err != nil {
		t.Fatalf("installClone: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("the clone was not installed at %s: %v", dir, err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("the temporary clone still exists after install: %v", err)
	}
}

// A destination that raced in while git ran is NEVER replaced: not an empty
// directory, not a file, not a symlink. os.Rename alone would refuse the
// directory but silently replace the file and the symlink, so each shape is
// pinned here.
func TestInstallCloneRefusesARacedInDestination(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "widgets")

	// An empty directory that appeared during the clone.
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(base, "widgets.clone-2")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	err := installClone(tmp, dir, "acme/widgets")
	if err == nil {
		t.Fatal("installClone replaced a directory that appeared during the clone")
	}
	if !strings.Contains(err.Error(), "appeared") {
		t.Errorf("err %q does not say the destination appeared during the clone", err)
	}
	if entries, readErr := os.ReadDir(dir); readErr != nil || len(entries) != 0 {
		t.Fatalf("the raced-in directory was modified: %v %v", entries, readErr)
	}

	// A file at the destination: os.Rename WOULD replace this one, so the
	// check is what stops it.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("someone else's file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installClone(tmp, dir, "acme/widgets"); err == nil {
		t.Fatal("installClone replaced a file that appeared during the clone")
	}
	if b, readErr := os.ReadFile(dir); readErr != nil || string(b) != "someone else's file" {
		t.Fatalf("the raced-in file was modified: %q %v", b, readErr)
	}

	// A symlink at the destination: likewise refused, and its target untouched.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "outside")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	if err := installClone(tmp, dir, "acme/widgets"); err == nil {
		t.Fatal("installClone replaced a symlink that appeared during the clone")
	}
	if fi, err := os.Lstat(dir); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the raced-in symlink was replaced: %v", err)
	}
	if entries, readErr := os.ReadDir(target); readErr != nil || len(entries) != 0 {
		t.Fatalf("the symlink's target was modified: %v %v", entries, readErr)
	}
}

// --- CLA-713: the clone remote is configurable -----------------------------
//
// The fleet's derived clones used to be HTTPS-only, which made a session's
// branch pushes depend on a GitHub token riding in its environment. These tests
// pin the three things the change must not get wrong: the default stays
// https://github.com/, an ssh base yields git@<alias>:owner/name.git and clones,
// and an ssh base spawns no `gh auth token`.

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// gitShim puts a `git` first on PATH that fakes the CLONE (recording the URL it
// was asked for and creating a repo whose origin is that URL) and delegates
// every other invocation to the real git - so the derivation's post-clone
// verification and the attribution check run against real git. The clone is
// faked because these tests use URLs that are deliberately not fetchable.
func gitShim(t *testing.T) (recordPath string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	dir := t.TempDir()
	recordPath = filepath.Join(dir, "clone-urls")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = clone ]; then\n" +
		"  printf '%s\\n' \"$3\" >> " + shellQuote(recordPath) + " || exit 1\n" +
		"  " + shellQuote(realGit) + " init -q \"$4\" >/dev/null 2>&1 || exit 1\n" +
		"  " + shellQuote(realGit) + " -C \"$4\" remote add origin \"$3\" || exit 1\n" +
		"  exit 0\n" +
		"fi\n" +
		"exec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return recordPath
}

// ghShim points cloneGHBin at a stub that records being run and answers with a
// token. The marker is the assertion: an ssh clone must never create it.
func ghShim(t *testing.T) (calledPath string) {
	t.Helper()
	dir := t.TempDir()
	calledPath = filepath.Join(dir, "gh-called")
	script := "#!/bin/sh\n: > " + shellQuote(calledPath) + "\necho gho_stub_token\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := cloneGHBin
	cloneGHBin = filepath.Join(dir, "gh")
	t.Cleanup(func() { cloneGHBin = prev })
	return calledPath
}

func TestCloneURL(t *testing.T) {
	cases := []struct {
		name string
		base string
		want string
	}{
		{"the built-in default", config.DefaultCloneURLBase, "https://github.com/acme/widgets.git"},
		{"an ssh host alias", "git@github-personal:", "git@github-personal:acme/widgets.git"},
		{"an ssh host", "git@github.com:", "git@github.com:acme/widgets.git"},
		{"a mirror prefix", "https://git.example.com/mirror/", "https://git.example.com/mirror/acme/widgets.git"},
		{"a local mirror", "file:///srv/mirrors/", "file:///srv/mirrors/acme/widgets.git"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := cloneURL(c.base, "acme", "widgets")
			if err != nil {
				t.Fatalf("cloneURL: %v", err)
			}
			if got != c.want {
				t.Errorf("cloneURL(%q) = %q, want %q", c.base, got, c.want)
			}
		})
	}

	// An empty base means the package default (the var a test can point at a
	// local bare repository), never a hard-coded second copy of it.
	got, err := cloneURL("", "acme", "widgets")
	if err != nil {
		t.Fatalf("cloneURL with the default base: %v", err)
	}
	if want := cloneRemoteBase + "acme/widgets.git"; got != want {
		t.Errorf("cloneURL(\"\") = %q, want %q (the package default base)", got, want)
	}

	// A prefix with no separator is refused HERE, naming the key, rather than
	// concatenated into https://github.comacme/widgets.git.
	if _, err := cloneURL("https://github.com", "acme", "widgets"); err == nil || !strings.Contains(err.Error(), "clone_url_base") {
		t.Fatalf("cloneURL with no separator = %v, want a clone_url_base error", err)
	}
}

// An ssh base is cloned as git@<alias>:owner/name.git, and no gh token is
// resolved for it: the key is the credential, so the session's environment does
// not need one (CLA-713).
func TestDeriveWorkdirClonesFromAnSSHBaseWithoutAToken(t *testing.T) {
	record := gitShim(t)
	ghCalled := ghShim(t)
	root := t.TempDir()

	got, err := deriveWorkdir(context.Background(), root, "acme", "acme/widgets", "git@github-personal:")
	if err != nil {
		t.Fatalf("deriveWorkdir with an ssh base: %v", err)
	}
	if want := filepath.Join(root, "acme", "widgets"); got != want {
		t.Fatalf("deriveWorkdir = %q, want %q", got, want)
	}

	// The installed clone's origin is the ssh URL, so a session's fetch and
	// push use the host alias and the key alone.
	remote, err := checkoutOriginRemote(got)
	if err != nil {
		t.Fatalf("the clone is not an attributable checkout: %v", err)
	}
	if want := "git@github-personal:acme/widgets.git"; remote != want {
		t.Fatalf("the clone's origin is %q, want %q", remote, want)
	}

	// The git child was asked to clone exactly that URL.
	urls, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the git shim recorded no clone: %v", err)
	}
	if want := "git@github-personal:acme/widgets.git\n"; string(urls) != want {
		t.Fatalf("git was asked to clone %q, want %q", urls, want)
	}

	// And no `gh auth token` was spawned: an ssh remote does not consult it.
	if _, err := os.Stat(ghCalled); !os.IsNotExist(err) {
		t.Fatalf("an ssh clone spawned gh (marker %s exists, stat err %v) - the key must be the only credential", ghCalled, err)
	}
}

// The default transport is unchanged end to end: an https github base still
// resolves the owner's token for the clone (the pre-CLA-713 behaviour), and the
// URL is the historical one.
func TestDeriveWorkdirWithTheDefaultBaseStillScopesTheToken(t *testing.T) {
	record := gitShim(t)
	ghCalled := ghShim(t)
	root := t.TempDir()

	if _, err := deriveWorkdir(context.Background(), root, "acme", "acme/widgets", config.DefaultCloneURLBase); err != nil {
		t.Fatalf("deriveWorkdir with the default base: %v", err)
	}
	urls, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the git shim recorded no clone: %v", err)
	}
	if want := "https://github.com/acme/widgets.git\n"; string(urls) != want {
		t.Fatalf("git was asked to clone %q, want %q", urls, want)
	}
	if _, err := os.Stat(ghCalled); err != nil {
		t.Fatalf("the default https github base did not resolve a gh token: %v", err)
	}
}

// A malformed clone_url_base refuses the instance BEFORE any project is
// derived, even when every repo already exists: a typo must not sit dormant
// until the first clone needs it.
func TestDeriveInstanceWorkdirsRefusesAMalformedCloneBase(t *testing.T) {
	cfg := &config.Config{
		BacklogURL:  "https://clankerbar.com/mcp/acme",
		PrimaryRepo: "acme/widgets",
	}
	_, err := deriveInstanceWorkdirs(context.Background(), cfg, t.TempDir(), "https://github.com")
	if err == nil || !strings.Contains(err.Error(), "clone_url_base") || !strings.Contains(err.Error(), "must end with /") {
		t.Fatalf("deriveInstanceWorkdirs = %v, want a clone_url_base refusal naming the missing separator", err)
	}
}

// The base is threaded from the supervisor's per-project derivation, not only
// from the single-repo helper: every repo a project names is cloned from it.
func TestDeriveInstanceWorkdirsUsesTheConfiguredCloneBase(t *testing.T) {
	record := gitShim(t)
	ghShim(t)
	cfg := &config.Config{Projects: []config.Project{
		{Slug: "acme", PrimaryRepo: "acme/widgets", Repos: map[string]string{"acme/gadgets": "gadgets"}},
	}}
	root := t.TempDir()

	if _, err := deriveInstanceWorkdirs(context.Background(), cfg, root, "git@github-personal:"); err != nil {
		t.Fatalf("deriveInstanceWorkdirs with an ssh base: %v", err)
	}
	urls, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the git shim recorded no clone: %v", err)
	}
	got := string(urls)
	for _, want := range []string{"git@github-personal:acme/widgets.git", "git@github-personal:acme/gadgets.git"} {
		if !strings.Contains(got, want) {
			t.Errorf("clones = %q, want it to contain %q", got, want)
		}
	}
	// And the installed checkouts carry that ssh URL as their origin, which is
	// what a session's fetch and push actually resolve.
	for _, name := range []string{"widgets", "gadgets"} {
		remote, err := checkoutOriginRemote(filepath.Join(root, "acme", name))
		if err != nil {
			t.Fatalf("the %s clone is not an attributable checkout: %v", name, err)
		}
		if want := "git@github-personal:acme/" + name + ".git"; remote != want {
			t.Errorf("the %s clone's origin is %q, want %q", name, remote, want)
		}
	}
}
