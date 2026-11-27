package mcpbridge

// The target resolver is the security boundary the bridge adds: a checkout may
// say WHICH project it is (a slug), and never where the credential goes. These
// tests pin the resolution ladder and the upward walk's stopping rules.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testOrigin = "https://clankerbar.com"

func writeTreeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func resolveForTest(t *testing.T, o TargetOptions) (string, string) {
	t.Helper()
	var warn bytes.Buffer
	o.Warn = &warn
	got, err := ResolveTarget(o)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	return got, warn.String()
}

func TestResolveTargetExplicitFlags(t *testing.T) {
	cases := []struct {
		name    string
		project string
		multi   bool
		want    string
	}{
		{name: "multi", multi: true, want: testOrigin + "/mcp"},
		{name: "project", project: "clankerbar-cli", want: testOrigin + "/mcp/clankerbar-cli"},
		{name: "project escaped", project: "a/b c", want: testOrigin + "/mcp/a%2Fb%20c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := resolveForTest(t, TargetOptions{
				Origin: testOrigin, Project: tc.project, Multi: tc.multi,
				CWD: t.TempDir(), Home: t.TempDir(),
			})
			if got != tc.want {
				t.Errorf("target = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveTargetRefusesMultiAndProject(t *testing.T) {
	_, err := ResolveTarget(TargetOptions{Origin: testOrigin, Project: "x", Multi: true})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v, want a mutually-exclusive refusal", err)
	}
}

func TestResolveTargetSingleProjectFile(t *testing.T) {
	root := t.TempDir()
	writeTreeFile(t, filepath.Join(root, projectFileName), `{"project":"clankerbar-cli"}`)
	got, warn := resolveForTest(t, TargetOptions{Origin: testOrigin, CWD: root, Home: t.TempDir()})
	if got != testOrigin+"/mcp/clankerbar-cli" {
		t.Errorf("target = %q", got)
	}
	if warn != "" {
		t.Errorf("single-project file produced a warning: %s", warn)
	}
}

func TestResolveTargetWalksUpToProject(t *testing.T) {
	root := t.TempDir()
	writeTreeFile(t, filepath.Join(root, projectFileName), `{"project":"alpha"}`)
	cwd := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	got, _ := resolveForTest(t, TargetOptions{Origin: testOrigin, CWD: cwd, Home: t.TempDir()})
	if got != testOrigin+"/mcp/alpha" {
		t.Errorf("target = %q, want the parent project", got)
	}
}

func TestResolveTargetFoldersLongestPrefix(t *testing.T) {
	root := t.TempDir()
	writeTreeFile(t, filepath.Join(root, projectFileName), `{"folders":{
		"clankerbar": "clankerbar-main",
		"clankerbar-cli": "clankerbar",
		"clankerbar-cli/sub": "clankerbar-sub"
	}}`)

	t.Run("longest wins", func(t *testing.T) {
		cwd := filepath.Join(root, "clankerbar-cli", "sub", "deep")
		if err := os.MkdirAll(cwd, 0o700); err != nil {
			t.Fatal(err)
		}
		got, _ := resolveForTest(t, TargetOptions{Origin: testOrigin, CWD: cwd, Home: t.TempDir()})
		if got != testOrigin+"/mcp/clankerbar-sub" {
			t.Errorf("target = %q, want the longest matching folder", got)
		}
	})

	t.Run("own folder", func(t *testing.T) {
		cwd := filepath.Join(root, "clankerbar-cli")
		if err := os.MkdirAll(cwd, 0o700); err != nil {
			t.Fatal(err)
		}
		got, _ := resolveForTest(t, TargetOptions{Origin: testOrigin, CWD: cwd, Home: t.TempDir()})
		if got != testOrigin+"/mcp/clankerbar" {
			t.Errorf("target = %q", got)
		}
	})

	t.Run("sibling is not a path prefix", func(t *testing.T) {
		// "clankerbar" is a string prefix of "clankerbar-cli", but not a path
		// prefix of it in either direction: a lookalike sibling matches nothing.
		cwd := filepath.Join(root, "clankerbar-cli2")
		if err := os.MkdirAll(cwd, 0o700); err != nil {
			t.Fatal(err)
		}
		got, _ := resolveForTest(t, TargetOptions{Origin: testOrigin, CWD: cwd, Home: t.TempDir()})
		if got != testOrigin+"/mcp" {
			t.Errorf("target = %q: a string-prefix sibling matched", got)
		}
	})
}

func TestResolveTargetProjectBeforeFolders(t *testing.T) {
	root := t.TempDir()
	writeTreeFile(t, filepath.Join(root, projectFileName), `{"project":"one","folders":{".":"two"}}`)
	got, _ := resolveForTest(t, TargetOptions{Origin: testOrigin, CWD: root, Home: t.TempDir()})
	if got != testOrigin+"/mcp/one" {
		t.Errorf("target = %q, want the file's own project", got)
	}
}

func TestResolveTargetIgnoresMCPJSON(t *testing.T) {
	root := t.TempDir()
	// A checkout does not get to name where the credential goes: only a project
	// slug from .clankerbar.json is read, never an MCP URL from .mcp.json.
	writeTreeFile(t, filepath.Join(root, ".mcp.json"),
		`{"mcpServers":{"clankerbar":{"url":"https://evil.example/mcp/other"}}}`)
	got, _ := resolveForTest(t, TargetOptions{Origin: testOrigin, CWD: root, Home: t.TempDir()})
	if got != testOrigin+"/mcp" {
		t.Errorf("target = %q, want the bare trusted origin", got)
	}
}

func TestResolveTargetMalformedFileWarnsOnceAndFallsBack(t *testing.T) {
	cases := []struct{ name, content string }{
		{name: "not JSON", content: `{"project":`},
		{name: "typo key", content: `{"projet":"typo"}`},
		{name: "null", content: `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeTreeFile(t, filepath.Join(root, projectFileName), tc.content)
			got, warn := resolveForTest(t, TargetOptions{Origin: testOrigin, CWD: root, Home: t.TempDir()})
			if got != testOrigin+"/mcp" {
				t.Errorf("target = %q, want the bare surface", got)
			}
			if strings.Count(warn, projectFileName) != 1 {
				t.Errorf("want exactly one warning naming %s, got:\n%s", projectFileName, warn)
			}
		})
	}
}

func TestResolveTargetEmptyFolderSlugIsMalformed(t *testing.T) {
	root := t.TempDir()
	writeTreeFile(t, filepath.Join(root, projectFileName), `{"folders":{"repo":"  "}}`)
	cwd := filepath.Join(root, "repo")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	got, warn := resolveForTest(t, TargetOptions{Origin: testOrigin, CWD: cwd, Home: t.TempDir()})
	if got != testOrigin+"/mcp" {
		t.Errorf("target = %q, want the bare surface", got)
	}
	if !strings.Contains(warn, "names no project") {
		t.Errorf("warning does not explain the empty slug:\n%s", warn)
	}
}

func TestResolveTargetStopsAtGitRoot(t *testing.T) {
	root := t.TempDir()
	writeTreeFile(t, filepath.Join(root, projectFileName), `{"project":"outside-the-repo"}`)
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(repo, "sub")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	got, _ := resolveForTest(t, TargetOptions{Origin: testOrigin, CWD: cwd, Home: t.TempDir()})
	if got != testOrigin+"/mcp" {
		t.Errorf("target = %q: the walk read above the checkout root", got)
	}
}

func TestResolveTargetReadsProjectAtGitRoot(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTreeFile(t, filepath.Join(repo, projectFileName), `{"project":"in-repo"}`)
	cwd := filepath.Join(repo, "sub")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	got, _ := resolveForTest(t, TargetOptions{Origin: testOrigin, CWD: cwd, Home: t.TempDir()})
	if got != testOrigin+"/mcp/in-repo" {
		t.Errorf("target = %q, want the repo's own mapping", got)
	}
}

func TestResolveTargetStopsAtHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	writeTreeFile(t, filepath.Join(home, projectFileName), `{"folders":{"other":"x"}}`)
	writeTreeFile(t, filepath.Join(root, projectFileName), `{"project":"above-home"}`)
	cwd := filepath.Join(home, "work")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	got, _ := resolveForTest(t, TargetOptions{Origin: testOrigin, CWD: cwd, Home: home})
	if got != testOrigin+"/mcp" {
		t.Errorf("target = %q: the walk continued above home", got)
	}
}

func TestResolveOriginExplicitHost(t *testing.T) {
	got, err := ResolveOrigin("https://plane.example")
	if err != nil || got != "https://plane.example" {
		t.Fatalf("ResolveOrigin = %q, %v", got, err)
	}
	loopback, err := ResolveOrigin("http://127.0.0.1:8080")
	if err != nil || loopback != "http://127.0.0.1:8080" {
		t.Fatalf("loopback ResolveOrigin = %q, %v", loopback, err)
	}
	if _, err := ResolveOrigin("http://plane.example"); err == nil {
		t.Error("plain http to a non-loopback host was accepted")
	}
}

func TestResolveOriginFromHomeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".config", "clankerbar")
	writeTreeFile(t, filepath.Join(cfgDir, "config.json"), `{"backlog_url":"https://plane.example"}`)
	got, err := ResolveOrigin("")
	if err != nil || got != "https://plane.example" {
		t.Fatalf("ResolveOrigin = %q, %v", got, err)
	}
}

func TestResolveOriginDefaultsWithNoConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := ResolveOrigin("")
	if err != nil || got != DefaultOrigin {
		t.Fatalf("ResolveOrigin = %q, %v; want %q", got, err, DefaultOrigin)
	}
}

func TestResolveOriginRefusesInsecureConfiguredBacklog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTreeFile(t, filepath.Join(home, ".config", "clankerbar", "config.json"),
		`{"backlog_url":"http://plane.example"}`)
	if _, err := ResolveOrigin(""); err == nil || !strings.Contains(err.Error(), "--host") {
		t.Fatalf("err = %v, want a refusal naming --host", err)
	}
}
