package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// Every harness the registry actually offers must pass config validation. The
// accepted set is derived from harness.Known (not a hand-kept switch), so a newly
// registered adapter can never be rejected here again — the bug this guards: the
// opencode adapter was registered but the old validation switch still rejected it,
// making `clankerbar run --harness opencode` fail before harness.Get was consulted.
func TestValidateHarnessFromRegistry(t *testing.T) {
	names := harness.Names()
	if len(names) == 0 {
		t.Fatal("registry is empty; expected at least the built-in adapters")
	}
	sawOpencode := false
	for _, name := range names {
		if name == "opencode" {
			sawOpencode = true
		}
		c := defaults()
		c.Harness = name
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() rejected registered harness %q: %v", name, err)
		}
	}
	if !sawOpencode {
		t.Error("opencode must be registered and accepted (the CLA-117 adapter has to be reachable)")
	}

	// An unregistered harness is still rejected, and the message lists the
	// registered names so the error is actionable.
	c := defaults()
	c.Harness = "nope-not-a-harness"
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() accepted an unregistered harness")
	}
	if !strings.Contains(err.Error(), "opencode") {
		t.Errorf("rejection message %q should list the registered harness names", err.Error())
	}
}

func TestResolveEnv(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "token")
	if err := os.WriteFile(secret, []byte("  sk-ant-oat01-abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("nil map yields nil", func(t *testing.T) {
		got, err := resolveEnv(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Fatalf("want nil, got %v", got)
		}
	})

	t.Run("literals and @file, sorted and trimmed", func(t *testing.T) {
		got, err := resolveEnv(map[string]string{
			"ZED":                     "last",
			"CLAUDE_CODE_OAUTH_TOKEN": "@" + secret,
			"ALPHA":                   "first",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{
			"ALPHA=first",
			"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-abc",
			"ZED=last",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("want %v, got %v", want, got)
		}
	})

	t.Run("missing @file is an error naming the key", func(t *testing.T) {
		_, err := resolveEnv(map[string]string{"TOK": "@" + filepath.Join(dir, "nope")})
		if err == nil {
			t.Fatal("want error for missing file, got nil")
		}
	})
}

func TestValidatePopulatesEnvSlice(t *testing.T) {
	c := defaults()
	c.Env = map[string]string{"FOO": "bar"}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := c.EnvSlice(); !reflect.DeepEqual(got, []string{"FOO=bar"}) {
		t.Fatalf("EnvSlice = %v", got)
	}
}

func TestBacklogEndpoint(t *testing.T) {
	dir := t.TempDir()
	mcp := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"clankerbar":{"type":"http","url":"https://clankerbar.com/mcp/proj"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("derives the project-scoped path from .mcp.json when backlog_url is a bare base", func(t *testing.T) {
		c := &Config{BacklogURL: "https://clankerbar.com", MCPConfigPath: mcp}
		if got := c.BacklogEndpoint(); got != "https://clankerbar.com/mcp/proj" {
			t.Errorf("BacklogEndpoint() = %q, want the trusted origin + the .mcp.json slug", got)
		}
	})

	t.Run("explicit backlog_url with an /mcp path wins over .mcp.json", func(t *testing.T) {
		c := &Config{BacklogURL: "https://example.com/mcp/other", MCPConfigPath: mcp}
		if got := c.BacklogEndpoint(); got != "https://example.com/mcp/other" {
			t.Errorf("BacklogEndpoint() = %q, want the explicit backlog_url", got)
		}
	})

	t.Run("returns empty when only a bare base and no .mcp.json url are available", func(t *testing.T) {
		// A slug-less base is not a usable endpoint; "" makes New() not-wired so the
		// loop blind-drains instead of retrying an endpoint the plane always rejects.
		c := &Config{BacklogURL: "https://clankerbar.com", MCPConfigPath: ""}
		if got := c.BacklogEndpoint(); got != "" {
			t.Errorf("BacklogEndpoint() = %q, want \"\"", got)
		}
	})
}

// BacklogSummaryURL points at the plane's backlog-summary surface (counts + console
// pause). When a project slug is derivable from the resolved MCP endpoint it returns
// the slug-ful `/api/projects/<slug>/backlog-summary` form (CLA-141), which the
// operator's ACCOUNT key can poll; only when no slug resolves does it fall back to
// the legacy slug-less route, which needs a project-scoped key. It still resolves in
// more cases than BacklogEndpoint (a bare base with no .mcp.json is usable — via the
// legacy form).
//
// The ORIGIN, though, is always backlog_url's (CLA-257). It is the one part of a
// credentialed URL a file inside the workdir may not move; .mcp.json contributes the
// slug and nothing else.
func TestBacklogSummaryURL(t *testing.T) {
	dir := t.TempDir()
	mcp := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"clankerbar":{"type":"http","url":"https://self.example.com/mcp/proj"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("takes the SLUG from .mcp.json but never its origin", func(t *testing.T) {
		// Before CLA-257 this returned https://self.example.com/... — the origin came
		// from a file inside the workdir, so a committed .mcp.json in a cloned repo
		// chose where the account-scoped key went. Now only /mcp/proj's slug is read.
		c := &Config{BacklogURL: "https://clankerbar.com", MCPConfigPath: mcp}
		if got := c.BacklogSummaryURL(); got != "https://clankerbar.com/api/projects/proj/backlog-summary" {
			t.Errorf("BacklogSummaryURL() = %q, want backlog_url's origin + the .mcp.json slug", got)
		}
	})

	t.Run("falls back to BacklogURL's own origin when no MCP endpoint resolves", func(t *testing.T) {
		// The key improvement over BacklogEndpoint: a bare base with no .mcp.json is
		// still usable here (the route needs no project slug), so pause/count-gating
		// work rather than dropping to blind mode.
		c := &Config{BacklogURL: "https://clankerbar.com", MCPConfigPath: ""}
		if got := c.BacklogSummaryURL(); got != "https://clankerbar.com/api/backlog-summary" {
			t.Errorf("BacklogSummaryURL() = %q, want the bare-base origin", got)
		}
	})

	t.Run("an explicit /mcp/<slug> backlog_url yields that origin AND that slug", func(t *testing.T) {
		// Pre-CLA-141 this stripped down to the origin (the legacy route ignored the
		// slug); now the slug the operator explicitly named selects the slug-ful form.
		c := &Config{BacklogURL: "https://example.com/mcp/other", MCPConfigPath: ""}
		if got := c.BacklogSummaryURL(); got != "https://example.com/api/projects/other/backlog-summary" {
			t.Errorf("BacklogSummaryURL() = %q, want the origin + slug-ful path", got)
		}
	})

	t.Run("an explicit backlog_url wins over a different .mcp.json origin", func(t *testing.T) {
		// Explicit config overrides (README): the operator pointed backlog_url at a
		// self-hosted plane, so the summary poll must hit THAT origin even though
		// .mcp.json names https://self.example.com. Regression for finding #4.
		c := &Config{BacklogURL: "https://plane.internal/mcp/proj", MCPConfigPath: mcp}
		if got := c.BacklogSummaryURL(); got != "https://plane.internal/api/projects/proj/backlog-summary" {
			t.Errorf("BacklogSummaryURL() = %q, want the explicit backlog_url's origin to win", got)
		}
	})

	t.Run("the DEFAULT base still beats a resolved .mcp.json origin", func(t *testing.T) {
		// This is the case the exploit rode in on, and the reason the old behaviour
		// had to go rather than be narrowed: leaving backlog_url at its default is the
		// documented normal setup, so "defer to .mcp.json when backlog_url is default"
		// meant every ordinary run took its credential destination from the checkout.
		// Self-hosting is still supported — by SAYING so in backlog_url, per CLA-257.
		c := &Config{BacklogURL: defaultBacklogURL, MCPConfigPath: mcp}
		if got := c.BacklogSummaryURL(); got != "https://clankerbar.com/api/projects/proj/backlog-summary" {
			t.Errorf("BacklogSummaryURL() = %q, want the default base's origin", got)
		}
	})

	t.Run("empty when no origin can be resolved", func(t *testing.T) {
		c := &Config{BacklogURL: "", MCPConfigPath: ""}
		if got := c.BacklogSummaryURL(); got != "" {
			t.Errorf("BacklogSummaryURL() = %q, want \"\"", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Multi-project config (CLA-142): the `projects` list and its URL derivation.

func TestProjectsValidation(t *testing.T) {
	base := func() *Config {
		return &Config{Harness: "claude", Prompt: "Work the backlog."}
	}

	t.Run("no projects list — single-project mode validates exactly as before", func(t *testing.T) {
		if err := base().Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	})

	t.Run("a valid projects list normalizes paths and passes", func(t *testing.T) {
		c := base()
		c.Projects = []Project{
			{Slug: "clankerbar", WorkDir: "~/dev"},
			{Slug: "ezyapp", WorkDir: "/repos/ezyapp"},
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
		if strings.HasPrefix(c.Projects[0].WorkDir, "~") {
			t.Errorf("workdir %q not home-expanded", c.Projects[0].WorkDir)
		}
	})

	t.Run("a project without a slug is rejected", func(t *testing.T) {
		c := base()
		c.Projects = []Project{{WorkDir: "/repos/x"}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "slug is required") {
			t.Errorf("Validate() = %v, want a slug-required error", err)
		}
	})

	t.Run("duplicate slugs are rejected", func(t *testing.T) {
		c := base()
		c.Projects = []Project{{Slug: "same"}, {Slug: "same"}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate slug") {
			t.Errorf("Validate() = %v, want a duplicate-slug error", err)
		}
	})

	t.Run("each project's mcp config defaults to its own workdir's .mcp.json", func(t *testing.T) {
		dir := t.TempDir()
		mcp := filepath.Join(dir, ".mcp.json")
		if err := os.WriteFile(mcp, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		c := base()
		c.Projects = []Project{{Slug: "proj", WorkDir: dir}, {Slug: "bare", WorkDir: t.TempDir()}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		if got := c.Projects[0].MCPConfigPath; got != mcp {
			t.Errorf("MCPConfigPath = %q, want the workdir's own .mcp.json %q", got, mcp)
		}
		// No .mcp.json in the workdir → left empty (the loop falls back to the
		// top-level mcp_config_path at invocation time).
		if got := c.Projects[1].MCPConfigPath; got != "" {
			t.Errorf("MCPConfigPath = %q, want \"\" when the workdir has no .mcp.json", got)
		}
	})

	t.Run("the top-level mcp_config_path also defaults from the workdir", func(t *testing.T) {
		// Claude's -p mode does not auto-discover .mcp.json; without this default a
		// bare run from a workdir carrying one would spawn sessions with no
		// clankerbar tools and the poller could derive no slug.
		dir := t.TempDir()
		mcp := filepath.Join(dir, ".mcp.json")
		if err := os.WriteFile(mcp, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		c := base()
		c.WorkDir = dir
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		if got := c.MCPConfigPath; got != mcp {
			t.Errorf("MCPConfigPath = %q, want the workdir's .mcp.json %q", got, mcp)
		}
	})
}

func TestProjectSummaryURL(t *testing.T) {
	t.Run("defaults to the public plane origin with the project's slug in the path", func(t *testing.T) {
		c := &Config{BacklogURL: defaultBacklogURL}
		p := Project{Slug: "ezyapp"}
		if got := c.ProjectSummaryURL(p); got != "https://clankerbar.com/api/projects/ezyapp/backlog-summary" {
			t.Errorf("ProjectSummaryURL() = %q", got)
		}
	})

	t.Run("ignores the project's own .mcp.json origin (CLA-257)", func(t *testing.T) {
		// Per-project self-hosting moved to the same rule as everything else: state
		// the origin in backlog_url. A project's .mcp.json no longer redirects the
		// key, and a run whose backlog_url disagrees with it is refused by Validate
		// outright — see TestHostileMCPConfigRefused.
		dir := t.TempDir()
		mcp := filepath.Join(dir, ".mcp.json")
		if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"clankerbar":{"type":"http","url":"https://plane.internal/mcp/proj"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		c := &Config{BacklogURL: defaultBacklogURL}
		p := Project{Slug: "proj", MCPConfigPath: mcp}
		if got := c.ProjectSummaryURL(p); got != "https://clankerbar.com/api/projects/proj/backlog-summary" {
			t.Errorf("ProjectSummaryURL() = %q, want backlog_url's origin", got)
		}
	})

	t.Run("an explicit backlog_url origin wins for every project", func(t *testing.T) {
		c := &Config{BacklogURL: "https://plane.internal"}
		p := Project{Slug: "proj"}
		if got := c.ProjectSummaryURL(p); got != "https://plane.internal/api/projects/proj/backlog-summary" {
			t.Errorf("ProjectSummaryURL() = %q, want the explicit origin", got)
		}
	})

	t.Run("path-escapes a slug so a hostile config cannot smuggle path segments", func(t *testing.T) {
		c := &Config{BacklogURL: defaultBacklogURL}
		p := Project{Slug: "a/b"}
		if got := c.ProjectSummaryURL(p); got != "https://clankerbar.com/api/projects/a%2Fb/backlog-summary" {
			t.Errorf("ProjectSummaryURL() = %q, want the slug path-escaped", got)
		}
	})
}

// The CLA-310 empty-rollup opt-out resolves per project: a matching entry's
// own value wins; an unmatched slug (the single-project shape reaching the
// resolver with an empty one) falls back to the top level.
func TestAllowUncheckedPRFor(t *testing.T) {
	c := &Config{
		AllowUncheckedPR: false,
		Projects: []Project{
			{Slug: "strict"},
			{Slug: "loose", AllowUncheckedPR: true},
		},
	}

	if c.AllowUncheckedPRFor("strict") {
		t.Errorf("strict project inherited nothing and should refuse")
	}
	if !c.AllowUncheckedPRFor("loose") {
		t.Errorf("loose project opted out and should warn")
	}
	if c.AllowUncheckedPRFor("") {
		t.Errorf("unmatched slug should fall back to the top-level value (false here)")
	}

	top := &Config{AllowUncheckedPR: true}
	if !top.AllowUncheckedPRFor("anything") {
		t.Errorf("single-project mode should read the top-level field")
	}
}

func TestProjectsSlugMCPMismatchRefused(t *testing.T) {
	// The slug decides which queue is polled; the .mcp.json decides which project
	// sessions work. A disagreement is a silent split-brain — Validate must refuse.
	dir := t.TempDir()
	mcp := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(mcp, []byte(`{"mcpServers":{"clankerbar":{"type":"http","url":"https://clankerbar.com/mcp/other"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Config{Harness: "claude", Prompt: "Work the backlog.", Projects: []Project{{Slug: "proj", WorkDir: dir}}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "does not match its .mcp.json") {
		t.Errorf("Validate() = %v, want a slug/.mcp.json mismatch refusal", err)
	}
}

// Which dial stopped a run is the first thing an operator asks, and the log line
// used to print all three figures side by side — so a run capped only on wall
// clock still reported a token count and a dollar figure, which read as the cause.
func TestBudgetExceededByNamesTheDimension(t *testing.T) {
	cases := []struct {
		name    string
		budget  Budget
		tokens  int
		cost    float64
		elapsed time.Duration
		want    string // substring; "" means not exceeded
	}{
		{
			name:    "wall clock alone names wall clock, not the incidental spend",
			budget:  Budget{MaxWallClock: Duration(8 * time.Hour)},
			tokens:  140387,
			cost:    147.98,
			elapsed: 10*time.Hour + 23*time.Minute,
			want:    "wall clock",
		},
		{
			budget: Budget{MaxCostUSD: 100},
			name:   "cost ceiling names cost",
			cost:   147.98,
			want:   "cost $147.98",
		},
		{
			budget: Budget{MaxTokens: 1000},
			name:   "token ceiling names tokens",
			tokens: 5000,
			want:   "tokens 5000",
		},
		{
			name:    "under every ceiling reports nothing",
			budget:  Budget{MaxWallClock: Duration(8 * time.Hour), MaxCostUSD: 100},
			cost:    12,
			elapsed: time.Hour,
			want:    "",
		},
		{
			name:   "zero ceilings are disabled, not instantly exceeded",
			budget: Budget{},
			tokens: 1 << 30,
			cost:   1e6,
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.budget.ExceededBy(tc.tokens, tc.cost, tc.elapsed)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("ExceededBy = %q, want no breach", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("ExceededBy = %q, want it to name %q", got, tc.want)
			}
			// Exceeded must stay consistent with the reason it now reports.
			if !tc.budget.Exceeded(tc.tokens, tc.cost, tc.elapsed) {
				t.Error("Exceeded disagrees with ExceededBy")
			}
		})
	}
}

// The deadline is what lets the loop decide, mid-iteration, whether waiting out a
// usage limit is worth anything — a zero ceiling means there is no deadline.
func TestBudgetDeadline(t *testing.T) {
	start := time.Now()
	if got := (Budget{}).Deadline(start); !got.IsZero() {
		t.Errorf("no wall-clock ceiling should give no deadline, got %v", got)
	}
	got := Budget{MaxWallClock: Duration(8 * time.Hour)}.Deadline(start)
	if want := start.Add(8 * time.Hour); !got.Equal(want) {
		t.Errorf("Deadline = %v, want %v", got, want)
	}
}

func TestBudgetRemaining(t *testing.T) {
	b := Budget{MaxWallClock: Duration(8 * time.Hour)}
	if got, bounded := b.Remaining(2 * time.Hour); !bounded || got != 6*time.Hour {
		t.Errorf("Remaining(2h) = %s, %v; want 6h, true", got, bounded)
	}
	// Overspent is reported as negative rather than clamped: the caller compares
	// it against a wait, and clamping to zero would read as "no time left" for a
	// run that is already over its ceiling — the same answer by luck, not by rule.
	if got, _ := b.Remaining(9 * time.Hour); got != -time.Hour {
		t.Errorf("Remaining(9h) = %s, want -1h", got)
	}
	if _, bounded := (Budget{}).Remaining(time.Hour); bounded {
		t.Error("an unset ceiling must report bounded=false")
	}
	// A per-harness block carries no wall clock, so it cannot make an unbounded
	// run look bounded to waitPastBudget — which reads exactly this flag to decide
	// whether waiting out a reset is worth anything (CLA-367 keeps the clock the
	// RUN's, however many harnesses it is spent on).
	perHarness := Budget{PerHarness: map[string]HarnessBudget{"opencode": {MaxCostUSD: 2}}}
	if _, bounded := perHarness.Remaining(time.Hour); bounded {
		t.Error("a per-harness spend block must not report a wall-clock bound")
	}
}

// The default prompt decides how much ONE session takes on, and it silently
// decided the wrong thing for as long as this package has existed: the default
// was "Work the backlog.", which the served protocol defines as *drain the whole
// ready queue*. So a loop whose entire purpose is one-task-per-session — a fresh,
// bounded context each iteration — asked every session to do the opposite, and no
// test disagreed. One observed session ran 2h06m on a single accumulating context
// and had to be killed, because `loopPaused`/STOP/HALT are read between
// ITERATIONS and an iteration had become "the whole queue".
//
// This pins the phrase rather than merely asserting non-emptiness, because the
// wording is the interface: the agent reading it has read the protocol, where
// "work the backlog" and "work the next backlog item" are two different
// instructions. A paraphrase that loses "next" is the regression to catch.
func TestDefaultPromptAsksForOneTask(t *testing.T) {
	got := defaults().Prompt

	if got != defaultPrompt {
		t.Fatalf("defaults().Prompt = %q, want the defaultPrompt constant %q", got, defaultPrompt)
	}
	// The protocol's single-task phrase, matched case-insensitively so a capital
	// letter is not a failure while a changed INSTRUCTION is.
	if !strings.Contains(strings.ToLower(got), "next backlog item") {
		t.Errorf("default prompt %q does not ask for the NEXT backlog item; the served protocol\n"+
			"reads that phrase as 'exactly one task', and anything else risks a full drain", got)
	}
	// Ban the drain VOCABULARY, not one exact string. "Work the next backlog item,
	// then keep going until next_task is dry" passes the check above while doing
	// exactly what this test exists to stop, so a single-phrase ban is too narrow.
	for _, banned := range []string{
		"work the backlog",
		"drain",
		"until next_task is dry",
		"keep going",
		"whole queue",
		"every ready task",
	} {
		if strings.Contains(strings.ToLower(got), banned) {
			t.Errorf("default prompt %q contains %q, which reads as an instruction to work past\n"+
				"one task; that costs the bounded per-session context and pushes the operator's\n"+
				"pause/HALT boundary to the end of the queue", got, banned)
		}
	}
}

// The symmetric regression to the one above: an operator who DOES set a prompt
// must keep it. Layering can fail in both directions, and a default that
// clobbered an explicit setting would be the more offensive failure - it would
// silently override a deliberate choice rather than supply a missing one.
func TestLoadKeepsAnExplicitPrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clankerbar.json")
	const custom = "Work the backlog."
	if err := os.WriteFile(path, []byte(`{"harness":"claude","prompt":"`+custom+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Deliberately the DRAIN phrase: opting back into the old behaviour is a
	// supported choice, and this pins that the new default does not take it away.
	if c.Prompt != custom {
		t.Errorf("Load() Prompt = %q, want the explicitly configured %q", c.Prompt, custom)
	}
}

// A config file that says nothing about the prompt must inherit the one-task
// default. This is the path every end user is on: `go install` plus a config that
// sets a harness and little else. Asserting it through Load (rather than through
// defaults() alone) is the point — the layering is what could drop it.
func TestLoadWithoutPromptInheritsOneTaskDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clankerbar.json")
	if err := os.WriteFile(path, []byte(`{"harness":"claude"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Prompt != defaultPrompt {
		t.Errorf("Load() Prompt = %q, want %q", c.Prompt, defaultPrompt)
	}
}

// --- CLA-288: the zero-spend attempt bound -----------------------------------

func TestZeroSpendAttemptBound(t *testing.T) {
	t.Run("an unset dial resolves to the built-in default", func(t *testing.T) {
		c := &Config{}
		if got := c.ZeroSpendAttemptBound(); got != DefaultMaxZeroSpendAttempts {
			t.Errorf("bound = %d, want the default %d - the breaker is always on, so nothing resolves to zero", got, DefaultMaxZeroSpendAttempts)
		}
	})

	t.Run("the operator's own value wins", func(t *testing.T) {
		c := &Config{MaxZeroSpendAttempts: 7}
		if got := c.ZeroSpendAttemptBound(); got != 7 {
			t.Errorf("bound = %d, want the configured 7", got)
		}
	})

	t.Run("a negative value is refused, not clamped", func(t *testing.T) {
		// Clamping would leave the config file reading as "set" while the default
		// quietly applied - the silent-inert shape this whole task is about.
		c := &Config{Harness: "claude", Prompt: "work", MaxZeroSpendAttempts: -1}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "max_zero_spend_attempts") {
			t.Errorf("Validate() = %v, want an error naming max_zero_spend_attempts", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Per-harness breakers (CLA-367).
//
// A mixed-harness run cannot be bounded by one number: 75M tokens is a sane week
// of Claude and roughly $2 on a DeepSeek-class backend, and a dollar figure is
// meaningless for a session billed to a subscription (CLA-289). So each harness
// carries its own block, measured in its own unit, over its own sessions.

func TestHarnessBudgetExceededByNamesTheHarness(t *testing.T) {
	cases := []struct {
		name    string
		block   HarnessBudget
		harness string
		tokens  int
		cost    float64
		want    string // substring; "" means not exceeded
	}{
		{
			// The claude side: plan-calibrated tokens, unchanged semantics.
			name:    "a token ceiling names the harness and the tokens",
			block:   HarnessBudget{MaxTokens: 75_000_000},
			harness: "claude",
			tokens:  75_000_000,
			want:    "claude tokens 75000000",
		},
		{
			// The metered side: dollars, summed off the adapter's CostUSD.
			name:    "a cost ceiling names the harness and the dollars",
			block:   HarnessBudget{MaxCostUSD: 2},
			harness: "opencode",
			cost:    2.05,
			want:    "opencode cost $2.05",
		},
		{
			// The whole point of the split: a cheap backend's tokens never reach a
			// dial measured in dollars, so a block only sees the unit it set.
			name:    "the unit this block did not set is not measured against it",
			block:   HarnessBudget{MaxCostUSD: 2},
			harness: "opencode",
			tokens:  400_000_000,
			want:    "",
		},
		{
			name:    "under the ceiling reports nothing",
			block:   HarnessBudget{MaxTokens: 100, MaxCostUSD: 2},
			harness: "claude",
			tokens:  99,
			cost:    1.99,
			want:    "",
		},
		{
			name:    "a zero block is disabled, not instantly exceeded",
			block:   HarnessBudget{},
			harness: "claude",
			tokens:  1 << 30,
			cost:    1e6,
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.block.ExceededBy(tc.harness, tc.tokens, tc.cost)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("ExceededBy = %q, want no breach", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("ExceededBy = %q, want it to name %q", got, tc.want)
			}
		})
	}
}

// Each block is counted over ONLY its own harness's spend, and either trip stops
// the run: the mixed-harness case the feature exists for — implement on opencode
// under a dollar ceiling, review on claude under a token ceiling.
func TestBudgetExceededByHarnessIsIndependentPerHarness(t *testing.T) {
	b := Budget{PerHarness: map[string]HarnessBudget{
		"claude":   {MaxTokens: 75_000_000},
		"opencode": {MaxCostUSD: 2},
	}}

	// The claude side trips on tokens...
	if got := b.ExceededByHarness("claude", 80_000_000, 0); !strings.Contains(got, "claude tokens") {
		t.Errorf("claude token ceiling did not trip: %q", got)
	}
	// ...and is untouched by dollars, the dial it has no meaning for.
	if got := b.ExceededByHarness("claude", 1000, 500); got != "" {
		t.Errorf("claude tripped on a dollar figure it never set: %q", got)
	}
	// The opencode side trips on dollars...
	if got := b.ExceededByHarness("opencode", 0, 2.5); !strings.Contains(got, "opencode cost") {
		t.Errorf("opencode cost ceiling did not trip: %q", got)
	}
	// ...and is untouched by a token count that would have ended the claude side
	// twice over.
	if got := b.ExceededByHarness("opencode", 200_000_000, 0); got != "" {
		t.Errorf("opencode tripped on claude's unit: %q", got)
	}
	// A harness with no block of its own is unbounded HERE: the run-wide dials are
	// the only thing over it, and ExceededByHarness is not asked about them.
	if got := b.ExceededByHarness("codex", 1<<30, 1e6); got != "" {
		t.Errorf("a harness with no block reported a breach: %q", got)
	}
}

// The pre-CLA-367 configs are what must not move: with no per_harness block the
// global dials behave exactly as they always have, and nothing new can trip.
func TestBudgetWithoutPerHarnessIsUnchanged(t *testing.T) {
	b := Budget{MaxTokens: 100}
	if got := b.ExceededByHarness("claude", 1<<30, 1e6); got != "" {
		t.Errorf("a budget with no per_harness block reported a per-harness breach: %q", got)
	}
	if got := b.ExceededBy(100, 0, 0); !strings.Contains(got, "tokens 100") {
		t.Errorf("the global dial stopped naming its own dimension: %q", got)
	}
	if !b.CountsSpendFor("claude") || !b.CountsSpendFor("opencode") {
		t.Error("a global spend dial covers every harness, as it always did")
	}
}

// CountsSpend is what CLA-262's side effects turn on: a session whose spend
// cannot be measured breaks a promise only where a promise was made. In a
// mixed-harness run that promise follows the harness whose breaker is set.
func TestBudgetCountsSpendForFollowsTheHarnessWhoseBreakerIsSet(t *testing.T) {
	b := Budget{
		MaxWallClock: Duration(8 * time.Hour), // not a spend ceiling, on either side
		PerHarness:   map[string]HarnessBudget{"opencode": {MaxCostUSD: 2}},
	}
	if !b.CountsSpendFor("opencode") {
		t.Error("opencode has a spend ceiling of its own; an unreadable opencode session breaks it")
	}
	if b.CountsSpendFor("claude") {
		t.Error("claude is under no spend ceiling here, so an unreadable claude session breaks no promise")
	}
	if b.CountsSpend() {
		t.Error("CountsSpend asks about the RUN-WIDE dials and must not be moved by a per-harness block")
	}
}

// A block keyed by a name no adapter answers to can never be charged, so it is a
// ceiling that cannot trip — the run is unbounded on exactly the side the
// operator meant to bound. Refused at load rather than discovered at 3am.
func TestValidateRejectsAnUnknownPerHarnessName(t *testing.T) {
	c := defaults()
	c.Budget.PerHarness = map[string]HarnessBudget{"nope-not-a-harness": {MaxCostUSD: 2}}
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() accepted a per_harness block for an unregistered harness")
	}
	if !strings.Contains(err.Error(), "per_harness") || !strings.Contains(err.Error(), "opencode") {
		t.Errorf("rejection %q should name the field and list the registered harnesses", err.Error())
	}

	c = defaults()
	c.Budget.PerHarness = map[string]HarnessBudget{"opencode": {MaxCostUSD: 2}, "claude": {MaxTokens: 75_000_000}}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() rejected blocks for registered harnesses: %v", err)
	}
}

// The blocks arrive from a JSON config file, so the field names an operator types
// are part of the interface — a rename would silently disable their ceiling.
func TestPerHarnessBudgetDecodesFromJSON(t *testing.T) {
	var b Budget
	const in = `{"max_wall_clock":"6h","per_harness":{"opencode":{"max_cost_usd":2},"claude":{"max_tokens":75000000}}}`
	if err := json.Unmarshal([]byte(in), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := b.PerHarness["opencode"].MaxCostUSD; got != 2 {
		t.Errorf("per_harness.opencode.max_cost_usd = %v, want 2", got)
	}
	if got := b.PerHarness["claude"].MaxTokens; got != 75_000_000 {
		t.Errorf("per_harness.claude.max_tokens = %d, want 75000000", got)
	}
}

// The per-session runaway detector (CLA-343) derives from the run's token
// ceiling, and CLA-367 gives that ceiling a second home. Following the
// documented migration — moving claude's max_tokens into its own block — must
// not silently loosen the detector to the 150M floor.
func TestSessionTokenCeilingFollowsAPerHarnessTokenCeiling(t *testing.T) {
	perHarness := Budget{PerHarness: map[string]HarnessBudget{
		"claude": {MaxTokens: 20_000_000},
	}}
	if got := perHarness.SessionTokenCeilingFor("claude"); got != 40_000_000 {
		t.Errorf("SessionTokenCeilingFor(claude) = %d, want 2x the block's own ceiling", got)
	}
	// A harness with no token ceiling anywhere still gets the floor, not another
	// harness's derivation.
	if got := perHarness.SessionTokenCeilingFor("opencode"); got != sessionTokenFloor {
		t.Errorf("SessionTokenCeilingFor(opencode) = %d, want the floor", got)
	}
	// The run-wide dial is the tighter promise where both are set: it bounds the
	// whole run, so it wins.
	both := Budget{MaxTokens: 5_000_000, PerHarness: map[string]HarnessBudget{
		"claude": {MaxTokens: 90_000_000},
	}}
	if got := both.SessionTokenCeilingFor("claude"); got != 10_000_000 {
		t.Errorf("SessionTokenCeilingFor = %d, want the run-wide dial's derivation", got)
	}
	// The operator's own dial still wins over everything.
	explicit := Budget{MaxSessionTokens: 7, PerHarness: map[string]HarnessBudget{
		"claude": {MaxTokens: 90_000_000},
	}}
	if got := explicit.SessionTokenCeilingFor("claude"); got != 7 {
		t.Errorf("SessionTokenCeilingFor = %d, want the operator's own max_session_tokens", got)
	}
	// The harness-blind form is unchanged for every pre-CLA-367 config.
	if got := (Budget{MaxTokens: 75_000_000}).SessionTokenCeiling(); got != 150_000_000 {
		t.Errorf("SessionTokenCeiling = %d, want 2x max_tokens as before", got)
	}
}
