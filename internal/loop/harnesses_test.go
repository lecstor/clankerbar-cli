package loop

import (
	"errors"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/harness"
)

// Per-phase harness selection (CLA-366): a sequence may implement on one harness
// and review on another. These tests drive TWO fakes through one drain and
// assert which session each one got — the whole feature is that the second phase
// spawns a different binary with different fields, and both halves are invisible
// from a single-adapter driver.

// namedFake is a fakeAdapter that reports a chosen name, so a test can tell the
// two apart in a log line and in adapterFor's own resolution.
type namedFake struct {
	*fakeAdapter
	name string
}

func (f namedFake) Name() string { return f.name }

// mixedDriver builds the implement-on-opencode / review-on-claude driver: the
// run's harness is claude (so `impl` is reached only through the phase's own
// `harness` field), and the opencode adapter is handed out through newAdapter,
// which is the seam production fills with harness.Get.
func mixedDriver(t *testing.T, impl, review *fakeAdapter) *Driver {
	t.Helper()
	cfg := fastCfg()
	cfg.Prompt = ""
	cfg.Model = "claude-opus-5"
	cfg.ConfigDir = "/home/me/.claude"
	cfg.MCPConfigPath = "/repo/.mcp.json"
	cfg.SettingsPath = "/home/me/headless.json"
	cfg.Phases = []config.Phase{
		{Name: "implement", Harness: "opencode"},
		{Name: "review"},
	}
	cfg.Harnesses = map[string]config.HarnessConfig{
		"opencode": {ConfigDir: "/oc/config", MCPConfigPath: "/oc/opencode-mcp.json", Model: "opencode-go/deepseek-v4-flash"},
	}
	d := NewMulti(cfg, review, []Target{{Poller: busyPoller(), Releaser: &fakeReleaser{}}})
	d.newAdapter = func(name string) (harness.Adapter, error) {
		if name == "opencode" {
			return namedFake{impl, "opencode"}, nil
		}
		return nil, errors.New("no adapter " + name)
	}
	openTestStateDir(t, d)
	return d
}

func TestDrainPhases_EachPhaseSpawnsItsOwnHarness(t *testing.T) {
	impl := &fakeAdapter{steps: []invokeStep{{res: checkpointed(10, 0.10)}}}
	review := &fakeAdapter{steps: []invokeStep{{res: okResult(5, 0.05)}}}
	d := mixedDriver(t, impl, review)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}

	if impl.invokeCalls != 1 || review.invokeCalls != 1 {
		t.Fatalf("implement harness ran %d session(s) and review %d, want one each — a sequence that runs both phases on one adapter is the bug this feature removes",
			impl.invokeCalls, review.invokeCalls)
	}
	// And the handoff still works ACROSS the harness boundary: the review session
	// is seeded with the claim the implement session left held, which is what lets
	// it resume the run instead of claiming a task of its own.
	got := review.invocations[0]
	if got.ResumeClaim != openClaim() {
		t.Errorf("the review phase's ResumeClaim = %+v, want the implement phase's claim — a claim is plane state and crosses harnesses freely", got.ResumeClaim)
	}
	if !strings.Contains(got.Prompt, openClaim().RunID) {
		t.Errorf("the review phase's prompt did not get the run id substituted: %q", got.Prompt)
	}
}

func TestInvocationFor_EachPhaseGetsItsOwnHarnessFields(t *testing.T) {
	impl := &fakeAdapter{steps: []invokeStep{{res: checkpointed(10, 0.10)}}}
	review := &fakeAdapter{steps: []invokeStep{{res: okResult(5, 0.05)}}}
	d := mixedDriver(t, impl, review)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}

	oc := impl.invocations[0]
	// Every one of these leaking the claude value is a session that dies at
	// startup: opencode refuses to start on Claude's `mcpServers`, and a claude
	// model alias is one no other provider has.
	if oc.MCPConfigPath != "/oc/opencode-mcp.json" {
		t.Errorf("opencode session got mcp_config_path %q, want its own opencode-schema file", oc.MCPConfigPath)
	}
	if oc.ConfigDir != "/oc/config" {
		t.Errorf("opencode session got config_dir %q, want its own", oc.ConfigDir)
	}
	if oc.Model != "opencode-go/deepseek-v4-flash" {
		t.Errorf("opencode session got model %q, want opencode's own alias", oc.Model)
	}
	if oc.SettingsPath != "" {
		t.Errorf("opencode session got settings_path %q — that file is Claude's --settings and means nothing here", oc.SettingsPath)
	}

	cl := review.invocations[0]
	if cl.MCPConfigPath != "/repo/.mcp.json" || cl.ConfigDir != "/home/me/.claude" ||
		cl.SettingsPath != "/home/me/headless.json" || cl.Model != "claude-opus-5" {
		t.Errorf("the claude phase lost a run-wide field: %+v", cl)
	}
}

func TestInvocationFor_TiersResolveOnThePhasesOwnHarness(t *testing.T) {
	impl := &fakeAdapter{steps: []invokeStep{{res: checkpointed(10, 0.10)}}}
	review := &fakeAdapter{steps: []invokeStep{{res: okResult(5, 0.05)}}}
	d := mixedDriver(t, impl, review)
	// One bucket NAME across two harnesses, two different aliases: that is the
	// whole reason a tier is a bucket rather than a model alias.
	d.cfg.Models = map[string]string{"strong": "claude-opus-5"}
	d.cfg.Harnesses["opencode"] = config.HarnessConfig{
		ConfigDir: "/oc/config", MCPConfigPath: "/oc/opencode-mcp.json",
		Models: map[string]string{"strong": "opencode-go/deepseek-v4-flash"},
	}
	d.cfg.Phases[0].Tier = "strong"
	d.cfg.Phases[1].Tier = "strong"

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}

	if got := impl.invocations[0].Model; got != "opencode-go/deepseek-v4-flash" {
		t.Errorf("the opencode phase's `strong` reached the session as %q, want opencode's alias", got)
	}
	if got := review.invocations[0].Model; got != "claude-opus-5" {
		t.Errorf("the claude phase's `strong` reached the session as %q, want claude's alias", got)
	}
}

func TestAdapterFor_ResolutionAndItsFailureMode(t *testing.T) {
	h := &fakeAdapter{}
	d, _ := phaseDriver(t, h, twoPhases())

	t.Run("a phase naming nothing runs on the driver's own adapter", func(t *testing.T) {
		got, err := d.adapterFor(config.Phase{Name: "implement"})
		if err != nil || got != harness.Adapter(h) {
			t.Errorf("adapterFor(unnamed) = (%v, %v), want the driver's adapter — this is what keeps a single-harness run unchanged", got, err)
		}
	})
	t.Run("a phase naming the run's own harness does too, without the registry", func(t *testing.T) {
		got, err := d.adapterFor(config.Phase{Name: "review", Harness: d.cfg.Harness})
		if err != nil || got != harness.Adapter(h) {
			t.Errorf("adapterFor(%q) = (%v, %v), want the driver's adapter", d.cfg.Harness, got, err)
		}
	})
	t.Run("an unresolvable harness is an error, never a silent fallback", func(t *testing.T) {
		// Validate has already refused an unregistered name, so reaching here means
		// the config and the registry disagree. Running the phase on the other
		// harness would be the review session spawning on the implement harness
		// with nothing in the log to say so.
		d.newAdapter = func(name string) (harness.Adapter, error) { return nil, errors.New("unknown harness " + name) }
		if _, err := d.adapterFor(config.Phase{Name: "review", Harness: "nope"}); err == nil {
			t.Fatal("adapterFor resolved an unknown harness; it must refuse rather than fall back to the driver's")
		}
	})
}

func TestDrainPhase_AnUnresolvableHarnessFailsThePhaseWithoutSpawning(t *testing.T) {
	h := &fakeAdapter{}
	d, _ := phaseDriver(t, h, []config.Phase{{Name: "implement", Harness: "ghost"}, {Name: "review"}})
	d.newAdapter = func(name string) (harness.Adapter, error) { return nil, errors.New("unknown harness " + name) }

	_, _, _, err := drainPhasesOnce(t, d)
	if err == nil {
		t.Fatal("drainPhases succeeded with an unresolvable phase harness")
	}
	if h.invokeCalls != 0 {
		t.Errorf("the driver spawned %d session(s) anyway — a phase whose harness could not be resolved must not run on another one", h.invokeCalls)
	}
}

// A usage limit belongs to one provider's account, so the wait that polls for
// its reset has to probe the harness that hit it — probing the other one answers
// a question nobody asked, and spends on it.
func TestSupervisedWait_ProbesThePhasesOwnHarness(t *testing.T) {
	impl := &fakeAdapter{steps: []invokeStep{{res: limitResult()}, {res: checkpointed(10, 0.10)}}}
	review := &fakeAdapter{steps: []invokeStep{{res: okResult(5, 0.05)}}}
	d := mixedDriver(t, impl, review)

	if _, _, stop, err := drainPhasesOnce(t, d); err != nil || stop {
		t.Fatalf("drainPhases: err=%v stop=%v", err, stop)
	}
	if impl.probeCalls == 0 {
		t.Error("the limited opencode phase was never probed on its own adapter")
	}
	if review.probeCalls != 0 {
		t.Errorf("the claude adapter was probed %d time(s) for a limit opencode hit — a limit is one account's, and a probe is a real paid session", review.probeCalls)
	}
}
