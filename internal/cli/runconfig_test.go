package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lecstor/clankerbar-cli/internal/config"
	"github.com/lecstor/clankerbar-cli/internal/plane"
)

// rcPlaneEnv builds a doctorEnv whose run-config client serves one scripted
// answer for every project.
func rcPlaneEnv(answer func() (*plane.RunConfigState, error)) doctorEnv {
	e := defaultDoctorEnv()
	e.newRCfgAPI = func(mcpURL, apiKey string) plane.RunConfigAPI {
		return scriptedRC{answer: answer}
	}
	return e
}

type scriptedRC struct {
	answer func() (*plane.RunConfigState, error)
}

func (s scriptedRC) RunConfig(context.Context) (*plane.RunConfigState, error) {
	return s.answer()
}
func (s scriptedRC) ProposeRunConfig(context.Context, map[string]any, string) error {
	return errors.New("not used here")
}

func findRunConfigCheck(t *testing.T, checks []check) check {
	t.Helper()
	for _, c := range checks {
		if c.name == "run-config" || strings.HasPrefix(c.name, "run-config[") {
			return c
		}
	}
	t.Fatalf("no run-config check among %d checks", len(checks))
	return check{}
}

// Nothing stored: doctor says plainly that the local file rules are in force.
func TestCheckRunConfigs_NothingStoredReportsLocalRulesInForce(t *testing.T) {
	cfg := validCfg(t)
	checks := checkRunConfigs(context.Background(), cfg, rcPlaneEnv(func() (*plane.RunConfigState, error) {
		return nil, plane.ErrNoConfig
	}))
	c := findRunConfigCheck(t, checks)
	if c.status != pass || !strings.Contains(c.detail, "local rules") {
		t.Errorf("status=%v detail=%q, want a PASS saying local rules are in force", c.status, c.detail)
	}
}

// A stored document in force is reported as such, with its version.
func TestCheckRunConfigs_StoredDocumentReportedAsInForce(t *testing.T) {
	cfg := validCfg(t)
	raw, _ := json.Marshal(map[string]any{"model": "plane-model"})
	checks := checkRunConfigs(context.Background(), cfg, rcPlaneEnv(func() (*plane.RunConfigState, error) {
		return &plane.RunConfigState{Version: 3, Config: raw}, nil
	}))
	c := findRunConfigCheck(t, checks)
	if c.status != pass || !strings.Contains(c.detail, "stored v3") {
		t.Errorf("status=%v detail=%q, want a PASS naming stored v3", c.status, c.detail)
	}
}

// Machine-fit: a stored turn cap under opencode shape-validates on the plane
// but cannot fire here - that is exactly what this check exists to say.
func TestCheckRunConfigs_StoredTurnCapUnderOpencodeWarnedInert(t *testing.T) {
	cfg := validCfg(t)
	raw, _ := json.Marshal(map[string]any{"harness": "opencode", "max_turns": 40})
	checks := checkRunConfigs(context.Background(), cfg, rcPlaneEnv(func() (*plane.RunConfigState, error) {
		return &plane.RunConfigState{Version: 2, Config: raw}, nil
	}))
	c := findRunConfigCheck(t, checks)
	if c.status != warn || !strings.Contains(c.detail, "INERT") || !strings.Contains(c.detail, "max_turns") {
		t.Errorf("status=%v detail=%q, want a WARN marking the stored max_turns INERT", c.status, c.detail)
	}
}

func TestCheckRunConfigs_UndecodableAndRefusedDocumentsWarnNotFail(t *testing.T) {
	cfg := validCfg(t)
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"undecodable", `{"model":`, "undecodable"},
		{"validate-refused", `{"harness":"not-a-harness"}`, "REFUSED locally"},
		{"newer-schema", `{"$schema_version":9999,"model":"plane-x"}`, "newer than this build understands"},
		{"newer-schema-empty", `{"$schema_version":9999}`, "newer than this build understands"},
	} {
		raw := json.RawMessage(tc.raw)
		checks := checkRunConfigs(context.Background(), cfg, rcPlaneEnv(func() (*plane.RunConfigState, error) {
			return &plane.RunConfigState{Version: 5, Config: raw}, nil
		}))
		c := findRunConfigCheck(t, checks)
		if c.status != warn || !strings.Contains(c.detail, tc.want) {
			t.Errorf("%s: status=%v detail=%q, want WARN containing %q", tc.name, c.status, c.detail, tc.want)
		}
	}
}

// CLA-475: doctor reports the same loud refusal the loop enforces — a stored
// harness swap over run-wide machine wiring warns REFUSED rather than
// reporting an overlay that would re-home the old harness's dialect.
func TestCheckRunConfigs_HarnessSwapOverWiringWarnsRefused(t *testing.T) {
	cfg := validCfg(t)
	cfg.Harness = "claude"
	cfg.Model = "claude-alias"
	cfg.ConfigDir = "/local/claude-dir"
	cfg.MCPConfigPath = "/local/claude-mcp.json"
	cfg.SettingsPath = "/local/claude-settings.json"
	raw, _ := json.Marshal(map[string]any{"harness": "opencode"})
	checks := checkRunConfigs(context.Background(), cfg, rcPlaneEnv(func() (*plane.RunConfigState, error) {
		return &plane.RunConfigState{Version: 7, Config: raw}, nil
	}))
	c := findRunConfigCheck(t, checks)
	if c.status != warn || !strings.Contains(c.detail, "REFUSED locally") {
		t.Errorf("status=%v detail=%q, want WARN containing REFUSED locally", c.status, c.detail)
	}
}

// Unwired: nothing to compare against, and the backlog wiring check already
// reports that gap - so this check stays silent rather than duplicating it.
func TestCheckRunConfigs_NotWiredIsSilent(t *testing.T) {
	cfg := validCfg(t)
	checks := checkRunConfigs(context.Background(), cfg, rcPlaneEnv(func() (*plane.RunConfigState, error) {
		return nil, plane.ErrNotWired
	}))
	if c := findRunConfigCheckQuiet(checks); c != nil {
		t.Fatalf("unwired plane produced a run-config check: %+v", *c)
	}
}

func findRunConfigCheckQuiet(checks []check) *check {
	for i := range checks {
		if checks[i].name == "run-config" || strings.HasPrefix(checks[i].name, "run-config[") {
			return &checks[i]
		}
	}
	return nil
}

// Multi-project: each project's document is reported under its own label.
func TestCheckRunConfigs_MultiProjectGetsOneCheckPerProject(t *testing.T) {
	cfg := validCfg(t)
	cfg.Projects = []config.Project{{Slug: "alpha"}, {Slug: "beta"}}
	raw, _ := json.Marshal(map[string]any{})
	checks := checkRunConfigs(context.Background(), cfg, rcPlaneEnv(func() (*plane.RunConfigState, error) {
		return &plane.RunConfigState{Version: 1, Config: raw}, nil
	}))
	for _, want := range []string{"run-config[alpha]", "run-config[beta]"} {
		var found bool
		for _, c := range checks {
			if c.name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no %q check among %d", want, len(checks))
		}
	}
}

// The propose-config command renders the movable half and records it PENDING;
// machine-local keys never ride along; an empty render is refused outright.
func TestProposeConfig_CommandFlow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"harness": "claude", "model": "opus", "models": {"strong": "opus"}, "workdir": "`+dir+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLANKERBAR_API_KEY", "k-test")

	var gotDoc map[string]any
	var gotNotes string
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Params.Name != "propose_project_run_config" {
			t.Errorf("tool = %q", body.Params.Name)
		}
		called = true
		gotDoc, _ = body.Params.Arguments["config"].(map[string]any)
		gotNotes, _ = body.Params.Arguments["notes"].(string)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`))
	}))
	defer srv.Close()

	// Point the config's wiring at the fake plane so the command reaches it.
	if err := os.WriteFile(path, []byte(`{"harness": "claude", "model": "opus", "models": {"strong": "opus"}, "workdir": "`+dir+`", "backlog_url": "`+srv.URL+`/mcp/demo"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ProposeConfig(context.Background(), []string{"--config", path}); err != nil {
		t.Fatalf("ProposeConfig: %v", err)
	}
	if !called {
		t.Fatal("no proposal was recorded")
	}
	if gotDoc["harness"] != "claude" || gotDoc["model"] != "opus" {
		t.Errorf("document = %+v, want the file's movable dials", gotDoc)
	}
	if _, ok := gotDoc["workdir"]; ok {
		t.Error("the proposal carries workdir; machine paths never move to the plane")
	}
	if !strings.Contains(gotNotes, "propose-config") {
		t.Errorf("notes = %q, want the auto derivation note naming the command", gotNotes)
	}
}

// A config carrying nothing but the defaults imports exactly its one movable
// dial - and never invents entries for keys the file did not set.
func TestProposeConfig_MinimalConfigImportsExactlyItsMovableHalf(t *testing.T) {
	var gotDoc map[string]any
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Params.Name == "propose_project_run_config" {
			called = true
			gotDoc, _ = body.Params.Arguments["config"].(map[string]any)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"backlog_url": "`+srv.URL+`/mcp/demo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLANKERBAR_API_KEY", "k-test")

	if err := ProposeConfig(context.Background(), []string{"--config", path}); err != nil {
		t.Fatalf("ProposeConfig: %v", err)
	}
	if !called {
		t.Fatal("no proposal was recorded")
	}
	if len(gotDoc) != 1 || gotDoc["harness"] != "claude" {
		t.Errorf("document = %+v, want exactly the resolved harness dial", gotDoc)
	}
}

// CLA-598: a stored STEP whose harness this machine cannot run is a FAILED
// check naming the project, the step and the harness — not the WARN every
// other stored-document refusal gets. The loop keeps its previous config
// either way, but a ratified pipeline that silently cannot run here is exactly
// the kind of degradation doctor exists to surface before an unattended drain.
func TestCheckRunConfigs_StoredStepOnAnUnrunnableHarnessFails(t *testing.T) {
	cfg := &config.Config{
		Harness:    "opencode",
		Prompt:     "work the backlog",
		BacklogURL: "https://clankerbar.com",
		Projects:   []config.Project{{Slug: "demo"}},
	}
	raw, _ := json.Marshal(map[string]any{
		"phases": []map[string]any{
			{"name": "implement"},
			{"name": "review", "harness": "codex"},
			{"name": "fix"},
		},
	})
	checks := checkRunConfigs(context.Background(), cfg, rcPlaneEnv(func() (*plane.RunConfigState, error) {
		return &plane.RunConfigState{Version: 4, Config: raw}, nil
	}))
	c := findRunConfigCheck(t, checks)
	if c.status != fail {
		t.Fatalf("status=%v detail=%q, want FAIL for a stored step this machine cannot run", c.status, c.detail)
	}
	for _, want := range []string{"demo", `phases[1] ("review")`, "codex"} {
		if !strings.Contains(c.detail, want) {
			t.Errorf("detail=%q, want it to name %q", c.detail, want)
		}
	}
	if !strings.Contains(c.remedy, "harnesses.codex") {
		t.Errorf("remedy=%q, want it to name the block to declare", c.remedy)
	}
}

// 2.review-3.3 (CLA-598 fix round 2): a panel with no phase sequence to expand
// passes the CLI's own Validate (the key is inert locally), but the PLANE refuses
// it as stored policy (decision 94497792). propose-config must refuse it locally
// with a message the operator can act on, never send it.
func TestProposeConfig_RefusesAPanelThePlaneCannotExpand(t *testing.T) {
	var gotDoc map[string]any
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Params.Name == "propose_project_run_config" {
			called = true
			gotDoc, _ = body.Params.Arguments["config"].(map[string]any)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"harness": "opencode", "prompt": "work the backlog", "backlog_url": "` + srv.URL + `/mcp/demo",` +
		` "review_panel": [{"model": "opencode-go/gpt-6-luna"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLANKERBAR_API_KEY", "k-test")

	err := ProposeConfig(context.Background(), []string{"--config", path})
	if err == nil {
		t.Fatalf("propose-config proposed %v; the plane refuses a panel with no expandable review phase", gotDoc)
	}
	if !strings.Contains(err.Error(), "review_panel") || !strings.Contains(err.Error(), "review") {
		t.Errorf("the refusal does not name the panel and the missing review phase: %v", err)
	}
	if called {
		t.Error("a proposal was sent for a document the plane cannot ratify")
	}
}

// CLA-482: single-project mode has no projects[] entry for --slug to match, and
// that left the endpoint on the config's OWN project while the success line
// echoed the requested one. The write must follow the slug it names.
func TestProposeConfig_SingleProjectTargetsTheNamedSlug(t *testing.T) {
	for _, tc := range []struct {
		name string
		// body writes the config file and returns its JSON.
		body func(t *testing.T, dir, planeURL string) string
		slug string
		want string
	}{
		{
			// The wiring's own /mcp/<slug> names "demo"; --slug names another.
			name: "backlog_url naming the config's own project",
			body: func(t *testing.T, dir, planeURL string) string {
				return `{"harness": "claude", "model": "opus", "workdir": "` + dir + `", "backlog_url": "` + planeURL + `/mcp/demo"}`
			},
			slug: "other",
			want: "/mcp/other",
		},
		{
			// Same single-project mode, but the slug is lifted from .mcp.json and
			// the origin from backlog_url.
			name: "mcp_config_path naming the config's own project",
			body: func(t *testing.T, dir, planeURL string) string {
				mcp := filepath.Join(dir, "mcp.json")
				servers := `{"mcpServers": {"clankerbar": {"type": "http", "url": "` + planeURL + `/mcp/demo"}}}`
				if err := os.WriteFile(mcp, []byte(servers), 0o600); err != nil {
					t.Fatal(err)
				}
				return `{"harness": "claude", "model": "opus", "workdir": "` + dir + `", "backlog_url": "` + planeURL + `", "mcp_config_path": "` + mcp + `"}`
			},
			slug: "other",
			want: "/mcp/other",
		},
		{
			// Naming the config's own project still targets it.
			name: "the config's own slug",
			body: func(t *testing.T, dir, planeURL string) string {
				return `{"harness": "claude", "model": "opus", "workdir": "` + dir + `", "backlog_url": "` + planeURL + `/mcp/demo"}`
			},
			slug: "demo",
			want: "/mcp/demo",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}`))
			}))
			defer srv.Close()

			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, []byte(tc.body(t, dir, srv.URL)), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CLANKERBAR_API_KEY", "k-test")

			if err := ProposeConfig(context.Background(), []string{"--config", path, "--slug", tc.slug}); err != nil {
				t.Fatalf("ProposeConfig: %v", err)
			}
			if gotPath != tc.want {
				t.Errorf("proposal went to %q, want %q - the endpoint must follow the slug the command named", gotPath, tc.want)
			}
		})
	}
}
