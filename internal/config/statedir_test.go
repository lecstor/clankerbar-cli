package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resolve(t *testing.T, c *Config) string {
	t.Helper()
	got, err := c.ResolveStateDir()
	if err != nil {
		t.Fatalf("ResolveStateDir: %v", err)
	}
	return got
}

// The whole point of CLA-259: the daemon's own writes no longer land inside the
// one tree its spawned sessions are permitted to write.
func TestDefaultStateDirIsOutsideTheWorkDir(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	work := t.TempDir()

	got := resolve(t, &Config{WorkDir: work})

	if strings.HasPrefix(got, work+string(filepath.Separator)) || got == work {
		t.Fatalf("state dir %s is inside the workdir %s", got, work)
	}
	if want := filepath.Join(state, "clankerbar", "loop"); !strings.HasPrefix(got, want+string(filepath.Separator)) {
		t.Errorf("state dir %s is not under %s", got, want)
	}
}

// StateRoot is the parent of the per-workdir state dirs: the `loop` root the
// retrospective dead-phase scan walks, one level above ResolveStateDir's answer.
func TestStateRootIsTheLoopParentOfTheStateDirs(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	work := t.TempDir()

	root, err := StateRoot()
	if err != nil {
		t.Fatalf("StateRoot: %v", err)
	}
	want := filepath.Join(state, "clankerbar", "loop")
	if root != want {
		t.Errorf("StateRoot = %s, want %s", root, want)
	}
	// And the per-workdir state dir sits directly under it.
	if got := resolve(t, &Config{WorkDir: work}); filepath.Dir(got) != root {
		t.Errorf("ResolveStateDir = %s, want it directly under StateRoot %s", got, root)
	}
}

// The old location is not merely unused — a leftover one is reported so the
// operator learns their markers there are dead, and is never read.
func TestLegacyStateDirIsReportedOnlyWhenItExists(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	work := t.TempDir()
	c := &Config{WorkDir: work}

	if got := c.LegacyStateDir(); got != "" {
		t.Errorf("no leftover dir: got %q, want empty", got)
	}
	legacy := filepath.Join(work, ".clankerbar-loop")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := c.LegacyStateDir(); got != legacy {
		t.Errorf("leftover dir: got %q, want %q", got, legacy)
	}
}

// An operator who explicitly points state_dir back at the old path is not told
// their own choice is a leftover.
func TestLegacyStateDirIsSilentWhenItIsTheConfiguredOne(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	work := t.TempDir()
	legacy := filepath.Join(work, ".clankerbar-loop")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	c := &Config{WorkDir: work, StateDir: legacy}

	if got := c.LegacyStateDir(); got != "" {
		t.Errorf("explicitly configured old path: got %q, want empty", got)
	}
}

// Two checkouts sharing a basename (`~/dev/clankerbar` and a worktree called
// `clankerbar` elsewhere) must not share a STOP marker or interleave transcripts.
func TestStateDirIsPerWorkDirNotPerBasename(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	parent := t.TempDir()
	a := filepath.Join(parent, "one", "clankerbar")
	b := filepath.Join(parent, "two", "clankerbar")

	da, db := resolve(t, &Config{WorkDir: a}), resolve(t, &Config{WorkDir: b})
	if da == db {
		t.Fatalf("two workdirs share a state dir: %s", da)
	}
	// ...but the basename is still visible, so an operator hunting a transcript
	// recognises the directory.
	if !strings.Contains(filepath.Base(da), "clankerbar") {
		t.Errorf("state dir %s does not name its workdir", da)
	}
}

// Same workdir, same answer, run after run — the STOP marker's location cannot
// wander.
func TestStateDirIsStableForOneWorkDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	work := t.TempDir()
	if a, b := resolve(t, &Config{WorkDir: work}), resolve(t, &Config{WorkDir: work}); a != b {
		t.Errorf("unstable: %s then %s", a, b)
	}
}

func TestExplicitStateDirWinsAndIsAbsolute(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	explicit := t.TempDir()
	if got := resolve(t, &Config{WorkDir: t.TempDir(), StateDir: explicit}); got != explicit {
		t.Errorf("explicit state_dir: got %s, want %s", got, explicit)
	}

	got := resolve(t, &Config{StateDir: "relative-state"})
	if !filepath.IsAbs(got) {
		t.Errorf("a relative state_dir must be reported absolute, got %s", got)
	}
}

// XDG_STATE_HOME must be absolute per the spec. A relative one would resolve
// against whatever cwd the daemon was started in — the ambiguity underWorkDir
// exists to stamp out — so it is ignored rather than honoured.
//
// This test deliberately defeats the binary's teststate.Isolate isolation: a
// relative XDG_STATE_HOME is ignored, so for its duration the derivation
// points back at the operator's real ~/.local/state. That is safe ONLY while
// the derived path is never opened or created here - the assertion compares
// strings. If this test ever needs to touch the filesystem at the derived
// path, it must first set an absolute XDG_STATE_HOME of its own.
func TestRelativeXDGStateHomeIsIgnored(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative/state")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this machine")
	}
	got := resolve(t, &Config{WorkDir: t.TempDir()})
	if want := filepath.Join(home, ".local", "state"); !strings.HasPrefix(got, want+string(filepath.Separator)) {
		t.Errorf("state dir %s did not fall back to %s", got, want)
	}
}

// A workdir named by somebody else must not be able to steer the state dir
// anywhere: the slug is one path component, whatever the basename contains.
func TestStateSlugIsAlwaysOnePathComponent(t *testing.T) {
	for _, base := range []string{"..", ".", "", "..%2f..", "a/b", "  ", "-", "...", strings.Repeat("x", 200)} {
		got := sanitizeSlug(base)
		if got == "" || got == "." || got == ".." ||
			strings.ContainsRune(got, filepath.Separator) || strings.ContainsRune(got, '/') {
			t.Errorf("sanitizeSlug(%q) = %q, which is not a safe path component", base, got)
		}
		if len(got) > 40 {
			t.Errorf("sanitizeSlug(%q) = %q, longer than the 40-char cap", base, got)
		}
	}
}

// The state dir is pinned when the config is VALIDATED, not recomputed at each
// point of use. It used to be derived from `filepath.Abs(WorkDir)` on demand, so
// one config described a different run depending on where the process stood:
// `clankerbar run` and `clankerbar doctor` could hash to two different state
// dirs, and doctor would report "no stop markers" about a directory the running
// loop had never opened. Before CLA-259 that cwd-dependence was at least visible
// as `./.clankerbar-loop`; a hashed name under ~/.local/state hides it.
func TestStateDirIsPinnedAtValidateNotAtUse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	work := t.TempDir()
	t.Chdir(filepath.Dir(work))

	c := defaults()
	c.WorkDir = filepath.Base(work) // relative, as a hand-written config may well be
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !filepath.IsAbs(c.WorkDir) {
		t.Errorf("workdir after Validate = %q, want an absolute path", c.WorkDir)
	}

	before := resolve(t, c)
	t.Chdir(t.TempDir())
	after := resolve(t, c)

	if before != after {
		t.Errorf("state dir moved with the process cwd:\n  before chdir: %s\n  after chdir:  %s", before, after)
	}
}

// An unset workdir still means "where this process was started" - it is the one
// thing about a validated config that two invocations can legitimately disagree
// on, so doctor is told to say so rather than print a hash and leave it there.
func TestWorkDirIsImplicitOnlyWhenItWasNotConfigured(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	c := defaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !c.WorkDirIsImplicit() {
		t.Error("an unset workdir should be reported as implicit")
	}

	c2 := defaults()
	c2.WorkDir = t.TempDir()
	if err := c2.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if c2.WorkDirIsImplicit() {
		t.Error("a configured workdir should not be reported as implicit")
	}
}

// The workdir a session is spawned in is the boundary statedir.Open is handed to
// tell a planted symlink from the operator's own filesystem layout, so it has to
// be absolute and it has to cover every project.
func TestSessionWorkDirsAreAbsoluteAndCoverEveryProject(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	top := t.TempDir()
	other := t.TempDir()

	c := defaults()
	c.WorkDir = top
	c.Projects = []Project{
		{Slug: "alpha"},
		{Slug: "beta", WorkDir: other},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	got := c.SessionWorkDirs()
	want := []string{top, other}
	if len(got) != len(want) {
		t.Fatalf("SessionWorkDirs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SessionWorkDirs()[%d] = %q, want %q", i, got[i], want[i])
		}
		if !filepath.IsAbs(got[i]) {
			t.Errorf("SessionWorkDirs()[%d] = %q, want an absolute path", i, got[i])
		}
	}

	// No projects: the top-level workdir is the only place sessions run.
	c2 := defaults()
	c2.WorkDir = top
	if err := c2.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := c2.SessionWorkDirs(); len(got) != 1 || got[0] != top {
		t.Errorf("SessionWorkDirs() with no projects = %v, want [%s]", got, top)
	}
}
