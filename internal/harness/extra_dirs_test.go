package harness

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// CLA-437 tests: the permission policy handed to a session covers every repo its
// project declares, whatever directory the session starts in, and claude gets
// those directories by name on its argv.

func checkoutWithGit(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	return path
}

func rules(p map[string]string, tool string) []string {
	return strings.Fields(p[tool])
}

func has(rules []string, want string) bool {
	for _, r := range rules {
		if r == want {
			return true
		}
	}
	return false
}

// hasAbsAllow reports whether one of rules allows "<dir>/**" in the absolute
// form external_directory asks with, matched by suffix because filepath.Abs
// inside the policy can differ from the caller's spelling on macOS (/var vs
// /private/var).
func hasAbsAllow(rules []string, dir string) bool {
	suffix := filepath.ToSlash(dir) + "/**:allow"
	for _, r := range rules {
		if strings.HasSuffix(r, suffix) {
			return true
		}
	}
	return false
}

// The headline case from the doneWhen: a TWO-REPO project's policy covers both
// paths. The session starts inside repo A's checkout, so repo B is reached
// through the instance-relative "../repo-b/**" form 2.x asks with, and through
// the absolute external_directory allow.
func TestOpencodeTwoRepoPolicyCoversBothPaths(t *testing.T) {
	parent := t.TempDir()
	repoA := checkoutWithGit(t, filepath.Join(parent, "repo-a"))
	repoB := checkoutWithGit(t, filepath.Join(parent, "repo-b"))

	p := parsePolicy(t, opencodePermission(false, repoA, []string{repoB}))

	read, edit := rules(p, "read"), rules(p, "edit")
	if !has(read, "**:allow") || !has(edit, "**:allow") {
		t.Errorf("read=%v edit=%v; plain-relative asks need **:allow", read, edit)
	}
	if !has(read, "../**:deny") || !has(edit, "../**:deny") {
		t.Errorf("read=%v edit=%v; undeclared escapes must fail closed via ../**:deny", read, edit)
	}
	if !has(read, "../repo-b/**:allow") || !has(edit, "../repo-b/**:allow") {
		t.Errorf("read=%v edit=%v; the declared sibling's ../-form allow is missing", read, edit)
	}
	// external_directory gates every path-taking tool before its ask: BOTH repos
	// allowed absolutely, the catch-all deny kept for everything else.
	ext := rules(p, "external_directory")
	if !has(ext, "*:deny") {
		t.Errorf("external_directory = %v; *:deny must survive so undeclared trees stay gated", ext)
	}
	if !hasAbsAllow(ext, repoA) || !hasAbsAllow(ext, repoB) {
		t.Errorf("external_directory = %v; want absolute allows for BOTH repos regardless of cwd", ext)
	}
	// Last-match-wins ordering is load-bearing: within `read`, the blanket
	// "../**" deny must sort BEFORE the specific "../repo-b/**" allow, or the
	// deny wins and the feature is dead on arrival. Go marshals map keys sorted,
	// so asserting position in the folded list asserts wire order.
	denyAt, allowAt := -1, -1
	for i, r := range read {
		switch r {
		case "../**:deny":
			denyAt = i
		case "../repo-b/**:allow":
			allowAt = i
		}
	}
	if denyAt < 0 || allowAt < 0 || denyAt > allowAt {
		t.Errorf("read order = %v; the ../** deny must sort before each declared ../<dir>/** allow", read)
	}

	// The base shape survives: catch-all denies everything else, exfil denied.
	if p["*"] != "deny" {
		t.Errorf("* = %q, want deny", p["*"])
	}
	assertNetworkDenied(t, p)
}

// A session spawned OUTSIDE any git repo gets the same shape: on 2.x the
// instance directory, not git, decides it. With no extras, nothing names any
// other tree.
func TestOpencodePolicyExtraDirsOutsideACheckout(t *testing.T) {
	parent := t.TempDir()
	wd := filepath.Join(parent, "work")
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	extra := checkoutWithGit(t, filepath.Join(parent, "sibling-repo"))

	with := parsePolicy(t, opencodePermission(false, wd, []string{extra}))
	if !has(rules(with, "read"), "../sibling-repo/**:allow") {
		t.Errorf("declared sibling missing from read: %v", with["read"])
	}
	if !hasAbsAllow(rules(with, "external_directory"), extra) {
		t.Errorf("declared sibling missing from external_directory: %v", with["external_directory"])
	}

	base := parsePolicy(t, opencodePermission(false, "/Users/jason/dev", nil))
	if got := base["read"]; got != "**:allow ../**:deny mcp:*:allow" {
		t.Errorf("no-extras read = %q", got)
	}
}

// claude receives the declared repos by name on its argv: one --add-dir flag,
// then each directory as its OWN argv element. The flag is variadic over
// separate elements, so the earlier joined-string emission parsed two extras as
// ONE path containing spaces and granted nothing (CLA-443). The assertions are
// element-wise for exactly that reason: a contains-check over
// strings.Join(args, " ") passes under both emissions and cannot catch the
// difference. Probes build their own argv and never carry them.
func TestClaudeArgsExtraDirs(t *testing.T) {
	dirs := []string{"/repos/a", "/repos/b", "/repos/c"}
	args := claudeArgs(Invocation{
		Prompt:       "Work.",
		SettingsPath: "/cfg/settings.json",
		ExtraDirs:    dirs,
	})
	if n := count(args, "--add-dir"); n != 1 {
		t.Errorf("--add-dir appears %d times in %v, want exactly once", n, args)
	}
	at := slices.Index(args, "--add-dir")
	if at < 0 {
		t.Fatalf("--add-dir missing from %v", args)
	}
	// Nothing follows the dirs in this invocation, so everything after the flag
	// must be exactly the dirs, one element each - the shape a joined string
	// fails, and the shape claude's variadic parse needs.
	if rest := args[at+1:]; !slices.Equal(rest, dirs) {
		t.Errorf("argv after --add-dir = %q, want each declared checkout as its own element %v", rest, dirs)
	}
	if got := claudeArgs(Invocation{Prompt: "Work."}); has(got, "--add-dir") {
		t.Errorf("no-extra invocation emitted --add-dir: %v", got)
	}
	probe := codexArgs(Invocation{Probe: true, ExtraDirs: []string{"/repos/a"}})
	if has(probe, "--add-dir") || has(probe, "/repos/a") {
		t.Errorf("probe argv leaked ExtraDirs: %v", probe)
	}
}

func count(args []string, want string) int {
	n := 0
	for _, a := range args {
		if a == want {
			n++
		}
	}
	return n
}
