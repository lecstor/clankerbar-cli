package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkout builds a fake checkout (a directory; .git deliberately NOT required,
// matching what resolution promises) and returns its path.
func checkout(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	return path
}

func workdirWith(t *testing.T, entries ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, e := range entries {
		checkout(t, filepath.Join(dir, e))
	}
	return dir
}

// An EMPTY value names an identity the fleet derives itself (CLA-678): the
// key declares the repo, the layout says where it lives, and resolution lands
// on <workdir>/<name> exactly as if the key had been omitted.
func TestResolveCheckoutEmptyValueMeansTheDerivedCheckout(t *testing.T) {
	wd := workdirWith(t, "clankerbar-cli")
	got, err := ResolveCheckout(map[string]string{"lecstor/clankerbar-cli": ""}, "", wd, "lecstor/clankerbar-cli")
	if err != nil {
		t.Fatalf("ResolveCheckout with an empty value: %v", err)
	}
	if want := filepath.Join(wd, "clankerbar-cli"); got != want {
		t.Fatalf("ResolveCheckout = %q, want the derived %q", got, want)
	}
	// The empty value is not a wildcard: an identity with no checkout under
	// the workdir still fails loudly, it just fails at resolution.
	if _, err := ResolveCheckout(map[string]string{"lecstor/ghost": ""}, "", wd, "lecstor/ghost"); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err = %v, want repo_not_found for an identity with no derived checkout", err)
	}
	// And it is the SCOPE's list too: the derived checkout is what the
	// permission policy is told to cover.
	gotDirs := DeclaredCheckouts(map[string]string{"lecstor/clankerbar-cli": ""}, "", wd)
	if len(gotDirs) != 1 || gotDirs[0] != filepath.Join(wd, "clankerbar-cli") {
		t.Fatalf("DeclaredCheckouts = %v, want the derived checkout", gotDirs)
	}
}

// The four matching steps of ResolveCheckout's documented order, and their
// precedence over one another.
func TestResolveCheckoutOrder(t *testing.T) {
	t.Run("explicit full owner/name key wins", func(t *testing.T) {
		wd := workdirWith(t, "cli")
		want := checkout(t, filepath.Join(t.TempDir(), "elsewhere"))
		got, err := ResolveCheckout(
			map[string]string{"lecstor/cli": want, "cli": wd + "/cli"},
			"", wd, "lecstor/cli")
		if err != nil {
			t.Fatalf("ResolveCheckout: %v", err)
		}
		if got != want {
			t.Errorf("got %s, want the full-key entry %s", got, want)
		}
	})
	t.Run("bare-name key beats the conventions", func(t *testing.T) {
		wd := workdirWith(t, "cli")
		want := checkout(t, filepath.Join(t.TempDir(), "bare"))
		got, err := ResolveCheckout(map[string]string{"cli": want}, "", wd, "other-owner/cli")
		if err != nil {
			t.Fatalf("ResolveCheckout: %v", err)
		}
		if got != want {
			t.Errorf("got %s, want the bare-name entry %s", got, want)
		}
	})
	t.Run("workdir itself is the checkout when its basename matches", func(t *testing.T) {
		parent := t.TempDir()
		self := checkout(t, filepath.Join(parent, "myrepo"))
		got, err := ResolveCheckout(nil, "", self, "acme/myrepo")
		if err != nil {
			t.Fatalf("ResolveCheckout: %v", err)
		}
		if got != self {
			t.Errorf("got %s, want the workdir itself %s", got, self)
		}
	})
	t.Run("workdir/<name> convention last", func(t *testing.T) {
		wd := workdirWith(t, "cli")
		got, err := ResolveCheckout(nil, "", wd, "acme/cli")
		if err != nil {
			t.Fatalf("ResolveCheckout: %v", err)
		}
		if got != filepath.Join(wd, "cli") {
			t.Errorf("got %s, want the convention hit", got)
		}
	})
}

func TestResolveCheckoutNotFound(t *testing.T) {
	wd := t.TempDir() // exists, but contains no checkouts
	_, err := ResolveCheckout(nil, "", wd, "acme/missing")
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err = %v, want ErrRepoNotFound", err)
	}
	if !strings.Contains(err.Error(), "repo_not_found") {
		t.Errorf("err = %v, want the literal repo_not_found code in the text", err)
	}
	// A declared-but-absent path fails the same way: a configured path that does
	// not exist must not spawn a session into it.
	_, err = ResolveCheckout(map[string]string{"acme/ghost": "/nonexistent/checkout/cla437"}, "", wd, "acme/ghost")
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("declared-but-missing path: err = %v, want ErrRepoNotFound", err)
	}
}

func TestResolveCheckoutEmptyIdentity(t *testing.T) {
	t.Run("nothing configured is no opinion", func(t *testing.T) {
		got, err := ResolveCheckout(nil, "", t.TempDir(), "")
		if err != nil || got != "" {
			t.Errorf("got (%q, %v), want (\"\", nil) - the legacy workdir behaviour must be untouched when nothing is declared", got, err)
		}
	})
	t.Run("sole declared repo is primary implicitly", func(t *testing.T) {
		wd := t.TempDir()
		want := checkout(t, filepath.Join(wd, "only"))
		got, err := ResolveCheckout(map[string]string{"acme/only": want}, "", wd, "")
		if err != nil {
			t.Fatalf("ResolveCheckout: %v", err)
		}
		if got != want {
			t.Errorf("got %s, want the sole entry %s", got, want)
		}
	})
	t.Run("explicit primary wins over the sole-entry default", func(t *testing.T) {
		wd := workdirWith(t, "a", "b")
		repos := map[string]string{"acme/a": wd + "/a", "acme/b": wd + "/b"}
		got, err := ResolveCheckout(repos, "acme/b", wd, "")
		if err != nil {
			t.Fatalf("ResolveCheckout: %v", err)
		}
		if got != wd+"/b" {
			t.Errorf("got %s, want the named primary", got)
		}
	})
	t.Run("ambiguous repos with no primary fails loudly", func(t *testing.T) {
		wd := t.TempDir()
		repos := map[string]string{"acme/a": "/x/a", "acme/b": "/x/b"}
		_, err := ResolveCheckout(repos, "", wd, "")
		if !errors.Is(err, ErrRepoNotFound) {
			t.Fatalf("err = %v, want ErrRepoNotFound - ambiguity is refused, never guessed", err)
		}
	})
}

func TestResolveCheckoutPathForms(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	wd := t.TempDir()
	t.Run("tilde expands", func(t *testing.T) {
		checkout(t, filepath.Join(home, "repo"))
		got, err := ResolveCheckout(map[string]string{"o/repo": "~/repo"}, "", wd, "o/repo")
		if err != nil {
			t.Fatalf("ResolveCheckout: %v", err)
		}
		if got != filepath.Join(home, "repo") {
			t.Errorf("got %s, want the ~-expanded path", got)
		}
	})
	t.Run("relative resolves against the workdir", func(t *testing.T) {
		checkout(t, filepath.Join(wd, "rel-repo"))
		got, err := ResolveCheckout(map[string]string{"o/r": "rel-repo"}, "", wd, "o/r")
		if err != nil {
			t.Fatalf("ResolveCheckout: %v", err)
		}
		if got != filepath.Join(wd, "rel-repo") {
			t.Errorf("got %s, want the workdir-relative path", got)
		}
	})
}

func TestDeclaredCheckouts(t *testing.T) {
	wd := t.TempDir()
	a := checkout(t, filepath.Join(wd, "a"))
	b := checkout(t, filepath.Join(wd, "b"))
	ghost := filepath.Join(wd, "ghost")
	repos := map[string]string{
		"acme/a":     a,
		"a":          a, // same checkout under two identities: deduplicated
		"acme/b":     b,
		"acme/ghost": ghost, // unresolvable: skipped here, reported by doctor
	}
	got := DeclaredCheckouts(repos, "acme/a", wd) // primary duplicates acme/a
	want := []string{a, b}                        // sorted, deduped, ghost dropped
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %s, want %s (full list %v)", i, got[i], want[i], want)
		}
	}
}

func TestValidateReposStructure(t *testing.T) {
	// Validate checks the whole config, so this needs the minimum legal shell
	// around the repo map under test.
	base := func(repos map[string]string) *Config {
		return &Config{Harness: "claude", Prompt: "Work the next backlog item.", Repos: repos}
	}
	// An empty VALUE is legal and means "the fleet derives this checkout"
	// (CLA-678) — see the acceptance below, and the resolution test that pins
	// what it resolves to.
	if err := base(map[string]string{" ": "/x"}).Validate(); err == nil || !strings.Contains(err.Error(), "empty name") {
		t.Errorf("err = %v, want an empty-name refusal", err)
	}
	// A well-formed map validates: resolution-time failures (missing checkouts)
	// are doctor's report and the iteration's repo_not_found, never a refused run.
	if err := base(map[string]string{"o/r": "/x", "a/b": "~/y"}).Validate(); err != nil {
		t.Errorf("err = %v, want a structurally valid map accepted", err)
	}
}

func TestReposForProjectOverride(t *testing.T) {
	top := map[string]string{"t/r": "/top"}
	proj := map[string]string{"p/r": "/proj"}
	cfg := &Config{
		Repos:       top,
		PrimaryRepo: "t/r",
		Projects: []Project{
			{Slug: "mine", Repos: proj, PrimaryRepo: "p/r"},
			{Slug: "inherit"},
		},
	}
	if got := cfg.ReposFor("mine"); len(got) != 1 || got["p/r"] != "/proj" {
		t.Errorf("ReposFor(mine) = %v, want the project's own map to replace the top-level one", got)
	}
	if got := cfg.PrimaryRepoFor("mine"); got != "p/r" {
		t.Errorf("PrimaryRepoFor(mine) = %q", got)
	}
	if got := cfg.ReposFor("inherit"); len(got) != 1 || got["t/r"] != "/top" {
		t.Errorf("ReposFor(inherit) = %v, want the top-level map", got)
	}
	if got := cfg.ReposFor(""); len(got) != 1 || got["t/r"] != "/top" {
		t.Errorf("ReposFor(\"\") = %v, want the top-level map for a single-project run", got)
	}
}

// CLA-608: the per-repo PR base resolves like the repos map itself - a
// matching project's NON-EMPTY map REPLACES the top-level one (no merge), and
// the lookup the driver makes is by the task's full identity first, then its
// bare name (the spelling local checkouts are named by, and the same fallback
// ResolveCheckout applies). Empty means "nothing overridden", never "no
// branch" - the driver goes to origin for the answer.
func TestPRBaseFor_ProjectReplaceAndBareNameMatch(t *testing.T) {
	cfg := &Config{
		PRBase: map[string]string{"top/one": "main", "two": "release"},
		Projects: []Project{
			{Slug: "mine", PRBase: map[string]string{"p/r": "integration"}},
			{Slug: "inherit"},
		},
	}
	if got := cfg.PRBaseFor("mine", "p/r"); got != "integration" {
		t.Errorf("PRBaseFor(mine, p/r) = %q, want the project's own entry", got)
	}
	// Replace, not merge: the project's map does not inherit the top-level one.
	if got := cfg.PRBaseFor("mine", "top/one"); got != "" {
		t.Errorf("PRBaseFor(mine, top/one) = %q, want empty - a project's map replaces the top-level one", got)
	}
	if got := cfg.PRBaseFor("inherit", "top/one"); got != "main" {
		t.Errorf("PRBaseFor(inherit, top/one) = %q, want the top-level identity entry", got)
	}
	// Full identity first, bare name second: "other/two" has no identity entry,
	// and the bare "two" entry is what an operator writing local-checkout
	// spellings gets.
	if got := cfg.PRBaseFor("", "other/two"); got != "release" {
		t.Errorf("PRBaseFor(\"\", other/two) = %q, want the bare-name fallback to match", got)
	}
	if got := cfg.PRBaseFor("", ""); got != "" {
		t.Errorf("PRBaseFor with no repo = %q, want empty - nothing to key a lookup by", got)
	}
	if got := cfg.PRBaseFor("", "nowhere/at-all"); got != "" {
		t.Errorf("PRBaseFor(nowhere/at-all) = %q, want empty - the driver resolves against origin instead", got)
	}
}
