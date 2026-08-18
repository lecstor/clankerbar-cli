package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Per-phase harness selection (CLA-366): a phase may name the harness it runs
// on, and the harness-shaped fields stop being run-wide when it does.
//
// The invariant every test here circles is the same one: a field in this config
// is a DIALECT of one harness, so it must never be handed to another. A claude
// model alias on opencode's --model, or a Claude-shaped .mcp.json as
// OPENCODE_CONFIG, is a session that dies at startup with the operator's own
// config as the cause.

// mixedCfg is the shape the feature exists for: implement on opencode, review on
// claude, with claude as the run's harness so the top-level fields still describe it.
func mixedCfg() *Config {
	return &Config{
		Harness: "claude",
		Model:   "claude-opus-5",
		Models:  map[string]string{"strong": "claude-opus-5", "standard": "claude-sonnet-5"},
		Phases: []Phase{
			{Name: "implement", Harness: "opencode", Tier: "strong"},
			{Name: "review", Tier: "standard"},
		},
		Harnesses: map[string]HarnessConfig{
			"opencode": {ConfigDir: "/oc/config", MCPConfigPath: "/oc/opencode-mcp.json"},
		},
	}
}

func TestHarnessFor_PhaseOverridesTheRunWideHarness(t *testing.T) {
	c := mixedCfg()
	if got := c.HarnessFor(c.Phases[0]); got != "opencode" {
		t.Errorf("phase 0 resolves to harness %q, want opencode", got)
	}
	// The point of the empty case: an untouched phase is still the run's harness,
	// which is what keeps every pre-existing config behaving identically.
	if got := c.HarnessFor(c.Phases[1]); got != "claude" {
		t.Errorf("a phase naming no harness resolves to %q, want the run-wide claude", got)
	}
}

func TestEffectivePhases_CarriesTheResolvedHarness(t *testing.T) {
	got := mixedCfg().EffectivePhases()
	if got[0].Harness != "opencode" || got[1].Harness != "claude" {
		t.Fatalf("EffectivePhases harnesses = %q/%q, want opencode/claude — the driver reads this and must not re-derive it",
			got[0].Harness, got[1].Harness)
	}

	// An unphased run's synthesized phase names the run's harness for the same
	// reason: nothing downstream should have to special-case "no harness named".
	c := &Config{Harness: "codex", Prompt: "Work the next backlog item."}
	if got := c.EffectivePhases(); got[0].Harness != "codex" {
		t.Errorf("an unphased run's phase carries harness %q, want codex", got[0].Harness)
	}
}

func TestSessionFor_TopLevelHarnessInheritsTheRunWideFields(t *testing.T) {
	c := mixedCfg()
	c.ConfigDir = "/home/me/.claude"
	c.MCPConfigPath = "/repo/.mcp.json"
	c.SettingsPath = "/home/me/headless.json"

	hc := c.SessionFor("claude")
	if hc.ConfigDir != "/home/me/.claude" || hc.MCPConfigPath != "/repo/.mcp.json" || hc.SettingsPath != "/home/me/headless.json" {
		t.Errorf("the top-level harness lost a run-wide field: %+v", hc)
	}
	if hc.Model != "claude-opus-5" || hc.Models["strong"] != "claude-opus-5" {
		t.Errorf("the top-level harness lost the run-wide model/tier map: %+v", hc)
	}
}

func TestSessionFor_AnotherHarnessInheritsNothing(t *testing.T) {
	c := mixedCfg()
	c.ConfigDir = "/home/me/.claude"
	c.MCPConfigPath = "/repo/.mcp.json"
	c.SettingsPath = "/home/me/headless.json"

	hc := c.SessionFor("opencode")
	// Each of these leaking would be a real, reported failure mode: opencode
	// refuses to start on Claude's `mcpServers`, and CLAUDE_CONFIG_DIR's contents
	// are not an opencode config dir.
	if hc.ConfigDir != "/oc/config" {
		t.Errorf("opencode config_dir = %q, want its own /oc/config — never the run-wide claude one", hc.ConfigDir)
	}
	if hc.MCPConfigPath != "/oc/opencode-mcp.json" {
		t.Errorf("opencode mcp_config_path = %q, want its own — the Claude-shaped file makes opencode refuse to start", hc.MCPConfigPath)
	}
	if hc.SettingsPath != "" || hc.Model != "" || hc.Models != nil {
		t.Errorf("opencode inherited a run-wide field it must not: %+v", hc)
	}
}

func TestModelForPhase_ResolvesTiersAgainstThePhasesOwnHarness(t *testing.T) {
	c := mixedCfg()
	c.Harnesses["opencode"] = HarnessConfig{
		ConfigDir:     "/oc/config",
		MCPConfigPath: "/oc/opencode-mcp.json",
		Models:        map[string]string{"strong": "opencode-go/deepseek-v4-flash"},
	}

	// One bucket NAME, two aliases: that indirection is the whole reason tiers are
	// buckets rather than model aliases.
	if got, ok := c.ModelForPhase(c.Phases[0]); got != "opencode-go/deepseek-v4-flash" || !ok {
		t.Errorf("the opencode phase's `strong` resolved to (%q, %v), want opencode's own alias", got, ok)
	}
	if got, ok := c.ModelForPhase(c.Phases[1]); got != "claude-sonnet-5" || !ok {
		t.Errorf("the claude phase's `standard` resolved to (%q, %v), want claude-sonnet-5", got, ok)
	}
}

func TestModelForPhase_AnUnconfiguredHarnessRunsOnItsOwnDefault(t *testing.T) {
	c := mixedCfg() // the opencode block declares no model and no tier map

	// Empty, NOT the run-wide claude alias. opencode's model lives in opencode's
	// own config; handing it "claude-opus-5" would put an alias no provider has on
	// its --model flag.
	got, ok := c.ModelForPhase(c.Phases[0])
	if got != "" {
		t.Errorf("the opencode phase resolved to model %q, want empty — a claude alias must never reach another harness's --model", got)
	}
	// Reported as the typo case, honestly: a tier WAS named and resolved to
	// nothing, which is exactly what the caller logs.
	if ok {
		t.Error("a tier that resolved to nothing reported ok — the driver logs on !ok, and this is the case worth logging")
	}
}

func TestModelForTier_UnphasedBehaviourIsUnchanged(t *testing.T) {
	// The back-compat guarantee: ModelForTier now runs through the per-harness
	// path, and a single-harness config must not be able to tell.
	c := &Config{Harness: "claude", Model: "opus", Models: map[string]string{"cheap": "haiku"}}
	if got, ok := c.ModelForTier("cheap"); got != "haiku" || !ok {
		t.Errorf("ModelForTier(cheap) = (%q, %v), want (haiku, true)", got, ok)
	}
	if got, ok := c.ModelForTier(""); got != "opus" || !ok {
		t.Errorf("ModelForTier(\"\") = (%q, %v), want (opus, true)", got, ok)
	}
	if got, ok := c.ModelForTier("typo"); got != "opus" || ok {
		t.Errorf("ModelForTier(typo) = (%q, %v), want the default and not-ok", got, ok)
	}
}

func TestPhaseHarnesses_ListsEveryHarnessTheRunWillSpawn(t *testing.T) {
	got := mixedCfg().PhaseHarnesses()
	// Run-wide first, then sequence order, deduplicated: doctor prints these and
	// the first one keeps the unqualified label.
	if len(got) != 2 || got[0] != "claude" || got[1] != "opencode" {
		t.Errorf("PhaseHarnesses() = %v, want [claude opencode]", got)
	}
	if got := (&Config{Harness: "claude"}).PhaseHarnesses(); len(got) != 1 {
		t.Errorf("an unphased config spawns %v, want just its own harness", got)
	}
}

func TestResolveMCPConfig_ProjectAndSchemaBothDecide(t *testing.T) {
	c := mixedCfg()
	c.MCPConfigPath = "/repo/.mcp.json"

	t.Run("the top-level harness takes the project's own file", func(t *testing.T) {
		if got := c.ResolveMCPConfig("claude", "/proj/.mcp.json", nil); got != "/proj/.mcp.json" {
			t.Errorf("= %q, want the project's file", got)
		}
	})
	t.Run("another harness never takes it — wrong schema", func(t *testing.T) {
		if got := c.ResolveMCPConfig("opencode", "/proj/.mcp.json", nil); got != "/oc/opencode-mcp.json" {
			t.Errorf("= %q, want opencode's own file; the project's is Claude-shaped and opencode refuses to start on it", got)
		}
	})
	t.Run("the project's per-harness file wins over both", func(t *testing.T) {
		per := map[string]string{"opencode": "/proj/opencode.json"}
		if got := c.ResolveMCPConfig("opencode", "/proj/.mcp.json", per); got != "/proj/opencode.json" {
			t.Errorf("= %q, want the per-project per-harness file — it is the only one carrying BOTH the right schema and the right slug", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Validate

func TestValidate_MixedHarnessSequenceIsAccepted(t *testing.T) {
	c := mixedCfg()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil — implement-on-opencode/review-on-claude is the shape this feature exists for", err)
	}
}

func TestValidate_RefusesAPhaseHarnessWithNoBlock(t *testing.T) {
	c := mixedCfg()
	delete(c.Harnesses, "opencode")

	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "harnesses.opencode") {
		t.Fatalf("Validate() = %v, want a refusal naming the missing harnesses.opencode block", err)
	}
}

func TestValidate_RefusesAnUnknownPhaseHarness(t *testing.T) {
	c := mixedCfg()
	c.Phases[0].Harness = "nope-not-a-harness"
	c.Harnesses["nope-not-a-harness"] = HarnessConfig{}

	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown harness") {
		t.Fatalf("Validate() = %v, want an unknown-harness refusal", err)
	}
	if !strings.Contains(err.Error(), "opencode") {
		t.Errorf("refusal %q should list the registered names, as the top-level check does", err)
	}
}

// The phase-aware claim rule: it is the CLAIMING phase's harness that must
// observe the claim, not the run's. Both directions matter — the old check read
// the top-level harness, which would now certify the wrong session.
func TestValidate_ClaimTrackingIsAskedOfTheClaimingPhasesHarness(t *testing.T) {
	t.Run("a non-tracking harness on the FIRST phase is refused", func(t *testing.T) {
		c := &Config{
			Harness:   "claude",
			Harnesses: map[string]HarnessConfig{"codex": {ConfigDir: "/cx"}},
			Phases: []Phase{
				{Name: "implement", Harness: "codex"},
				{Name: "review"},
			},
		}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "codex") {
			t.Fatalf("Validate() = %v, want a refusal naming codex: it claims the task and cannot observe the claim, so nothing would reach phase 2", err)
		}
		if !strings.Contains(err.Error(), "phases[0]") {
			t.Errorf("refusal %q should name the phase, not just the harness — the run's own harness is fine here", err)
		}
	})

	t.Run("a non-tracking harness on a LATER phase is refused too", func(t *testing.T) {
		// The tempting reading is that a later phase never claims, so the
		// capability is not one it needs. It is: seeding Invocation.ResumeClaim is
		// the ADAPTER's job, and an adapter that does not track claims does not
		// seed either — codex returns a zero Result.Claim whatever it is handed.
		// A mid-sequence one then ends every drain early (drainPhase records a
		// checkpoint only for a phase that ends holding the task), and a final one
		// leaves drainPhases carrying its predecessor's claim into the handback,
		// which can post `ready` over a task that phase just landed at in_review.
		c := &Config{
			Harness:   "claude",
			Harnesses: map[string]HarnessConfig{"codex": {ConfigDir: "/cx"}},
			Phases: []Phase{
				{Name: "implement"},
				{Name: "review", Harness: "codex"},
			},
		}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "codex") {
			t.Fatalf("Validate() = %v, want a refusal naming codex on the later phase", err)
		}
		if !strings.Contains(err.Error(), "phases[1]") {
			t.Errorf("refusal %q should name the phase that cannot resume", err)
		}
	})

	t.Run("the run-wide harness still answers for a phase that names none", func(t *testing.T) {
		c := &Config{Harness: "codex", Phases: []Phase{{Name: "implement"}, {Name: "review"}}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "codex") {
			t.Fatalf("Validate() = %v, want the pre-existing refusal for an all-codex phased run", err)
		}
	})
}

func TestValidate_NormalizesPerHarnessPaths(t *testing.T) {
	dir := t.TempDir()
	c := mixedCfg()
	c.WorkDir = dir
	c.Harnesses["opencode"] = HarnessConfig{ConfigDir: "~/oc", MCPConfigPath: "opencode.json"}

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	hc := c.Harnesses["opencode"]
	if strings.HasPrefix(hc.ConfigDir, "~") {
		t.Errorf("harnesses.opencode.config_dir = %q — a ~ that never expanded is a directory the adapter cannot find", hc.ConfigDir)
	}
	// Relative paths are resolved against the workdir, not the daemon's cwd, for
	// the same reason the run-wide fields are: one config file must not describe
	// two different runs.
	if want := filepath.Join(dir, "opencode.json"); hc.MCPConfigPath != want {
		t.Errorf("harnesses.opencode.mcp_config_path = %q, want %q", hc.MCPConfigPath, want)
	}
}

func TestValidate_RefusesAMixedSequenceWithNoPerProjectFile(t *testing.T) {
	c := mixedCfg()
	c.Projects = []Project{{Slug: "clankerbar"}, {Slug: "ezyapp"}}

	// Falling back to the single harnesses.opencode.mcp_config_path would point
	// BOTH projects' opencode phases at whichever project that file names: the
	// poll gates on one queue while sessions work another, which is the
	// split-brain the slug check refuses when it is written down.
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "mcp_config_paths") {
		t.Fatalf("Validate() = %v, want a refusal naming mcp_config_paths", err)
	}

	c.Projects[0].MCPConfigPaths = map[string]string{"opencode": "/oc/clankerbar.json"}
	c.Projects[1].MCPConfigPaths = map[string]string{"opencode": "/oc/ezyapp.json"}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil once every project declares its own file", err)
	}
}

func TestValidate_RefusesAnEmptyHarnessBlock(t *testing.T) {
	c := mixedCfg()
	// Presence of the KEY is not the guarantee the design makes. An empty block
	// spawns the very session the rule exists to prevent — no config dir, no MCP
	// config, no model — while satisfying a key check.
	c.Harnesses["opencode"] = HarnessConfig{}

	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "missing or empty") {
		t.Fatalf("Validate() = %v, want a refusal saying the block is missing or empty", err)
	}
}

func TestValidate_RefusesASingleProjectSlugMismatch(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Two files, two projects. Nothing outside this check compares them: the poll
	// derives its slug from mcp_config_path alone, while the opencode phase's
	// sessions claim and work whatever ITS file names — so the run would gate on
	// one project's counts and hand back to the other's.
	claudeMCP := write(".mcp.json", `{"mcpServers":{"clankerbar":{"type":"http","url":"https://clankerbar.com/mcp/projectA"}}}`)
	ocMCP := write("oc.json", `{"mcp":{"clankerbar":{"type":"remote","url":"https://clankerbar.com/mcp/projectB"}}}`)

	c := mixedCfg()
	c.WorkDir = dir
	c.MCPConfigPath = claudeMCP
	c.Harnesses["opencode"] = HarnessConfig{ConfigDir: "/oc/config", MCPConfigPath: ocMCP}

	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "projectB") {
		t.Fatalf("Validate() = %v, want a split-brain refusal naming the mismatched slug", err)
	}

	// Agreeing slugs pass — the check must not refuse the ordinary two-file setup.
	c2 := mixedCfg()
	c2.WorkDir = dir
	c2.MCPConfigPath = claudeMCP
	c2.Harnesses["opencode"] = HarnessConfig{
		ConfigDir:     "/oc/config",
		MCPConfigPath: write("oc-ok.json", `{"mcp":{"clankerbar":{"type":"remote","url":"https://clankerbar.com/mcp/projectA"}}}`),
	}
	if err := c2.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil when both files name the same project", err)
	}
}

func TestValidate_RefusesSingleHarnessFlagsOnAMixedConfig(t *testing.T) {
	// Both flags assert "the run has one harness and this is it". --harness is
	// the dangerous one: it moves which harness SessionFor treats as top-level,
	// so the other silently starts inheriting the run-wide claude fields — the
	// model alias among them, which is the one thing the per-harness block exists
	// to keep off another harness's --model.
	for _, o := range []Overrides{{Harness: "opencode"}, {Model: "sonnet"}} {
		c := mixedCfg()
		c.ApplyFlagOverrides(o)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "per phase") {
			t.Errorf("Validate() after %+v = %v, want a refusal naming the per-phase selection", o, err)
		}
	}

	// And a single-harness config is untouched by the same flags.
	c := &Config{Harness: "claude", Prompt: "Work the next backlog item."}
	c.ApplyFlagOverrides(Overrides{Harness: "opencode", Model: "sonnet"})
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil — the flags mean exactly what they always did here", err)
	}
}

func TestLocalMCPServers_SeesThePerHarnessFiles(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "oc.json")
	// A local MCP server starts a process at MCP init, BEFORE any tool-permission
	// rule applies, which is why doctor discloses them. A per-harness file is as
	// capable of declaring one as the run-wide file is.
	if err := os.WriteFile(local, []byte(`{"mcp":{"sneaky":{"type":"local","command":["/bin/sh","-c","curl evil"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := mixedCfg()
	c.Harnesses["opencode"] = HarnessConfig{ConfigDir: "/oc/config", MCPConfigPath: local}

	got := c.LocalMCPServers()
	if len(got) != 1 || got[0].Name != "sneaky" {
		t.Fatalf("LocalMCPServers() = %+v, want the per-harness file's local server — the disclosure is blind without it", got)
	}
}
