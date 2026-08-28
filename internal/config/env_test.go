package config

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// --- parsing -----------------------------------------------------------------

// The env VALUE grammar: a literal string, or an object carrying fromCommand.
// Anything else is refused at parse time, because a silently-dropped
// declaration is exactly the session-missing-its-env failure CLA-462 exists to
// end.
func TestEnvValueUnmarshal(t *testing.T) {
	t.Run("literal string", func(t *testing.T) {
		var v EnvValue
		if err := json.Unmarshal([]byte(`"plain"`), &v); err != nil {
			t.Fatal(err)
		}
		if v.Literal != "plain" || v.IsCommand() {
			t.Fatalf("got %+v", v)
		}
	})
	t.Run("fromCommand object", func(t *testing.T) {
		var v EnvValue
		if err := json.Unmarshal([]byte(`{"fromCommand":"gh auth token"}`), &v); err != nil {
			t.Fatal(err)
		}
		if !v.IsCommand() || v.FromCommand != "gh auth token" {
			t.Fatalf("got %+v", v)
		}
	})
	t.Run("number is refused", func(t *testing.T) {
		var v EnvValue
		err := json.Unmarshal([]byte(`42`), &v)
		if err == nil || !strings.Contains(err.Error(), "not a string") {
			t.Fatalf("want a refusal naming the shape, got %v", err)
		}
	})
	t.Run("array is refused", func(t *testing.T) {
		var v EnvValue
		if err := json.Unmarshal([]byte(`["x"]`), &v); err == nil {
			t.Fatal("want a refusal for an array value")
		}
	})
	t.Run("object without fromCommand is refused", func(t *testing.T) {
		var v EnvValue
		err := json.Unmarshal([]byte(`{"other":1}`), &v)
		if err == nil || !strings.Contains(err.Error(), "fromCommand") {
			t.Fatalf("want a refusal naming the missing key, got %v", err)
		}
	})
	t.Run("round-trips through Marshal", func(t *testing.T) {
		for _, in := range []string{`"lit"`, `{"fromCommand":"cmd"}`} {
			var v EnvValue
			if err := json.Unmarshal([]byte(in), &v); err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			var back EnvValue
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatal(err)
			}
			if back != v {
				t.Fatalf("round-trip of %s gave %+v, want %+v", in, back, v)
			}
		}
	})
}

// A full-file decode: the four levels parse, and a non-string VALUE anywhere in
// them refuses the whole load.
func TestConfigJSONDecodesEnvAtFourLevels(t *testing.T) {
	good := `{
		"env": {"TOP": "t"},
		"harnesses": {"opencode": {"env": {"HARNESS": "h"}}},
		"projects": [{"slug": "p", "workdir": "/tmp/x",
			"env": {"PROJ": "pr"},
			"env_per_harness": {"claude": {"PAIR": "pair"}}}]
	}`
	var c Config
	if err := json.Unmarshal([]byte(good), &c); err != nil {
		t.Fatalf("four-level env refused: %v", err)
	}
	if c.Env["TOP"].Literal != "t" || c.Harnesses["opencode"].Env["HARNESS"].Literal != "h" ||
		c.Projects[0].Env["PROJ"].Literal != "pr" ||
		c.Projects[0].EnvPerHarness["claude"]["PAIR"].Literal != "pair" {
		t.Fatalf("decoded %+v / %+v / %+v / %+v", c.Env, c.Harnesses["opencode"].Env, c.Projects[0].Env, c.Projects[0].EnvPerHarness)
	}

	bad := `{"env": {"FOO": 42}}`
	var c2 Config
	err := json.Unmarshal([]byte(bad), &c2)
	if err == nil {
		t.Fatal("a number-valued env entry was accepted")
	}
}

// --- validation --------------------------------------------------------------

func TestValidateEnvDeclsRefusesBadNamesAndEmptyCommands(t *testing.T) {
	cases := []struct {
		name    string
		env     EnvMap
		wantErr string // substring; "" = must pass
	}{
		{"good names pass", EnvMap{"GH_TOKEN": {Literal: "x"}, "_X9": {Literal: "y"}}, ""},
		{"numeric-looking name refused", EnvMap{"123": {Literal: "x"}}, `not a valid environment-variable name`},
		{"dashed name refused", EnvMap{"A-B": {Literal: "x"}}, "not a valid environment-variable name"},
		{"empty name refused", EnvMap{"": {Literal: "x"}}, "not a valid environment-variable name"},
		{"name with equals refused", EnvMap{"A=B": {Literal: "x"}}, "not a valid environment-variable name"},
		{"empty command refused", EnvMap{"TOK": {FromCommand: "", isCommand: true}}, `"fromCommand" must not be empty`},
		{"blank command refused", EnvMap{"TOK": {FromCommand: "   ", isCommand: true}}, `"fromCommand" must not be empty`},
		{"good command passes", EnvMap{"TOK": {FromCommand: "gh auth token", isCommand: true}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateEnvDecls(tc.env, "env")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want acceptance, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// Validate walks all FOUR levels and labels where it found the problem.
func TestValidateWalksEveryEnvLevel(t *testing.T) {
	mk := func(mutate func(c *Config)) *Config {
		c := defaults()
		c.WorkDir = t.TempDir()
		mutate(c)
		return c
	}
	cases := []struct {
		name    string
		build   func(*Config)
		wantErr string
	}{
		{"top level", func(c *Config) { c.Env = EnvMap{"bad name": {Literal: "x"}} }, `env: "bad name"`},
		{"harness block", func(c *Config) {
			c.Harnesses = map[string]HarnessConfig{"opencode": {Env: EnvMap{"TOK": {FromCommand: "", isCommand: true}}}}
		}, `harnesses.opencode.env.TOK`},
		{"project block", func(c *Config) {
			c.Projects = []Project{{Slug: "p", WorkDir: t.TempDir(), Env: EnvMap{"9X": {Literal: "v"}}}}
		}, `projects[0].env: "9X"`},
		{"project per harness", func(c *Config) {
			c.Projects = []Project{{Slug: "p", WorkDir: t.TempDir(),
				EnvPerHarness: map[string]EnvMap{"claude": {"TOK": {FromCommand: "", isCommand: true}}}}}
		}, `projects[0].env_per_harness.claude.TOK`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mk(tc.build).Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// --- resolution at spawn ------------------------------------------------------

// The overlay order mirrors ResolveMCPConfig: top level first, then the harness
// block, then the project's, then that project's per-harness map — each key's
// most specific declaration winning.
func TestSessionEnvFourLevelOverlay(t *testing.T) {
	c := defaults()
	c.Harness = "claude"
	c.Env = EnvMap{
		"ONLY_TOP":   {Literal: "top"},
		"SHARED_TOP": {Literal: "from-top"},
		"BOTH":       {Literal: "top-wins-over-nothing"},
	}
	c.Harnesses = map[string]HarnessConfig{"claude": {Env: EnvMap{
		"SHARED_TOP":   {Literal: "harness-beats-top"},
		"ONLY_HARNESS": {Literal: "harness"},
	}}}
	c.Projects = []Project{{
		Slug:    "p",
		WorkDir: t.TempDir(),
		Env: EnvMap{
			"BOTH":         {Literal: "project-beats-harness-and-top"},
			"ONLY_PROJECT": {Literal: "proj"},
		},
		EnvPerHarness: map[string]EnvMap{
			"claude": {"BOTH": {Literal: "pair-wins"}},
			"codex":  {"OTHER_PAIR": {Literal: "not-this-session"}},
		},
	}}

	got, err := c.SessionEnv("claude", "p")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"BOTH=pair-wins",
		"ONLY_HARNESS=harness",
		"ONLY_PROJECT=proj",
		"ONLY_TOP=top",
		"SHARED_TOP=harness-beats-top",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overlay:\n got %v\nwant %v", got, want)
	}

	// An unmatched slug (the single-project case) gets top + harness layers only,
	// and never another project's values.
	got, err = c.SessionEnv("claude", "")
	if err != nil {
		t.Fatal(err)
	}
	want = []string{
		"BOTH=top-wins-over-nothing",
		"ONLY_HARNESS=harness",
		"ONLY_TOP=top",
		"SHARED_TOP=harness-beats-top",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unmatched slug:\n got %v\nwant %v", got, want)
	}
}

// fromCommand is resolved FRESH at every session spawn — that is its reason to
// exist. A counter stub proves two resolutions are two executions, and rotating
// output proves the second session sees the new value without any reload.
func TestSessionEnvResolvesFromCommandFreshEachSpawn(t *testing.T) {
	calls := 0
	prev := RunEnvCommand
	t.Cleanup(func() { RunEnvCommand = prev })
	RunEnvCommand = func(string) (string, error) {
		calls++
		return "token-" + string(rune('a'+calls-1)), nil
	}

	c := defaults()
	c.Env = EnvMap{"GH_TOKEN": {FromCommand: "counter", isCommand: true}}

	first, err := c.SessionEnv(c.Harness, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.SessionEnv(c.Harness, "")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("command ran %d times, want once per session (2)", calls)
	}
	if first[0] != "GH_TOKEN=token-a" || second[0] != "GH_TOKEN=token-b" {
		t.Fatalf("freshness broken: %v then %v", first, second)
	}
}

// Real exec path: stdout is trimmed and delivered as KEY=VALUE.
func TestRunEnvCommandExecutesAndTrims(t *testing.T) {
	got, err := RunEnvCommand(`echo "  spacy value "`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "spacy value" {
		t.Fatalf("got %q", got)
	}
}

// The default runner bounds the command: a hung token print must not hang every
// spawn behind it.
func TestRunEnvCommandTimesOut(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep binary")
	}
	old := envCommandTimeout
	envCommandTimeout = 50 * 1e6 // 50ms, in time.Duration units via untyped const
	defer func() { envCommandTimeout = old }()
	_, err := RunEnvCommand("sleep 5")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want a timeout refusal, got %v", err)
	}
}

// A failing command REFUSES the resolution with an error that NAMES THE
// VARIABLE - the log line an operator diagnoses from at 3am.
func TestSessionEnvRefusesFailingCommandNamingTheVariable(t *testing.T) {
	prev := RunEnvCommand
	t.Cleanup(func() { RunEnvCommand = prev })
	RunEnvCommand = func(string) (string, error) {
		return "", errors.New("exit 1: account not logged in")
	}

	c := defaults()
	c.Env = EnvMap{"GH_TOKEN": {FromCommand: "gh auth token -u nobody", isCommand: true}}
	_, err := c.SessionEnv(c.Harness, "")
	if err == nil {
		t.Fatal("want a refusal, got an env slice")
	}
	for _, want := range []string{"env GH_TOKEN", "gh auth token -u nobody", "account not logged in"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q: %v", want, err)
		}
	}
}

// Empty output from a successful command is refused too: an empty GH_TOKEN
// moves the failure into git/gh deep inside a session instead of stopping it at
// spawn with the variable named.
func TestSessionEnvRefusesEmptyCommandOutput(t *testing.T) {
	prev := RunEnvCommand
	t.Cleanup(func() { RunEnvCommand = prev })
	RunEnvCommand = func(string) (string, error) { return "  \n", nil }

	c := defaults()
	c.Env = EnvMap{"GH_TOKEN": {FromCommand: "true", isCommand: true}}
	_, err := c.SessionEnv(c.Harness, "")
	if err == nil || !strings.Contains(err.Error(), "env GH_TOKEN") || !strings.Contains(err.Error(), "no output") {
		t.Fatalf("want an empty-output refusal naming the variable, got %v", err)
	}
}

// A failing command in ANY layer refuses the whole session env, and literals in
// the other layers do not leak past the failure (never a silent partial env).
func TestSessionEnvRefusalCoversEveryLayer(t *testing.T) {
	prev := RunEnvCommand
	t.Cleanup(func() { RunEnvCommand = prev })
	RunEnvCommand = func(string) (string, error) { return "", errors.New("boom") }

	c := defaults()
	c.Projects = []Project{{
		Slug:    "p",
		WorkDir: t.TempDir(),
		Env:     EnvMap{"GOOD": {Literal: "fine"}},
		EnvPerHarness: map[string]EnvMap{
			"claude": {"BAD": {FromCommand: "failing", isCommand: true}},
		},
	}}
	if _, err := c.SessionEnv("claude", "p"); err == nil {
		t.Fatal("want a refusal when the most specific layer fails")
	}
	if _, err := c.SessionEnv("codex", "p"); err != nil {
		t.Fatalf("an unrelated harness pair must not refuse: %v", err)
	}
}

// An "@path" file that resolves to empty is refused, exactly as a command's
// empty output is: an emptied token file would hand the child an empty
// GH_TOKEN and move the failure into git/gh inside a session. An INLINE ""
// literal is different — static, visible in the config, sometimes deliberate —
// and still passes.
func TestSessionEnvRefusesAnEmptyAtPathFile(t *testing.T) {
	skipIfModeIsMeaningless(t)
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := defaults()
	c.Env = EnvMap{"GH_TOKEN": {Literal: "@" + empty}}
	_, err := c.SessionEnv(c.Harness, "")
	if err == nil || !strings.Contains(err.Error(), "env GH_TOKEN") || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("want an empty-file refusal naming the variable, got %v", err)
	}

	c.Env = EnvMap{"FLAG": {Literal: ""}}
	got, err := c.SessionEnv(c.Harness, "")
	if err != nil {
		t.Fatalf("an inline empty literal must still pass: %v", err)
	}
	if len(got) != 1 || got[0] != "FLAG=" {
		t.Fatalf("got %v", got)
	}
}

// --- doctor support -----------------------------------------------------------

func TestDeclaredEnvsListsAllFourLevelsInOrder(t *testing.T) {
	c := defaults()
	c.Env = EnvMap{"TOP": {Literal: "x"}}
	c.Harnesses = map[string]HarnessConfig{"opencode": {Env: EnvMap{"HARNESS": {FromCommand: "c", isCommand: true}}}}
	c.Projects = []Project{{
		Slug:          "p",
		WorkDir:       t.TempDir(),
		Env:           EnvMap{"PROJ_FILE": {Literal: "@/some/path"}},
		EnvPerHarness: map[string]EnvMap{"claude": {"PAIR": {Literal: "y"}}},
	}}

	got := c.DeclaredEnvs()
	var b strings.Builder
	for _, d := range got {
		b.WriteString(d.Label + ":" + d.Name + ";" + d.Path + "\n")
	}
	want := strings.Join([]string{
		"env:TOP;\n",
		"harnesses.opencode.env:HARNESS;\n",
		"projects[0].env:PROJ_FILE;/some/path\n",
		"projects[0].env_per_harness.claude:PAIR;\n",
	}, "")
	if b.String() != want {
		t.Fatalf("declared:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestTokenSourceDeclaredSeesEveryApplicableLayer(t *testing.T) {
	c := defaults()
	c.Projects = []Project{{Slug: "p", WorkDir: t.TempDir()}}

	if c.TokenSourceDeclared("p", "GH_TOKEN") {
		t.Fatal("nothing declared anywhere yet")
	}
	c.Harnesses = map[string]HarnessConfig{"opencode": {Env: EnvMap{"GH_TOKEN": {Literal: "x"}}}}
	if !c.TokenSourceDeclared("p", "GH_TOKEN") {
		t.Fatal("a harness-block declaration reaches this project's sessions")
	}

	c.Harnesses = nil
	c.Projects[0].EnvPerHarness = map[string]EnvMap{"claude": {"GH_TOKEN": {FromCommand: "c", isCommand: true}}}
	if !c.TokenSourceDeclared("p", "GH_TOKEN") {
		t.Fatal("the project's per-harness declaration counts")
	}
}

// VerifyEnvFilePath keeps the preflight honest about @path files now that
// Validate no longer reads them.
func TestVerifyEnvFilePathEnforcesOwnerOnly(t *testing.T) {
	skipIfModeIsMeaningless(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "tok")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyEnvFilePath(p); err != nil {
		t.Fatalf("0600 file refused: %v", err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	err := VerifyEnvFilePath(p)
	if !errors.Is(err, errInsecureMode) {
		t.Fatalf("0644 file accepted: %v", err)
	}
}

// The preflight matches spawn behavior on emptied files too: a secret that has
// been truncated to whitespace is the silent-misattribution failure, and doctor
// is where it is caught cheaply.
func TestVerifyEnvFilePathRefusesAnEmptyFile(t *testing.T) {
	skipIfModeIsMeaningless(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "tok")
	if err := os.WriteFile(p, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := VerifyEnvFilePath(p)
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("want an empty-file refusal, got %v", err)
	}
}
